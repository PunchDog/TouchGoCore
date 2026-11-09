package dbcache

import (
	"context"
	"errors"
	"fmt"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"touchgocore/config"
	mysqldb "touchgocore/db/mysql"
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

// TestDeleteRemovesCacheAndRow 删除后行 hash 与脏账均清空，再查必须回源。
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
	if src.delCalls != 1 {
		t.Fatalf("数据库删除调用次数=%d", src.delCalls)
	}
	if h := fs.hash(rowKey); len(h) != 0 {
		t.Fatalf("删除后行 hash 应为空，实际 %v", h)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("删除后脏账应为空，实际 %d", n)
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

// TestDeleteDBFailureKeepsCache 数据库删除失败时不得清缓存，墓碑需回滚。
func TestDeleteDBFailureKeepsCache(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "keep"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	src.delErr = errSinkDown

	err := e.remove(ctx, b, &idCond{ID: ptrInt(7)})
	if !errors.Is(err, errSinkDown) {
		t.Fatalf("应返回数据库删除错误，实际 %v", err)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); len(h) == 0 {
		t.Fatal("数据库删除失败时行 hash 应保留")
	}
	if fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("数据库删除失败时墓碑应回滚")
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
type fakeStore struct {
	mu        sync.Mutex
	hashes    map[string]map[string]string
	dirty     map[string]map[string]int64 // dirtyKey → member → score
	retry     map[string]map[string]int64
	tomb      map[string]int64 // tombKey → 过期时刻(UnixMilli)，0 表示不过期
	ttl       map[string]time.Duration
	backfills int
	hsetErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		hashes: make(map[string]map[string]string),
		dirty:  make(map[string]map[string]int64),
		retry:  make(map[string]map[string]int64),
		tomb:   make(map[string]int64),
		ttl:    make(map[string]time.Duration),
	}
}

func (s *fakeStore) HGetAll(_ context.Context, key string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMap(s.hashes[key]), nil
}

func (s *fakeStore) HSetAll(_ context.Context, rowKey, tombKey string, fields []string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hsetErr != nil {
		return s.hsetErr
	}
	// 重建该行前先清墓碑，与 redisStore 的 pipeline 语义一致。
	delete(s.tomb, tombKey)
	h := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		h[fields[i]] = fields[i+1]
	}
	s.hashes[rowKey] = h
	if ttl > 0 {
		s.ttl[rowKey] = ttl
	}
	return nil
}

func (s *fakeStore) BackfillAndGet(_ context.Context, key string, fields []string, ttl time.Duration) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hashes[key]; len(h) > 0 {
		return copyMap(h), nil
	}
	s.backfills++
	h := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		h[fields[i]] = fields[i+1]
	}
	s.hashes[key] = h
	if ttl > 0 {
		s.ttl[key] = ttl
	}
	return copyMap(h), nil
}

func (s *fakeStore) ZAdd(_ context.Context, key, member string, score int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.dirty[key]
	if m == nil {
		m = make(map[string]int64)
		s.dirty[key] = m
	}
	m[member] = score
	return nil
}

func (s *fakeStore) DueMembers(_ context.Context, key string, maxScore int64, count int) ([]dirtyMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []dirtyMember
	for member, score := range s.dirty[key] {
		if score <= maxScore {
			out = append(out, dirtyMember{Member: member, Score: score})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].Member < out[j].Member
	})
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out, nil
}

func (s *fakeStore) Claim(_ context.Context, dirtyKey, rowKey, tombKey, member string, score int64) (map[string]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tombAlive(tombKey) {
		delete(s.dirty[dirtyKey], member)
		return nil, false, nil
	}
	cur, ok := s.dirty[dirtyKey][member]
	if !ok || cur != score {
		return nil, false, nil
	}
	delete(s.dirty[dirtyKey], member)
	h := copyMap(s.hashes[rowKey])
	if len(h) == 0 {
		return nil, false, nil
	}
	return h, true, nil
}

func (s *fakeStore) ZRem(_ context.Context, key, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dirty[key], member)
	return nil
}

func (s *fakeStore) ZScore(_ context.Context, key, member string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	score, ok := s.dirty[key][member]
	return score, ok, nil
}

func (s *fakeStore) HDel(_ context.Context, key, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.retry[key], member)
	return nil
}

func (s *fakeStore) HIncrBy(_ context.Context, key, member string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.retry[key]
	if m == nil {
		m = make(map[string]int64)
		s.retry[key] = m
	}
	m[member]++
	return m[member], nil
}

func (s *fakeStore) SetTomb(_ context.Context, tombKey string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := int64(0)
	if ttl > 0 {
		exp = time.Now().Add(ttl).UnixMilli()
	}
	s.tomb[tombKey] = exp
	return nil
}

func (s *fakeStore) DelTomb(_ context.Context, tombKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tomb, tombKey)
	return nil
}

func (s *fakeStore) DelRow(_ context.Context, dirtyKey, rowKey, retryKey, tombKey, member string, tombTTL time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := int64(0)
	if tombTTL > 0 {
		exp = time.Now().Add(tombTTL).UnixMilli()
	}
	s.tomb[tombKey] = exp
	delete(s.dirty[dirtyKey], member)
	delete(s.retry[retryKey], member)
	if _, ok := s.hashes[rowKey]; ok {
		delete(s.hashes, rowKey)
		return 1, nil
	}
	return 0, nil
}

// tombAlive 判断墓碑是否仍生效（调用方需持有 s.mu）。
func (s *fakeStore) tombAlive(tombKey string) bool {
	exp, ok := s.tomb[tombKey]
	if !ok {
		return false
	}
	if exp > 0 && exp <= time.Now().UnixMilli() {
		delete(s.tomb, tombKey)
		return false
	}
	return true
}

// hasTomb 返回墓碑是否仍生效（测试断言用）。
func (s *fakeStore) hasTomb(tombKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tombAlive(tombKey)
}

func (s *fakeStore) Ping(context.Context) error { return nil }

// hash 返回某行当前内容（测试断言用）。
func (s *fakeStore) hash(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMap(s.hashes[key])
}

// dirtyCount 返回脏账成员数。
func (s *fakeStore) dirtyCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.dirty[key])
}

func (s *fakeStore) retryCount(key, member string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retry[key][member]
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
	keys, err := makeTableKeys(e.cfg.KeyPrefix, "test_user")
	if err != nil {
		t.Fatalf("makeTableKeys: %v", err)
	}
	b, err := newBinding[testUser]("test_user", backendMySQL, BindOption[testUser]{}, keys, e.cfg)
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

func TestKeyFromRowZeroValue(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if _, err := keyFromRow(m, reflect.ValueOf(&testUser{}).Elem()); err == nil {
		t.Fatal("主键为 0 应返回 ErrKeyMissing")
	}
	key, err := keyFromRow(m, reflect.ValueOf(&testUser{ID: 42}).Elem())
	if err != nil {
		t.Fatalf("keyFromRow: %v", err)
	}
	if key != "42" {
		t.Fatalf("key=%q，期望 42", key)
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

// exactKeyVals 构造 test_user 的完整行键条件，供 refreshFromDB / scheduleRefresh 使用。
func exactKeyVals() []condVal {
	return []condVal{{Column: "id", Values: []any{int64(7)}}}
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

// TestRefreshFromDBInvalidatesOnHSetFailure 验证写后刷新失败时的三条兜底：
// 旧行 hash 被删、墓碑/脏账/重试账状态正确、下次点查必然回源拿到库里的新值。
func TestRefreshFromDBInvalidatesOnHSetFailure(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	rowKey, tombKey := seedStaleRow(t, fs, b)

	// 库里已是新值；注入 Redis 写失败，走 refreshFromDB 的失败分支。
	src.one = &testUser{ID: 7, Name: "fresh"}
	fs.hsetErr = errors.New("redis hset down")
	e.refreshFromDB(ctx, b, exactKeyVals())

	if h := fs.hash(rowKey); len(h) != 0 {
		t.Fatalf("刷新失败后旧行 hash 必须被删，实际=%v", h)
	}
	if !fs.hasTomb(tombKey) {
		t.Fatal("刷新失败后应立墓碑，阻止在途脏账回写旧值")
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("刷新失败后脏账应销账，实际 %d", n)
	}
	if n := fs.retryCount(b.keys.retry, "7"); n != 0 {
		t.Fatalf("刷新失败后重试计数应清零，实际 %d", n)
	}

	// 解除故障注入：queryOne 点查不得再命中旧行，必须回源读到 fresh。
	fs.hsetErr = nil
	var out testUser
	if err := e.query[testUser](ctx, "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if out.Name != "fresh" {
		t.Fatalf("点查应回源拿到库里的新值，实际 out.Name=%q", out.Name)
	}
	if src.oneCalls != 2 {
		t.Fatalf("refreshFromDB 回源 1 次 + 失效后点查回源 1 次，实际 %d 次", src.oneCalls)
	}
}

// TestInvalidateCachedClearsRowHashAndLedger 验证原子写经 scheduleRefresh 排队的
// 写后刷新失败时，框架会主动失效该行：删旧行 hash、立墓碑、销脏账/重试账，
// 并让下次点查必然回源。
//
// 触发路径刻意走 refreshFromDB 的 HSetAll 失败分支（e.scheduleRefresh →
// refreshFromDB），而不是直接调用 invalidateCached：回退 mutate.go 里对
// invalidateCached 的调用后本用例必须 FAIL，否则说明它没有真正守护修复。
func TestInvalidateCachedClearsRowHashAndLedger(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	rowKey, tombKey := seedStaleRow(t, fs, b)

	// 库里已是新值；注入 Redis 写失败，由原子写的 scheduleRefresh 排队触发刷新。
	src.one = &testUser{ID: 7, Name: "fresh"}
	fs.hsetErr = errors.New("redis hset down")
	e.scheduleRefresh(b, exactKeyVals(), nil)

	// ① 旧行 hash 被删：点查不再命中「stale」。
	if h := fs.hash(rowKey); len(h) != 0 {
		t.Fatalf("刷新失败后旧行 hash 必须被删，实际=%v", h)
	}
	// ③ 墓碑 / 脏账 / 重试账状态正确。
	if !fs.hasTomb(tombKey) {
		t.Fatal("刷新失败后应立墓碑，阻止在途脏账回写旧值")
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("刷新失败后脏账应销账，实际 %d", n)
	}
	if n := fs.retryCount(b.keys.retry, "7"); n != 0 {
		t.Fatalf("刷新失败后重试计数应清零，实际 %d", n)
	}

	// ② 解除故障注入，下次 Query 必须回源拿到库里的新值。
	fs.hsetErr = nil
	var out testUser
	if err := e.query[testUser](ctx, "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if out.Name != "fresh" {
		t.Fatalf("点查应回源拿到库里的新值，实际 out.Name=%q", out.Name)
	}
	if src.oneCalls != 2 {
		t.Fatalf("刷新回源 1 次 + 失效后点查回源 1 次，实际 %d 次", src.oneCalls)
	}
}

// failSetTombStore 只让 SetTomb 失败，其余语义沿用内存 fake。
type failSetTombStore struct{ *fakeStore }

func (s *failSetTombStore) SetTomb(_ context.Context, _ string, _ time.Duration) error {
	return errors.New("set tomb down")
}

// TestDropCachedContinuesAfterSetTombFailure 验证事务删除后的 SetTomb 失败
// 不再直接 return：DelRow 仍会执行并自行补立墓碑、删掉旧行 hash、销脏账。
func TestDropCachedContinuesAfterSetTombFailure(t *testing.T) {
	e, fs, _, _, b := newTestEngine(t)
	rowKey, tombKey := seedStaleRow(t, fs, b)
	e.store = &failSetTombStore{fakeStore: fs}

	e.dropCached(b, exactKeyVals())

	if h := fs.hash(rowKey); len(h) != 0 {
		t.Fatalf("SetTomb 失败后仍须删掉旧行 hash，实际=%v", h)
	}
	if !fs.hasTomb(tombKey) {
		t.Fatal("DelRow 应自行补立墓碑")
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("SetTomb 失败后仍须销脏账，实际 %d", n)
	}
}

// failDelRowStore 只让 DelRow 失败并记录调用次数，用于验证 remove 的兜底重试。
type failDelRowStore struct {
	*fakeStore
	calls int
}

func (s *failDelRowStore) DelRow(_ context.Context, _, _, _, _, _ string, _ time.Duration) (int64, error) {
	s.calls++
	return 0, errors.New("delrow down")
}

// TestRemoveRetriesInvalidateOnDelRowFailure 验证单行删除的 Redis 清理失败
// 走与刷新失败统一的 Error + invalidateCached 兜底（DelRow 至少被尝试两次）。
func TestRemoveRetriesInvalidateOnDelRowFailure(t *testing.T) {
	e, fs, _, _, b := newTestEngine(t)
	store := &failDelRowStore{fakeStore: fs}
	e.store = store

	if err := e.remove(context.Background(), b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if store.calls != 2 {
		t.Fatalf("DelRow 失败后应再经 invalidateCached 重试一次，实际调用 %d 次", store.calls)
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

func TestSaveRedisFailureFallsBackToDB(t *testing.T) {
	e, fs, _, sink, _ := newTestEngine(t)
	fs.hsetErr = errors.New("redis down")
	if err := e.save[testUser](context.Background(), "test_user", "7", &testUser{ID: 7, Name: "fallback"}, true); err != nil {
		t.Fatalf("mustSave 应退化为直写 DB: %v", err)
	}
	rows := sink.snapshot()
	if len(rows) != 1 || rows[0].Name != "fallback" {
		t.Fatalf("直写 DB 内容=%v", rows)
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
	e, _, _, _, _ := newTestEngine(t)
	err := e.save[testUser](context.Background(), "test_user", "", &testUser{Name: "no-id"}, false)
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

	if _, ok, _ := fs.Claim(ctx, dirtyKey, rowKey, tombKey, "7", 100); !ok {
		t.Fatal("首次领取应成功")
	}
	// 并发 Save 刷新了 score，旧 score 的领取不得销账。
	_ = fs.ZAdd(ctx, dirtyKey, "7", 200)
	if _, ok, _ := fs.Claim(ctx, dirtyKey, rowKey, tombKey, "7", 100); ok {
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

// 集成测试环境变量名：与 task_engine/storage_test.go 完全一致，运行时注入。
const (
	txEnvMySQLHost     = "TASKENGINE_TEST_MYSQL_HOST"
	txEnvMySQLUser     = "TASKENGINE_TEST_MYSQL_USER"
	txEnvMySQLPassword = "TASKENGINE_TEST_MYSQL_PASSWORD"
	txEnvMySQLDB       = "TASKENGINE_TEST_MYSQL_DB"
	txEnvRedisAddr     = "TASKENGINE_TEST_REDIS_ADDR"
	txEnvRedisPassword = "TASKENGINE_TEST_REDIS_PASSWORD"
	txEnvRedisDB       = "TASKENGINE_TEST_REDIS_DB"
)

const (
	// txUserTable / txOrderTable 是本文件独占的表名；task_engine 的表同库共存但互不触碰。
	txUserTable  = "tx_case_user"
	txOrderTable = "tx_case_order"
	// txTestKeyPrefix 是本文件独占的 Redis 键前缀，清理时只扫这个前缀。
	txTestKeyPrefix = "txcase20261109"
	// txTestRedisDB 固定 14，与 task_engine 的 DB 15 分离。
	txTestRedisDB = 14
)

// txTestEnv 是真库连接参数（值只在进程内存中，不落盘）。
type txTestEnv struct {
	mysqlHost     string
	mysqlUser     string
	mysqlPassword string
	mysqlDB       string
	redisAddr     string
	redisPassword string
	redisDB       int
}

// txCaseUser 单列行键（id）模型；TableName 让 mysqldb.Repository 的泛型仓储
// 找到 tx_case_user（TxQuery 走 NewTxRepository[T]，表名由 T 决定）。
type txCaseUser struct {
	ID    int64  `cache:"id" gorm:"column:id;primaryKey" json:"id"`
	Name  string `cache:"name" json:"name"`
	Score int64  `cache:"score" json:"score"`
	State string `cache:"state" json:"state"`
}

// TableName 返回本用例独占的用户表名。
func (txCaseUser) TableName() string { return txUserTable }

// txCaseOrder 复合行键（user_id + seq）模型，用于完整/不完整行键断言。
type txCaseOrder struct {
	UserID int64  `cache:"user_id" gorm:"column:user_id" json:"user_id"`
	Seq    int64  `cache:"seq" gorm:"column:seq" json:"seq"`
	Amount int64  `cache:"amount" json:"amount"`
	State  string `cache:"state" json:"state"`
}

// TableName 返回本用例独占的订单表名。
func (txCaseOrder) TableName() string { return txOrderTable }

// 事务条件结构体：指针字段参与条件，nil 字段跳过（parseCond 语义）。
// idCond / nameCond / ptrInt / ptrStr 复用 fake_test.go 中的既有夹具。
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

// txSchemaStatements 每个用例前重建自己的两张表；绝不 DROP / TRUNCATE task_* 表。
var txSchemaStatements = []string{
	"DROP TABLE IF EXISTS " + txUserTable,
	"DROP TABLE IF EXISTS " + txOrderTable,
	"CREATE TABLE " + txUserTable + " (" +
		"`id` BIGINT NOT NULL," +
		"`name` VARCHAR(64) NOT NULL DEFAULT ''," +
		"`score` BIGINT NOT NULL DEFAULT 0," +
		"`state` VARCHAR(32) NOT NULL DEFAULT ''," +
		"PRIMARY KEY (`id`)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	"CREATE TABLE " + txOrderTable + " (" +
		"`user_id` BIGINT NOT NULL," +
		"`seq` BIGINT NOT NULL," +
		"`amount` BIGINT NOT NULL DEFAULT 0," +
		"`state` VARCHAR(32) NOT NULL DEFAULT ''," +
		"PRIMARY KEY (`user_id`, `seq`)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
}

// 基座状态：进程只初始化一次，defaultEngine 是进程级全局单例（engine.go:30），
// 每个用例开始时安装、结束时还原，绝不与其它用例并行（不调用 t.Parallel）。
var (
	txSetupOnce  sync.Once
	txSetupErr   error
	txTestClient *mysqldb.Client
	txTestRedis  *redis.Client
	txTestEngine *Engine
	txTestEnvAll = []string{
		txEnvMySQLHost, txEnvMySQLUser, txEnvMySQLPassword, txEnvMySQLDB,
		txEnvRedisAddr, txEnvRedisPassword, txEnvRedisDB,
	}
)

// loadTxTestEnv 读取 7 个环境变量；缺任一变量、MySQL 必填项为空、Redis DB
// 不是整数时返回 false，由调用方 t.Skip。Redis 库号固定改为 14，避免与
// task_engine 共用 DB 15 时互相清 key。
func loadTxTestEnv() (txTestEnv, bool) {
	for _, name := range txTestEnvAll {
		if _, ok := os.LookupEnv(name); !ok {
			return txTestEnv{}, false
		}
	}
	env := txTestEnv{
		mysqlHost:     os.Getenv(txEnvMySQLHost),
		mysqlUser:     os.Getenv(txEnvMySQLUser),
		mysqlPassword: os.Getenv(txEnvMySQLPassword),
		mysqlDB:       os.Getenv(txEnvMySQLDB),
		redisAddr:     os.Getenv(txEnvRedisAddr),
		redisPassword: os.Getenv(txEnvRedisPassword),
	}
	if env.mysqlHost == "" || env.mysqlUser == "" || env.mysqlDB == "" || env.redisAddr == "" {
		return txTestEnv{}, false
	}
	if raw := os.Getenv(txEnvRedisDB); raw != "" {
		if _, err := strconv.Atoi(raw); err != nil {
			return txTestEnv{}, false
		}
	}
	env.redisDB = txTestRedisDB
	return env, true
}

// initTxStorage 建客户端、初始化缓存层并绑定两张独占表；完成后还原 defaultEngine，
// 避免把真库引擎泄漏给同包其它（脱离真库的）用例。
func initTxStorage(env txTestEnv) error {
	client, err := mysqldb.NewClient(&config.MySqlDBConfig{
		Host:     env.mysqlHost,
		Username: env.mysqlUser,
		Password: env.mysqlPassword,
		DBName:   env.mysqlDB,
	})
	if err != nil {
		return fmt.Errorf("连接 MySQL: %w", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		return fmt.Errorf("MySQL Ping: %w", err)
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     env.redisAddr,
		Password: env.redisPassword,
		DB:       env.redisDB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return fmt.Errorf("连接 Redis DB %d: %w", env.redisDB, err)
	}

	prev := defaultEngine.Swap(nil)
	restore := func(err error) error {
		defaultEngine.Store(prev)
		return err
	}
	cfg := DefaultConfig()
	cfg.KeyPrefix = txTestKeyPrefix
	// 不启动定时器（本文件从不调用 Start），FlushInterval 拉长只为语义清晰。
	cfg.FlushInterval = time.Hour
	if err := Init(cfg, rdb); err != nil {
		return restore(fmt.Errorf("初始化缓存层: %w", err))
	}
	if err := BindMySQL[txCaseUser](txUserTable, client, BindOption[txCaseUser]{}); err != nil {
		return restore(fmt.Errorf("绑定 %s: %w", txUserTable, err))
	}
	if err := BindMySQL[txCaseOrder](txOrderTable, client, BindOption[txCaseOrder]{
		KeyColumns: []string{"user_id", "seq"},
	}); err != nil {
		return restore(fmt.Errorf("绑定 %s: %w", txOrderTable, err))
	}
	e := Default()
	defaultEngine.Store(prev)

	txTestClient = client
	txTestRedis = rdb
	txTestEngine = e
	return nil
}

// setupTxStorage 初始化门控基座，并在每个用例前后重置数据表 / Redis 键。
func setupTxStorage(t *testing.T) *Engine {
	t.Helper()
	env, ok := loadTxTestEnv()
	if !ok {
		t.Skipf("dbcache 事务真库集成测试未配置：需运行时注入 %s / %s / %s / %s / %s / %s / %s"+
			"（值不写入仓库任何文件）；MySQL 库名必须含 test 且指向专用隔离库，Redis 固定使用 DB %d",
			txEnvMySQLHost, txEnvMySQLUser, txEnvMySQLPassword, txEnvMySQLDB,
			txEnvRedisAddr, txEnvRedisPassword, txEnvRedisDB, txTestRedisDB)
	}
	if !strings.Contains(strings.ToLower(env.mysqlDB), "test") {
		t.Fatalf("%s 必须包含 test，当前 %q", txEnvMySQLDB, env.mysqlDB)
	}
	txSetupOnce.Do(func() { txSetupErr = initTxStorage(env) })
	if txSetupErr != nil {
		t.Fatalf("初始化 dbcache 事务测试存储失败: %v", txSetupErr)
	}

	old := defaultEngine.Swap(txTestEngine)
	t.Cleanup(func() {
		defaultEngine.Store(old)
		clearTxRedisKeys(t)
	})
	resetTxTables(t)
	clearTxRedisKeys(t)
	return txTestEngine
}

// resetTxTables 只重建本文件自己的两张表。
func resetTxTables(t *testing.T) {
	t.Helper()
	for _, stmt := range txSchemaStatements {
		txExecSQL(t, stmt)
	}
}

// clearTxRedisKeys 只删本文件前缀的键，不碰同库其它用例的键。
func clearTxRedisKeys(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pattern := "*" + txTestKeyPrefix + "*"
	var cursor uint64
	for {
		keys, next, err := txTestRedis.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			t.Fatalf("扫描 Redis 键: %v", err)
		}
		if len(keys) > 0 {
			if err := txTestRedis.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("删除 Redis 键: %v", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// txExecSQL 执行一条测试 DDL / DML。
func txExecSQL(t *testing.T, stmt string) {
	t.Helper()
	if err := txTestClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Exec(stmt).Error
	}); err != nil {
		t.Fatalf("执行 SQL %q: %v", stmt, err)
	}
}

// txDBRows 直查数据库拿真相（绕开 Redis），用于提交 / 回滚断言。
func txDBRows[T any](t *testing.T, table, where, order string, args ...any) []T {
	t.Helper()
	rows := make([]T, 0)
	err := txTestClient.Raw(context.Background(), func(gx *gorm.DB) error {
		q := gx.Table(table)
		if where != "" {
			q = q.Where(where, args...)
		}
		if order != "" {
			q = q.Order(order)
		}
		return q.Find(&rows).Error
	})
	if err != nil {
		t.Fatalf("查询 %s 失败: %v", table, err)
	}
	return rows
}

// txSeedUsers 往独占用户表插初始行。
func txSeedUsers(t *testing.T, rows ...*txCaseUser) {
	t.Helper()
	for _, row := range rows {
		r := row
		if err := txTestClient.Raw(context.Background(), func(gx *gorm.DB) error {
			return gx.Table(txUserTable).Create(r).Error
		}); err != nil {
			t.Fatalf("写入 %s 行 %+v: %v", txUserTable, r, err)
		}
	}
}

// txSeedOrders 往独占订单表插初始行。
func txSeedOrders(t *testing.T, rows ...*txCaseOrder) {
	t.Helper()
	for _, row := range rows {
		r := row
		if err := txTestClient.Raw(context.Background(), func(gx *gorm.DB) error {
			return gx.Table(txOrderTable).Create(r).Error
		}); err != nil {
			t.Fatalf("写入 %s 行 %+v: %v", txOrderTable, r, err)
		}
	}
}

// txKeys 生成一张表的 Redis 键集合（与 Init 使用的 KeyPrefix 一致）。
func txKeys(t *testing.T, table string) tableKeys {
	t.Helper()
	keys, err := makeTableKeys(txTestKeyPrefix, table)
	if err != nil {
		t.Fatalf("makeTableKeys(%q): %v", table, err)
	}
	return keys
}

// txRowHash 直读 Redis 行 hash。
func txRowHash(t *testing.T, keys tableKeys, key string) map[string]string {
	t.Helper()
	h, err := txTestRedis.HGetAll(context.Background(), keys.rowPrefix+key).Result()
	if err != nil {
		t.Fatalf("HGetAll(%s): %v", keys.rowPrefix+key, err)
	}
	return h
}

// txTombExists 判断删除墓碑是否存在。
func txTombExists(t *testing.T, keys tableKeys, key string) bool {
	t.Helper()
	n, err := txTestRedis.Exists(context.Background(), keys.tombPrefix+key).Result()
	if err != nil {
		t.Fatalf("Exists(%s): %v", keys.tombPrefix+key, err)
	}
	return n > 0
}

// txBackfillUser 用非事务点查把当前数据库值回填进 Redis，制造「缓存旧值」前置状态。
func txBackfillUser(t *testing.T, id int64) {
	t.Helper()
	var out txCaseUser
	if err := Query[txCaseUser](context.Background(), txUserTable, &idCond{ID: ptrInt(id)}, &out); err != nil {
		t.Fatalf("回填缓存 id=%d: %v", id, err)
	}
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

// txAssertDBUser 断言数据库里 id 行的 name / state / score。
func txAssertDBUser(t *testing.T, id int64, name, state string, score int64) {
	t.Helper()
	rows := txDBRows[txCaseUser](t, txUserTable, "`id` = ?", "`id` ASC", id)
	if len(rows) != 1 {
		t.Fatalf("数据库 %s id=%d 期望 1 行，实际 %d 行", txUserTable, id, len(rows))
	}
	got := rows[0]
	if got.Name != name || got.State != state || got.Score != score {
		t.Fatalf("数据库 %s id=%d 期望 name=%q state=%q score=%d，实际 %+v",
			txUserTable, id, name, state, score, got)
	}
}

// txInTxOption 与 db.InTx 等价：门面只把 fn 放进 Options.InTx，CacheSave 检测后
// 转调 RunTx（db/cache_api.go 的 InTx / CacheSave）。dbcache 内部测试不能 import
// touchgocore/db（db → dbcache → db 会形成测试期 import cycle），故在本包内以
// 完全相同的 Option 形状 + RunTx 调用覆盖门面的运行时路径。
func txInTxOption(fn func(context.Context) error) Option {
	return func(o *Options) { o.InTx = fn }
}

// txFailHSetStore 只让 HSetAll 失败，其余语义委托给真实 redisStore。
// 用于「提交成功但提交后刷新失败」时验证 C06 的 invalidateCached 兜底。
type txFailHSetStore struct {
	kvStore
	err error
}

// HSetAll 注入写穿缓存失败。
func (s *txFailHSetStore) HSetAll(_ context.Context, _, _ string, _ []string, _ time.Duration) error {
	return s.err
}

// TestTxBeginTxSaveCommitRefreshesCache 覆盖 BeginTx → TxSave → Commit：
// MySQL 落新值，且提交成功后 refreshes 执行、Redis 行 hash 被刷新成新值。
func TestTxBeginTxSaveCommitRefreshesCache(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txBackfillUser(t, 1)
	txAssertUserHash(t, "1", "before", "new", 1)

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tx.Context() == nil || tx.DB() == nil {
		t.Fatal("BeginTx 应返回可用的 Context / DB")
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable, nil); !errors.Is(err, ErrNilRow) {
		t.Fatalf("TxSave(nil) 期望 ErrNilRow，实际 %v", err)
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	txAssertDBUser(t, 1, "after", "paid", 9)
	txAssertUserHash(t, "1", "after", "paid", 9)
}

// TestTxWrapTxSaveCommitRefreshesCache 覆盖 WrapTx 用外部 mysqldb.Tx 句柄
// 构造缓存层事务后提交并刷新缓存。
func TestTxWrapTxSaveCommitRefreshesCache(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 2, State: "new"})
	txBackfillUser(t, 1)

	raw, err := txTestClient.BeginTx(ctx)
	if err != nil {
		t.Fatalf("mysqldb.Client.BeginTx: %v", err)
	}
	tx := WrapTx(raw)
	defer func() { _ = tx.Rollback() }()
	if tx.Context() == nil || tx.DB() == nil {
		t.Fatal("WrapTx 应保留外部事务的 Context / DB")
	}
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "wrapped", Score: 8, State: "done"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	txAssertDBUser(t, 1, "wrapped", "done", 8)
	txAssertUserHash(t, "1", "wrapped", "done", 8)
}

// TestTxRollbackDiscardsRefreshes 覆盖 BeginTx → TxSave → Rollback：
// refreshes 被丢弃，MySQL 无新行 / 旧行不变，Redis 仍是旧值。
func TestTxRollbackDiscardsRefreshes(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txBackfillUser(t, 1)
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

	rows := txDBRows[txCaseUser](t, txUserTable, "", "`id` ASC")
	if len(rows) != 1 || rows[0].ID != 1 || rows[0].Name != "before" {
		t.Fatalf("回滚后数据库应只剩原本的 1 行且未变，实际 %+v", rows)
	}
	txAssertUserHash(t, "1", "before", "new", 1)
	if h := txRowHash(t, keys, "2"); len(h) != 0 {
		t.Fatalf("回滚后不应刷新新插入行的缓存，实际 %v", h)
	}
}

// TestTxQuerySingleAndListUseTxConnection 覆盖 TxQuery 的 *T 与 *[]T 两种形态：
// 始终读事务连接（能看到未提交的写），不读 Redis。
func TestTxQuerySingleAndListUseTxConnection(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t,
		&txCaseUser{ID: 1, Name: "one", Score: 1, State: "new"},
		&txCaseUser{ID: 2, Name: "two", Score: 2, State: "new"})
	txBackfillUser(t, 1)

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

	// 事务内的写尚未提交，Redis 仍应是旧值：TxQuery 既不读 Redis 也不刷新它。
	txAssertUserHash(t, "1", "one", "new", 1)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	rows := txDBRows[txCaseUser](t, txUserTable, "", "`id` ASC")
	if len(rows) != 2 {
		t.Fatalf("回滚后数据库应保持 2 行，实际 %d 行", len(rows))
	}
}

// TestTxLockOneExactKeyPartialKeyAndRowLock 覆盖 TxLockOne：完整行键可用、
// 非完整行键 / 多行 IN 条件被 ErrPartialKey 拒绝，并真实验证 FOR UPDATE 行锁。
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

	// 真实行锁：另一连接用 NOWAIT 抢同一行必须立即失败；释放锁后同一语句成功。
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	other, err := txTestClient.BeginTx(lockCtx)
	if err != nil {
		t.Fatalf("第二个事务 BeginTx: %v", err)
	}
	var blocked txCaseOrder
	lockQuery := func(txDB *gorm.DB) error {
		return txDB.WithContext(lockCtx).Table(txOrderTable).
			Where("`user_id` = ? AND `seq` = ?", int64(1), int64(1)).
			Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).
			Take(&blocked).Error
	}
	if err := lockQuery(other.DB); err == nil {
		_ = other.Rollback()
		t.Fatal("第一个事务已持行锁，第二个事务 NOWAIT 应失败")
	}
	if err := other.Rollback(); err != nil {
		t.Fatalf("第二个事务 Rollback: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	released, err := txTestClient.BeginTx(lockCtx)
	if err != nil {
		t.Fatalf("释放锁后 BeginTx: %v", err)
	}
	defer func() { _ = released.Rollback() }()
	if err := lockQuery(released.DB); err != nil {
		t.Fatalf("释放行锁后 NOWAIT 查询应成功，实际 %v", err)
	}
}

// TestTxIncrementAndCompareAndSwap 覆盖事务内原子累加 / CAS：提交后生效并刷新缓存，
// 且 condIsExactKey 拒绝全表 / 多行条件。
func TestTxIncrementAndCompareAndSwap(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "alice", Score: 10, State: "new"})
	txBackfillUser(t, 1)

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

	txAssertDBUser(t, 1, "alice", "paid", 15)
	txAssertUserHash(t, "1", "alice", "paid", 15)
}

// TestTxUpdateInAndDeleteInCommit 覆盖批量更新 / 批量删除在事务提交后生效，
// 更新行缓存被刷新、被删行的行 hash 被 dropCached 清掉并立墓碑。
func TestTxUpdateInAndDeleteInCommit(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t,
		&txCaseUser{ID: 1, Name: "one", Score: 1, State: "new"},
		&txCaseUser{ID: 2, Name: "two", Score: 2, State: "new"},
		&txCaseUser{ID: 3, Name: "three", Score: 3, State: "new"})
	txBackfillUser(t, 1)
	txBackfillUser(t, 2)
	txBackfillUser(t, 3)
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

	rows := txDBRows[txCaseUser](t, txUserTable, "", "`id` ASC")
	if len(rows) != 2 {
		t.Fatalf("提交后应只剩 2 行，实际 %d 行 %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.State != "done" {
			t.Fatalf("提交后 id=%d 状态应为 done，实际 %+v", row.ID, row)
		}
	}
	txAssertUserHash(t, "1", "one", "done", 1)
	txAssertUserHash(t, "2", "two", "done", 2)
	if h := txRowHash(t, keys, "3"); len(h) != 0 {
		t.Fatalf("删除提交后行 3 的缓存应被清掉，实际 %v", h)
	}
	if !txTombExists(t, keys, "3") {
		t.Fatal("删除提交后应留下行 3 的墓碑")
	}
}

// TestTxUpdateInAndDeleteInRollback 覆盖批量写「事务内生效、回滚后不生效」，
// 且回滚不触发任何缓存刷新 / 清理。
func TestTxUpdateInAndDeleteInRollback(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t,
		&txCaseUser{ID: 1, Name: "one", Score: 1, State: "new"},
		&txCaseUser{ID: 2, Name: "two", Score: 2, State: "new"},
		&txCaseUser{ID: 3, Name: "three", Score: 3, State: "new"})
	txBackfillUser(t, 3)
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
		t.Fatalf("事务内第 3 行应已删除，实际 %+v", remaining)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	rows := txDBRows[txCaseUser](t, txUserTable, "", "`id` ASC")
	if len(rows) != 3 {
		t.Fatalf("回滚后数据库应仍有 3 行，实际 %d 行", len(rows))
	}
	for _, row := range rows {
		if row.State != "new" {
			t.Fatalf("回滚后 id=%d 状态应为 new，实际 %+v", row.ID, row)
		}
	}
	if h := txRowHash(t, keys, "3"); h["name"] != "three" {
		t.Fatalf("回滚后不应清掉行 3 的缓存，实际 %v", h)
	}
	if txTombExists(t, keys, "3") {
		t.Fatal("回滚后不应给行 3 立墓碑")
	}
}

// TestTxInsertAndInsertBatch 覆盖 insert.go 的 TxInsert / TxInsertBatch：
// 提交后插入行生效并刷新缓存，冲突行 affected=0，回滚后新行不存在且不刷新缓存。
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

	rows := txDBRows[txCaseUser](t, txUserTable, "", "`id` ASC")
	if len(rows) != 4 {
		t.Fatalf("提交后应有 4 行，实际 %d 行 %+v", len(rows), rows)
	}
	if rows[0].Name != "original" {
		t.Fatalf("冲突行不应被覆盖，实际 %+v", rows[0])
	}
	txAssertUserHash(t, "2", "ins2", "new", 2)
	txAssertUserHash(t, "3", "ins3", "new", 3)
	txAssertUserHash(t, "4", "ins4", "new", 4)

	// 回滚路径：新行不落库、缓存不刷新。
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
	rows = txDBRows[txCaseUser](t, txUserTable, "`id` = ?", "`id` ASC", int64(5))
	if len(rows) != 0 {
		t.Fatalf("回滚后不应存在 id=5，实际 %+v", rows)
	}
	if h := txRowHash(t, keys, "5"); len(h) != 0 {
		t.Fatalf("回滚后不应刷新 id=5 的缓存，实际 %v", h)
	}
}

// TestRunTxFacadeCommit 覆盖 db.InTx → RunTx 门面路径：用与门面完全一致的
// Option 形状，fn 内经事务 ctx 走 Query(ForUpdate) / Save(整行) / Save(原子累加)，
// 提交后三者生效且缓存被刷新。
func TestRunTxFacadeCommit(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txBackfillUser(t, 1)

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

	txAssertDBUser(t, 1, "facade", "paid", 7)
	txAssertUserHash(t, "1", "facade", "paid", 7)
}

// TestRunTxFacadeErrorRollsBack 覆盖门面 fn 返回错误：事务回滚、缓存不刷新。
func TestRunTxFacadeErrorRollsBack(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txBackfillUser(t, 1)

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

	txAssertDBUser(t, 1, "before", "new", 1)
	txAssertUserHash(t, "1", "before", "new", 1)
}

// TestRunTxFacadePanicRollsBack 覆盖门面 panic 路径：RunTx 回滚后重新 panic，
// 且不执行提交后刷新。
func TestRunTxFacadePanicRollsBack(t *testing.T) {
	setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txBackfillUser(t, 1)

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

	txAssertDBUser(t, 1, "before", "new", 1)
	txAssertUserHash(t, "1", "before", "new", 1)
}

// TestTxCommitRefreshFailureInvalidatesCached 覆盖「提交成功但提交后刷新失败」：
// Commit 仍返回 nil（不误报提交失败），C06 的 invalidateCached 删掉旧行 hash、
// 立墓碑，下次点查必然回源拿到库里的新值。真库 + 真 Redis + HSetAll 故障注入。
func TestTxCommitRefreshFailureInvalidatesCached(t *testing.T) {
	e := setupTxStorage(t)
	ctx := context.Background()
	txSeedUsers(t, &txCaseUser{ID: 1, Name: "before", Score: 1, State: "new"})
	txBackfillUser(t, 1)
	keys := txKeys(t, txUserTable)

	origStore := e.store
	e.store = &txFailHSetStore{kvStore: origStore, err: errors.New("redis hset down")}
	defer func() { e.store = origStore }()

	tx, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := TxSave[txCaseUser](tx, ctx, txUserTable,
		&txCaseUser{ID: 1, Name: "after", Score: 9, State: "paid"}); err != nil {
		t.Fatalf("TxSave: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("刷新失败不应让 Commit 返回错误（库已提交），实际 %v", err)
	}
	e.store = origStore

	txAssertDBUser(t, 1, "after", "paid", 9)
	if h := txRowHash(t, keys, "1"); len(h) != 0 {
		t.Fatalf("刷新失败后旧行 hash 必须被主动失效，实际 %v", h)
	}
	if !txTombExists(t, keys, "1") {
		t.Fatal("刷新失败后应立墓碑阻止在途脏账回写旧值")
	}
	var out txCaseUser
	if err := Query[txCaseUser](ctx, txUserTable, &idCond{ID: ptrInt(1)}, &out); err != nil {
		t.Fatalf("失效后点查: %v", err)
	}
	if out.Name != "after" || out.Score != 9 || out.State != "paid" {
		t.Fatalf("失效后点查必须回源拿到库里的新值，实际 %+v", out)
	}
}
