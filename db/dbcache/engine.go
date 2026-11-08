package dbcache

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"

	mysqldb "touchgocore/db/mysql"
)

// Engine 是缓存层实例：持有一张张表的绑定、Redis 语义存储与生命周期状态。
type Engine struct {
	mu      sync.RWMutex
	cfg     Config
	store   kvStore
	tables  map[string]*binding
	running bool
	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool
	// mysqlClient 由 BindMySQL 注入，供事务/原子写原语使用（不对外暴露）。
	mysqlClient *mysqldb.Client
}

var defaultEngine atomic.Pointer[Engine]

// newEngine 构造实例；测试可直接注入内存 kvStore。
func newEngine(cfg Config, st kvStore) *Engine {
	return &Engine{
		cfg:    cfg.normalize(),
		store:  st,
		tables: make(map[string]*binding),
	}
}

// Init 创建并安装默认引擎。rdb 为 nil 时返回错误。
func Init(cfg Config, rdb redis.Cmdable) error {
	if rdb == nil {
		return fmt.Errorf("%w: redis client is nil", ErrNotInitialized)
	}
	defaultEngine.Store(newEngine(cfg, newRedisStore(rdb)))
	return nil
}

// Default 返回默认引擎；未初始化时为 nil。
func Default() *Engine { return defaultEngine.Load() }

// Close 卸载默认引擎并尽力完成 final flush（幂等）。
func Close() {
	e := defaultEngine.Swap(nil)
	if e == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.CloseBudget)
	defer cancel()
	_ = e.Stop(ctx)
}

// Start 启动默认引擎的延迟落库定时器。
func Start(ctx context.Context) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	return e.Start(ctx)
}

// Stop 停止默认引擎并执行 final flush。
func Stop(ctx context.Context) error {
	e := defaultEngine.Load()
	if e == nil {
		return nil
	}
	return e.Stop(ctx)
}

// Flush 立即把指定表的全部脏键写入数据库。
func Flush(ctx context.Context, table string) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	b, err := e.lookupTable(table)
	if err != nil {
		return err
	}
	return e.flushAll(ctx, b)
}

// Start 启动本实例的延迟落库定时器；重复调用幂等。
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.running = true
	e.stopped.Store(false)
	tables := make([]*binding, 0, len(e.tables))
	for _, b := range e.tables {
		tables = append(tables, b)
	}
	e.mu.Unlock()

	started := make([]*binding, 0, len(tables))
	for _, b := range tables {
		if err := startBindingTimer(e, b); err != nil {
			for _, sb := range started {
				_ = stopBinding(e, sb, context.Background())
			}
			e.mu.Lock()
			e.running = false
			e.mu.Unlock()
			return err
		}
		started = append(started, b)
	}
	return nil
}

// Stop 停止定时器并逐表 final flush；仍有脏键时返回 ErrFlushIncomplete。
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	e.running = false
	e.stopped.Store(true)
	cancel := e.cancel
	e.cancel = nil
	tables := make([]*binding, 0, len(e.tables))
	for _, b := range e.tables {
		tables = append(tables, b)
	}
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fctx, fcancel := context.WithTimeout(ctx, e.cfg.CloseBudget)
	defer fcancel()

	var failures []string
	for _, b := range tables {
		if err := stopBinding(e, b, fctx); err != nil {
			failures = append(failures, err.Error())
		}
		if n := e.pendingCount(fctx, b); n > 0 {
			failures = append(failures, fmt.Sprintf("%s:%d", b.table, n))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%w: %s", ErrFlushIncomplete, strings.Join(failures, ", "))
	}
	return nil
}

// register 登记一张表的绑定；重复登记返回 ErrTableBound。
func (e *Engine) register(b *binding) error {
	e.mu.Lock()
	if _, ok := e.tables[b.table]; ok {
		e.mu.Unlock()
		return ErrTableBound
	}
	e.tables[b.table] = b
	running := e.running
	e.mu.Unlock()
	if running {
		return startBindingTimer(e, b)
	}
	return nil
}

// lookupTable 按表名取绑定，不校验模型类型。
func (e *Engine) lookupTable(table string) (*binding, error) {
	e.mu.RLock()
	b := e.tables[table]
	e.mu.RUnlock()
	if b == nil {
		return nil, fmt.Errorf("%w: %s", ErrTableUnbound, table)
	}
	return b, nil
}

// lookupTyped 按表名取绑定并校验模型类型。
func (e *Engine) lookupTyped(table string, typOf reflect.Type) (*binding, error) {
	b, err := e.lookupTable(table)
	if err != nil {
		return nil, err
	}
	if b.typ != typOf {
		return nil, fmt.Errorf("%w: %s", ErrTypeMismatch, table)
	}
	return b, nil
}
