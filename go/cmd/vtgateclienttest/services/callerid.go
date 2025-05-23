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

package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/callerid"
	querypb "vitess.io/vitess/go/vt/proto/query"
	vtgatepb "vitess.io/vitess/go/vt/proto/vtgate"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/vtgateservice"
)

// CallerIDPrefix is the prefix to send with queries so they go
// through this test suite.
const CallerIDPrefix = "callerid://"

// callerIDClient implements vtgateservice.VTGateService, and checks
// the received callerid matches the one passed out of band by the client.
type callerIDClient struct {
	fallbackClient
}

func newCallerIDClient(fallback vtgateservice.VTGateService) *callerIDClient {
	return &callerIDClient{
		fallbackClient: newFallbackClient(fallback),
	}
}

// checkCallerID will see if this module is handling the request, and
// if it is, check the callerID from the context.  Returns false if
// the query is not for this module.  Returns true and the error to
// return with the call if this module is handling the request.
func (c *callerIDClient) checkCallerID(ctx context.Context, received string) (bool, error) {
	if !strings.HasPrefix(received, CallerIDPrefix) {
		return false, nil
	}

	jsonCallerID := []byte(received[len(CallerIDPrefix):])
	expectedCallerID := &vtrpcpb.CallerID{}
	if err := json.Unmarshal(jsonCallerID, expectedCallerID); err != nil {
		return true, fmt.Errorf("cannot unmarshal provided callerid: %v", err)
	}

	receivedCallerID := callerid.EffectiveCallerIDFromContext(ctx)
	if receivedCallerID == nil {
		return true, fmt.Errorf("no callerid received in the query")
	}

	if !proto.Equal(receivedCallerID, expectedCallerID) {
		return true, fmt.Errorf("callerid mismatch, got %v expected %v", receivedCallerID, expectedCallerID)
	}

	return true, fmt.Errorf("SUCCESS: callerid matches")
}

func (c *callerIDClient) Execute(
	ctx context.Context,
	mysqlCtx vtgateservice.MySQLConnection,
	session *vtgatepb.Session,
	sql string,
	bindVariables map[string]*querypb.BindVariable,
	prepared bool,
) (*vtgatepb.Session, *sqltypes.Result, error) {
	if ok, err := c.checkCallerID(ctx, sql); ok {
		return session, nil, err
	}
	return c.fallbackClient.Execute(ctx, mysqlCtx, session, sql, bindVariables, prepared)
}

func (c *callerIDClient) ExecuteBatch(ctx context.Context, session *vtgatepb.Session, sqlList []string, bindVariablesList []map[string]*querypb.BindVariable) (*vtgatepb.Session, []sqltypes.QueryResponse, error) {
	if len(sqlList) == 1 {
		if ok, err := c.checkCallerID(ctx, sqlList[0]); ok {
			return session, nil, err
		}
	}
	return c.fallbackClient.ExecuteBatch(ctx, session, sqlList, bindVariablesList)
}

func (c *callerIDClient) StreamExecute(ctx context.Context, mysqlCtx vtgateservice.MySQLConnection, session *vtgatepb.Session, sql string, bindVariables map[string]*querypb.BindVariable, callback func(*sqltypes.Result) error) (*vtgatepb.Session, error) {
	if ok, err := c.checkCallerID(ctx, sql); ok {
		return session, err
	}
	return c.fallbackClient.StreamExecute(ctx, mysqlCtx, session, sql, bindVariables, callback)
}

// ExecuteMulti is part of the VTGateService interface
func (c *callerIDClient) ExecuteMulti(ctx context.Context, mysqlCtx vtgateservice.MySQLConnection, session *vtgatepb.Session, sqlString string) (newSession *vtgatepb.Session, qrs []*sqltypes.Result, err error) {
	queries, err := sqlparser.NewTestParser().SplitStatementToPieces(sqlString)
	if err != nil {
		return session, nil, err
	}
	var result *sqltypes.Result
	for _, query := range queries {
		session, result, err = c.Execute(ctx, mysqlCtx, session, query, nil, false)
		if err != nil {
			return session, qrs, err
		}
		qrs = append(qrs, result)
	}
	return session, qrs, nil
}

// StreamExecuteMulti is part of the VTGateService interface
func (c *callerIDClient) StreamExecuteMulti(ctx context.Context, mysqlCtx vtgateservice.MySQLConnection, session *vtgatepb.Session, sqlString string, callback func(qr sqltypes.QueryResponse, more bool, firstPacket bool) error) (*vtgatepb.Session, error) {
	queries, err := sqlparser.NewTestParser().SplitStatementToPieces(sqlString)
	if err != nil {
		return session, err
	}
	for idx, query := range queries {
		firstPacket := true
		session, err = c.StreamExecute(ctx, mysqlCtx, session, query, nil, func(result *sqltypes.Result) error {
			err = callback(sqltypes.QueryResponse{QueryResult: result}, idx < len(queries)-1, firstPacket)
			firstPacket = false
			return err
		})
		if err != nil {
			return session, err
		}
	}
	return session, nil
}
