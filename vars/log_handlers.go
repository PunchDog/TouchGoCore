package vars

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// createOptimizedZapCore 创建优化的Zap核心配置。
// 返回值里的 asyncWriter 仅在异步模式下非空，调用方必须持有它并在 zap 之后关闭，
// 否则缓冲区里未落盘的日志会随进程退出丢失。
func createOptimizedZapCore(cfg LogConfig) (zapcore.Core, io.WriteCloser, *AsyncWriteSyncer, error) {
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
		return nil, nil, nil, err
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
	var asyncWriter *AsyncWriteSyncer
	if cfg.Async {
		asyncWriter = NewAsyncWriteSyncer(rotatingWriter, cfg.AsyncBufferSize)
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
	zapLevel, err := zapLevelFor(cfg.LogLevel)
	if err != nil {
		if asyncWriter != nil {
			_ = asyncWriter.Close()
		}
		_ = rotatingWriter.Close()
		return nil, nil, nil, err
	}

	// 创建核心
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		writeSyncer,
		zap.NewAtomicLevelAt(zapLevel),
	)

	return core, rotatingWriter, asyncWriter, nil
}

// zapLevelFor 把配置里的日志级别字符串转成 zap 级别。
// 未知级别不再默默退化成 Debug（会在生产环境放大成日志洪泛），直接返回错误。
func zapLevelFor(level string) (zapcore.Level, error) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return zapcore.DebugLevel, nil
	case "", "INFO":
		return zapcore.InfoLevel, nil
	case "WARN", "WARNING":
		return zapcore.WarnLevel, nil
	case "ERROR":
		return zapcore.ErrorLevel, nil
	case "OFF": // strings.ToUpper 后与 LogLevelOff("off") 等价
		// 高于所有真实级别；曾取 Fatal 会因为走到 zap 的 os.Exit 路径
		return zapcore.InvalidLevel, nil
	default:
		return zapcore.InfoLevel, fmt.Errorf("%w: %s", ErrInvalidLogLevel, level)
	}
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

	// globalAttrs 用原子指针承载：Handle 由任意协程读取，WithGlobalAttrs  copy-on-write 替换，
	// 直接 append 共享切片会与读取构成数据竞争。
	globalAttrs atomic.Pointer[[]slog.Attr]
}

// emptyAttrs 供未设置全局属性时复用的空切片
var emptyAttrs = []slog.Attr{}

// NewOptimizedZapSlogHandler 创建优化的日志处理器
func NewOptimizedZapSlogHandler(zapLogger *zap.Logger, level slog.Level, callerSkip int) *OptimizedZapSlogHandler {
	lv := &slog.LevelVar{}
	lv.Set(level)

	h := &OptimizedZapSlogHandler{
		zapLogger:   zapLogger.WithOptions(zap.AddCallerSkip(callerSkip)),
		level:       lv,
		addSource:   true,
		pid:         os.Getpid(),
		callerSkip:  callerSkip,
		fieldsPool: sync.Pool{
			New: func() interface{} {
				return make([]zap.Field, 0, 12)
			},
		},
	}
	h.globalAttrs.Store(&emptyAttrs)
	return h
}

// globalAttrList 返回当前全局属性快照（只读，不得就地修改）
func (h *OptimizedZapSlogHandler) globalAttrList() []slog.Attr {
	if p := h.globalAttrs.Load(); p != nil {
		return *p
	}
	return emptyAttrs
}

// WithGlobalAttrs 追加全局属性：copy-on-write 生成新切片后原子替换，
// 已在途的 Handle 继续使用旧快照，不会读到半更新的数据。
func (h *OptimizedZapSlogHandler) WithGlobalAttrs(attrs ...slog.Attr) {
	if len(attrs) == 0 {
		return
	}
	for {
		old := h.globalAttrs.Load()
		next := make([]slog.Attr, 0, len(*old)+len(attrs))
		next = append(next, *old...)
		next = append(next, attrs...)
		if h.globalAttrs.CompareAndSwap(old, &next) {
			return
		}
	}
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

	globalAttrs := h.globalAttrList()

	// 预分配足够容量
	attrCount := r.NumAttrs() + len(globalAttrs) + 2 // +2 for time and pid
	if cap(fields) < attrCount {
		fields = make([]zap.Field, 0, attrCount)
	}

	// 添加时间戳
	fields = append(fields, zap.Time("time", r.Time))
	fields = append(fields, zap.Int("pid", h.pid))

	// 添加全局属性
	for _, attr := range globalAttrs {
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

// newChildHandler 基于当前处理器派生子处理器，复制全局属性快照并重建对象池
func (h *OptimizedZapSlogHandler) newChildHandler() *OptimizedZapSlogHandler {
	attrs := append([]slog.Attr{}, h.globalAttrList()...)
	child := &OptimizedZapSlogHandler{
		zapLogger:   h.zapLogger,
		level:       h.level,
		addSource:   h.addSource,
		groupPrefix: h.groupPrefix,
		pid:         h.pid,
		callerSkip:  h.callerSkip,
	}
	// 重新创建对象池以避免复制lock
	child.fieldsPool = sync.Pool{
		New: func() interface{} {
			return make([]zap.Field, 0, 12)
		},
	}
	child.globalAttrs.Store(&attrs)
	return child
}

// WithAttrs 创建带属性的子处理器
func (h *OptimizedZapSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	child := h.newChildHandler()
	child.zapLogger = h.zapLogger.With(h.slogAttrsToZapFields(attrs)...)
	return child
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

	child := h.newChildHandler()
	child.groupPrefix = newPrefix
	return child
}

// slogAttrsToZapFields 转换属性字段
func (h *OptimizedZapSlogHandler) slogAttrsToZapFields(attrs []slog.Attr) []zap.Field {
	fields := make([]zap.Field, len(attrs))
	for i, attr := range attrs {
		fields[i] = zap.Any(attr.Key, attr.Value.Any())
	}
	return fields
}
