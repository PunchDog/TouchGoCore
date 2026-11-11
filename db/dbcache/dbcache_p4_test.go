package dbcache

// ============================================================================
// P4 验收：读形状扩展（范围/排序/分页/分组/聚合/JOIN/投影）的 Redis 权威引擎。
//
// 全部 fake 可跑，零 SQL。覆盖 whatapp 真实读形状：自定义投影 + AnyDest（FS-8
// 未选列保持零值）、绑定类型排序、等值/范围过滤、COUNT/COALESCE(SUM) 落 struct、
// GROUP BY + SUM/COUNT、JOIN + TCol + COALESCE（LEFT 保留无匹配 / INNER 丢弃）、
// DateBucket + GroupByAlias 趋势日桶、GREATEST(a-COALESCE(b,0),0) 嵌套聚合、
// AnyOf OR 分组、Limit/Offset 分页、扫描预算 fail-closed、公开 Query 路由到引擎。
// ============================================================================

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 投影目标：Age 未被 SELECT，必须保持零值（FS-8 安全白名单口径）。
type specProjRow struct {
	ID   int64  `gorm:"column:id"`
	Name string `gorm:"column:name"`
	Age  int    `gorm:"column:age"`
}

type specTotalRow struct {
	Total int64 `gorm:"column:total"`
}

type specGroupRow struct {
	Name  string `gorm:"column:name"`
	Total int64  `gorm:"column:total"`
	Cnt   int64  `gorm:"column:cnt"`
}

type specTrendRow struct {
	Day   time.Time `gorm:"column:day"`
	Total int64     `gorm:"column:total"`
}

type specJoinRow struct {
	Name    string `gorm:"column:name"`
	Balance int64  `gorm:"column:balance"`
}

type specWallet struct {
	ID      int64 `cache:"id,pk"`
	UID     int64 `cache:"uid"`
	Balance int64 `cache:"balance"`
}

type specLedger struct {
	ID     int64  `cache:"id,pk"`
	Amount int64  `cache:"amount"`
	Fee    *int64 `cache:"fee"`
}

// bindSpecTable 在给定引擎上纯 Redis 绑定一张表（无源/sink，spec 读不回源）。
func bindSpecTable[T any](t *testing.T, e *Engine, table string) *binding {
	t.Helper()
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table, false)
	if err != nil {
		t.Fatalf("makeTableKeys %s: %v", table, err)
	}
	b, err := newBinding[T](table, backendMySQL, BindOption[T]{}, keys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding %s: %v", table, err)
	}
	if err := e.register(b); err != nil {
		t.Fatalf("register %s: %v", table, err)
	}
	return b
}

func seedUsers(t *testing.T, e *Engine, rows ...*testUser) {
	t.Helper()
	ctx := context.Background()
	for _, row := range rows {
		if err := e.save[testUser](ctx, "test_user", "", row, false); err != nil {
			t.Fatalf("save %d: %v", row.ID, err)
		}
	}
}

// TestSpecSelectAnyDestCustomStruct：Select 投影到自定义结构（AnyDest），未选列零值。
func TestSpecSelectAnyDestCustomStruct(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "alice", Age: 30},
		&testUser{ID: 2, Name: "bob", Age: 40},
	)
	var out []specProjRow
	o := &Options{
		AllowAnyDest: true,
		Selects:      []SelectItem{{Expr: Col("id"), Alias: "id"}, {Expr: Col("name"), Alias: "name"}},
		Orders:       []Order{{Column: "id"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 || out[0].ID != 1 || out[0].Name != "alice" || out[1].Name != "bob" {
		t.Fatalf("投影结果不符: %+v", out)
	}
	if out[0].Age != 0 || out[1].Age != 0 {
		t.Fatalf("未 SELECT 的 age 必须保持零值（FS-8），实际 %+v", out)
	}
	if src.listCalls != 0 || src.oneCalls != 0 {
		t.Fatal("spec 读全程 Redis，不得回源")
	}
}

// TestSpecOrderByBoundType：无 Select 时取基表整行，按数值列降序排序到绑定类型。
func TestSpecOrderByBoundType(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "a", Age: 10},
		&testUser{ID: 2, Name: "b", Age: 50},
		&testUser{ID: 3, Name: "c", Age: 30},
	)
	var out []testUser
	o := &Options{Orders: []Order{{Column: "age", Desc: true}}}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 3 || out[0].Age != 50 || out[1].Age != 30 || out[2].Age != 10 {
		t.Fatalf("age 降序不符: %+v", out)
	}
}

// TestSpecEqualRangeFilter：等值 Range 过滤 + 排序，只命中匹配行。
func TestSpecEqualRangeFilter(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "alice"},
		&testUser{ID: 2, Name: "bob"},
		&testUser{ID: 3, Name: "alice"},
	)
	var out []testUser
	o := &Options{
		Ranges: []Range{{Column: "name", Op: "=", Value: "alice"}},
		Orders: []Order{{Column: "id"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 || out[0].ID != 1 || out[1].ID != 3 {
		t.Fatalf("name=alice 应命中 id 1/3，实际 %+v", out)
	}
}

// TestSpecCountIntoStruct：COUNT(*) 聚合落 struct{Total}（whatapp cacheCountRow）。
func TestSpecCountIntoStruct(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "a"},
		&testUser{ID: 2, Name: "b"},
		&testUser{ID: 3, Name: "c"},
	)
	var out specTotalRow
	o := &Options{Agg: &Aggregate{Fn: "COUNT", Alias: "total"}}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if out.Total != 3 {
		t.Fatalf("COUNT(*) 应为 3，实际 %d", out.Total)
	}
}

// TestSpecAggregateExprCoalesceSum：COALESCE(SUM(age),0) 落 struct（whatapp 积分汇总）。
func TestSpecAggregateExprCoalesceSum(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Age: 10},
		&testUser{ID: 2, Age: 25},
		&testUser{ID: 3, Age: 5},
	)
	var out specTotalRow
	o := &Options{Agg: &Aggregate{Expr: Coalesce(SumExpr(Col("age")), Lit(int64(0))), Alias: "total"}}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if out.Total != 40 {
		t.Fatalf("COALESCE(SUM(age),0) 应为 40，实际 %d", out.Total)
	}
}

// TestSpecAggregateExprEmptyCoalesce：空集 SUM 为 NULL，COALESCE 兜底 0。
func TestSpecAggregateExprEmptyCoalesce(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	var out specTotalRow
	o := &Options{
		Agg:    &Aggregate{Expr: Coalesce(SumExpr(Col("age")), Lit(int64(0))), Alias: "total"},
		Ranges: []Range{{Column: "name", Op: "=", Value: "nobody"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if out.Total != 0 {
		t.Fatalf("空集 COALESCE(SUM,0) 应为 0，实际 %d", out.Total)
	}
}

// TestSpecLargeIDProjectionNoScientificNotation：大整数 id（生产 member 11 位）
// 经 Select 投影回 int64 必须精确。回归 scalar() 把整数列抹成 float64 →
// encodeResult FormatFloat('g') 产 "9.99e+08" → decodeRow ParseInt 失败。
func TestSpecLargeIDProjectionNoScientificNotation(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 999961001, Name: "big1"},
		&testUser{ID: 10000000001, Name: "big2"},
	)
	var out []specProjRow
	o := &Options{
		AllowAnyDest: true,
		Selects:      []SelectItem{{Expr: Col("id"), Alias: "id"}, {Expr: Col("name"), Alias: "name"}},
		Orders:       []Order{{Column: "id"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 || out[0].ID != 999961001 || out[1].ID != 10000000001 {
		t.Fatalf("大整数 id 投影须精确（无科学计数法），实际 %+v", out)
	}
	if src.listCalls != 0 || src.oneCalls != 0 {
		t.Fatal("spec 读全程 Redis，不得回源")
	}
}

// TestSpecLargeSumAggregateNoScientificNotation：SUM 跨行得 float64，整值大额
// （≥1e6）回退 int64 定点串。回归 encodeResult 对整值 float64 产 "1.5e+10" →
// 落 struct{Total int64} 时 ParseInt 失败。
func TestSpecLargeSumAggregateNoScientificNotation(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Age: 4000000000},
		&testUser{ID: 2, Age: 5000000000},
		&testUser{ID: 3, Age: 6000000000},
	)
	var out specTotalRow
	o := &Options{Agg: &Aggregate{Expr: Coalesce(SumExpr(Col("age")), Lit(int64(0))), Alias: "total"}}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if out.Total != 15000000000 {
		t.Fatalf("大额 SUM 须精确落 int64（无科学计数法），实际 %d", out.Total)
	}
}

// TestSpecGroupBySumCount：GROUP BY name + SUM(age)/COUNT(*)（whatapp aggRow）。
func TestSpecGroupBySumCount(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "alice", Age: 10},
		&testUser{ID: 2, Name: "alice", Age: 20},
		&testUser{ID: 3, Name: "bob", Age: 7},
	)
	var out []specGroupRow
	o := &Options{
		Groups: []string{"name"},
		Selects: []SelectItem{
			{Expr: SumExpr(Col("age")), Alias: "total"},
			{Expr: CountExpr(Star(), false), Alias: "cnt"},
		},
		Orders: []Order{{Column: "name"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应分 2 组，实际 %d", len(out))
	}
	if out[0].Name != "alice" || out[0].Total != 30 || out[0].Cnt != 2 {
		t.Fatalf("alice 组 total=30 cnt=2，实际 %+v", out[0])
	}
	if out[1].Name != "bob" || out[1].Total != 7 || out[1].Cnt != 1 {
		t.Fatalf("bob 组 total=7 cnt=1，实际 %+v", out[1])
	}
}

// TestSpecJoinLeftTColCoalesce：LEFT JOIN + TCol + COALESCE，无匹配右表列兜底 0。
func TestSpecJoinLeftTColCoalesce(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	bindSpecTable[specWallet](t, e, "spec_wallet")
	seedUsers(t, e,
		&testUser{ID: 1, Name: "alice"},
		&testUser{ID: 2, Name: "bob"},
	)
	for _, w := range []*specWallet{{ID: 1, UID: 1, Balance: 90}} {
		if err := e.save[specWallet](ctx, "spec_wallet", "", w, false); err != nil {
			t.Fatalf("save wallet: %v", err)
		}
	}
	var out []specJoinRow
	o := &Options{
		AllowAnyDest: true,
		Joins:        []Join{{Type: "LEFT", Table: "spec_wallet", Alias: "w", LeftColumn: "id", RightColumn: "uid"}},
		Selects: []SelectItem{
			{Expr: Col("name"), Alias: "name"},
			{Expr: Coalesce(TCol("w", "balance"), Lit(int64(0))), Alias: "balance"},
		},
		Orders: []Order{{Column: "id"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("LEFT JOIN 应保留 2 行，实际 %d", len(out))
	}
	if out[0].Name != "alice" || out[0].Balance != 90 {
		t.Fatalf("alice 有钱包 balance=90，实际 %+v", out[0])
	}
	if out[1].Name != "bob" || out[1].Balance != 0 {
		t.Fatalf("bob 无钱包 COALESCE 兜底 0，实际 %+v", out[1])
	}
}

// TestSpecJoinInnerDropsUnmatched：INNER JOIN 丢弃无匹配左行。
func TestSpecJoinInnerDropsUnmatched(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	bindSpecTable[specWallet](t, e, "spec_wallet")
	seedUsers(t, e,
		&testUser{ID: 1, Name: "alice"},
		&testUser{ID: 2, Name: "bob"},
	)
	if err := e.save[specWallet](ctx, "spec_wallet", "", &specWallet{ID: 1, UID: 1, Balance: 90}, false); err != nil {
		t.Fatalf("save wallet: %v", err)
	}
	var out []specJoinRow
	o := &Options{
		AllowAnyDest: true,
		Joins:        []Join{{Type: "INNER", Table: "spec_wallet", Alias: "w", LeftColumn: "id", RightColumn: "uid"}},
		Selects: []SelectItem{
			{Expr: Col("name"), Alias: "name"},
			{Expr: TCol("w", "balance"), Alias: "balance"},
		},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 1 || out[0].Name != "alice" || out[0].Balance != 90 {
		t.Fatalf("INNER JOIN 应只留 alice，实际 %+v", out)
	}
}

// TestSpecDateBucketGroupByAliasTrend：DateBucket + GroupByAlias 趋势日桶（whatapp trend）。
func TestSpecDateBucketGroupByAliasTrend(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	d1 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	seedUsers(t, e,
		&testUser{ID: 1, Created: d1},
		&testUser{ID: 2, Created: d1},
		&testUser{ID: 3, Created: d2},
	)
	var out []specTrendRow
	o := &Options{
		AllowAnyDest: true,
		Selects: []SelectItem{
			{Expr: DateBucket("created", 0), Alias: "day"},
			{Expr: CountExpr(Star(), false), Alias: "total"},
		},
		GroupAliases: []string{"day"},
		Orders:       []Order{{Column: "day", Alias: true}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应分 2 个日桶，实际 %d", len(out))
	}
	if out[0].Day.Format("2006-01-02") != "2026-11-10" || out[0].Total != 2 {
		t.Fatalf("11-10 桶 count=2，实际 %+v", out[0])
	}
	if out[1].Day.Format("2006-01-02") != "2026-11-11" || out[1].Total != 1 {
		t.Fatalf("11-11 桶 count=1，实际 %+v", out[1])
	}
}

// TestSpecGreatestSubCoalesceSum：SUM(GREATEST(amount-COALESCE(fee,0),0))（whatapp 派发净额）。
func TestSpecGreatestSubCoalesceSum(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	bindSpecTable[specLedger](t, e, "spec_ledger")
	fee30 := int64(30)
	fee20 := int64(20)
	for _, l := range []*specLedger{
		{ID: 1, Amount: 100, Fee: &fee30}, // 70
		{ID: 2, Amount: 50, Fee: nil},     // 50（NULL fee 兜底 0）
		{ID: 3, Amount: 10, Fee: &fee20},  // 0（负数被 GREATEST 截 0）
	} {
		if err := e.save[specLedger](ctx, "spec_ledger", "", l, false); err != nil {
			t.Fatalf("save ledger: %v", err)
		}
	}
	var out specTotalRow
	o := &Options{Agg: &Aggregate{
		Expr:  SumExpr(Greatest(Sub(Col("amount"), Coalesce(Col("fee"), Lit(int64(0)))), Lit(int64(0)))),
		Alias: "total",
	}}
	if err := querySpec[specLedger](e, ctx, "spec_ledger", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if out.Total != 120 {
		t.Fatalf("净额合计应为 70+50+0=120，实际 %d", out.Total)
	}
}

// TestSpecAnyOfGroups：OR 分组（组内任一命中，组间 AND）。
func TestSpecAnyOfGroups(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "alice", Age: 40},
		&testUser{ID: 2, Name: "bob", Age: 20},
		&testUser{ID: 3, Name: "carol", Age: 25},
	)
	var out []testUser
	o := &Options{
		AnyOfGroups: [][]Range{{
			{Column: "age", Op: ">", Value: 30},
			{Column: "name", Op: "=", Value: "bob"},
		}},
		Orders: []Order{{Column: "id"}},
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 || out[0].ID != 1 || out[1].ID != 2 {
		t.Fatalf("age>30 OR name=bob 应命中 id 1/2，实际 %+v", out)
	}
}

// TestSpecRangeFiltersLimitOffset：范围过滤 + 排序 + Offset/Limit 分页。
func TestSpecRangeFiltersLimitOffset(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Age: 10},
		&testUser{ID: 2, Age: 20},
		&testUser{ID: 3, Age: 30},
		&testUser{ID: 4, Age: 40},
		&testUser{ID: 5, Age: 50},
	)
	var out []testUser
	o := &Options{
		Ranges: []Range{{Column: "age", Op: ">=", Value: 20}, {Column: "age", Op: "<=", Value: 40}},
		Orders: []Order{{Column: "age"}},
		Offset: 1,
		Limit:  2,
	}
	if err := querySpec[testUser](e, ctx, "test_user", nil, &out, o); err != nil {
		t.Fatalf("querySpec: %v", err)
	}
	if len(out) != 2 || out[0].Age != 30 || out[1].Age != 40 {
		t.Fatalf("[20,40] 排序后 offset1 limit2 应为 age 30/40，实际 %+v", out)
	}
}

// TestSpecScanBudgetFailsClosed：键集超 MaxScanRows 时 spec 读 fail-closed。
func TestSpecScanBudgetFailsClosed(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	e.cfg.MaxScanRows = 2
	seedUsers(t, e,
		&testUser{ID: 1}, &testUser{ID: 2}, &testUser{ID: 3},
	)
	var out []testUser
	o := &Options{Orders: []Order{{Column: "id"}}}
	err := querySpec[testUser](e, ctx, "test_user", nil, &out, o)
	if !errors.Is(err, ErrScanBudgetExceeded) {
		t.Fatalf("超扫描预算应 fail-closed ErrScanBudgetExceeded，实际 %v", err)
	}
}

// TestQueryRoutesSpecToRedisEngine：公开 Query 带读形状选项时路由到 Redis spec 引擎，不回源。
func TestQueryRoutesSpecToRedisEngine(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	ctx := context.Background()
	seedUsers(t, e,
		&testUser{ID: 1, Name: "a"},
		&testUser{ID: 2, Name: "b"},
		&testUser{ID: 3, Name: "c"},
	)
	var out []testUser
	err := Query[testUser](ctx, "test_user", nil, &out, func(o *Options) {
		o.Limit = 2
		o.Orders = []Order{{Column: "id"}}
	})
	if err != nil {
		t.Fatalf("Query spec: %v", err)
	}
	if len(out) != 2 || out[0].ID != 1 || out[1].ID != 2 {
		t.Fatalf("Limit2 应取 id 1/2，实际 %+v", out)
	}
	if src.listCalls != 0 || src.oneCalls != 0 {
		t.Fatal("公开 Query 的 spec 路径全程 Redis，不得回源")
	}
}
