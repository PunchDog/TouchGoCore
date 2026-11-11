package dbcache

// ============================================================================
// 事务核心：Redis 权威的延迟原子批（Model A）。
//
// 事务内的写不立即落 Redis，而是缓冲成 writeOp 序列，并在一份私有 memState
// overlay 上即时复用 applyOne 求值——读己写与提交重放共用同一套语义，二者天然
// 同构。读路径（matchingRows）把 overlay 叠加在懒加载的 Redis 真值上：事务内
// 能看到自己未提交的写，事务外看不到。
//
// 行锁（TxRowLocks，默认开）在首次触碰一行时取得，覆盖到提交/回滚；提交经
// txScript 一次原子重放全部 op（脚本复核锁归属后逐 op 应用并释放锁），回滚直接
// 丢弃 overlay，Redis 分毫不动。MySQL 只是提交后由脏账异步物化的 sink。
// ============================================================================

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"touchgocore/metrics"
)

var txSeq atomic.Int64

// txState 是一段框架内部事务的私有状态：overlay + 缓冲写 + 行锁账本。
// 非并发安全：一个事务由单一 goroutine 串行使用。
type txState struct {
	e      *Engine
	ctx    context.Context
	txid   string
	writes []txWrite
	mem    *memState

	loaded     map[string]bool // rowKey → 已从 Redis 懒加载进 overlay
	uqLoaded   map[string]bool // uqKey → 已加载唯一守卫
	tombLoaded map[string]bool // tombKey → 已加载墓碑存活态
	ksLoaded   map[string]bool // keySet → 已加载存活行键集

	locks   []string
	lockSet map[string]bool

	written map[string]bool // table\x00key → 事务写后该行存活（overlay 视角）
	deleted map[string]bool // table\x00key → 事务内已删除
	done    bool
}

// newTxState 构造事务状态；txid 全局唯一，用于行锁归属与提交复核。
func newTxState(e *Engine, ctx context.Context) *txState {
	if ctx == nil {
		ctx = context.Background()
	}
	n := txSeq.Add(1)
	return &txState{
		e:          e,
		ctx:        ctx,
		txid:       fmt.Sprintf("tx-%d-%d", time.Now().UnixNano(), n),
		mem:        newMemState(),
		loaded:     make(map[string]bool),
		uqLoaded:   make(map[string]bool),
		tombLoaded: make(map[string]bool),
		ksLoaded:   make(map[string]bool),
		lockSet:    make(map[string]bool),
		written:    make(map[string]bool),
		deleted:    make(map[string]bool),
	}
}

// RunTx 在框架内部开启事务并执行 fn；fn 内必须通过 ctx 调四个数据接口。
// fn 返回错误或 panic 都回滚（丢弃 overlay、释放行锁），成功则原子提交。
func RunTx(ctx context.Context, fn func(context.Context) error) error {
	e := Default()
	if e == nil {
		return ErrNotInitialized
	}
	st := newTxState(e, ctx)
	tctx := context.WithValue(st.ctx, txKey{}, st)
	defer func() {
		if p := recover(); p != nil {
			_ = st.rollback(context.Background())
			panic(p)
		}
	}()
	if err := fn(tctx); err != nil {
		_ = st.rollback(context.Background())
		return err
	}
	return st.commit(tctx)
}

// due 是脏账到期时间：提交后由定时 flush 异步物化到 sink。
func (s *txState) due(b *binding) int64 {
	return time.Now().Add(b.interval).UnixMilli()
}

// acquireLock 取得一行锁（TxRowLocks 关时 noop）；等待超时返回 ErrTxConflict。
func (s *txState) acquireLock(ctx context.Context, table, lockKey string) error {
	if !s.e.cfg.TxRowLocks || s.lockSet[lockKey] {
		return nil
	}
	ok, err := s.e.store.TryLock(ctx, lockKey, s.txid, int64(s.e.cfg.TxLockTTLMS), int64(s.e.cfg.TxLockWaitMS))
	if err != nil {
		return err
	}
	if !ok {
		metrics.DBCache.IncTxConflict(table)
		return fmt.Errorf("%w: lock wait timeout %s", ErrTxConflict, lockKey)
	}
	s.lockSet[lockKey] = true
	s.locks = append(s.locks, lockKey)
	return nil
}

// releaseLocks 比较删除释放本事务持有的全部行锁（不误删他人锁）。
func (s *txState) releaseLocks(ctx context.Context) {
	if !s.e.cfg.TxRowLocks {
		return
	}
	for _, lk := range s.locks {
		_ = s.e.store.Unlock(ctx, lk, s.txid)
	}
	s.locks = nil
	s.lockSet = make(map[string]bool)
}

// loadRow 在持锁后把行 hash / 唯一守卫 / 墓碑懒加载进 overlay（只加载一次）。
func (s *txState) loadRow(ctx context.Context, k writeKeys) error {
	if !s.loaded[k.row] {
		h, err := s.e.store.HGetAll(ctx, k.row)
		if err != nil {
			return err
		}
		if len(h) > 0 {
			s.mem.hashes[k.row] = h
		}
		s.loaded[k.row] = true
	}
	if !s.uqLoaded[k.uq] {
		uq, err := s.e.store.HGetAll(ctx, k.uq)
		if err != nil {
			return err
		}
		s.mem.hashes[k.uq] = uq
		s.uqLoaded[k.uq] = true
	}
	if !s.tombLoaded[k.tomb] {
		alive, err := s.e.store.TombAlive(ctx, k.tomb)
		if err != nil {
			return err
		}
		if alive {
			s.mem.strs[k.tomb] = "1"
		}
		s.tombLoaded[k.tomb] = true
	}
	return nil
}

// loadKeySet 懒加载表的存活行键集（受 MaxScanRows 预算约束，超限 fail-closed）。
func (s *txState) loadKeySet(ctx context.Context, b *binding) error {
	if s.ksLoaded[b.keys.keySet] {
		return nil
	}
	keys, err := s.e.store.KeySetScan(ctx, b.keys.keySet, s.e.cfg.MaxScanRows)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExceeded) {
			metrics.DBCache.IncScanBudget(b.table)
		}
		return err
	}
	set := s.mem.set(b.keys.keySet)
	for _, k := range keys {
		set[k] = struct{}{}
	}
	s.ksLoaded[b.keys.keySet] = true
	return nil
}

// apply 缓冲一条写：取行锁 → 懒加载 → overlay 上即时 applyOne（读己写 + 提前
// 检出冲突）→ 追加进提交重放序列。校验失败零副作用（不缓冲、不落 Redis）。
func (s *txState) apply(ctx context.Context, b *binding, key string, op *writeOp) (*writeResult, error) {
	if s.done {
		return nil, fmt.Errorf("%w: transaction already finished", ErrTxConflict)
	}
	k := b.writeKeysFor(key)
	k.lock = b.keys.lockPrefix + key
	if err := s.acquireLock(ctx, b.table, k.lock); err != nil {
		return nil, err
	}
	if err := s.loadRow(ctx, k); err != nil {
		return nil, err
	}
	op.Key = key
	op.Idx = b.idxDescs()
	op.UQ = b.uniques
	res := applyOne(s.mem, k, op, s.mem.hashes[k.row], time.Now().UnixMilli())
	if res.Err != "" {
		return nil, txWriteErr(b, key, res.Err)
	}
	s.writes = append(s.writes, txWrite{b: b, key: key, op: op})
	mk := b.table + "\x00" + key
	if op.Op == "delete" {
		s.deleted[mk] = true
		delete(s.written, mk)
	} else if res.Affected > 0 {
		s.written[mk] = true
		delete(s.deleted, mk)
	}
	return res, nil
}

// commit 经 txScript 一次原子重放全部缓冲写；无论成败都释放行锁。
func (s *txState) commit(ctx context.Context) error {
	if s.done {
		return nil
	}
	s.done = true
	defer s.releaseLocks(ctx)
	if len(s.writes) == 0 {
		return nil
	}
	txid := ""
	if s.e.cfg.TxRowLocks {
		txid = s.txid
	}
	_, err := s.e.applyTx(ctx, txid, s.writes)
	if err != nil && errors.Is(err, ErrTxConflict) {
		// 提交重放冲突（锁丢失）：按涉及的表各记一次中止。
		seen := make(map[string]bool)
		for _, w := range s.writes {
			if !seen[w.b.table] {
				seen[w.b.table] = true
				metrics.DBCache.IncTxConflict(w.b.table)
			}
		}
	}
	return err
}

// rollback 丢弃 overlay 与缓冲写、释放行锁；Redis 不受任何影响。
func (s *txState) rollback(ctx context.Context) error {
	if s.done {
		return nil
	}
	s.done = true
	s.releaseLocks(ctx)
	s.writes = nil
	s.mem = newMemState()
	return nil
}

// txWriteErr 把 overlay 检出的 op 错误映射成公开错误。
func txWriteErr(b *binding, key, errStr string) error {
	switch errStr {
	case "duplicate":
		return fmt.Errorf("%w: %s/%s", ErrDuplicateKey, b.table, key)
	case "lock_lost":
		return fmt.Errorf("%w: %s/%s lock lost", ErrTxConflict, b.table, key)
	default:
		return fmt.Errorf("dbcache: tx write %s/%s: %s", b.table, key, errStr)
	}
}

// matchingRows 返回事务视角下命中条件的行 hash：完整行键走点查，否则扫键集
// 并叠加事务写（written − deleted），逐行用 evalConds 过滤。forUpdate 时对点查
// 行取锁。读到的行优先取 overlay（含未提交写），缺失才懒加载 Redis 真值。
func (s *txState) matchingRows(ctx context.Context, b *binding, vals []condVal, o *Options) ([]map[string]string, error) {
	if o == nil {
		o = &Options{}
	}
	conds, err := condValsToDescs(b, vals)
	if err != nil {
		return nil, err
	}
	// 事务内读同样要消费结构化条件（Equal/Range/IN 等）：否则「写后在事务内按
	// 条件回读」会把刚写入的行过滤丢掉。
	for _, r := range o.Ranges {
		if r.Table != "" {
			return nil, ErrBadColumn
		}
		c, cerr := condDescFromRange(b, r)
		if cerr != nil {
			return nil, cerr
		}
		conds = append(conds, c)
	}
	anyOf, err := groupsToDescs(b, o.AnyOfGroups)
	if err != nil {
		return nil, err
	}
	if condIsExactKey(b.meta, vals) {
		key, kerr := keyFromCond(b.meta, vals)
		if kerr != nil {
			return nil, kerr
		}
		k := b.writeKeysFor(key)
		k.lock = b.keys.lockPrefix + key
		if o.ForUpdate {
			if err := s.acquireLock(ctx, b.table, k.lock); err != nil {
				return nil, err
			}
		}
		if err := s.loadRow(ctx, k); err != nil {
			return nil, err
		}
		h := s.mem.hashes[k.row]
		if len(h) == 0 || !evalConds(h, conds, anyOf) {
			return nil, nil
		}
		return specPage([]map[string]string{h}, o), nil
	}
	if err := s.loadKeySet(ctx, b); err != nil {
		return nil, err
	}
	keys := make(map[string]bool)
	for key := range s.mem.sets[b.keys.keySet] {
		keys[key] = true
	}
	prefix := b.table + "\x00"
	for mk := range s.written {
		if strings.HasPrefix(mk, prefix) && !s.deleted[mk] {
			keys[mk[len(prefix):]] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	out := make([]map[string]string, 0, len(sorted))
	for _, key := range sorted {
		if s.deleted[b.table+"\x00"+key] {
			continue
		}
		k := b.writeKeysFor(key)
		k.lock = b.keys.lockPrefix + key
		if err := s.loadRow(ctx, k); err != nil {
			return nil, err
		}
		h := s.mem.hashes[k.row]
		if len(h) == 0 || !evalConds(h, conds, anyOf) {
			continue
		}
		out = append(out, h)
	}
	sortSpecRows(out, o.Orders)
	return specPage(out, o), nil
}

// condValsToDescs 把等值/IN 条件翻译成写路径 condDesc（对 live 行求值用）。
func condValsToDescs(b *binding, vals []condVal) ([]condDesc, error) {
	out := make([]condDesc, 0, len(vals))
	for _, v := range vals {
		if err := validateColumn(b, v.Column); err != nil {
			return nil, err
		}
		if len(v.Values) == 0 {
			continue
		}
		typ := colTypeFor(b.meta.Fields[b.meta.ByColumn[v.Column]].Type)
		if len(v.Values) == 1 {
			s, err := encodeValue(reflect.ValueOf(v.Values[0]))
			if err != nil {
				return nil, err
			}
			out = append(out, condDesc{Col: v.Column, Op: "=", Val: s, Type: typ})
			continue
		}
		c := condDesc{Col: v.Column, Op: "IN", Type: typ}
		for _, val := range v.Values {
			s, err := encodeValue(reflect.ValueOf(val))
			if err != nil {
				return nil, err
			}
			c.Vals = append(c.Vals, s)
		}
		out = append(out, c)
	}
	return out, nil
}

// rangesToDescs 把结构化条件 / OR 分组翻译成 condDesc（增量/CAS 的附加校验）。
func rangesToDescs(b *binding, ranges []Range) ([]condDesc, error) {
	out := make([]condDesc, 0, len(ranges))
	for _, r := range ranges {
		c, err := condDescFromRange(b, r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func groupsToDescs(b *binding, groups [][]Range) ([][]condDesc, error) {
	var out [][]condDesc
	for _, group := range groups {
		g, err := rangesToDescs(b, group)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// condVals 归一化原子写的行定位条件：Cond 结构体优先，其次行键字符串。
func (s *txState) condVals(b *binding, key string, o *Options) ([]condVal, error) {
	if o.Cond != nil {
		return parseCond(b.meta, b.kind, o.Cond)
	}
	if key != "" {
		return keyValsFromString(b, key)
	}
	// o.Cond 缺席时从结构化条件（db.Condition）的键列等值推导行键；
	// 推不出完整行键由 condIsExactKey 拒为 ErrPartialKey（绝不无 WHERE 全表写）。
	return valsFromConditions(b.meta, o.Conditions), nil
}

// keyValsFromString 把行键字符串还原成条件（单列行键）。
func keyValsFromString(b *binding, key string) ([]condVal, error) {
	if len(b.meta.KeyCols) != 1 {
		return nil, ErrPartialKey
	}
	return []condVal{{Column: b.meta.KeyCols[0], Values: []any{key}}}, nil
}

// saveInTx 事务内整行写 / 原子累加 / CAS / 取号，结果即时进 overlay。
func (s *txState) saveInTx(ctx context.Context, b *binding, key string, row any, o *Options) error {
	switch {
	case o.Sequence != nil:
		return s.sequenceInTx(ctx, b, row, o)
	case o.Increment != "":
		vals, err := s.condVals(b, key, o)
		if err != nil {
			return err
		}
		n, err := s.incrementInTx(ctx, b, vals, o.Increment, o.Delta, o)
		if o.Affected != nil {
			*o.Affected = n
		}
		return err
	case o.AtomicMax != "":
		vals, err := s.condVals(b, key, o)
		if err != nil {
			return err
		}
		n, err := s.maxInTx(ctx, b, vals, o.AtomicMax, o.MaxValue, o)
		if o.Affected != nil {
			*o.Affected = n
		}
		return err
	case o.CASSet != nil:
		vals, err := s.condVals(b, key, o)
		if err != nil {
			return err
		}
		n, err := s.casInTx(ctx, b, vals, o.CASExpect, o.CASSet, o)
		if o.Affected != nil {
			*o.Affected = n
		}
		return err
	default:
		if row == nil {
			return ErrNilRow
		}
		if key == "" {
			var err error
			key, err = b.keyFromRow(row)
			if err != nil {
				return err
			}
		}
		fields, err := b.encodeFields(row)
		if err != nil {
			return err
		}
		secret, err := s.e.encodeSecrets(b, o.SecretSet)
		if err != nil {
			return err
		}
		op := &writeOp{
			Op:      "save",
			Fields:  fieldsToMap(fields),
			Secret:  secret,
			Due:     s.due(b),
			TombTTL: s.e.tombTTL().Milliseconds(),
		}
		res, err := s.apply(ctx, b, key, op)
		if err != nil {
			return err
		}
		if o.Affected != nil {
			*o.Affected = res.Affected
		}
		return nil
	}
}

// incrementInTx 事务内单行原子累加（条件必须完整覆盖行键）。
func (s *txState) incrementInTx(ctx context.Context, b *binding, vals []condVal, column string, delta int64, o *Options) (int64, error) {
	if err := validateColumn(b, column); err != nil {
		return 0, err
	}
	if !condIsExactKey(b.meta, vals) {
		return 0, ErrPartialKey
	}
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return 0, err
	}
	conds, err := rangesToDescs(b, o.Conditions)
	if err != nil {
		return 0, err
	}
	anyOf, err := groupsToDescs(b, o.AnyOfGroups)
	if err != nil {
		return 0, err
	}
	op := &writeOp{
		Op:      "increment",
		Column:  column,
		Delta:   delta,
		Conds:   conds,
		AnyOf:   anyOf,
		Due:     s.due(b),
		TombTTL: s.e.tombTTL().Milliseconds(),
	}
	res, err := s.apply(ctx, b, key, op)
	if err != nil {
		return 0, err
	}
	return res.Affected, nil
}

// maxInTx 事务内单行原子取大（条件必须完整覆盖行键）。
func (s *txState) maxInTx(ctx context.Context, b *binding, vals []condVal, column string, value int64, o *Options) (int64, error) {
	if err := validateColumn(b, column); err != nil {
		return 0, err
	}
	if !condIsExactKey(b.meta, vals) {
		return 0, ErrPartialKey
	}
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return 0, err
	}
	conds, err := rangesToDescs(b, o.Conditions)
	if err != nil {
		return 0, err
	}
	anyOf, err := groupsToDescs(b, o.AnyOfGroups)
	if err != nil {
		return 0, err
	}
	op := &writeOp{
		Op:      "max",
		Column:  column,
		Max:     strconv.FormatInt(value, 10),
		Conds:   conds,
		AnyOf:   anyOf,
		Due:     s.due(b),
		TombTTL: s.e.tombTTL().Milliseconds(),
	}
	res, err := s.apply(ctx, b, key, op)
	if err != nil {
		return 0, err
	}
	return res.Affected, nil
}

// casInTx 事务内单行条件更新（expect 全部命中才写 set）。
func (s *txState) casInTx(ctx context.Context, b *binding, vals []condVal, expect, set map[string]any, o *Options) (int64, error) {
	if len(set) == 0 {
		return 0, ErrBadColumn
	}
	for col := range set {
		if err := validateColumn(b, col); err != nil {
			return 0, err
		}
	}
	if !condIsExactKey(b.meta, vals) {
		return 0, ErrPartialKey
	}
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return 0, err
	}
	setMap, err := encodeFieldMap(set)
	if err != nil {
		return 0, err
	}
	expectMap := make(map[string]expectDesc, len(expect))
	for col, v := range expect {
		d, derr := expectDescFrom(b, col, v)
		if derr != nil {
			return 0, derr
		}
		expectMap[col] = d
	}
	conds, err := rangesToDescs(b, o.Conditions)
	if err != nil {
		return 0, err
	}
	anyOf, err := groupsToDescs(b, o.AnyOfGroups)
	if err != nil {
		return 0, err
	}
	op := &writeOp{
		Op:      "cas",
		Set:     setMap,
		Expect:  expectMap,
		Conds:   conds,
		AnyOf:   anyOf,
		Due:     s.due(b),
		TombTTL: s.e.tombTTL().Milliseconds(),
	}
	res, err := s.apply(ctx, b, key, op)
	if err != nil {
		return 0, err
	}
	return res.Affected, nil
}

// sequenceInTx 事务内原子取号：新行插入取 0，已有行把 Column 加 Delta 并回填 Out。
func (s *txState) sequenceInTx(ctx context.Context, b *binding, row any, o *Options) error {
	if row == nil {
		return ErrNilRow
	}
	col := o.Sequence.Column
	if err := validateColumn(b, col); err != nil {
		return err
	}
	key, err := b.keyFromRow(row)
	if err != nil {
		return err
	}
	k := b.writeKeysFor(key)
	k.lock = b.keys.lockPrefix + key
	if err := s.acquireLock(ctx, b.table, k.lock); err != nil {
		return err
	}
	if err := s.loadRow(ctx, k); err != nil {
		return err
	}
	var id int64
	if len(s.mem.hashes[k.row]) > 0 {
		op := &writeOp{Op: "increment", Column: col, Delta: o.Sequence.Delta, Due: s.due(b), TombTTL: s.e.tombTTL().Milliseconds()}
		res, aerr := s.apply(ctx, b, key, op)
		if aerr != nil {
			return aerr
		}
		id, _ = strconv.ParseInt(res.Row[col], 10, 64)
	} else {
		fields, ferr := b.encodeFields(row)
		if ferr != nil {
			return ferr
		}
		op := &writeOp{Op: "insert", Fields: fieldsToMap(fields), Due: s.due(b), TombTTL: s.e.tombTTL().Milliseconds()}
		if _, aerr := s.apply(ctx, b, key, op); aerr != nil {
			return aerr
		}
	}
	if o.Sequence.Out != nil {
		*o.Sequence.Out = id
	}
	if o.Affected != nil {
		*o.Affected = 1
	}
	return nil
}

// insertInTx 事务内插入（冲突 affected=0）；自增列零值即时 SeqAlloc 取号回填。
func (s *txState) insertInTx(ctx context.Context, b *binding, row any, o *Options) (int64, error) {
	if row == nil {
		return 0, ErrNilRow
	}
	if b.autoInc != "" {
		rv := reflect.ValueOf(row).Elem()
		fv := rv.Field(b.meta.Fields[b.meta.ByColumn[b.autoInc]].Index)
		if isZeroValue(fv) {
			n, aerr := s.e.store.SeqAlloc(ctx, b.keys.seq, 1)
			if aerr != nil {
				return 0, aerr
			}
			switch fv.Kind() {
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				fv.SetUint(uint64(n))
			default:
				fv.SetInt(n)
			}
		}
	}
	key, err := b.keyFromRow(row)
	if err != nil {
		return 0, err
	}
	fields, err := b.encodeFields(row)
	if err != nil {
		return 0, err
	}
	var secret map[string]string
	if o != nil {
		secret, err = s.e.encodeSecrets(b, o.SecretSet)
		if err != nil {
			return 0, err
		}
	}
	op := &writeOp{
		Op:      "insert",
		Fields:  fieldsToMap(fields),
		Secret:  secret,
		Due:     s.due(b),
		TombTTL: s.e.tombTTL().Milliseconds(),
	}
	res, err := s.apply(ctx, b, key, op)
	if err != nil {
		return 0, err
	}
	return res.Affected, nil
}

// deleteInTx 事务内删除一行（条件必须完整覆盖行键）：立墓碑防异步回源复活。
func (s *txState) deleteInTx(ctx context.Context, b *binding, vals []condVal) error {
	if !condIsExactKey(b.meta, vals) {
		return fmt.Errorf("%w: %s", ErrPartialKey, b.table)
	}
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return err
	}
	op := &writeOp{Op: "delete", Sink: b.sinkDelete != nil, Due: s.due(b), TombTTL: s.e.tombTTL().Milliseconds()}
	_, err = s.apply(ctx, b, key, op)
	return err
}

// deleteWhereInTx 事务内按结构化条件批量删除：把行键列上的 = / IN 展开成行键
// 逐行删除。复合键要求其余键列均为等值；不支持非键列条件（拒绝全表删）。
func (s *txState) deleteWhereInTx(ctx context.Context, b *binding, ranges []Range) (int64, error) {
	keys, err := deleteKeysFromRanges(b, ranges)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, key := range keys {
		op := &writeOp{Op: "delete", Sink: b.sinkDelete != nil, Due: s.due(b), TombTTL: s.e.tombTTL().Milliseconds()}
		res, err := s.apply(ctx, b, key, op)
		if err != nil {
			return 0, err
		}
		total += res.Affected
	}
	return total, nil
}

// deleteKeysFromRanges 从行键列上的 = / IN 条件展开行键列表。
func deleteKeysFromRanges(b *binding, ranges []Range) ([]string, error) {
	base := make([]condVal, 0, len(ranges))
	var inCol string
	var inVals []any
	for _, r := range ranges {
		if err := validateColumn(b, r.Column); err != nil {
			return nil, err
		}
		switch r.Op {
		case "=":
			base = append(base, condVal{Column: r.Column, Values: []any{r.Value}})
		case "IN":
			list, ok := r.Value.([]any)
			if !ok || len(list) == 0 {
				return nil, ErrBadColumn
			}
			if inCol != "" {
				return nil, ErrPartialKey
			}
			inCol, inVals = r.Column, list
		default:
			return nil, ErrBadColumn
		}
	}
	if inCol == "" {
		if !condIsExactKey(b.meta, base) {
			return nil, fmt.Errorf("%w: %s", ErrPartialKey, b.table)
		}
		key, err := keyFromCond(b.meta, base)
		if err != nil {
			return nil, err
		}
		return []string{key}, nil
	}
	keys := make([]string, 0, len(inVals))
	for _, v := range inVals {
		vals := append(append([]condVal(nil), base...), condVal{Column: inCol, Values: []any{v}})
		if !condIsExactKey(b.meta, vals) {
			return nil, fmt.Errorf("%w: %s", ErrPartialKey, b.table)
		}
		key, err := keyFromCond(b.meta, vals)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}
