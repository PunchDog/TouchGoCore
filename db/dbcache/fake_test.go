package dbcache

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

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
	ttl       map[string]time.Duration
	backfills int
	hsetErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		hashes: make(map[string]map[string]string),
		dirty:  make(map[string]map[string]int64),
		retry:  make(map[string]map[string]int64),
		ttl:    make(map[string]time.Duration),
	}
}

func (s *fakeStore) HGetAll(_ context.Context, key string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMap(s.hashes[key]), nil
}

func (s *fakeStore) HSetAll(_ context.Context, key string, fields []string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hsetErr != nil {
		return s.hsetErr
	}
	h := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		h[fields[i]] = fields[i+1]
	}
	s.hashes[key] = h
	if ttl > 0 {
		s.ttl[key] = ttl
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

func (s *fakeStore) Claim(_ context.Context, dirtyKey, rowKey, member string, score int64) (map[string]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	oneCalls  int
	listCalls int
	lastVals  []condVal
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
