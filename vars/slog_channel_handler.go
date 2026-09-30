package vars

import (
	"context"
	"log/slog"
	"time"
)

// ============================================================================
// slog → 异步通道（S70）。
//
// 门面函数（Debug/Info/Warning/Error）本来就直投通道，但宿主与三方库里大量代码用
// 的是 slog.Info / logger.With(...) 这一套。通道模式下管理器过去没有可用的
// slog.Handler，GetLogger() 退回 slog.Default()，slog.SetDefault 于是变成自我赋值，
// 这些调用一条都不会进日志文件。本文件补上这条链路。
//
// 落地仍走 writeLogged 这唯一的入口：级别过滤、通道投递、通道缺席时同步写同一个
// 文件句柄，全部与门面路径共用，不在这里另开第二份实现。
// ============================================================================

// channelSlogHandler 把 slog 记录接进 ChannelLoggerManager
type channelSlogHandler struct {
	m           *ChannelLoggerManager
	attrs       []slog.Attr // WithAttrs 累积的公共字段
	groupPrefix string      // WithGroup 前缀，与 OptimizedZapSlogHandler 同语义
}

func newChannelSlogHandler(m *ChannelLoggerManager) *channelSlogHandler {
	return &channelSlogHandler{m: m}
}

// Enabled 与文件写入口径一致：off 与低于配置级别的记录不落地
// （命令行另说，Info 及以上始终会打屏，那是 printToConsole 的职责）。
func (h *channelSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.m.isEnabled.Load() && !h.m.off.Load() && h.m.ShouldWriteFile(level)
}

func (h *channelSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	attrs = append(attrs, h.attrs...)
	if r.NumAttrs() > 0 {
		r.Attrs(func(attr slog.Attr) bool {
			if h.groupPrefix != "" {
				attr.Key = h.groupPrefix + attr.Key
			}
			attrs = append(attrs, attr)
			return true
		})
	}

	file, line := callerFromPC(r.PC)
	when := r.Time
	if when.IsZero() {
		// 直接调用 handler 的记录不会填时间，落成年份 0001 等于没有现场
		when = time.Now()
	}
	h.m.enqueueRecord(logEntry{
		level:   r.Level,
		msg:     r.Message,
		time:    when,
		attrs:   attrs,
		context: ctx,
		file:    file,
		line:    line,
	})
	return nil
}

// WithAttrs 派生带公共字段的子处理器；空字段沿用自身，避免每条日志多一次分配
func (h *channelSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &channelSlogHandler{m: h.m, attrs: merged, groupPrefix: h.groupPrefix}
}

func (h *channelSlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	prefix := name + "."
	if h.groupPrefix != "" {
		prefix = h.groupPrefix + prefix
	}
	return &channelSlogHandler{m: h.m, attrs: h.attrs, groupPrefix: prefix}
}

// ============================================================================
// managerSlogHandler：把 slog 调用间接到管理器「当前」持有的 handler 上。
//
// adoptSlogDefault 是一次性快照：slog.SetDefault(m.GetLogger()) 之后，zap 模式的
// SetLevel 会重建整条 writer/handler 链路并关闭旧句柄。若 GetLogger 交出的是当时的
// handler 实例，slog.Default 会永远绑在已关闭的旧链路上继续写，日志静默丢失。
// 间接层每次调用都在 m.mu 读锁下解析当前 handler，天然跟随 SetLevel 换代。
// ============================================================================

type managerSlogHandler struct {
	m *ChannelLoggerManager
}

// current 取当前生效的 handler 快照（可能为 nil）。
func (h *managerSlogHandler) current() slog.Handler {
	h.m.mu.RLock()
	defer h.m.mu.RUnlock()
	return h.m.slogHandler
}

func (h *managerSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if c := h.current(); c != nil {
		return c.Enabled(ctx, level)
	}
	return false
}

func (h *managerSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if c := h.current(); c != nil {
		return c.Handle(ctx, r)
	}
	return nil
}

func (h *managerSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	if c := h.current(); c != nil {
		return c.WithAttrs(attrs)
	}
	return h
}

func (h *managerSlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	if c := h.current(); c != nil {
		return c.WithGroup(name)
	}
	return h
}
