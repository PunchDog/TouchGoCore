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

	// 异步循环在独立协程跑，Write 与 Sync 只投不写，避免阻塞业务调用方。
	a.wg.Add(1)
	go a.run()

	return a
}

// Write 写入数据
func (a *AsyncWriteSyncer) Write(p []byte) (n int, err error) {
	if a.closed.Load() {
		return 0, fmt.Errorf("writer is closed")
	}

	// 必须拷贝 p：Write 不保证同步消费，原地复用会在另一协程写盘时读到半截数据。
	buf := make([]byte, len(p))
	copy(buf, p)

	select {
	case a.input <- buf:
		return len(p), nil
	default:
		// 缓冲满：直接丢弃并记一笔，不阻塞调用方。
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
	case <-time.After(2 * time.Second):
		// 队列被占用，退化为不阻塞
		return nil
	case <-a.stop:
		return nil
	}

	select {
	case <-done:
		return nil
	case <-a.stop:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("async writer sync timeout")
	}
}

// Close 关闭写入器
func (a *AsyncWriteSyncer) Close() error {
	if !a.closed.CompareAndSwap(false, true) {
		return nil
	}

	// 先发停止信号再等协程退出：run 在 stop 触发后自行排空并 return。
	close(a.stop)
	a.wg.Wait()

	// 关闭前兜底：把内存里残留的缓冲落盘，否则最后一段日志丢失。
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
			// 把已提交的写入并入缓冲，攒够再一次性刷盘。
			a.buffer = append(a.buffer, data...)
			// 缓冲区满时立即刷新
			if len(a.buffer) >= 4096 {
				a.flushBuffer()
			}

		case done := <-a.flush:
			// 先排空已提交的写入再落盘，保证 Sync 返回即代表此前所有 Write 已可见
			a.drainInput()
			a.flushBuffer()
			// 通知flush完成（done 由调用方创建，必须在此关闭，
			// 否则 zap 的 Sync() 会永久等待）
			if done != nil {
				close(done)
			}

		case <-flushInterval.C:
			if len(a.buffer) > 0 {
				a.flushBuffer()
			}

		case <-a.stop:
			// 先排空 input，避免关闭时丢失缓冲区内尚未落盘的日志
			a.drainInput()
			a.flushBuffer()
			return
		}
	}
}

// drainInput 非阻塞地把 input 中已提交的写入搬到缓冲区
func (a *AsyncWriteSyncer) drainInput() {
	// 循环取空 input：每次取到 default 即说明已无在途写入，立即退出。
	for {
		select {
		case data := <-a.input:
			a.buffer = append(a.buffer, data...)
		default:
			return
		}
	}
}

// flushBuffer 刷新缓冲区
func (a *AsyncWriteSyncer) flushBuffer() {
	// 空缓冲直接返回，避免无谓的 Write 系统调用。
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
