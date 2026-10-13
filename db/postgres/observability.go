package postgres

import (
	"time"

	"gorm.io/gorm"
)

// MetricsHook 是 PostgreSQL 包与可观测性之间的解耦接口
type MetricsHook interface {
	OnQuery(op string, dur time.Duration, kind Kind)
	OnSlowQuery(op string, sql string, dur time.Duration)
	OnConnection(state string, n float64)
}

// NoopMetrics 默认空实现
type NoopMetrics struct{}

// OnQuery 空实现
func (NoopMetrics) OnQuery(string, time.Duration, Kind) {}

// OnSlowQuery 空实现
func (NoopMetrics) OnSlowQuery(string, string, time.Duration) {}

// OnConnection 空实现
func (NoopMetrics) OnConnection(string, float64) {}

// observabilityPlugin gorm 插件
type observabilityPlugin struct {
	hook    MetricsHook
	slowThr time.Duration
}

// NewObservabilityPlugin 创建 gorm.Plugin
func NewObservabilityPlugin(hook MetricsHook, slowThreshold time.Duration) gorm.Plugin {
	if hook == nil {
		hook = NoopMetrics{}
	}
	if slowThreshold <= 0 {
		slowThreshold = 200 * time.Millisecond
	}
	return &observabilityPlugin{hook: hook, slowThr: slowThreshold}
}

// Name 实现 gorm.Plugin
func (o *observabilityPlugin) Name() string { return "touchgocore-postgres-observability" }

// Initialize 注册 Before/After 回调
func (o *observabilityPlugin) Initialize(db *gorm.DB) error {
	beforeName := "touchgocore-postgres:observability:before"
	afterName := "touchgocore-postgres-observability:after"

	if err := db.Callback().Create().Before("gorm:before_create").Register(beforeName, o.before); err != nil {
		return err
	}
	if err := db.Callback().Query().Before("gorm:query").Register(beforeName, o.before); err != nil {
		return err
	}
	if err := db.Callback().Update().Before("gorm:before_update").Register(beforeName, o.before); err != nil {
		return err
	}
	if err := db.Callback().Delete().Before("gorm:before_delete").Register(beforeName, o.before); err != nil {
		return err
	}

	if err := db.Callback().Create().After("gorm:after_create").Register(afterName, o.after); err != nil {
		return err
	}
	if err := db.Callback().Query().After("gorm:after_query").Register(afterName, o.after); err != nil {
		return err
	}
	if err := db.Callback().Update().After("gorm:after_update").Register(afterName, o.after); err != nil {
		return err
	}
	if err := db.Callback().Delete().After("gorm:after_delete").Register(afterName, o.after); err != nil {
		return err
	}
	return nil
}

const ctxStartKey = "touchgocore_postgres_observability_start"

func (o *observabilityPlugin) before(db *gorm.DB) {
	db.Set(ctxStartKey, time.Now())
}

func (o *observabilityPlugin) after(db *gorm.DB) {
	if db == nil || db.Statement == nil {
		return
	}
	v, ok := db.Get(ctxStartKey)
	if !ok {
		return
	}
	start, ok := v.(time.Time)
	if !ok {
		return
	}
	dur := time.Since(start)
	// gorm 的 Count/Row/Raw 场景下 db.Statement.Schema 可能为 nil，做空值保护
	op := "raw"
	if db.Statement.Schema != nil {
		if table := db.Statement.Schema.Table; table != "" {
			op = table
		}
	}
	kind := classify(db.Statement.Error)
	o.hook.OnQuery(op, dur, kind)
	if db.Statement.Error == nil && dur >= o.slowThr {
		o.hook.OnSlowQuery(op, db.Statement.SQL.String(), dur)
	}
}
