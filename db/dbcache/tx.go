package dbcache

// ============================================================================
// 事务原语：Redis 权威的延迟原子批。
//
// BeginTx 不再依赖 MySQL 连接：事务状态是一份私有 overlay（txState），写缓冲成
// writeOp 序列并在 overlay 上即时求值（读己写），Commit 经 txScript 一次原子重放
// 到 Redis，Rollback 直接丢弃 overlay。MySQL 只是提交后由脏账异步物化的 sink。
// ============================================================================

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// startTxSpan 以调用方 / 事务 ctx 为父开启 span；ctx 为 nil 时回退 background。
//
// 不引入全局 tracer 单例（otel.Tracer 是包级函数，未配置 provider 时返回 noop），
// 也不配置采样率 / 后端：埋点只提供上下文透传，不改变任何业务语义。
func startTxSpan(ctx context.Context, spanName string) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	return otel.Tracer("touchgocore.dbcache.tx").Start(ctx, spanName)
}

// recordTxSpanError 把错误记到 span；不改变任何返回值语义。
func recordTxSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Tx 是一段框架内部事务的句柄；由 BeginTx 创建，内部持有 Redis 权威 overlay。
type Tx struct {
	st  *txState
	ctx context.Context
}

// BeginTx 开启缓存层事务（只依赖 Redis，不需要 MySQL 连接）。
func BeginTx(ctx context.Context) (*Tx, error) {
	ctx, span := startTxSpan(ctx, "touchgocore.dbcache.tx.BeginTx")
	defer span.End()
	e := Default()
	if e == nil {
		recordTxSpanError(span, ErrNotInitialized)
		return nil, ErrNotInitialized
	}
	st := newTxState(e, ctx)
	return &Tx{st: st, ctx: context.WithValue(ctx, txKey{}, st)}, nil
}

// Commit 经 txScript 一次原子重放全部缓冲写到 Redis；无论成败都释放行锁。
func (t *Tx) Commit() error {
	_, span := startTxSpan(t.Context(), "touchgocore.dbcache.tx.Commit")
	defer span.End()
	st, err := t.state()
	if err != nil {
		recordTxSpanError(span, err)
		return err
	}
	err = st.commit(t.ctx)
	recordTxSpanError(span, err)
	return err
}

// Rollback 丢弃 overlay 与缓冲写、释放行锁；Redis 分毫不动。
func (t *Tx) Rollback() error {
	_, span := startTxSpan(t.Context(), "touchgocore.dbcache.tx.Rollback")
	defer span.End()
	st, err := t.state()
	if err != nil {
		recordTxSpanError(span, err)
		return err
	}
	err = st.rollback(t.ctx)
	recordTxSpanError(span, err)
	return err
}

// Context 返回事务 ctx（携带 txState，供四个数据接口在事务内路由）。
func (t *Tx) Context() context.Context {
	if t == nil {
		return context.Background()
	}
	return t.ctx
}

// state 返回事务状态；句柄无效时 ErrNotInitialized。
func (t *Tx) state() (*txState, error) {
	if t == nil || t.st == nil {
		return nil, ErrNotInitialized
	}
	return t.st, nil
}

// TxQuery 事务内查询：单条（*T）或列表（*[]T），读 overlay 叠加 Redis 真值（读己写）。
func TxQuery[T any](t *Tx, ctx context.Context, table string, cond any, out any) error {
	st, err := t.state()
	if err != nil {
		return err
	}
	b, err := st.e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return err
	}
	rows, err := st.matchingRows(ctx, b, vals, &Options{})
	if err != nil {
		return err
	}
	return decodeRows[T](b, rows, out)
}

// TxLockOne 事务内按完整行键取行锁并读单行（读己写）；非完整行键拒绝。
func TxLockOne[T any](t *Tx, ctx context.Context, table string, cond any, out *T) error {
	st, err := t.state()
	if err != nil {
		return err
	}
	b, err := st.e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return err
	}
	if !condIsExactKey(b.meta, vals) {
		return ErrPartialKey
	}
	rows, err := st.matchingRows(ctx, b, vals, &Options{ForUpdate: true})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrNoRows
	}
	row, err := b.decode(rows[0])
	if err != nil {
		return err
	}
	*out = *row.(*T)
	return nil
}

// TxSave 事务内整行写（冲突列 = 绑定的 KeyColumns），结果即时进 overlay。
func TxSave[T any](t *Tx, ctx context.Context, table string, row *T) error {
	if row == nil {
		return ErrNilRow
	}
	st, err := t.state()
	if err != nil {
		return err
	}
	b, err := st.e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	return st.saveInTx(ctx, b, "", any(row), &Options{})
}

// TxIncrement 事务内单行原子累加（条件须完整覆盖行键）。
func TxIncrement(t *Tx, ctx context.Context, table string, cond any, column string, delta int64) (int64, error) {
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTable(table)
	if err != nil {
		return 0, err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return 0, err
	}
	return st.incrementInTx(ctx, b, vals, column, delta, &Options{})
}

// TxAtomicMax 事务内单行原子取大（条件须完整覆盖行键）。
func TxAtomicMax(t *Tx, ctx context.Context, table string, cond any, column string, value int64) (int64, error) {
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTable(table)
	if err != nil {
		return 0, err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return 0, err
	}
	return st.maxInTx(ctx, b, vals, column, value, &Options{})
}

// TxCompareAndSwap 事务内单行条件更新（expect 全部命中才写 set）。
func TxCompareAndSwap(t *Tx, ctx context.Context, table string, cond any, expect, set map[string]any) (int64, error) {
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTable(table)
	if err != nil {
		return 0, err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return 0, err
	}
	return st.casInTx(ctx, b, vals, expect, set, &Options{})
}

// TxUpdateIn 事务内按单列行键集合逐行 CAS（多行条件更新）；keyColumn 须为完整行键。
func TxUpdateIn(t *Tx, ctx context.Context, table, keyColumn string, ids []any, expect, set map[string]any) (int64, error) {
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTable(table)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := validateColumn(b, keyColumn); err != nil {
		return 0, err
	}
	if len(set) == 0 {
		return 0, ErrBadColumn
	}
	var total int64
	for _, id := range ids {
		vals := []condVal{{Column: keyColumn, Values: []any{id}}}
		n, cerr := st.casInTx(ctx, b, vals, expect, set, &Options{})
		if cerr != nil {
			return 0, cerr
		}
		total += n
	}
	return total, nil
}

// TxDeleteIn 事务内按单列行键集合逐行删除（立墓碑防异步回源复活）。
func TxDeleteIn(t *Tx, ctx context.Context, table, keyColumn string, ids []any) (int64, error) {
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTable(table)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := validateColumn(b, keyColumn); err != nil {
		return 0, err
	}
	return st.deleteWhereInTx(ctx, b, []Range{{Column: keyColumn, Op: "IN", Value: ids}})
}
