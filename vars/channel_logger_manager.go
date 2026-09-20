package vars

import (
	"fmt"
	"io"
	"log/slog"
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
	slogHandler *OptimizedZapSlogHandler
	zapLogger   *zap.Logger
	isEnabled   atomic.Bool
	mu          sync.RWMutex
	writer      io.WriteCloser

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

// initLocked 真正的初始化流程，调用方必须持有 m.mu 写锁
func (m *ChannelLoggerManager) initLocked() error {
	// 解析并记录文件写入级别与 off 状态（供 Info/Warning/Error 等函数按等级过滤文件写入）
	m.writeLevel.Store(int32(parseLogLevel(m.config.LogLevel)))
	m.off.Store(strings.EqualFold(m.config.LogLevel, LogLevelOff))

	// 检查是否禁用
	if m.config.Async && m.config.AsyncBufferSize > 0 {
		m.isEnabled.Store(true)
		return nil
	}

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
	m.isEnabled.Store(true)

	return nil
}

// LogAsync 异步记录日志（通过Channel）
func (m *ChannelLoggerManager) LogAsync(level slog.Level, msg string, attrs ...slog.Attr) {
	channel := m.channel.Load()
	if !m.isEnabled.Load() || channel == nil {
		return
	}

	channel.Enqueue(level, msg, attrs, nil)
}

// LogAsyncSimple 简单异步日志（消息应已格式化）
func (m *ChannelLoggerManager) LogAsyncSimple(level slog.Level, msg string) {
	channel := m.channel.Load()
	if !m.isEnabled.Load() || channel == nil {
		return
	}

	channel.EnqueueSimple(level, msg)
}

// GetLogger 获取slog.Logger
func (m *ChannelLoggerManager) GetLogger() *slog.Logger {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.slogHandler != nil {
		return slog.New(m.slogHandler)
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

// SetLevel 动态设置日志级别（需要重新初始化）
func (m *ChannelLoggerManager) SetLevel(level string) error {
	if _, err := zapLevelFor(level); err != nil {
		return err
	}

	// 锁内摘走旧资源引用（init 会再次取锁，持锁调用将自死锁；channel.Close 会等在途日志，也必须在锁外）
	m.mu.Lock()
	m.config.LogLevel = level
	oldWriter, oldAsyncWriter := m.writer, m.asyncWriter
	m.writer, m.asyncWriter = nil, nil
	m.slogHandler = nil
	m.mu.Unlock()

	oldChannel := m.channel.Swap(nil)

	if oldChannel != nil {
		oldChannel.Close()
	}
	if oldAsyncWriter != nil {
		oldAsyncWriter.Close()
	}
	if oldWriter != nil {
		oldWriter.Close()
	}

	// 重新初始化；异步模式下 initLocked 只置标志位，必须把 Channel 一并重建，
	// 否则换绑后的 writer 没有生产者，日志会静默全丢。
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.initLocked(); err != nil {
		return err
	}
	if m.config.Async && m.config.AsyncBufferSize > 0 {
		return m.initWithChannelLocked()
	}
	return nil
}

// ==================== 全局Channel日志管理器 ====================

var (
	globalChannelLogger *ChannelLoggerManager
	channelOnce         sync.Once
)

// InitializeChannelLogger 初始化全局Channel日志管理器
func InitializeChannelLogger(cfg LogConfig) error {
	var initErr error
	channelOnce.Do(func() {
		manager, err := NewChannelLoggerManager(cfg)
		if err != nil {
			initErr = err
			return
		}

		// 如果启用异步，使用Channel模式
		if cfg.Async {
			initErr = manager.initWithChannel()
			if initErr != nil {
				return
			}
		}

		globalChannelLogger = manager
		if globalChannelLogger.IsEnabled() {
			slog.SetDefault(globalChannelLogger.GetLogger())
		}
	})

	return initErr
}

// InitializeChannelLoggerWithDefaults 使用默认配置初始化
func InitializeChannelLoggerWithDefaults() error {
	cfg := DefaultConfig()
	cfg.Async = true // 默认启用异步
	return InitializeChannelLogger(cfg)
}

// ShutdownChannelLogger 关闭全局Channel日志管理器
func ShutdownChannelLogger() error {
	if globalChannelLogger == nil {
		return nil
	}

	// 刷新
	_ = globalChannelLogger.Flush()

	return globalChannelLogger.Close()
}

// GetChannelLogger 获取全局Channel日志管理器
func GetChannelLogger() *ChannelLoggerManager {
	return globalChannelLogger
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
