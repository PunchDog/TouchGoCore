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
	config   AsyncChannelConfig
	input    chan logEntry      // 日志输入channel（永不 close，由 stop 通知消费者退出）
	stop     chan struct{}      // 停止信号，Close 时关闭一次
	writer   io.Writer          // 实际写入器
	closed   atomic.Bool        // 关闭标志
	stopping atomic.Bool        // 正在停止中
	wg       sync.WaitGroup     // goroutine同步

	// 统计字段全部使用原子类型：入队方是多协程、消费协程是单协程，
	// 任何裸 int64 字段都会在 Enqueue/GetStats/run 之间构成数据竞争。
	queued       atomic.Int64 // 入队日志数
	written      atomic.Int64 // 写入日志数
	dropped      atomic.Int64 // 丢弃的日志数
	queuePeak    atomic.Int64 // 队列峰值
	writeLatency atomic.Int64 // 最近一次批量写入耗时（ns）
	lastFlushNs  atomic.Int64 // 上次刷新时间（UnixNano，0 表示从未刷新）

	flushSig  chan chan struct{} // 刷新信号；nil 表示无需回执，非 nil 时由消费者写入信号值
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
		stop:     make(chan struct{}),
		writer:   writer,
		flushSig: make(chan chan struct{}, 1), // 传递回调 channel（nil 表示无需回执）
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
			return false
		}
	}

	// 阻塞模式：等待写入；关闭进行中立即放弃，避免消费者退出后卡满超时
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case a.input <- entry:
		return true
	case <-a.stop:
		a.dropped.Add(1)
		return false
	case <-timer.C:
		a.dropped.Add(1)
		return false
	}
}

// EnqueueSimple 简化版入队（无attrs）
func (a *AsyncLoggerChannel) EnqueueSimple(level slog.Level, msg string) bool {
	return a.Enqueue(level, msg, nil, nil)
}

// Flush 手动刷新缓冲区（不带回调）
func (a *AsyncLoggerChannel) Flush() error {
	if a.closed.Load() {
		return fmt.Errorf("channel is closed")
	}

	// 发送刷新信号（nil 表示无需回执，消费者不会 close 任何共享 channel）
	select {
	case a.flushSig <- nil:
	default:
	}
	return nil
}

// FlushAndWait 刷新并等待完成
func (a *AsyncLoggerChannel) FlushAndWait(timeout time.Duration) error {
	if a.closed.Load() {
		return fmt.Errorf("channel is closed")
	}

	done := make(chan struct{}, 1)

	// 用可停止的定时器：time.After 在成功路径上仍会残留一个直到超时才回收的定时器
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case a.flushSig <- done:
	case <-a.stop:
		return fmt.Errorf("channel is closed")
	case <-timer.C:
		return fmt.Errorf("flush signal busy after %v", timeout)
	}

	timer.Reset(timeout)
	select {
	case <-done:
		return nil
	case <-a.stop:
		return fmt.Errorf("channel is closed")
	case <-timer.C:
		return fmt.Errorf("flush timeout after %v", timeout)
	}
}

// Close 停止接收新日志，等待队列排空并写完后关闭底层 writer。
// 数据通道 input 不再 close：消费者靠 stop 退出并主动排空 input，
// 从而避免与并发 Enqueue 竞争导致的 send-on-closed panic。
func (a *AsyncLoggerChannel) Close() error {
	if !a.closed.CompareAndSwap(false, true) {
		return nil // 已经关闭
	}

	close(a.stop)

	// 等待goroutine结束（带超时）
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	var waitErr error
	select {
	case <-done:
		// 消费者退出后，仍可能有在途条目刚刚完成入队，由关闭方补写，
		// 否则这些日志既不计入落盘也不计入丢弃，静默消失
		a.drainStragglers()
	case <-time.After(10 * time.Second):
		waitErr = fmt.Errorf("close timeout, some logs may not be written")
	}

	// 关闭底层writer
	if closer, ok := a.writer.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			if waitErr != nil {
				return fmt.Errorf("%w; %v", waitErr, err)
			}
			return err
		}
	}

	return waitErr
}

// drainStragglers 在消费者退出后补写 input 中的在途条目（单协程调用）
func (a *AsyncLoggerChannel) drainStragglers() {
	if len(a.input) == 0 {
		return
	}

	batch := a.batchPool.Get().(*batchEntries)
	defer func() {
		batch.entries = batch.entries[:0]
		a.batchPool.Put(batch)
	}()

	for {
		select {
		case entry := <-a.input:
			batch.entries = append(batch.entries, entry)
		default:
			if len(batch.entries) > 0 {
				a.writeBatch(batch)
			}
			return
		}
	}
}

// GetStats 获取统计信息。
// 各字段各自原子，跨字段不保证同一瞬间的一致性（例如 TotalEnqueued 可能比
// TotalWritten+TotalDropped 多出仍在队列中的在途条目）。
func (a *AsyncLoggerChannel) GetStats() AsyncChannelStats {
	stats := AsyncChannelStats{
		TotalEnqueued: a.queued.Load(),
		TotalWritten:  a.written.Load(),
		TotalDropped:  a.dropped.Load(),
		QueuePeak:     a.queuePeak.Load(),
		BufferUsed:    len(a.input),
		WriteLatency:  time.Duration(a.writeLatency.Load()),
	}
	if ns := a.lastFlushNs.Load(); ns > 0 {
		stats.LastFlush = time.Unix(0, ns)
	}
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
		a.writeLatency.Store(int64(latency))
		a.lastFlushNs.Store(time.Now().UnixNano())

		// 重置批处理
		batch.entries = batch.entries[:0]
		lastWriteTime = time.Now()
	}

	// 排空 input 中剩余条目并写入（关闭流程与停止信号共用）
	drainInput := func() {
		for {
			select {
			case entry := <-a.input:
				batch.entries = append(batch.entries, entry)
				if batch.size() >= a.config.FlushThreshold {
					flush()
				}
			default:
				flush()
				return
			}
		}
	}

	for {
		select {
		case <-a.stop:
			drainInput()
			return

		case entry := <-a.input:
			// 添加到批处理
			batch.entries = append(batch.entries, entry)

			// 更新峰值
			a.recordQueuePeak(len(a.input))

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
			// 先排空已提交的入队再回执，否则 FlushAndWait 可能在条目尚未
			// 被消费时就返回，调用方以为日志已落盘
			drainInput()
			// 通知调用方：向回调 channel 写入信号值而非 close，
			// 使同一 channel 可被多次安全通知，也不存在重复关闭风险。
			if callback != nil {
				select {
				case callback <- struct{}{}:
				default:
				}
			}
		}
	}
}

// batchEntries 批处理条目容器
type batchEntries struct {
	entries []logEntry
}

// recordQueuePeak 单调记录队列深度峰值
func (a *AsyncLoggerChannel) recordQueuePeak(queueLen int) {
	for {
		old := a.queuePeak.Load()
		if int64(queueLen) <= old || a.queuePeak.CompareAndSwap(old, int64(queueLen)) {
			return
		}
	}
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
		a.written.Add(int64(len(batch.entries)))
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
