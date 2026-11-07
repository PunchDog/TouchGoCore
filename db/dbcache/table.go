package dbcache

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/localtimer"
)

// BindOption 是表绑定的可选参数。
type BindOption[T any] struct {
	KeyColumns      []string                          // 组成 Redis 行键的列；默认 [主键列]
	ConflictColumns []string                          // 批量 upsert 冲突列；默认 = KeyColumns
	OrderBy         []string                          // 单行回源排序，如 ["id DESC"]
	FlushInterval   time.Duration                     // 0 = 用全局配置
	TTL             time.Duration                     // 0 = 用全局配置
	FlushFn         func(context.Context, []*T) error // 非空时覆盖默认批量 upsert
}

// binding 是一张表的类型擦除后绑定：读写闭包 + Redis 键 + 定时器。
type binding struct {
	table        string
	typ          reflect.Type
	meta         *tableMeta
	kind         backendKind
	keys         tableKeys
	interval     time.Duration
	ttl          time.Duration
	orderBy      []string
	conflictCols []string

	decode       func(map[string]string) (any, error)
	encodeFields func(any) ([]string, error)
	keyFromRow   func(any) (string, error)
	srcFindOne   func(context.Context, []condVal) (any, error)
	srcFindList  func(context.Context, []condVal) ([]any, error)
	sinkBatch    func(context.Context, []any) error

	timerMu sync.Mutex
	timer   *tableTimer
	busy    atomic.Bool
}

// tableTimer 是每表一个的周期落库定时器；Tick 异步执行，避免阻塞时间轮。
type tableTimer struct {
	localtimer.Timer
	engine  *Engine
	binding *binding
}

// Tick 触发本表的到期脏键落库。
func (t *tableTimer) Tick() { t.engine.flushDue(t.binding) }

// IsAsyncTick 让落库在定时器异步工作池中执行。
func (t *tableTimer) IsAsyncTick() bool { return true }

// newBinding 构建表绑定并生成编解码闭包。
func newBinding[T any](table string, kind backendKind, opt BindOption[T], keys tableKeys, cfg Config) (*binding, error) {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	meta, err := buildMeta(typ, opt.KeyColumns, kind)
	if err != nil {
		return nil, err
	}
	if !meta.HasKey {
		return nil, fmt.Errorf("%w: table %s has no key columns", ErrKeyMissing, table)
	}
	conflict := opt.ConflictColumns
	if len(conflict) == 0 {
		conflict = append([]string(nil), meta.KeyCols...)
	}
	for _, col := range conflict {
		if _, ok := meta.ByColumn[col]; !ok {
			return nil, fmt.Errorf("%w: conflict column %q not found", ErrBadColumn, col)
		}
	}
	interval := opt.FlushInterval
	if interval <= 0 {
		interval = cfg.FlushInterval
	}
	ttl := opt.TTL
	if ttl <= 0 {
		ttl = cfg.TTL
	}
	b := &binding{
		table:        table,
		typ:          typ,
		meta:         meta,
		kind:         kind,
		keys:         keys,
		interval:     interval,
		ttl:          ttl,
		orderBy:      append([]string(nil), opt.OrderBy...),
		conflictCols: conflict,
	}
	b.decode = func(h map[string]string) (any, error) {
		var out T
		if err := decodeRow(meta, reflect.ValueOf(&out).Elem(), h); err != nil {
			return nil, err
		}
		return &out, nil
	}
	b.encodeFields = func(row any) ([]string, error) {
		return encodeRow(meta, reflect.ValueOf(row).Elem())
	}
	b.keyFromRow = func(row any) (string, error) {
		return keyFromRow(meta, reflect.ValueOf(row).Elem())
	}
	return b, nil
}

// startBindingTimer 为本表创建并注册周期定时器。
func startBindingTimer(e *Engine, b *binding) error {
	b.timerMu.Lock()
	if b.timer != nil {
		b.timerMu.Unlock()
		return nil
	}
	ms := b.interval.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	tm, err := localtimer.NewTimer[*tableTimer](ms, -1, func(t *tableTimer) {
		t.engine = e
		t.binding = b
	})
	if err != nil {
		b.timerMu.Unlock()
		return err
	}
	b.timer = tm
	b.timerMu.Unlock()
	if err := localtimer.AddTimer(tm); err != nil {
		tm.Remove()
		b.timerMu.Lock()
		b.timer = nil
		b.timerMu.Unlock()
		return err
	}
	return nil
}

// stopBinding 停止本表定时器，等待在途 Tick 退出后执行 final flush。
func stopBinding(e *Engine, b *binding, ctx context.Context) error {
	b.timerMu.Lock()
	tm := b.timer
	b.timer = nil
	b.timerMu.Unlock()
	if tm != nil {
		tm.Pause()
	}
	deadline := time.Now().Add(e.cfg.CloseBudget)
	for b.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	err := e.flushAll(ctx, b)
	if tm != nil {
		tm.Remove()
	}
	return err
}
