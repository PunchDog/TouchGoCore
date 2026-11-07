package dbcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"touchgocore/config"
)

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
