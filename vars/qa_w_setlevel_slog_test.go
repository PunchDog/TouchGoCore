package vars

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// ============================================================================
// qa_w 修复回归（槽位 W）：
//   V1 SetLevel（zap 模式）重建 writer 后，adoptSlogDefault 时拿到的 slog.Logger
//      仍持旧 handler 快照 → 继续写已关闭句柄，日志静默消失。
// ============================================================================

// TestQaW_SlogFollowsSetLevelRebuild GetLogger 返回的 logger 在 SetLevel
// 重建链路后必须仍然落盘。修复前它绑死旧 handler，写入已关闭的旧 writer。
func TestQaW_SlogFollowsSetLevelRebuild(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.LogName = "qa_w_setlevel"
	cfg.Async = false
	cfg.Stdout = false
	cfg.LogLevel = LogLevelInfo

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	defer m.Close()

	// 模拟 adoptSlogDefault：一次性快照 GetLogger()
	logger := m.GetLogger()
	logger.Error("qa_w_marker_before")

	// zap 模式 SetLevel 会重建 writer 并关闭旧 writer
	if err := m.SetLevel(LogLevelDebug); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	logger.Error("qa_w_marker_after")
	_ = m.Flush()

	data, err := os.ReadFile(filepath.Join(cfg.LogPath, cfg.LogName+".log"))
	if err != nil {
		t.Fatalf("读取日志文件: %v", err)
	}
	if !bytes.Contains(data, []byte("qa_w_marker_before")) {
		t.Fatal("✘ SetLevel 之前的日志未落盘（测试环境异常）")
	}
	if !bytes.Contains(data, []byte("qa_w_marker_after")) {
		t.Fatal("✘ SetLevel 后 slog 仍持旧 handler 快照，日志写进了已关闭句柄")
	}
}
