package vars

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 异步日志写入器 ====================

// AsyncWriteSyncer 异步写入器
type AsyncWriteSyncer struct {
	input   chan []byte
	flush   chan chan struct{}
	stop    chan struct{}
	writer  io.Writer
	closed  atomic.Bool
	wg      sync.WaitGroup
	buffer  []byte
	dropped atomic.Int64
}

// NewAsyncWriteSyncer 创建异步写入器
func NewAsyncWriteSyncer(writer io.Writer, bufferSize int) *AsyncWriteSyncer {
	a := &AsyncWriteSyncer{
		input:  make(chan []byte, bufferSize),
		flush:  make(chan chan struct{}, 1),
		stop:   make(chan struct{}),
		writer: writer,
		buffer: make([]byte, 0, 1024),
	}

	a.wg.Add(1)
	go a.run()

	return a
}

// Write 写入数据
func (a *AsyncWriteSyncer) Write(p []byte) (n int, err error) {
	if a.closed.Load() {
		return 0, fmt.Errorf("writer is closed")
	}

	// 复制数据以避免竞态条件
	buf := make([]byte, len(p))
	copy(buf, p)

	select {
	case a.input <- buf:
		return len(p), nil
	default:
		a.dropped.Add(1)
		return len(p), nil
	}
}

// Sync 同步数据
func (a *AsyncWriteSyncer) Sync() error {
	if a.closed.Load() {
		return nil
	}

	done := make(chan struct{}, 1)
	select {
	case a.flush <- done:
		<-done
		return nil
	default:
		return nil
	}
}

// Close 关闭写入器
func (a *AsyncWriteSyncer) Close() error {
	if !a.closed.CompareAndSwap(false, true) {
		return nil
	}

	close(a.stop)
	a.wg.Wait()

	// 处理剩余数据
	if len(a.buffer) > 0 {
		a.writer.Write(a.buffer)
	}

	if syncer, ok := a.writer.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}

	return nil
}

// run 异步处理循环
func (a *AsyncWriteSyncer) run() {
	defer a.wg.Done()

	flushInterval := time.NewTicker(100 * time.Millisecond)
	defer flushInterval.Stop()

	for {
		select {
		case data := <-a.input:
			a.buffer = append(a.buffer, data...)
			// 缓冲区满时立即刷新
			if len(a.buffer) >= 4096 {
				a.flushBuffer()
			}

		case <-a.flush:
			a.flushBuffer()
			// 通知flush完成
			select {
			case done := <-a.flush:
				close(done)
			default:
			}

		case <-flushInterval.C:
			if len(a.buffer) > 0 {
				a.flushBuffer()
			}

		case <-a.stop:
			a.flushBuffer()
			return
		}
	}
}

// flushBuffer 刷新缓冲区
func (a *AsyncWriteSyncer) flushBuffer() {
	if len(a.buffer) == 0 {
		return
	}

	_, err := a.writer.Write(a.buffer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "async write error: %v\n", err)
	}

	a.buffer = a.buffer[:0]
}

// Dropped 返回丢弃的日志数量
func (a *AsyncWriteSyncer) Dropped() int64 {
	return a.dropped.Load()
}
