package vars

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ============================================================================
// 阶段13复核整改 F2：SetLevel 换绑期间不得留下「日志静默丢弃」的窗口。
//
// 旧写法在改级别时把 writer 置空、把通道摘掉再重建，而 Close 最长要等在途日志落盘
// （默认 5 秒预算）。那段窗口里 submit 既拿不到通道也拿不到 writer，门面日志全部
// 无声消失；重建失败时更是留着 isEnabled=true 让此后每条日志都丢掉。
//
// 通道模式下级别只参与 accepts 的过滤，与句柄/通道的建立无关，因此改成只改过滤位。
// ============================================================================

// writerSnapshot 取当前落地句柄（仅测试用，避免直接读字段造成竞态误判）
func (m *ChannelLoggerManager) writerSnapshot() interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.writer
}

func TestSetLevelInChannelModeKeepsPipeline(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.LogPath, cfg.LogName = dir, "setlevel"
	cfg.LogLevel = LogLevelInfo
	cfg.Async = true
	cfg.AsyncBufferSize = 64
	cfg.Stdout = false

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	if err := m.initWithChannel(); err != nil {
		t.Fatalf("initWithChannel: %v", err)
	}
	channelBefore, writerBefore := m.channel.Load(), m.writerSnapshot()
	if channelBefore == nil || writerBefore == nil {
		t.Fatal("✘ 前置条件：通道模式下通道与句柄都应就绪")
	}

	if err := m.SetLevel(LogLevelError); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}

	// 通道与句柄原封不动：拆链窗口（通道已摘、writer 已置空、Close 还在等）根本不会出现
	if got := m.channel.Load(); got != channelBefore {
		t.Fatalf("✘ SetLevel 重建了异步通道，换绑窗口内日志无人接收: before=%p after=%p", channelBefore, got)
	}
	if got := m.writerSnapshot(); got != writerBefore {
		t.Fatalf("✘ SetLevel 换掉了文件句柄: %v -> %v", writerBefore, got)
	}

	// 级别照样立刻生效：warn 不落地，error 落地
	m.LogAsyncSimple(slog.LevelWarn, "dropped-by-level")
	m.LogAsyncSimple(slog.LevelError, "kept-by-level")
	if err := m.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "setlevel.log"))
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "dropped-by-level") {
		t.Fatalf("✘ SetLevel(Error) 后 warn 仍落地，级别没生效:\n%s", got)
	}
	if !strings.Contains(got, "kept-by-level") {
		t.Fatalf("✘ SetLevel 后 error 未落地:\n%s", got)
	}
}

// TestSetLevelDuringConcurrentWritesKeepsAll 换绑期间持续写日志，一条都不能丢。
//
// 旧写法在 SetLevel 里 nil 掉 writer 再重建，落在该窗口的调用走 writeEntrySync 时
// 拿到 nil 直接返回；这里以「并发写 + 一次改级别」的口径钉住结果。
func TestSetLevelDuringConcurrentWritesKeepsAll(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.LogPath, cfg.LogName = dir, "concurrent"
	cfg.LogLevel = LogLevelInfo
	cfg.Async = true
	cfg.AsyncBufferSize = 1024
	cfg.Stdout = false

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	if err := m.initWithChannel(); err != nil {
		t.Fatalf("initWithChannel: %v", err)
	}

	const per = 200
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				m.LogAsyncSimple(slog.LevelInfo, string(rune('A'+g))+strconv.Itoa(i))
			}
		}(g)
	}
	for i := 0; i < 20; i++ {
		if err := m.SetLevel(LogLevelInfo); err != nil {
			t.Fatalf("SetLevel: %v", err)
		}
	}
	wg.Wait()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "concurrent.log"))
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if got, want := len(contentLines(string(data))), 4*per; got != want {
		t.Fatalf("✘ 改级别期间丢日志: 落地=%d 应=%d", got, want)
	}
}

// TestSetLevelFailureKeepsOldPipeline 非通道模式下重建失败时必须继续用旧句柄写，
// 而不是留下一个 isEnabled=true 却没有任何落地路径的管理器。
func TestSetLevelFailureKeepsOldPipeline(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.LogPath, cfg.LogName = dir, "fallback"
	cfg.LogLevel = LogLevelInfo
	cfg.Async = false
	cfg.Stdout = false

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	writerBefore := m.writerSnapshot()

	// 让重建必然失败：把日志名换成带非法字符的
	m.mu.Lock()
	m.config.LogName = "bad\x00name"
	m.mu.Unlock()
	if err := m.SetLevel(LogLevelWarn); err == nil {
		t.Fatal("✘ 非法日志名未导致重建失败，本用例失去意义")
	}

	if got := m.writerSnapshot(); got != writerBefore {
		t.Fatalf("✘ 重建失败却换掉了落地句柄: %v -> %v", writerBefore, got)
	}
	if !m.IsEnabled() {
		t.Fatal("✘ 重建失败后不该把管理器置为停用")
	}
	m.LogAsyncSimple(slog.LevelWarn, "still-landing")
	_ = m.Close()

	data, err := os.ReadFile(filepath.Join(dir, "fallback.log"))
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(data), "still-landing") {
		t.Fatalf("✘ 改级别失败后日志停写:\n%s", data)
	}
}
