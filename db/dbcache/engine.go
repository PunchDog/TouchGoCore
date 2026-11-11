package dbcache

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"touchgocore/vars"
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
	// sinkWarned 记录已告警「mustSave 无 sink」的表，避免日志刷屏。
	sinkWarned sync.Map
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
// RequireAOF=true 时检查 Redis AOF，缺失响亮告警（不拒启动，兼容未开 AOF 的部署）。
func Init(cfg Config, rdb redis.Cmdable) error {
	if rdb == nil {
		return fmt.Errorf("%w: redis client is nil", ErrNotInitialized)
	}
	cfg = cfg.normalize()
	if cfg.RequireAOF {
		checkAOF(rdb)
	}
	defaultEngine.Store(newEngine(cfg, newRedisStore(rdb)))
	return nil
}

// checkAOF 读取 persistence INFO 校验 appendonly；读不到或未开启都只告警。
func checkAOF(rdb redis.Cmdable) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := rdb.Info(ctx, "persistence").Result()
	if err != nil {
		vars.Warning("dbcache: 无法读取 Redis persistence INFO，跳过 AOF 校验: %v", err)
		return
	}
	if !strings.Contains(info, "appendonly:yes") {
		vars.Warning("dbcache: Redis 未开启 AOF（appendonly:yes），权威模式崩溃可能丢最后一段写；建议开启 AOF")
	}
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

// FlushAll 把所有存在待物化脏账（dirty / dirtyDel）的表立即落库；无脏账的表跳过。
// 显式同步点（事务删除后、DB 断言前）用它，避免逐表空转。
func FlushAll(ctx context.Context) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	e.mu.RLock()
	tables := make([]*binding, 0, len(e.tables))
	for _, b := range e.tables {
		tables = append(tables, b)
	}
	e.mu.RUnlock()
	var first error
	for _, b := range tables {
		if e.pendingCount(ctx, b) == 0 {
			continue
		}
		if err := e.flushAll(ctx, b); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// FlushKey 精确物化一个键：先落 dirty（upsert），再落 dirtyDel（删除）。
// 事务提交后的同步点用它——整表 flush 会被同表一条坏行（如约束违例）拖垮。
func FlushKey(ctx context.Context, table, key string) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	b, err := e.lookupTable(table)
	if err != nil {
		return err
	}
	if key == "" {
		return ErrKeyMissing
	}
	if err := e.flushKey(ctx, b, key); err != nil {
		return err
	}
	// 删除账单独处理：flushKey 只认 dirty 账本。
	_, ok, err := e.store.ZScore(ctx, b.keys.dirtyDel, key)
	if err != nil || !ok {
		return err
	}
	if err := e.store.ZRem(ctx, b.keys.dirtyDel, key); err != nil {
		return err
	}
	cur, err := e.store.HGetAll(ctx, b.keys.rowPrefix+key)
	if err != nil {
		return err
	}
	if len(cur) > 0 {
		return nil // 删除后同键已重建，本次删除作废
	}
	vals, err := valsFromKey(b.meta, key)
	if err != nil {
		return err
	}
	if err := e.deleteSink(ctx, b, vals); err != nil {
		e.markRetry(ctx, b, b.keys.dirtyDel, key)
		return err
	}
	_ = e.store.HDel(ctx, b.keys.retry, key)
	return nil
}

// KeyOf 按表绑定从行推导 Redis 行键（行自带自增主键时须先拿到回填后的值）。
func KeyOf[T any](table string, row *T) (string, error) {
	e := defaultEngine.Load()
	if e == nil {
		return "", ErrNotInitialized
	}
	if row == nil {
		return "", ErrNilRow
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return "", err
	}
	return b.keyFromRow(any(row))
}

// KeyFromCond 按表绑定从「完整覆盖行键」的条件结构体推导行键。
func KeyFromCond(table string, cond any) (string, error) {
	e := defaultEngine.Load()
	if e == nil {
		return "", ErrNotInitialized
	}
	b, err := e.lookupTable(table)
	if err != nil {
		return "", err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return "", err
	}
	if !condIsExactKey(b.meta, vals) {
		return "", fmt.Errorf("%w: %s", ErrPartialKey, table)
	}
	return keyFromCond(b.meta, vals)
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
		// 启动即回收上次进程崩溃遗留的过期租约，重挂脏账继续落库。
		if _, err := e.store.RequeueInflight(e.ctx, b.keys.inflight, b.keys.dirty, time.Now().UnixMilli()); err != nil {
			vars.Warning("dbcache %s: start requeue inflight: %v", b.table, err)
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
