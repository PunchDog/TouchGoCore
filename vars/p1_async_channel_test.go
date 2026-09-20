package vars

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// memChanWriter 记录写入内容并可选阻塞，用于断言关闭时的排空行为
type memChanWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	slow time.Duration
}

func (w *memChanWriter) Write(p []byte) (int, error) {
	if w.slow > 0 {
		time.Sleep(w.slow)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *memChanWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func newTestChan(t *testing.T, w io.Writer, bufSize int) *AsyncLoggerChannel {
	t.Helper()
	cfg := DefaultAsyncChannelConfig()
	cfg.BufferSize = bufSize
	cfg.FlushInterval = 5 * time.Millisecond
	cfg.BatchTimeout = 5 * time.Millisecond
	return NewAsyncLoggerChannel(w, cfg)
}

// TestAsyncChannel_FlushAfterCloseNoPanic 回归：Flush 曾投递一个已关闭的共享 channel，
// 消费者执行 close(callback) 时对已关闭 channel 再关 → panic 杀死进程。
func TestAsyncChannel_FlushAfterCloseNoPanic(t *testing.T) {
	w := &memChanWriter{}
	a := newTestChan(t, w, 16)

	if err := a.Flush(); err != nil {
		t.Fatalf("Flush before close returned error: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// 关闭后继续 Flush / FlushAndWait，既不能 panic 也不能挂死
	for i := 0; i < 20; i++ {
		_ = a.Flush()
		_ = a.FlushAndWait(50 * time.Millisecond)
	}

	if err := runWithRecover(func() {
		for i := 0; i < 100; i++ {
			_ = a.Flush()
		}
	}); err != nil {
		t.Fatalf("Flush after Close panicked: %v", err)
	}
}

// TestAsyncChannel_FlushAndWaitReturns 回归：FlushAndWait 必须得到消费者的回执并返回，
// 而不是静默丢掉刷新信号后一直等到超时。
func TestAsyncChannel_FlushAndWaitReturns(t *testing.T) {
	w := &memChanWriter{}
	a := newTestChan(t, w, 128)
	defer func() { _ = a.Close() }()

	for i := 0; i < 10; i++ {
		if !a.EnqueueSimple(slog.LevelInfo, "flush-and-wait-entry") {
			t.Fatalf("Enqueue rejected entry %d", i)
		}
	}

	done := make(chan error, 1)
	go func() { done <- a.FlushAndWait(3 * time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FlushAndWait returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("FlushAndWait did not return")
	}

	if got := len(w.String()); got == 0 {
		t.Fatalf("FlushAndWait returned but nothing was written")
	}
}

// TestAsyncChannel_ConcurrentEnqueueClose 回归：Close 曾 close(input)，与并发 Enqueue
// 竞争时对已关闭 channel 发送 → panic。现协议为永不关闭数据通道。
func TestAsyncChannel_ConcurrentEnqueueClose(t *testing.T) {
	w := &memChanWriter{}
	a := newTestChan(t, w, 8)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				a.EnqueueSimple(slog.LevelInfo, "concurrent-entry")
			}
		}(i)
	}

	// 生产者仍在投递时关闭
	time.Sleep(2 * time.Millisecond)
	if err := runWithRecover(func() {
		_ = a.Close()
	}); err != nil {
		t.Fatalf("Close panicked: %v", err)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatalf("producers stuck after Close")
	}

	if err := runWithRecover(func() { _ = a.Close() }); err != nil {
		t.Fatalf("second Close panicked: %v", err)
	}
}

// TestAsyncChannel_CloseDrainsQueue 回归：关闭前入队的日志必须落盘，不能整批丢弃。
func TestAsyncChannel_CloseDrainsQueue(t *testing.T) {
	w := &memChanWriter{slow: time.Millisecond}
	a := newTestChan(t, w, 256)

	const total = 50
	for i := 0; i < total; i++ {
		if !a.EnqueueSimple(slog.LevelInfo, "drain-me") {
			t.Fatalf("Enqueue rejected at %d", i)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	out := w.String()
	if out == "" {
		t.Fatalf("nothing written on close")
	}
	if n := strings.Count(out, "drain-me"); n < 2 {
		t.Fatalf("expected drained entries in output, got %d", n)
	}
}

// runWithRecover 在隔离 recover 中执行 fn，把 panic 转为可断言的错误
func runWithRecover(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	fn()
	return nil
}
