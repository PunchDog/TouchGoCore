package mysql

import (
	"errors"
	"strings"
	"testing"

	"touchgocore/config"
)

// ==================== P3：mysql DSN 不得进 Error.SQL ====================

const qaSecretPass = "s3cr3t-pa55"

func TestRedactDSN_StripsPassword(t *testing.T) {
	dsn := "app:" + qaSecretPass + "@tcp(127.0.0.1:3306)/game?parseTime=true&loc=Local&charset=utf8mb4"
	got := redactDSN(dsn)
	if strings.Contains(got, qaSecretPass) {
		t.Fatalf("脱敏后仍含密码: %q", got)
	}
	for _, want := range []string{"app", "127.0.0.1:3306", "game"} {
		if !strings.Contains(got, want) {
			t.Fatalf("脱敏应保留非敏感标识 %q: %q", want, got)
		}
	}
	// 解析失败时宁可整串丢弃也不泄露
	if got := redactDSN("%%%not-a-dsn%%%:" + qaSecretPass); strings.Contains(got, qaSecretPass) {
		t.Fatalf("异常输入泄露密码: %q", got)
	}
}

// 集成口径：拨号失败返回的 *Error，其 SQL 字段不得含密码。
func TestNewClient_OpenErrorSQLHasNoPassword(t *testing.T) {
	cfg := &config.MySqlDBConfig{
		Host:     "127.0.0.1:1", // 立即拒绝，测试不依赖真实 MySQL
		Username: "app",
		Password: qaSecretPass,
		DBName:   "game",
	}
	_, err := NewClient(cfg)
	if err == nil {
		t.Skip("本机 127.0.0.1:1 竟然可连，跳过集成断言")
	}
	var me *Error
	if !errors.As(err, &me) {
		t.Fatalf("应返回 *Error, 得 %T: %v", err, err)
	}
	if strings.Contains(me.SQL, qaSecretPass) {
		t.Fatalf("Error.SQL 含密码明文: %q", me.SQL)
	}
	if strings.Contains(err.Error(), qaSecretPass) {
		t.Fatalf("错误文案含密码明文: %q", err.Error())
	}
}
