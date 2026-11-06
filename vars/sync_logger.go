package vars

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ==================== 同步日志管理器（独立可用的管理器） ====================

// LoggerManager 日志管理器。
//
// 它不再是全局日志器的 fallback（S70）：全局那一路由 ChannelLoggerManager 独占文件句柄，
// 这里另开一个句柄写同一个 .log 正是「轮转后日志写进备份文件」的成因。
// 类型本身保留给需要独立实例（临时目录、单独一份文件）的调用方。
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
	// 配置非法直接失败：后续写入都依赖这份配置。
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

	// off 不建任何链路：直接标记未启用并返回，避免打开文件句柄。
	if strings.EqualFold(lm.config.LogLevel, LogLevelOff) {
		lm.isEnabled = false
		return nil
	}

	// 建核心并打开文件句柄；失败直接上抛，调用方负责关闭。
	core, file, err := createZapCore(lm.config)
	if err != nil {
		return err
	}

	lm.file = file
	lm.zapLogger = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))

	// 把配置字符串解成 slog.Level；非法值上抛而非静默默认。
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(lm.config.LogLevel)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLogLevel, err)
	}

	// 用桥接 handler 把 slog 记录转给 zap 落盘。
	handler := NewZapSlogHandler(lm.zapLogger, slogLevel)
	lm.logger = slog.New(handler)
	lm.isEnabled = true

	return nil
}

// Close 关闭日志管理器
func (lm *LoggerManager) Close() error {
	// 整段持锁：Sync/Close 文件句柄与置空字段必须原子，避免与 SetLevel 并发。
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
		// 未启用或 logger 为空时退回 slog.Default()：保证调用方总有可用 logger。
		return slog.Default()
	}

	return lm.logger
}

// SetLevel 动态设置日志级别
func (lm *LoggerManager) SetLevel(level string) error {
	// off 是纯降级：关掉落盘、标记未启用，不必重建链路。
	if strings.EqualFold(level, LogLevelOff) {
		lm.mu.Lock()
		lm.config.LogLevel = level
		lm.isEnabled = false
		lm.mu.Unlock()
		return nil
	}

	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLogLevel, err)
	}

	// 锁内摘走旧资源引用（init 会再次取锁，持锁调用将自死锁）
	lm.mu.Lock()
	lm.config.LogLevel = level
	oldFile, oldZap := lm.file, lm.zapLogger
	lm.file, lm.zapLogger, lm.logger, lm.isEnabled = nil, nil, nil, false
	lm.mu.Unlock()

	// 锁外关闭旧文件句柄
	if oldZap != nil {
		_ = oldZap.Sync()
	}
	if oldFile != nil {
		_ = oldFile.Close()
	}

	// 锁外关旧句柄后重建整条链路。
	return lm.init()
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

// 字段池：每条 Handle 借一片 zap.Field 切片，用完归零归还，减少分配。
var fieldPool = sync.Pool{
	New: func() interface{} {
		return make([]zap.Field, 0, 8) // 预分配合理容量
	},
}

// Handle 处理日志记录
func (h *ZapSlogHandler) Handle(_ context.Context, r slog.Record) error {
	// 从对象池借字段切片，用完归零归还，避免每条日志分配。
	fields := fieldPool.Get().([]zap.Field)
	defer func() {
		// 重置并放回对象池
		fields = fields[:0]
		fieldPool.Put(fields)
	}()

	// 预分配 pid + 记录属性所需容量，减少 append 扩容。
	if cap(fields) < r.NumAttrs()+1 {
		fields = make([]zap.Field, 0, r.NumAttrs()+1)
	}

	fields = append(fields, zap.Int("PID", h.pid)) // 用启动期缓存的 pid，避免每条日志查 os.Getpid()

	r.Attrs(func(attr slog.Attr) bool {
		fields = append(fields, zap.Any(attr.Key, attr.Value.Any()))
		return true
	})

	// 按级别分派到对应 zap 方法。
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
	// sync.Once 保证工作目录只解析一次，进程内共用同一结果。
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
	// 调用点未定义（如测试/合成记录）用占位串，避免空字段引发解析异常。
	if !caller.Defined {
		enc.AppendString("unknown:0")
		return
	}

	enc.AppendString(fmt.Sprintf("%s:%d", caller.TrimmedPath(), caller.Line))
}

// createZapCore 创建Zap核心配置
func createZapCore(cfg LogConfig) (zapcore.Core, *os.File, error) {
	// 输出目录不存在先建，否则下面 OpenFile 会失败。
	if err := os.MkdirAll(cfg.LogPath, 0755); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrDirectoryCreateFailed, err)
	}

	// 拼接最终文件路径：目录 + 文件名 + .log。
	filePath := filepath.Join(cfg.LogPath, cfg.LogName+".log")

	// 以追加模式打开，保留历史内容而非覆盖。
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrFileCreateFailed, err)
	}

	// 组装 zap JSON 编码器，键名与全局链路对齐。
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

	// 按配置串起标准输出与文件写入器。
	var writers []zapcore.WriteSyncer
	if cfg.Stdout {
		writers = append(writers, zapcore.AddSync(os.Stdout))
	}
	writers = append(writers, zapcore.AddSync(file))

	multiWriter := zapcore.NewMultiWriteSyncer(writers...)

	// 设置日志级别（未知级别直接报错，不再默默退化成 Debug）
	zapLevel, err := zapLevelFor(cfg.LogLevel)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		multiWriter,
		zap.NewAtomicLevelAt(zapLevel),
	)

	return core, file, nil
}
