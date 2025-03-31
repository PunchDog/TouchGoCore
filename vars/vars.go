package vars

import (
	"context"
	"log/slog"
	"os"
	"runtime"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ZapSlogHandler 实现 slog.Handler
type ZapSlogHandler struct {
	zapLogger   *zap.Logger
	level       *slog.LevelVar // 动态日志级别
	addSource   bool           // 是否记录调用位置
	groupPrefix string         // 存储当前分组路径（如 "parent.child."）
}

// Enabled 检查级别是否启用
func (h *ZapSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle 核心日志处理逻辑
func (h *ZapSlogHandler) Handle(_ context.Context, r slog.Record) error {
	// 转换字段为Zap格式（避免反射）
	fields := make([]zap.Field, 0, r.NumAttrs())
	r.Attrs(func(attr slog.Attr) bool {
		fields = append(fields, zap.Any(attr.Key, attr.Value.Any()))
		return true
	})

	// 添加调用位置信息
	if h.addSource && r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		if frame, _ := fs.Next(); frame.File != "" {
			fields = append(fields,
				zap.String("file", frame.File),
				zap.Int("line", frame.Line),
			)
		}
	}

	// 调用Zap写入日志
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

// WithAttrs 创建子Logger（继承字段）
func (h *ZapSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newZapLogger := h.zapLogger.With(h.slogAttrsToZapFields(attrs)...)
	return &ZapSlogHandler{
		zapLogger: newZapLogger,
		level:     h.level,
		addSource: h.addSource,
	}
}

func (h *ZapSlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h // 空分组无操作
	}

	// 拼接新的分组前缀
	newPrefix := name + "."
	if h.groupPrefix != "" {
		newPrefix = h.groupPrefix + newPrefix
	}

	// 创建新 Handler 并应用分组前缀
	return &ZapSlogHandler{
		zapLogger:   h.zapLogger,
		level:       h.level,
		groupPrefix: newPrefix,
		addSource:   h.addSource,
	}
}

// 辅助方法：转换slog字段到Zap格式
func (h *ZapSlogHandler) slogAttrsToZapFields(attrs []slog.Attr) []zap.Field {
	fields := make([]zap.Field, len(attrs))
	for i, attr := range attrs {
		fields[i] = zap.Any(attr.Key, attr.Value.Any())
	}
	return fields
}

// NewZapSlogHandler 创建 Handler
func NewZapSlogHandler(zapLogger *zap.Logger, level slog.Level) *ZapSlogHandler {
	lv := &slog.LevelVar{}
	lv.Set(level)
	return &ZapSlogHandler{
		zapLogger: zapLogger.WithOptions(zap.AddCallerSkip(1)), // 修正调用层级
		level:     lv,
		addSource: true,
	}
}

func createZapCore(path, logname string) zapcore.Core {
	// 配置Encoder（JSON格式）
	encoderCfg := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "message",
		StacktraceKey:  "stacktrace",
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	}

	// 输出到文件和控制台（双写）
	os.MkdirAll(path, os.ModePerm) //如果path目录不存在则创建
	path += "/" + logname + ".log"
	file, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	multiWriter := zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(file))

	return zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		multiWriter,
		zap.NewAtomicLevelAt(zap.DebugLevel),
	)
}

// 全局初始化
func Run(path, logname, szlevel string) {
	core := createZapCore(path, logname)
	zapLogger := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))
	var level slog.Level = slog.LevelDebug
	level.UnmarshalText([]byte(szlevel))
	handler := NewZapSlogHandler(zapLogger, level)
	slogger_ := slog.New(handler)
	slog.SetDefault(slogger_)
}

func Debug(msg string, args ...any) {
	slog.Debug(msg, args...)
}

func Info(msg string, args ...any) {
	slog.Info(msg, args...)
}

func Warning(msg string, args ...any) {
	slog.Warn(msg, args...)
}

func Error(msg string, args ...any) {
	slog.Error(msg, args...)
}
