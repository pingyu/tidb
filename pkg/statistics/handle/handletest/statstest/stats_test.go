// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statstest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/statistics"
	statstestutil "github.com/pingcap/tidb/pkg/statistics/handle/ddl/testutil"
	"github.com/pingcap/tidb/pkg/statistics/handle/internal"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/analyzehelper"
	"github.com/stretchr/testify/require"
)

func TestStatsCacheProcess(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (c1 int, c2 int)")
	testKit.MustExec("insert into t values(1, 2)")
	analyzehelper.TriggerPredicateColumnsCollection(t, testKit, store, "t", "c1", "c2")
	do := dom
	is := do.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	tableInfo := tbl.Meta()
	statsTbl := do.StatsHandle().GetTableStats(tableInfo)
	require.True(t, statsTbl.Pseudo)
	require.Zero(t, statsTbl.Version)
	currentVersion := do.StatsHandle().MaxTableStatsVersion()
	testKit.MustExec("analyze table t")
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl.Pseudo)
	require.NotZero(t, statsTbl.Version)
	require.Equal(t, currentVersion, do.StatsHandle().MaxTableStatsVersion())
	newVersion := do.StatsHandle().GetNextCheckVersionWithOffset()
	require.Equal(t, currentVersion, newVersion, "analyze should not move forward the stats cache version")

	// Insert more rows
	testKit.MustExec("insert into t values(2, 3)")
	require.NoError(t, do.StatsHandle().DumpStatsDeltaToKV(true))
	require.NoError(t, do.StatsHandle().Update(context.Background(), is))
	newVersion = do.StatsHandle().MaxTableStatsVersion()
	require.NotEqual(t, currentVersion, newVersion, "update with no table should move forward the stats cache version")
}

func TestStatsCache(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (c1 int, c2 int)")
	testKit.MustExec("insert into t values(1, 2)")
	analyzehelper.TriggerPredicateColumnsCollection(t, testKit, store, "t", "c1", "c2")
	do := dom
	is := do.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	tableInfo := tbl.Meta()
	statsTbl := do.StatsHandle().GetTableStats(tableInfo)
	require.True(t, statsTbl.Pseudo)
	testKit.MustExec("analyze table t")
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl.Pseudo)
	testKit.MustExec("create index idx_t on t(c1)")
	do.InfoSchema()
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	// If index is build, but stats is not updated. statsTbl can also work.
	require.False(t, statsTbl.Pseudo)
	// But the added index will not work.
	require.Nil(t, statsTbl.GetIdx(int64(1)))

	testKit.MustExec("analyze table t")
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl.Pseudo)
	// If the new schema drop a column, the table stats can still work.
	testKit.MustExec("alter table t drop column c2")
	is = do.InfoSchema()
	do.StatsHandle().Clear()
	err = do.StatsHandle().Update(context.Background(), is)
	require.NoError(t, err)
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl.Pseudo)

	// If the new schema add a column, the table stats can still work.
	testKit.MustExec("alter table t add column c10 int")
	is = do.InfoSchema()

	do.StatsHandle().Clear()
	err = do.StatsHandle().Update(context.Background(), is)
	require.NoError(t, err)
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl.Pseudo)
}

func TestStatsCacheMemTracker(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (c1 int, c2 int, c3 int)")
	analyzehelper.TriggerPredicateColumnsCollection(t, testKit, store, "t", "c1", "c2", "c3")
	testKit.MustExec("insert into t values(1, 2, 3)")
	do := dom
	is := do.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	tableInfo := tbl.Meta()
	statsTbl := do.StatsHandle().GetTableStats(tableInfo)
	require.True(t, statsTbl.MemoryUsage().TotalMemUsage == 0)
	require.True(t, statsTbl.Pseudo)

	testKit.MustExec("analyze table t")
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)

	require.False(t, statsTbl.Pseudo)
	testKit.MustExec("create index idx_t on t(c1)")
	do.InfoSchema()
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)

	// If index is build, but stats is not updated. statsTbl can also work.
	require.False(t, statsTbl.Pseudo)
	// But the added index will not work.
	require.Nil(t, statsTbl.GetIdx(int64(1)))

	testKit.MustExec("analyze table t")
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)

	require.False(t, statsTbl.Pseudo)

	// If the new schema drop a column, the table stats can still work.
	testKit.MustExec("alter table t drop column c2")
	is = do.InfoSchema()
	do.StatsHandle().Clear()
	err = do.StatsHandle().Update(context.Background(), is)
	require.NoError(t, err)

	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.True(t, statsTbl.MemoryUsage().TotalMemUsage > 0)
	require.False(t, statsTbl.Pseudo)

	// If the new schema add a column, the table stats can still work.
	testKit.MustExec("alter table t add column c10 int")
	is = do.InfoSchema()

	do.StatsHandle().Clear()
	err = do.StatsHandle().Update(context.Background(), is)
	require.NoError(t, err)
	statsTbl = do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl.Pseudo)
}

func TestStatsStoreAndLoad(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (c1 int, c2 int)")
	recordCount := 1000
	for i := 0; i < recordCount; i++ {
		testKit.MustExec("insert into t values (?, ?)", i, i+1)
	}
	testKit.MustExec("create index idx_t on t(c2)")
	analyzehelper.TriggerPredicateColumnsCollection(t, testKit, store, "t", "c1")
	do := dom
	is := do.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	tableInfo := tbl.Meta()

	testKit.MustExec("analyze table t")
	statsTbl1 := do.StatsHandle().GetTableStats(tableInfo)

	do.StatsHandle().Clear()
	err = do.StatsHandle().Update(context.Background(), is)
	require.NoError(t, err)
	statsTbl2 := do.StatsHandle().GetTableStats(tableInfo)
	require.False(t, statsTbl2.Pseudo)
	require.Equal(t, int64(recordCount), statsTbl2.RealtimeCount)
	internal.AssertTableEqual(t, statsTbl1, statsTbl2)
}

func testInitStatsMemTrace(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t1 (a int, b int, c int, primary key(a), key idx(b))")
	tk.MustExec("insert into t1 values (1,1,1),(2,2,2),(3,3,3),(4,4,4),(5,5,5),(6,7,8)")
	tk.MustExec("analyze table t1")
	for i := 2; i < 10; i++ {
		tk.MustExec(fmt.Sprintf("create table t%v (a int, b int, c int, primary key(a), key idx(b))", i))
		tk.MustExec(fmt.Sprintf("insert into t%v select * from t1", i))
		tk.MustExec(fmt.Sprintf("analyze table t%v", i))
	}
	h := dom.StatsHandle()
	is := dom.InfoSchema()
	h.Clear()
	require.Equal(t, h.MemConsumed(), int64(0))
	require.NoError(t, h.InitStats(context.Background(), is))

	var memCostTot int64
	for i := 1; i < 10; i++ {
		tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr(fmt.Sprintf("t%v", i)))
		require.NoError(t, err)
		tStats := h.GetTableStats(tbl.Meta())
		memCostTot += tStats.MemoryUsage().TotalMemUsage
	}
	tables := h.StatsCache.Values()
	for _, tt := range tables {
		tbl, ok := h.StatsCache.Get(tt.PhysicalID)
		require.True(t, ok)
		require.Equal(t, tbl.PhysicalID, tt.PhysicalID)
	}

	require.Equal(t, h.MemConsumed(), memCostTot)
}

func TestInitStatsMemTraceWithLite(t *testing.T) {
	restore := config.RestoreFunc()
	defer restore()
	testInitStatsMemTraceFunc(t, true)
}

func TestInitStatsMemTraceWithoutLite(t *testing.T) {
	restore := config.RestoreFunc()
	defer restore()
	testInitStatsMemTraceFunc(t, false)
}

func TestInitStatsMemTraceWithConcurrentLite(t *testing.T) {
	restore := config.RestoreFunc()
	defer restore()
	testInitStatsMemTraceFunc(t, true)
}

func TestInitStatsMemTraceWithoutConcurrentLite(t *testing.T) {
	restore := config.RestoreFunc()
	defer restore()
	testInitStatsMemTraceFunc(t, false)
}

func testInitStatsMemTraceFunc(t *testing.T, liteInitStats bool) {
	originValue := config.GetGlobalConfig().Performance.LiteInitStats
	defer func() {
		config.GetGlobalConfig().Performance.LiteInitStats = originValue
	}()
	config.GetGlobalConfig().Performance.LiteInitStats = liteInitStats
	testInitStatsMemTrace(t)
}

func TestInitStats(t *testing.T) {
	originValue := config.GetGlobalConfig().Performance.LiteInitStats
	defer func() {
		config.GetGlobalConfig().Performance.LiteInitStats = originValue
	}()
	config.GetGlobalConfig().Performance.LiteInitStats = false
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("set @@session.tidb_analyze_version = 1")
	testKit.MustExec("create table t(a int, b int, c int, primary key(a), key idx(b))")
	testKit.MustExec("insert into t values (1,1,1),(2,2,2),(3,3,3),(4,4,4),(5,5,5),(6,7,8)")
	testKit.MustExec("analyze table t")
	h := dom.StatsHandle()
	is := dom.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	// `Update` will not use load by need strategy when `Lease` is 0, and `InitStats` is only called when
	// `Lease` is not 0, so here we just change it.
	h.SetLease(time.Millisecond)

	h.Clear()
	require.NoError(t, h.InitStats(context.Background(), is))
	table0 := h.GetTableStats(tbl.Meta())
	h.Clear()
	require.NoError(t, h.Update(context.Background(), is))
	// Index and pk are loaded.
	needed := fmt.Sprintf(`Table:%v RealtimeCount:6
column:1 ndv:6 totColSize:0
column:2 ndv:6 totColSize:6
column:3 ndv:6 totColSize:6
index:1 ndv:6
num: 1 lower_bound: 1 upper_bound: 1 repeats: 1 ndv: 0
num: 1 lower_bound: 2 upper_bound: 2 repeats: 1 ndv: 0
num: 1 lower_bound: 3 upper_bound: 3 repeats: 1 ndv: 0
num: 1 lower_bound: 4 upper_bound: 4 repeats: 1 ndv: 0
num: 1 lower_bound: 5 upper_bound: 5 repeats: 1 ndv: 0
num: 1 lower_bound: 7 upper_bound: 7 repeats: 1 ndv: 0`, tbl.Meta().ID)
	require.Equal(t, needed, table0.String())
	h.SetLease(0)
}

func TestInitStats51358(t *testing.T) {
	originValue := config.GetGlobalConfig().Performance.LiteInitStats
	defer func() {
		config.GetGlobalConfig().Performance.LiteInitStats = originValue
	}()
	config.GetGlobalConfig().Performance.LiteInitStats = false
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("set @@session.tidb_analyze_version = 1")
	testKit.MustExec("create table t(a int, b int, c int, primary key(a), key idx(b))")
	testKit.MustExec("insert into t values (1,1,1),(2,2,2),(3,3,3),(4,4,4),(5,5,5),(6,7,8)")
	testKit.MustExec("analyze table t")
	h := dom.StatsHandle()
	is := dom.InfoSchema()
	// `Update` will not use load by need strategy when `Lease` is 0, and `InitStats` is only called when
	// `Lease` is not 0, so here we just change it.
	h.SetLease(time.Millisecond)

	h.Clear()
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/statistics/handle/cache/StatsCacheGetNil", "return()"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/statistics/handle/cache/StatsCacheGetNil"))
	}()
	require.NoError(t, h.InitStats(context.Background(), is))
	tbl, err := dom.InfoSchema().TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	stats := h.GetTableStats(tbl.Meta())
	stats.ForEachColumnImmutable(func(_ int64, column *statistics.Column) bool {
		if mysql.HasPriKeyFlag(column.Info.GetFlag()) {
			// primary key column has no stats info, because primary key's is_index is false. so it cannot load the topn
			require.Nil(t, column.TopN)
		}
		require.False(t, column.IsFullLoad())
		return false
	})
}

func TestInitStatsVer2(t *testing.T) {
	originValue := config.GetGlobalConfig().Performance.LiteInitStats
	defer func() {
		config.GetGlobalConfig().Performance.LiteInitStats = originValue
	}()
	config.GetGlobalConfig().Performance.LiteInitStats = false
	initStatsVer2(t)
}

func TestInitStatsVer2Concurrency(t *testing.T) {
	originValue := config.GetGlobalConfig().Performance.LiteInitStats
	defer func() {
		config.GetGlobalConfig().Performance.LiteInitStats = originValue
	}()
	config.GetGlobalConfig().Performance.LiteInitStats = false
	initStatsVer2(t)
}

func initStatsVer2(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set @@session.tidb_analyze_version=2")
	tk.MustExec("create table t(a int, b int, c int, d int, index idx(a), index idxab(a, b))")
	h := dom.StatsHandle()
	err := statstestutil.HandleNextDDLEventWithTxn(h)
	require.NoError(t, err)
	analyzehelper.TriggerPredicateColumnsCollection(t, tk, store, "t", "c")
	tk.MustExec("insert into t values(1, 1, 1, 1), (2, 2, 2, 2), (3, 3, 3, 3), (4, 4, 4, 4), (4, 4, 4, 4), (4, 4, 4, 4)")
	tk.MustExec("analyze table t with 2 topn, 3 buckets")
	tk.MustExec("alter table t add column e int default 1")
	err = statstestutil.HandleNextDDLEventWithTxn(h)
	require.NoError(t, err)
	is := dom.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	// `Update` will not use load by need strategy when `Lease` is 0, and `InitStats` is only called when
	// `Lease` is not 0, so here we just change it.
	h.SetLease(time.Millisecond)

	h.Clear()
	require.NoError(t, h.InitStats(context.Background(), is))
	table0 := h.GetTableStats(tbl.Meta())
	require.Equal(t, 5, table0.ColNum())
	require.True(t, table0.GetCol(1).IsAllEvicted())
	require.True(t, table0.GetCol(2).IsAllEvicted())
	require.True(t, table0.GetCol(3).IsAllEvicted())
	require.True(t, !table0.GetCol(4).IsStatsInitialized())
	require.True(t, table0.GetCol(5).IsStatsInitialized())
	require.Equal(t, 2, table0.IdxNum())
	h.Clear()
	require.NoError(t, h.InitStats(context.Background(), is))
	table1 := h.GetTableStats(tbl.Meta())
	internal.AssertTableEqual(t, table0, table1)
	h.SetLease(0)
}

func TestInitStatsIssue41938(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set @@global.tidb_analyze_version=1")
	tk.MustExec("set @@session.tidb_analyze_version=1")
	tk.MustExec("create table t1 (a timestamp primary key)")
	tk.MustExec("insert into t1 values ('2023-03-07 14:24:30'), ('2023-03-07 14:24:31'), ('2023-03-07 14:24:32'), ('2023-03-07 14:24:33')")
	tk.MustExec("analyze table t1 with 0 topn")
	h := dom.StatsHandle()
	// `InitStats` is only called when `Lease` is not 0, so here we just change it.
	h.SetLease(time.Millisecond)
	h.Clear()
	require.NoError(t, h.InitStats(context.Background(), dom.InfoSchema()))
	h.SetLease(0)
}

func TestDumpStatsDeltaInBatch(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	testKit := testkit.NewTestKit(t, store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t1 (c1 int, c2 int)")
	testKit.MustExec("insert into t1 values (1, 1), (2, 2), (3, 3)")
	testKit.MustExec("create table t2 (c1 int, c2 int)")
	testKit.MustExec("insert into t2 values (1, 1), (2, 2), (3, 3)")

	// Dump stats delta in one batch.
	handle := dom.StatsHandle()
	require.NoError(t, handle.DumpStatsDeltaToKV(true))

	// Check the mysql.stats_meta table.
	rows := testKit.MustQuery("select modify_count, count, version from mysql.stats_meta order by table_id").Rows()
	require.Len(t, rows, 2)

	require.Equal(t, "3", rows[0][0])
	require.Equal(t, "3", rows[0][1])
	require.Equal(t, "3", rows[1][0])
	require.Equal(t, "3", rows[1][1])
	require.Equal(
		t,
		rows[0][2],
		rows[1][2],
		"The version of two tables should be the same because they are dumped in the same transaction.",
	)
}
