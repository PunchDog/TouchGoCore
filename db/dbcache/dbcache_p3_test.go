package dbcache

// ============================================================================
// P3 验收：列表/非键读 Redis 权威化 + 可拆卸 sink + 秘密列落库。
//
// 全部 fake 可跑。覆盖：Redis 有行时列表/非键单条不回源、扫描预算 fail-closed、
// 资金表（AllowSourceBackfill=false）冷启动不回源、BindRedis 纯权威绑定无源/sink、
// 秘密列经 sinkSecrets 按行键落库（异步 flushAll 与同步 mustSave 两条路径）、
// 秘密落库失败重挂脏账不丢弃。
// ============================================================================

import (
	"context"
	"errors"
	"testing"
)

// TestQueryListReadsRedisNotSource：Redis 键集有行时，列表读扫 Redis 过滤，
// 绝不回源 srcFindList（Redis 唯一权威）。
func TestQueryListReadsRedisNotSource(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	ctx := context.Background()
	for _, row := range []*testUser{
		{ID: 1, Name: "alice"},
		{ID: 2, Name: "alice"},
		{ID: 3, Name: "bob"},
	} {
		if err := e.save[testUser](ctx, "test_user", "", row, false); err != nil {
			t.Fatalf("save %d: %v", row.ID, err)
		}
	}
	var out []testUser
	if err := e.query[testUser](ctx, "test_user", &nameCond{Name: ptrStr("alice")}, &out); err != nil {
		t.Fatalf("query list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("name=alice 应命中 2 行，实际 %d", len(out))
	}
	if src.listCalls != 0 {
		t.Fatalf("Redis 有行时列表读不得回源，src.listCalls=%d", src.listCalls)
	}
}

// TestQueryNonKeySingleReadsRedis：非键条件单条读扫 Redis 取首条命中，不回源。
func TestQueryNonKeySingleReadsRedis(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 5, Name: "carol"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	var out testUser
	if err := e.query[testUser](ctx, "test_user", &nameCond{Name: ptrStr("carol")}, &out); err != nil {
		t.Fatalf("query non-key single: %v", err)
	}
	if out.ID != 5 {
		t.Fatalf("非键单条应读 Redis 命中 id=5，实际 %d", out.ID)
	}
	if src.oneCalls != 0 {
		t.Fatalf("Redis 有行时非键单条不得回源，src.oneCalls=%d", src.oneCalls)
	}
}

// TestQueryListScanBudgetFailsClosed：键集行数超 MaxScanRows 时列表读 fail-closed，
// 返回 ErrScanBudgetExceeded，绝不静默截断或回源。
func TestQueryListScanBudgetFailsClosed(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	e.cfg.MaxScanRows = 2
	for i := int64(1); i <= 3; i++ {
		if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: i, Name: "x"}, false); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	var out []testUser
	err := e.query[testUser](ctx, "test_user", &nameCond{Name: ptrStr("x")}, &out)
	if !errors.Is(err, ErrScanBudgetExceeded) {
		t.Fatalf("超扫描预算应 fail-closed ErrScanBudgetExceeded，实际 %v", err)
	}
}

// TestQueryListFundsTableNoBackfill：资金表（AllowSourceBackfill=false）冷启动
// 键集空时返回空列表，即便 MySQL 有历史行也绝不回源。
func TestQueryListFundsTableNoBackfill(t *testing.T) {
	e, _, src, _, b := newTestEngine(t)
	ctx := context.Background()
	b.allowSourceBackfill = false
	src.list = []*testUser{{ID: 9, Name: "history"}}
	var out []testUser
	if err := e.query[testUser](ctx, "test_user", &nameCond{Name: ptrStr("history")}, &out); err != nil {
		t.Fatalf("资金表冷启动列表读应返回空而非错误，实际 %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("资金表键集空应返回空列表，实际 %d 行", len(out))
	}
	if src.listCalls != 0 {
		t.Fatalf("资金表绝不回源，src.listCalls=%d", src.listCalls)
	}
}

// TestBindRedisPureAuthority：BindRedis（nil client）是纯 Redis 权威绑定——
// 强制关闭回源、不挂任何源/sink 闭包；Save/点查/列表全在 Redis，mustSave 无 sink
// 告警放行（SinkRequired=false）。
func TestBindRedisPureAuthority(t *testing.T) {
	fs := newFakeStore()
	e := newEngine(DefaultConfig(), fs)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	ctx := context.Background()

	if err := BindRedis[testUser]("pure_user", BindOption[testUser]{AllowSourceBackfill: true}); err != nil {
		t.Fatalf("BindRedis: %v", err)
	}
	b, err := e.lookupTable("pure_user")
	if err != nil {
		t.Fatalf("lookupTable: %v", err)
	}
	if b.allowSourceBackfill {
		t.Fatal("纯 Redis 绑定应强制关闭回源")
	}
	if b.srcFindOne != nil || b.srcFindList != nil || b.sinkBatch != nil || b.sinkDelete != nil || b.sinkSecrets != nil {
		t.Fatal("纯 Redis 绑定不得挂载任何源/sink 闭包")
	}

	if err := Save[testUser](ctx, "pure_user", "1", &testUser{ID: 1, Name: "alice"}, true); err != nil {
		t.Fatalf("mustSave 无 sink 应告警放行，实际 %v", err)
	}
	var out testUser
	if err := Query[testUser](ctx, "pure_user", &idCond{ID: ptrInt(1)}, &out); err != nil {
		t.Fatalf("Query 点查: %v", err)
	}
	if out.Name != "alice" {
		t.Fatalf("点查应读 Redis，实际 %q", out.Name)
	}
	var list []testUser
	if err := Query[testUser](ctx, "pure_user", &nameCond{Name: ptrStr("alice")}, &list); err != nil {
		t.Fatalf("Query 列表: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("列表应命中 1 行，实际 %d", len(list))
	}
}

// TestFlushPersistsSecretsViaSink：异步 flushAll 把 :s: hash 的秘密列经 sinkSecrets
// 按行键落库；秘密不进模型行，落库成功后脏账清空。
func TestFlushPersistsSecretsViaSink(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	ctx := context.Background()
	var gotVals []condVal
	var gotSecrets map[string]any
	calls := 0
	b.sinkSecrets = func(_ context.Context, vals []condVal, secrets map[string]any) error {
		calls++
		gotVals = vals
		gotSecrets = secrets
		return nil
	}
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice", Point: 100}, false, map[string]any{"passwd_hash": "s3cr3t"})
	if fs.hash(b.keys.secretPrefix+"7")["passwd_hash"] != "s3cr3t" {
		t.Fatal("秘密列应进 :s: hash")
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if calls != 1 {
		t.Fatalf("sinkSecrets 应调用一次，实际 %d", calls)
	}
	if len(gotVals) != 1 || gotVals[0].Column != "id" || gotVals[0].Values[0] != int64(7) {
		t.Fatalf("秘密落库行键应为 id=7，实际 %v", gotVals)
	}
	if gotSecrets["passwd_hash"] != "s3cr3t" {
		t.Fatalf("秘密列值应透传，实际 %v", gotSecrets)
	}
	if fs.dirtyCount(b.keys.dirty) != 0 {
		t.Fatal("落库成功后脏账应清空")
	}
}

// TestMustSavePersistsSecrets：同步 mustSave 路径（flushKey）同样落库秘密列，
// 与异步路径同口径，避免 mustSave+SecretSet 静默丢秘密。
func TestMustSavePersistsSecrets(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	calls := 0
	b.sinkSecrets = func(_ context.Context, vals []condVal, secrets map[string]any) error {
		calls++
		if len(vals) != 1 || vals[0].Values[0] != int64(7) {
			t.Fatalf("秘密落库行键应为 id=7，实际 %v", vals)
		}
		if secrets["passwd_hash"] != "s3cr3t" {
			t.Fatalf("秘密列值应透传，实际 %v", secrets)
		}
		return nil
	}
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice"}, true, map[string]any{"passwd_hash": "s3cr3t"})
	if calls != 1 {
		t.Fatalf("mustSave 应同步落库秘密一次，实际 %d", calls)
	}
	if fs.dirtyCount(b.keys.dirty) != 0 {
		t.Fatal("mustSave 落库成功后脏账应清空")
	}
}

// TestFlushSecretsFailureRequeues：秘密落库失败时重挂脏账（不丢弃）并上抛错误；
// flushAll 用 MaxInt64 领取，失败必须退出本轮而非死循环。
func TestFlushSecretsFailureRequeues(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	ctx := context.Background()
	b.sinkSecrets = func(_ context.Context, _ []condVal, _ map[string]any) error {
		return errSinkDown
	}
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice"}, false, map[string]any{"passwd_hash": "s3cr3t"})
	err := e.flushAll(ctx, b)
	if !errors.Is(err, errSinkDown) {
		t.Fatalf("秘密落库失败应上抛 errSinkDown，实际 %v", err)
	}
	if fs.dirtyCount(b.keys.dirty) == 0 {
		t.Fatal("秘密落库失败应重挂脏账（不丢弃）")
	}
}