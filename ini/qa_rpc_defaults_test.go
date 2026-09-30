package ini

import (
	"os"
	"path/filepath"
	"testing"
)

// ============================================================================
// 槽位 RPC 复核修复回归（T6-E 移交）：
//   - GetInt32/GetUint32/GetInt64/GetUint64/GetFloat32/GetFloat64 吞掉解析错误，
//     键缺失或值非法时返回 0 而不是调用方给的默认值。修复后错误一律回默认值。
//     红：缺键断言全部拿到 0；绿：拿到默认值。
// ============================================================================

func TestQaRpcNumericDefaultsHonored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "def.ini")
	if err := os.WriteFile(path, []byte("[S]\nbad = abc\nempty =\nok = 42\nf = 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	// 键缺失 → 默认值（修复前：0）
	if got := cfg.GetInt32("S", "missing", 7); got != 7 {
		t.Fatalf("✘ GetInt32 缺键应回默认 7，实际 %d", got)
	}
	if got := cfg.GetUint32("S", "missing", 9); got != 9 {
		t.Fatalf("✘ GetUint32 缺键应回默认 9，实际 %d", got)
	}
	if got := cfg.GetInt64("S", "missing", 11); got != 11 {
		t.Fatalf("✘ GetInt64 缺键应回默认 11，实际 %d", got)
	}
	if got := cfg.GetUint64("S", "missing", 13); got != 13 {
		t.Fatalf("✘ GetUint64 缺键应回默认 13，实际 %d", got)
	}
	if got := cfg.GetFloat32("S", "missing", 1.5); got != 1.5 {
		t.Fatalf("✘ GetFloat32 缺键应回默认 1.5，实际 %v", got)
	}
	if got := cfg.GetFloat64("S", "missing", 2.5); got != 2.5 {
		t.Fatalf("✘ GetFloat64 缺键应回默认 2.5，实际 %v", got)
	}

	// 值非法 / 空值 → 默认值
	if got := cfg.GetInt32("S", "bad", 7); got != 7 {
		t.Fatalf("✘ GetInt32 非法值应回默认 7，实际 %d", got)
	}
	if got := cfg.GetInt64("S", "empty", 11); got != 11 {
		t.Fatalf("✘ GetInt64 空值应回默认 11，实际 %d", got)
	}
	if got := cfg.GetFloat64("S", "bad", 2.5); got != 2.5 {
		t.Fatalf("✘ GetFloat64 非法值应回默认 2.5，实际 %v", got)
	}

	// 合法值照常解析
	if got := cfg.GetInt32("S", "ok", 7); got != 42 {
		t.Fatalf("✘ GetInt32 合法值应为 42，实际 %d", got)
	}
	if got := cfg.GetInt64("S", "ok", 7); got != 42 {
		t.Fatalf("✘ GetInt64 合法值应为 42，实际 %d", got)
	}
	if got := cfg.GetUint32("S", "ok", 7); got != 42 {
		t.Fatalf("✘ GetUint32 合法值应为 42，实际 %d", got)
	}
	if got := cfg.GetUint64("S", "ok", 7); got != 42 {
		t.Fatalf("✘ GetUint64 合法值应为 42，实际 %d", got)
	}
	if got := cfg.GetFloat64("S", "f", 0); got != 1.25 {
		t.Fatalf("✘ GetFloat64 合法值应为 1.25，实际 %v", got)
	}

	// section 缺失 → 默认值
	if got := cfg.GetInt64("NOSEC", "missing", 5); got != 5 {
		t.Fatalf("✘ section 缺失应回默认 5，实际 %d", got)
	}
}
