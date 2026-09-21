package mysql

// ============================================================================
// 观测插件离线回归（S50 补充覆盖）：after 回调原先直接取
// db.Statement.Schema.Table，而 gorm 在 Count/Raw/Row 这类语句上不一定解析得出
// Schema，于是「上报查询耗时」这一步自己再 panic 一次，把真正的数据库错误盖掉。
//
// 判空的位置也在断言之列：gorm 的 DB.Get 实现就是
// `db.Statement.Settings.Load(key)`，若把 Statement 判空写在 db.Get 之后，
// nil Statement 会先进了 gorm 再回来，那句保护形同虚设 —— 这条由
// TestAfterSurvivesNilStatement 钉住。
// ============================================================================

import (
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// captureHook 只记录上报入参，够判「报成了哪个表名」。
type captureHook struct {
	ops  []string
	slow []string
}

func (h *captureHook) OnQuery(op string, _ time.Duration, _ Kind) { h.ops = append(h.ops, op) }
func (h *captureHook) OnSlowQuery(op, _ string, _ time.Duration)  { h.slow = append(h.slow, op) }
func (h *captureHook) OnConnection(string, float64)               {}

func newTestPlugin(hook MetricsHook) *observabilityPlugin {
	return NewObservabilityPlugin(hook, 10*time.Millisecond).(*observabilityPlugin)
}

// reportWith 用给定语句上下文跑一次 after，返回上报的 op。
// 起始时间手动写进 Settings，等价于 before 回调刚跑过。
func reportWith(t *testing.T, stmt *gorm.Statement) string {
	t.Helper()

	stmt.Settings.Store(ctxStartKey, time.Now().Add(-time.Second))
	// gorm 的 Statement 内嵌 *DB，after 里 db.Statement.Error 读的就是这条内嵌链；
	// 手工造的上下文必须双向回指，否则测的是自己搭出来的畸形对象而不是产品代码。
	stmt.DB = &gorm.DB{Statement: stmt}

	hook := &captureHook{}
	newTestPlugin(hook).after(stmt.DB)

	if len(hook.ops) != 1 {
		t.Fatalf("✘ OnQuery 应恰好一次，实际 %d 次", len(hook.ops))
	}
	// 耗时 1 秒 > 阈值，慢查询分支同样要摸到 Schema/SQL，一并校验。
	if len(hook.slow) != 1 || hook.slow[0] != hook.ops[0] {
		t.Fatalf("✘ 慢查询上报与 OnQuery 口径不一致: slow=%v ops=%v", hook.slow, hook.ops)
	}
	return hook.ops[0]
}

// TestAfterSurvivesNilSchema：无 Schema（Count/Raw 之类）必须回退 raw 而不是 panic。
func TestRegression_AfterSurvivesNilSchema(t *testing.T) {
	if got := reportWith(t, &gorm.Statement{}); got != "raw" {
		t.Fatalf("✘ 无 Schema 时 op 应为 raw，实际 %q", got)
	}
}

// TestAfterReportsTableName：有 Schema 时按表名上报，兜底不该变成常态。
func TestRegression_AfterReportsTableName(t *testing.T) {
	stmt := &gorm.Statement{Schema: &schema.Schema{Table: "repo_probe_rows"}}
	if got := reportWith(t, stmt); got != "repo_probe_rows" {
		t.Fatalf("✘ op 应为表名，实际 %q", got)
	}
}

// TestAfterFallsBackOnEmptyTableName：Schema 在而表名为空（表达式建表等）同样回退 raw。
func TestRegression_AfterFallsBackOnEmptyTableName(t *testing.T) {
	stmt := &gorm.Statement{Schema: &schema.Schema{}}
	if got := reportWith(t, stmt); got != "raw" {
		t.Fatalf("✘ 空表名应回退 raw，实际 %q", got)
	}
}

// TestAfterSurvivesNilStatement：没有语句上下文时静默跳过上报，
// 且必须是在 gorm 的 DB.Get 之前就跳过。
func TestRegression_AfterSurvivesNilStatement(t *testing.T) {
	hook := &captureHook{}
	newTestPlugin(hook).after(&gorm.DB{})

	if len(hook.ops) != 0 || len(hook.slow) != 0 {
		t.Fatalf("✘ 无 Statement 不该有任何上报: ops=%v slow=%v", hook.ops, hook.slow)
	}
}
