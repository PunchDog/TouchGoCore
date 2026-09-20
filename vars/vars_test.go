package vars

import (
	"log/slog"
	"testing"
)

func TestLogConfig_Default(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxSize != 100 || cfg.MaxAge != 30 || cfg.MaxBackups != 10 {
		t.Fatalf("默认值异常: %+v", cfg)
	}
	if !cfg.Async || cfg.AsyncBufferSize != 10000 {
		t.Fatalf("异步默认值异常: %+v", cfg)
	}
	if cfg.Fields == nil {
		t.Fatal("Fields 应当预分配为非 nil")
	}
}

func TestLogConfig_Validate_Normalize(t *testing.T) {
	cfg := LogConfig{LogLevel: "INFO", LogPath: "./logs", LogName: "x"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate 失败: %v", err)
	}
	if cfg.MaxSize != 100 || cfg.AsyncBufferSize != 10000 {
		t.Fatalf("validate 应填默认值")
	}
}

func TestLogConfig_Validate_InvalidLevel(t *testing.T) {
	cfg := LogConfig{LogLevel: "WRONG"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("非法级别应当报错")
	}
}

func TestLogConfig_Validate_LevelCaseInsensitive(t *testing.T) {
	cfg := LogConfig{LogLevel: "DeBuG"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("大小写不敏感失败: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("未归一化为小写: %s", cfg.LogLevel)
	}
}

func TestLogConfig_Validate_Off(t *testing.T) {
	cfg := LogConfig{LogLevel: "off"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("off 应通过: %v", err)
	}
}

func TestLogLevels(t *testing.T) {
	if LogLevelDebug != "debug" || LogLevelInfo != "info" ||
		LogLevelWarn != "warn" || LogLevelError != "error" || LogLevelOff != "off" {
		t.Fatal("日志级别常量变更")
	}
}

func TestAssembleLine_NoArgs(t *testing.T) {
	if got := assembleLine("hello", nil); got != "hello" {
		t.Fatalf("无参应为原串: %s", got)
	}
}

func TestAssembleLine_WithFormatVerbs(t *testing.T) {
	if got := assembleLine("x=%d", []any{42}); got != "x=42" {
		t.Fatalf("占位符格式化失败: %s", got)
	}
}

func TestAssembleLine_WithoutVerbs(t *testing.T) {
	got := assembleLine("hello", []any{1, "world"})
	if got != "hello 1 world" {
		t.Fatalf("无占位符应拼接: %s", got)
	}
}

func TestHasFormatVerbs_EscapedPercent(t *testing.T) {
	if hasFormatVerbs("100%") {
		t.Fatal("尾部裸百分号是字面量，不应识别为占位符")
	}
	if hasFormatVerbs("完成度 100% 以上") {
		t.Fatal("后接非动词字符的百分号不应识别为占位符")
	}
	if hasFormatVerbs("100%%") {
		t.Fatal("100%% 不应识别为占位符")
	}
	if hasFormatVerbs("plain") {
		t.Fatal("普通字符串不应识别为占位符")
	}
	if !hasFormatVerbs("a=%d") {
		t.Fatal("带占位动词的串应识别为占位符")
	}
}

func TestCountFormatVerbs(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"plain", 0},
		{"100%", 0},
		{"完成度 100% 以上", 0},
		{"100%%", 0},
		{"%%d", 0},
		{"%d", 1},
		{"x=%d y=%s", 2},
		{"%v %+v %-20.3f", 3},
		{"%[2]d%[1]s", 2},
		{"进度 50%% 共 %d 项", 1},
		{"%q %t %T %p %e %g %x %X %b %c %o %U", 12},
		{"%", 0},
		{"%!", 0},
	}

	for _, c := range cases {
		if got := countFormatVerbs(c.in); got != c.want {
			t.Fatalf("countFormatVerbs(%q)=%d want=%d", c.in, got, c.want)
		}
	}
}

// TestAssembleLine_PercentLiteral 回归：中文日志里常见的 "100% 完成" 曾被当成格式串，
// 导致参数被 Sprintf 吞掉、占位符错位。
func TestAssembleLine_PercentLiteral(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		args []any
		want string
	}{
		{"裸百分号", "100% 完成", []any{"任务"}, "100% 完成 任务"},
		{"百分号结尾", "进度 50%", []any{3, "x"}, "进度 50% 3 x"},
		{"转义百分号", "100%% 命中", []any{"规则"}, "100%% 命中 规则"},
		{"真实占位符", "命中 %s 共 %d 条", []any{"敏感词", 3}, "命中 敏感词 共 3 条"},
		// 混合场景：裸百分号必须先转义，否则 fmt 会把中文字节当动词吃掉参数
		{"百分号与占位符混排", "50% 命中 %d", []any{7}, "50% 命中 7"},
		{"百分号在占位符后", "命中 %s，完成率 100%", []any{"规则"}, "命中 规则，完成率 100%"},
	}

	for _, c := range cases {
		if got := assembleLine(c.msg, c.args); got != c.want {
			t.Fatalf("%s: assembleLine(%q,%v)=%q want=%q", c.name, c.msg, c.args, got, c.want)
		}
	}
}

func TestEscapeStrayPercents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"%d", "%d"},
		{"100%%", "100%%"},
		{"100%", "100%%"},
		{"% 命中 %d", "%% 命中 %d"},
		{"%%d", "%%d"},
		{"%a%b", "%%a%b"},
	}

	for _, c := range cases {
		if got := escapeStrayPercents(c.in); got != c.want {
			t.Fatalf("escapeStrayPercents(%q)=%q want=%q", c.in, got, c.want)
		}
	}
}

func TestConsoleLevelPrefix(t *testing.T) {
	cases := []struct {
		lvl  slog.Level
		want string
	}{
		{slog.LevelDebug, "[DEBUG]"},
		{slog.LevelInfo, "[INFO]"},
		{slog.LevelWarn, "[WARN]"},
		{slog.LevelError, "[ERROR]"},
		{slog.Level(99), "[INFO]"},
	}
	for _, c := range cases {
		if got := consoleLevelPrefix(c.lvl); got != c.want {
			t.Fatalf("level=%v want=%s got=%s", c.lvl, c.want, got)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"OFF", slog.Level(1000)},
		{"unknown", slog.LevelInfo},
	}
	for _, c := range cases {
		if got := parseLogLevel(c.in); got != c.want {
			t.Fatalf("parseLogLevel(%q)=%v want=%v", c.in, got, c.want)
		}
	}
}

func TestDefaultAsyncChannelConfig(t *testing.T) {
	c := DefaultAsyncChannelConfig()
	if c.BufferSize != 10000 || c.BatchSize != 100 || c.FlushThreshold != 4096 {
		t.Fatalf("异步通道默认配置异常: %+v", c)
	}
	if c.DropOnFull {
		t.Fatal("默认应阻塞而非丢日志")
	}
}

func TestLogConfig_Validate_NegativeMaxAge(t *testing.T) {
	cfg := LogConfig{LogLevel: "INFO", MaxAge: -1}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate 失败: %v", err)
	}
	if cfg.MaxAge != 30 {
		t.Fatalf("负值 MaxAge 应回退到默认 30，实际=%d", cfg.MaxAge)
	}
}

func TestLogConfig_Validate_NegativeCallerSkip(t *testing.T) {
	cfg := LogConfig{LogLevel: "INFO", CallerSkip: -2}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate 失败: %v", err)
	}
	if cfg.CallerSkip != 1 {
		t.Fatalf("负值 CallerSkip 应回退到 1，实际=%d", cfg.CallerSkip)
	}
}
