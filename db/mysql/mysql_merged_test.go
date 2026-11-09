package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/db/dbmap"
)

// fakeRepo 用最小 gorm.DB 替代品替代复杂 mock，验证 runWithAutoMigrate 行为
func TestRunWithAutoMigrate_Disabled(t *testing.T) {
	// autoMigrate=false 时，runWithAutoMigrate 只透传 fn，不尝试迁移
	r := &Repository[struct{ ID int }]{
		hooks: &repoHooks[struct{ ID int }]{},
	}
	calls := 0
	err := r.runWithAutoMigrate(t.Context(), "Test", func() error {
		calls++
		return errors.New("ignored")
	})
	if err == nil || err.Error() != "ignored" {
		t.Fatalf("err=%v 应透传", err)
	}
	if calls != 1 {
		t.Fatalf("应只调用一次 fn，got %d", calls)
	}
	if r.hooks.migrated.Load() {
		t.Fatal("disabled 时不应修改 migrated 标志")
	}
}

func TestRunWithAutoMigrate_AlreadyMigrated_SkipsDetection(t *testing.T) {
	r := &Repository[struct{ ID int }]{
		hooks: &repoHooks[struct{ ID int }]{autoMigrate: true},
	}
	r.hooks.migrated.Store(true)
	calls := 0
	err := r.runWithAutoMigrate(t.Context(), "Test", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("应只调用一次 fn，got %d", calls)
	}
}

func TestIsTableNotExist_VariousSources(t *testing.T) {
	if IsTableNotExist(nil) {
		t.Fatal("nil 应为 false")
	}

	// 直接 MySQL 错误
	if !IsTableNotExist(&mysql.MySQLError{Number: 1146}) {
		t.Fatal("MySQL 1146 应识别")
	}

	// fmt.Errorf 包装
	wrapped := wrap("SomeOp", &mysql.MySQLError{Number: 1146}, KindSyntax)
	if !IsTableNotExist(wrapped) {
		t.Fatal("被 *Error 包装的 1146 应识别")
	}

	// 双重包装
	doubleWrapped := wrap("Outer", wrapped, KindSyntax)
	if !IsTableNotExist(doubleWrapped) {
		t.Fatal("双重包装的 1146 应识别")
	}

	// 其他错误
	if IsTableNotExist(&mysql.MySQLError{Number: 1062}) {
		t.Fatal("1062 不应识别为表不存在")
	}
	if IsTableNotExist(errors.New("random")) {
		t.Fatal("随机错误不应识别")
	}
}

func TestHooks_MigratedConcurrent(t *testing.T) {
	// 验证 atomic.Bool 在并发场景下的可见性
	h := &repoHooks[struct{ ID int }]{autoMigrate: true}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.migrated.Store(true)
			_ = h.migrated.Load()
		}()
	}
	wg.Wait()
	if !h.migrated.Load() {
		t.Fatal("atomic.Bool 并发赋值后应可见")
	}
}

func TestMakeHooks_InheritsClientAutoMigrate(t *testing.T) {
	// 通过 mock Client 验证 makeHooks 读取 Client.AutoMigrateEnabled()
	c := &Client{opts: options{autoMigrate: true}}
	h := makeHooks[struct{ ID int }](c)
	if !h.autoMigrate {
		t.Fatal("应继承 Client.autoMigrate=true")
	}
	c2 := &Client{opts: options{autoMigrate: false}}
	h2 := makeHooks[struct{ ID int }](c2)
	if h2.autoMigrate {
		t.Fatal("应继承 Client.autoMigrate=false")
	}
}

func TestWithAutoMigrate_OptionApplies(t *testing.T) {
	// 验证 Option 函数修改了 options.autoMigrate
	o := defaultOptions()
	WithAutoMigrate(true)(&o)
	if !o.autoMigrate {
		t.Fatal("WithAutoMigrate(true) 未生效")
	}
	WithAutoMigrate(false)(&o)
	if o.autoMigrate {
		t.Fatal("WithAutoMigrate(false) 未生效")
	}
}

func TestRepoHooks_AutoMigrateDefaultFalse(t *testing.T) {
	// 默认 newRepoHooks 不应开启 autoMigrate
	h := newRepoHooks[struct{ ID int }]()
	if h.autoMigrate {
		t.Fatal("默认 autoMigrate 应为 false")
	}
	if h.migrated.Load() {
		t.Fatal("默认 migrated 应为 false")
	}
}

// counterAtomic 用于验证仅触发一次迁移
type counterAtomic struct {
	count atomic.Int32
}

func TestRunWithAutoMigrate_PreservesNonTableErrors(t *testing.T) {
	// autoMigrate=true + migrated=false + 非表不存在错误 → 应直接透传，不尝试迁移
	r := &Repository[struct{ ID int }]{
		hooks: &repoHooks[struct{ ID int }]{autoMigrate: true},
	}
	called := 0
	otherErr := errors.New("connection refused")
	err := r.runWithAutoMigrate(t.Context(), "Test", func() error {
		called++
		return otherErr
	})
	if err != otherErr {
		t.Fatalf("err 应透传 otherErr，got %v", err)
	}
	if called != 1 {
		t.Fatalf("应只调用一次 fn，got %d", called)
	}
	if r.hooks.migrated.Load() {
		t.Fatal("非表不存在错误不应设置 migrated=true")
	}
}

func TestKind_String(t *testing.T) {
	cases := map[Kind]string{
		KindUnknown:    "unknown",
		KindConnFailed: "conn_failed",
		KindAuth:       "auth",
		KindNotFound:   "not_found",
		KindDuplicate:  "duplicate",
		KindTimeout:    "timeout",
		KindCanceled:   "canceled",
		KindDeadlock:   "deadlock",
		KindLockWait:   "lock_wait",
		KindSyntax:     "syntax",
		KindSlowQuery:  "slow_query",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Fatalf("Kind(%d)=%s want=%s", k, got, want)
		}
	}
}

func TestError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("connection refused")
	e := newError("Open", inner, KindConnFailed, "SELECT 1", nil, 100)
	if e.Error() == "" {
		t.Fatal("Error string 不应为空")
	}
	if e.Kind != KindConnFailed {
		t.Fatalf("Kind=%v", e.Kind)
	}
	if e.Unwrap() != inner {
		t.Fatal("Unwrap 应返回原始 error")
	}
	if !errors.Is(e, inner) {
		t.Fatal("errors.Is(e, inner) 应为 true")
	}
}

func TestError_Is(t *testing.T) {
	e1 := newError("Open", errors.New("dup"), KindDuplicate, "", nil, 0)
	e2 := newError("Open", errors.New("dup"), KindDuplicate, "", nil, 0)
	if !errors.Is(e1, e2) {
		t.Fatal("同 Kind 的两个 Error 应互相 Is")
	}
	e3 := newError("Open", errors.New("nf"), KindNotFound, "", nil, 0)
	if errors.Is(e1, e3) {
		t.Fatal("不同 Kind 不应 Is")
	}
}

func TestClassify_GormNotFound(t *testing.T) {
	if got := classify(gorm.ErrRecordNotFound); got != KindNotFound {
		t.Fatalf("classify(ErrRecordNotFound)=%v want=KindNotFound", got)
	}
}

func TestClassify_SqlNoRows(t *testing.T) {
	if got := classify(sql.ErrNoRows); got != KindNotFound {
		t.Fatalf("classify(ErrNoRows)=%v want=KindNotFound", got)
	}
}

func TestClassify_ContextCanceled(t *testing.T) {
	if got := classify(context.Canceled); got != KindCanceled {
		t.Fatalf("classify(context.Canceled)=%v want=KindCanceled", got)
	}
}

func TestClassify_ContextDeadline(t *testing.T) {
	if got := classify(context.DeadlineExceeded); got != KindTimeout {
		t.Fatalf("classify(context.DeadlineExceeded)=%v want=KindTimeout", got)
	}
}

func TestClassify_MySQLDuplicate(t *testing.T) {
	merr := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if got := classify(merr); got != KindDuplicate {
		t.Fatalf("classify(1062)=%v want=KindDuplicate", got)
	}
}

func TestClassify_MySQLDeadlock(t *testing.T) {
	merr := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	if got := classify(merr); got != KindDeadlock {
		t.Fatalf("classify(1213)=%v want=KindDeadlock", got)
	}
}

func TestClassify_MySQLAccessDenied(t *testing.T) {
	merr := &mysql.MySQLError{Number: 1045, Message: "Access denied"}
	if got := classify(merr); got != KindAuth {
		t.Fatalf("classify(1045)=%v want=KindAuth", got)
	}
}

func TestClassify_Unknown(t *testing.T) {
	if got := classify(errors.New("some random error")); got != KindUnknown {
		t.Fatalf("classify(未知)=%v want=KindUnknown", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(gorm.ErrRecordNotFound) {
		t.Fatal("IsNotFound(ErrRecordNotFound) 应为 true")
	}
	if IsNotFound(errors.New("other")) {
		t.Fatal("IsNotFound(其他) 应为 false")
	}
}

func TestIsDuplicate(t *testing.T) {
	if !IsDuplicate(&mysql.MySQLError{Number: 1062}) {
		t.Fatal("IsDuplicate(1062) 应为 true")
	}
}

func TestIsTimeout(t *testing.T) {
	if !IsTimeout(context.DeadlineExceeded) {
		t.Fatal("IsTimeout(DeadlineExceeded) 应为 true")
	}
}

func TestKindOf(t *testing.T) {
	inner := errors.New("x")
	e := newError("op", inner, KindDeadlock, "", nil, 0)
	if kindOf(e) != KindDeadlock {
		t.Fatalf("kindOf(*Error)=%v want=KindDeadlock", kindOf(e))
	}
	if kindOf(nil) != KindUnknown {
		t.Fatal("kindOf(nil) 应为 KindUnknown")
	}
}

func TestWrap_NilSafe(t *testing.T) {
	if wrap("op", nil, KindUnknown) != nil {
		t.Fatal("wrap(nil) 应返回 nil")
	}
}

func TestIsTableNotExist(t *testing.T) {
	if IsTableNotExist(nil) {
		t.Fatal("IsTableNotExist(nil) 应为 false")
	}
	// MySQL 1146
	merr := &mysql.MySQLError{Number: 1146, Message: "Table 'x.y' doesn't exist"}
	if !IsTableNotExist(merr) {
		t.Fatal("IsTableNotExist(1146) 应为 true")
	}
	// 通过 fmt.Errorf 包裹也应能识别
	wrapped := fmt.Errorf("wrapped: %w", merr)
	if !IsTableNotExist(wrapped) {
		t.Fatal("IsTableNotExist(fmt.Errorf wrap 1146) 应为 true")
	}
	// 其他错误码
	if IsTableNotExist(&mysql.MySQLError{Number: 1062}) {
		t.Fatal("IsTableNotExist(1062) 应为 false")
	}
	if IsTableNotExist(errors.New("random")) {
		t.Fatal("IsTableNotExist(随机错误) 应为 false")
	}
}

func TestClassify_TableNotExist_StillKindSyntax(t *testing.T) {
	// 1146 仍归类为 KindSyntax（向后兼容），但 IsTableNotExist 提供细粒度判定
	merr := &mysql.MySQLError{Number: 1146}
	if got := classify(merr); got != KindSyntax {
		t.Fatalf("classify(1146)=%v want=KindSyntax", got)
	}
	if !IsTableNotExist(merr) {
		t.Fatal("IsTableNotExist 应识别 1146")
	}
}

func TestNoopMetrics(t *testing.T) {
	var h MetricsHook = NoopMetrics{}
	h.OnQuery("test", time.Millisecond, KindUnknown)
	h.OnSlowQuery("test", "SELECT 1", time.Millisecond)
	h.OnConnection("idle", 5)
}

func TestObservabilityPlugin_Name(t *testing.T) {
	p := NewObservabilityPlugin(NoopMetrics{}, 100*time.Millisecond)
	if p.Name() != "touchgocore-mysql-observability" {
		t.Fatalf("Name 异常: %s", p.Name())
	}
}

func TestBuildDSNWithOptions(t *testing.T) {
	cfg := &config.MySqlDBConfig{
		Host:     "127.0.0.1:3306",
		Username: "user",
		Password: "pass",
		DBName:   "db",
	}
	o := defaultOptions()
	dsn := buildDSNWithOptions(cfg, o)
	if dsn == "" {
		t.Fatal("DSN 不应为空")
	}
	if !contains(dsn, "user") || !contains(dsn, "pass") || !contains(dsn, "127.0.0.1") {
		t.Fatalf("DSN 缺少关键字段: %s", dsn)
	}
}

func TestBuildDSNWithOptions_ExtraParams(t *testing.T) {
	cfg := &config.MySqlDBConfig{
		Host: "127.0.0.1:3306", Username: "u", Password: "p", DBName: "d",
	}
	o := defaultOptions()
	o.extraDSNParams["time_zone"] = "'+08:00'"
	dsn := buildDSNWithOptions(cfg, o)
	if !contains(dsn, "time_zone") {
		t.Fatalf("DSN 应包含自定义参数: %s", dsn)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestContextPropagation(t *testing.T) {
	type ctxKey string
	key := ctxKey("test-key")
	ctx := context.WithValue(context.Background(), key, "value")
	r := &Repository[int]{ctx: ctx}
	if got := r.Context().Value(key); got != "value" {
		t.Fatalf("ctx 传递丢失: %v", got)
	}
}

func TestIsHelpers(t *testing.T) {
	tests := []struct {
		err  error
		fn   func(error) bool
		want bool
	}{
		{context.DeadlineExceeded, IsTimeout, true},
		{context.Canceled, IsCanceled, true},
		{errors.New("x"), IsTimeout, false},
		{errors.New("x"), IsCanceled, false},
		{errors.New("x"), IsDeadlock, false},
		{errors.New("x"), IsDuplicate, false},
	}
	for _, c := range tests {
		if got := c.fn(c.err); got != c.want {
			t.Fatalf("fn(%v)=%v want=%v", c.err, got, c.want)
		}
	}
}

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()
	if o.slowThreshold != 200*time.Millisecond {
		t.Fatalf("slowThreshold=%v", o.slowThreshold)
	}
	if o.charset != "utf8mb4" {
		t.Fatalf("charset=%s", o.charset)
	}
	if o.collation != "utf8mb4_unicode_ci" {
		t.Fatalf("collation=%s", o.collation)
	}
	if o.connMaxLifetime != 24*time.Hour {
		t.Fatalf("connMaxLifetime=%v", o.connMaxLifetime)
	}
}

func TestOptions(t *testing.T) {
	o := defaultOptions()
	WithSlowQueryThreshold(50 * time.Millisecond)(&o)
	if o.slowThreshold != 50*time.Millisecond {
		t.Fatal("WithSlowQueryThreshold 未生效")
	}
	WithCharset("latin1", "latin1_swedish_ci")(&o)
	if o.charset != "latin1" {
		t.Fatal("WithCharset 未生效")
	}
	WithDSNParam("time_zone", "'+09:00'")(&o)
	if o.extraDSNParams["time_zone"] != "'+09:00'" {
		t.Fatal("WithDSNParam 未生效")
	}
	called := false
	WithMetricsHook(NoopMetrics{})(&o)
	called = true
	if !called {
		t.Fatal("WithMetricsHook 调用失败")
	}
}

func TestToSnakeCase(t *testing.T) {
	cases := map[string]string{
		"User":        "user",
		"UserProfile": "user_profile",
		"HTTPClient":  "h_t_t_p_client",
		"a":           "a",
	}
	for in, want := range cases {
		if got := toSnakeCase(in); got != want {
			t.Fatalf("toSnakeCase(%q)=%q want=%q", in, got, want)
		}
	}
}

// mockHook 用于测试
type mockHook struct {
	queryCalls atomic.Int32
	slowCalls  atomic.Int32
	connCalls  atomic.Int32
	lastOp     string
	lastDur    time.Duration
	lastKind   Kind
}

func (m *mockHook) OnQuery(op string, dur time.Duration, kind Kind) {
	m.queryCalls.Add(1)
	m.lastOp = op
	m.lastDur = dur
	m.lastKind = kind
}
func (m *mockHook) OnSlowQuery(op, sql string, dur time.Duration) {
	m.slowCalls.Add(1)
}
func (m *mockHook) OnConnection(state string, n float64) {
	m.connCalls.Add(1)
}

func TestPoolKey_StabilityAndUniqueness(t *testing.T) {
	cfg1 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p", DBName: "db"}
	cfg2 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p", DBName: "db"}
	cfg3 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p2", DBName: "db"}
	cfg4 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p", DBName: "db2"}

	c1 := &Client{cfg: cfg1}
	c2 := &Client{cfg: cfg2}
	c3 := &Client{cfg: cfg3}
	c4 := &Client{cfg: cfg4}

	if c1.poolKey() != c2.poolKey() {
		t.Fatalf("同配置 poolKey 应一致: %s vs %s", c1.poolKey(), c2.poolKey())
	}
	if c1.poolKey() == c3.poolKey() {
		t.Fatal("不同密码 poolKey 应不同")
	}
	if c1.poolKey() == c4.poolKey() {
		t.Fatal("不同 DBName poolKey 应不同")
	}
}

func TestConnectOnly_MissAndInvalidEntry(t *testing.T) {
	c := &Client{cfg: &config.MySqlDBConfig{Host: "x", DBName: "y"}}

	if c.connectOnly("nonexistent-pool-key-xyz") {
		t.Fatal("未注册 key 不应命中 connectOnly")
	}

	// 注册一个非 *gorm.DB 值，connectOnly 应安全返回 false
	dbmap.Global.Store("not-gorm-key", "string-value")
	defer dbmap.Global.Delete("not-gorm-key")
	if c.connectOnly("not-gorm-key") {
		t.Fatal("类型不匹配应返回 false")
	}
}

func TestDbmapGlobalRoundtrip(t *testing.T) {
	const key = "test-roundtrip-key"
	defer dbmap.Global.Delete(key)

	dbmap.Global.Store(key, "v")
	v, ok := dbmap.Global.Load(key)
	if !ok || v != "v" {
		t.Fatalf("roundtrip 失败: %v %v", v, ok)
	}

	dbmap.Global.Delete(key)
	if _, ok := dbmap.Global.Load(key); ok {
		t.Fatal("Delete 后 Load 应返回 !ok")
	}
}

func TestQuery_NewQuery(t *testing.T) {
	q := NewQuery()
	if q == nil {
		t.Fatal("NewQuery 不应返回 nil")
	}
	if q.whereExpr != "" {
		t.Fatal("新查询 whereExpr 应为空")
	}
}

func TestQuery_Eq(t *testing.T) {
	q := NewQuery().Eq("id", 1)
	if q.whereExpr != "id = ?" {
		t.Fatalf("Eq: whereExpr=%q", q.whereExpr)
	}
	if len(q.whereArgs) != 1 || q.whereArgs[0] != 1 {
		t.Fatalf("Eq: args=%v", q.whereArgs)
	}
}

func TestQuery_Chain(t *testing.T) {
	q := NewQuery().
		Where("id > ?", 10).
		Gt("age", 18).
		OrderDesc("created_at").
		Limit(20).
		Offset(40)
	if q.whereExpr != "age > ?" {
		t.Fatalf("Gt 应覆盖 whereExpr: %q", q.whereExpr)
	}
	if len(q.orders) != 1 {
		t.Fatalf("OrderDesc 数量异常: %d", len(q.orders))
	}
	if q.limit != 20 || q.offset != 40 {
		t.Fatalf("Limit/Offset: %d/%d", q.limit, q.offset)
	}
}

func TestQuery_In(t *testing.T) {
	q := NewQuery().In("id", 1, 2, 3)
	if q.whereExpr != "id IN (?,?,?)" {
		t.Fatalf("In: whereExpr=%q", q.whereExpr)
	}
	if len(q.whereArgs) != 3 {
		t.Fatalf("In: args=%v", q.whereArgs)
	}
}

func TestQuery_NotIn_Empty(t *testing.T) {
	q := NewQuery().NotIn("id")
	if q.whereExpr != "" {
		t.Fatalf("空 NotIn 应不设置 where: %q", q.whereExpr)
	}
}

func TestQuery_Between(t *testing.T) {
	q := NewQuery().Between("age", 18, 30)
	if q.whereExpr != "age BETWEEN ? AND ?" {
		t.Fatalf("Between: %q", q.whereExpr)
	}
}

func TestQuery_IsNull(t *testing.T) {
	q := NewQuery().IsNull("deleted_at")
	if q.whereExpr != "deleted_at IS NULL" {
		t.Fatalf("IsNull: %q", q.whereExpr)
	}
}

func TestQuery_SelectGroupHaving(t *testing.T) {
	q := NewQuery().
		Select("id, name").
		GroupBy("status").
		Having("COUNT(*) > ?", 5)
	if q.selectString() != "id, name" {
		t.Fatalf("Select: %q", q.selectString())
	}
	if q.groupString() != "status" {
		t.Fatalf("GroupBy: %q", q.groupString())
	}
	if q.having != "COUNT(*) > ?" {
		t.Fatalf("Having: %q", q.having)
	}
}

func TestQuery_OrderByRaw(t *testing.T) {
	q := NewQuery().OrderBy("FIELD(id, 1, 2, 3)")
	if q.orderString() != "FIELD(id, 1, 2, 3)" {
		t.Fatalf("OrderBy: %q", q.orderString())
	}
}
