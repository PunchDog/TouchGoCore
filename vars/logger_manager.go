package vars

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ==================== 日志管理器 ====================

// OptimizedLoggerManager 优化的日志管理器
type OptimizedLoggerManager struct {
	config       LogConfig
	logger       *slog.Logger
	zapLogger    *zap.Logger
	writer       io.WriteCloser
	handler      *OptimizedZapSlogHandler
	isEnabled    atomic.Bool
	mu           sync.RWMutex
	asyncWriter  *AsyncWriteSyncer
}

// NewOptimizedLoggerManager 创建优化的日志管理器
func NewOptimizedLoggerManager(cfg LogConfig) (*OptimizedLoggerManager, error) {
	// 验证配置
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid log config: %w", err)
	}

	manager := &OptimizedLoggerManager{
		config: cfg,
	}

	if err := manager.init(); err != nil {
		return nil, err
	}

	return manager, nil
}

// init 初始化日志管理器
func (lm *OptimizedLoggerManager) init() error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// 检查日志级别
	if strings.EqualFold(lm.config.LogLevel, LogLevelOff) {
		lm.isEnabled.Store(false)
		return nil
	}

	// 创建优化的Zap核心
	core, writer, err := createOptimizedZapCore(lm.config)
	if err != nil {
		return err
	}

	lm.writer = writer

	// 创建Zap logger
	lm.zapLogger = zap.New(core,
		zap.AddCaller(),
		zap.AddCallerSkip(lm.config.CallerSkip),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)

	// 添加全局字段
	if len(lm.config.Fields) > 0 {
		fields := make([]zap.Field, 0, len(lm.config.Fields))
		for k, v := range lm.config.Fields {
			fields = append(fields, zap.String(k, v))
		}
		lm.zapLogger = lm.zapLogger.With(fields...)
	}

	// 转换日志级别
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(lm.config.LogLevel)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLogLevel, err)
	}

	// 创建优化的Slog处理器
	lm.handler = NewOptimizedZapSlogHandler(lm.zapLogger, slogLevel, lm.config.CallerSkip)

	// 添加全局属性
	if len(lm.config.Fields) > 0 {
		globalAttrs := make([]slog.Attr, 0, len(lm.config.Fields))
		for k, v := range lm.config.Fields {
			globalAttrs = append(globalAttrs, slog.String(k, v))
		}
		lm.handler.WithGlobalAttrs(globalAttrs...)
	}

	lm.logger = slog.New(lm.handler)
	lm.isEnabled.Store(true)

	return nil
}

// Close 关闭日志管理器
func (lm *OptimizedLoggerManager) Close() error {
	if !lm.isEnabled.CompareAndSwap(true, false) {
		return nil
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	// 刷新Zap logger
	if lm.zapLogger != nil {
		_ = lm.zapLogger.Sync()
	}

	// 关闭写入器
	if lm.writer != nil {
		if err := lm.writer.Close(); err != nil {
			return fmt.Errorf("failed to close writer: %w", err)
		}
		lm.writer = nil
	}

	lm.logger = nil
	lm.zapLogger = nil
	lm.handler = nil

	return nil
}

// GetLogger 获取当前日志器
func (lm *OptimizedLoggerManager) GetLogger() *slog.Logger {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	if !lm.isEnabled.Load() || lm.logger == nil {
		return slog.Default()
	}

	return lm.logger
}

// GetZapLogger 获取Zap logger（用于需要高性能的场景）
func (lm *OptimizedLoggerManager) GetZapLogger() *zap.Logger {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	if !lm.isEnabled.Load() || lm.zapLogger == nil {
		return zap.NewNop()
	}

	return lm.zapLogger
}

// SetLevel 动态设置日志级别
func (lm *OptimizedLoggerManager) SetLevel(level string) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if strings.EqualFold(level, LogLevelOff) {
		lm.isEnabled.Store(false)
		return nil
	}

	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLogLevel, err)
	}

	// 更新配置和级别
	lm.config.LogLevel = level
	lm.handler.level.Set(slogLevel)

	// 如果之前被禁用，需要重新初始化
	if !lm.isEnabled.Load() {
		lm.isEnabled.Store(true)
		// 可以在这里重新初始化，但简单起见只更新级别
	}

	return nil
}

// SetEnabled 设置日志启用状态
func (lm *OptimizedLoggerManager) SetEnabled(enabled bool) {
	lm.isEnabled.Store(enabled)
}

// IsEnabled 检查日志是否启用
func (lm *OptimizedLoggerManager) IsEnabled() bool {
	return lm.isEnabled.Load()
}

// GetStats 获取日志统计信息
func (lm *OptimizedLoggerManager) GetStats() LogStats {
	stats := LogStats{
		Enabled: lm.IsEnabled(),
	}

	lm.mu.RLock()
	if lm.zapLogger != nil {
		// Zap本身不暴露统计信息，这里可以添加自定义统计
	}
	lm.mu.RUnlock()

	return stats
}

// LogStats 日志统计信息
type LogStats struct {
	Enabled bool
	// 可以添加更多统计信息，如：
	// TotalLogs     int64
	// DroppedLogs   int64
	// CurrentSize   int64
	DroppedLogs int64
}

// ==================== 结构化日志方法 ====================

// LogWithFields 记录带字段的日志
func (lm *OptimizedLoggerManager) LogWithFields(level slog.Level, msg string, fields ...slog.Attr) {
	if !lm.IsEnabled() {
		return
	}

	logger := lm.GetLogger()

	// 转换slog.Attr为key-value pairs
	args := make([]any, 0, len(fields)*2)
	for _, attr := range fields {
		args = append(args, attr.Key, attr.Value.Any())
	}

	switch level {
	case slog.LevelDebug:
		logger.Debug(msg, args...)
	case slog.LevelInfo:
		logger.Info(msg, args...)
	case slog.LevelWarn:
		logger.Warn(msg, args...)
	case slog.LevelError:
		logger.Error(msg, args...)
	default:
		logger.Info(msg, args...)
	}
}

// LogWithContext 记录带上下文的日志
func (lm *OptimizedLoggerManager) LogWithContext(ctx context.Context, level slog.Level, msg string, args ...any) {
	if !lm.IsEnabled() {
		return
	}

	logger := lm.GetLogger()

	switch level {
	case slog.LevelDebug:
		logger.Log(ctx, slog.LevelDebug, msg, args...)
	case slog.LevelInfo:
		logger.Log(ctx, slog.LevelInfo, msg, args...)
	case slog.LevelWarn:
		logger.Log(ctx, slog.LevelWarn, msg, args...)
	case slog.LevelError:
		logger.Log(ctx, slog.LevelError, msg, args...)
	default:
		logger.Log(ctx, slog.LevelInfo, msg, args...)
	}
}

// ==================== 便捷方法 ====================

// Debug 调试级别日志
func (lm *OptimizedLoggerManager) Debug(msg string, args ...any) {
	if !lm.IsEnabled() {
		return
	}
	lm.GetLogger().Debug(msg, args...)
}

// Info 信息级别日志
func (lm *OptimizedLoggerManager) Info(msg string, args ...any) {
	if !lm.IsEnabled() {
		return
	}
	lm.GetLogger().Info(msg, args...)
}

// Warn 警告级别日志
func (lm *OptimizedLoggerManager) Warn(msg string, args ...any) {
	if !lm.IsEnabled() {
		return
	}
	lm.GetLogger().Warn(msg, args...)
}

// Error 错误级别日志
func (lm *OptimizedLoggerManager) Error(msg string, args ...any) {
	if !lm.IsEnabled() {
		return
	}
	lm.GetLogger().Error(msg, args...)
}

// ==================== 全局单例 ====================

var (
	globalOptimizedLogger *OptimizedLoggerManager
	optimizedOnce         sync.Once
)

// InitializeOptimized 使用优化配置初始化全局日志器
func InitializeOptimized(cfg LogConfig) error {
	var initErr error
	optimizedOnce.Do(func() {
		globalOptimizedLogger, initErr = NewOptimizedLoggerManager(cfg)
		if initErr == nil && globalOptimizedLogger.IsEnabled() {
			slog.SetDefault(globalOptimizedLogger.GetLogger())
		}
	})

	return initErr
}

// InitializeOptimizedWithDefaults 使用默认配置初始化
func InitializeOptimizedWithDefaults() error {
	return InitializeOptimized(DefaultConfig())
}

// ShutdownOptimized 关闭全局优化日志器
func ShutdownOptimized() error {
	if globalOptimizedLogger == nil {
		return nil
	}

	return globalOptimizedLogger.Close()
}

// GetOptimizedLogger 获取全局优化日志器
func GetOptimizedLogger() *OptimizedLoggerManager {
	if globalOptimizedLogger == nil {
		// 自动初始化
		_ = InitializeOptimizedWithDefaults()
	}
	return globalOptimizedLogger
}

// ==================== 全局便捷函数 ====================

// DebugOpt 优化的Debug日志
func DebugOpt(msg string, args ...any) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.GetLogger().Debug(msg, args...)
	}
}

// InfoOpt 优化的Info日志
func InfoOpt(msg string, args ...any) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.GetLogger().Info(msg, args...)
	}
}

// WarnOpt 优化的Warn日志
func WarnOpt(msg string, args ...any) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.GetLogger().Warn(msg, args...)
	}
}

// ErrorOpt 优化的Error日志
func ErrorOpt(msg string, args ...any) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.GetLogger().Error(msg, args...)
	}
}

// DebugWithFields 带字段的Debug日志
func DebugWithFields(msg string, fields ...slog.Attr) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.LogWithFields(slog.LevelDebug, msg, fields...)
	}
}

// InfoWithFields 带字段的Info日志
func InfoWithFields(msg string, fields ...slog.Attr) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.LogWithFields(slog.LevelInfo, msg, fields...)
	}
}

// WarnWithFields 带字段的Warn日志
func WarnWithFields(msg string, fields ...slog.Attr) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.LogWithFields(slog.LevelWarn, msg, fields...)
	}
}

// ErrorWithFields 带字段的Error日志
func ErrorWithFields(msg string, fields ...slog.Attr) {
	logger := GetOptimizedLogger()
	if logger.IsEnabled() {
		logger.LogWithFields(slog.LevelError, msg, fields...)
	}
}
