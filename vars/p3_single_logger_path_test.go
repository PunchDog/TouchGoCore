package vars

import (
	"bufio"
	"log/slog"
	"os"
	"path"
	"strings"
	"testing"
)

// ============================================================================
// S70：vars 双通道合并 —— 全局日志器只剩一条路，文件句柄归 ChannelLoggerManager 独占。
//
// 变更前有两处旁路：
//  1. Initialize 顺带建一个同步 LoggerManager 当 fallback，两者各开一个句柄写同名
//     .log；旁路句柄不认识旋转，轮转后继续往改名后的备份文件里追加，且格式是 zap JSON。
//  2. 异步模式下管理器没有 slog.Handler，GetLogger() 退回 slog.Default()，
//     slog.SetDefault 成了自我赋值，宿主直接用 slog.Info 的一条都进不了文件。
//
// 下面的用例分别钉住这两条，外加「Shutdown 之后可再 Initialize」与级别过滤。
// ============================================================================

// withGlobalLogger 在子用例期间接管全局日志器并在退出时交还原状。
// 返回日志目录与管理器句柄，便于直接改内部状态模拟换绑窗口。
func withGlobalLogger(t *testing.T, tune func(*LogConfig)) (string, *ChannelLoggerManager) {
	t.Helper()

	prevManager := GetChannelLogger()
	prevDefault := slog.Default()
	prevActive := loggerActive

	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.LogName = "single"
	cfg.LogLevel = LogLevelInfo
	cfg.Stdout = false
	if tune != nil {
		tune(&cfg)
	}

	// 注册顺序即反向执行顺序：必须先关管理器，再让 TempDir 删目录，
	// 否则删除撞上一个还开着句柄的日志文件。
	t.Cleanup(func() {
		loggerMu.Lock()
		_ = shutdownChannelLoggerLocked()
		globalChannelLogger.Store(prevManager)
		loggerActive = prevActive
		releaseSlogDefault()
		loggerMu.Unlock()
		slog.SetDefault(prevDefault)
	})

	if err := Initialize(cfg); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// 同一用例里第二次 Initialize 必须是无操作：重复装配等于再开一个句柄写同一份文件
	if err := Initialize(cfg); err != nil {
		t.Fatalf("重复 Initialize: %v", err)
	}

	manager := GetChannelLogger()
	if manager == nil {
		t.Fatal("Initialize 之后全局管理器仍为空")
	}
	return cfg.LogPath, manager
}

// readLog 读取管理器当前写的那份主日志（.log），不存在时返回空串
func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(path.Join(dir, name+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("读取日志失败: %v", err)
	}
	return string(data)
}

// contentLines 去掉空行，便于逐行核对格式
func contentLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func mustFlush(t *testing.T) {
	t.Helper()
	if err := FlushAsyncLogs(); err != nil {
		t.Fatalf("FlushAsyncLogs: %v", err)
	}
}

// TestAsyncSlogReachesFile 异步模式下 slog.Info 必须落进日志文件（旧版一条都不落）。
func TestAsyncSlogReachesFile(t *testing.T) {
	dir, _ := withGlobalLogger(t, nil)

	slog.Info("slog-direct-entry", "uid", "1001")
	mustFlush(t)

	data := readLog(t, dir, "single")
	if !strings.Contains(data, "slog-direct-entry") {
		t.Fatalf("✘ slog 入口没进文件（GetLogger 仍退回 slog.Default）:\n%s", data)
	}
	if !strings.Contains(data, "uid=1001") {
		t.Fatalf("✘ slog 附带的字段丢了:\n%s", data)
	}
	// 调用点必须解析到本测试文件：Record.PC 用错就变成 slog 内部帧或空
	if !strings.Contains(data, "p3_single_logger_path_test.go") {
		t.Fatalf("✘ slog 入口调用点没解析到业务帧:\n%s", data)
	}
	if strings.Contains(data, `"message":"slog-direct-entry"`) {
		t.Fatalf("✘ 通道模式写出了 zap JSON 格式:\n%s", data)
	}
}

// TestFacadeAndSlogShareOnePath 门面与 slog 两条入口落到同一个文件、同一种行格式。
func TestFacadeAndSlogShareOnePath(t *testing.T) {
	dir, _ := withGlobalLogger(t, nil)

	Warning("facade-line")
	slog.Warn("slog-line")
	mustFlush(t)

	lines := contentLines(readLog(t, dir, "single"))
	if len(lines) != 2 {
		t.Fatalf("✘ 期望两行同源日志，实际 %d 行:\n%q", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "20") || !strings.Contains(line, " WARN [") {
			t.Fatalf("✘ 有一行不是通道格式（第二条句柄或 zap 混写）: %q", line)
		}
	}
	if !strings.Contains(lines[0], "facade-line") || !strings.Contains(lines[1], "slog-line") {
		t.Fatalf("✘ 两条入口的顺序或内容不符: %q", lines)
	}
}

// TestDegradedWriteKeepsSameFileAndFormat 通道缺席（SetLevel 换绑窗口）时，降级写入
// 必须同步落到管理器自己那个句柄上，且格式与消费者逐字相同。
// 旧版这里是「通道不在就把整行丢掉」，需要降级时又另开一个 JSON 句柄。
func TestDegradedWriteKeepsSameFileAndFormat(t *testing.T) {
	dir, manager := withGlobalLogger(t, nil)

	Info("normal-entry")
	mustFlush(t)

	// 模拟换绑窗口：channel 已摘走、管理器仍是启用状态
	manager.channel.Store(nil)
	Warning("degraded-entry")

	data := readLog(t, dir, "single")
	if !strings.Contains(data, "normal-entry") {
		t.Fatalf("✘ 常态写入没落盘:\n%s", data)
	}
	if !strings.Contains(data, "degraded-entry") {
		t.Fatalf("✘ 通道缺席时日志被丢弃:\n%s", data)
	}
	// 两行必须同格式：降级分支共用 appendEntry，不因为换路径就换格式
	for _, line := range contentLines(data) {
		if !strings.Contains(line, " WARN [") && !strings.Contains(line, " INFO [") {
			t.Fatalf("✘ 降级写出的格式与消费者不一致: %q", line)
		}
	}
}

// TestNoBypassAfterManagerDisabled 管理器停用后不得再有第二条句柄补写。
// 旧版 writeToFile 在 !IsEnabled 时退回独立的 LoggerManager，写出的正是那种内容。
func TestNoBypassAfterManagerDisabled(t *testing.T) {
	dir, manager := withGlobalLogger(t, nil)

	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	Warning("post-close-entry")
	mustFlush(t)

	if data := readLog(t, dir, "single"); strings.Contains(data, "post-close-entry") {
		t.Fatalf("✘ 已关闭的管理器仍被旁路句柄写入（轮转后这就是写进备份文件的成因）:\n%s", data)
	}
	// 旁路句柄开的是它自己的文件，目录里因此会多出第二份同名日志
	if entries := mustReadDir(t, dir); len(entries) != 1 || entries[0].Name() != "single.log" {
		t.Fatalf("✘ 日志目录里出现了第二份文件: %v", names(entries))
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestDirectAsyncCallerRespectsLevel 级别过滤收在管理器里：绕过门面直接调
// LogAsyncSimple 的调用方不能把低级别日志灌进高级别文件。
func TestDirectAsyncCallerRespectsLevel(t *testing.T) {
	dir, manager := withGlobalLogger(t, func(c *LogConfig) { c.LogLevel = LogLevelWarn })

	manager.LogAsyncSimple(slog.LevelInfo, "info-under-warn")
	manager.LogAsync(slog.LevelError, "error-under-warn")
	mustFlush(t)

	data := readLog(t, dir, "single")
	if strings.Contains(data, "info-under-warn") {
		t.Fatalf("✘ 低于配置级别的日志写进了文件:\n%s", data)
	}
	if !strings.Contains(data, "error-under-warn") {
		t.Fatalf("✘ 达到级别的日志没写进文件:\n%s", data)
	}
}

// TestReinitializeAfterShutdown Shutdown 之后可以再 Initialize（旧版 sync.Once 焊死，
// 第二次初始化静默无效，新配置整份丢掉）。
func TestReinitializeAfterShutdown(t *testing.T) {
	firstDir := t.TempDir()
	first := DefaultConfig()
	first.LogPath = firstDir
	first.LogName = "first"
	first.LogLevel = LogLevelInfo
	first.Stdout = false

	prevDefault := slog.Default()
	t.Cleanup(func() {
		loggerMu.Lock()
		_ = shutdownChannelLoggerLocked()
		loggerActive = false
		releaseSlogDefault()
		loggerMu.Unlock()
		slog.SetDefault(prevDefault)
	})

	if err := Initialize(first); err != nil {
		t.Fatalf("第一次 Initialize: %v", err)
	}
	Info("entry-in-first")
	mustFlush(t)
	if err := Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !strings.Contains(readLog(t, firstDir, "first"), "entry-in-first") {
		t.Fatal("✘ 第一轮日志未落盘")
	}

	secondDir := t.TempDir()
	second := DefaultConfig()
	second.LogPath = secondDir
	second.LogName = "second"
	second.LogLevel = LogLevelError
	second.Stdout = false
	if err := Initialize(second); err != nil {
		t.Fatalf("第二次 Initialize: %v", err)
	}

	Info("info-in-second")
	Error("error-in-second")
	mustFlush(t)

	data := readLog(t, secondDir, "second")
	if data == "" {
		t.Fatalf("✘ Shutdown 之后重新初始化无效（第二次 Initialize 被 Once 吞掉）:\n第一个目录内容=%v",
			names(mustReadDir(t, secondDir)))
	}
	if strings.Contains(data, "info-in-second") {
		t.Fatalf("✘ 新配置未生效，仍按旧级别写入:\n%s", data)
	}
	if !strings.Contains(data, "error-in-second") {
		t.Fatalf("✘ 新管理器没有收到日志:\n%s", data)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录 %s: %v", dir, err)
	}
	return entries
}

// TestSlogDefaultAdoptedAndReleased 默认 slog 记录器随 Initialize 接管、随 Shutdown 交还。
// 交还这一步不能省：默认器若还指着已关闭管理器的 handler，进程收尾阶段的 slog.Info
// （退出日志、panic 打印）会静默消失，而那正是最需要留在现场的一条。
func TestSlogDefaultAdoptedAndReleased(t *testing.T) {
	_, _ = withGlobalLogger(t, nil)

	if _, ok := slog.Default().Handler().(*channelSlogHandler); !ok {
		t.Fatalf("✘ 默认 slog 未被接管，handler=%T", slog.Default().Handler())
	}

	if err := Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, ok := slog.Default().Handler().(*channelSlogHandler); ok {
		t.Fatal("✘ Shutdown 后默认 slog 仍指着已关闭的管理器")
	}
}

// TestNonAsyncModeKeepsSingleFormat 非异步模式仍走 zap，同一路径不混两种格式。
func TestNonAsyncModeKeepsSingleFormat(t *testing.T) {
	dir, _ := withGlobalLogger(t, func(c *LogConfig) { c.Async = false })

	Warning("facade-sync")
	slog.Warn("slog-sync")
	mustFlush(t)

	data := readLog(t, dir, "single")
	for _, want := range []string{`"message":"facade-sync"`, `"message":"slog-sync"`} {
		if !strings.Contains(data, want) {
			t.Fatalf("✘ 非异步模式缺少 %s:\n%s", want, data)
		}
	}
	for _, line := range contentLines(data) {
		if !strings.HasPrefix(line, "{") {
			t.Fatalf("✘ 同一份文件里混进了通道格式（两种格式混写）: %q", line)
		}
	}
}
