package vars

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ==================== 同步日志管理器（fallback 路径） ====================

// LoggerManager 日志管理器
type LoggerManager struct {
	config    LogConfig
	logger    *slog.Logger
	zapLogger *zap.Logger
	file      *os.File
	isEnabled bool
	mu        sync.RWMutex
}

// NewLoggerManager 创建新的日志管理器
func NewLoggerManager(cfg LogConfig) (*LoggerManager, error) {
	// 验证配置
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid log config: %w", err)
	}

	manager := &LoggerManager{
		config: cfg,
	}

	if err := manager.init(); err != nil {
		return nil, err
	}

	return manager, nil
}

// init 初始化日志管理器
func (lm *LoggerManager) init() error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// 检查日志级别
	if strings.EqualFold(lm.config.LogLevel, LogLevelOff) {
		lm.isEnabled = false
		return nil
	}

	// 创建Zap核心
	core, file, err := createZapCore(lm.config)
	if err != nil {
		return err
	}

	lm.file = file
	lm.zapLogger = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))

	// 转换日志级别
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(lm.config.LogLevel)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLogLevel, err)
	}

	// 创建Slog处理器
	handler := NewZapSlogHandler(lm.zapLogger, slogLevel)
	lm.logger = slog.New(handler)
	lm.isEnabled = true

	return nil
}

// Close 关闭日志管理器
func (lm *LoggerManager) Close() error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if lm.zapLogger != nil {
		_ = lm.zapLogger.Sync()
	}

	if lm.file != nil {
		if err := lm.file.Close(); err != nil {
			return err
		}
		lm.file = nil
	}

	lm.logger = nil
	lm.zapLogger = nil
	lm.isEnabled = false

	return nil
}

// GetLogger 获取当前日志器
func (lm *LoggerManager) GetLogger() *slog.Logger {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	if !lm.isEnabled || lm.logger == nil {
		// 返回一个简单的控制台日志器作为fallback
		return slog.Default()
	}

	return lm.logger
}

// SetLevel 动态设置日志级别
func (lm *LoggerManager) SetLevel(level string) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if strings.EqualFold(level, LogLevelOff) {
		lm.isEnabled = false
		return nil
	}

	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLogLevel, err)
	}

	// 重新初始化日志器
	lm.config.LogLevel = level
	if err := lm.init(); err != nil {
		return err
	}

	return nil
}

// IsEnabled 检查日志是否启用
func (lm *LoggerManager) IsEnabled() bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	return lm.isEnabled
}

// ==================== ZapSlogHandler（slog → zap 桥接） ====================

// ZapSlogHandler 优化后的日志处理器
type ZapSlogHandler struct {
	zapLogger   *zap.Logger
	level       *slog.LevelVar
	addSource   bool
	groupPrefix string
	pid         int // 进程ID缓存
}

// NewZapSlogHandler 创建日志处理器
func NewZapSlogHandler(zapLogger *zap.Logger, level slog.Level) *ZapSlogHandler {
	lv := &slog.LevelVar{}
	lv.Set(level)

	return &ZapSlogHandler{
		zapLogger: zapLogger.WithOptions(zap.AddCallerSkip(1)),
		level:     lv,
		addSource: true,
		pid:       os.Getpid(), // 缓存进程ID
	}
}

// Enabled 检查日志级别是否启用
func (h *ZapSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// 字段池，减少内存分配
var fieldPool = sync.Pool{
	New: func() interface{} {
		return make([]zap.Field, 0, 8) // 预分配合理容量
	},
}

// Handle 处理日志记录
func (h *ZapSlogHandler) Handle(_ context.Context, r slog.Record) error {
	// 从对象池获取字段切片
	fields := fieldPool.Get().([]zap.Field)
	defer func() {
		// 重置并放回对象池
		fields = fields[:0]
		fieldPool.Put(fields)
	}()

	// 预分配足够容量
	if cap(fields) < r.NumAttrs()+1 {
		fields = make([]zap.Field, 0, r.NumAttrs()+1)
	}

	fields = append(fields, zap.Int("PID", h.pid)) // 使用缓存

	r.Attrs(func(attr slog.Attr) bool {
		fields = append(fields, zap.Any(attr.Key, attr.Value.Any()))
		return true
	})

	// 使用更高效的switch语句
	switch r.Level {
	case slog.LevelDebug:
		h.zapLogger.Debug(r.Message, fields...)
	case slog.LevelInfo:
		h.zapLogger.Info(r.Message, fields...)
	case slog.LevelWarn:
		h.zapLogger.Warn(r.Message, fields...)
	case slog.LevelError:
		h.zapLogger.Error(r.Message, fields...)
	}

	return nil
}

// WithAttrs 创建带属性的子处理器
func (h *ZapSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h // 空属性直接返回
	}

	newZapLogger := h.zapLogger.With(h.slogAttrsToZapFields(attrs)...)
	return &ZapSlogHandler{
		zapLogger:   newZapLogger,
		level:       h.level,
		addSource:   h.addSource,
		groupPrefix: h.groupPrefix,
		pid:         h.pid,
	}
}

// WithGroup 创建分组处理器
func (h *ZapSlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	newPrefix := name + "."
	if h.groupPrefix != "" {
		newPrefix = h.groupPrefix + newPrefix
	}

	return &ZapSlogHandler{
		zapLogger:   h.zapLogger,
		level:       h.level,
		groupPrefix: newPrefix,
		addSource:   h.addSource,
		pid:         h.pid,
	}
}

// slogAttrsToZapFields 转换属性字段
func (h *ZapSlogHandler) slogAttrsToZapFields(attrs []slog.Attr) []zap.Field {
	fields := make([]zap.Field, len(attrs))
	for i, attr := range attrs {
		fields[i] = zap.Any(attr.Key, attr.Value.Any())
	}
	return fields
}

// ==================== Zap Core 构建 ====================

// 工作目录缓存
var (
	cachedWorkdir    string
	workdirOnce      sync.Once
	workdirInitError error
)

// getWorkdir 获取缓存的工目录
func getWorkdir() (string, error) {
	workdirOnce.Do(func() {
		cachedWorkdir, workdirInitError = os.Getwd()
		if cachedWorkdir != "" {
			cachedWorkdir = strings.ReplaceAll(cachedWorkdir, "\\", "/")
		}
	})
	return cachedWorkdir, workdirInitError
}

// callerEncoder 优化后的调用位置编码器
func callerEncoder(caller zapcore.EntryCaller, enc zapcore.PrimitiveArrayEncoder) {
	if !caller.Defined {
		enc.AppendString("unknown:0")
		return
	}

	enc.AppendString(fmt.Sprintf("%s:%d", caller.TrimmedPath(), caller.Line))
}

// createZapCore 创建Zap核心配置
func createZapCore(cfg LogConfig) (zapcore.Core, *os.File, error) {
	// 创建日志目录
	if err := os.MkdirAll(cfg.LogPath, 0755); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrDirectoryCreateFailed, err)
	}

	// 配置文件路径
	filePath := path.Join(cfg.LogPath, cfg.LogName+".log")

	// 打开日志文件
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrFileCreateFailed, err)
	}

	// 配置编码器
	encoderCfg := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    "function",
		MessageKey:     "message",
		StacktraceKey:  "stacktrace",
		EncodeTime:     zapcore.TimeEncoderOfLayout(time.DateTime),
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeCaller:   callerEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	}

	// 创建输出器
	var writers []zapcore.WriteSyncer
	if cfg.Stdout {
		writers = append(writers, zapcore.AddSync(os.Stdout))
	}
	writers = append(writers, zapcore.AddSync(file))

	multiWriter := zapcore.NewMultiWriteSyncer(writers...)

	// 设置日志级别
	zapLevel := zap.DebugLevel
	switch strings.ToUpper(cfg.LogLevel) {
	case "DEBUG":
		zapLevel = zap.DebugLevel
	case "INFO":
		zapLevel = zap.InfoLevel
	case "WARN":
		zapLevel = zap.WarnLevel
	case "ERROR":
		zapLevel = zap.ErrorLevel
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		multiWriter,
		zap.NewAtomicLevelAt(zapLevel),
	)

	return core, file, nil
}
