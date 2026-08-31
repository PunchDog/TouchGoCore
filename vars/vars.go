package vars

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// 错误定义
var (
	ErrLoggerNotInitialized  = errors.New("logger not initialized")
	ErrInvalidLogLevel       = errors.New("invalid log level")
	ErrFileCreateFailed      = errors.New("failed to create log file")
	ErrDirectoryCreateFailed = errors.New("failed to create log directory")
)

// 日志级别枚举
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
	LogLevelOff   = "off"
)

// LogConfig 日志配置结构
// 使用结构体封装配置，提高可维护性
type LogConfig struct {
	LogPath        string            // 日志文件路径
	LogName        string            // 日志文件名（不含扩展名）
	LogLevel       string            // 日志级别
	MaxSize        int64             // 日志文件最大大小（MB）
	MaxAge         int               // 日志文件最大保留天数
	MaxBackups     int               // 最大备份数量（新增）
	Compress       bool              // 是否压缩旧日志
	Stdout         bool              // 是否输出到标准输出
	Async          bool              // 是否异步日志（新增）
	AsyncBufferSize int               // 异步缓冲区大小（新增）
	CallerSkip     int               // 调用栈跳过层数（新增）
	Fields         map[string]string // 全局字段（新增）
}

// Validate 验证配置有效性
func (cfg *LogConfig) Validate() error {
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 100 // 设置默认值
	}
	if cfg.MaxAge < 0 {
		cfg.MaxAge = 30 // 设置默认值
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = 10 // 设置默认值
	}
	if cfg.AsyncBufferSize <= 0 {
		cfg.AsyncBufferSize = 10000 // 设置默认值
	}
	if cfg.CallerSkip < 0 {
		cfg.CallerSkip = 1 // 设置默认值
	}

	// 验证日志级别
	validLevels := map[string]bool{
		LogLevelDebug: true,
		LogLevelInfo:  true,
		LogLevelWarn:  true,
		LogLevelError: true,
		LogLevelOff:   true,
	}

	level := strings.ToLower(cfg.LogLevel)
	if !validLevels[level] {
		return fmt.Errorf("invalid log level: %s", cfg.LogLevel)
	}
	cfg.LogLevel = level

	return nil
}

// DefaultConfig 默认配置
func DefaultConfig() LogConfig {
	return LogConfig{
		LogPath:         "./logs",
		LogName:         "default",
		LogLevel:        LogLevelDebug,
		MaxSize:         100,                 // 100MB
		MaxAge:          30,                  // 30天
		MaxBackups:      10,                  // 最大备份数
		Compress:        true,
		Stdout:          true,
		Async:           true,                // 异步日志
		AsyncBufferSize: 10000,               // 异步缓冲区
		CallerSkip:      1,                   // 调用栈跳过
		Fields:          make(map[string]string),
	}
}

// LoggerManager 日志管理器
type LoggerManager struct {
	config    LogConfig
	logger    *slog.Logger
	zapLogger *zap.Logger
	file      *os.File
	isEnabled bool
	mu        sync.RWMutex
}

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

// 工作目录缓存
var (
	cachedWorkdir     string
	workdirOnce       sync.Once
	workdirInitError  error
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

// 全局变量（使用单例模式）
var (
	globalLogger *LoggerManager
	once         sync.Once
)

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

// 全局函数（向后兼容）
func Run(path, name, level string) {
	cfg := DefaultConfig()
	cfg.LogLevel = level
	cfg.LogPath = path
	cfg.LogName = name
	Initialize(cfg)
}

// Initialize 初始化全局日志器（默认使用异步Channel模式）
func Initialize(cfg LogConfig) error {
	var initErr error
	once.Do(func() {
		// 默认启用异步模式以保证引擎效率
		if cfg.AsyncBufferSize <= 0 {
			cfg.AsyncBufferSize = 10000
		}

		// 创建基于Channel的异步日志管理器
		channelManager, err := NewChannelLoggerManager(cfg)
		if err != nil {
			initErr = err
			return
		}

		// 如果启用异步，使用Channel模式
		if cfg.Async {
			initErr = channelManager.initWithChannel()
			if initErr != nil {
				return
			}
		}

		// 存储到全局变量（保持兼容性）
		globalLogger, initErr = NewLoggerManager(cfg)
		if initErr != nil {
			// 如果旧模式失败，使用Channel管理器
			initErr = nil
		}

		// 将Channel管理器也设为全局
		InitializeChannelLogger(cfg)

		if GetChannelLogger() != nil && GetChannelLogger().IsEnabled() {
			slog.SetDefault(GetChannelLogger().GetLogger())
		}
	})

	return initErr
}

// InitializeWithDefaults 使用默认配置初始化
func InitializeWithDefaults() error {
	return Initialize(DefaultConfig())
}

// Shutdown 关闭全局日志器
func Shutdown() error {
	// 先关闭异步Channel日志（确保所有日志刷出）
	if err := ShutdownChannelLogger(); err != nil {
		return err
	}

	if globalLogger == nil {
		return nil
	}

	return globalLogger.Close()
}

// formatLogMsg 统一格式化日志消息（处理带占位符的格式化参数）
func formatLogMsg(msg string, args ...any) string {
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// consoleLevelPrefix 返回命令行输出使用的级别前缀
func consoleLevelPrefix(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "[DEBUG]"
	case slog.LevelInfo:
		return "[INFO]"
	case slog.LevelWarn:
		return "[WARN]"
	case slog.LevelError:
		return "[ERROR]"
	default:
		return "[INFO]"
	}
}

// printToConsole 优先将日志同步输出到命令行（立即可见，便于实时观察）。
// 不受日志级别限制；级别过滤仅作用于后续的 log 文件写入。
func printToConsole(level slog.Level, msg string) {
	ts := time.Now().Format("15:04:05")
	fmt.Printf("%s %s %s\n", ts, consoleLevelPrefix(level), msg)
}

// writeToFile 按配置日志级别将日志异步写入 log 文件；
// 级别不足、日志器未启用或级别为 off 时跳过（不写文件，但命令行已先行输出）。
func writeToFile(level slog.Level, msg string) {
	ch := GetChannelLogger()
	if ch == nil || !ch.IsEnabled() {
		// 回退到旧版日志器（其内部已按等级过滤）
		logWithLevel(level, "%s", msg)
		return
	}
	if ch.IsOff() || !ch.ShouldWriteFile(level) {
		return
	}
	ch.LogAsyncSimple(level, msg)
}

// Debug 调试级别日志：优先输出到命令行，其次按等级写入 log 文件
func Debug(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := formatLogMsg(msg, args...)
	printToConsole(slog.LevelDebug, formatted)
	writeToFile(slog.LevelDebug, formatted)
}

// Info 信息级别日志：优先输出到命令行，其次按等级写入 log 文件
func Info(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := formatLogMsg(msg, args...)
	printToConsole(slog.LevelInfo, formatted)
	writeToFile(slog.LevelInfo, formatted)
}

// Warning 警告级别日志：优先输出到命令行，其次按等级写入 log 文件
func Warning(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := formatLogMsg(msg, args...)
	printToConsole(slog.LevelWarn, formatted)
	writeToFile(slog.LevelWarn, formatted)
}

// Error 错误级别日志：优先输出到命令行，其次按等级写入 log 文件
func Error(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := formatLogMsg(msg, args...)
	printToConsole(slog.LevelError, formatted)
	writeToFile(slog.LevelError, formatted)
}

// logWithLevel 统一的日志记录函数
func logWithLevel(level slog.Level, msg string, args ...any) {
	var formattedMsg string
	if len(args) > 0 {
		// 如果有参数，使用fmt.Sprintf格式化消息
		formattedMsg = fmt.Sprintf(msg, args...)
	} else {
		// 没有参数，直接使用原始消息
		formattedMsg = msg
	}

	if globalLogger == nil || !globalLogger.IsEnabled() {
		// 使用简单的fmt输出作为fallback
		logSimple(level, formattedMsg, nil...)
		return
	}

	logger := globalLogger.GetLogger()

	switch level {
	case slog.LevelDebug:
		logger.Debug(formattedMsg)
	case slog.LevelInfo:
		logger.Info(formattedMsg)
	case slog.LevelWarn:
		logger.Warn(formattedMsg)
	case slog.LevelError:
		logger.Error(formattedMsg)
	}
}

// logSimple 简单的日志输出（用于未初始化时）
func logSimple(level slog.Level, msg string, args ...any) {
	levelStr := "[INFO]"
	switch level {
	case slog.LevelDebug:
		levelStr = "[DEBUG]"
	case slog.LevelWarn:
		levelStr = "[WARN]"
	case slog.LevelError:
		levelStr = "[ERROR]"
	}

	if len(args) > 0 {
		fmt.Printf("%s %s\n", levelStr, fmt.Sprintf(msg, args...))
	} else {
		fmt.Printf("%s %s\n", levelStr, msg)
	}
}

// 初始化函数（默认使用默认配置）
func init() {
	// 默认初始化，但允许后续重新配置
	// _ = InitializeWithDefaults()
}

// ==================== 优化的调用位置编码器 ====================

// OptimizeCallerEncoder 优化的调用位置编码器
func OptimizeCallerEncoder(caller zapcore.EntryCaller, enc zapcore.PrimitiveArrayEncoder) {
	if !caller.Defined {
		enc.AppendString("undefined:0")
		return
	}

	// 使用TrimmedPath获取短路径，避免完整路径
	enc.AppendString(fmt.Sprintf("%s:%d", caller.TrimmedPath(), caller.Line))
}

// ==================== 创建优化的Zap核心 ====================

// createOptimizedZapCore 创建优化的Zap核心配置
func createOptimizedZapCore(cfg LogConfig) (zapcore.Core, io.WriteCloser, error) {
	// 创建旋转日志写入器
	rotatingWriter, err := NewRotatingFileWriter(
		cfg.LogPath,
		cfg.LogName,
		cfg.MaxSize*1024*1024, // 转换为字节
		cfg.MaxAge,
		cfg.MaxBackups,
		cfg.Compress,
	)
	if err != nil {
		return nil, nil, err
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
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeTime:     zapcore.TimeEncoderOfLayout(time.RFC3339Nano),
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeCaller:   OptimizeCallerEncoder,
		EncodeDuration: zapcore.MillisDurationEncoder,
	}

	// 创建写入器链
	var writers []zapcore.WriteSyncer

	// 添加异步或同步文件写入器
	if cfg.Async {
		asyncWriter := NewAsyncWriteSyncer(rotatingWriter, cfg.AsyncBufferSize)
		writers = append(writers, zapcore.AddSync(asyncWriter))
	} else {
		writers = append(writers, zapcore.AddSync(rotatingWriter))
	}

	// 添加标准输出
	if cfg.Stdout {
		writers = append(writers, zapcore.AddSync(os.Stdout))
	}

	// 组合写入器
	var writeSyncer zapcore.WriteSyncer
	if len(writers) == 1 {
		writeSyncer = writers[0]
	} else {
		writeSyncer = zapcore.NewMultiWriteSyncer(writers...)
	}

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
	case "OFF":
		zapLevel = zap.FatalLevel // 禁用所有日志
	}

	// 创建核心
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		writeSyncer,
		zap.NewAtomicLevelAt(zapLevel),
	)

	return core, rotatingWriter, nil
}

// ==================== 优化的Slog处理器 ====================

// OptimizedZapSlogHandler 优化的日志处理器
type OptimizedZapSlogHandler struct {
	zapLogger   *zap.Logger
	level       *slog.LevelVar
	addSource   bool
	groupPrefix string
	pid         int
	fieldsPool  sync.Pool
	callerSkip  int
	globalAttrs []slog.Attr
}

// NewOptimizedZapSlogHandler 创建优化的日志处理器
func NewOptimizedZapSlogHandler(zapLogger *zap.Logger, level slog.Level, callerSkip int) *OptimizedZapSlogHandler {
	lv := &slog.LevelVar{}
	lv.Set(level)

	return &OptimizedZapSlogHandler{
		zapLogger:  zapLogger.WithOptions(zap.AddCallerSkip(callerSkip)),
		level:      lv,
		addSource:  true,
		pid:        os.Getpid(),
		callerSkip: callerSkip,
		globalAttrs: make([]slog.Attr, 0),
		fieldsPool: sync.Pool{
			New: func() interface{} {
				return make([]zap.Field, 0, 12)
			},
		},
	}
}

// WithGlobalAttrs 添加全局属性
func (h *OptimizedZapSlogHandler) WithGlobalAttrs(attrs ...slog.Attr) {
	h.globalAttrs = append(h.globalAttrs, attrs...)
}

// Enabled 检查日志级别是否启用
func (h *OptimizedZapSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle 处理日志记录
func (h *OptimizedZapSlogHandler) Handle(_ context.Context, r slog.Record) error {
	// 从对象池获取字段切片
	fields := h.fieldsPool.Get().([]zap.Field)
	defer func() {
		fields = fields[:0]
		h.fieldsPool.Put(fields)
	}()

	// 预分配足够容量
	attrCount := r.NumAttrs() + len(h.globalAttrs) + 2 // +2 for time and pid
	if cap(fields) < attrCount {
		fields = make([]zap.Field, 0, attrCount)
	}

	// 添加时间戳
	fields = append(fields, zap.Time("time", r.Time))
	fields = append(fields, zap.Int("pid", h.pid))

	// 添加全局属性
	for _, attr := range h.globalAttrs {
		fields = append(fields, zap.Any(attr.Key, attr.Value.Any()))
	}

	// 添加记录属性
	r.Attrs(func(attr slog.Attr) bool {
		// 处理分组前缀
		if h.groupPrefix != "" {
			fields = append(fields, zap.Any(h.groupPrefix+attr.Key, attr.Value.Any()))
		} else {
			fields = append(fields, zap.Any(attr.Key, attr.Value.Any()))
		}
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
	default:
		h.zapLogger.Info(r.Message, fields...)
	}

	return nil
}

// WithAttrs 创建带属性的子处理器
func (h *OptimizedZapSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	newZapLogger := h.zapLogger.With(h.slogAttrsToZapFields(attrs)...)
	newHandler := &OptimizedZapSlogHandler{
		zapLogger:   newZapLogger,
		level:       h.level,
		addSource:   h.addSource,
		groupPrefix: h.groupPrefix,
		pid:         h.pid,
		globalAttrs: append([]slog.Attr{}, h.globalAttrs...),
	}

	// 重新创建对象池以避免复制lock
	newHandler.fieldsPool = sync.Pool{
		New: func() interface{} {
			return make([]zap.Field, 0, 12)
		},
	}

	return newHandler
}

// WithGroup 创建分组处理器
func (h *OptimizedZapSlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	newPrefix := name + "."
	if h.groupPrefix != "" {
		newPrefix = h.groupPrefix + newPrefix
	}

	newHandler := &OptimizedZapSlogHandler{
		zapLogger:   h.zapLogger,
		level:       h.level,
		groupPrefix: newPrefix,
		addSource:   h.addSource,
		pid:         h.pid,
		globalAttrs: append([]slog.Attr{}, h.globalAttrs...),
	}

	// 重新创建对象池以避免复制lock
	newHandler.fieldsPool = sync.Pool{
		New: func() interface{} {
			return make([]zap.Field, 0, 12)
		},
	}

	return newHandler
}

// slogAttrsToZapFields 转换属性字段
func (h *OptimizedZapSlogHandler) slogAttrsToZapFields(attrs []slog.Attr) []zap.Field {
	fields := make([]zap.Field, len(attrs))
	for i, attr := range attrs {
		fields[i] = zap.Any(attr.Key, attr.Value.Any())
	}
	return fields
}
