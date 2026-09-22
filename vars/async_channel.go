package vars

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	input    chan logEntry  // 日志输入channel（永不 close，由 stop 通知消费者退出）
	stop     chan struct{}  // 停止信号，Close 时关闭一次
	writer   io.Writer      // 实际写入器
	closed   atomic.Bool    // 关闭标志
	stopping atomic.Bool    // 正在停止中
	wg       sync.WaitGroup // goroutine同步

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

	// scratch 是消费者协程私有的格式化缓冲（只有 run / drainStragglers 会碰），
	// 不入池、不加锁：批量写盘每批省掉一次整批容量的分配与两次全量拷贝。
	scratch []byte
}

// NewAsyncLoggerChannel 创建异步日志Channel
//
// writer 契约：批量落盘时传入 Write 的切片由本通道复用（下一次写入会原地覆写
// 其内容），实现不得保留 p，只能在调用期间读取或自行拷贝。标准库 io.Writer 已
// 有此要求，此处明示是因为自定义 writer 最常见的违规写法就是 `buf = append(buf, p...)`
// 之后异步消费——那样会读到脏数据。仓库内置的 RotatingFileWriter 与
// AsyncWriteSyncer 都在 Write 内同步落盘或先拷贝，满足该契约。
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
	file, line := getCaller()
	return a.enqueue(logEntry{
		level:   level,
		msg:     msg,
		time:    time.Now(),
		attrs:   attrs,
		context: ctx,
		file:    file,
		line:    line,
	})
}

// enqueue 投递一条已备好的条目。调用点解析与投递分开，是因为走 slog 入口的条目
// 带的是 Record.PC（见 callerFromPC），不能扫栈。
func (a *AsyncLoggerChannel) enqueue(entry logEntry) bool {
	if a.closed.Load() {
		return false
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
		batch.reset()
		a.batchPool.Put(batch)
	}()

	for {
		select {
		case entry := <-a.input:
			batch.add(entry)
			// 分批写出：一次性把整个残留队列格式化进一块缓冲，峰值内存按队列容量
			// （默认 10000 条）放大，退出路径上反而最容易触发 OOM。
			if batch.bytes >= a.config.FlushThreshold {
				a.writeBatch(batch)
				batch.reset()
			}
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
	defer func() {
		// 归还前必须清空：否则池里那份容器带着上一批的条目与字节数，下一位认领者
		// 既钉着整批字符串，又会从非零长度、非零 bytes 开始累积。
		batch.reset()
		a.batchPool.Put(batch)
	}()

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
		batch.reset()
		lastWriteTime = time.Now()
	}

	// 排空 input 中剩余条目并写入（关闭流程与停止信号共用）
	drainInput := func() {
		for {
			select {
			case entry := <-a.input:
				batch.add(entry)
				if batch.bytes >= a.config.FlushThreshold {
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
			batch.add(entry)

			// 更新峰值
			a.recordQueuePeak(len(a.input))

			// 检查是否需要立即刷新
			if batch.bytes >= a.config.FlushThreshold {
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

// recordQueuePeak 单调记录队列深度峰值
func (a *AsyncLoggerChannel) recordQueuePeak(queueLen int) {
	for {
		old := a.queuePeak.Load()
		if int64(queueLen) <= old || a.queuePeak.CompareAndSwap(old, int64(queueLen)) {
			return
		}
	}
}

// writeBatch 写入一批日志：整批格式化进同一块缓冲，只做一次 Write 调用。
//
// buf 复用消费者协程私有的 a.scratch：本方法只由 run 与 drainStragglers 调用，
// 两者都在同一个协程上，无并发；io.Writer 契约本就要求实现不得保留传入的 p。
// 旧实现是 strings.Builder → String()（拷一次）→ []byte(string)（再拷一次）。
func (a *AsyncLoggerChannel) writeBatch(batch *batchEntries) {
	if len(batch.entries) == 0 {
		return
	}

	buf := a.scratch[:0]
	if est := batch.bytes; cap(buf) < est {
		buf = make([]byte, 0, est+est/4)
	}
	for _, entry := range batch.entries {
		buf = appendEntry(buf, entry)
	}

	_, err := a.writer.Write(buf)
	a.scratch = buf
	if err != nil {
		fmt.Fprintf(os.Stderr, "async log write error: %v\n", err)
		return
	}
	a.written.Add(int64(len(batch.entries)))
}
