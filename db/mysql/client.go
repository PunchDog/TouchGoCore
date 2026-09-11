package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/config"
	"touchgocore/db/dbmap"
	"touchgocore/vars"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	defaultSlowThreshold   = 200 * time.Millisecond
	defaultConnMaxLifetime = 24 * time.Hour
	defaultCharset         = "utf8mb4"
	defaultCollation       = "utf8mb4_unicode_ci"
)

// Client 是 MySQL 客户端入口
type Client struct {
	cfg    *config.MySqlDBConfig
	engine *gorm.DB
	opts   options
	mu     sync.RWMutex
	closed atomic.Bool
}

// options 内部不可变配置
type options struct {
	slowThreshold   time.Duration
	metricsHook     MetricsHook
	connMaxLifetime time.Duration
	loggerLevel     logger.LogLevel
	extraDSNParams  map[string]string
	charset         string
	collation       string
	autoMigrate     bool
}

// Option 函数式配置
type Option func(*options)

// WithSlowQueryThreshold 设置慢查询阈值
func WithSlowQueryThreshold(d time.Duration) Option {
	return func(o *options) { o.slowThreshold = d }
}

// WithMetricsHook 注入自定义可观测性 hook
func WithMetricsHook(h MetricsHook) Option {
	return func(o *options) { o.metricsHook = h }
}

// WithConnMaxLifetime 设置连接最大存活时间
func WithConnMaxLifetime(d time.Duration) Option {
	return func(o *options) { o.connMaxLifetime = d }
}

// WithLoggerLevel 设置 gorm 日志级别
func WithLoggerLevel(l logger.LogLevel) Option {
	return func(o *options) { o.loggerLevel = l }
}

// WithDSNParam 添加自定义 DSN 参数
func WithDSNParam(k, v string) Option {
	return func(o *options) {
		if o.extraDSNParams == nil {
			o.extraDSNParams = make(map[string]string)
		}
		o.extraDSNParams[k] = v
	}
}

// WithCharset 覆盖字符集
func WithCharset(charset, collation string) Option {
	return func(o *options) {
		o.charset = charset
		o.collation = collation
	}
}

// WithAutoMigrate 启用/关闭「表不存在自动建表」。
//
// 默认 false。开启后，Repository[T] 首次执行写/读操作时，若目标表不存在，
// 自动调用 gorm AutoMigrate(T) 建表并重试一次；之后该 Repository 实例内不再检测。
func WithAutoMigrate(enable bool) Option {
	return func(o *options) { o.autoMigrate = enable }
}

func defaultOptions() options {
	return options{
		slowThreshold:   defaultSlowThreshold,
		metricsHook:     NoopMetrics{},
		connMaxLifetime: defaultConnMaxLifetime,
		loggerLevel:     logger.Silent,
		charset:         defaultCharset,
		collation:       defaultCollation,
		extraDSNParams:  make(map[string]string),
	}
}

// poolKey 基于连接配置生成稳定的注册表键：host + dbname + username + password 前 8 字节哈希。
// 不同账号/不同库天然区分，密码变更会触发新建连接。
func (c *Client) poolKey() string {
	cfg := c.cfg
	sum := sha256.Sum256([]byte(cfg.Password))
	return cfg.Host + "-" + cfg.DBName + "-" + cfg.Username + "-" + hex.EncodeToString(sum[:8])
}

// connectOnly 尝试从全局注册表复用现有 *gorm.DB，复用 socket 连接池。
// 命中后 c.engine 被填充，返回 true；未命中返回 false，调用方继续执行完整初始化。
func (c *Client) connectOnly(key string) bool {
	v, ok := dbmap.Global.Load(key)
	if !ok {
		return false
	}
	engine, ok := v.(*gorm.DB)
	if !ok {
		return false
	}
	// 健康检查：底层 sql.DB 已关闭则视为失效，重新拨号
	sqlDB, err := engine.DB()
	if err != nil || sqlDB == nil {
		dbmap.Global.Delete(key)
		return false
	}
	// 健康检查：探测底层 sql.DB 是否仍可用（底层连接已被 Close 则 Ping 应失败）
	pingCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	if err := sqlDB.PingContext(pingCtx); err != nil {
		cancel()
		dbmap.Global.Delete(key)
		return false
	}
	cancel()
	return true
}

// NewClient 创建 MySQL 客户端。同配置多次调用会复用同一 socket 连接池（首次完整初始化，后续命中注册表）。
func NewClient(cfg *config.MySqlDBConfig, opts ...Option) (*Client, error) {
	if cfg == nil {
		return nil, wrap("NewClient", fmt.Errorf("nil config"), KindInvalidColumn)
	}
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}

	c := &Client{cfg: cfg, opts: o}
	key := c.poolKey()

	// 命中既有连接，直接复用
	if c.connectOnly(key) {
		sqlDB, _ := c.engine.DB()
		if sqlDB != nil {
			stats := sqlDB.Stats()
			o.metricsHook.OnConnection("idle", float64(stats.Idle))
			o.metricsHook.OnConnection("open", float64(stats.OpenConnections))
		}
		vars.Info("MySQL 复用连接 key=%s host=%s db=%s", key, cfg.Host, cfg.DBName)
		return c, nil
	}

	// 首次创建：完整拨号 + 注册可观测性插件
	start := time.Now()
	dsn := buildDSNWithOptions(cfg, o)
	engine, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(o.loggerLevel),
	})
	if err != nil {
		return nil, newError("Open", fmt.Errorf("failed to open mysql: %w", err),
			classify(err), dsn, nil, time.Since(start))
	}
	sqlDB, err := engine.DB()
	if err != nil {
		return nil, wrap("Open", err, KindConnFailed)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	sqlDB.SetConnMaxLifetime(o.connMaxLifetime)

	if err := engine.Use(NewObservabilityPlugin(o.metricsHook, o.slowThreshold)); err != nil {
		vars.Warning("注册可观测性插件失败: %v", err)
	}
	vars.Info("MySQL 连接成功 host=%s db=%s", cfg.Host, cfg.DBName)

	c.engine = engine
	dbmap.Global.Store(key, engine)
	stats := sqlDB.Stats()
	o.metricsHook.OnConnection("idle", float64(stats.Idle))
	o.metricsHook.OnConnection("open", float64(stats.OpenConnections))
	return c, nil
}

// Close 关闭客户端。幂等；首次调用关闭底层 socket 并清出注册表，后续调用 no-op。
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sqlDB, err := c.engine.DB()
	if err == nil && sqlDB != nil {
		if err := sqlDB.Close(); err != nil {
			vars.Warning("MySQL 关闭 socket 失败: %v", err)
		}
	}
	dbmap.Global.Delete(c.poolKey())
	c.engine = nil
	return nil
}

// Ping 健康检查
func (c *Client) Ping(ctx context.Context) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed.Load() {
		return wrap("Ping", fmt.Errorf("client closed"), KindConnFailed)
	}
	sqlDB, err := c.engine.DB()
	if err != nil {
		return wrap("Ping", err, KindConnFailed)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return newError("Ping", err, classify(err), "", nil, 0)
	}
	return nil
}

// Stats 返回当前连接池状态
func (c *Client) Stats() (sql.DBStats, error) {
	sqlDB, err := c.engine.DB()
	if err != nil {
		return sql.DBStats{}, wrap("Stats", err, KindConnFailed)
	}
	return sqlDB.Stats(), nil
}

// Config 返回不可变配置
func (c *Client) Config() *config.MySqlDBConfig { return c.cfg }

// Engine 返回内部 gorm.DB
func (c *Client) Engine() *gorm.DB { return c.engine }

// AutoMigrateEnabled 返回是否启用了「表不存在自动建表」。
// Repository[T] 通过此判断决定是否在查询前后做迁移检测。
func (c *Client) AutoMigrateEnabled() bool { return c.opts.autoMigrate }

// WithContext 返回绑定 ctx 的会话
func (c *Client) WithContext(ctx context.Context) *Session {
	return &Session{client: c, ctx: ctx}
}

// BeginTx 开启事务
func (c *Client) BeginTx(ctx context.Context, opts ...*sql.TxOptions) (*Tx, error) {
	if c.closed.Load() {
		return nil, wrap("BeginTx", fmt.Errorf("client closed"), KindConnFailed)
	}
	var gormOpts *sql.TxOptions
	if len(opts) > 0 {
		gormOpts = opts[0]
	}
	tx := c.engine.WithContext(ctx).Begin(gormOpts)
	if tx.Error != nil {
		return nil, newError("BeginTx", tx.Error, classify(tx.Error), "", nil, 0)
	}
	return &Tx{DB: tx, client: c, ctx: ctx}, nil
}

// Raw 提供 gorm 逃生舱
func (c *Client) Raw(ctx context.Context, fn func(tx *gorm.DB) error) error {
	err := fn(c.engine.WithContext(ctx))
	if err == nil {
		return nil
	}
	return newError("Raw", err, classify(err), "", nil, 0)
}

// Repository 入口（返回非泛型工厂，因 Go 1.26.7 工具链限制不能在方法上声明类型参数）
func (c *Client) Repository() *RepositoryFactory {
	return &RepositoryFactory{client: c, ctx: context.Background()}
}

// RepositoryFactory 非泛型工厂
type RepositoryFactory struct {
	client *Client
	ctx    context.Context
}

// WithContext 替换 ctx
func (f *RepositoryFactory) WithContext(ctx context.Context) *RepositoryFactory {
	cp := *f
	cp.ctx = ctx
	return &cp
}

// Client 返回底层 Client
func (f *RepositoryFactory) Client() *Client { return f.client }

// Context 返回 ctx
func (f *RepositoryFactory) Context() context.Context { return f.ctx }

// buildDSNWithOptions 拼装 DSN
func buildDSNWithOptions(cfg *config.MySqlDBConfig, o options) string {
	charset := o.charset
	collation := o.collation
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&loc=Local&charset=%s&collation=%s",
		cfg.Username, cfg.Password, cfg.Host, cfg.DBName, charset, collation)
	if len(o.extraDSNParams) > 0 {
		dsn += "&"
		first := true
		for k, v := range o.extraDSNParams {
			if !first {
				dsn += "&"
			}
			dsn += fmt.Sprintf("%s=%s", k, v)
			first = false
		}
	}
	return dsn
}