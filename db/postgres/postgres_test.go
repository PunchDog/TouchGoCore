package postgres

import (
	"errors"
	"strings"
	"testing"

	"touchgocore/config"

	"github.com/jackc/pgx/v5/pgconn"
)

const qaSecretPass = "s3cr3t pa55" // 含空格，验证引号包裹与脱敏

func TestPoolKey_StabilityUniquenessAndPrefix(t *testing.T) {
	base := &config.PostgreDBConfig{Host: "h", Port: 5432, Username: "u", Password: "p", DBName: "db"}
	c1 := &Client{cfg: base}
	c2 := &Client{cfg: &config.PostgreDBConfig{Host: "h", Port: 5432, Username: "u", Password: "p", DBName: "db"}}
	c3 := &Client{cfg: &config.PostgreDBConfig{Host: "h", Port: 5432, Username: "u", Password: "p2", DBName: "db"}}
	c4 := &Client{cfg: &config.PostgreDBConfig{Host: "h", Port: 5432, Username: "u", Password: "p", DBName: "db2"}}
	c5 := &Client{cfg: &config.PostgreDBConfig{Host: "h", Port: 6543, Username: "u", Password: "p", DBName: "db"}}

	if c1.poolKey() != c2.poolKey() {
		t.Fatalf("同配置 poolKey 应一致: %s vs %s", c1.poolKey(), c2.poolKey())
	}
	if !strings.HasPrefix(c1.poolKey(), "pg-") {
		t.Fatalf("poolKey 应带 pg- 前缀与 mysql 键隔离: %s", c1.poolKey())
	}
	if c1.poolKey() == c3.poolKey() {
		t.Fatal("不同密码 poolKey 应不同")
	}
	if c1.poolKey() == c4.poolKey() {
		t.Fatal("不同 DBName poolKey 应不同")
	}
	if c1.poolKey() == c5.poolKey() {
		t.Fatal("不同 Port poolKey 应不同")
	}
}

func TestBuildDSN_DefaultsAndOverrides(t *testing.T) {
	cfg := &config.PostgreDBConfig{Host: "127.0.0.1", Username: "app", Password: "pw", DBName: "game"}
	o := defaultOptions()
	dsn := buildDSNWithOptions(cfg, o)
	// 端口默认 5432，sslmode/timezone/schema 默认值
	for _, want := range []string{"host=127.0.0.1", "port=5432", "user=app", "dbname=game", "sslmode=disable", "TimeZone=UTC", "search_path=public"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN 缺 %q: %s", want, dsn)
		}
	}
	if !strings.Contains(dsn, "password='pw'") {
		t.Fatalf("密码应单引号包裹: %s", dsn)
	}

	o2 := defaultOptions()
	o2.sslMode = "require"
	o2.timeZone = "Asia/Shanghai"
	o2.schema = "app"
	o2.extraDSNParams = map[string]string{"connect_timeout": "10"}
	cfg2 := &config.PostgreDBConfig{Host: "h", Port: 6000, Username: "u", Password: "p", DBName: "d"}
	dsn2 := buildDSNWithOptions(cfg2, o2)
	for _, want := range []string{"port=6000", "sslmode=require", "TimeZone=Asia/Shanghai", "search_path=app", "connect_timeout=10"} {
		if !strings.Contains(dsn2, want) {
			t.Fatalf("DSN 缺 %q: %s", want, dsn2)
		}
	}
}

func TestRedactDSN_StripsQuotedPassword(t *testing.T) {
	dsn := buildDSNWithOptions(&config.PostgreDBConfig{
		Host: "127.0.0.1", Port: 5432, Username: "app", Password: qaSecretPass, DBName: "game",
	}, defaultOptions())
	got := redactDSN(dsn)
	if strings.Contains(got, qaSecretPass) {
		t.Fatalf("脱敏后仍含密码: %q", got)
	}
	if !strings.Contains(got, "password=***") {
		t.Fatalf("脱敏应替换为 ***: %q", got)
	}
	for _, want := range []string{"host=127.0.0.1", "dbname=game", "user=app"} {
		if !strings.Contains(got, want) {
			t.Fatalf("脱敏应保留非敏感标识 %q: %q", want, got)
		}
	}
	// 无 password 段 → 整串丢弃
	if r := redactDSN("host=h dbname=d"); r != "" {
		t.Fatalf("无密码段应返回空串, 得 %q", r)
	}
	if r := redactDSN(""); r != "" {
		t.Fatalf("空串应返回空串, 得 %q", r)
	}
}

func TestClassify_PgStateCodes(t *testing.T) {
	cases := map[string]Kind{
		sqlstateUniqueViolation:     KindDuplicate,
		sqlstateUndefinedTable:      KindSyntax,
		sqlstateUndefinedColumn:     KindInvalidColumn,
		sqlstateInvalidPassword:     KindAuth,
		sqlstateSerializationFail:   KindDeadlock,
		sqlstateDeadlockDetected:    KindDeadlock,
		sqlstateLockNotAvailable:    KindLockWait,
		sqlstateQueryCanceled:       KindCanceled,
		sqlstateTooManyConnections:  KindPoolExhausted,
		sqlstateForeignKeyViolation: KindConstraint,
		sqlstateNotNullViolation:    KindConstraint,
		"08006":                     KindConnFailed,
	}
	for code, want := range cases {
		err := &pgconn.PgError{Code: code}
		if got := classify(err); got != want {
			t.Errorf("SQLSTATE %s: 期望 %v, 得 %v", code, want, got)
		}
	}
}

func TestIsTableNotExist(t *testing.T) {
	if !IsTableNotExist(&pgconn.PgError{Code: sqlstateUndefinedTable}) {
		t.Fatal("42P01 应判定为表不存在")
	}
	if IsTableNotExist(&pgconn.PgError{Code: sqlstateUniqueViolation}) {
		t.Fatal("唯一约束冲突不应判定为表不存在")
	}
	if IsTableNotExist(nil) {
		t.Fatal("nil 不应判定为表不存在")
	}
}

func TestIsAssertions(t *testing.T) {
	dup := newError("Create", &pgconn.PgError{Code: sqlstateUniqueViolation}, KindDuplicate, "", nil, 0)
	if !IsDuplicate(dup) {
		t.Fatal("IsDuplicate 应为真")
	}
	if IsNotFound(dup) {
		t.Fatal("IsNotFound 应为假")
	}
	if !errors.Is(dup, &Error{Kind: KindDuplicate}) {
		t.Fatal("errors.Is 应按 Kind 匹配")
	}
}
