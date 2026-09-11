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
	if !hasFormatVerbs("100%") {
		t.Fatal("100% 应识别为占位符")
	}
	if hasFormatVerbs("100%%") {
		t.Fatal("100%% 不应识别为占位符")
	}
	if hasFormatVerbs("plain") {
		t.Fatal("普通字符串不应识别为占位符")
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
