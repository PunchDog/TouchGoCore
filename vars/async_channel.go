package vars

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 增强型异步日志Channel ====================

// AsyncChannelConfig 异步Channel配置
type AsyncChannelConfig struct {
	BufferSize     int           // Channel缓冲区大小
	FlushInterval  time.Duration // 定期刷新间隔
	FlushThreshold int           // 触发刷新的阈值（字节数）
	DropOnFull     bool          // 缓冲区满时是否丢日志（false则阻塞）
	BatchSize      int           // 批量写入大小
	BatchTimeout   time.Duration // 批量写入超时
}

// DefaultAsyncChannelConfig 默认配置
func DefaultAsyncChannelConfig() AsyncChannelConfig {
	return AsyncChannelConfig{
		BufferSize:     10000,
		FlushInterval:  100 * time.Millisecond,
		FlushThreshold: 4096, // 4KB
		DropOnFull:     false,
		BatchSize:      100,
		BatchTimeout:   50 * time.Millisecond,
	}
}

// logEntry 日志条目
type logEntry struct {
	level   slog.Level
	msg     string
	time    time.Time
	attrs   []slog.Attr
	context context.Context
	file    string // 调用者文件路径（新增）
	line    int    // 调用者行号（新增）
}

// getCaller 获取业务调用者的文件路径与行号。
// 通过遍历调用栈，跳过本包（vars）内部函数帧，
// 返回第一个不属于 vars 包的调用者——即真正发起日志的业务代码位置。
// 相比固定 skip 深度，本方案不受函数内联、调用链中间层增减影响，更稳健。
func getCaller() (string, int) {
	for depth := 1; depth < 16; depth++ {
		pc, file, line, ok := runtime.Caller(depth)
		if !ok {
			break
		}
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			continue
		}
		name := fn.Name()
		// 跳过 vars 包内部函数帧（含 getCaller 本身、Enqueue、Info 便捷函数等）
		if strings.Contains(name, "/vars.") || strings.HasPrefix(name, "touchgocore/vars.") {
			continue
		}
		// Windows 下统一使用反斜杠路径，与系统文件路径风格一致
		if runtime.GOOS == "windows" {
			file = strings.ReplaceAll(file, "/", "\\")
		}
		return file, line
	}
	return "", 0
}

// AsyncChannelStats 异步日志统计
type AsyncChannelStats struct {
	TotalEnqueued int64         // 总入队数
	TotalWritten  int64         // 总写入数
	TotalDropped  int64         // 总丢弃数
	QueuePeak     int64         // 队列峰值
	BufferUsed    int           // 当前缓冲区使用
	WriteLatency  time.Duration // 最近写入延迟
	LastFlush     time.Time     // 上次刷新时间
}

// AsyncLoggerChannel 异步日志Channel
// 使用专用的goroutine处理日志写入，避免阻塞业务逻辑
type AsyncLoggerChannel struct {
	config    AsyncChannelConfig
	input     chan logEntry      // 日志输入channel
	writer    io.Writer          // 实际写入器
	closed    atomic.Bool        // 关闭标志
	stopping  atomic.Bool        // 正在停止中
	wg        sync.WaitGroup     // goroutine同步
	mu        sync.RWMutex       // 统计信息锁
	stats     AsyncChannelStats  // 统计信息
	dropped   atomic.Int64       // 丢弃的日志数
	queued    atomic.Int64       // 入队日志数
	written   atomic.Int64       // 写入日志数
	flushSig  chan chan struct{} // 刷新信号（使用chan chan实现回调）
	batchPool sync.Pool          // 批处理对象池
}

// NewAsyncLoggerChannel 创建异步日志Channel
func NewAsyncLoggerChannel(writer io.Writer, config AsyncChannelConfig) *AsyncLoggerChannel {
	if config.BufferSize <= 0 {
		config.BufferSize = DefaultAsyncChannelConfig().BufferSize
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = DefaultAsyncChannelConfig().FlushInterval
	}
	if config.FlushThreshold <= 0 {
		config.FlushThreshold = DefaultAsyncChannelConfig().FlushThreshold
	}
	if config.BatchSize <= 0 {
		config.BatchSize = DefaultAsyncChannelConfig().BatchSize
	}
	if config.BatchTimeout <= 0 {
		config.BatchTimeout = DefaultAsyncChannelConfig().BatchTimeout
	}

	ch := &AsyncLoggerChannel{
		config:   config,
		input:    make(chan logEntry, config.BufferSize),
		writer:   writer,
		flushSig: make(chan chan struct{}, 1), // 使用 chan chan struct{} 来传递回调
	}

	// 初始化批处理对象池
	ch.batchPool = sync.Pool{
		New: func() interface{} {
			return &batchEntries{
				entries: make([]logEntry, 0, config.BatchSize),
			}
		},
	}

	// 启动消费者goroutine
	ch.wg.Add(1)
	go ch.run()

	return ch
}

// Enqueue 入队日志（异步，不会阻塞）
// 返回是否成功入队
func (a *AsyncLoggerChannel) Enqueue(level slog.Level, msg string, attrs []slog.Attr, ctx context.Context) bool {
	if a.closed.Load() {
		return false
	}

	file, line := getCaller()

	entry := logEntry{
		level:   level,
		msg:     msg,
		time:    time.Now(),
		attrs:   attrs,
		context: ctx,
		file:    file,
		line:    line,
	}

	a.queued.Add(1)

	if a.config.DropOnFull {
		// 非阻塞模式：满则丢弃
		select {
		case a.input <- entry:
			return true
		default:
			a.dropped.Add(1)
			a.stats.TotalDropped++
			return false
		}
	}

	// 阻塞模式：等待写入
	select {
	case a.input <- entry:
		return true
	case <-time.After(5 * time.Second): // 5秒超时
		a.dropped.Add(1)
		a.stats.TotalDropped++
		return false
	}
}

// EnqueueSimple 简化版入队（无attrs）
func (a *AsyncLoggerChannel) EnqueueSimple(level slog.Level, msg string) bool {
	return a.Enqueue(level, msg, nil, nil)
}

// sharedNoopChan 共享的无操作channel（用于无回调的刷新）
var sharedNoopChan = make(chan struct{})

func init() {
	close(sharedNoopChan)
}

// Flush 手动刷新缓冲区（不带回调）
func (a *AsyncLoggerChannel) Flush() error {
	if a.closed.Load() {
		return fmt.Errorf("channel is closed")
	}

	// 发送刷新信号（使用共享的无回调channel）
	select {
	case a.flushSig <- sharedNoopChan:
	default:
	}
	return nil
}

// FlushAndWait 刷新并等待完成
func (a *AsyncLoggerChannel) FlushAndWait(timeout time.Duration) error {
	done := make(chan struct{}, 1)

	select {
	case a.flushSig <- done:
	default:
		// 已经有一个flush在等待
	}

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("flush timeout after %v", timeout)
	}
}

// Close 关闭channel，等待所有日志写入完成
func (a *AsyncLoggerChannel) Close() error {
	if !a.closed.CompareAndSwap(false, true) {
		return nil // 已经关闭
	}

	// 关闭输入channel
	close(a.input)

	// 等待goroutine结束（带超时）
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 正常结束
	case <-time.After(10 * time.Second):
		return fmt.Errorf("close timeout, some logs may not be written")
	}

	// 关闭底层writer
	if closer, ok := a.writer.(io.Closer); ok {
		return closer.Close()
	}

	return nil
}

// GetStats 获取统计信息
func (a *AsyncLoggerChannel) GetStats() AsyncChannelStats {
	stats := AsyncChannelStats{
		TotalEnqueued: a.queued.Load(),
		TotalWritten:  a.written.Load(),
		TotalDropped:  a.dropped.Load(),
		QueuePeak:     a.stats.QueuePeak,
		BufferUsed:    len(a.input),
	}

	a.mu.RLock()
	stats.WriteLatency = a.stats.WriteLatency
	stats.LastFlush = a.stats.LastFlush
	a.mu.RUnlock()

	return stats
}

// IsHealthy 健康检查
func (a *AsyncLoggerChannel) IsHealthy() bool {
	if a.closed.Load() {
		return false
	}

	// 检查队列是否过载（超过80%容量）
	queueLen := len(a.input)
	capacity := a.config.BufferSize
	loadFactor := float64(queueLen) / float64(capacity)

	return loadFactor < 0.8
}

// GetLoadFactor 获取当前负载因子
func (a *AsyncLoggerChannel) GetLoadFactor() float64 {
	queueLen := len(a.input)
	capacity := a.config.BufferSize
	return float64(queueLen) / float64(capacity)
}

// ==================== 消费循环与批处理 ====================

// run 异步处理循环
func (a *AsyncLoggerChannel) run() {
	defer a.wg.Done()

	// 批处理缓冲区
	batch := a.batchPool.Get().(*batchEntries)
	defer a.batchPool.Put(batch)

	// 定时器
	ticker := time.NewTicker(a.config.FlushInterval)
	defer ticker.Stop()

	// 最后写入时间
	var lastWriteTime time.Time

	flush := func() {
		if len(batch.entries) == 0 {
			return
		}

		start := time.Now()
		a.writeBatch(batch)
		latency := time.Since(start)

		// 更新统计
		a.mu.Lock()
		a.stats.WriteLatency = latency
		a.stats.LastFlush = time.Now()
		a.mu.Unlock()

		// 重置批处理
		batch.entries = batch.entries[:0]
		lastWriteTime = time.Now()
	}

	for {
		select {
		case entry, ok := <-a.input:
			if !ok {
				// Channel关闭，刷写剩余数据
				flush()
				return
			}

			// 添加到批处理
			batch.entries = append(batch.entries, entry)

			// 更新峰值
			queueLen := len(a.input)
			currentPeak := atomic.LoadInt64(&a.stats.QueuePeak)
			if int64(queueLen) > currentPeak {
				atomic.CompareAndSwapInt64(&a.stats.QueuePeak, currentPeak, int64(queueLen))
			}

			// 检查是否需要立即刷新
			if batch.size() >= a.config.FlushThreshold {
				flush()
			}

		case <-ticker.C:
			// 定时刷新
			if time.Since(lastWriteTime) >= a.config.FlushInterval {
				flush()
			}

		case callback := <-a.flushSig:
			flush()
			// 关闭回调 channel（发送方通过检查 channel 是否关闭来判断）
			close(callback)
		}
	}
}

// batchEntries 批处理条目容器
type batchEntries struct {
	entries []logEntry
}

// size 计算批处理大小（估算字节数）
func (b *batchEntries) size() int {
	size := 0
	for _, e := range b.entries {
		size += len(e.msg) + 64 // 消息长度 + 估算头部
	}
	return size
}

// writeBatch 写入一批日志（优化：合并为单次 Write 调用，减少系统调用开销）
func (a *AsyncLoggerChannel) writeBatch(batch *batchEntries) {
	// 估算总大小，减少扩容
	estSize := 0
	for _, entry := range batch.entries {
		estSize += len(entry.msg) + 64 // 消息 + 估算头部
	}

	var buf strings.Builder
	buf.Grow(estSize)

	for _, entry := range batch.entries {
		buf.WriteString(a.formatEntry(entry))
	}

	// 单次 Write 调用
	data := buf.String()
	if _, err := a.writer.Write([]byte(data)); err != nil {
		fmt.Fprintf(os.Stderr, "async log write error: %v\n", err)
	} else {
		writtenCount := int64(len(batch.entries))
		a.written.Add(writtenCount)
		a.stats.TotalWritten += writtenCount
	}
}

// formatEntry 格式化日志条目（使用 strings.Builder 优化字符串拼接）
func (a *AsyncLoggerChannel) formatEntry(entry logEntry) string {
	levelStr := "INFO"
	switch entry.level {
	case slog.LevelDebug:
		levelStr = "DEBUG"
	case slog.LevelWarn:
		levelStr = "WARN"
	case slog.LevelError:
		levelStr = "ERROR"
	}

	// 估算容量：时间(19) + 空格 + 级别(5) + 括号 + 文件路径(估) + 消息 + 换行
	estLen := 32 + len(entry.file) + 8 + len(entry.msg)
	if len(entry.attrs) > 0 {
		for _, attr := range entry.attrs {
			estLen += len(attr.Key) + 16
		}
	}

	var sb strings.Builder
	sb.Grow(estLen)
	sb.WriteString(entry.time.Format(time.DateTime))
	sb.WriteByte(' ')
	sb.WriteString(levelStr)
	sb.WriteString(" [")
	if entry.file != "" {
		sb.WriteString(entry.file)
		sb.WriteByte(':')
		sb.WriteString(fmt.Sprintf("%d", entry.line))
		sb.WriteByte(' ')
	}
	sb.WriteString(entry.msg)
	sb.WriteByte(']')

	if len(entry.attrs) > 0 {
		for _, attr := range entry.attrs {
			sb.WriteByte(' ')
			sb.WriteString(attr.Key)
			sb.WriteByte('=')
			fmt.Fprintf(&sb, "%v", attr.Value.Any())
		}
	}

	sb.WriteByte('\n')
	return sb.String()
}
