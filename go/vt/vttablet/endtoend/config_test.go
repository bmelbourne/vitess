/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endtoend

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/sqltypes"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/endtoend/framework"
	"vitess.io/vitess/go/vt/vttablet/tabletserver"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
)

func TestPoolSize(t *testing.T) {
	revert := changeVar(t, "ReadPoolSize", "1")
	defer revert()

	vstart := framework.DebugVars()
	verifyIntValue(t, vstart, "ConnPoolCapacity", 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		framework.NewClient().Execute("select sleep(0.5) from dual", nil)
		wg.Done()
	}()
	// The queries have to be different so consolidator doesn't kick in.
	go func() {
		framework.NewClient().Execute("select sleep(0.49) from dual", nil)
		wg.Done()
	}()
	wg.Wait()

	// Parallel plan building can cause multiple conn pool waits.
	// Check that the wait count was at least incremented once so
	// we know it's working.
	tag := "ConnPoolWaitCount"
	got := framework.FetchInt(framework.DebugVars(), tag)
	want := framework.FetchInt(vstart, tag)
	assert.LessOrEqual(t, want, got)
}

func TestStreamPoolSize(t *testing.T) {
	revert := changeVar(t, "StreamPoolSize", "1")
	defer revert()

	vstart := framework.DebugVars()
	verifyIntValue(t, vstart, "StreamConnPoolCapacity", 1)
}

// TestTxPoolSize starts 2 transactions, one in normal pool and one in found rows pool of transaction pool.
// Changing the pool size to 1, we verify that the pool size is updated and the pool is full when we try to acquire next transaction.
func TestTxPoolSize(t *testing.T) {
	vstart := framework.DebugVars()

	verifyIntValue(t, vstart, "TransactionPoolCapacity", 20)
	verifyIntValue(t, vstart, "FoundRowsPoolCapacity", 20)

	client1 := framework.NewClient()
	err := client1.Begin( /* found rows pool*/ false)
	require.NoError(t, err)
	defer client1.Rollback()
	verifyIntValue(t, framework.DebugVars(), "TransactionPoolAvailable", framework.FetchInt(vstart, "TransactionPoolAvailable")-1)

	client2 := framework.NewClient()
	err = client2.Begin( /* found rows pool*/ true)
	require.NoError(t, err)
	defer client2.Rollback()
	verifyIntValue(t, framework.DebugVars(), "FoundRowsPoolAvailable", framework.FetchInt(vstart, "FoundRowsPoolAvailable")-1)

	revert := changeVar(t, "TransactionPoolSize", "1")
	defer revert()
	vend := framework.DebugVars()
	verifyIntValue(t, vend, "TransactionPoolAvailable", 0)
	verifyIntValue(t, vend, "TransactionPoolCapacity", 1)
	verifyIntValue(t, vend, "FoundRowsPoolAvailable", 0)
	verifyIntValue(t, vend, "FoundRowsPoolCapacity", 1)
	assert.Equal(t, 1, framework.Server.TxPoolSize())

	client3 := framework.NewClient()

	// tx pool - normal
	err = client3.Begin( /* found rows pool*/ false)
	require.ErrorContains(t, err, "connection limit exceeded")
	compareIntDiff(t, framework.DebugVars(), "Errors/RESOURCE_EXHAUSTED", vstart, 1)

	// tx pool - found rows
	err = client3.Begin( /* found rows pool*/ true)
	require.ErrorContains(t, err, "connection limit exceeded")
	compareIntDiff(t, framework.DebugVars(), "Errors/RESOURCE_EXHAUSTED", vstart, 2)
}

func TestDisableConsolidator(t *testing.T) {
	totalConsolidationsTag := "Waits/Histograms/Consolidations/Count"
	initial := framework.FetchInt(framework.DebugVars(), totalConsolidationsTag)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		framework.NewClient().Execute("select sleep(0.5) from dual", nil)
		wg.Done()
	}()
	go func() {
		framework.NewClient().Execute("select sleep(0.5) from dual", nil)
		wg.Done()
	}()
	wg.Wait()
	afterOne := framework.FetchInt(framework.DebugVars(), totalConsolidationsTag)
	assert.Equal(t, initial+1, afterOne, "expected one consolidation")

	revert := changeVar(t, "Consolidator", tabletenv.Disable)
	defer revert()
	var wg2 sync.WaitGroup
	wg2.Add(2)
	go func() {
		framework.NewClient().Execute("select sleep(0.5) from dual", nil)
		wg2.Done()
	}()
	go func() {
		framework.NewClient().Execute("select sleep(0.5) from dual", nil)
		wg2.Done()
	}()
	wg2.Wait()
	noNewConsolidations := framework.FetchInt(framework.DebugVars(), totalConsolidationsTag)
	assert.Equal(t, afterOne, noNewConsolidations, "expected no new consolidations")
}

func TestConsolidatorReplicasOnly(t *testing.T) {
	type executeFn func(
		query string, bindvars map[string]*querypb.BindVariable,
	) (*sqltypes.Result, error)

	testCases := []struct {
		name                   string
		getExecuteFn           func(qc *framework.QueryClient) executeFn
		totalConsolidationsTag string
	}{
		{
			name:                   "Execute",
			getExecuteFn:           func(qc *framework.QueryClient) executeFn { return qc.Execute },
			totalConsolidationsTag: "Waits/Histograms/Consolidations/Count",
		},
		{
			name:                   "StreamExecute",
			getExecuteFn:           func(qc *framework.QueryClient) executeFn { return qc.StreamExecute },
			totalConsolidationsTag: "Waits/Histograms/StreamConsolidations/Count",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			initial := framework.FetchInt(framework.DebugVars(), testCase.totalConsolidationsTag)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				testCase.getExecuteFn(framework.NewClient())("select sleep(0.5) from dual", nil)
				wg.Done()
			}()
			go func() {
				testCase.getExecuteFn(framework.NewClient())("select sleep(0.5) from dual", nil)
				wg.Done()
			}()
			wg.Wait()
			afterOne := framework.FetchInt(framework.DebugVars(), testCase.totalConsolidationsTag)
			assert.Equal(t, initial+1, afterOne, "expected one consolidation")

			revert := changeVar(t, "Consolidator", tabletenv.NotOnPrimary)
			defer revert()

			// primary should not do query consolidation
			var wg2 sync.WaitGroup
			wg2.Add(2)
			go func() {
				testCase.getExecuteFn(framework.NewClient())("select sleep(0.5) from dual", nil)
				wg2.Done()
			}()
			go func() {
				testCase.getExecuteFn(framework.NewClient())("select sleep(0.5) from dual", nil)
				wg2.Done()
			}()
			wg2.Wait()
			noNewConsolidations := framework.FetchInt(framework.DebugVars(), testCase.totalConsolidationsTag)
			assert.Equal(t, afterOne, noNewConsolidations, "expected no new consolidations")

			// become a replica, where query consolidation should happen
			client := framework.NewClientWithTabletType(topodatapb.TabletType_REPLICA)

			err := client.SetServingType(topodatapb.TabletType_REPLICA)
			require.NoError(t, err)
			defer func() {
				err = client.SetServingType(topodatapb.TabletType_PRIMARY)
				require.NoError(t, err)
			}()

			initial = framework.FetchInt(framework.DebugVars(), testCase.totalConsolidationsTag)
			var wg3 sync.WaitGroup
			wg3.Add(2)
			go func() {
				testCase.getExecuteFn(client)("select sleep(0.5) from dual", nil)
				wg3.Done()
			}()
			go func() {
				testCase.getExecuteFn(client)("select sleep(0.5) from dual", nil)
				wg3.Done()
			}()
			wg3.Wait()
			afterOne = framework.FetchInt(framework.DebugVars(), testCase.totalConsolidationsTag)
			assert.Equal(t, initial+1, afterOne, "expected another consolidation")
		})
	}
}

func TestQueryEnginePlanCacheSize(t *testing.T) {
	var cachedPlanSize = int((&tabletserver.TabletPlan{}).CachedSize(true))

	// sleep to avoid race between SchemaChanged event clearing out the plans cache which breaks this test
	framework.Server.WaitForSchemaReset(2 * time.Second)

	bindVars := map[string]*querypb.BindVariable{
		"ival1": sqltypes.Int64BindVariable(1),
		"ival2": sqltypes.Int64BindVariable(1),
	}

	framework.Server.ClearQueryPlanCache()

	client := framework.NewClient()
	_, _ = client.Execute("select * from vitess_test where intval=:ival1", bindVars)
	_, _ = client.Execute("select * from vitess_test where intval=:ival1", bindVars)
	assert.Equal(t, 1, framework.Server.QueryPlanCacheLen())

	vend := framework.DebugVars()
	assert.GreaterOrEqual(t, framework.FetchInt(vend, "QueryEnginePlanCacheSize"), cachedPlanSize)

	_, _ = client.Execute("select * from vitess_test where intval=:ival2", bindVars)
	require.Equal(t, 2, framework.Server.QueryPlanCacheLen())

	vend = framework.DebugVars()
	assert.GreaterOrEqual(t, framework.FetchInt(vend, "QueryEnginePlanCacheSize"), 2*cachedPlanSize)

	_, _ = client.Execute("select * from vitess_test where intval=1", bindVars)
	require.Equal(t, 3, framework.Server.QueryPlanCacheLen())

	vend = framework.DebugVars()
	assert.GreaterOrEqual(t, framework.FetchInt(vend, "QueryEnginePlanCacheSize"), 3*cachedPlanSize)
}

func TestMaxResultSize(t *testing.T) {
	revert := changeVar(t, "MaxResultSize", "2")
	defer revert()

	client := framework.NewClient()
	query := "select * from vitess_test"
	_, err := client.Execute(query, nil)
	assert.Error(t, err)
	want := "Row count exceeded"
	assert.Contains(t, err.Error(), want, "Error: %v, must start with %s", err, want)
	verifyIntValue(t, framework.DebugVars(), "MaxResultSize", 2)
	framework.Server.SetMaxResultSize(10)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)
}

func TestWarnResultSize(t *testing.T) {
	revert := changeVar(t, "WarnResultSize", "2")
	defer revert()
	client := framework.NewClient()

	originalWarningsResultsExceededCount := framework.FetchInt(framework.DebugVars(), "Warnings/ResultsExceeded")
	query := "select * from vitess_test"
	_, _ = client.Execute(query, nil)
	newWarningsResultsExceededCount := framework.FetchInt(framework.DebugVars(), "Warnings/ResultsExceeded")
	exceededCountDiff := newWarningsResultsExceededCount - originalWarningsResultsExceededCount
	assert.Equal(t, 1, exceededCountDiff, "Warnings.ResultsExceeded counter should have increased by 1")

	verifyIntValue(t, framework.DebugVars(), "WarnResultSize", 2)
	framework.Server.SetWarnResultSize(10)
	_, _ = client.Execute(query, nil)
	newerWarningsResultsExceededCount := framework.FetchInt(framework.DebugVars(), "Warnings/ResultsExceeded")
	exceededCountDiff = newerWarningsResultsExceededCount - newWarningsResultsExceededCount
	assert.Equal(t, 0, exceededCountDiff, "Warnings.ResultsExceeded counter should not have increased")
}

func TestQueryTimeout(t *testing.T) {
	vstart := framework.DebugVars()
	defer framework.Server.QueryTimeout.Store(framework.Server.QueryTimeout.Load())
	framework.Server.QueryTimeout.Store(100 * time.Millisecond.Nanoseconds())

	client := framework.NewClient()
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute("select sleep(1) from vitess_test", nil)
	assert.Equal(t, vtrpcpb.Code_CANCELED, vterrors.Code(err))
	_, err = client.Execute("select 1 from dual", nil)
	assert.Equal(t, vtrpcpb.Code_ABORTED, vterrors.Code(err))
	vend := framework.DebugVars()
	verifyIntValue(t, vend, "QueryTimeout", int(100*time.Millisecond))
	compareIntDiff(t, vend, "Kills/Connections", vstart, 1)
}

// TestHeartbeatMetric validates the heartbeat metrics exists from the connection pool.
func TestHeartbeatMetric(t *testing.T) {
	tcases := []struct {
		metricName string
		exp        any
	}{{
		metricName: "HeartbeatWriteAppPoolCapacity",
		exp:        2,
	}, {
		metricName: "HeartbeatWriteAllPrivsPoolCapacity",
		exp:        2,
	}}

	metrics := framework.DebugVars()
	for _, tcase := range tcases {
		t.Run(tcase.metricName, func(t *testing.T) {
			mValue, exists := metrics[tcase.metricName]
			require.True(t, exists, "metric %s not found", tcase.metricName)
			require.EqualValues(t, tcase.exp, mValue, "metric %s value is %d, want %d", tcase.metricName, mValue, tcase.exp)
		})
	}
}

func changeVar(t *testing.T, name, value string) (revert func()) {
	t.Helper()

	vals := framework.FetchJSON("/debug/env?format=json")
	initial, ok := vals[name]
	if !ok {
		t.Fatalf("%s not found in: %v", name, vals)
	}
	vals = framework.PostJSON("/debug/env?format=json", map[string]string{
		"varname": name,
		"value":   value,
	})
	verifyMapValue(t, vals, name, value)
	return func() {
		vals = framework.PostJSON("/debug/env?format=json", map[string]string{
			"varname": name,
			"value":   fmt.Sprintf("%v", initial),
		})
		verifyMapValue(t, vals, name, initial)
	}
}

func verifyMapValue(t *testing.T, values map[string]any, tag string, want any) {
	t.Helper()
	val, ok := values[tag]
	if !ok {
		t.Fatalf("%s not found in: %v", tag, values)
	}
	assert.Equal(t, want, val)
}

func compareIntDiff(t *testing.T, end map[string]any, tag string, start map[string]any, diff int) {
	t.Helper()
	verifyIntValue(t, end, tag, framework.FetchInt(start, tag)+diff)
}

func verifyIntValue(t *testing.T, values map[string]any, tag string, want int) {
	t.Helper()
	require.Equal(t, want, framework.FetchInt(values, tag), tag)
}
