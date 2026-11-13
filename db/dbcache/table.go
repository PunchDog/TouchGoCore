package dbcache

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/localtimer"
	"touchgocore/vars"
)

// BindOption 是表绑定的可选参数。
type BindOption[T any] struct {
	KeyColumns      []string                          // 组成 Redis 行键的列；默认 [主键列]
	ConflictColumns []string                          // 批量 upsert 冲突列；默认 = KeyColumns
	OrderBy         []string                          // 单行回源排序，如 ["id DESC"]
	FlushInterval   time.Duration                     // 0 = 用全局配置
	TTL             time.Duration                     // 已废弃：权威模式行不过期，>0 钳到 0 并告警
	FlushFn         func(context.Context, []*T) error // 非空时覆盖默认批量 upsert

	Indexes             []string   // 二级索引列：num 列走 score，其余走定宽 lex；与行写同 Lua 维护
	UniqueColumns       [][]string // 唯一列组（行键之外的 DDL 唯一键）：Insert 幂等/Save 守卫
	SecretColumns       []string   // 秘密列白名单（不在缓存行结构体里）：只进 :s:，永不进 :r:
	AutoIncrement       string     // 自增列：Insert 零值时 Redis INCR 取号并回填行结构体
	AllowSourceBackfill bool       // dashed：点查未命中允许回源回填（迁移期用，资金表禁开）
}

// indexSpec 一个二级索引声明（键由 keys.ixPrefix+col 推出）。
type indexSpec struct {
	col  string
	kind string // num|lex
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

	indexes             []indexSpec
	uniques             [][]string
	secrets             map[string]bool
	autoInc             string
	allowSourceBackfill bool

	decode       func(map[string]string) (any, error)
	encodeFields func(any) ([]string, error)
	keyFromRow   func(any) (string, error)
	srcFindOne   func(context.Context, []condVal) (any, error)
	srcFindList  func(context.Context, []condVal) ([]any, error)
	srcPage      func(ctx context.Context, offset, limit int) ([]any, error)
	// seedSeq 把自增序列抬到「源表当前最大 id」之上：换前缀/清库后序列从 0 起，
	// 不抬就会与既有行撞键（Insert 返回 affected=0，只追加台账会因此丢行）。
	// 无自增列或无源时为 nil。
	seedSeq      func(ctx context.Context) (int64, error)
	sinkBatch    func(context.Context, []any) error
	sinkDelete   func(context.Context, []condVal) error
	sinkSecrets  func(context.Context, []condVal, map[string]any) error

	timerMu sync.Mutex
	timer   *tableTimer
	busy    atomic.Bool
}

// writeKeysFor 展开一次写涉及的全部键（lock 占位=row，P1 单 op 不校验锁）。
func (b *binding) writeKeysFor(key string) writeKeys {
	return writeKeys{
		row:      b.keys.rowPrefix + key,
		tomb:     b.keys.tombPrefix + key,
		dirty:    b.keys.dirty,
		retry:    b.keys.retry,
		keySet:   b.keys.keySet,
		secret:   b.keys.secretPrefix + key,
		seq:      b.keys.seq,
		dirtyDel: b.keys.dirtyDel,
		uq:       b.keys.uq,
		lock:     b.keys.rowPrefix + key,
		inflight: b.keys.inflight,
		ix:       b.ixKeys(),
	}
}

// ixKeys 索引键列表（与 idxDescs 同序）。
func (b *binding) ixKeys() []string {
	out := make([]string, len(b.indexes))
	for i, ix := range b.indexes {
		out[i] = b.keys.ixPrefix + ix.col
	}
	return out
}

// idxDescs 索引声明（传给 Lua/参考实现）。
func (b *binding) idxDescs() []idxDesc {
	out := make([]idxDesc, len(b.indexes))
	for i, ix := range b.indexes {
		out[i] = idxDesc{Key: b.keys.ixPrefix + ix.col, Col: ix.col, Kind: ix.kind}
	}
	return out
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
	if ttl > 0 {
		vars.Warning("dbcache %s: 权威模式行不过期，BindOption.TTL %v 钳到 0", table, ttl)
		ttl = 0
	}
	indexes := make([]indexSpec, 0, len(opt.Indexes))
	seenIx := make(map[string]bool, len(opt.Indexes))
	for _, col := range opt.Indexes {
		pos, ok := meta.ByColumn[col]
		if !ok {
			return nil, fmt.Errorf("%w: index column %q not found", ErrBadColumn, col)
		}
		if seenIx[col] {
			continue
		}
		seenIx[col] = true
		kind := "lex"
		if colTypeFor(meta.Fields[pos].Type) == "num" {
			kind = "num"
		}
		indexes = append(indexes, indexSpec{col: col, kind: kind})
	}
	for _, group := range opt.UniqueColumns {
		if len(group) == 0 {
			return nil, fmt.Errorf("%w: empty unique column group", ErrBadColumn)
		}
		for _, col := range group {
			if _, ok := meta.ByColumn[col]; !ok {
				return nil, fmt.Errorf("%w: unique column %q not found", ErrBadColumn, col)
			}
		}
	}
	secrets := make(map[string]bool, len(opt.SecretColumns))
	for _, col := range opt.SecretColumns {
		if !columnNameRe.MatchString(col) {
			return nil, fmt.Errorf("%w: secret column %q", ErrBadColumn, col)
		}
		secrets[col] = true
	}
	if opt.AutoIncrement != "" {
		pos, ok := meta.ByColumn[opt.AutoIncrement]
		if !ok {
			return nil, fmt.Errorf("%w: auto-increment column %q not found", ErrBadColumn, opt.AutoIncrement)
		}
		switch meta.Fields[pos].Type.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			return nil, fmt.Errorf("%w: auto-increment column %q not integer", ErrBadColumn, opt.AutoIncrement)
		}
	}
	b := &binding{
		table:               table,
		typ:                 typ,
		meta:                meta,
		kind:                kind,
		keys:                keys,
		interval:            interval,
		ttl:                 ttl,
		orderBy:             append([]string(nil), opt.OrderBy...),
		conflictCols:        conflict,
		indexes:             indexes,
		uniques:             append([][]string(nil), opt.UniqueColumns...),
		secrets:             secrets,
		autoInc:             opt.AutoIncrement,
		allowSourceBackfill: opt.AllowSourceBackfill,
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
