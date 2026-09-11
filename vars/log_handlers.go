package vars

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

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
		zapLogger:   zapLogger.WithOptions(zap.AddCallerSkip(callerSkip)),
		level:       lv,
		addSource:   true,
		pid:         os.Getpid(),
		callerSkip:  callerSkip,
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
