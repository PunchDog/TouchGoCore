package zalo

// 本文件是存储 seam 的单元闸：账号 id 规则、DSN 脱敏、表名白名单、零值 store 的
// 防御行为。真库行为（建表/Upsert/Get/List/Delete）在 store_postgres_integration_test.go，
// 需要 TEST_ZALO_POSTGRES_DSN 才跑。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// TestZAAccountIDValidation 账号 id 是 DB 主键与日志标识，规则必须逐字收口：
// 字母/数字开头，只含字母/数字/点/下划线/连字符，长度 1-64；".", "..", 空白、
// 路径分隔符、非 ASCII 一律拒绝。
func TestZAAccountIDValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{name: "single_letter", id: "a", ok: true},
		{name: "single_digit", id: "7", ok: true},
		{name: "default", id: "default", ok: true},
		{name: "mixed_punctuation", id: "Acct-1.2_x", ok: true},
		{name: "max_len", id: strings.Repeat("a", 64), ok: true},
		{name: "empty", id: "", ok: false},
		{name: "space_only", id: " ", ok: false},
		{name: "dot", id: ".", ok: false},
		{name: "dotdot", id: "..", ok: false},
		{name: "leading_dot", id: ".a", ok: false},
		{name: "leading_dash", id: "-a", ok: false},
		{name: "leading_underscore", id: "_a", ok: false},
		{name: "inner_space", id: "a b", ok: false},
		{name: "trailing_space", id: "a ", ok: false},
		{name: "slash", id: "a/b", ok: false},
		{name: "backslash", id: `a\b`, ok: false},
		{name: "colon", id: "a:b", ok: false},
		{name: "newline", id: "a\nb", ok: false},
		{name: "too_long", id: strings.Repeat("a", 65), ok: false},
		{name: "non_ascii", id: "账号", ok: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateZAAccountID(tc.id)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("应合法，实得错误: %v", err)
			case !tc.ok && err == nil:
				t.Fatal("应拒绝，实得 nil")
			case !tc.ok && !errors.Is(err, ErrZaloBadAccountID):
				t.Fatalf("错误必须包装 ErrZaloBadAccountID，实得 %v", err)
			}
		})
	}
}

// TestZAAccountIDErrorHidesInput 错误文案只描述规则：id 可能来自外部请求，
// 把原文拼进 error 会让日志被不可信内容污染。
func TestZAAccountIDErrorHidesInput(t *testing.T) {
	t.Parallel()

	const bad = "Evil/../Account"
	err := validateZAAccountID(bad)
	if err == nil {
		t.Fatal("应拒绝")
	}
	if !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("应包装 ErrZaloBadAccountID，实得 %v", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatal("错误文案回显了输入全文")
	}
}

// TestZAMaskDSN 三层脱敏：整串替换、keyword 的 password=、URL 的 :pass@。
// DSN 本身绝不能顺着 error 进日志。
func TestZAMaskDSN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		dsn     string
		msg     string
		wantSub []string
	}{
		{
			name:    "whole_dsn_echoed",
			dsn:     "postgres://app:pw-alpha@db:5432/zalo?sslmode=disable",
			msg:     "打开会话库失败: 连接 postgres://app:pw-alpha@db:5432/zalo?sslmode=disable 被拒",
			wantSub: []string{"***"},
		},
		{
			name:    "keyword_password_layer",
			dsn:     "",
			msg:     "open failed: host=db password=pw-beta user=app",
			wantSub: []string{"password=***"},
		},
		{
			name:    "keyword_single_quoted_password",
			dsn:     "",
			msg:     "open failed: host=db password='pw beta quoted' user=app",
			wantSub: []string{"password=***"},
		},
		{
			name:    "keyword_double_quoted_password",
			dsn:     "",
			msg:     `open failed: password="pw beta double" user=app`,
			wantSub: []string{"password=***"},
		},
		{
			name:    "url_password_layer",
			dsn:     "",
			msg:     "dial tcp: postgres://app:pw-gamma@db:5432/zalo: connection refused",
			wantSub: []string{"postgres://app:***@db:5432/zalo"},
		},
		{
			name:    "clean_message_untouched",
			dsn:     "postgres://app:pw-delta@db:5432/zalo",
			msg:     "connection refused",
			wantSub: []string{"connection refused"},
		},
		{
			name:    "empty_dsn_and_message",
			dsn:     "",
			msg:     "",
			wantSub: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := maskDSN(tc.msg, tc.dsn)
			for _, sub := range tc.wantSub {
				if !strings.Contains(got, sub) {
					t.Fatalf("脱敏结果缺少 %q，实得 %q", sub, got)
				}
			}
			// 断言口令本体不残留（测试用假口令，只判断是否被抹掉）。
			for _, secret := range []string{"pw-alpha", "pw-beta", "pw-gamma", "pw-delta", "pw beta quoted", "pw beta double"} {
				if strings.Contains(got, secret) {
					t.Fatalf("口令未被脱敏（%s 残留）", secret)
				}
			}
		})
	}
}

// TestZAPGStoreTableNameWhitelist 表名会被拼进 SQL，只允许小写字母开头的
// [a-z0-9_]，并限制在 PostgreSQL 标识符上限 63 字节内。
func TestZAPGStoreTableNameWhitelist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "production", in: zaSessionsTable, ok: true},
		{name: "integration", in: "zalo_sessions_it_1712345678901234567", ok: true},
		{name: "single", in: "a", ok: true},
		{name: "max_len", in: strings.Repeat("a", 63), ok: true},
		{name: "empty", in: "", ok: false},
		{name: "too_long", in: strings.Repeat("a", 64), ok: false},
		{name: "uppercase", in: "ZaloSessions", ok: false},
		{name: "leading_digit", in: "1abc", ok: false},
		{name: "leading_underscore", in: "_abc", ok: false},
		{name: "dash", in: "zalo-sessions", ok: false},
		{name: "space", in: "zalo sessions", ok: false},
		{name: "dot", in: "zalo.sessions", ok: false},
		{name: "semicolon", in: "zalo;drop", ok: false},
		{name: "quote", in: `zalo"`, ok: false},
		{name: "schema_qualified", in: "public.zalo_sessions", ok: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validZATableName(tc.in); got != tc.ok {
				t.Fatalf("validZATableName=%v，期望 %v", got, tc.ok)
			}
		})
	}
}

// TestZAPGStoreConstructorValidation 构造期参数闸：空 DSN 报 ErrZaloNotStarted，
// 非法表名在 sql.Open 之前就被拒（用不可达 DSN 证明不会真的拨号）。
func TestZAPGStoreConstructorValidation(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{"", "   "} {
		if _, err := newZAPGStore(t.Context(), dsn, zaSessionsTable); !errors.Is(err, ErrZaloNotStarted) {
			t.Fatalf("空 DSN 应包装 ErrZaloNotStarted，实得 %v", err)
		}
	}
	// 127.0.0.1:1 不可达：若实现先拨号再校验表名，这里会卡在 Ping 超时上。
	if _, err := newZAPGStore(t.Context(), "postgres://user:pw@127.0.0.1:1/none?sslmode=disable", "Bad-Name"); err == nil {
		t.Fatal("非法表名应返回错误")
	}
}

// TestZAPGStoreZeroValueGuards 零值 store（绕过构造函数）应给错误而不是 panic；
// Close 对 nil 接收者与零值都幂等。
func TestZAPGStoreZeroValueGuards(t *testing.T) {
	t.Parallel()

	var nilStore *zaPGStore
	if err := nilStore.Close(); err != nil {
		t.Fatalf("nil 接收者 Close 应为 nil，实得 %v", err)
	}
	zero := &zaPGStore{}
	if err := zero.Close(); err != nil {
		t.Fatalf("零值 Close 应为 nil，实得 %v", err)
	}
	if err := zero.Upsert(t.Context(), zaSessionRecord{AccountID: "ok", Credentials: []byte(`{}`)}); err == nil {
		t.Fatal("零值 store 的 Upsert 应返回错误")
	}
	if _, err := zero.Get(t.Context(), "ok"); err == nil {
		t.Fatal("零值 store 的 Get 应返回错误")
	}
	if _, err := zero.List(t.Context()); err == nil {
		t.Fatal("零值 store 的 List 应返回错误")
	}
	if err := zero.Delete(t.Context(), "ok"); err == nil {
		t.Fatal("零值 store 的 Delete 应返回错误")
	}
}

// TestZAPGStoreClosedAfterClose 关库后的迟到调用必须给「已关闭」哨兵：
// manager 关停期若还有 ensureConn 在飞，它应该直接拒绝，而不是把 sql.ErrDBClosed
// 当成可重试的库故障反复重试。
func TestZAPGStoreClosedAfterClose(t *testing.T) {
	t.Parallel()

	// sql.Open 只解析 DSN，不拨号；这里不需要真库。
	db, err := sql.Open("pgx", "postgres://user:pw@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open 失败: %v", err)
	}
	store := &zaPGStore{db: db}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := store.ready(); !errors.Is(err, errZAGStoreClosed) {
		t.Fatalf("Close 后 ready 应返回 errZAGStoreClosed，实得 %v", err)
	}
	if _, err := store.Get(context.Background(), "ok"); !errors.Is(err, errZAGStoreClosed) {
		t.Fatalf("Close 后 Get 应返回 errZAGStoreClosed，实得 %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
}
