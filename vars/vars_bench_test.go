package vars

import (
	"log/slog"
	"strings"
	"testing"
)

func BenchmarkHasFormatVerbs(b *testing.B) {
	cases := []string{
		"hello world",
		"100%",
		"100%%done",
		"x=%d y=%s",
		"plain text",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, c := range cases {
			_ = hasFormatVerbs(c)
		}
	}
}

func BenchmarkAssembleLine_NoVerb(b *testing.B) {
	args := []any{1, "world", 3.14, true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = assembleLine("hello", args)
	}
}

func BenchmarkAssembleLine_WithVerb(b *testing.B) {
	args := []any{42, "alice", 3.14}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = assembleLine("x=%d name=%s v=%.2f", args)
	}
}

func BenchmarkConsoleLevelPrefix(b *testing.B) {
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range levels {
			_ = consoleLevelPrefix(l)
		}
	}
}

func BenchmarkStringBuilder_Concat(b *testing.B) {
	var sb strings.Builder
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sb.Reset()
		sb.WriteString("hello")
		sb.WriteByte(' ')
		sb.WriteString("world")
		_ = sb.String()
	}
}
