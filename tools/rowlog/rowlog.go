package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"

	"vitess.io/vitess/go/mysql/replication"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/binlog/binlogplayer"
	"vitess.io/vitess/go/vt/discovery"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vtctl/vtctlclient"
	"vitess.io/vitess/go/vt/vtgate/vtgateconn"

	_ "vitess.io/vitess/go/vt/topo/etcd2topo" // TODO: after #11394, add rowlog to this https://github.com/vitessio/vitess/pull/11394/files#diff-ee3c1b94c587244ea0645a8ee10187e1112167725f752d58cf17bab6e6d1047cR85
	_ "vitess.io/vitess/go/vt/vtctl/grpcvtctlclient"
	_ "vitess.io/vitess/go/vt/vtgate/grpcvtgateconn"
	_ "vitess.io/vitess/go/vt/vttablet/grpctabletconn"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	logutilpb "vitess.io/vitess/go/vt/proto/logutil"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtgatepb "vitess.io/vitess/go/vt/proto/vtgate"
)

type RowLogConfig struct {
	ids, cells []string

	sourceKeyspace, targetKeyspace, table, vtgate, vtctld, pk string
}

func (rlc *RowLogConfig) String() string {
	s := fmt.Sprintf("\tsource:%s, target:%s, table:%s, ids:%s, pk:%s\n",
		rlc.sourceKeyspace, rlc.targetKeyspace, rlc.table, strings.Join(rlc.ids, ","), rlc.pk)
	s += fmt.Sprintf("\tvtgate:%s, vtctld:%s, cells:%s", rlc.vtgate, rlc.vtctld, strings.Join(rlc.cells, ","))
	return s
}

func (rlc *RowLogConfig) Validate() bool {
	if rlc.table == "" || len(rlc.cells) == 0 || rlc.vtctld == "" || rlc.vtgate == "" || len(rlc.ids) == 0 || rlc.targetKeyspace == "" || rlc.sourceKeyspace == "" || rlc.pk == "" {
		return false
	}
	return true
}

func usage() {
	logger := logutil.NewConsoleLogger()
	pflag.CommandLine.SetOutput(logutil.NewLoggerWriter(logger))
	pflag.Usage = func() {
		logger.Printf("Rowlog Usage:\n")
		s := "rowlog --ids <id list csv> --table <table_name> --pk <primary_key_only_ints> --source <source_keyspace> --target <target_keyspace> "
		s += "--vtctld <vtctl url> --vtgate <vtgate url> --cells <cell names csv> --topo-implementation <topo type, eg: etcd2> "
		s += "--topo-global-server-address <top url> --topo-global-root <topo root dir>\n"
		logger.Printf(s)
	}
}

func main() {
	usage()
	ctx := context.Background()
	config := parseCommandLine()
	if !config.Validate() {
		pflag.Usage()
		return
	}
	log.Infof("Starting rowlogger with config: %s", config)
	fmt.Printf("Starting rowlogger with\n%v\n", config)
	ts := topo.Open()
	sourceTablet := getTablet(ctx, ts, config.cells, config.sourceKeyspace)
	targetTablet := getTablet(ctx, ts, config.cells, config.targetKeyspace)
	log.Infof("Using tablets %s and %s to get positions", sourceTablet, targetTablet)

	var wg sync.WaitGroup
	var stream = func(keyspace, tablet string) {
		defer wg.Done()
		var startPos, stopPos string
		var i int
		var done, fieldsPrinted bool
		var err error
		for {
			i++
			if i > 100 {
				log.Errorf("returning without completion : Timing out for keyspace %s", keyspace)
				return
			}
			log.Infof("%s Iteration:%d", keyspace, i)
			startPos, stopPos, done, fieldsPrinted, err = startStreaming(ctx, config.vtgate, config.vtctld, keyspace, tablet, config.table, config.pk, config.ids, startPos, stopPos, fieldsPrinted)
			if done {
				log.Infof("Finished streaming all events for keyspace %s", keyspace)
				fmt.Printf("Finished streaming all events for keyspace %s\n", keyspace)
				return
			}
			if startPos != "" {
				log.Infof("resuming streaming from %s, error received was %v", startPos, err)
			} else {
				log.Errorf("returning without completion of keyspace %s because of error %v", keyspace, err)
				return
			}
		}
	}

	wg.Add(1)
	go stream(config.sourceKeyspace, sourceTablet)

	wg.Add(1)
	go stream(config.targetKeyspace, targetTablet)

	wg.Wait()

	log.Infof("rowlog done streaming from both source and target")
	fmt.Printf("\n\nRowlog completed\nIf the program worked you should see two log files with the related binlog entries: %s.log and %s.log\n",
		config.sourceKeyspace, config.targetKeyspace)
}

func startStreaming(ctx context.Context, vtgate, vtctld, keyspace, tablet, table, pk string, ids []string, startPos, stopPos string, fieldsPrinted bool) (string, string, bool, bool, error) {
	var err error
	if startPos == "" {
		flavor := getFlavor(ctx, vtctld, keyspace)
		if flavor == "" {
			log.Errorf("Invalid flavor for %s", keyspace)
			return "", "", false, false, nil
		}
		startPos, stopPos, _ = getPositions(ctx, vtctld, tablet)
		startPos = flavor + "/" + startPos
		stopPos = flavor + "/" + stopPos
	}
	log.Infof("Streaming keyspace %s from %s upto %s", keyspace, startPos, stopPos)
	fmt.Printf("Streaming keyspace %s from %s upto %s\n", keyspace, startPos, stopPos)
	vgtid := &binlogdatapb.VGtid{
		ShardGtids: []*binlogdatapb.ShardGtid{{
			Keyspace: keyspace,
			Shard:    "0",
			Gtid:     startPos,
		}},
	}
	filter := &binlogdatapb.Filter{
		Rules: []*binlogdatapb.Rule{{
			Match:  table,
			Filter: "select * from " + table,
		}},
		FieldEventMode: 1,
	}
	conn, err := vtgateconn.Dial(ctx, vtgate)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	reader, _ := conn.VStream(ctx, topodatapb.TabletType_PRIMARY, vgtid, filter, &vtgatepb.VStreamFlags{})
	var fields []*querypb.Field
	var gtid string
	var plan *TablePlan
	var lastLoggedAt int64
	var totalRowsForTable, filteredRows int
	for {
		evs, err := reader.Recv()
		switch err {
		case nil:
			for _, ev := range evs {
				now := time.Now().Unix()
				if now-lastLoggedAt > 60 && ev.Timestamp != 0 { // every minute
					lastLoggedAt = now
					log.Infof("%s Progress: %d/%d rows, %s: %s", keyspace, filteredRows, totalRowsForTable,
						time.Unix(ev.Timestamp, 0).Format(time.RFC3339), gtid)
					fmt.Printf(".")
				}
				switch ev.Type {
				case binlogdatapb.VEventType_VGTID:
					gtid = ev.Vgtid.ShardGtids[0].Gtid
				case binlogdatapb.VEventType_FIELD:
					fields = ev.FieldEvent.Fields
					plan = getTablePlan(keyspace, fields, ev.FieldEvent.TableName, pk, ids)
					if !fieldsPrinted {
						outputHeader(plan)
						fieldsPrinted = true
					}
				case binlogdatapb.VEventType_ROW:
					totalRowsForTable += len(ev.RowEvent.RowChanges)
					rows := processRowEvent(plan, gtid, ev)
					if len(rows) > 0 {
						filteredRows += len(rows)
						outputRows(plan, rows)
					}
				default:
				}
			}
			var err error
			var currentPosition, stopPosition replication.Position
			currentPosition, err = binlogplayer.DecodePosition(gtid)
			if err != nil {
				fmt.Printf("Error decoding position for %s:%vs\n", gtid, err.Error())
			}
			stopPosition, err = binlogplayer.DecodePosition(stopPos)
			if err != nil {
				fmt.Printf("Error decoding position for %s:%vs\n", stopPos, err.Error())
			}
			if currentPosition.AtLeast(stopPosition) {
				log.Infof("Finished streaming keyspace %s from %s upto %s, total rows seen %d", keyspace, startPos, stopPos, totalRowsForTable)
				return "", "", true, true, nil
			}

			if testResumability {
				return gtid, stopPos, false, fieldsPrinted, nil
			}
		case io.EOF:
			log.Infof("stream ended before reaching stop pos")
			fmt.Printf("stream ended before reaching stop pos\n")
			return "", "", false, fieldsPrinted, nil
		default:
			log.Errorf("remote error: %s, returning gtid %s, stopPos %s", err, gtid, stopPos)
			fmt.Printf("remote error: %s, returning gtid %s, stopPos %s\n", err.Error(), gtid, stopPos)
			return gtid, stopPos, false, fieldsPrinted, err
		}
	}
}

func output(filename, s string) {
	f, err := os.OpenFile(filename+".log",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Errorf(err.Error())
	}
	defer f.Close()
	if _, err := f.WriteString(s + "\n"); err != nil {
		log.Errorf(err.Error())
	}
	log.Infof("Writing to %s.log: %s", filename, s)
}

func outputHeader(plan *TablePlan) {
	s := getHeader(plan)
	output(plan.keyspace, s)
}

func getHeader(plan *TablePlan) string {
	s := ""
	for _, field := range plan.fields {
		s += field.Name + "\t"
	}
	s += "op\ttimestamp\tgtid"
	return s
}

func outputRows(plan *TablePlan, rows []*RowLog) {
	for _, row := range rows {
		s := ""
		for _, val := range row.values {
			s += val + "\t"
		}
		s += fmt.Sprintf("%s\t%s\t%s", row.op, row.when, row.gtid)
		output(plan.keyspace, s)
	}
}

func mustSend(plan *TablePlan, afterVals, beforeVals []string) bool {
	if len(afterVals) > 0 {
		if _, ok := plan.allowedIds[afterVals[plan.pkIndex]]; ok {
			return true
		}
	}
	if len(beforeVals) > 0 {
		if _, ok := plan.allowedIds[beforeVals[plan.pkIndex]]; ok {
			return true
		}
	}
	return false
}

type RowLog struct {
	op     string
	values []string
	when   string
	gtid   string
}

func processRowEvent(plan *TablePlan, gtid string, ev *binlogdatapb.VEvent) []*RowLog {
	op := "insert"
	var rowLogs []*RowLog
	for _, change := range ev.RowEvent.RowChanges {
		var after, before []sqltypes.Value
		var afterVals, beforeVals []string
		if change.After != nil {
			after = sqltypes.MakeRowTrusted(plan.fields, change.After)
		}
		if change.Before != nil {
			before = sqltypes.MakeRowTrusted(plan.fields, change.Before)
		}
		for _, val := range after {
			bytes, err := val.ToBytes()
			if err != nil {
				panic(err)
			}
			afterVals = append(afterVals, string(bytes))
		}
		for _, val := range before {
			bytes, err := val.ToBytes()
			if err != nil {
				panic(err)
			}
			beforeVals = append(beforeVals, string(bytes))
		}
		if !mustSend(plan, afterVals, beforeVals) {
			continue
		}

		if len(after) > 0 && len(before) > 0 {
			op = "update"
		} else if len(before) > 0 {
			op = "delete"
			afterVals = beforeVals
		}

		rowLog := &RowLog{
			op:     op,
			values: afterVals,
			when:   time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
			gtid:   gtid,
		}
		rowLogs = append(rowLogs, rowLog)
	}
	return rowLogs
}

func getTablePlan(keyspace string, fields []*querypb.Field, table, pk string, ids []string) *TablePlan {
	allowedIds := make(map[string]bool)
	for _, id := range ids {
		allowedIds[id] = true
	}
	var pkIndex int64
	for i, field := range fields {
		if field.Name == pk {
			pkIndex = int64(i)
			break
		}
	}
	return &TablePlan{
		table:      table,
		pk:         pk,
		allowedIds: allowedIds,
		pkIndex:    pkIndex,
		fields:     fields,
		keyspace:   keyspace,
	}
}

type TablePlan struct {
	table, pk  string
	allowedIds map[string]bool
	pkIndex    int64
	fields     []*querypb.Field
	keyspace   string
}

func getFlavor(ctx context.Context, server, keyspace string) string {
	curPos, err := getPosition(ctx, server, keyspace, "0")
	if err != nil {
		return ""
	}
	if curPos == "" {
		return ""
	}
	flavor := strings.Split(curPos, "/")[0]
	return flavor
}

func getTablet(ctx context.Context, ts *topo.Server, cells []string, keyspace string) string {
	picker, err := discovery.NewTabletPicker(
		ctx,
		ts,
		cells,
		"",
		keyspace,
		"0",
		"primary",
		discovery.TabletPickerOptions{
			CellPreference: "OnlySpecified",
		},
	)
	if err != nil {
		return ""
	}
	tab, err := picker.PickForStreaming(ctx)
	if err != nil {
		return ""
	}
	tabletId := fmt.Sprintf("%s-%d", tab.Alias.Cell, tab.Alias.Uid)
	return tabletId

}
func trickGlog() {
	var args []string
	os.Args, args = os.Args[:1], os.Args[1:]
	defer func() { os.Args = append(os.Args, args...) }()
	flag.Parse()
}

var testResumability bool

func parseCommandLine() *RowLogConfig {
	trickGlog()
	sourceKeyspace := pflag.StringP("source", "s", "", "")
	targetKeyspace := pflag.StringP("target", "t", "", "")
	ids := pflag.StringSlice("ids", nil, "")
	pk := pflag.String("pk", "", "")
	table := pflag.String("table", "", "")
	vtgate := pflag.String("vtgate", "", "")
	vtctld := pflag.String("vtctld", "", "")
	cells := pflag.StringSlice("cells", nil, "")

	pflag.BoolVar(&testResumability, "test_resumability", testResumability, "set to test stream resumability")

	pflag.Parse()

	return &RowLogConfig{
		sourceKeyspace: *sourceKeyspace,
		targetKeyspace: *targetKeyspace,
		table:          *table,
		pk:             *pk,
		ids:            *ids,
		vtctld:         *vtctld,
		vtgate:         *vtgate,
		cells:          *cells,
	}
}

func processPositionResult(gtidset string) (string, string) {
	gtids := strings.Trim(strings.Replace(gtidset, "|", "", 10), " \n")
	arr := strings.Split(gtids, ":")
	subs := strings.Split(arr[1], "-")
	id, err := strconv.Atoi(subs[0])
	if err != nil {
		fmt.Println(err.Error())
		return "", ""
	}
	firstPos := arr[0] + ":" + strconv.Itoa(id) // subs[0]
	lastPos := gtids
	return firstPos, lastPos
}

// hack, should read json in a structured manner
func parseExecOutput(result string) string {
	resultMap := make(map[string]any)
	err := json.Unmarshal([]byte(result), &resultMap)
	if err != nil {
		fmt.Printf("error parsing result json %s", result)
		return ""
	}
	rows := reflect.ValueOf(resultMap["rows"])
	s := fmt.Sprintf("%v", rows)
	s = strings.Trim(s, "[]")
	return s
}

func getPositions(ctx context.Context, server, tablet string) (string, string, error) {
	query := "select GTID_SUBTRACT(@@GLOBAL.gtid_executed, GTID_SUBTRACT(@@GLOBAL.gtid_executed, @@GLOBAL.gtid_purged));"
	results, err := execVtctl(ctx, server, []string{"ExecuteFetchAsDba", "--json", tablet, query})
	if err != nil {
		fmt.Println(err)
		log.Errorf(err.Error())
		return "", "", err
	}
	firstPos := parseExecOutput(strings.Join(results, ""))

	query = "select @@GLOBAL.gtid_executed;"
	results, err = execVtctl(ctx, server, []string{"ExecuteFetchAsDba", "--json", tablet, query})
	if err != nil {
		fmt.Println(err)
		log.Errorf(err.Error())
		return "", "", err
	}
	lastPos := parseExecOutput(strings.Join(results, ""))
	return firstPos, lastPos, nil
}

func getPosition(ctx context.Context, server, keyspace, shard string) (string, error) {
	results, err := execVtctl(ctx, server, []string{"ShardReplicationPositions", fmt.Sprintf("%s:%s", keyspace, shard)})
	if err != nil {
		fmt.Println(err)
		return "", err
	}
	splits := strings.Split(results[0], " ")
	return splits[8], nil
}

func execVtctl(ctx context.Context, server string, args []string) ([]string, error) {
	client, err := vtctlclient.New(ctx, server)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	if client == nil {
		fmt.Printf("Unable to contact %s\n", server)
		return nil, fmt.Errorf("unable to contact %s", server)
	}
	defer client.Close()

	stream, err := client.ExecuteVtctlCommand(ctx, args, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot execute remote command: %v", err)
	}

	var results []string
	for {
		e, err := stream.Recv()
		switch err {
		case nil:
			if e.Level == logutilpb.Level_CONSOLE {
				results = append(results, e.Value)
			}
		case io.EOF:
			return results, nil
		default:
			log.Errorf("remote error: %v", err)
			return nil, fmt.Errorf("remote error: %v", err)
		}
	}
}
