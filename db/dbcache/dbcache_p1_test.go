package dbcache

// ============================================================================
// P1 验收：Redis 权威写核心（键集/版本/索引/唯一守卫/秘密列/claim 租约）。
//
// 全部 fake 可跑；TestRedisParityWriteOps 用真 Redis 差分对拍 Lua 与 semantics.go
// 参考实现（env 闸沿用 TASKENGINE_TEST_REDIS_*，缺省 Skip）。
// TestCoreFilesHaveNoSQLImports 是架构闸：已 Redis 化的核心文件禁止 SQL import，
// 随 P2-P4 推进扩充清单，防「悄悄接回 gorm」。
// ============================================================================

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// p1Row 覆盖索引（num+lex）、唯一守卫、自增主键四类声明。
type p1Row struct {
	ID     int64  `cache:"id,pk"`
	Uname  string `cache:"uname"`
	Point  int64  `cache:"point"`
	Status int32  `cache:"status"`
}

// p1Cond 行键条件。
type p1Cond struct {
	ID *int64 `cache:"id"`
}

// newP1Engine 装配带索引/唯一/自增/秘密白名单的绑定与记录型 sink。
func newP1Engine(t *testing.T) (*Engine, *fakeStore, *binding, *fakeSink) {
	t.Helper()
	fs := newFakeStore()
	e := newEngine(DefaultConfig(), fs)
	keys, err := makeTableKeys(e.cfg.KeyPrefix, "p1_row", false)
	if err != nil {
		t.Fatalf("makeTableKeys: %v", err)
	}
	b, err := newBinding[p1Row]("p1_row", backendMySQL, BindOption[p1Row]{
		Indexes:       []string{"point", "uname"},
		UniqueColumns: [][]string{{"uname"}},
		AutoIncrement: "id",
		SecretColumns: []string{"passwd_hash", "pay_passwd_hash"},
	}, keys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding: %v", err)
	}
	sink := &fakeSink{}
	b.sinkBatch = func(_ context.Context, rows []any) error {
		// fakeSink 只收 testUser；这里只计数，内容断言走 Redis 侧。
		sink.mu.Lock()
		sink.calls += len(rows)
		sink.mu.Unlock()
		return nil
	}
	b.sinkDelete = func(_ context.Context, _ []condVal) error { return nil }
	if err := e.register(b); err != nil {
		t.Fatalf("register: %v", err)
	}
	return e, fs, b, sink
}

// p1Save 整行写的快捷封装。
func p1Save(t *testing.T, e *Engine, row *p1Row, mustSave bool, secrets map[string]any) {
	t.Helper()
	if err := e.save[p1Row](context.Background(), "p1_row", "", row, mustSave, &Options{SecretSet: secrets}); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func TestWriteSaveMaintainsKeySetAndVersion(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice", Point: 100}, false, nil)

	row := fs.hash(b.keys.rowPrefix + "7")
	if row[versionField] != "1" {
		t.Fatalf("首写版本应为 1，实际 %q", row[versionField])
	}
	if _, ok := fs.m.sets[b.keys.keySet]["7"]; !ok {
		t.Fatal("键集应包含 7")
	}
	if score := fs.m.zsets[b.keys.ixPrefix+"point"]["7"]; score != 100 {
		t.Fatalf("num 索引 score=100，实际 %v", score)
	}
	if _, ok := fs.m.zsets[b.keys.ixPrefix+"uname"]["alice\x007"]; !ok {
		t.Fatal("lex 索引应含 alice\\x007")
	}

	// 二写：版本+1，lex 索引旧 member 删、新 member 建。
	p1Save(t, e, &p1Row{ID: 7, Uname: "bob", Point: 50}, false, nil)
	row = fs.hash(b.keys.rowPrefix + "7")
	if row[versionField] != "2" {
		t.Fatalf("二写版本应为 2，实际 %q", row[versionField])
	}
	z := fs.m.zsets[b.keys.ixPrefix+"uname"]
	if _, ok := z["alice\x007"]; ok {
		t.Fatal("旧 lex member 应被删除")
	}
	if _, ok := z["bob\x007"]; !ok {
		t.Fatal("新 lex member 应建立")
	}
	if score := fs.m.zsets[b.keys.ixPrefix+"point"]["7"]; score != 50 {
		t.Fatalf("num 索引应更新到 50，实际 %v", score)
	}
}

func TestInsertUniqueGuardConflictZero(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	ctx := context.Background()
	n, err := e.Insert(ctx, "p1_row", &p1Row{ID: 7, Uname: "alice"})
	if err != nil || n != 1 {
		t.Fatalf("首插 n=%d err=%v", n, err)
	}
	// 唯一列冲突：affected=0，不覆盖、不建键集 entry。
	n, err = e.Insert(ctx, "p1_row", &p1Row{ID: 8, Uname: "alice"})
	if err != nil || n != 0 {
		t.Fatalf("唯一冲突应 n=0 err=nil，实际 n=%d err=%v", n, err)
	}
	if _, ok := fs.m.sets[b.keys.keySet]["8"]; ok {
		t.Fatal("冲突行不得进键集")
	}
	// 行键冲突：同样 affected=0。
	n, err = e.Insert(ctx, "p1_row", &p1Row{ID: 7, Uname: "other"})
	if err != nil || n != 0 {
		t.Fatalf("行键冲突应 n=0，实际 n=%d err=%v", n, err)
	}
	if fs.hash(b.keys.rowPrefix + "7")["uname"] != "alice" {
		t.Fatal("冲突不得覆盖既有行")
	}
	// 不同唯一值：正常插入。
	n, err = e.Insert(ctx, "p1_row", &p1Row{ID: 8, Uname: "bob"})
	if err != nil || n != 1 {
		t.Fatalf("无冲突应 n=1，实际 n=%d err=%v", n, err)
	}
}

func TestInsertAutoIncrementBackfillRow(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	ctx := context.Background()
	r1 := &p1Row{Uname: "a"}
	r2 := &p1Row{Uname: "b"}
	if _, err := e.Insert(ctx, "p1_row", r1); err != nil {
		t.Fatalf("insert1: %v", err)
	}
	if _, err := e.Insert(ctx, "p1_row", r2); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	if r1.ID != 1 || r2.ID != 2 {
		t.Fatalf("自增 ID 应即时回填结构体：r1=%d r2=%d", r1.ID, r2.ID)
	}
	_, ok1 := fs.m.sets[b.keys.keySet]["1"]
	_, ok2 := fs.m.sets[b.keys.keySet]["2"]
	if !ok1 || !ok2 {
		t.Fatal("取号行应进键集")
	}
	if fs.m.strs[b.keys.seq] != "2" {
		t.Fatalf("序列应为 2，实际 %q", fs.m.strs[b.keys.seq])
	}
}

func TestDeleteClearsIndexesGuardsSecrets(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice", Point: 100}, false, map[string]any{"passwd_hash": "secret"})
	if len(fs.hash(b.keys.secretPrefix+"7")) == 0 {
		t.Fatal("秘密列应进 :s:")
	}
	if err := e.remove(context.Background(), b, &p1Cond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(fs.hash(b.keys.rowPrefix+"7")) != 0 {
		t.Fatal("行 hash 应删")
	}
	if len(fs.hash(b.keys.secretPrefix+"7")) != 0 {
		t.Fatal("秘密 hash 应删")
	}
	if len(fs.m.sets[b.keys.keySet]) != 0 {
		t.Fatal("键集应清")
	}
	if len(fs.m.zsets[b.keys.ixPrefix+"point"]) != 0 || len(fs.m.zsets[b.keys.ixPrefix+"uname"]) != 0 {
		t.Fatal("索引应清")
	}
	if len(fs.m.hashes[b.keys.uq]) != 0 {
		t.Fatal("唯一守卫应清")
	}
	if !fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("应立墓碑")
	}
	if fs.dirtyCount(b.keys.dirtyDel) != 1 {
		t.Fatal("有 sink 时应入 dirtyDel")
	}
}

func TestIncrementGuardedConditionLive(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Point: 100}, false, nil)
	ctx := context.Background()
	dec := func() int64 {
		n, err := e.atomicWrite(ctx, "p1_row", &Options{
			Cond:       &p1Cond{ID: ptrInt(7)},
			Increment:  "point",
			Delta:      -30,
			Conditions: []Range{{Column: "point", Op: ">=", Value: int64(50)}},
		})
		if err != nil {
			t.Fatalf("atomicWrite: %v", err)
		}
		return n
	}
	if n := dec(); n != 1 { // 100 → 70
		t.Fatalf("首次扣减应命中，n=%d", n)
	}
	if n := dec(); n != 1 { // 70 → 40
		t.Fatalf("二次扣减应命中，n=%d", n)
	}
	if n := dec(); n != 0 { // 40 < 50 → 拒绝
		t.Fatalf("低于下限应 affected=0，n=%d", n)
	}
	if got := fs.hash(b.keys.rowPrefix + "7")["point"]; got != "40" {
		t.Fatalf("拒绝后值应不变，实际 %q", got)
	}
	if score := fs.m.zsets[b.keys.ixPrefix+"point"]["7"]; score != 40 {
		t.Fatalf("索引应同步到 40，实际 %v", score)
	}
}

func TestAtomicMaxGuardedConditionLive(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Point: 100}, false, nil)
	ctx := context.Background()
	bump := func(value, minPoint int64) int64 {
		n, err := e.atomicWrite(ctx, "p1_row", &Options{
			Cond:       &p1Cond{ID: ptrInt(7)},
			AtomicMax:  "point",
			MaxValue:   value,
			Conditions: []Range{{Column: "point", Op: ">=", Value: minPoint}},
		})
		if err != nil {
			t.Fatalf("atomicWrite: %v", err)
		}
		return n
	}
	if n := bump(200, 100); n != 1 { // 100 → 200
		t.Fatalf("候选更大应命中，n=%d", n)
	}
	if n := bump(150, 100); n != 1 { // 候选小，值不变仍命中
		t.Fatalf("候选更小仍命中（值不变），n=%d", n)
	}
	if n := bump(999, 300); n != 0 { // 200 < 300 → 拒绝
		t.Fatalf("条件不命中应 affected=0，n=%d", n)
	}
	if got := fs.hash(b.keys.rowPrefix + "7")["point"]; got != "200" {
		t.Fatalf("拒绝后值应保持 200，实际 %q", got)
	}
	if score := fs.m.zsets[b.keys.ixPrefix+"point"]["7"]; score != 200 {
		t.Fatalf("索引应同步到 200，实际 %v", score)
	}
}

func TestCasExpectLive(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Status: 0}, false, nil)
	ctx := context.Background()
	cas := func() int64 {
		n, err := e.atomicWrite(ctx, "p1_row", &Options{
			Cond:      &p1Cond{ID: ptrInt(7)},
			CASExpect: map[string]any{"status": int32(0)},
			CASSet:    map[string]any{"status": int32(1)},
		})
		if err != nil {
			t.Fatalf("cas: %v", err)
		}
		return n
	}
	if n := cas(); n != 1 {
		t.Fatalf("期望命中应 n=1，实际 %d", n)
	}
	if n := cas(); n != 0 {
		t.Fatalf("期望已失效应 n=0，实际 %d", n)
	}
	if got := fs.hash(b.keys.rowPrefix + "7")["status"]; got != "1" {
		t.Fatalf("状态应为 1，实际 %q", got)
	}
}

func TestClaimLeaseCrashRequeue(t *testing.T) {
	e, fs, b, sink := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Point: 1}, false, nil)
	ctx := context.Background()
	// 模拟 Claim 后进程崩溃：租约已过期但成员还在 inflight。
	expired := time.Now().Add(-time.Minute).UnixMilli()
	score, _, _ := fs.ZScore(ctx, b.keys.dirty, "7")
	if _, ok, _ := fs.Claim(ctx, b.keys.dirty, b.keys.rowPrefix+"7", b.keys.tombPrefix+"7", b.keys.inflight, "7", score, expired); !ok {
		t.Fatal("领取应成功")
	}
	if fs.dirtyCount(b.keys.dirty) != 0 || fs.dirtyCount(b.keys.inflight) != 1 {
		t.Fatal("Claim 应把成员移入 inflight")
	}
	n, err := fs.RequeueInflight(ctx, b.keys.inflight, b.keys.dirty, time.Now().UnixMilli())
	if err != nil || n != 1 {
		t.Fatalf("过期租约应回收 1 条，n=%d err=%v", n, err)
	}
	if fs.dirtyCount(b.keys.dirty) != 1 || fs.dirtyCount(b.keys.inflight) != 0 {
		t.Fatal("回收后应重挂 dirty、清空 inflight")
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("回收后应落库一次，实际 %d", sink.calls)
	}
}

func TestFlushSkipsStaleVersion(t *testing.T) {
	e, fs, b, sink := newP1Engine(t)
	ctx := context.Background()
	p1Save(t, e, &p1Row{ID: 7, Uname: "v1", Point: 1}, false, nil)
	// 领取 v1 后崩溃前窗口：行被二写到 v2（新脏账），旧租约仍在 inflight。
	score, _, _ := fs.ZScore(ctx, b.keys.dirty, "7")
	h, ok, _ := fs.Claim(ctx, b.keys.dirty, b.keys.rowPrefix+"7", b.keys.tombPrefix+"7", b.keys.inflight, "7", score, time.Now().Add(-time.Minute).UnixMilli())
	if !ok || h[versionField] != "1" {
		t.Fatal("应领到 v1")
	}
	p1Save(t, e, &p1Row{ID: 7, Uname: "v2", Point: 2}, false, nil)
	// 正常 flush 落 v2；过期租约回收后再 flush 也只落 v2（版本复核跳过陈旧值）。
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if _, err := fs.RequeueInflight(ctx, b.keys.inflight, b.keys.dirty, time.Now().UnixMilli()); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll2: %v", err)
	}
	if sink.calls == 0 {
		t.Fatal("应有落库")
	}
	if got := fs.hash(b.keys.rowPrefix + "7")["uname"]; got != "v2" {
		t.Fatalf("Redis 真值应为 v2，实际 %q", got)
	}
}

func TestSecretNeverInRowHash(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice"}, true, map[string]any{"passwd_hash": "hash", "pay_passwd_hash": "pay"})
	row := fs.hash(b.keys.rowPrefix + "7")
	if _, ok := row["passwd_hash"]; ok {
		t.Fatal("秘密列永不进行 hash")
	}
	if _, ok := row["pay_passwd_hash"]; ok {
		t.Fatal("秘密列永不进行 hash（未声明白名单列经模型校验也走 :s:）")
	}
	sec := fs.hash(b.keys.secretPrefix + "7")
	if sec["passwd_hash"] != "hash" {
		t.Fatalf("秘密列应进 :s:，实际 %v", sec)
	}
}

// p1AuthDest 模拟消费方鉴权模型：绑定行不含秘密列，读目标结构体含（按列名扫描）。
type p1AuthDest struct {
	ID         int64  `cache:"id"`
	Uname      string `cache:"uname"`
	PasswdHash string `cache:"passwd_hash"`
}

// TestWithSecretsReadOptIn：默认读永不投影秘密列；WithSecrets 显式开启后从 :s:
// 白名单列读回（Redis 真源），并可按列名扫进鉴权目标结构体。
func TestWithSecretsReadOptIn(t *testing.T) {
	e, _, _, _ := newP1Engine(t)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	ctx := context.Background()
	p1Save(t, e, &p1Row{ID: 7, Uname: "alice", Point: 100}, false, map[string]any{"passwd_hash": "s3cr3t", "pay_passwd_hash": "pay"})
	cond := []Range{{Column: "uname", Op: "=", Value: "alice"}}

	var plain map[string]string
	if err := Query[p1Row](ctx, "p1_row", nil, &plain, func(o *Options) { o.NoCache = true; o.Ranges = cond; o.Limit = 1 }); err != nil {
		t.Fatalf("默认 spec 读: %v", err)
	}
	if _, ok := plain["passwd_hash"]; ok {
		t.Fatal("默认读不得投影秘密列")
	}
	if plain["uname"] != "alice" {
		t.Fatalf("普通列应正常读回: %v", plain)
	}

	var withSec map[string]string
	if err := Query[p1Row](ctx, "p1_row", nil, &withSec, func(o *Options) { o.WithSecrets = true; o.Ranges = cond; o.Limit = 1 }); err != nil {
		t.Fatalf("WithSecrets 读: %v", err)
	}
	if withSec["passwd_hash"] != "s3cr3t" || withSec["pay_passwd_hash"] != "pay" {
		t.Fatalf("WithSecrets 应读回 :s: 白名单列，实际 %v", withSec)
	}

	var dest p1AuthDest
	if err := Query[p1Row](ctx, "p1_row", nil, &dest, func(o *Options) {
		o.WithSecrets = true
		o.AllowAnyDest = true
		o.Ranges = cond
		o.Limit = 1
	}); err != nil {
		t.Fatalf("WithSecrets AnyDest 读: %v", err)
	}
	if dest.PasswdHash != "s3cr3t" || dest.Uname != "alice" || dest.ID != 7 {
		t.Fatalf("AnyDest 秘密列扫描失败: %+v", dest)
	}

	var destPlain p1AuthDest
	if err := Query[p1Row](ctx, "p1_row", nil, &destPlain, func(o *Options) { o.AllowAnyDest = true; o.Ranges = cond; o.Limit = 1 }); err != nil {
		t.Fatalf("默认 AnyDest 读: %v", err)
	}
	if destPlain.PasswdHash != "" {
		t.Fatal("不带 WithSecrets 时目标秘密列必须保持零值")
	}
}

// TestCoreFilesHaveNoSQLImports 架构闸：已 Redis 化的核心文件禁止 gorm/SQL 后端
// import。清单随切片扩充，防「悄悄接回 gorm」让 MySQL 重新变成硬依赖。
// P2 纳入 tx/options/insert/txstate；P3 纳入 bootstrap（纯 Redis 装配入口）；
// P4 纳入 query/mutate/engine/spec：querySpec 改 Redis 引擎、mutate 残余 gorm 路径
// 与 engine.mysqlClient 一并拆除。source_*.go 永久排除（sink 后端合法持 SQL）。
func TestCoreFilesHaveNoSQLImports(t *testing.T) {
	core := []string{
		"save.go", "delete.go", "flush.go", "semantics.go", "lua_write.go",
		"writeop.go", "store.go", "codec.go", "meta.go", "config.go",
		"errors.go", "table.go", "tx.go", "options.go", "insert.go", "txstate.go",
		"bootstrap.go", "query.go", "mutate.go", "engine.go", "spec.go",
	}
	fset := token.NewFileSet()
	for _, name := range core {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "gorm.io/") ||
				strings.HasPrefix(p, "touchgocore/db/mysql") ||
				strings.HasPrefix(p, "touchgocore/db/postgres") ||
				strings.HasPrefix(p, "touchgocore/db/mongo") {
				t.Errorf("%s 禁止 SQL import %q（Redis 权威核心零 gorm）", name, p)
			}
		}
	}
}

// TestRedisParityWriteOps 真 Redis 差分对拍：同一 op 序列分别走 Lua（redisStore）
// 与参考实现（memState），全状态 dump 必须一致。env 缺省 Skip。
func TestRedisParityWriteOps(t *testing.T) {
	addr := os.Getenv(txEnvRedisAddr)
	if addr == "" {
		t.Skipf("跳过：未设 %s（真 Redis 差分对拍）", txEnvRedisAddr)
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
	rs := newRedisStore(rdb)
	prefix := "tgcparity"
	cleanup := func() {
		keys, _ := rdb.Keys(ctx, "{"+prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	}
	cleanup()
	defer cleanup()

	k, err := makeTableKeys(prefix, "p1_row", false)
	if err != nil {
		t.Fatalf("makeTableKeys: %v", err)
	}
	b := &binding{keys: k, indexes: []indexSpec{{col: "point", kind: "num"}, {col: "uname", kind: "lex"}}, uniques: [][]string{{"uname"}}}
	mem := newMemState()
	now := time.Now().UnixMilli()

	ops := []*writeOp{
		{Op: "save", Fields: map[string]string{"id": "7", "uname": "alice", "point": "100", "status": "0"}, Secret: map[string]string{"passwd_hash": "x"}, Due: now, TombTTL: 30000},
		{Op: "insert", Fields: map[string]string{"id": "8", "uname": "alice", "point": "1"}, Due: now, TombTTL: 30000}, // 唯一冲突 → 0
		{Op: "insert", Fields: map[string]string{"id": "8", "uname": "bob", "point": "1"}, Due: now, TombTTL: 30000},
		{Op: "increment", Column: "point", Delta: -30, Conds: []condDesc{{Col: "point", Op: ">=", Val: "50", Type: "num"}}, Due: now, TombTTL: 30000},
		{Op: "increment", Column: "point", Delta: -90, Conds: []condDesc{{Col: "point", Op: ">=", Val: "100", Type: "num"}}, Due: now, TombTTL: 30000}, // 70<100 拒绝
		{Op: "cas", Expect: map[string]expectDesc{"status": {Mode: "eq", Val: "0", Type: "num"}}, Set: map[string]string{"status": "1", "uname": "carol"}, Due: now, TombTTL: 30000},
		{Op: "save", Fields: map[string]string{"id": "9", "uname": "alice", "point": "5"}, Due: now, TombTTL: 30000},         // 守卫冲突 duplicate
		{Op: "delete", Sink: true, Due: now, TombTTL: 30000},                                                                 // 删 7
		{Op: "save", NX: true, Fields: map[string]string{"id": "7", "uname": "zzz", "point": "1"}, Due: now, TombTTL: 30000}, // 撞墓碑拒绝复活
		{Op: "delete", NoTomb: true, Due: now, TombTTL: 30000},                                                               // 纯失效 8（不立墓碑）
		{Op: "save", Fields: map[string]string{"id": "10", "uname": "maxu", "point": "100"}, Due: now, TombTTL: 30000},
		{Op: "max", Column: "point", Max: "200", Due: now, TombTTL: 30000},                                                                       // 100 → 200
		{Op: "max", Column: "point", Max: "999", Conds: []condDesc{{Col: "point", Op: ">=", Val: "300", Type: "num"}}, Due: now, TombTTL: 30000}, // 200<300 拒绝
		{Op: "max", Column: "point", Max: "-5", Due: now, TombTTL: 30000},                                                                        // 负候选 < 200，命中但值不变
		{Op: "save", Fields: map[string]string{"id": "11", "uname": "maxv", "point": "-10"}, Due: now, TombTTL: 30000},
		{Op: "max", Column: "point", Max: "-5", Due: now, TombTTL: 30000}, // -10 → -5
		{Op: "max", Column: "point", Max: "5", Due: now, TombTTL: 30000},  // 行 12 缺席 → 0
	}
	keys := []string{"7", "7", "7", "7", "7", "7", "9", "7", "7", "8", "10", "10", "10", "10", "11", "11", "12"}
	for i, op := range ops {
		key := keys[i]
		op.Key = key
		op.Idx = b.idxDescs()
		op.UQ = b.uniques
		wk := b.writeKeysFor(key)
		payload := mustJSON(t, op)
		luaRaw, err := rs.ApplyWrite(ctx, wk.redisKeys(), payload, "")
		if err != nil {
			t.Fatalf("lua op[%d]: %v", i, err)
		}
		var luaRes writeResult
		if uerr := json.Unmarshal([]byte(luaRaw), &luaRes); uerr != nil {
			t.Fatalf("decode op[%d]: %v", i, uerr)
		}
		memRes := applyWriteOp(mem, wk, op, "", now)
		if luaRes.Affected != memRes.Affected || luaRes.Err != memRes.Err || luaRes.Ver != memRes.Ver {
			t.Fatalf("op[%d %s] 结果漂移：lua=%+v mem=%+v", i, op.Op, luaRes, memRes)
		}
	}
	// 全状态对拍：行/秘密/键集/索引/守卫/脏账/dirtyDel/墓碑。
	for _, key := range []string{"7", "8", "9", "10", "11", "12"} {
		wk := b.writeKeysFor(key)
		luaRow, _ := rs.HGetAll(ctx, wk.row)
		if !equalMaps(luaRow, mem.hashes[wk.row]) {
			t.Fatalf("行 %s 漂移：lua=%v mem=%v", key, luaRow, mem.hashes[wk.row])
		}
		luaSec, _ := rs.HGetAll(ctx, wk.secret)
		if !equalMaps(luaSec, mem.hashes[wk.secret]) {
			t.Fatalf("秘密 %s 漂移：lua=%v mem=%v", key, luaSec, mem.hashes[wk.secret])
		}
	}
	for _, zkey := range []string{k.ixPrefix + "point", k.ixPrefix + "uname", k.dirty, k.dirtyDel} {
		luaZ, err := rdb.ZRangeWithScores(ctx, zkey, 0, -1).Result()
		if err != nil {
			t.Fatalf("zrange %s: %v", zkey, err)
		}
		got := make(map[string]float64, len(luaZ))
		for _, z := range luaZ {
			got[z.Member.(string)] = z.Score
		}
		if !equalFloatMaps(got, mem.zsets[zkey]) {
			t.Fatalf("%s 漂移：lua=%v mem=%v", zkey, got, mem.zsets[zkey])
		}
	}
	luaSet, _ := rdb.SMembers(ctx, k.keySet).Result()
	sort.Strings(luaSet)
	memSet := make([]string, 0, len(mem.sets[k.keySet]))
	for m := range mem.sets[k.keySet] {
		memSet = append(memSet, m)
	}
	sort.Strings(memSet)
	if strings.Join(luaSet, ",") != strings.Join(memSet, ",") {
		t.Fatalf("键集漂移：lua=%v mem=%v", luaSet, memSet)
	}
	luaUq, _ := rdb.HGetAll(ctx, k.uq).Result()
	if !equalMaps(luaUq, mem.hashes[k.uq]) {
		t.Fatalf("守卫漂移：lua=%v mem=%v", luaUq, mem.hashes[k.uq])
	}
}

// assertParityState 全状态对拍：行/秘密/索引/脏账/dirtyDel/键集/守卫/墓碑。
func assertParityState(t *testing.T, rdb redis.Cmdable, mem *memState, b *binding, rowKeys []string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	k := b.keys
	for _, key := range rowKeys {
		wk := b.writeKeysFor(key)
		luaRow, _ := rdb.HGetAll(ctx, wk.row).Result()
		if !equalMaps(luaRow, mem.hashes[wk.row]) {
			t.Fatalf("行 %s 漂移：lua=%v mem=%v", key, luaRow, mem.hashes[wk.row])
		}
		luaSec, _ := rdb.HGetAll(ctx, wk.secret).Result()
		if !equalMaps(luaSec, mem.hashes[wk.secret]) {
			t.Fatalf("秘密 %s 漂移：lua=%v mem=%v", key, luaSec, mem.hashes[wk.secret])
		}
		luaTomb, _ := rdb.Exists(ctx, wk.tomb).Result()
		if (luaTomb > 0) != mem.alive(wk.tomb, now) {
			t.Fatalf("墓碑 %s 漂移：lua=%v mem=%v", key, luaTomb, mem.alive(wk.tomb, now))
		}
	}
	for _, zkey := range append(b.ixKeys(), k.dirty, k.dirtyDel) {
		luaZ, err := rdb.ZRangeWithScores(ctx, zkey, 0, -1).Result()
		if err != nil {
			t.Fatalf("zrange %s: %v", zkey, err)
		}
		got := make(map[string]float64, len(luaZ))
		for _, z := range luaZ {
			got[z.Member.(string)] = z.Score
		}
		if !equalFloatMaps(got, mem.zsets[zkey]) {
			t.Fatalf("%s 漂移：lua=%v mem=%v", zkey, got, mem.zsets[zkey])
		}
	}
	luaSet, _ := rdb.SMembers(ctx, k.keySet).Result()
	sort.Strings(luaSet)
	memSet := make([]string, 0, len(mem.sets[k.keySet]))
	for m := range mem.sets[k.keySet] {
		memSet = append(memSet, m)
	}
	sort.Strings(memSet)
	if strings.Join(luaSet, ",") != strings.Join(memSet, ",") {
		t.Fatalf("键集漂移：lua=%v mem=%v", luaSet, memSet)
	}
	luaUq, _ := rdb.HGetAll(ctx, k.uq).Result()
	if !equalMaps(luaUq, mem.hashes[k.uq]) {
		t.Fatalf("守卫漂移：lua=%v mem=%v", luaUq, mem.hashes[k.uq])
	}
}

// txParityBlocks 为镜像侧构造与 Engine.applyTx 同形的键块（lock=lockPrefix+key）。
func txParityBlocks(b *binding, writes []txWrite) []writeKeys {
	blocks := make([]writeKeys, len(writes))
	for i, w := range writes {
		w.op.Key = w.key
		w.op.Idx = b.idxDescs()
		w.op.UQ = b.uniques
		k := b.writeKeysFor(w.key)
		k.lock = b.keys.lockPrefix + w.key
		blocks[i] = k
	}
	return blocks
}

// TestRedisParityTxOps 真 Redis 事务批差分对拍：txScript（经 Engine.applyTx）与
// applyTxOps 镜像跑同一批 op，逐 op 结果 + 全状态 dump 必须一致。覆盖同行链式
// 读己写、唯一守卫、删除墓碑、行锁获取/释放与 lock_lost 零副作用。env 缺省 Skip。
func TestRedisParityTxOps(t *testing.T) {
	addr := os.Getenv(txEnvRedisAddr)
	if addr == "" {
		t.Skipf("跳过：未设 %s（真 Redis 事务批对拍）", txEnvRedisAddr)
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
	rs := newRedisStore(rdb)
	prefix := "tgcparitytx"
	cleanup := func() {
		keys, _ := rdb.Keys(ctx, "{"+prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	}
	cleanup()
	defer cleanup()
	k, err := makeTableKeys(prefix, "p1_row", false)
	if err != nil {
		t.Fatalf("makeTableKeys: %v", err)
	}
	b := &binding{keys: k, indexes: []indexSpec{{col: "point", kind: "num"}, {col: "uname", kind: "lex"}}, uniques: [][]string{{"uname"}}}
	e := newEngine(DefaultConfig(), rs)
	now := time.Now().UnixMilli()

	// 场景一：无锁批，同行链式 increment/cas 读己写 + 插入冲突 + 删除墓碑。
	writes := []txWrite{
		{b, "7", &writeOp{Op: "save", Fields: map[string]string{"id": "7", "uname": "alice", "point": "100", "status": "0"}, Secret: map[string]string{"passwd_hash": "x"}, Due: now, TombTTL: 30000}},
		{b, "7", &writeOp{Op: "increment", Column: "point", Delta: -30, Conds: []condDesc{{Col: "point", Op: ">=", Val: "50", Type: "num"}}, Due: now, TombTTL: 30000}},
		{b, "7", &writeOp{Op: "max", Column: "point", Max: "80", Due: now, TombTTL: 30000}}, // 70 → 80 同行读己写
		{b, "7", &writeOp{Op: "increment", Column: "point", Delta: -90, Conds: []condDesc{{Col: "point", Op: ">=", Val: "100", Type: "num"}}, Due: now, TombTTL: 30000}},
		{b, "7", &writeOp{Op: "cas", Expect: map[string]expectDesc{"status": {Mode: "eq", Val: "0", Type: "num"}}, Set: map[string]string{"status": "1", "uname": "carol"}, Due: now, TombTTL: 30000}},
		{b, "8", &writeOp{Op: "insert", Fields: map[string]string{"id": "8", "uname": "bob", "point": "1"}, Due: now, TombTTL: 30000}},
		{b, "8", &writeOp{Op: "insert", Fields: map[string]string{"id": "8", "uname": "bob", "point": "2"}, Due: now, TombTTL: 30000}},
		{b, "8", &writeOp{Op: "delete", Sink: true, Due: now, TombTTL: 30000}},
		{b, "9", &writeOp{Op: "save", Fields: map[string]string{"id": "9", "uname": "alice", "point": "5"}, Due: now, TombTTL: 30000}},
	}
	luaResults, err := e.applyTx(ctx, "", writes)
	if err != nil {
		t.Fatalf("applyTx: %v", err)
	}
	mem := newMemState()
	blocks := txParityBlocks(b, writes)
	ops := make([]*writeOp, len(writes))
	for i, w := range writes {
		ops[i] = w.op
	}
	memResults, memErr := applyTxOps(mem, blocks, ops, "", now)
	if memErr != "" {
		t.Fatalf("mirror err: %s", memErr)
	}
	if len(luaResults) != len(memResults) {
		t.Fatalf("结果数漂移：lua=%d mem=%d", len(luaResults), len(memResults))
	}
	for i := range luaResults {
		if luaResults[i].Affected != memResults[i].Affected || luaResults[i].Ver != memResults[i].Ver || luaResults[i].Err != memResults[i].Err {
			t.Fatalf("op[%d] 结果漂移：lua=%+v mem=%+v", i, luaResults[i], memResults[i])
		}
	}
	assertParityState(t, rdb, mem, b, []string{"7", "8", "9"})

	// 场景二：持锁批。先 TryLock 取得行锁，批成功后锁必须释放。
	cleanup()
	txid := "tx-parity-1"
	for _, key := range []string{"7", "8"} {
		ok, lerr := rs.TryLock(ctx, k.lockPrefix+key, txid, 30000, 0)
		if lerr != nil || !ok {
			t.Fatalf("TryLock %s: ok=%v err=%v", key, ok, lerr)
		}
	}
	lockWrites := []txWrite{
		{b, "7", &writeOp{Op: "save", Fields: map[string]string{"id": "7", "uname": "alice", "point": "10"}, Due: now, TombTTL: 30000}},
		{b, "8", &writeOp{Op: "save", Fields: map[string]string{"id": "8", "uname": "bob", "point": "20"}, Due: now, TombTTL: 30000}},
		{b, "7", &writeOp{Op: "increment", Column: "point", Delta: 5, Due: now, TombTTL: 30000}},
	}
	if _, err := e.applyTx(ctx, txid, lockWrites); err != nil {
		t.Fatalf("持锁 applyTx: %v", err)
	}
	for _, key := range []string{"7", "8"} {
		n, _ := rdb.Exists(ctx, k.lockPrefix+key).Result()
		if n != 0 {
			t.Fatalf("批成功后行锁 %s 未释放", key)
		}
	}
	mem2 := newMemState()
	for _, key := range []string{"7", "8"} {
		mem2.strs[k.lockPrefix+key] = txid
		mem2.expiry[k.lockPrefix+key] = now + 30000
	}
	blocks2 := txParityBlocks(b, lockWrites)
	ops2 := make([]*writeOp, len(lockWrites))
	for i, w := range lockWrites {
		ops2[i] = w.op
	}
	if _, memErr := applyTxOps(mem2, blocks2, ops2, txid, now); memErr != "" {
		t.Fatalf("持锁镜像 err: %s", memErr)
	}
	for _, key := range []string{"7", "8"} {
		if mem2.alive(k.lockPrefix+key, now) {
			t.Fatalf("镜像批成功后行锁 %s 未释放", key)
		}
	}
	assertParityState(t, rdb, mem2, b, []string{"7", "8"})

	// 场景三：lock_lost（未持锁却带 txid）→ 零副作用。
	cleanup()
	lostWrites := []txWrite{
		{b, "7", &writeOp{Op: "save", Fields: map[string]string{"id": "7", "uname": "alice", "point": "10"}, Due: now, TombTTL: 30000}},
	}
	if _, err := e.applyTx(ctx, "tx-not-held", lostWrites); err == nil {
		t.Fatalf("lock_lost 期望错误")
	}
	mem3 := newMemState()
	blocks3 := txParityBlocks(b, lostWrites)
	if _, memErr := applyTxOps(mem3, blocks3, []*writeOp{lostWrites[0].op}, "tx-not-held", now); memErr != "lock_lost" {
		t.Fatalf("镜像 lock_lost 期望 lock_lost，实际 %q", memErr)
	}
	assertParityState(t, rdb, mem3, b, []string{"7"})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func equalFloatMaps(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestP1FixtureFilesPresent 反空转：架构闸清单里的文件必须真实存在，
// 防止「因为什么都没扫所以什么都没违规」的恒绿。
func TestP1FixtureFilesPresent(t *testing.T) {
	for _, name := range []string{"semantics.go", "lua_write.go", "writeop.go"} {
		if _, err := os.Stat(filepath.Join(".", name)); err != nil {
			t.Fatalf("核心文件 %s 缺失：%v", name, err)
		}
	}
}
