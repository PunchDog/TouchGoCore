package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// ==================== 测试替身：无 Redis/MySQL 环境下全 fake ====================

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) Add(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

type fakeEntry struct {
	val string
	exp time.Time
}

// fakeKV 内存版 KV：与 cache 共享注入时钟模拟物理 TTL，记录操作顺序验证 Redis 优先。
// 同时实现 Journaler（内存 ZSET），脏账本路径默认在所有用 fakeKV 的用例中开启。
// 还实现 SeqKV/JournalSeq（内存版 Lua CAS），于是写线的 seq 保序防线在单测里
// 走的是与生产同形的条件写路径，而不是「fake 不支持 → 退回无条件 Set」的空转。
type fakeKV struct {
	mu      sync.Mutex
	m       map[string]fakeEntry
	z       map[string]map[string]float64 // zkey → member → score(seq)
	ops     []string
	clk     *fakeClock
	getErr  error
	setErr  error
	delErr  error
	jErr    error // 账本操作统一注入失败
	getCall int
	setCall int
	delCall int

	// seqGate 仅单测用：条件写（SetSeq/DelSeq/JAddUpsertSeq/JAddDeleteSeq）进入
	// 临界区之前回调，用来确定性制造「先取到小 seq 的一方后落地」的交错。
	// 本机无 C 工具链、-race 不可用，靠注入交错 + 不变量断言代替竞态探测。
	// 生产路径恒为 nil。回调在锁外执行，可安全阻塞。
	seqGate func(key string, seq int64)
}

// 编译期确认：fakeKV 覆盖 cache 对 KV 的全部可选能力，
// 否则新增的 CAS 分支会在单测里静默走不到。
var (
	_ KV         = (*fakeKV)(nil)
	_ Journaler  = (*fakeKV)(nil)
	_ SeqKV      = (*fakeKV)(nil)
	_ JournalSeq = (*fakeKV)(nil)
)

// envelopeSeqOf 取 envelope 顶层 seq（JSON 字段名固定 "s"，见 envelope.go）。
// 解析失败按 0——与 Lua 里「值不可解析当作无 seq 放行」同语义。
func envelopeSeqOf(raw string) int64 {
	var t struct {
		Seq int64 `json:"s"`
	}
	if json.Unmarshal([]byte(raw), &t) != nil {
		return 0
	}
	return t.Seq
}

func newFakeKV(clk *fakeClock) *fakeKV {
	return &fakeKV{m: map[string]fakeEntry{}, z: map[string]map[string]float64{}, clk: clk}
}

func (f *fakeKV) record(op, key string) {
	f.ops = append(f.ops, op+":"+key)
}

func (f *fakeKV) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCall++
	f.record("get", key)
	if f.getErr != nil {
		return "", f.getErr
	}
	e, ok := f.m[key]
	if !ok || f.clk.Now().After(e.exp) || f.clk.Now().Equal(e.exp) {
		return "", ErrNoEntry
	}
	return e.val, nil
}

func (f *fakeKV) MGet(ctx context.Context, keys []string) ([]string, error) {
	out := make([]string, len(keys))
	for i, k := range keys {
		v, err := f.Get(ctx, k)
		if errors.Is(err, ErrNoEntry) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeKV) Set(_ context.Context, key, value string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCall++
	f.record("set", key)
	if f.setErr != nil {
		return f.setErr
	}
	f.m[key] = fakeEntry{val: value, exp: f.clk.Now().Add(ttl)}
	return nil
}

func (f *fakeKV) Del(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCall++
	for _, k := range keys {
		f.record("del", k)
		delete(f.m, k)
	}
	return f.delErr
}

// opsOf 返回操作序列快照（用于断言 set→get 顺序）
func (f *fakeKV) opsOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// rawOf 直读物理存储的原始串（断言用：绕过 TTL 判定与 ops 记录）
func (f *fakeKV) rawOf(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[key]
	if !ok {
		return "", false
	}
	return e.val, true
}

// hardDel 无视 TTL 与 ops 记录直接摘掉物理键：确定性制造「键恰好过期/不存在，
// 于是 CAS 放行了一个本该输的旧写」的交错（生产上等价的形状是物理 TTL 到期）。
func (f *fakeKV) hardDel(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
}

// ---------- fakeKV 的 Journaler 实现（内存 ZSET） ----------

// jPut 账本核心：SET/DEL 值 + 销反向旧账 + 记新账（一键至多一条）。
func (f *fakeKV) jPut(key string, setVal string, ttl time.Duration, zkey, keyStr string, seq int64, del bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jPutLocked(key, setVal, ttl, zkey, keyStr, seq, del, false)
}

// jPutLocked jPut 的持锁本体。monotone=true 时 ZADD 前先做 ZSCORE 守卫
// （与 jAddUpsertSeqScript/jAddDeleteSeqScript 逐字同语义：仅当现有 score 更小才抬）。
func (f *fakeKV) jPutLocked(key string, setVal string, ttl time.Duration, zkey, keyStr string, seq int64, del, monotone bool) error {
	if f.jErr != nil {
		return f.jErr
	}
	opp := jOpDeletePrefix + keyStr // 需销掉的反向账
	newOp := jOpUpsertPrefix
	if del {
		if f.delErr != nil {
			return f.delErr
		}
		f.delCall++
		f.record("del", key)
		delete(f.m, key)
		opp = jOpUpsertPrefix + keyStr
		newOp = jOpDeletePrefix
	} else {
		if f.setErr != nil {
			return f.setErr
		}
		f.setCall++
		f.record("set", key)
		if ttl <= 0 {
			ttl = time.Minute
		}
		f.m[key] = fakeEntry{val: setVal, exp: f.clk.Now().Add(ttl)}
	}
	zs := f.z[zkey]
	if zs == nil {
		zs = map[string]float64{}
		f.z[zkey] = zs
	}
	delete(zs, opp)
	member := newOp + keyStr
	if monotone {
		if sc, ok := zs[member]; ok && sc >= float64(seq) {
			return nil // 账本 score 单调上行：旧 seq 不得把它拉低
		}
	}
	f.record("zadd", member)
	zs[member] = float64(seq)
	return nil
}

func (f *fakeKV) JAddUpsert(_ context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) error {
	return f.jPut(key, value, ttl, zkey, keyStr, seq, false)
}

func (f *fakeKV) JAddDelete(_ context.Context, key, zkey, keyStr string, seq int64) error {
	return f.jPut(key, "", 0, zkey, keyStr, seq, true)
}

// ---------- fakeKV 的 SeqKV / JournalSeq 实现（内存版 Lua CAS） ----------

// seqBlocked 内存版 seqGuardLua：现有 envelope 的 seq 严格大于本次才拒。
// 用「严格大于」而非「大于等于」：同 seq 重写只来自 renewIfAged（用同一个值
// 刷新超龄条目的逻辑新鲜期），必须放行，否则那个功能在单测里会变成永远空转。
// 键不存在或已物理过期 → 放行（与 Lua 的 GET 拿到 false/nil 一致）。
// 调用方须持有 f.mu。
func (f *fakeKV) seqBlocked(key string, seq int64) bool {
	e, ok := f.m[key]
	if !ok {
		return false
	}
	if now := f.clk.Now(); now.After(e.exp) || now.Equal(e.exp) {
		return false
	}
	return envelopeSeqOf(e.val) > seq
}

func (f *fakeKV) SetSeq(_ context.Context, key, value string, ttl time.Duration, seq int64) (bool, error) {
	if f.seqGate != nil {
		f.seqGate(key, seq)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCall++
	f.record("set", key) // 与 Set 同名：对外仍是一次 Redis SET，现有操作序列断言不变
	if f.setErr != nil {
		return false, f.setErr
	}
	if f.seqBlocked(key, seq) {
		return false, nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	f.m[key] = fakeEntry{val: value, exp: f.clk.Now().Add(ttl)}
	return true, nil
}

func (f *fakeKV) DelSeq(_ context.Context, key string, seq int64) (bool, error) {
	if f.seqGate != nil {
		f.seqGate(key, seq)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCall++
	f.record("del", key)
	if f.delErr != nil {
		return false, f.delErr
	}
	if f.seqBlocked(key, seq) {
		return false, nil
	}
	delete(f.m, key)
	return true, nil
}

// jPutSeq 条件记账：CAS 未生效时既不写值也不记账（与 jAddUpsertSeqScript 同语义）。
func (f *fakeKV) jPutSeq(key string, setVal string, ttl time.Duration, zkey, keyStr string, seq int64, del bool) (bool, error) {
	if f.seqGate != nil {
		f.seqGate(key, seq)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jErr != nil {
		return false, f.jErr
	}
	if f.seqBlocked(key, seq) {
		return false, nil
	}
	if err := f.jPutLocked(key, setVal, ttl, zkey, keyStr, seq, del, true); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeKV) JAddUpsertSeq(_ context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) (bool, error) {
	return f.jPutSeq(key, value, ttl, zkey, keyStr, seq, false)
}

func (f *fakeKV) JAddDeleteSeq(_ context.Context, key, zkey, keyStr string, seq int64) (bool, error) {
	return f.jPutSeq(key, "", 0, zkey, keyStr, seq, true)
}

func (f *fakeKV) JClear(_ context.Context, zkey string, members ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jErr != nil {
		return f.jErr
	}
	for _, m := range members {
		f.record("zrem", m)
		if zs := f.z[zkey]; zs != nil {
			delete(zs, m)
		}
	}
	return nil
}

func (f *fakeKV) JClearIfSeq(_ context.Context, zkey, member string, seq int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jErr != nil {
		return f.jErr
	}
	zs := f.z[zkey]
	if zs == nil {
		return nil
	}
	sc, ok := zs[member]
	if !ok || int64(sc) != seq {
		return nil // score 已变：新 Write 拥有该键，旧 flush 不得销账
	}
	f.record("zrem", member)
	delete(zs, member)
	return nil
}

func (f *fakeKV) JScan(_ context.Context, zkey string) ([]JournalEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jErr != nil {
		return nil, f.jErr
	}
	var out []JournalEntry
	for m, sc := range f.z[zkey] {
		je, ok := jParseMember(m)
		if !ok {
			continue
		}
		je.Seq = int64(sc)
		out = append(out, je)
	}
	sortJournalEntries(out)
	return out, nil
}

// journalOf 直接读账本成员（断言用）
func (f *fakeKV) journalOf(zkey string) map[string]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]float64{}
	for m, s := range f.z[zkey] {
		out[m] = s
	}
	return out
}

type testVal struct {
	N int `json:"n"`
}

// mapLoader 从内存表回源；notFound 集合内返回 ErrNotFound；failN>0 时前 N 次报错
type mapLoader struct {
	mu       sync.Mutex
	data     map[string]*testVal
	notFound map[string]bool
	calls    int
	failN    int
	err      error
	block    chan struct{} // 非 nil 时 Load 阻塞直到关闭
}

func (l *mapLoader) Load(ctx context.Context, key string) (*testVal, error) {
	if l.block != nil {
		select {
		case <-l.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.failN > 0 {
		l.failN--
		err := l.err
		l.mu.Unlock()
		defer l.mu.Lock()
		if err == nil {
			err = errors.New("source down")
		}
		return nil, err
	}
	if l.notFound[key] {
		return nil, ErrNotFound
	}
	v, ok := l.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

func (l *mapLoader) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// recSaver 记录落库；每 N 次调用可注入失败
type recSaver struct {
	mu      sync.Mutex
	saved   map[string]*testVal
	saves   int
	deletes int
	failFn  func(n int) error
	saveErr error
}

func newRecSaver() *recSaver {
	return &recSaver{saved: map[string]*testVal{}}
}

func (s *recSaver) Save(_ context.Context, key string, val *testVal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.failFn != nil {
		if err := s.failFn(s.saves); err != nil {
			return err
		}
	}
	s.saved[key] = val
	return nil
}

func (s *recSaver) Delete(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	return nil
}

func (s *recSaver) counts() (saves, deletes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves, s.deletes
}

func (s *recSaver) get(key string) *testVal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[key]
}

// recBatchSaver 实现 BatchSaver，记录每批大小
type recBatchSaver struct {
	recSaver
	batchSizes []int
}

func newRecBatchSaver() *recBatchSaver {
	return &recBatchSaver{recSaver: *newRecSaver()}
}

func (s *recBatchSaver) SaveBatch(_ context.Context, items []Item[string]) error {
	s.mu.Lock()
	s.batchSizes = append(s.batchSizes, len(items))
	s.mu.Unlock()
	for _, it := range items {
		switch it.Op {
		case OpUpsert:
			v, _ := it.Value.(*testVal)
			if err := s.Save(context.Background(), it.Key, v); err != nil {
				return err
			}
		case OpDelete:
			if err := s.Delete(context.Background(), it.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ==================== 构造助手 ====================

func testConfig() Config {
	c := DefaultConfig()
	c.TTL = time.Second
	c.LogicalPct = 50
	c.JitterPct = 0
	c.ReadTimeout = 200 * time.Millisecond
	c.WriteTimeout = 200 * time.Millisecond
	c.FailBackoff = 100 * time.Millisecond
	c.NegativeTTL = 500 * time.Millisecond
	return c
}

type harness struct {
	kv  *fakeKV
	clk *fakeClock
	c   *Cache[string, testVal]
	ld  *mapLoader
	sn  Saver[string, testVal]
}

func newHarness(t *testing.T, opts ...Option[string, testVal]) *harness {
	t.Helper()
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ld := &mapLoader{data: map[string]*testVal{}, notFound: map[string]bool{}}
	base := []Option[string, testVal]{
		WithConfig[string, testVal](testConfig()),
		WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld),
	}
	c, err := New("t", kv, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{kv: kv, clk: clk, c: c, ld: ld}
}
