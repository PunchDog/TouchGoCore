package vars

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// ==================== 带Channel的日志管理器 ====================

// ChannelLoggerManager 基于异步Channel的日志管理器
type ChannelLoggerManager struct {
	config      LogConfig
	slogHandler slog.Handler
	zapLogger   *zap.Logger
	isEnabled   atomic.Bool
	mu          sync.RWMutex
	writer      io.WriteCloser

	// channelMode：本管理器跑的是「异步通道 + 旋转写入器」还是「zap 直写」。
	// 两条路的落地格式本就不同（文本 / JSON），降级分支必须按模式选，
	// 不能在有通道模式时突然写出 zap 的 JSON。
	channelMode atomic.Bool

	// asyncWriter 是 zap 的异步落盘包装，必须在 zap 之后、writer 之前关闭，
	// 否则缓冲区里的日志会随进程退出丢失（createOptimizedZapCore 内部创建，需回传持有）。
	asyncWriter *AsyncWriteSyncer

	// channel 由 SetLevel/Close 换绑、由任意业务协程读取，必须原子快照
	channel atomic.Pointer[AsyncLoggerChannel]

	writeLevel atomic.Int32 // 文件写入的最低级别（低于该级别的日志不写入文件）；off 时用高哨兵值禁用
	off        atomic.Bool  // 日志级别是否被配置为 off（完全静默）
}

// NewChannelLoggerManager 创建基于Channel的日志管理器
func NewChannelLoggerManager(cfg LogConfig) (*ChannelLoggerManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid log config: %w", err)
	}

	manager := &ChannelLoggerManager{
		config: cfg,
	}

	if err := manager.init(); err != nil {
		return nil, err
	}

	return manager, nil
}

// init 初始化
func (m *ChannelLoggerManager) init() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initLocked()
}

// applyLevelLocked 按当前 config.LogLevel 刷新文件写入的过滤位。调用方须持 m.mu。
func (m *ChannelLoggerManager) applyLevelLocked() {
	// 解析并记录文件写入级别与 off 状态（供 Info/Warning/Error 等函数按等级过滤文件写入）
	m.writeLevel.Store(int32(parseLogLevel(m.config.LogLevel)))
	m.off.Store(strings.EqualFold(m.config.LogLevel, LogLevelOff))
}

// initLocked 真正的初始化流程，调用方必须持有 m.mu 写锁
func (m *ChannelLoggerManager) initLocked() error {
	m.applyLevelLocked()
	// 检查是否禁用
	if m.config.Async && m.config.AsyncBufferSize > 0 {
		// 通道模式：zap 不参与，落盘由 initWithChannelLocked 建的旋转写入器 +
		// AsyncLoggerChannel 负责。先置模式再置 isEnabled，让降级分支在
		// 「通道还没建好」的窗口里也知道该走文本而不是 zap。
		m.channelMode.Store(true)
		m.isEnabled.Store(true)
		return nil
	}

	m.channelMode.Store(false)

	// 创建优化的Zap核心
	core, writer, asyncWriter, err := createOptimizedZapCore(m.config)
	if err != nil {
		return err
	}

	m.writer = writer
	m.asyncWriter = asyncWriter

	// 创建Zap logger
	m.zapLogger = zap.New(core,
		zap.AddCaller(),
		zap.AddCallerSkip(m.config.CallerSkip),
	)

	// 创建slog处理器（off 由 parseLogLevel 折成高于所有级别的哨兵，不再走 UnmarshalText 报错）
	m.slogHandler = NewOptimizedZapSlogHandler(m.zapLogger, parseLogLevel(m.config.LogLevel), m.config.CallerSkip)

	m.isEnabled.Store(true)
	return nil
}

// initWithChannel 使用Channel模式初始化
func (m *ChannelLoggerManager) initWithChannel() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initWithChannelLocked()
}

// initWithChannelLocked 建立旋转写入器与异步Channel，调用方必须持有 m.mu 写锁
func (m *ChannelLoggerManager) initWithChannelLocked() error {
	// 创建旋转写入器
	rotatingWriter, err := NewRotatingFileWriter(
		m.config.LogPath,
		m.config.LogName,
		m.config.MaxSize*1024*1024,
		m.config.MaxAge,
		m.config.MaxBackups,
		m.config.Compress,
	)
	if err != nil {
		return err
	}

	m.writer = rotatingWriter

	// 创建异步Channel
	channelConfig := DefaultAsyncChannelConfig()
	channelConfig.BufferSize = m.config.AsyncBufferSize
	channelConfig.DropOnFull = false // 阻塞模式，保证不丢日志

	m.channel.Store(NewAsyncLoggerChannel(rotatingWriter, channelConfig))
	m.slogHandler = newChannelSlogHandler(m)
	m.channelMode.Store(true)
	m.isEnabled.Store(true)

	return nil
}

// LogAsync 异步记录日志（通过Channel）
func (m *ChannelLoggerManager) LogAsync(level slog.Level, msg string, attrs ...slog.Attr) {
	m.writeLogged(level, msg, attrs)
}

// LogAsyncSimple 简单异步日志（消息应已格式化）
func (m *ChannelLoggerManager) LogAsyncSimple(level slog.Level, msg string) {
	m.writeLogged(level, msg, nil)
}

// writeLogged 文件落地的唯一入口（S70）。
//
// 三条分支都只认这一个管理器手里的 writer，不存在「另开一个日志管理器、再开一个
// 文件句柄」的旁路：旁路的句柄不认识旋转，日志轮转之后它还继续往已被改名的备份文件
// 里写，主文件从此看不出后一半现场。
//
// 级别过滤放在这里而不是调用方：异步通道自己不看级别，此前绕过门面直接调
// LogAsyncSimple 的调用方能把 debug 灌进 info 级别的文件里。
func (m *ChannelLoggerManager) writeLogged(level slog.Level, msg string, attrs []slog.Attr) {
	if !m.accepts(level) {
		return
	}
	file, line := getCaller()
	m.submit(logEntry{level: level, msg: msg, time: time.Now(), attrs: attrs, file: file, line: line})
}

// enqueueRecord 投递一条调用点已解析好的记录（slog 入口带 Record.PC，不能再扫栈）。
func (m *ChannelLoggerManager) enqueueRecord(entry logEntry) {
	if !m.accepts(entry.level) {
		return
	}
	m.submit(entry)
}

// accepts 该级别的日志是否需要写文件
func (m *ChannelLoggerManager) accepts(level slog.Level) bool {
	return m.isEnabled.Load() && !m.off.Load() && m.ShouldWriteFile(level)
}

// submit 按当前模式选落地路径：
//   - 通道在：投递给消费者（常态）；
//   - 通道模式但通道不在（SetLevel 换绑窗口、Close 之后仍有零星调用）：同步写同一个
//     文件句柄，格式与消费者逐字相同；
//   - 非通道模式：走 zap，写出该模式本来的 JSON。
func (m *ChannelLoggerManager) submit(entry logEntry) {
	if channel := m.channel.Load(); channel != nil {
		channel.enqueue(entry)
		return
	}
	if !m.channelMode.Load() {
		m.logViaHandler(entry)
		return
	}
	m.writeEntrySync(entry)
}

// writeEntrySync 通道缺席时同步落到同一个文件句柄。
// 旋转写入器自带锁且写入即追加，与消费者协程并存时不会互相截断。
func (m *ChannelLoggerManager) writeEntrySync(entry logEntry) {
	m.mu.RLock()
	writer := m.writer
	m.mu.RUnlock()
	if writer == nil {
		return
	}

	if _, err := writer.Write(appendEntry(nil, entry)); err != nil {
		fmt.Fprintf(os.Stderr, "sync log write error: %v\n", err)
	}
}

// logViaHandler 非通道模式（zap 直写）下落地：属性照原样带进记录，不因为换路径就丢字段
func (m *ChannelLoggerManager) logViaHandler(entry logEntry) {
	m.mu.RLock()
	handler := m.slogHandler
	m.mu.RUnlock()
	if handler == nil {
		return
	}

	record := slog.NewRecord(entry.time, entry.level, entry.msg, 0)
	record.AddAttrs(entry.attrs...)
	_ = handler.Handle(entry.context, record)
}

// GetLogger 获取slog.Logger。
//
// 通道模式下它返回接进通道的那个 handler（S70）：此前 slogHandler 只在非异步分支里
// 建，异步模式下恒为 nil，于是 slog.SetDefault(m.GetLogger()) 等于把默认器换成
// slog.Default() 自己，slog.Info 一类调用永远进不了日志文件——门面 vars.Info 走的是
// 另一条路（LogAsyncSimple），所以只有直接用 slog 的宿主会踩到。
func (m *ChannelLoggerManager) GetLogger() *slog.Logger {
	m.mu.RLock()
	handler := m.slogHandler
	m.mu.RUnlock()

	if handler != nil {
		return slog.New(handler)
	}

	return slog.Default()
}

// Flush 刷新日志
func (m *ChannelLoggerManager) Flush() error {
	if channel := m.channel.Load(); channel != nil {
		return channel.FlushAndWait(5 * time.Second)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.asyncWriter != nil {
		return m.asyncWriter.Sync()
	}
	if m.zapLogger != nil {
		return m.zapLogger.Sync()
	}

	return nil
}

// Close 关闭日志管理器
func (m *ChannelLoggerManager) Close() error {
	if !m.isEnabled.CompareAndSwap(true, false) {
		return nil
	}

	// 关闭Channel（会等待在途日志写完，不能持锁执行）
	if channel := m.channel.Swap(nil); channel != nil {
		if err := channel.Close(); err != nil {
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 刷新Zap logger
	if m.zapLogger != nil {
		_ = m.zapLogger.Sync()
	}

	// 再关 zap 的异步包装，把缓冲区里最后一段日志落到文件
	if m.asyncWriter != nil {
		if err := m.asyncWriter.Close(); err != nil {
			return err
		}
		m.asyncWriter = nil
	}

	// 最后关闭底层writer
	if m.writer != nil {
		if err := m.writer.Close(); err != nil {
			return err
		}
		m.writer = nil
	}

	return nil
}

// IsEnabled 检查是否启用
func (m *ChannelLoggerManager) IsEnabled() bool {
	return m.isEnabled.Load()
}

// parseLogLevel 将配置字符串解析为 slog.Level。
// off 返回高于所有真实级别的值，用于禁用一切文件写入。
func parseLogLevel(level string) slog.Level {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	case "OFF":
		return slog.Level(1000) // 高于任何真实日志级别，表示不写入文件
	default:
		return slog.LevelInfo
	}
}

// IsOff 判断日志级别是否被配置为 off（完全静默，命令行与文件均不输出）
func (m *ChannelLoggerManager) IsOff() bool {
	return m.off.Load()
}

// ShouldWriteFile 判断给定级别是否达到文件写入的最低级别（低于则不写文件）
func (m *ChannelLoggerManager) ShouldWriteFile(level slog.Level) bool {
	return level >= slog.Level(m.writeLevel.Load())
}

// GetStats 获取Channel统计
func (m *ChannelLoggerManager) GetStats() AsyncChannelStats {
	if channel := m.channel.Load(); channel != nil {
		return channel.GetStats()
	}
	return AsyncChannelStats{}
}

// IsHealthy 健康检查
func (m *ChannelLoggerManager) IsHealthy() bool {
	if channel := m.channel.Load(); channel != nil {
		return channel.IsHealthy()
	}
	return m.isEnabled.Load()
}

// GetLoadFactor 获取负载因子
func (m *ChannelLoggerManager) GetLoadFactor() float64 {
	if channel := m.channel.Load(); channel != nil {
		return channel.GetLoadFactor()
	}
	return 0
}

// SetLevel 动态设置日志级别。
//
// 通道模式（常态）只改过滤位：级别既不参与文件句柄也不参与异步通道的建立，为它重建
// 整条生产者—消费者链路是纯代价 —— 旧写法先把 writer 置空再换绑，而 Close 最长要等
// 在途日志落盘数秒，那段窗口里所有 writeEntrySync 都落在 nil writer 上被静默丢弃，
// 重建失败时更是留着 isEnabled=true 让此后每条日志都无声消失。
//
// 非通道模式必须重建：zap 的级别编译在 handler/core 里，改不动。这里先建新再关旧，
// 任一时刻 writer/slogHandler 都有一个活着，换绑期间日志照常落地；建新失败则原样返回
// 错误并继续用旧链路写，不会为了一个改不下去的级别把日志能力一起弄丢。
func (m *ChannelLoggerManager) SetLevel(level string) error {
	if _, err := zapLevelFor(level); err != nil {
		return err
	}

	m.mu.Lock()
	m.config.LogLevel = level
	if m.channelMode.Load() {
		m.applyLevelLocked()
		m.mu.Unlock()
		return nil
	}
	oldWriter, oldAsyncWriter, oldZap := m.writer, m.asyncWriter, m.zapLogger
	if err := m.initLocked(); err != nil {
		// createOptimizedZapCore 在赋值前就返回错误，旧链路原封不动
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// 新链路已就绪，此后没人再取得旧句柄（writeEntrySync 取的是锁内快照）
	if oldAsyncWriter != nil {
		_ = oldAsyncWriter.Close()
	}
	if oldWriter != nil {
		_ = oldWriter.Close()
	}
	if oldZap != nil {
		_ = oldZap.Sync()
	}
	return nil
}

// ==================== 全局Channel日志管理器 ====================

// globalChannelLogger 全库唯一的文件日志落地点。
//
// 写侧由 vars.go 的 loggerMu 串行（S70 起门面 Initialize/Shutdown 与这里的导出入口
// 共用同一把锁），但读侧不在锁内：GetChannelLogger 被每条日志调用，加锁等于把
// 「日志会不会自己排队」绑回一把全局锁。指针用 atomic 存取，Shutdown 与写日志并发时
// 读到的要么是旧管理器要么是 nil，绝不会是半个。
var globalChannelLogger atomic.Pointer[ChannelLoggerManager]

// initializeChannelLoggerLocked 建立全局管理器。调用方必须持有 loggerMu。
//
// 这里不再有 sync.Once：Once 让「Shutdown 之后重新 Initialize」静默返回 nil，
// 而宿主在测试里关一次再开一次是常态，旧写法会把第二次的配置整份丢掉。
// 是否已初始化改由 loggerActive 判定，放在 vars.Initialize 里。
func initializeChannelLoggerLocked(cfg LogConfig) (*ChannelLoggerManager, error) {
	manager, err := NewChannelLoggerManager(cfg)
	if err != nil {
		return nil, err
	}

	// 如果启用异步，使用Channel模式
	if cfg.Async {
		if err := manager.initWithChannel(); err != nil {
			// 半初始化：init 已经开了文件句柄（非异步分支）或 initWithChannel 开了旋转写入器，
			// 不关就等于每次初始化失败留一个 fd，且没人再持有它。
			_ = manager.Close()
			return nil, err
		}
	}

	globalChannelLogger.Store(manager)
	return manager, nil
}

// shutdownChannelLoggerLocked 刷新并关闭全局管理器，同时清空引用。调用方必须持有 loggerMu。
// 清空是「Shutdown 之后可再 Initialize」的后半：只关不清会让写日志的分支继续持有
// 一个已关闭的管理器，之后的调用全都落到已释放的 writer 上。
func shutdownChannelLoggerLocked() error {
	manager := globalChannelLogger.Swap(nil)
	if manager == nil {
		return nil
	}

	// 先冲刷在途日志；刷新失败（超时）也要继续 Close，否则文件句柄直接泄漏，
	// 而 Close 本身会再等一次消费者收敛。
	_ = manager.Flush()
	return manager.Close()
}

// InitializeChannelLogger 初始化全局Channel日志管理器。
// 与 vars.Initialize 同一条路径（S70）：装配 + 接管默认 slog 记录器，不再有第二份实现。
func InitializeChannelLogger(cfg LogConfig) error {
	loggerMu.Lock()
	defer loggerMu.Unlock()

	if loggerActive {
		return nil
	}
	manager, err := initializeChannelLoggerLocked(cfg)
	if err != nil {
		return err
	}
	loggerActive = true
	if manager.IsEnabled() {
		adoptSlogDefault(manager)
	}
	return nil
}

// InitializeChannelLoggerWithDefaults 使用默认配置初始化
func InitializeChannelLoggerWithDefaults() error {
	cfg := DefaultConfig()
	cfg.Async = true // 默认启用异步
	return InitializeChannelLogger(cfg)
}

// ShutdownChannelLogger 关闭全局Channel日志管理器（不含命令行缓冲，那是 vars.Shutdown 的职责）
func ShutdownChannelLogger() error {
	loggerMu.Lock()
	defer loggerMu.Unlock()

	loggerActive = false
	err := shutdownChannelLoggerLocked()
	releaseSlogDefault()
	return err
}

// GetChannelLogger 获取全局Channel日志管理器；未初始化（或已 Shutdown）返回 nil。
func GetChannelLogger() *ChannelLoggerManager {
	return globalChannelLogger.Load()
}

// ==================== 全局便捷函数 ====================

// LogAsync 全局异步日志
func LogAsync(level slog.Level, msg string, args ...any) {
	logger := GetChannelLogger()
	if logger != nil && logger.IsEnabled() {
		formattedMsg := msg
		if len(args) > 0 {
			formattedMsg = fmt.Sprintf(msg, args...)
		}
		logger.LogAsyncSimple(level, formattedMsg)
	}
}

// LogDebugAsync Debug异步日志
func LogDebugAsync(msg string, args ...any) {
	LogAsync(slog.LevelDebug, msg, args...)
}

// LogInfoAsync Info异步日志
func LogInfoAsync(msg string, args ...any) {
	LogAsync(slog.LevelInfo, msg, args...)
}

// LogWarnAsync Warn异步日志
func LogWarnAsync(msg string, args ...any) {
	LogAsync(slog.LevelWarn, msg, args...)
}

// LogErrorAsync Error异步日志
func LogErrorAsync(msg string, args ...any) {
	LogAsync(slog.LevelError, msg, args...)
}

// FlushAsyncLogs 刷新所有异步日志
func FlushAsyncLogs() error {
	logger := GetChannelLogger()
	if logger != nil {
		return logger.Flush()
	}
	return nil
}

// GetAsyncLogStats 获取异步日志统计
func GetAsyncLogStats() AsyncChannelStats {
	logger := GetChannelLogger()
	if logger != nil {
		return logger.GetStats()
	}
	return AsyncChannelStats{}
}

// IsAsyncLoggerHealthy 检查异步日志器健康状态
func IsAsyncLoggerHealthy() bool {
	logger := GetChannelLogger()
	if logger != nil {
		return logger.IsHealthy()
	}
	return true // 如果未初始化，认为健康
}
