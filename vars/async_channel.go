package vars

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
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
// 一次 runtime.Callers 采集整条栈、一次 CallersFrames 展开，跳过本包（vars）内部
// 帧，返回第一个不属于 vars 的调用者——即真正发起日志的业务代码位置。
//
// 旧实现对 depth=1..15 逐个调用 runtime.Caller：每次都要从栈顶重新走一遍（合计
// O(深度²)），且每次 Callers/FuncForPC 各分配一次，本机实测单条日志 4.3µs / 15 次
// 分配。同时判定用的是 Contains("/vars.")，任何路径里带 /vars. 的第三方包
// （example.com/vars.Foo）都会被误当成本包帧而跳过，定位到更外层。
func getCaller() (string, int) {
	// 采集代价与窗口大小成正比（从栈顶逐帧走到窗口上限）：常规链只有个位数 vars
	// 内部帧，先用小窗口；小窗口采满仍未见业务帧才说明链异常深，再用大窗口重来。
	// 两个窗口各自定长，小窗口不参与大窗口的逃逸分析，常态路径不会把 64 帧的数组
	// 顶上堆。
	var fast [callerFastFrames]uintptr
	if file, line, ok := expandCaller(fast[:]); ok {
		return file, line
	}

	var full [callerFrameLimit]uintptr
	if file, line, ok := expandCaller(full[:]); ok {
		return file, line
	}
	return "", 0
}

// expandCaller 展开 pcs 采到的帧，返回第一个不属于 vars 包的调用者位置。
// ok=false 表示窗口内全是本包帧（或整条栈都不属于业务），调用方应换更大的窗口重试。
func expandCaller(pcs []uintptr) (string, int, bool) {
	n := runtime.Callers(2, pcs)
	if n == 0 {
		return "", 0, false
	}

	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.File != "" && !strings.HasPrefix(frame.Function, varsFuncPrefix) {
			return normalizeCallerFile(frame.File), frame.Line, true
		}
		if !more {
			return "", 0, false
		}
	}
}

// 小窗口覆盖 vars 内部帧（Info → writeToFile → LogAsyncSimple → EnqueueSimple →
// Enqueue → getCaller）加余量；采满仍未命中就退回大窗口重扫。
const (
	callerFastFrames = 8
	callerFrameLimit = 64
)

// normalizeCallerFile 统一路径分隔符：Windows 下与系统文件路径风格一致。
// 只有真的含正斜杠时才重建字符串，同一调用点反复打日志时不再每次分配。
func normalizeCallerFile(file string) string {
	if isWindows && strings.Contains(file, "/") {
		return strings.ReplaceAll(file, "/", "\\")
	}
	return file
}

const isWindows = runtime.GOOS == "windows"

// varsFuncPrefix 本包函数名的公共前缀（形如 "touchgocore/vars."），用于精确判定
// 「这一帧是不是 vars 内部帧」。前缀在包初始化时从自身函数名反推，换模块路径或
// 被 vendor 后依然成立。
var varsFuncPrefix = computeVarsFuncPrefix()

func computeVarsFuncPrefix() string {
	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return ""
	}
	name := fn.Name() // 形如 touchgocore/vars.computeVarsFuncPrefix
	tail := name[strings.LastIndexByte(name, '/')+1:]
	if i := strings.IndexByte(tail, '.'); i >= 0 {
		return name[:len(name)-len(tail)+i+1]
	}
	return name
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

// batchEntries 批处理条目容器。
//
// bytes 随 append 增量维护：旧实现每收到一条日志就调用 size() 重算整批字节数，
// 一批 n 条累计 O(n²) 次遍历，默认阈值（4KB ≈ 100 条）下每批白走近 5000 圈。
type batchEntries struct {
	entries []logEntry
	bytes   int
}

// entryHeaderBytes 时间、级别、括号、路径与行号的估算头部
const entryHeaderBytes = 64

func (b *batchEntries) add(entry logEntry) {
	b.entries = append(b.entries, entry)
	b.bytes += len(entry.msg) + entryHeaderBytes
}

// reset 清空批次。
//
// clear 逐槽置零是必须的：entries[:0] 只把长度归零，底层数组依旧攥着这一批的
// msg / attrs / context，而容器来自 sync.Pool，被钉住的整批字符串最长要拖到下一
// 批填满才可能释放。
func (b *batchEntries) reset() {
	clear(b.entries)
	b.entries = b.entries[:0]
	b.bytes = 0
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
		buf = a.appendEntry(buf, entry)
	}

	_, err := a.writer.Write(buf)
	a.scratch = buf
	if err != nil {
		fmt.Fprintf(os.Stderr, "async log write error: %v\n", err)
		return
	}
	a.written.Add(int64(len(batch.entries)))
}

// levelString 日志级别的展示文本，与历史输出逐字一致。
func levelString(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelWarn:
		return "WARN"
	case slog.LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// appendEntry 把一条日志按既有格式追加到 buf 尾部并返回扩容后的切片。
// 输出与旧的 formatEntry 完全一致，只是不再为每条日志单独分配字符串。
func (a *AsyncLoggerChannel) appendEntry(buf []byte, entry logEntry) []byte {
	buf = entry.time.AppendFormat(buf, time.DateTime)
	buf = append(buf, ' ')
	buf = append(buf, levelString(entry.level)...)
	buf = append(buf, " ["...)
	if entry.file != "" {
		buf = append(buf, entry.file...)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(entry.line), 10)
		buf = append(buf, ' ')
	}
	buf = append(buf, entry.msg...)
	buf = append(buf, ']')

	for _, attr := range entry.attrs {
		buf = append(buf, ' ')
		buf = append(buf, attr.Key...)
		buf = append(buf, '=')
		buf = fmt.Appendf(buf, "%v", attr.Value.Any())
	}

	return append(buf, '\n')
}
