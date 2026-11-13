package dbcache

// ============================================================================
// BackfillTable 迁移播种 + P5 运维指标的验收：
//   - BackfillTable 幂等（NX 不覆盖已有行、墓碑不复活）、无源明确报错、
//     真 MySQL srcPage 分页播种（env 闸缺省 Skip）。
//   - 四个指标（sink lag / tx conflict / scan budget / sink absent）必须被
//     真实路径触发并经 Prometheus registry 可读（delta 断言，不依赖绝对值）。
// ============================================================================

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"touchgocore/config"
	mysqldb "touchgocore/db/mysql"
	"touchgocore/metrics"
)

// TestBackfillTableIdempotent 分页播种幂等：首次全量入 Redis；源行变化后重跑
// 不覆盖已有行；已删除行（墓碑存活）不复活。
func TestBackfillTableIdempotent(t *testing.T) {
	e, _ := gapEngine(t, true)
	ctx := context.Background()
	b, err := e.lookupTable(gapMemberTable)
	if err != nil {
		t.Fatalf("lookupTable: %v", err)
	}
	// 内存源：5 行，页大小 2 → 3 页（2/2/1）。
	src := []*gapMember{{ID: 1, Point: 10}, {ID: 2, Point: 20}, {ID: 3, Point: 30}, {ID: 4, Point: 40}, {ID: 5, Point: 50}}
	b.srcPage = func(_ context.Context, offset, limit int) ([]any, error) {
		if offset >= len(src) {
			return nil, nil
		}
		end := offset + limit
		if end > len(src) {
			end = len(src)
		}
		out := make([]any, 0, end-offset)
		for _, r := range src[offset:end] {
			out = append(out, r)
		}
		return out, nil
	}

	n, err := BackfillTable[gapMember](ctx, gapMemberTable, 2)
	if err != nil || n != 5 {
		t.Fatalf("首次播种应 5 行，实际 n=%d err=%v", n, err)
	}
	for _, r := range src {
		if m := readGapMember(t, r.ID); m.Point != r.Point {
			t.Fatalf("行 %d 应播种 point=%d，实际 %d", r.ID, r.Point, m.Point)
		}
	}

	// 源行变化后重跑：NX 不覆盖，Redis 保持首次播种值。
	src[0].Point = 999
	n2, err := BackfillTable[gapMember](ctx, gapMemberTable, 2)
	if err != nil || n2 != 0 {
		t.Fatalf("重跑应 0 新行，实际 n=%d err=%v", n2, err)
	}
	if m := readGapMember(t, 1); m.Point != 10 {
		t.Fatalf("已有行不得被覆盖，期望 10，实际 %d", m.Point)
	}

	// 删除行 2 后重跑：墓碑存活，NX 跳过，不复活。
	two := int64(2)
	if err := Delete(ctx, gapMemberTable, &gapCond{ID: &two}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	n3, err := BackfillTable[gapMember](ctx, gapMemberTable, 2)
	if err != nil || n3 != 0 {
		t.Fatalf("墓碑行重跑应 0 新行，实际 n=%d err=%v", n3, err)
	}
	var gone gapMember
	if err := Query[gapMember](ctx, gapMemberTable, &gapCond{ID: &two}, &gone); !errors.Is(err, ErrNoRows) {
		t.Fatalf("已删除行不得复活，实际 %v", err)
	}
}

// TestBackfillTableNoSource 纯 Redis 绑定无 srcPage：明确报错而非静默 no-op。
func TestBackfillTableNoSource(t *testing.T) {
	gapEngine(t, true)
	_, err := BackfillTable[gapMember](context.Background(), gapMemberTable, 10)
	if err == nil || !strings.Contains(err.Error(), "backfill") {
		t.Fatalf("无源应明确报错，实际 %v", err)
	}
}

// TestBackfillTableMySQLSource 真 MySQL srcPage 分页播种（env 闸缺省 Skip）：
// 7 行源表按页大小 3 全量入 Redis，重跑幂等。临时表用后即 DROP。
func TestBackfillTableMySQLSource(t *testing.T) {
	host := os.Getenv(gapEnvMySQLHost)
	if host == "" {
		t.Skipf("跳过：未设 %s（BackfillTable 真 MySQL 源）", gapEnvMySQLHost)
	}
	dbName := os.Getenv(gapEnvMySQLDB)
	if !strings.Contains(dbName, "test") {
		t.Fatalf("对拍库名必须含 test（安全闸），实际 %q", dbName)
	}
	mc, err := mysqldb.NewClient(&config.MySqlDBConfig{
		Host: host, Username: os.Getenv(gapEnvMySQLUser),
		Password: os.Getenv(gapEnvMySQLPassword), DBName: dbName, Loc: "UTC",
	})
	if err != nil {
		t.Skipf("跳过：MySQL 不可用: %v", err)
	}
	defer mc.Close()
	gdb := mc.Engine()
	if err := gdb.Exec("CREATE TABLE IF NOT EXISTS gap_members (id BIGINT PRIMARY KEY, point BIGINT, state VARCHAR(32))").Error; err != nil {
		t.Fatalf("DDL: %v", err)
	}
	t.Cleanup(func() { gdb.Exec("DROP TABLE IF EXISTS gap_members") })
	gdb.Exec("TRUNCATE TABLE gap_members")
	for i := 1; i <= 7; i++ {
		if err := gdb.Exec("INSERT INTO gap_members (id,point,state) VALUES (?,?,?)", i, i*10, "s").Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Redis 侧：挂源绑定（BindMySQL 读 defaultEngine，先安装）。
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = "gapbf20261111"
	cfg.RequireAOF = false
	e := newEngine(cfg, fs)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	if err := BindMySQL[gapMember](gapMemberTable, mc, BindOption[gapMember]{}); err != nil {
		t.Fatalf("BindMySQL: %v", err)
	}

	ctx := context.Background()
	n, err := BackfillTable[gapMember](ctx, gapMemberTable, 3)
	if err != nil || n != 7 {
		t.Fatalf("7 行源应全量播种，实际 n=%d err=%v", n, err)
	}
	if m := readGapMember(t, 7); m.Point != 70 {
		t.Fatalf("行 7 应 point=70，实际 %d", m.Point)
	}
	n2, err := BackfillTable[gapMember](ctx, gapMemberTable, 3)
	if err != nil || n2 != 0 {
		t.Fatalf("重跑应幂等 0 新行，实际 n=%d err=%v", n2, err)
	}
}

// gapMetricValue 从 Prometheus registry 读指定表标签的指标当前值（找不到为 0）。
func gapMetricValue(t *testing.T, name, table string) float64 {
	t.Helper()
	fams, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			hit := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "table" && lp.GetValue() == table {
					hit = true
				}
			}
			if !hit {
				continue
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// TestDBCacheMetricsRecorded 四个运维指标必须由真实路径触发：
// scan budget（超预算列表查询）、sink absent（mustSave 无 sink）、
// tx conflict（锁等待超时）、sink lag（flush tick 探测脏账）。
func TestDBCacheMetricsRecorded(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = "gapmetrics20261111"
	cfg.RequireAOF = false
	cfg.MaxScanRows = 1 // 扫描预算钳到 1：两行即超限
	cfg.TxLockWaitMS = 200
	e := newEngine(cfg, fs)
	b := bindGap[gapMember](t, e, gapMemberTable, BindOption[gapMember]{})
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })

	seedGapMember(t, &gapMember{ID: 1, Point: 10})
	seedGapMember(t, &gapMember{ID: 2, Point: 20})

	// scan budget：两行超预算 → fail-closed + 计数。
	before := gapMetricValue(t, "touchgocore_dbcache_scan_budget_exceeded_total", gapMemberTable)
	var list []gapMember
	err := Query[gapMember](ctx, gapMemberTable, nil, &list, func(o *Options) {
		o.Orders = []Order{{Column: "id"}}
	})
	if !errors.Is(err, ErrScanBudgetExceeded) {
		t.Fatalf("应超预算 fail-closed，实际 %v", err)
	}
	if after := gapMetricValue(t, "touchgocore_dbcache_scan_budget_exceeded_total", gapMemberTable); after <= before {
		t.Fatalf("scan budget 计数未增长: %v → %v", before, after)
	}

	// sink absent：mustSave 无 sink（SinkRequired=false）→ 告警放行 + 计数。
	before = gapMetricValue(t, "touchgocore_dbcache_sink_absent_total", gapMemberTable)
	if err := Save[gapMember](ctx, gapMemberTable, "", &gapMember{ID: 3, Point: 30}, true); err != nil {
		t.Fatalf("mustSave 无 sink 应告警放行，实际 %v", err)
	}
	if after := gapMetricValue(t, "touchgocore_dbcache_sink_absent_total", gapMemberTable); after <= before {
		t.Fatalf("sink absent 计数未增长: %v → %v", before, after)
	}

	// tx conflict：A 持锁，B 等待超时 → 计数。
	before = gapMetricValue(t, "touchgocore_dbcache_tx_conflict_total", gapMemberTable)
	id := int64(1)
	cond := &gapCond{ID: &id}
	txA, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx A: %v", err)
	}
	var mA gapMember
	if err := TxLockOne[gapMember](txA, txA.Context(), gapMemberTable, cond, &mA); err != nil {
		t.Fatalf("A lock read: %v", err)
	}
	txB, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx B: %v", err)
	}
	var mB gapMember
	if err := TxLockOne[gapMember](txB, txB.Context(), gapMemberTable, cond, &mB); !errors.Is(err, ErrTxConflict) {
		t.Fatalf("B 应冲突，实际 %v", err)
	}
	_ = txB.Rollback()
	_ = txA.Rollback()
	if after := gapMetricValue(t, "touchgocore_dbcache_tx_conflict_total", gapMemberTable); after <= before {
		t.Fatalf("tx conflict 计数未增长: %v → %v", before, after)
	}

	// sink lag：挂 sink 后 flush tick 探测脏账 → gauge 记录待物化数。
	b.sinkBatch = func(_ context.Context, _ []any) error { return nil }
	e.flushDue(b)
	if lag := gapMetricValue(t, "touchgocore_dbcache_sink_lag", gapMemberTable); lag < 1 {
		t.Fatalf("sink lag 应 >=1（3 行脏账），实际 %v", lag)
	}
}

// TestSeqSeedRaisesOnly 序列播种只抬不降：播种值低于当前值时不得把号压回去
// （并发下压号会让两个调用方拿到同一个 id，只追加台账就会丢行）。
func TestSeqSeedRaisesOnly(t *testing.T) {
	_, fs := gapEngine(t, true)
	ctx := context.Background()
	key := "{seqseed}:seq"
	if err := fs.SeqSeed(ctx, key, 500); err != nil {
		t.Fatalf("SeqSeed 500: %v", err)
	}
	if err := fs.SeqSeed(ctx, key, 100); err != nil {
		t.Fatalf("SeqSeed 100: %v", err)
	}
	if got, _ := fs.SeqAlloc(ctx, key, 0); got != 500 {
		t.Fatalf("播种只抬不降，期望 500，实际 %d", got)
	}
	if err := fs.SeqSeed(ctx, key, 900); err != nil {
		t.Fatalf("SeqSeed 900: %v", err)
	}
	if got, _ := fs.SeqAlloc(ctx, key, 0); got != 900 {
		t.Fatalf("播种应抬到 900，实际 %d", got)
	}
}

// TestBackfillSeedsAutoIncrementSeq 回填必须把自增序列抬到源表最大 id 之上：
// 否则新前缀冷启动后第一次取号会撞上刚灌进来的行键，Insert 返回 affected=0，
// 调用方按「插入成功」继续就丢行。
func TestBackfillSeedsAutoIncrementSeq(t *testing.T) {
	// 自增列只在 BindOption 里声明（行结构体用的是 cache tag，不是 gorm tag），
	// 所以这里单独装一台引擎按 AutoIncrement 绑定，不用 gapEngine 的通用绑定。
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = "gapbfseq20261113"
	cfg.RequireAOF = false
	e := newEngine(cfg, fs)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	ctx := context.Background()
	b := bindGap[gapMember](t, e, gapMemberTable, BindOption[gapMember]{AutoIncrement: "id"})
	src := []*gapMember{{ID: 100, Point: 1}, {ID: 200, Point: 2}}
	b.srcPage = func(_ context.Context, offset, limit int) ([]any, error) {
		if offset >= len(src) {
			return nil, nil
		}
		end := offset + limit
		if end > len(src) {
			end = len(src)
		}
		out := make([]any, 0, end-offset)
		for _, r := range src[offset:end] {
			out = append(out, r)
		}
		return out, nil
	}
	// 模拟 BindMySQL 给自增表挂的播种闭包（真实现取源表 MAX(id)）。
	b.seedSeq = func(ctx context.Context) (int64, error) {
		var maxID int64
		for _, r := range src {
			if r.ID > maxID {
				maxID = r.ID
			}
		}
		if err := e.store.SeqSeed(ctx, b.keys.seq, maxID); err != nil {
			return 0, err
		}
		return maxID, nil
	}

	if n, err := BackfillTable[gapMember](ctx, gapMemberTable, 10); err != nil || n != 2 {
		t.Fatalf("回填应 2 行，实际 n=%d err=%v", n, err)
	}
	// 回填后取号必须从 201 起，不能从 1 起（1 不撞，但 100/200 会撞）。
	var row gapMember
	row.Point = 3
	if _, err := Insert(ctx, gapMemberTable, &row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if row.ID != 201 {
		t.Fatalf("回填后取号应为 201（源表最大 id 200 + 1），实际 %d", row.ID)
	}
	if m := readGapMember(t, 201); m.Point != 3 {
		t.Fatalf("新行应可读回，实际 point=%d", m.Point)
	}
}
