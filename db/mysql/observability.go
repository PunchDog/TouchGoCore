package mysql

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// MetricsHook 是 MySQL 包与可观测性之间的解耦接口
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
func (o *observabilityPlugin) Name() string { return "touchgocore-mysql-observability" }

// Initialize 注册 Before/After 回调
func (o *observabilityPlugin) Initialize(db *gorm.DB) error {
	beforeName := "touchgocore-mysql:observability:before"
	afterName := "touchgocore-mysql-observability:after"

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

const ctxStartKey = "touchgocore_mysql_observability_start"

func (o *observabilityPlugin) before(db *gorm.DB) {
	db.Set(ctxStartKey, time.Now())
}

func (o *observabilityPlugin) after(db *gorm.DB) {
	// 判空必须放在取起点之前：gorm 的 DB.Get 实现就是
	// db.Statement.Settings.Load(...)，Statement 为 nil 时先在 gorm 内部炸掉，
	// 判在下游等于没判。
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
	// 注意：gorm 的 Count/Row/Raw 等场景下 db.Statement.Schema 可能为 nil，
	// 直接取 .Table 会触发空指针 panic。此处做空值保护：有 Schema 且有表名时用表名，否则回退 "raw"。
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

// loggerFromGorm 把 gorm LogLevel 转为 logger.Interface（占位实现）
func loggerFromGorm(level logger.LogLevel) logger.Interface {
	return logger.Default.LogMode(level)
}