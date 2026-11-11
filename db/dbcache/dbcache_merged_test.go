package dbcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"touchgocore/config"
	"touchgocore/localtimer"
)

type codecModel struct {
	ID     int64          `cache:"id"`
	Int8   int8           `cache:"int8"`
	Uint32 uint32         `cache:"uint32"`
	Float  float64        `cache:"float"`
	Bool   bool           `cache:"bool"`
	Text   string         `cache:"text"`
	Bytes  []byte         `cache:"bytes"`
	At     time.Time      `cache:"at"`
	Dur    time.Duration  `cache:"dur"`
	Nested map[string]int `cache:"nested"`
	List   []string       `cache:"list"`
	Ptr    *string        `cache:"ptr"`
}

func testCodecMeta(t *testing.T) *tableMeta {
	t.Helper()
	m, err := buildMeta(typeOf[codecModel](), []string{"id"}, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	return m
}

func TestCodecRoundTrip(t *testing.T) {
	m := testCodecMeta(t)
	ptr := "hello"
	want := codecModel{
		ID:     1,
		Int8:   -8,
		Uint32: 42,
		Float:  3.25,
		Bool:   true,
		Text:   "文本",
		Bytes:  []byte{0, 1, 2, 255},
		At:     time.Date(2026, 11, 7, 6, 0, 0, 123456789, time.UTC),
		Dur:    3 * time.Second,
		Nested: map[string]int{"a": 1},
		List:   []string{"x", "y"},
		Ptr:    &ptr,
	}
	fields, err := encodeRow(m, reflect.ValueOf(want))
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	h := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		h[fields[i]] = fields[i+1]
	}
	var got codecModel
	if err := decodeRow(m, reflect.ValueOf(&got).Elem(), h); err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if !got.At.Equal(want.At) {
		t.Fatalf("时间=%v，期望 %v", got.At, want.At)
	}
	got.At = want.At
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip 不一致:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestCodecNilPointerAndMissingColumn(t *testing.T) {
	m := testCodecMeta(t)
	fields, err := encodeRow(m, reflect.ValueOf(codecModel{ID: 1}))
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	foundEmptyPtr := false
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "ptr" {
			if fields[i+1] != "" {
				t.Fatalf("nil 指针应编码为空串，实际 %q", fields[i+1])
			}
			foundEmptyPtr = true
		}
	}
	if !foundEmptyPtr {
		t.Fatal("nil 指针字段必须写入空串，避免旧值残留")
	}
	h := map[string]string{"id": "1"}
	var got codecModel
	if err := decodeRow(m, reflect.ValueOf(&got).Elem(), h); err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if got.Ptr != nil {
		t.Fatalf("缺失列应保持 nil，实际 %v", got.Ptr)
	}
}

func TestCodecInvalidValue(t *testing.T) {
	m := testCodecMeta(t)
	var got codecModel
	err := decodeRow(m, reflect.ValueOf(&got).Elem(), map[string]string{"id": "1", "int8": "abc"})
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("期望 ErrDecode，实际 %v", err)
	}
}

func TestDecodeTimeFallbackFormats(t *testing.T) {
	tm, err := decodeValue("2026-11-07 06:00:00", timeType)
	if err != nil {
		t.Fatalf("decodeValue: %v", err)
	}
	if tm.Interface().(time.Time).Year() != 2026 {
		t.Fatalf("解析结果=%v", tm.Interface())
	}
}

// TestDeleteRemovesCacheAndRow 删除在 Redis 侧原子完成（行/脏账即清、入 dirtyDel），
// sink 删除异步物化：flush 后 delCalls=1；再查必须回源。
func TestDeleteRemovesCacheAndRow(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "cache"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	rowKey := b.keys.rowPrefix + "7"
	if len(fs.hash(rowKey)) == 0 {
		t.Fatal("保存后行 hash 应为非空")
	}

	src.one = nil
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if src.delCalls != 0 {
		t.Fatalf("Redis 权威删除不应同步调 sink，实际 delCalls=%d", src.delCalls)
	}
	if h := fs.hash(rowKey); len(h) != 0 {
		t.Fatalf("删除后行 hash 应为空，实际 %v", h)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("删除后脏账应为空，实际 %d", n)
	}
	if n := fs.dirtyCount(b.keys.dirtyDel); n != 1 {
		t.Fatalf("删除应入 dirtyDel 异步账，实际 %d", n)
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if src.delCalls != 1 {
		t.Fatalf("flush 后数据库删除调用次数=%d", src.delCalls)
	}

	var out testUser
	err := e.query[testUser](ctx, "test_user", &idCond{ID: ptrInt(7)}, &out)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("删除后查询应回源并返回 ErrNoRows，实际 %v", err)
	}
}

// TestDeleteDropsPendingDirty 删除前的待落库脏账不得在之后的 flush 里写回。
func TestDeleteDropsPendingDirty(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "pending"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 1 {
		t.Fatalf("应有一条待落库脏账，实际 %d", n)
	}
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("删除后 flush 不应写回任何行，实际 %d", len(rows))
	}
}

// TestDeleteTombBlocksUnclaimedDirty 墓碑生效期间，残留脏账也不得写回。
func TestDeleteTombBlocksUnclaimedDirty(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "pending"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 模拟删除流程已立墓碑但尚未走完清理：脏账与行 hash 都还在。
	if err := fs.SetTomb(ctx, b.keys.tombPrefix+"7", 0); err != nil {
		t.Fatalf("SetTomb: %v", err)
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("墓碑生效时不应回写，实际 %d", len(rows))
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("墓碑命中后脏账应销掉，实际 %d", n)
	}
}

// TestDeleteThenSaveRestoresRow 删除后重建同一主键必须能正常落库。
func TestDeleteThenSaveRestoresRow(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("删除后应留下墓碑")
	}
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "again"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	if fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("重建行后墓碑应被清除")
	}
	rows := sink.snapshot()
	if len(rows) != 1 || rows[0].Name != "again" {
		t.Fatalf("重建行应落库，实际 %v", rows)
	}
}

// TestDeletePartialKey 非键条件必须被拒绝，避免误删多行。
func TestDeletePartialKey(t *testing.T) {
	e, _, _, _, b := newTestEngine(t)
	err := e.remove(context.Background(), b, &nameCond{Name: ptrStr("db")})
	if !errors.Is(err, ErrPartialKey) {
		t.Fatalf("非键条件应返回 ErrPartialKey，实际 %v", err)
	}
}

// TestDeleteSinkFailureRetries 契约变更（Redis 权威）：删除在 Redis 侧即成功，
// sink 删除失败不清缓存回滚、而是退避重试不丢；恢复后 flush 必须删成。
func TestDeleteSinkFailureRetries(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "keep"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	src.delErr = errSinkDown

	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("Redis 权威删除应即成功，实际 %v", err)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); len(h) != 0 {
		t.Fatal("删除后行 hash 应为空（Redis 即真源）")
	}
	if !fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("删除后应留墓碑挡在途回写")
	}
	if err := e.flushAll(ctx, b); !errors.Is(err, errSinkDown) {
		t.Fatalf("sink 删除失败本轮应上抛，实际 %v", err)
	}
	if n := fs.retryCount(b.keys.retry, "7"); n != 1 {
		t.Fatalf("sink 删除失败应记重试，实际 %d", n)
	}
	if n := fs.dirtyCount(b.keys.dirtyDel); n != 1 {
		t.Fatalf("sink 删除失败应重挂 dirtyDel，实际 %d", n)
	}
	src.delErr = nil
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("恢复后 flushAll: %v", err)
	}
	if src.delCalls != 2 || len(src.deleted) != 1 {
		t.Fatalf("应为 1 次失败尝试 + 1 次成功删除，实际 delCalls=%d deleted=%d", src.delCalls, len(src.deleted))
	}
	if n := fs.dirtyCount(b.keys.dirtyDel); n != 0 {
		t.Fatalf("删成后 dirtyDel 应清空，实际 %d", n)
	}
}

// testUser 是测试模型：覆盖 SQL 列名、主键回退、指针字段与非列字段。
type testUser struct {
	ID      int64     `gorm:"column:id;primaryKey" json:"id"`
	Name    string    `json:"name"`
	Age     int       `gorm:"column:age"`
	Active  bool      `json:"active"`
	Score   *int64    `json:"score"`
	Created time.Time `json:"created"`
	Skip    string    `gorm:"-" json:"-"`
	private string
}

// testOther 用于类型不匹配断言。
type testOther struct {
	ID int64 `cache:"id"`
}

// fakeStore 是 kvStore 的内存实现，语义与生产 Lua 脚本对齐。
// fakeStore 是 kvStore 的内存实现：状态委托 memState，权威写路径委托
// semantics.applyWriteOp（与生产 Lua 逐条同构的参考实现），差分对拍防漂移。
type fakeStore struct {
	mu       sync.Mutex
	m        *memState
	hsetErr  error                // 注入故障：同时作用于 HSetAll 与 ApplyWrite（模拟 Redis 宕机）
	writeErr func(*writeOp) error // 按 op 类型选择性注入写失败（refresh 失败但 invalidate 成功等场景）
	ttl      map[string]time.Duration
}

func newFakeStore() *fakeStore {
	return &fakeStore{m: newMemState(), ttl: make(map[string]time.Duration)}
}

func (s *fakeStore) HGetAll(_ context.Context, key string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMap(s.m.hashes[key]), nil
}

func (s *fakeStore) HSetAll(_ context.Context, rowKey, tombKey string, fields []string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hsetErr != nil {
		return s.hsetErr
	}
	// 重建该行前先清墓碑，与 redisStore 的 pipeline 语义一致。
	delete(s.m.strs, tombKey)
	delete(s.m.expiry, tombKey)
	s.m.hashes[rowKey] = fieldsToMap(fields)
	if ttl > 0 {
		s.ttl[rowKey] = ttl
	}
	return nil
}

func (s *fakeStore) ApplyWrite(_ context.Context, keys []string, payload, txid string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hsetErr != nil {
		return "", s.hsetErr
	}
	var op writeOp
	if err := json.Unmarshal([]byte(payload), &op); err != nil {
		return "", err
	}
	if s.writeErr != nil {
		if err := s.writeErr(&op); err != nil {
			return "", err
		}
	}
	k := writeKeys{
		row: keys[0], tomb: keys[1], dirty: keys[2], retry: keys[3], keySet: keys[4],
		secret: keys[5], seq: keys[6], dirtyDel: keys[7], uq: keys[8], lock: keys[9],
		inflight: keys[10], ix: keys[11:],
	}
	res := applyWriteOp(s.m, k, &op, txid, time.Now().UnixMilli())
	raw, err := json.Marshal(res)
	return string(raw), err
}

func (s *fakeStore) ApplyTx(_ context.Context, keys []string, payload string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hsetErr != nil {
		return "", s.hsetErr
	}
	var p txPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", err
	}
	blocks := make([]writeKeys, 0, len(p.Ops))
	off := 0
	for _, op := range p.Ops {
		n := 11 + len(op.Idx)
		blocks = append(blocks, writeKeys{
			row: keys[off], tomb: keys[off+1], dirty: keys[off+2], retry: keys[off+3], keySet: keys[off+4],
			secret: keys[off+5], seq: keys[off+6], dirtyDel: keys[off+7], uq: keys[off+8], lock: keys[off+9],
			inflight: keys[off+10], ix: keys[off+11 : off+n],
		})
		off += n
	}
	results, errStr := applyTxOps(s.m, blocks, p.Ops, p.Txid, time.Now().UnixMilli())
	raw, err := json.Marshal(txResult{Results: results, Err: errStr})
	return string(raw), err
}

func (s *fakeStore) ZAdd(_ context.Context, key, member string, score int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.zset(key)[member] = float64(score)
	return nil
}

func (s *fakeStore) DueMembers(_ context.Context, key string, maxScore int64, count int) ([]dirtyMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m.dueMembers(key, maxScore, count), nil
}

func (s *fakeStore) Claim(_ context.Context, dirtyKey, rowKey, tombKey, inflightKey, member string, score, leaseDue int64) (map[string]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if s.m.alive(tombKey, now) {
		delete(s.m.zset(dirtyKey), member)
		return nil, false, nil
	}
	cur, ok := s.m.zset(dirtyKey)[member]
	if !ok || int64(cur) != score {
		return nil, false, nil
	}
	h := s.m.hashes[rowKey]
	if len(h) == 0 {
		delete(s.m.zset(dirtyKey), member)
		return nil, false, nil
	}
	delete(s.m.zset(dirtyKey), member)
	s.m.zset(inflightKey)[member] = float64(leaseDue)
	return copyMap(h), true, nil
}

func (s *fakeStore) ZRem(_ context.Context, key, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m.zset(key), member)
	return nil
}

func (s *fakeStore) ZScore(_ context.Context, key, member string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	score, ok := s.m.zset(key)[member]
	return int64(score), ok, nil
}

func (s *fakeStore) HDel(_ context.Context, key, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m.hashes[key], member)
	return nil
}

func (s *fakeStore) HIncrBy(_ context.Context, key, member string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.m.hash(key)
	n, _ := strconv.ParseInt(h[member], 10, 64)
	n++
	h[member] = strconv.FormatInt(n, 10)
	return n, nil
}

func (s *fakeStore) SetTomb(_ context.Context, tombKey string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.strs[tombKey] = "1"
	if ttl > 0 {
		s.m.expiry[tombKey] = time.Now().Add(ttl).UnixMilli()
	} else {
		delete(s.m.expiry, tombKey)
	}
	return nil
}

func (s *fakeStore) DelTomb(_ context.Context, tombKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m.strs, tombKey)
	delete(s.m.expiry, tombKey)
	return nil
}

func (s *fakeStore) TombAlive(_ context.Context, tombKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m.alive(tombKey, time.Now().UnixMilli()), nil
}

func (s *fakeStore) KeySetScan(_ context.Context, key string, budget int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.m.sets[key]
	if budget > 0 && len(set) > budget {
		return nil, fmt.Errorf("%w: %s has %d rows", ErrScanBudgetExceeded, key, len(set))
	}
	out := make([]string, 0, len(set))
	for member := range set {
		out = append(out, member)
	}
	sort.Strings(out)
	return out, nil
}

func (s *fakeStore) IndexRange(_ context.Context, key, min, max string, byLex bool, offset, count int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.m.zsets[key]
	var out []string
	if byLex {
		for member := range z {
			if !lexInRange(member, min, max) {
				continue
			}
			out = append(out, member)
		}
		sort.Strings(out)
	} else {
		lo, loEx := parseScoreBound(min, false)
		hi, hiEx := parseScoreBound(max, true)
		type sm struct {
			member string
			score  float64
		}
		var cand []sm
		for member, score := range z {
			if score < lo || (loEx && score == lo) || score > hi || (hiEx && score == hi) {
				continue
			}
			cand = append(cand, sm{member, score})
		}
		sort.Slice(cand, func(i, j int) bool {
			if cand[i].score != cand[j].score {
				return cand[i].score < cand[j].score
			}
			return cand[i].member < cand[j].member
		})
		for _, c := range cand {
			out = append(out, c.member)
		}
	}
	if offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out, nil
}

// parseScoreBound 解析 ZRANGEBYSCORE 边界：-inf/+inf、( 前缀表开区间。
func parseScoreBound(s string, isMax bool) (float64, bool) {
	ex := strings.HasPrefix(s, "(")
	s = strings.TrimPrefix(s, "(")
	switch s {
	case "-inf":
		return math.Inf(-1), false
	case "+inf", "inf":
		return math.Inf(1), false
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v, ex
}

// lexInRange 判断 member 是否在 ZRANGEBYLEX 边界内：- / + 全界，[ 含 ( 排。
func lexInRange(member, min, max string) bool {
	if min != "-" {
		v := min[1:]
		if min[0] == '[' && member < v {
			return false
		}
		if min[0] == '(' && member <= v {
			return false
		}
	}
	if max != "+" {
		v := max[1:]
		if max[0] == '[' && member > v {
			return false
		}
		if max[0] == '(' && member >= v {
			return false
		}
	}
	return true
}

func (s *fakeStore) IndexCard(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.m.zsets[key])), nil
}

func (s *fakeStore) RowsGet(_ context.Context, keys []string) ([]map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]string, len(keys))
	for i, key := range keys {
		out[i] = copyMap(s.m.hashes[key])
	}
	return out, nil
}

func (s *fakeStore) SeqAlloc(_ context.Context, key string, delta int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := strconv.ParseInt(s.m.strs[key], 10, 64)
	n += delta
	s.m.strs[key] = strconv.FormatInt(n, 10)
	return n, nil
}

func (s *fakeStore) TryLock(_ context.Context, key, txid string, ttlMS, waitMS int64) (bool, error) {
	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	for {
		s.mu.Lock()
		now := time.Now().UnixMilli()
		free := !s.m.alive(key, now)
		if free {
			s.m.strs[key] = txid
			s.m.expiry[key] = now + ttlMS
		}
		s.mu.Unlock()
		if free {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *fakeStore) Unlock(_ context.Context, key, txid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m.strs[key] == txid {
		delete(s.m.strs, key)
		delete(s.m.expiry, key)
	}
	return nil
}

func (s *fakeStore) RequeueInflight(_ context.Context, inflightKey, dirtyKey string, nowMS int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for member, score := range s.m.zset(inflightKey) {
		if int64(score) <= nowMS {
			s.m.zset(dirtyKey)[member] = float64(nowMS)
			delete(s.m.zset(inflightKey), member)
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) Ping(context.Context) error { return nil }

// hash 返回某行当前内容（测试断言用）。
func (s *fakeStore) hash(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMap(s.m.hashes[key])
}

// dirtyCount 返回脏账成员数。
func (s *fakeStore) dirtyCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m.zsets[key])
}

func (s *fakeStore) retryCount(key, member string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := strconv.ParseInt(s.m.hashes[key][member], 10, 64)
	return n
}

// hasTomb 返回墓碑是否仍生效（测试断言用）。
func (s *fakeStore) hasTomb(tombKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m.alive(tombKey, time.Now().UnixMilli())
}

// fakeSource 是绑定读取闭包的测试替身。
type fakeSource struct {
	one       *testUser
	list      []*testUser
	oneErr    error
	listErr   error
	delErr    error
	oneCalls  int
	listCalls int
	delCalls  int
	lastVals  []condVal
	deleted   [][]condVal
}

// fakeSink 记录批量落库内容，可注入错误。
type fakeSink struct {
	mu    sync.Mutex
	rows  []*testUser
	err   error
	calls int
}

func (s *fakeSink) add(rows []*testUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, rows...)
	return nil
}

func (s *fakeSink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *fakeSink) snapshot() []*testUser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*testUser(nil), s.rows...)
}

// newTestEngine 构造带 test_user 绑定的引擎。
func newTestEngine(t *testing.T) (*Engine, *fakeStore, *fakeSource, *fakeSink, *binding) {
	t.Helper()
	fs := newFakeStore()
	e := newEngine(DefaultConfig(), fs)
	keys, err := makeTableKeys(e.cfg.KeyPrefix, "test_user", false)
	if err != nil {
		t.Fatalf("makeTableKeys: %v", err)
	}
	b, err := newBinding[testUser]("test_user", backendMySQL, BindOption[testUser]{AllowSourceBackfill: true}, keys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding: %v", err)
	}
	src := &fakeSource{one: &testUser{ID: 7, Name: "db"}}
	sink := &fakeSink{}
	b.srcFindOne = func(_ context.Context, vals []condVal) (any, error) {
		src.oneCalls++
		src.lastVals = vals
		if src.oneErr != nil {
			return nil, src.oneErr
		}
		if src.one == nil {
			return nil, ErrNoRows
		}
		return src.one, nil
	}
	b.srcFindList = func(_ context.Context, vals []condVal) ([]any, error) {
		src.listCalls++
		src.lastVals = vals
		if src.listErr != nil {
			return nil, src.listErr
		}
		out := make([]any, len(src.list))
		for i, row := range src.list {
			out[i] = row
		}
		return out, nil
	}
	b.sinkBatch = func(_ context.Context, rows []any) error {
		typed := make([]*testUser, len(rows))
		for i, row := range rows {
			typed[i] = row.(*testUser)
		}
		return sink.add(typed)
	}
	b.sinkDelete = func(_ context.Context, vals []condVal) error {
		src.delCalls++
		if src.delErr != nil {
			return src.delErr
		}
		src.deleted = append(src.deleted, vals)
		return nil
	}
	if err := e.register(b); err != nil {
		t.Fatalf("register: %v", err)
	}
	return e, fs, src, sink, b
}

// idCond 构造只含主键的条件。
type idCond struct {
	ID *int64 `cache:"id"`
}

// nameCond 构造只含非键列的条件。
type nameCond struct {
	Name *string `cache:"name"`
}

func ptrInt(v int64) *int64   { return &v }
func ptrStr(v string) *string { return &v }
func ptrBool(v bool) *bool    { return &v }

var errSinkDown = errors.New("sink down")

func TestPackageLevelNotInitialized(t *testing.T) {
	old := defaultEngine.Swap(nil)
	defer defaultEngine.Store(old)

	ctx := context.Background()
	var out testUser
	if err := Query[testUser](ctx, "t", &idCond{ID: ptrInt(1)}, &out); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Query 期望 ErrNotInitialized，实际 %v", err)
	}
	if err := Save[testUser](ctx, "t", "1", &testUser{ID: 1}, false); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Save 期望 ErrNotInitialized，实际 %v", err)
	}
	if err := Flush(ctx, "t"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Flush 期望 ErrNotInitialized，实际 %v", err)
	}
	if err := Start(ctx); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Start 期望 ErrNotInitialized，实际 %v", err)
	}
	if err := Stop(ctx); err != nil {
		t.Fatalf("未初始化时 Stop 应为 no-op，实际 %v", err)
	}
}

func TestInitRejectsNilRedis(t *testing.T) {
	if err := Init(DefaultConfig(), nil); err == nil {
		t.Fatal("nil Redis 应返回错误")
	}
}

func TestPackageLevelQuerySave(t *testing.T) {
	e, _, src, sink, _ := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "pkg"}
	old := defaultEngine.Swap(e)
	defer defaultEngine.Store(old)

	ctx := context.Background()
	if err := Save[testUser](ctx, "test_user", "7", &testUser{ID: 7, Name: "pkg"}, true); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if rows := sink.snapshot(); len(rows) != 1 {
		t.Fatalf("mustSave 落库行=%v", rows)
	}
	var out testUser
	if err := Query[testUser](ctx, "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Name != "pkg" {
		t.Fatalf("out.Name=%q", out.Name)
	}
	if err := Flush(ctx, "test_user"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func TestConfigFrom(t *testing.T) {
	cfg := ConfigFrom(&config.CacheConfig{
		KeyPrefix:       "x",
		TTLMS:           60000,
		FlushIntervalMS: 500,
		QueryTimeoutMS:  100,
		FlushTimeoutMS:  200,
		CloseBudgetMS:   300,
		MaxRetry:        2,
		DirtyBatchSize:  10,
	})
	if cfg.KeyPrefix != "x" || cfg.TTL != time.Minute {
		t.Fatalf("cfg=%+v", cfg)
	}
	if cfg.FlushInterval != 500*time.Millisecond || cfg.DirtyBatchSize != 10 || cfg.MaxRetry != 2 {
		t.Fatalf("cfg=%+v", cfg)
	}
	def := ConfigFrom(nil)
	if def.KeyPrefix != DefaultConfig().KeyPrefix {
		t.Fatalf("nil 配置应回落默认值: %+v", def)
	}
}

func TestEscapeKeySegment(t *testing.T) {
	if got := escapeKeySegment("a:b%c"); got != "a%3Ab%25c" {
		t.Fatalf("escape=%q", got)
	}
}

func TestConfigNormalize(t *testing.T) {
	cfg := Config{}.normalize()
	def := DefaultConfig()
	if cfg.FlushInterval != def.FlushInterval || cfg.DirtyBatchSize != def.DirtyBatchSize {
		t.Fatalf("normalize 未补齐默认值: %+v", cfg)
	}
	if cfg.TTL != 0 {
		t.Fatalf("默认 TTL 应为 0，实际 %v", cfg.TTL)
	}
}

func TestStartStopWiresPeriodicFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localtimer.Run(ctx)
	defer localtimer.TimeStop(context.Background())

	e, fs, _, sink, b := newTestEngine(t)
	b.interval = 20 * time.Millisecond
	e.cfg.FlushInterval = 20 * time.Millisecond
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.save[testUser](ctx, "test_user", "7", &testUser{ID: 7, Name: "timer"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fs.dirtyCount(b.keys.dirty) == 0 && len(sink.snapshot()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("周期定时器未清空脏账，剩余 %d", n)
	}
	if rows := sink.snapshot(); len(rows) != 1 || rows[0].Name != "timer" {
		t.Fatalf("周期定时器落库内容=%v", rows)
	}
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestBuildMetaColumns(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	want := []string{"id", "name", "age", "active", "score", "created"}
	if len(m.Fields) != len(want) {
		t.Fatalf("字段数=%d，期望 %d（%v）", len(m.Fields), len(want), refColumns(m))
	}
	for i, col := range want {
		if m.Fields[i].Column != col {
			t.Fatalf("第 %d 列=%q，期望 %q", i, m.Fields[i].Column, col)
		}
	}
	if !m.HasPK || m.PKCol != "id" {
		t.Fatalf("主键=%q hasPK=%v，期望 id", m.PKCol, m.HasPK)
	}
	if !reflect.DeepEqual(m.KeyCols, []string{"id"}) {
		t.Fatalf("KeyCols=%v，期望 [id]", m.KeyCols)
	}
	for _, f := range m.Fields {
		if f.Column == "Skip" || f.Column == "Private" || f.Column == "private" {
			t.Fatalf("非列字段未跳过: %s", f.Column)
		}
	}
}

func TestBuildMetaCompositeKey(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), []string{"name", "id"}, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if !reflect.DeepEqual(m.KeyCols, []string{"name", "id"}) {
		t.Fatalf("KeyCols=%v", m.KeyCols)
	}
}

func TestBuildMetaBadKeyColumn(t *testing.T) {
	if _, err := buildMeta(typeOf[testUser](), []string{"nope"}, backendMySQL); err == nil {
		t.Fatal("未知 key 列应报错")
	}
}

func TestCacheTagSkip(t *testing.T) {
	type model struct {
		ID   int64  `cache:"id"`
		Keep string `cache:"keep"`
		Drop string `cache:"-"`
	}
	m, err := buildMeta(typeOf[model](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if len(m.Fields) != 2 {
		t.Fatalf("字段数=%d，期望 2（%v）", len(m.Fields), refColumns(m))
	}
}

func TestKeyFromRowZeroIntegerKey(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	// 零值整数键列合法（cut_point_record 抽奖行 participation_id=0）。
	key, err := keyFromRow(m, reflect.ValueOf(&testUser{}).Elem())
	if err != nil {
		t.Fatalf("零值整数键应合法，实际 %v", err)
	}
	if key != "0" {
		t.Fatalf("key=%q，期望 0", key)
	}
	key, err = keyFromRow(m, reflect.ValueOf(&testUser{ID: 42}).Elem())
	if err != nil {
		t.Fatalf("keyFromRow: %v", err)
	}
	if key != "42" {
		t.Fatalf("key=%q，期望 42", key)
	}
}

func TestKeyFromRowEmptyStringKey(t *testing.T) {
	type strKey struct {
		Code string `gorm:"column:code;primaryKey"`
		Val  int64  `gorm:"column:val"`
	}
	m, err := buildMeta(typeOf[strKey](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if _, err := keyFromRow(m, reflect.ValueOf(&strKey{}).Elem()); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("空串键应返回 ErrKeyMissing，实际 %v", err)
	}
	key, err := keyFromRow(m, reflect.ValueOf(&strKey{Code: "abc"}).Elem())
	if err != nil {
		t.Fatalf("keyFromRow: %v", err)
	}
	if key != "abc" {
		t.Fatalf("key=%q，期望 abc", key)
	}
}

func TestKeyFromRowCompositeZeroComponent(t *testing.T) {
	type composite struct {
		ActivityID      int64 `gorm:"column:activity_id"`
		ParticipationID int64 `gorm:"column:participation_id"`
		CutterMemberID  int64 `gorm:"column:cutter_member_id"`
		Point           int64 `gorm:"column:point"`
	}
	cols := []string{"activity_id", "participation_id", "cutter_member_id"}
	m, err := buildMeta(typeOf[composite](), cols, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	// participation_id=0 的抽奖行：复合键含零分量仍合法。
	key, err := keyFromRow(m, reflect.ValueOf(&composite{ActivityID: 7, ParticipationID: 0, CutterMemberID: 9}).Elem())
	if err != nil {
		t.Fatalf("复合键零分量应合法，实际 %v", err)
	}
	if key != "7:0:9" {
		t.Fatalf("key=%q，期望 7:0:9", key)
	}
}

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"ID":       "id",
		"MemberID": "member_id",
		"Name":     "name",
		"URLPath":  "url_path",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Fatalf("snakeCase(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func refColumns(m *tableMeta) []string {
	out := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		out[i] = f.Column
	}
	return out
}

func TestQueryRedisHitSkipsDB(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	cached := &testUser{ID: 7, Name: "cached"}
	fields, err := encodeRow(b.meta, reflect.ValueOf(cached).Elem())
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	if err := fs.HSetAll(context.Background(), b.keys.rowPrefix+"7", b.keys.tombPrefix+"7", fields, 0); err != nil {
		t.Fatalf("HSetAll: %v", err)
	}

	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 0 {
		t.Fatalf("Redis 命中不应回源，实际调用 %d 次", src.oneCalls)
	}
	if out.Name != "cached" {
		t.Fatalf("out.Name=%q", out.Name)
	}
}

func TestQueryMissBackfills(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "db"}

	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 1 {
		t.Fatalf("未命中应回源一次，实际 %d 次", src.oneCalls)
	}
	if out.Name != "db" {
		t.Fatalf("out.Name=%q", out.Name)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); h["name"] != "db" {
		t.Fatalf("回填内容=%v", h)
	}
}

func TestQueryKeyConditionWithExtraFilterGoesDB(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "db"}

	cond := struct {
		ID   *int64  `cache:"id"`
		Name *string `cache:"name"`
	}{ID: ptrInt(7), Name: ptrStr("db")}
	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &cond, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 1 {
		t.Fatalf("含非键列应回源，实际 %d 次", src.oneCalls)
	}
}

func TestQueryNonKeyCondition(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "db"}

	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &nameCond{Name: ptrStr("db")}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 1 {
		t.Fatalf("非键条件应回源一次，实际 %d 次", src.oneCalls)
	}
	if _, ok := fs.hash(b.keys.rowPrefix + "7")["name"]; !ok {
		t.Fatal("命中的行应回填 Redis")
	}
}

func TestQueryListBackfillsAll(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	src.list = []*testUser{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}

	var out []testUser
	if err := e.query[testUser](context.Background(), "test_user", &nameCond{Name: ptrStr("x")}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) != 2 || src.listCalls != 1 {
		t.Fatalf("列表结果=%v calls=%d", out, src.listCalls)
	}
	if fs.hash(b.keys.rowPrefix + "1")["name"] != "a" || fs.hash(b.keys.rowPrefix + "2")["name"] != "b" {
		t.Fatalf("列表回填不完整")
	}
}

func TestQueryEmptyCondition(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", &idCond{}, &out)
	if !errors.Is(err, ErrEmptyCondition) {
		t.Fatalf("期望 ErrEmptyCondition，实际 %v", err)
	}
}

func TestQueryBadCondition(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", idCond{ID: ptrInt(1)}, &out)
	if !errors.Is(err, ErrBadCond) {
		t.Fatalf("期望 ErrBadCond，实际 %v", err)
	}
}

func TestQueryBadOut(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out int
	err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(1)}, &out)
	if !errors.Is(err, ErrBadOut) {
		t.Fatalf("期望 ErrBadOut，实际 %v", err)
	}
}

func TestQueryUnboundAndTypeMismatch(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out testUser
	if err := e.query[testUser](context.Background(), "nope", &idCond{ID: ptrInt(1)}, &out); !errors.Is(err, ErrTableUnbound) {
		t.Fatalf("期望 ErrTableUnbound，实际 %v", err)
	}
	var other testOther
	if err := e.query[testOther](context.Background(), "test_user", &idCond{ID: ptrInt(1)}, &other); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("期望 ErrTypeMismatch，实际 %v", err)
	}
}

func TestQueryNoRows(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	src.one = nil
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(99)}, &out)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("期望 ErrNoRows，实际 %v", err)
	}
}

// TestQueryMissNoBackfillByDefault P1.5：默认（AllowSourceBackfill=false）未命中
// 直接 ErrNoRows，绝不回源 MySQL（资金表 fail-closed，库只是滞后物化视图）。
func TestQueryMissNoBackfillByDefault(t *testing.T) {
	e, _, src, _, b := newTestEngine(t)
	b.allowSourceBackfill = false
	src.one = &testUser{ID: 7, Name: "db"}
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(7)}, &out)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("默认未命中应 ErrNoRows（不回源），实际 %v", err)
	}
	if src.oneCalls != 0 {
		t.Fatalf("默认未命中不得回源，src.oneCalls=%d", src.oneCalls)
	}
}

// TestDeleteTombBlocksBackfillResurrection P1.5 读路径门控：删除立墓碑后，即便
// MySQL 异步删除尚未完成（srcFindOne 仍返回旧行），点查也必须在回源前被墓碑拦截，
// 返回 ErrNoRows 且行不被回填复活。先红后绿：回退 queryOne 墓碑门控本用例必 FAIL。
func TestDeleteTombBlocksBackfillResurrection(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "live"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	src.one = &testUser{ID: 7, Name: "stale"} // 模拟 MySQL 删除滞后
	var out testUser
	err := e.query[testUser](ctx, "test_user", &idCond{ID: ptrInt(7)}, &out)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("墓碑存活时点查应 ErrNoRows（防复活/陈旧读），实际 %v", err)
	}
	if src.oneCalls != 0 {
		t.Fatalf("墓碑门控应在回源前拦截，src.oneCalls=%d", src.oneCalls)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); len(h) != 0 {
		t.Fatalf("删除行不得被回填复活，row=%v", h)
	}
}

// TestSaveNXRefusesWhenTombAlive P1.5 写路径门控：NX 回填撞墓碑必须拒绝
// （affected=0、零副作用），与读路径门控互为纵深防御。
func TestSaveNXRefusesWhenTombAlive(t *testing.T) {
	e, fs, _, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	stale := &testUser{ID: 7, Name: "stale"}
	fields, err := encodeRow(b.meta, reflect.ValueOf(stale).Elem())
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	if h := e.backfillRow(ctx, b, "7", fields); len(h) != 0 {
		t.Fatalf("NX 撞墓碑应拒绝回填，实际 row=%v", h)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); len(h) != 0 {
		t.Fatalf("墓碑存活时行不得复活，row=%v", h)
	}
}

// failGetStore 让 Redis 点查读失败，用于验证读路径 fail-closed（不静默回源）。
type failGetStore struct{ *fakeStore }

func (s *failGetStore) HGetAll(_ context.Context, _ string) (map[string]string, error) {
	return nil, errors.New("redis get down")
}

// TestQueryRedisGetErrorFailsClosed P1.5：Redis 读错误直接上抛，绝不吞掉错误后
// 回源 MySQL 制造双真源。
func TestQueryRedisGetErrorFailsClosed(t *testing.T) {
	e, fs, src, _, _ := newTestEngine(t)
	e.store = &failGetStore{fakeStore: fs}
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(7)}, &out)
	if err == nil {
		t.Fatal("Redis 读错误应 fail-closed 上抛")
	}
	if src.oneCalls != 0 {
		t.Fatalf("Redis 读错误不得静默回源 MySQL，src.oneCalls=%d", src.oneCalls)
	}
}

// seedStaleRow 在 fake Redis 里放一份旧行 hash，并登记脏账与重试计数，
// 模拟「库里已是新值、缓存还停在旧值且有在途落库」的最坏状态。
func seedStaleRow(t *testing.T, fs *fakeStore, b *binding) (rowKey, tombKey string) {
	t.Helper()
	ctx := context.Background()
	rowKey = b.keys.rowPrefix + "7"
	tombKey = b.keys.tombPrefix + "7"
	stale := &testUser{ID: 7, Name: "stale"}
	fields, err := encodeRow(b.meta, reflect.ValueOf(stale).Elem())
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	if err := fs.HSetAll(ctx, rowKey, tombKey, fields, 0); err != nil {
		t.Fatalf("HSetAll: %v", err)
	}
	if err := fs.ZAdd(ctx, b.keys.dirty, "7", 100); err != nil {
		t.Fatalf("ZAdd: %v", err)
	}
	if _, err := fs.HIncrBy(ctx, b.keys.retry, "7"); err != nil {
		t.Fatalf("HIncrBy: %v", err)
	}
	if h := fs.hash(rowKey); h["name"] != "stale" {
		t.Fatalf("前置条件失败：旧行 hash=%v", h)
	}
	return rowKey, tombKey
}

// failApplyStore 让权威写路径整体失败，用于验证删除的 fail-closed 语义。
type failApplyStore struct{ *fakeStore }

func (s *failApplyStore) ApplyWrite(_ context.Context, _ []string, _, _ string) (string, error) {
	return "", errors.New("redis apply down")
}

// TestDeleteRedisFailureFailsClosed 契约变更（Redis 权威）：删除是单条 Lua 原子 op，
// Redis 失败直接上抛且零副作用（行/墓碑/脏账全不动），没有「DB 兜底」旁路。
func TestDeleteRedisFailureFailsClosed(t *testing.T) {
	e, fs, _, _, b := newTestEngine(t)
	rowKey, _ := seedStaleRow(t, fs, b)
	e.store = &failApplyStore{fakeStore: fs}

	err := e.remove(context.Background(), b, &idCond{ID: ptrInt(7)})
	if err == nil {
		t.Fatal("Redis 失败时删除应返回错误")
	}
	if h := fs.hash(rowKey); len(h) == 0 {
		t.Fatal("删除失败应零副作用，行 hash 保留")
	}
	if fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("删除失败不应立墓碑")
	}
	if n := fs.dirtyCount(b.keys.dirtyDel); n != 0 {
		t.Fatal("删除失败不应入 dirtyDel")
	}
}

func TestSaveDelayedCoalescesSameKey(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	if err := e.save[testUser](context.Background(), "test_user", "", &testUser{ID: 7, Name: "a"}, false); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := e.save[testUser](context.Background(), "test_user", "", &testUser{ID: 7, Name: "b"}, false); err != nil {
		t.Fatalf("save b: %v", err)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 1 {
		t.Fatalf("同键多次写应合并为 1 个脏成员，实际 %d", n)
	}
	if got := fs.hash(b.keys.rowPrefix + "7")["name"]; got != "b" {
		t.Fatalf("Redis 应为最后值 b，实际 %q", got)
	}
	if err := e.flushAll(context.Background(), b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	rows := sink.snapshot()
	if len(rows) != 1 || rows[0].Name != "b" {
		t.Fatalf("落库行=%v，期望仅最后值 b", rows)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("flush 后脏账应清空，实际 %d", n)
	}
}

func TestSaveMustSaveWritesBeforeReturn(t *testing.T) {
	e, _, _, sink, _ := newTestEngine(t)
	if err := e.save[testUser](context.Background(), "test_user", "7", &testUser{ID: 7, Name: "now"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	rows := sink.snapshot()
	if len(rows) != 1 || rows[0].Name != "now" {
		t.Fatalf("mustSave 返回前应已落库，实际 %v", rows)
	}
}

// TestSaveRedisFailureFailsClosed 契约变更（Redis 权威，原 TestSaveRedisFailureFallsBackToDB）：
// Redis 失败 mustSave 不再退化直写 DB（双真源旁路已删），而是返回错误且零副作用：
// sink 未被调用、脏账为空、行 hash 不存在。库只是滞后物化视图，不能绕过 Redis 写。
func TestSaveRedisFailureFailsClosed(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	fs.hsetErr = errors.New("redis down")
	err := e.save[testUser](context.Background(), "test_user", "7", &testUser{ID: 7, Name: "fallback"}, true)
	if err == nil {
		t.Fatal("Redis 失败时 mustSave 应返回错误（无 DB 退化）")
	}
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("不得绕过 Redis 直写 DB，实际 %v", rows)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("失败应零副作用，脏账=%d", n)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); len(h) != 0 {
		t.Fatalf("失败应零副作用，行 hash=%v", h)
	}
}

func TestSaveDelayedRedisFailureReturnsError(t *testing.T) {
	e, fs, _, _, _ := newTestEngine(t)
	fs.hsetErr = errors.New("redis down")
	err := e.save[testUser](context.Background(), "test_user", "7", &testUser{ID: 7}, false)
	if err == nil {
		t.Fatal("延迟写在 Redis 失败时应返回错误")
	}
}

func TestFlushFailureKeepsDirtyAndRetries(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	sink.setErr(errSinkDown)
	err := e.save[testUser](context.Background(), "test_user", "7", &testUser{ID: 7, Name: "retry"}, true)
	if !errors.Is(err, errSinkDown) {
		t.Fatalf("期望 sink 错误，实际 %v", err)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 1 {
		t.Fatalf("失败后应保留脏账，实际 %d", n)
	}
	if n := fs.retryCount(b.keys.retry, "7"); n != 1 {
		t.Fatalf("retry 计数=%d，期望 1", n)
	}

	sink.setErr(nil)
	if err := e.flushAll(context.Background(), b); err != nil {
		t.Fatalf("恢复后 flushAll: %v", err)
	}
	rows := sink.snapshot()
	if len(rows) != 1 || rows[0].Name != "retry" {
		t.Fatalf("落库行=%v", rows)
	}
	if n := fs.retryCount(b.keys.retry, "7"); n != 0 {
		t.Fatalf("成功后 retry 应清零，实际 %d", n)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("成功后脏账应清空，实际 %d", n)
	}
}

func TestSaveKeyMissing(t *testing.T) {
	type strKey struct {
		Code string `gorm:"column:code;primaryKey"`
		Val  int64  `gorm:"column:val"`
	}
	fs := newFakeStore()
	e := newEngine(DefaultConfig(), fs)
	keys, err := makeTableKeys(e.cfg.KeyPrefix, "str_key", false)
	if err != nil {
		t.Fatalf("makeTableKeys: %v", err)
	}
	b, err := newBinding[strKey]("str_key", backendMySQL, BindOption[strKey]{}, keys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding: %v", err)
	}
	if err := e.register(b); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 空串键无法推导行键，save 路径须把 keyFromRow 的 ErrKeyMissing 透传出来。
	err = e.save[strKey](context.Background(), "str_key", "", &strKey{Val: 1}, false)
	if !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("期望 ErrKeyMissing，实际 %v", err)
	}
}

func TestSaveNilRow(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	if err := e.save[testUser](context.Background(), "test_user", "7", nil, false); !errors.Is(err, ErrNilRow) {
		t.Fatalf("期望 ErrNilRow，实际 %v", err)
	}
}

func TestStopFinalFlushesDirty(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	if err := e.save[testUser](context.Background(), "test_user", "7", &testUser{ID: 7, Name: "final"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("Stop 后脏账应清空，实际 %d", n)
	}
	if rows := sink.snapshot(); len(rows) != 1 || rows[0].Name != "final" {
		t.Fatalf("Stop final flush 内容=%v", rows)
	}
}

func TestClaimDoesNotDropConcurrentSave(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	rowKey := "{tgc:t}:r:7"
	dirtyKey := "{tgc:t}:dirty"
	tombKey := "{tgc:t}:t:7"
	_ = fs.HSetAll(ctx, rowKey, tombKey, []string{"name", "old"}, 0)
	_ = fs.ZAdd(ctx, dirtyKey, "7", 100)

	inflightKey := "{tgc:t}:inflight"
	leaseDue := time.Now().Add(time.Minute).UnixMilli()
	if _, ok, _ := fs.Claim(ctx, dirtyKey, rowKey, tombKey, inflightKey, "7", 100, leaseDue); !ok {
		t.Fatal("首次领取应成功")
	}
	// 并发 Save 刷新了 score，旧 score 的领取不得销账。
	_ = fs.ZAdd(ctx, dirtyKey, "7", 200)
	if _, ok, _ := fs.Claim(ctx, dirtyKey, rowKey, tombKey, inflightKey, "7", 100, leaseDue); ok {
		t.Fatal("score 已变更时领取应失败")
	}
	if n := fs.dirtyCount(dirtyKey); n != 1 {
		t.Fatalf("并发写入的脏账不应被误删，实际 %d", n)
	}
}

// installTxSpanRecorder 安装临时 SDK TracerProvider，并把 span 收进内存 recorder。
//
// otel.SetTracerProvider 是进程级全局状态，必须在 t.Cleanup 恢复，避免污染
// 同包其它用例（本包用例均未使用 t.Parallel，顺序执行即可）。
func installTxSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	// SpanRecorder 本身就是 SpanProcessor，直接挂进 provider 即可在 OnEnd 收到 span。
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("关闭 TracerProvider: %v", err)
		}
	})
	return recorder
}

// findTxEndedSpan 按名字在已结束 span 里查找；找不到返回 nil。
func findTxEndedSpan(recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// TestStartTxSpanRecordsSpan 验证 startTxSpan / recordTxSpanError 在真实
// TracerProvider 下的行为：记录 span、继承父上下文、错误置 Error 状态。
func TestStartTxSpanRecordsSpan(t *testing.T) {
	t.Run("records span and inherits parent", func(t *testing.T) {
		recorder := installTxSpanRecorder(t)

		parentCtx, parent := otel.Tracer("touchgocore.test").Start(context.Background(), "parent.span")
		childCtx, child := startTxSpan(parentCtx, "test.tx.span")

		// 配了真 provider 后 span 必须有效（noop span 的 SpanContext 无效）。
		if !child.SpanContext().IsValid() {
			t.Fatal("span 上下文无效，说明没有走真实 TracerProvider")
		}
		if !child.IsRecording() {
			t.Fatal("span 未处于记录状态")
		}
		// 同一 trace：子 span 必须继承父 span 的 TraceID。
		if child.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Fatalf("traceID 未继承: child=%s parent=%s",
				child.SpanContext().TraceID(), parent.SpanContext().TraceID())
		}
		// 返回的 ctx 必须携带该 span，供 dbcache 下游继续挂子 span。
		if got := trace.SpanFromContext(childCtx); got.SpanContext().SpanID() != child.SpanContext().SpanID() {
			t.Fatal("返回的 ctx 未携带 startTxSpan 创建的 span")
		}

		child.End()
		parent.End()

		got := findTxEndedSpan(recorder, "test.tx.span")
		if got == nil {
			t.Fatalf("未记录到名为 test.tx.span 的 span，实际 %d 条", len(recorder.Ended()))
		}
		if got.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Fatalf("父 spanID=%s，期望 %s", got.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		if got.Status().Code != codes.Unset {
			t.Fatalf("无错误时 span 状态应为 Unset，实际 %v", got.Status().Code)
		}
	})

	t.Run("nil ctx falls back to background", func(t *testing.T) {
		recorder := installTxSpanRecorder(t)

		// ctx 为 nil 时不得 panic，且仍要发出 span（回退 context.Background）。
		_, span := startTxSpan(nil, "test.tx.span.nilctx")
		span.End()

		if got := findTxEndedSpan(recorder, "test.tx.span.nilctx"); got == nil {
			t.Fatal("ctx 为 nil 时未记录到 span")
		}
	})

	t.Run("records error status", func(t *testing.T) {
		recorder := installTxSpanRecorder(t)

		_, span := startTxSpan(context.Background(), "test.tx.span.error")
		// nil 错误不得改状态，也不得 panic。
		recordTxSpanError(span, nil)
		recordTxSpanError(span, errors.New("tx boom"))
		span.End()

		got := findTxEndedSpan(recorder, "test.tx.span.error")
		if got == nil {
			t.Fatal("未记录到错误用例 span")
		}
		if got.Status().Code != codes.Error {
			t.Fatalf("span 状态=%v，期望 Error", got.Status().Code)
		}
		if !strings.Contains(got.Status().Description, "tx boom") {
			t.Fatalf("状态描述=%q，期望包含 tx boom", got.Status().Description)
		}
		if len(got.Events()) == 0 {
			t.Fatal("RecordError 应产生一条 exception 事件")
		}
	})
}

// 真 Redis 差分对拍（dbcache_p1_test.go 的 TestRedisParity*）运行时注入的环境
// 变量名；值只在进程内存中，绝不写入仓库任何文件。事务用例本身已转纯 Redis
// （fakeStore），不再依赖这些变量，也不再需要 MySQL。
const (
	txEnvRedisAddr     = "TASKENGINE_TEST_REDIS_ADDR"
	txEnvRedisPassword = "TASKENGINE_TEST_REDIS_PASSWORD"
	txEnvRedisDB       = "TASKENGINE_TEST_REDIS_DB"
)

const (
	// txUserTable / txOrderTable 是本文件独占的表名。
	txUserTable  = "tx_case_user"
	txOrderTable = "tx_case_order"
	// txTestKeyPrefix 是本文件独占的 Redis 键前缀。
	txTestKeyPrefix = "txcase20261109"
)

// txCaseUser 单列行键（id）模型；cache tag 定义列名，KeyColumns 显式指定行键。
type txCaseUser struct {
	ID    int64  `cache:"id"`
	Name  string `cache:"name"`
	Score int64  `cache:"score"`
	State string `cache:"state"`
}

// txCaseOrder 复合行键（user_id + seq）模型，用于完整/不完整行键断言。
type txCaseOrder struct {
	UserID int64  `cache:"user_id"`
	Seq    int64  `cache:"seq"`
	Amount int64  `cache:"amount"`
	State  string `cache:"state"`
}

// 事务条件结构体：指针字段参与条件，nil 字段跳过（parseCond 语义）。
// idCond / nameCond / ptrInt / ptrStr 复用本文件既有夹具。
type txUserStateCond struct {
	State *string `cache:"state"`
}

type txUserIDsCond struct {
	IDs *[]int64 `cache:"id"`
}

type txOrderKeyCond struct {
	UserID *int64 `cache:"user_id"`
	Seq    *int64 `cache:"seq"`
}

// setupTxStorage 构造纯 Redis（fakeStore）事务夹具：绑定两张独占表并安装为
// defaultEngine，用例结束还原。无 MySQL 依赖——Redis 即真源，sink 缺席；
// 行锁等待钳到 200ms 让竞争用例快速失败，不拖慢套件。
func setupTxStorage(t *testing.T) (*Engine, *fakeStore) {
	t.Helper()
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = txTestKeyPrefix
	cfg.RequireAOF = false
	cfg.TxLockWaitMS = 200
	e := newEngine(cfg, fs)

	userKeys, err := makeTableKeys(e.cfg.KeyPrefix, txUserTable, false)
	if err != nil {
		t.Fatalf("makeTableKeys(%s): %v", txUserTable, err)
	}
	userBinding, err := newBinding[txCaseUser](txUserTable, backendMySQL,
		BindOption[txCaseUser]{KeyColumns: []string{"id"}}, userKeys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding(%s): %v", txUserTable, err)
	}
	if err := e.register(userBinding); err != nil {
		t.Fatalf("register(%s): %v", txUserTable, err)
	}

	orderKeys, err := makeTableKeys(e.cfg.KeyPrefix, txOrderTable, false)
	if err != nil {
		t.Fatalf("makeTableKeys(%s): %v", txOrderTable, err)
	}
	orderBinding, err := newBinding[txCaseOrder](txOrderTable, backendMySQL,
		BindOption[txCaseOrder]{KeyColumns: []string{"user_id", "seq"}}, orderKeys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding(%s): %v", txOrderTable, err)
	}
	if err := e.register(orderBinding); err != nil {
		t.Fatalf("register(%s): %v", txOrderTable, err)
	}

	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	return e, fs
}

// txStore 取夹具的 fakeStore（断言 Redis 真值用）。
func txStore(t *testing.T) *fakeStore {
	t.Helper()
	fs, ok := Default().store.(*fakeStore)
	if !ok {
		t.Fatalf("夹具 store 不是 fakeStore: %T", Default().store)
	}
	return fs
}

// txKeys 生成一张表的 Redis 键集合（与夹具 KeyPrefix 一致）。
func txKeys(t *testing.T, table string) tableKeys {
	t.Helper()
	keys, err := makeTableKeys(txTestKeyPrefix, table, false)
	if err != nil {
		t.Fatalf("makeTableKeys(%q): %v", table, err)
	}
	return keys
}

// txSeedUsers 经非事务 Save 把初始行写进 Redis（权威真源）。
func txSeedUsers(t *testing.T, rows ...*txCaseUser) {
	t.Helper()
	for _, row := range rows {
		if err := Save[txCaseUser](context.Background(), txUserTable, "", row, false); err != nil {
			t.Fatalf("seed %s %+v: %v", txUserTable, row, err)
		}
	}
}

// txSeedOrders 经非事务 Save 把初始订单行写进 Redis。
func txSeedOrders(t *testing.T, rows ...*txCaseOrder) {
	t.Helper()
	for _, row := range rows {
		if err := Save[txCaseOrder](context.Background(), txOrderTable, "", row, false); err != nil {
			t.Fatalf("seed %s %+v: %v", txOrderTable, row, err)
		}
	}
}

// txRowHash 直读 fakeStore 行 hash。
func txRowHash(t *testing.T, keys tableKeys, key string) map[string]string {
	t.Helper()
	return txStore(t).hash(keys.rowPrefix + key)
}

// txTombExists 判断删除墓碑是否仍生效。
func txTombExists(t *testing.T, keys tableKeys, key string) bool {
	t.Helper()
	return txStore(t).hasTomb(keys.tombPrefix + key)
}

// txAssertUserHash 断言 Redis 行 hash 的 name / state / score 三列。
func txAssertUserHash(t *testing.T, key, name, state string, score int64) {
	t.Helper()
	h := txRowHash(t, txKeys(t, txUserTable), key)
	if h["name"] != name || h["state"] != state || h["score"] != strconv.FormatInt(score, 10) {
		t.Fatalf("Redis 行 %s hash 不符: 期望 name=%q state=%q score=%d，实际 %v",
			key, name, state, score, h)
	}
}

// txLiveKeys 返回表当前存活行键（Redis 键集，升序），用于行数断言。
func txLiveKeys(t *testing.T, table string) []string {
	t.Helper()
	b, err := Default().lookupTable(table)
	if err != nil {
		t.Fatalf("lookupTable(%s): %v", table, err)
	}
	keys, err := txStore(t).KeySetScan(context.Background(), b.keys.keySet, 0)
	if err != nil {
		t.Fatalf("KeySetScan(%s): %v", table, err)
	}
	return keys
}

// txInTxOption 与 db.InTx 等价：门面只把 fn 放进 Options.InTx，CacheSave 检测后
// 转调 RunTx（db/cache_api.go 的 InTx / CacheSave）。dbcache 内部测试不能 import
// touchgocore/db（db → dbcache → db 会形成测试期 import cycle），故在本包内以
// 完全相同的 Option 形状 + RunTx 调用覆盖门面的运行时路径。
func txInTxOption(fn func(context.Context) error) Option {
	return func(o *Options) { o.InTx = fn }
}

// TestTxBeginTxSaveCommitWritesRedis 覆盖 BeginTx → TxSave → Commit：提交前
// overlay 不落 Redis（读己写隔离），Commit 经 txScript 一次原子写穿 Redis。
func TestTxBeginTxSaveCommitWritesRedis(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txAssertUserHash(t, "1", "before", "new", 1)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tx.Context() == nil {
		t.Fatal("BeginTx 应返回可用的 Context")
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable, nil); !errors.Is(err, ErrNilRow) {
		t.Fatalf("TxSave(nil) 期望 ErrNilRow，实际 %v", err)
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	// 提交前 Redis 仍是旧值：写只在私有 overlay 上求值。
	txAssertUserHash(t, "1", "before", "new", 1)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	txAssertUserHash(t, "1", "after", "paid", 9)
}

// TestTxCommitReleasesLockAndPersists 覆盖提交后行锁释放、后续事务读到已提交新值
// （取代旧 WrapTx 用例：外部 gorm 句柄逃生口已删除）。
func TestTxCommitReleasesLockAndPersists(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 2, State: "new"})

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "first", Score: 8, State: "done"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx2, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx(2): %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	var got txCaseUser
	if err := TxLockOne[txCaseUser](tx2, ctx, txUserTable, &idCond{ID: ptrInt(1)}, &got); err != nil {
		t.Fatalf("提交后行锁应已释放，TxLockOne: %v", err)
	}
	if got.Name != "first" || got.Score != 8 || got.State != "done" {
		t.Fatalf("第二个事务应读到已提交新值，实际 %+v", got)
	}
}

// TestTxRollbackDiscardsOverlay 覆盖 BeginTx → TxSave → Rollback：overlay 被
// 丢弃，Redis 旧行不变、新插入行不存在、键集不增。
func TestTxRollbackDiscardsOverlay(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	keys := txKeys(t, txUserTable)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}); err != nil {
		t.Fatalf("TxSave(更新): %v", err)
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 2, Name: "brandnew", Score: 5, State: "new"}); err != nil {
		t.Fatalf("TxSave(插入): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	txAssertUserHash(t, "1", "before", "new", 1)
	if h := txRowHash(t, keys, "2"); len(h) != 0 {
		t.Fatalf("回滚后不应写入新插入行，实际 %v", h)
	}
	if live := txLiveKeys(t, txUserTable); len(live) != 1 || live[0] != "1" {
		t.Fatalf("回滚后键集应只剩原本的 1 行，实际 %v", live)
	}
}

// TestTxQueryReadsOverlayNotRedis 覆盖 TxQuery 的 *T 与 *[]T 两种形态：读己写
// （overlay 叠加 Redis 真值），提交前 Redis 不变，回滚后 Redis 保持原样。
func TestTxQueryReadsOverlayNotRedis(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t,
		&txCaseUser{ID: 1, Name: "one", Score: 1, State: "new"},
		&txCaseUser{ID: 2, Name: "two", Score: 2, State: "new"})

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "uncommitted", Score: 1, State: "new"}); err != nil {
		t.Fatalf("TxSave(事务内更新): %v", err)
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 3, Name: "third", Score: 3, State: "new"}); err != nil {
		t.Fatalf("TxSave(事务内插入): %v", err)
	}

	var one txCaseUser
	if err := TxQuery[txCaseUser](tx, ctx, txUserTable, &idCond{ID: ptrInt(1)}, &one); err != nil {
		t.Fatalf("TxQuery(*T): %v", err)
	}
	if one.Name != "uncommitted" {
		t.Fatalf("TxQuery 单条应读到事务内未提交的新值，实际 %+v", one)
	}
	var list []txCaseUser
	if err := TxQuery[txCaseUser](tx, ctx, txUserTable,
		&txUserStateCond{State: ptrStr("new")}, &list); err != nil {
		t.Fatalf("TxQuery(*[]T): %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("TxQuery 列表应看到事务内 3 行（含未提交插入），实际 %d 行 %+v", len(list), list)
	}
	if err := TxQuery[txCaseUser](tx, ctx, txUserTable,
		&idCond{ID: ptrInt(1)}, &struct{}{}); !errors.Is(err, ErrBadOut) {
		t.Fatalf("TxQuery(错误 out) 期望 ErrBadOut，实际 %v", err)
	}

	// 事务内的写尚未提交，Redis 仍应是旧值。
	txAssertUserHash(t, "1", "one", "new", 1)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if live := txLiveKeys(t, txUserTable); len(live) != 2 {
		t.Fatalf("回滚后 Redis 应保持 2 行，实际 %v", live)
	}
}

// TestTxLockOneExactKeyPartialKeyAndRowLock 覆盖 TxLockOne：完整行键可用、
// 非完整行键 / 多行 IN 条件被 ErrPartialKey 拒绝，并经 Redis 行锁验证竞争：
// 第二个事务抢同一行锁等待超时 ErrTxConflict，释放后第三个事务成功。
func TestTxLockOneExactKeyPartialKeyAndRowLock(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedOrders(t,
		&txCaseOrder{UserID: 1, Seq: 1, Amount: 50, State: "new"},
		&txCaseOrder{UserID: 1, Seq: 2, Amount: 70, State: "new"})

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var got txCaseOrder
	if err := TxLockOne[txCaseOrder](tx, ctx, txOrderTable,
		&txOrderKeyCond{UserID: ptrInt(1), Seq: ptrInt(1)}, &got); err != nil {
		t.Fatalf("TxLockOne(完整行键): %v", err)
	}
	if got.Amount != 50 || got.UserID != 1 || got.Seq != 1 {
		t.Fatalf("TxLockOne 返回行不符: %+v", got)
	}
	if err := TxLockOne[txCaseOrder](tx, ctx, txOrderTable,
		&txOrderKeyCond{UserID: ptrInt(1)}, &got); !errors.Is(err, ErrPartialKey) {
		t.Fatalf("TxLockOne(复合键缺一列) 期望 ErrPartialKey，实际 %v", err)
	}
	var user txCaseUser
	if err := TxLockOne[txCaseUser](tx, ctx, txUserTable,
		&nameCond{Name: ptrStr("x")}, &user); !errors.Is(err, ErrPartialKey) {
		t.Fatalf("TxLockOne(非键列) 期望 ErrPartialKey，实际 %v", err)
	}
	ids := []int64{1, 2}
	if err := TxLockOne[txCaseUser](tx, ctx, txUserTable,
		&txUserIDsCond{IDs: &ids}, &user); !errors.Is(err, ErrPartialKey) {
		t.Fatalf("TxLockOne(键列 IN 多值) 期望 ErrPartialKey，实际 %v", err)
	}

	// 行锁竞争：第一个事务已持 order(1,1) 行锁，第二个事务抢同一行须超时失败。
	other, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("第二个事务 BeginTx: %v", err)
	}
	var blocked txCaseOrder
	if err := TxLockOne[txCaseOrder](other, ctx, txOrderTable,
		&txOrderKeyCond{UserID: ptrInt(1), Seq: ptrInt(1)}, &blocked); !errors.Is(err, ErrTxConflict) {
		_ = other.Rollback()
		t.Fatalf("第一个事务已持行锁，第二个事务应 ErrTxConflict，实际 %v", err)
	}
	if err := other.Rollback(); err != nil {
		t.Fatalf("第二个事务 Rollback: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	released, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("释放锁后 BeginTx: %v", err)
	}
	defer func() { _ = released.Rollback() }()
	if err := TxLockOne[txCaseOrder](released, ctx, txOrderTable,
		&txOrderKeyCond{UserID: ptrInt(1), Seq: ptrInt(1)}, &blocked); err != nil {
		t.Fatalf("释放行锁后 TxLockOne 应成功，实际 %v", err)
	}
}

// TestTxIncrementAndCompareAndSwap 覆盖事务内原子累加 / CAS：提交后生效于 Redis，
// 且 condIsExactKey 拒绝非键列 / 多行条件，nil 条件拒绝。
func TestTxIncrementAndCompareAndSwap(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "alice", Score: 10, State: "new"})

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := TxIncrement(tx, ctx, txUserTable, &idCond{ID: ptrInt(1)}, "score", 5)
	if err != nil {
		t.Fatalf("TxIncrement: %v", err)
	}
	if n != 1 {
		t.Fatalf("TxIncrement 期望影响 1 行，实际 %d", n)
	}
	if _, err := TxIncrement(tx, ctx, txUserTable,
		&nameCond{Name: ptrStr("alice")}, "score", 1); !errors.Is(err, ErrPartialKey) {
		t.Fatalf("TxIncrement(非键列/全表风险) 期望 ErrPartialKey，实际 %v", err)
	}
	ids := []int64{1, 2}
	if _, err := TxIncrement(tx, ctx, txUserTable,
		&txUserIDsCond{IDs: &ids}, "score", 1); !errors.Is(err, ErrPartialKey) {
		t.Fatalf("TxIncrement(键列 IN 多值) 期望 ErrPartialKey，实际 %v", err)
	}
	n, err = TxCompareAndSwap(tx, ctx, txUserTable, &idCond{ID: ptrInt(1)},
		map[string]any{"state": "new"}, map[string]any{"state": "paid"})
	if err != nil {
		t.Fatalf("TxCompareAndSwap: %v", err)
	}
	if n != 1 {
		t.Fatalf("TxCompareAndSwap 期望影响 1 行，实际 %d", n)
	}
	if _, err := TxCompareAndSwap(tx, ctx, txUserTable,
		&nameCond{Name: ptrStr("alice")}, nil, map[string]any{"state": "x"}); !errors.Is(err, ErrPartialKey) {
		t.Fatalf("TxCompareAndSwap(非键列) 期望 ErrPartialKey，实际 %v", err)
	}
	if _, err := TxCompareAndSwap(tx, ctx, txUserTable, nil,
		nil, map[string]any{"state": "x"}); !errors.Is(err, ErrEmptyCondition) {
		t.Fatalf("TxCompareAndSwap(nil 条件) 期望 ErrEmptyCondition，实际 %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	txAssertUserHash(t, "1", "alice", "paid", 15)
}

// TestTxCASKeyFromStructuredCondition 钉住「结构化条件可推导行键」：消费方
// （whatapp Tx.Update）经门面 CacheSave+CASSet 传 db.Condition("id","=",x) 定位行，
// 不传 MatchStruct。db.Condition 文档承诺「写路径用作定位条件」，故 condVals 在
// o.Cond 缺席时须从 Conditions 的键列等值推导行键，而非退回 ErrPartialKey。
func TestTxCASKeyFromStructuredCondition(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "alice", Score: 10, State: "new"})
	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 键列等值 + 非键列守卫（id=1 AND state<>paid）：当前 new<>paid 命中 ⇒ 写 1 行。
	var n int64
	err = Save[struct{}](tx.Context(), txUserTable, "", nil, false,
		func(o *Options) { o.CASSet = map[string]any{"state": "paid"} },
		func(o *Options) {
			o.Conditions = append(o.Conditions,
				Range{Column: "id", Op: "=", Value: int64(1)},
				Range{Column: "state", Op: "<>", Value: "paid"})
		},
		func(o *Options) { o.Affected = &n })
	if err != nil {
		t.Fatalf("CAS via Condition 应成功，实际 %v", err)
	}
	if n != 1 {
		t.Fatalf("首次期望影响 1 行，实际 %d", n)
	}

	// 读己写 + 守卫：state 已 paid，state<>paid 不命中 ⇒ 0 行（幂等空转）。
	n = 0
	err = Save[struct{}](tx.Context(), txUserTable, "", nil, false,
		func(o *Options) { o.CASSet = map[string]any{"state": "x"} },
		func(o *Options) {
			o.Conditions = append(o.Conditions,
				Range{Column: "id", Op: "=", Value: int64(1)},
				Range{Column: "state", Op: "<>", Value: "paid"})
		},
		func(o *Options) { o.Affected = &n })
	if err != nil {
		t.Fatalf("CAS guard 二次: %v", err)
	}
	if n != 0 {
		t.Fatalf("守卫不命中期望 0 行，实际 %d", n)
	}

	// 只有非键列条件（name=alice）推不出行键 ⇒ ErrPartialKey（fail-closed）。
	err = Save[struct{}](tx.Context(), txUserTable, "", nil, false,
		func(o *Options) { o.CASSet = map[string]any{"state": "y"} },
		func(o *Options) {
			o.Conditions = append(o.Conditions, Range{Column: "name", Op: "=", Value: "alice"})
		})
	if !errors.Is(err, ErrPartialKey) {
		t.Fatalf("非键列条件期望 ErrPartialKey，实际 %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	txAssertUserHash(t, "1", "alice", "paid", 10)
}

// TestTxUpdateInAndDeleteInCommit 覆盖批量更新 / 批量删除提交后生效于 Redis：
// 更新行改写、被删行 hash 清掉并立墓碑、键集收敛。
func TestTxUpdateInAndDeleteInCommit(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t,
		&txCaseUser{ID: 1, Name: "one", Score: 1, State: "new"},
		&txCaseUser{ID: 2, Name: "two", Score: 2, State: "new"},
		&txCaseUser{ID: 3, Name: "three", Score: 3, State: "new"})
	keys := txKeys(t, txUserTable)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := TxUpdateIn(tx, ctx, txUserTable, "id", []any{int64(1), int64(2)},
		map[string]any{"state": "new"}, map[string]any{"state": "done"})
	if err != nil {
		t.Fatalf("TxUpdateIn: %v", err)
	}
	if n != 2 {
		t.Fatalf("TxUpdateIn 期望影响 2 行，实际 %d", n)
	}
	n, err = TxDeleteIn(tx, ctx, txUserTable, "id", []any{int64(3)})
	if err != nil {
		t.Fatalf("TxDeleteIn: %v", err)
	}
	if n != 1 {
		t.Fatalf("TxDeleteIn 期望影响 1 行，实际 %d", n)
	}
	if _, err := TxDeleteIn(tx, ctx, txUserTable, "not_a_column", []any{int64(1)}); !errors.Is(err, ErrBadColumn) {
		t.Fatalf("TxDeleteIn(非法列) 期望 ErrBadColumn，实际 %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	txAssertUserHash(t, "1", "one", "done", 1)
	txAssertUserHash(t, "2", "two", "done", 2)
	if h := txRowHash(t, keys, "3"); len(h) != 0 {
		t.Fatalf("删除提交后行 3 的 hash 应被清掉，实际 %v", h)
	}
	if !txTombExists(t, keys, "3") {
		t.Fatal("删除提交后应留下行 3 的墓碑")
	}
	if live := txLiveKeys(t, txUserTable); len(live) != 2 {
		t.Fatalf("提交后键集应只剩 2 行，实际 %v", live)
	}
}

// TestTxUpdateInAndDeleteInRollback 覆盖批量写「事务内读己写生效、回滚后 Redis
// 分毫不动」：事务内看到 done×2 / new×0，回滚后三行仍是 new、无墓碑。
func TestTxUpdateInAndDeleteInRollback(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t,
		&txCaseUser{ID: 1, Name: "one", Score: 1, State: "new"},
		&txCaseUser{ID: 2, Name: "two", Score: 2, State: "new"},
		&txCaseUser{ID: 3, Name: "three", Score: 3, State: "new"})
	keys := txKeys(t, txUserTable)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := TxUpdateIn(tx, ctx, txUserTable, "id", []any{int64(1), int64(2)},
		map[string]any{"state": "new"}, map[string]any{"state": "done"}); err != nil {
		t.Fatalf("TxUpdateIn: %v", err)
	}
	if _, err := TxDeleteIn(tx, ctx, txUserTable, "id", []any{int64(3)}); err != nil {
		t.Fatalf("TxDeleteIn: %v", err)
	}
	var done []txCaseUser
	if err := TxQuery[txCaseUser](tx, ctx, txUserTable,
		&txUserStateCond{State: ptrStr("done")}, &done); err != nil {
		t.Fatalf("TxQuery(事务内批量结果): %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("事务内应看到 2 行 done，实际 %d 行 %+v", len(done), done)
	}
	var remaining []txCaseUser
	if err := TxQuery[txCaseUser](tx, ctx, txUserTable,
		&txUserStateCond{State: ptrStr("new")}, &remaining); err != nil {
		t.Fatalf("TxQuery(事务内剩余行): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("事务内第 3 行应已删除、1/2 行应已 done，实际 %+v", remaining)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	txAssertUserHash(t, "1", "one", "new", 1)
	txAssertUserHash(t, "2", "two", "new", 2)
	txAssertUserHash(t, "3", "three", "new", 3)
	if h := txRowHash(t, keys, "3"); h["name"] != "three" {
		t.Fatalf("回滚后不应清掉行 3 的 hash，实际 %v", h)
	}
	if txTombExists(t, keys, "3") {
		t.Fatal("回滚后不应给行 3 立墓碑")
	}
	if live := txLiveKeys(t, txUserTable); len(live) != 3 {
		t.Fatalf("回滚后键集应仍有 3 行，实际 %v", live)
	}
}

// TestTxInsertAndInsertBatch 覆盖 TxInsert / TxInsertBatch：提交后插入行写穿
// Redis，冲突行 affected=0 不覆盖，回滚后新行不存在、键集不增。
func TestTxInsertAndInsertBatch(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "original", Score: 1, State: "new"})
	keys := txKeys(t, txUserTable)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := TxInsert[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 2, Name: "ins2", Score: 2, State: "new"})
	if err != nil {
		t.Fatalf("TxInsert(新行): %v", err)
	}
	if n != 1 {
		t.Fatalf("TxInsert 新行期望 affected=1，实际 %d", n)
	}
	n, err = TxInsert[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "overwrite", Score: 9, State: "paid"})
	if err != nil {
		t.Fatalf("TxInsert(冲突行): %v", err)
	}
	if n != 0 {
		t.Fatalf("TxInsert 冲突行期望 affected=0，实际 %d", n)
	}
	n, err = TxInsertBatch[txCaseUser](tx, ctx, txUserTable, []*txCaseUser{
		{ID: 3, Name: "ins3", Score: 3, State: "new"},
		{ID: 4, Name: "ins4", Score: 4, State: "new"},
	})
	if err != nil {
		t.Fatalf("TxInsertBatch: %v", err)
	}
	if n != 2 {
		t.Fatalf("TxInsertBatch 期望 affected=2，实际 %d", n)
	}
	if _, err := TxInsert[txCaseUser](tx, ctx, txUserTable, nil); !errors.Is(err, ErrNilRow) {
		t.Fatalf("TxInsert(nil) 期望 ErrNilRow，实际 %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	txAssertUserHash(t, "1", "original", "new", 1)
	txAssertUserHash(t, "2", "ins2", "new", 2)
	txAssertUserHash(t, "3", "ins3", "new", 3)
	txAssertUserHash(t, "4", "ins4", "new", 4)
	if live := txLiveKeys(t, txUserTable); len(live) != 4 {
		t.Fatalf("提交后键集应有 4 行，实际 %v", live)
	}

	// 回滚路径：新行不写穿 Redis、键集不增。
	tx2, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx(回滚用例): %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	if _, err := TxInsert[txCaseUser](tx2, ctx, txUserTable,
		&txCaseUser{ID: 5, Name: "ins5", Score: 5, State: "new"}); err != nil {
		t.Fatalf("TxInsert(回滚用例): %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if h := txRowHash(t, keys, "5"); len(h) != 0 {
		t.Fatalf("回滚后不应写入 id=5，实际 %v", h)
	}
	if live := txLiveKeys(t, txUserTable); len(live) != 4 {
		t.Fatalf("回滚后键集应仍有 4 行，实际 %v", live)
	}
}

// TestRunTxFacadeCommit 覆盖 db.InTx → RunTx 门面路径：用与门面完全一致的
// Option 形状，fn 内经事务 ctx 走 Query(ForUpdate) / Save(整行) / Save(原子累加)，
// 提交后三者生效于 Redis。
func TestRunTxFacadeCommit(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})

	fn := func(ctx context.Context) error {
		var cur txCaseUser
		if err := Query[txCaseUser](ctx, txUserTable, &idCond{ID: ptrInt(1)}, &cur,
			func(o *Options) { o.ForUpdate = true }); err != nil {
			return fmt.Errorf("事务内 ForUpdate 查询: %w", err)
		}
		if cur.ID != 1 {
			return fmt.Errorf("事务内 ForUpdate 查询返回错误行: %+v", cur)
		}
		if err := Save[txCaseUser](ctx, txUserTable, "1",
			&txCaseUser{ID: 1, Name: "facade", Score: 2, State: "paid"}, true); err != nil {
			return fmt.Errorf("事务内整行写: %w", err)
		}
		if err := Save[txCaseUser](ctx, txUserTable, "1", nil, true,
			func(o *Options) { o.Increment = "score"; o.Delta = 5 }); err != nil {
			return fmt.Errorf("事务内原子累加: %w", err)
		}
		return nil
	}
	// db.CacheSave 的派发逻辑：ParseOptions 取出 InTx，再交给 RunTx。
	opts := ParseOptions([]Option{txInTxOption(fn)})
	if opts.InTx == nil {
		t.Fatal("Options.InTx 未被门面选项填入")
	}
	if err := RunTx(ctx, opts.InTx); err != nil {
		t.Fatalf("RunTx: %v", err)
	}

	txAssertUserHash(t, "1", "facade", "paid", 7)
}

// TestRunTxFacadeErrorRollsBack 覆盖门面 fn 返回错误：事务回滚、Redis 不变。
func TestRunTxFacadeErrorRollsBack(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})

	want := errors.New("facade fn failed")
	fn := func(ctx context.Context) error {
		if err := Save[txCaseUser](ctx, txUserTable, "1",
			&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}, true); err != nil {
			return err
		}
		return want
	}
	opts := ParseOptions([]Option{txInTxOption(fn)})
	if err := RunTx(ctx, opts.InTx); !errors.Is(err, want) {
		t.Fatalf("RunTx 应原样返回 fn 错误，实际 %v", err)
	}

	txAssertUserHash(t, "1", "before", "new", 1)
}

// TestRunTxFacadePanicRollsBack 覆盖门面 panic 路径：RunTx 回滚后重新 panic，
// Redis 不变。
func TestRunTxFacadePanicRollsBack(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})

	fn := func(ctx context.Context) error {
		if err := Save[txCaseUser](ctx, txUserTable, "1",
			&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}, true); err != nil {
			return err
		}
		panic("facade panic")
	}
	opts := ParseOptions([]Option{txInTxOption(fn)})
	func() {
		defer func() {
			if got := recover(); got != "facade panic" {
				t.Fatalf("RunTx 应把 panic 原样抛出，实际 %v", got)
			}
		}()
		_ = RunTx(ctx, opts.InTx)
	}()

	txAssertUserHash(t, "1", "before", "new", 1)
}

// TestTxCommitStoreFailureLeavesRedisUntouched 覆盖 Redis 权威下的提交失败语义
// （取代旧「提交成功但刷新失败」用例）：Commit 即写穿 Redis，store 故障时
// fail-closed 返回错误、Redis 分毫不动、不立墓碑，且行锁仍被释放。
func TestTxCommitStoreFailureLeavesRedisUntouched(t *testing.T) {
	_, fs := setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	keys := txKeys(t, txUserTable)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	// 注入 Redis 写故障：提交时 applyTx 失败（overlay 已求值，但未落 Redis）。
	fs.hsetErr = errors.New("redis down")
	if err := tx.Commit(); err == nil {
		t.Fatal("Redis 写失败时 Commit 必须 fail-closed 返回错误")
	}
	fs.hsetErr = nil

	txAssertUserHash(t, "1", "before", "new", 1)
	if txTombExists(t, keys, "1") {
		t.Fatal("提交失败不应立墓碑")
	}
	// 行锁已释放：后续事务可取得锁并读到旧值。
	tx2, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx(2): %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	var got txCaseUser
	if err := TxLockOne[txCaseUser](tx2, ctx, txUserTable, &idCond{ID: ptrInt(1)}, &got); err != nil {
		t.Fatalf("提交失败后行锁应已释放，TxLockOne: %v", err)
	}
	if got.Name != "before" || got.Score != 1 || got.State != "new" {
		t.Fatalf("提交失败后应读到未改动的旧值，实际 %+v", got)
	}
}

// TestTxInsertAutoIncrementBackfill 覆盖 F2：事务内自增列零值即时 SeqAlloc 取号
// 并回填调用方结构体（提交前即可见），序列非事务性即时推进；行本身仍走 overlay，
// 提交前不落 Redis，回滚后新行不写穿但已取号码不回退（标准序列语义，允许空洞）。
func TestTxInsertAutoIncrementBackfill(t *testing.T) {
	e, fs, b, _ := newP1Engine(t)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	ctx := context.Background()

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	r1 := &p1Row{Uname: "tx_a"}
	r2 := &p1Row{Uname: "tx_b"}
	n1, err := TxInsert[p1Row](tx, ctx, "p1_row", r1)
	if err != nil {
		t.Fatalf("TxInsert(1): %v", err)
	}
	n2, err := TxInsert[p1Row](tx, ctx, "p1_row", r2)
	if err != nil {
		t.Fatalf("TxInsert(2): %v", err)
	}
	if n1 != 1 || n2 != 1 {
		t.Fatalf("两新行 affected 应为 1，实际 %d / %d", n1, n2)
	}
	if r1.ID != 1 || r2.ID != 2 {
		t.Fatalf("F2：事务内自增 ID 应即时回填结构体，r1=%d r2=%d", r1.ID, r2.ID)
	}
	if fs.m.strs[b.keys.seq] != "2" {
		t.Fatalf("序列应即时推进到 2，实际 %q", fs.m.strs[b.keys.seq])
	}
	if h := fs.hash(b.keys.rowPrefix + "1"); len(h) != 0 {
		t.Fatalf("提交前行不应落 Redis（overlay 隔离），实际 %v", h)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if h := fs.hash(b.keys.rowPrefix + "1"); h["uname"] != "tx_a" {
		t.Fatalf("提交后行 1 应写穿 Redis，实际 %v", h)
	}
	if h := fs.hash(b.keys.rowPrefix + "2"); h["uname"] != "tx_b" {
		t.Fatalf("提交后行 2 应写穿 Redis，实际 %v", h)
	}
	if _, ok := fs.m.sets[b.keys.keySet]["1"]; !ok {
		t.Fatal("提交后行 1 应进键集")
	}
	if _, ok := fs.m.sets[b.keys.keySet]["2"]; !ok {
		t.Fatal("提交后行 2 应进键集")
	}

	// 回滚路径：取号即时回填且序列不回退，但行不写穿 Redis。
	tx2, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx(2): %v", err)
	}
	r3 := &p1Row{Uname: "tx_c"}
	if _, err := TxInsert[p1Row](tx2, ctx, "p1_row", r3); err != nil {
		t.Fatalf("TxInsert(回滚用例): %v", err)
	}
	if r3.ID != 3 {
		t.Fatalf("回滚用例取号应即时回填 id=3，实际 %d", r3.ID)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if fs.m.strs[b.keys.seq] != "3" {
		t.Fatalf("序列非事务性：回滚后仍应为 3（允许空洞），实际 %q", fs.m.strs[b.keys.seq])
	}
	if h := fs.hash(b.keys.rowPrefix + "3"); len(h) != 0 {
		t.Fatalf("回滚后 id=3 不应写穿 Redis，实际 %v", h)
	}
	if _, ok := fs.m.sets[b.keys.keySet]["3"]; ok {
		t.Fatal("回滚后 id=3 不应进键集")
	}
}
