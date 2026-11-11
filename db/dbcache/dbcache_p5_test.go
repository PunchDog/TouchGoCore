package dbcache

// ============================================================================
// P5 验收：拆卸演练 + 运维硬化。
//
// F3 拔 MySQL（nil sink）端到端：经公开装配入口 BindRedis 绑定纯 Redis 权威表，
// Save/Insert/点查/列表/Spec/Increment/CAS/事务/Delete 全绿，证明拔掉 MySQL 不
// 影响读写。mustSave 无 sink：SinkRequired=false 告警放行（Redis 写已成功），
// true 报 ErrSinkRequired（Redis 写仍已成功，错误只针对缺席的物化视图）。
// AOF 缺失只告警不拒启动（真 Redis env 闸）。claim 租约崩溃回收与版本复核跳
// 陈旧 flush 已由 P1 的 TestClaimLeaseCrashRequeue / TestFlushSkipsStaleVersion 覆盖。
// ============================================================================

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/redis/go-redis/v9"
)

// setupDetachEngine 构造纯 Redis 引擎并安装为 default，经 BindRedis（nil sink）
// 绑定一张独占表；用例结束还原 defaultEngine。
func setupDetachEngine(t *testing.T, sinkRequired bool) (*Engine, *fakeStore) {
	t.Helper()
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = "detach20261110"
	cfg.RequireAOF = false
	cfg.SinkRequired = sinkRequired
	cfg.TxLockWaitMS = 200
	e := newEngine(cfg, fs)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	if err := BindRedis[txCaseUser]("detach_user", BindOption[txCaseUser]{KeyColumns: []string{"id"}}); err != nil {
		t.Fatalf("BindRedis: %v", err)
	}
	return e, fs
}

// TestDetachDrillNilSinkFullCRUD：拔掉 MySQL（nil sink）后公开数据接口全链路在
// Redis 上读写——Save/Insert/点查/列表/Spec/Increment/CAS/事务/Delete 全绿。
func TestDetachDrillNilSinkFullCRUD(t *testing.T) {
	e, fs := setupDetachEngine(t, false)
	ctx := context.Background()
	b, err := e.lookupTable("detach_user")
	if err != nil {
		t.Fatalf("lookupTable: %v", err)
	}
	if b.srcFindOne != nil || b.srcFindList != nil || b.sinkBatch != nil || b.sinkDelete != nil || b.sinkSecrets != nil {
		t.Fatal("纯 Redis 绑定不得挂载任何源/sink 闭包")
	}
	if b.allowSourceBackfill {
		t.Fatal("纯 Redis 绑定应强制关闭回源")
	}

	// Save + Insert（新行 affected=1，冲突 affected=0）。
	if err := Save[txCaseUser](ctx, "detach_user", "", &txCaseUser{ID: 1, Name: "alice", Score: 10, State: "new"}, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if n, err := Insert[txCaseUser](ctx, "detach_user", &txCaseUser{ID: 2, Name: "bob", Score: 20, State: "new"}); err != nil || n != 1 {
		t.Fatalf("Insert 新行应 affected=1，实际 n=%d err=%v", n, err)
	}
	if n, err := Insert[txCaseUser](ctx, "detach_user", &txCaseUser{ID: 2, Name: "dup"}); err != nil || n != 0 {
		t.Fatalf("Insert 冲突应 affected=0，实际 n=%d err=%v", n, err)
	}

	// 点查 / 列表 / Spec 排序分页，全程 Redis。
	var u txCaseUser
	if err := Query[txCaseUser](ctx, "detach_user", &idCond{ID: ptrInt(1)}, &u); err != nil || u.Name != "alice" {
		t.Fatalf("点查应读 Redis alice，实际 %+v err=%v", u, err)
	}
	var list []txCaseUser
	if err := Query[txCaseUser](ctx, "detach_user", &txUserStateCond{State: ptrStr("new")}, &list); err != nil || len(list) != 2 {
		t.Fatalf("列表 state=new 应 2 行，实际 %d err=%v", len(list), err)
	}
	var top []txCaseUser
	if err := Query[txCaseUser](ctx, "detach_user", nil, &top, func(o *Options) {
		o.Orders = []Order{{Column: "score", Desc: true}}
		o.Limit = 1
	}); err != nil || len(top) != 1 || top[0].Name != "bob" {
		t.Fatalf("Spec score 降序 limit1 应为 bob，实际 %+v err=%v", top, err)
	}

	// Increment / CAS（Redis Lua 原子，资金/状态列唯一合法写路径）。
	if n, err := Increment(ctx, "detach_user", &idCond{ID: ptrInt(1)}, "score", 5); err != nil || n != 1 {
		t.Fatalf("Increment 应 affected=1，实际 n=%d err=%v", n, err)
	}
	if n, err := CompareAndSwap(ctx, "detach_user", &idCond{ID: ptrInt(1)},
		map[string]any{"state": "new"}, map[string]any{"state": "paid"}); err != nil || n != 1 {
		t.Fatalf("CAS new→paid 应 affected=1，实际 n=%d err=%v", n, err)
	}
	if n, err := CompareAndSwap(ctx, "detach_user", &idCond{ID: ptrInt(1)},
		map[string]any{"state": "new"}, map[string]any{"state": "paid"}); err != nil || n != 0 {
		t.Fatalf("CAS 期望落空应 affected=0，实际 n=%d err=%v", n, err)
	}
	if err := Query[txCaseUser](ctx, "detach_user", &idCond{ID: ptrInt(1)}, &u); err != nil || u.Score != 15 || u.State != "paid" {
		t.Fatalf("Increment+CAS 后应 score=15 state=paid，实际 %+v err=%v", u, err)
	}

	// 事务：BeginTx → TxSave → Commit 一次原子写穿 Redis（读己写）。
	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := TxSave[txCaseUser](tx, ctx, "detach_user", &txCaseUser{ID: 1, Name: "alice", Score: 99, State: "done"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := Query[txCaseUser](ctx, "detach_user", &idCond{ID: ptrInt(1)}, &u); err != nil || u.Score != 99 || u.State != "done" {
		t.Fatalf("事务提交后应 score=99 state=done，实际 %+v err=%v", u, err)
	}

	// Delete：立墓碑 + 销行，键集收缩；被删行点查 fail-closed（不回源）。
	if err := Delete(ctx, "detach_user", &idCond{ID: ptrInt(2)}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !fs.hasTomb(b.keys.tombPrefix + "2") {
		t.Fatal("Delete 应立墓碑防异步回源复活")
	}
	if err := Query[txCaseUser](ctx, "detach_user", &idCond{ID: ptrInt(2)}, &u); !errors.Is(err, ErrNoRows) {
		t.Fatalf("被删行点查应 ErrNoRows（纯 Redis 不回源），实际 %v", err)
	}
	var total specTotalRow
	if err := Query[txCaseUser](ctx, "detach_user", nil, &total, func(o *Options) {
		o.Agg = &Aggregate{Fn: "COUNT", Alias: "total"}
	}); err != nil || total.Total != 1 {
		t.Fatalf("删除后 COUNT(*) 应为 1，实际 %d err=%v", total.Total, err)
	}
}

// TestMustSaveSinkRequiredTrueErrors：SinkRequired=true 时 mustSave 无 sink 报
// ErrSinkRequired；但 Redis 权威写已成功（行 hash 存在），错误只针对缺席的 sink。
func TestMustSaveSinkRequiredTrueErrors(t *testing.T) {
	e, _ := setupDetachEngine(t, true)
	ctx := context.Background()
	err := Save[txCaseUser](ctx, "detach_user", "", &txCaseUser{ID: 1, Name: "alice", Score: 1}, true)
	if !errors.Is(err, ErrSinkRequired) {
		t.Fatalf("SinkRequired=true 无 sink 应 ErrSinkRequired，实际 %v", err)
	}
	var u txCaseUser
	if qerr := Query[txCaseUser](ctx, "detach_user", &idCond{ID: ptrInt(1)}, &u); qerr != nil || u.Name != "alice" {
		t.Fatalf("ErrSinkRequired 前 Redis 权威写应已成功，实际 %+v err=%v", u, qerr)
	}
	b, _ := e.lookupTable("detach_user")
	if b.sinkBatch != nil {
		t.Fatal("纯 Redis 绑定不应有 sink")
	}
}

// TestMustSaveSinkRequiredFalseWarns：SinkRequired=false（默认）时 mustSave 无 sink
// 告警放行（返回 nil），Redis 权威写成功。
func TestMustSaveSinkRequiredFalseWarns(t *testing.T) {
	setupDetachEngine(t, false)
	ctx := context.Background()
	if err := Save[txCaseUser](ctx, "detach_user", "", &txCaseUser{ID: 1, Name: "alice"}, true); err != nil {
		t.Fatalf("SinkRequired=false 无 sink 应告警放行 nil，实际 %v", err)
	}
	var u txCaseUser
	if err := Query[txCaseUser](ctx, "detach_user", &idCond{ID: ptrInt(1)}, &u); err != nil || u.Name != "alice" {
		t.Fatalf("告警放行后 Redis 写应成功，实际 %+v err=%v", u, err)
	}
}

// TestInitAOFMissingWarnsNotReject：RequireAOF=true 时 Init 校验 AOF，缺失只告警
// 不拒启动（兼容未开 AOF 的部署）。真 Redis env 闸，缺省 Skip。
func TestInitAOFMissingWarnsNotReject(t *testing.T) {
	addr := os.Getenv(txEnvRedisAddr)
	if addr == "" {
		t.Skipf("跳过：未设 %s（真 Redis AOF 校验）", txEnvRedisAddr)
	}
	dbNum := 0
	if raw := os.Getenv(txEnvRedisDB); raw != "" {
		dbNum, _ = strconv.Atoi(raw)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv(txEnvRedisPassword), DB: dbNum})
	defer rdb.Close()
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("跳过：Redis 不可用: %v", err)
	}
	old := defaultEngine.Swap(nil)
	t.Cleanup(func() { defaultEngine.Store(old) })
	cfg := DefaultConfig()
	cfg.RequireAOF = true
	cfg.KeyPrefix = "detachaof20261110"
	if err := Init(cfg, rdb); err != nil {
		t.Fatalf("RequireAOF=true 不应拒启动（AOF 缺失只告警），实际 %v", err)
	}
	if Default() == nil {
		t.Fatal("Init 应安装默认引擎")
	}
	Close()
}
