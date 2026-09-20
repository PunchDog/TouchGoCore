package vars

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAsyncWriteSyncer_SyncReturns 回归：run() 的 flush 分支曾丢弃调用方投递的 done channel，
// 导致 Sync() 在 <-done 上永久挂死（zap 的 Sync 随之挂死）。
func TestAsyncWriteSyncer_SyncReturns(t *testing.T) {
	w := &memChanWriter{}
	a := NewAsyncWriteSyncer(w, 64)
	defer func() { _ = a.Close() }()

	if _, err := a.Write([]byte("sync-must-return\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Sync() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sync returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Sync did not return (deadlock regression)")
	}

	if !strings.Contains(w.String(), "sync-must-return") {
		t.Fatalf("Sync returned before data reached the writer: %q", w.String())
	}
}

// TestAsyncWriteSyncer_SyncRepeated 连续 Sync 每一次都必须返回。
func TestAsyncWriteSyncer_SyncRepeated(t *testing.T) {
	w := &memChanWriter{}
	a := NewAsyncWriteSyncer(w, 64)
	defer func() { _ = a.Close() }()

	for i := 0; i < 20; i++ {
		if _, err := a.Write([]byte(fmt.Sprintf("line-%d\n", i))); err != nil {
			t.Fatalf("Write %d returned error: %v", i, err)
		}
		done := make(chan error, 1)
		go func() { done <- a.Sync() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Sync %d returned error: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Sync %d did not return", i)
		}
	}

	if n := strings.Count(w.String(), "line-"); n != 20 {
		t.Fatalf("expected 20 lines after repeated Sync, got %d", n)
	}
}

// TestAsyncWriteSyncer_CloseDrainsInput 回归：Close 曾不排空 input，
// 关闭时最多丢失 bufferSize 条日志。
func TestAsyncWriteSyncer_CloseDrainsInput(t *testing.T) {
	w := &memChanWriter{slow: 2 * time.Millisecond}
	a := NewAsyncWriteSyncer(w, 4096)

	const total = 600
	for i := 0; i < total; i++ {
		if _, err := a.Write([]byte(fmt.Sprintf("drain-entry-%04d\n", i))); err != nil {
			t.Fatalf("Write %d returned error: %v", i, err)
		}
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if d := a.Dropped(); d != 0 {
		t.Fatalf("expected no dropped writes, got %d", d)
	}
	if n := strings.Count(w.String(), "drain-entry-"); n != total {
		t.Fatalf("expected all %d entries flushed on Close, got %d", total, n)
	}
}
