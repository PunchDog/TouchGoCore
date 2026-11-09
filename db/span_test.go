package db

// ============================================================================
// 运行时 span 验证（C11）
//
// cache_api.go 的 startDBSpan / recordDBSpanError 只调用 otel.Tracer(...).Start(...)，
// 未配置 TracerProvider 时返回 noop，**看不出是否真的发出了 span**。
// 本用例在测试内临时安装 SDK TracerProvider + SpanRecorder，直接断言：
//   - startDBSpan 真的产出一条带名字的 span；
//   - span 带 db.system / db.table 两个属性；
//   - ctx 为 nil 时回退 background 仍能正常记录；
//   - recordDBSpanError 把错误记成 Error 状态（nil 错误不改状态）。
//
// 用例不依赖 MySQL / Redis，只验证埋点机制本身。
// ============================================================================

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installSpanRecorder 安装一个临时的 SDK TracerProvider 并把 span 收进内存 recorder。
//
// 必须在 t.Cleanup 里恢复全局 provider：otel.SetTracerProvider 是进程级全局状态，
// 不恢复会污染同包后续用例。本包用例均未使用 t.Parallel，顺序执行即可。
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	// SpanRecorder 本身就是 SpanProcessor，直接挂进 provider 即可在 OnEnd 收到 span。
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("关闭 TracerProvider: %v", err)
		}
	})
	return recorder
}

// spanAttrs 把 span 属性展开成 map[string]string，非字符串值以 AsString 归一。
func spanAttrs(kvs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

// findEndedSpan 按名字在已结束 span 里查找；找不到返回 nil。
func findEndedSpan(recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// TestStartDBSpanRecordsSpan 验证 startDBSpan 在真实 TracerProvider 下发出 span，
// 属性、ctx 回退与错误状态都符合预期。
func TestStartDBSpanRecordsSpan(t *testing.T) {
	t.Run("records span name and attributes", func(t *testing.T) {
		recorder := installSpanRecorder(t)

		ctx, span := startDBSpan(context.Background(), "test.span", "task_progress")
		if ctx == nil {
			t.Fatal("startDBSpan 返回 nil ctx")
		}
		// 配了真 provider 后 span 必须是有效的（noop span 的 SpanContext 无效）。
		if !span.SpanContext().IsValid() {
			t.Fatal("span 上下文无效，说明没有走真实 TracerProvider")
		}
		if !span.IsRecording() {
			t.Fatal("span 未处于记录状态")
		}
		span.End()

		got := findEndedSpan(recorder, "test.span")
		if got == nil {
			t.Fatalf("未记录到名为 test.span 的 span，实际 %d 条", len(recorder.Ended()))
		}
		attrs := spanAttrs(got.Attributes())
		if attrs["db.system"] != "touchgocore.dbcache" {
			t.Fatalf("db.system=%q，期望 touchgocore.dbcache", attrs["db.system"])
		}
		if attrs["db.table"] != "task_progress" {
			t.Fatalf("db.table=%q，期望 task_progress", attrs["db.table"])
		}
		if got.Status().Code != codes.Unset {
			t.Fatalf("无错误时 span 状态应为 Unset，实际 %v", got.Status().Code)
		}
	})

	t.Run("nil ctx falls back to background", func(t *testing.T) {
		recorder := installSpanRecorder(t)

		// ctx 为 nil 时不得 panic，且仍要发出 span（回退 context.Background）。
		_, span := startDBSpan(nil, "test.span.nilctx", "task_def")
		span.End()

		got := findEndedSpan(recorder, "test.span.nilctx")
		if got == nil {
			t.Fatal("ctx 为 nil 时未记录到 span")
		}
		if attrs := spanAttrs(got.Attributes()); attrs["db.table"] != "task_def" {
			t.Fatalf("db.table=%q，期望 task_def", attrs["db.table"])
		}
	})

	t.Run("records error status", func(t *testing.T) {
		recorder := installSpanRecorder(t)

		_, span := startDBSpan(context.Background(), "test.span.error", "task_progress")
		// nil 错误不得改状态，也不得 panic。
		recordDBSpanError(span, nil)
		recordDBSpanError(span, errors.New("boom"))
		span.End()

		got := findEndedSpan(recorder, "test.span.error")
		if got == nil {
			t.Fatal("未记录到错误用例 span")
		}
		if got.Status().Code != codes.Error {
			t.Fatalf("span 状态=%v，期望 Error", got.Status().Code)
		}
		if !strings.Contains(got.Status().Description, "boom") {
			t.Fatalf("状态描述=%q，期望包含 boom", got.Status().Description)
		}
		if len(got.Events()) == 0 {
			t.Fatal("RecordError 应产生一条 exception 事件")
		}
	})
}
