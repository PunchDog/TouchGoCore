package dbcache

import (
	"context"
	"errors"
	"testing"
)

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
	_ = fs.HSetAll(ctx, rowKey, []string{"name", "old"}, 0)
	_ = fs.ZAdd(ctx, dirtyKey, "7", 100)

	if _, ok, _ := fs.Claim(ctx, dirtyKey, rowKey, "7", 100); !ok {
		t.Fatal("首次领取应成功")
	}
	// 并发 Save 刷新了 score，旧 score 的领取不得销账。
	_ = fs.ZAdd(ctx, dirtyKey, "7", 200)
	if _, ok, _ := fs.Claim(ctx, dirtyKey, rowKey, "7", 100); ok {
		t.Fatal("score 已变更时领取应失败")
	}
	if n := fs.dirtyCount(dirtyKey); n != 1 {
		t.Fatalf("并发写入的脏账不应被误删，实际 %d", n)
	}
}
