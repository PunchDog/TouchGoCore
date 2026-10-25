package util

import (
	"strings"
	"testing"
)

// TestClip 对外摘要的截断口径：短串原样返回，超过 160 个字符按 rune 截断并追加省略号。
// 按 rune 而不是字节截，避免在多字节字符中间切出乱码。
func TestClip(t *testing.T) {
	if got := Clip("  abc  "); got != "  abc  " {
		t.Errorf("短串被改动: %q", got)
	}
	long := strings.Repeat("错", 200)
	got := Clip(long)
	if r := []rune(got); len(r) != 161 || !strings.HasSuffix(got, "…") {
		t.Errorf("长串截断不符: %d 个字符（期望 160 + 省略号）", len(r))
	}
	if got := Clip(strings.Repeat("a", 160)); len([]rune(got)) != 160 {
		t.Errorf("恰好 160 字符不该被截断: %d", len([]rune(got)))
	}
}
