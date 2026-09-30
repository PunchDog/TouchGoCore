package telegram

import (
	"strings"
	"testing"
)

// withPanicHook 在校验函数入口注入 panic，返回清理函数。
// 注入点在生产代码里恒为 nil（testPanicHook），只有测试会短暂置位。
func withPanicHook(t *testing.T) {
	t.Helper()
	old := testPanicHook
	testPanicHook = func() { panic("injected boom SECRET-INPUT") }
	t.Cleanup(func() { testPanicHook = old })
}

// TestValidateWebAppDataPanicFailsClosed panic 被 recover 后必须返回非 nil error：
// 返回值不具名时 recover 分支返回的是零值 nil error，「没校验完」会被当成「校验通过」。
func TestValidateWebAppDataPanicFailsClosed(t *testing.T) {
	withPanicHook(t)
	result, err := validateWebAppData("tok", "auth_date=1&hash=deadbeef")
	if err == nil {
		t.Fatalf("panic 后必须返回 error（fail-closed），实得 result=%v err=nil", result)
	}
	if result != nil {
		t.Fatalf("panic 后不得给出半份结果: %v", result)
	}
	// 错误文案不得带出 panic 内容或输入数据。
	if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "SECRET-INPUT") {
		t.Fatalf("错误文案泄漏 panic 内容/输入: %v", err)
	}
}

// TestTelegramVerifyPanicFailsClosed 同上：TelegramVerify 的 recover 分支必须置 error。
func TestTelegramVerifyPanicFailsClosed(t *testing.T) {
	withPanicHook(t)
	id, username, err := TelegramVerify("auth_date=1&hash=deadbeef")
	if err == nil {
		t.Fatalf("panic 后必须返回 error（fail-closed），实得 id=%q username=%q err=nil", id, username)
	}
	if id != "" || username != "" {
		t.Fatalf("panic 后不得给出半份身份: id=%q username=%q", id, username)
	}
	if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "SECRET-INPUT") {
		t.Fatalf("错误文案泄漏 panic 内容/输入: %v", err)
	}
}

// TestHashCompareConstantTimeBehavior hmac.Equal 替换后既有判定语义不变：
// 正确 hash 通过、篡改 hash 拒绝、长度不同的 hash 也拒绝（hmac.Equal 对异长直接 false）。
func TestHashCompareConstantTimeBehavior(t *testing.T) {
	// 长度不同的 hash 值：hmac.Equal 必须直接判 false，且不得 panic。
	if _, err := validateWebAppData("tok", "auth_date=1&hash=short"); err == nil {
		t.Fatal("异长 hash 应当被拒")
	}
	if _, err := validateWebAppData("tok", "auth_date=1&hash="+strings.Repeat("ab", 64)); err == nil {
		t.Fatal("64 字节 hex 形态的错误 hash 也应当被拒")
	}
}
