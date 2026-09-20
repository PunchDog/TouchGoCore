package vars_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"touchgocore/vars"

	fakevars "touchgocore/testdata/vars"
)

// ============================================================================
// 阶段 12（S65）回归用例：日志调用点定位。
//   1. 必须解析到「发起 Enqueue 的那一行」；
//   2. 包路径里带 vars 段的外部包不得被当成本包帧跳过（旧实现用 Contains("/vars.")）。
// ============================================================================

// lockedBuffer 消费者协程写、测试协程读（Flush 之后），加锁只为让并发语义干净。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newCaptureChannel(t *testing.T) (*vars.AsyncLoggerChannel, *lockedBuffer) {
	t.Helper()
	buf := &lockedBuffer{}
	cfg := vars.DefaultAsyncChannelConfig()
	cfg.DropOnFull = true
	ch := vars.NewAsyncLoggerChannel(buf, cfg)
	t.Cleanup(func() { _ = ch.Close() })
	return ch, buf
}

func TestCallerLocationIsBusinessCallSite(t *testing.T) {
	ch, buf := newCaptureChannel(t)

	// 注意：Enqueue 必须紧邻下一行，期望行号按 wantLine+1 计算
	_, wantFile, wantLine, ok := runtime.Caller(0)
	if !ch.Enqueue(slog.LevelInfo, "定位校验", nil, nil) {
		t.Fatal("入队失败")
	}
	if !ok {
		t.Fatal("runtime.Caller 取不到本行位置")
	}
	wantLine++
	if runtime.GOOS == "windows" {
		wantFile = strings.ReplaceAll(wantFile, "/", "\\")
	}

	if err := ch.FlushAndWait(2 * time.Second); err != nil {
		t.Fatalf("FlushAndWait 失败: %v", err)
	}
	want := fmt.Sprintf(`%s:%d`, wantFile, wantLine)
	if got := buf.String(); !strings.Contains(got, want) {
		t.Fatalf("✘ 调用点未定位到业务帧 %s，实际输出：%s", want, got)
	}
}

// TestCallerLocationKeepsForeignVarsPackage 回归（S65）：外部包 touchgocore/testdata/vars
// 的函数名含 "/vars."，旧实现把它当本包帧跳过，调用点被错报成下面的测试文件。
func TestCallerLocationKeepsForeignVarsPackage(t *testing.T) {
	ch, buf := newCaptureChannel(t)

	if !fakevars.EnqueueFromHelper(ch, slog.LevelInfo, "来自外部同名包") {
		t.Fatal("入队失败")
	}
	if err := ch.FlushAndWait(2 * time.Second); err != nil {
		t.Fatalf("FlushAndWait 失败: %v", err)
	}

	got := buf.String()
	sep := "/"
	if runtime.GOOS == "windows" {
		sep = `\`
	}
	wantTail := "testdata" + sep + "vars" + sep + "vars.go:"
	if !strings.Contains(got, wantTail) {
		t.Fatalf("✘ 外部 vars 包被误判为本包帧跳过，未定位到 %s，实际输出：%s", wantTail, got)
	}
}
