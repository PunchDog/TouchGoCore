package mysql

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
)

func TestNoopMetrics(t *testing.T) {
	var h MetricsHook = NoopMetrics{}
	h.OnQuery("test", time.Millisecond, KindUnknown)
	h.OnSlowQuery("test", "SELECT 1", time.Millisecond)
	h.OnConnection("idle", 5)
}

func TestObservabilityPlugin_Name(t *testing.T) {
	p := NewObservabilityPlugin(NoopMetrics{}, 100*time.Millisecond)
	if p.Name() != "touchgocore-mysql-observability" {
		t.Fatalf("Name 异常: %s", p.Name())
	}
}

func TestBuildDSNWithOptions(t *testing.T) {
	cfg := &config.MySqlDBConfig{
		Host:     "127.0.0.1:3306",
		Username: "user",
		Password: "pass",
		DBName:   "db",
	}
	o := defaultOptions()
	dsn := buildDSNWithOptions(cfg, o)
	if dsn == "" {
		t.Fatal("DSN 不应为空")
	}
	if !contains(dsn, "user") || !contains(dsn, "pass") || !contains(dsn, "127.0.0.1") {
		t.Fatalf("DSN 缺少关键字段: %s", dsn)
	}
}

func TestBuildDSNWithOptions_ExtraParams(t *testing.T) {
	cfg := &config.MySqlDBConfig{
		Host: "127.0.0.1:3306", Username: "u", Password: "p", DBName: "d",
	}
	o := defaultOptions()
	o.extraDSNParams["time_zone"] = "'+08:00'"
	dsn := buildDSNWithOptions(cfg, o)
	if !contains(dsn, "time_zone") {
		t.Fatalf("DSN 应包含自定义参数: %s", dsn)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestContextPropagation(t *testing.T) {
	type ctxKey string
	key := ctxKey("test-key")
	ctx := context.WithValue(context.Background(), key, "value")
	r := &Repository[int]{ctx: ctx}
	if got := r.Context().Value(key); got != "value" {
		t.Fatalf("ctx 传递丢失: %v", got)
	}
}

func TestIsHelpers(t *testing.T) {
	tests := []struct {
		err  error
		fn   func(error) bool
		want bool
	}{
		{context.DeadlineExceeded, IsTimeout, true},
		{context.Canceled, IsCanceled, true},
		{errors.New("x"), IsTimeout, false},
		{errors.New("x"), IsCanceled, false},
		{errors.New("x"), IsDeadlock, false},
		{errors.New("x"), IsDuplicate, false},
	}
	for _, c := range tests {
		if got := c.fn(c.err); got != c.want {
			t.Fatalf("fn(%v)=%v want=%v", c.err, got, c.want)
		}
	}
}

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()
	if o.slowThreshold != 200*time.Millisecond {
		t.Fatalf("slowThreshold=%v", o.slowThreshold)
	}
	if o.charset != "utf8mb4" {
		t.Fatalf("charset=%s", o.charset)
	}
	if o.collation != "utf8mb4_unicode_ci" {
		t.Fatalf("collation=%s", o.collation)
	}
	if o.connMaxLifetime != 24*time.Hour {
		t.Fatalf("connMaxLifetime=%v", o.connMaxLifetime)
	}
}

func TestOptions(t *testing.T) {
	o := defaultOptions()
	WithSlowQueryThreshold(50 * time.Millisecond)(&o)
	if o.slowThreshold != 50*time.Millisecond {
		t.Fatal("WithSlowQueryThreshold 未生效")
	}
	WithCharset("latin1", "latin1_swedish_ci")(&o)
	if o.charset != "latin1" {
		t.Fatal("WithCharset 未生效")
	}
	WithDSNParam("time_zone", "'+09:00'")(&o)
	if o.extraDSNParams["time_zone"] != "'+09:00'" {
		t.Fatal("WithDSNParam 未生效")
	}
	called := false
	WithMetricsHook(NoopMetrics{})(&o)
	called = true
	if !called {
		t.Fatal("WithMetricsHook 调用失败")
	}
}

func TestToSnakeCase(t *testing.T) {
	cases := map[string]string{
		"User":        "user",
		"UserProfile": "user_profile",
		"HTTPClient":  "h_t_t_p_client",
		"a":           "a",
	}
	for in, want := range cases {
		if got := toSnakeCase(in); got != want {
			t.Fatalf("toSnakeCase(%q)=%q want=%q", in, got, want)
		}
	}
}

// mockHook 用于测试
type mockHook struct {
	queryCalls atomic.Int32
	slowCalls  atomic.Int32
	connCalls  atomic.Int32
	lastOp     string
	lastDur    time.Duration
	lastKind   Kind
}

func (m *mockHook) OnQuery(op string, dur time.Duration, kind Kind) {
	m.queryCalls.Add(1)
	m.lastOp = op
	m.lastDur = dur
	m.lastKind = kind
}
func (m *mockHook) OnSlowQuery(op, sql string, dur time.Duration) {
	m.slowCalls.Add(1)
}
func (m *mockHook) OnConnection(state string, n float64) {
	m.connCalls.Add(1)
}
