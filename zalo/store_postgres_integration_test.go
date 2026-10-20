package zalo

// 本文件是 Postgres 会话库的真库闸：只在 TEST_ZALO_POSTGRES_DSN 非空时运行，
// 且必须指向一次性测试库（表名带 UnixNano，跑完 DROP）。
//
// 运行方式：
//
//	TEST_ZALO_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/zalo_test?sslmode=disable' \
//	  go test -count=1 -run TestZAPGStore ./zalo
//
// 没有 env 时直接 Skip：CI 默认不连库，本测试也不负责起数据库。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestZAPGStorePostgres 建表 → Upsert 两条 → Get → List → Delete → Get 查无 → Close，
// 并顺带验证 JSONB 往返的 credentials 仍能被凭证层解析。
func TestZAPGStorePostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_ZALO_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_ZALO_POSTGRES_DSN 未设置，跳过 Postgres 集成测试（须指向一次性测试库）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	table := fmt.Sprintf("zalo_sessions_it_%d", time.Now().UnixNano())
	if !validZATableName(table) {
		t.Fatalf("测试表名不满足白名单: %q", table)
	}

	store, err := newZAPGStore(ctx, dsn, table)
	if err != nil {
		t.Fatalf("构造 store 失败: %v", err)
	}

	// 清理用独立连接：用例中途失败或 store 已 Close 时仍能删表。
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Errorf("清理时打开库失败: %s", maskDSN(err.Error(), dsn))
			return
		}
		defer func() { _ = db.Close() }()
		if _, err := db.ExecContext(dropCtx, fmt.Sprintf("DROP TABLE IF EXISTS %s", zaTableIdent(table))); err != nil {
			t.Errorf("清理时删表失败: %s", maskDSN(err.Error(), dsn))
		}
	})

	// 建表后空表 List 应为空而不是 nil 错误。
	metas, err := store.List(ctx)
	if err != nil {
		t.Fatalf("空表 List 失败: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("空表 List 应返回 0 行，实得 %d", len(metas))
	}

	recA := zaSessionRecord{
		AccountID:   "it_a",
		UID:         "uid-a",
		Credentials: []byte(`{"imei":"imei-a","cookie":[{"name":"a","value":"1"}],"userAgent":"ua-a"}`),
	}
	recB := zaSessionRecord{
		AccountID:   "it_b",
		UID:         "uid-b",
		Credentials: []byte(`{"imei":"imei-b","cookie":[{"name":"b","value":"2"}],"userAgent":"ua-b"}`),
	}
	for _, rec := range []zaSessionRecord{recA, recB} {
		if err := store.Upsert(ctx, rec); err != nil {
			t.Fatalf("Upsert %s 失败: %v", rec.AccountID, err)
		}
	}

	// 覆盖写不能改 created_at：它是首次入库时间，排障时用来区分「老号重新登录」
	// 与「新号首次登录」。
	var createdA time.Time
	if err := store.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT created_at FROM %s WHERE account_id = $1`, zaTableIdent(table)), "it_a",
	).Scan(&createdA); err != nil {
		t.Fatalf("读取 created_at 失败: %v", err)
	}

	// 覆盖写：同账号第二次 Upsert 必须替换 uid 与 credentials，而不是插入新行。
	recA2 := zaSessionRecord{
		AccountID:   "it_a",
		UID:         "uid-a2",
		Credentials: []byte(`{"imei":"imei-a2","cookie":[{"name":"a","value":"1"}],"userAgent":"ua-a2"}`),
	}
	if err := store.Upsert(ctx, recA2); err != nil {
		t.Fatalf("覆盖写 Upsert 失败: %v", err)
	}
	var createdAAfter time.Time
	if err := store.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT created_at FROM %s WHERE account_id = $1`, zaTableIdent(table)), "it_a",
	).Scan(&createdAAfter); err != nil {
		t.Fatalf("覆盖写后读取 created_at 失败: %v", err)
	}
	if !createdAAfter.Equal(createdA) {
		t.Fatalf("覆盖写改了 created_at：before=%v after=%v", createdA, createdAAfter)
	}

	got, err := store.Get(ctx, "it_a")
	if err != nil {
		t.Fatalf("Get it_a 失败: %v", err)
	}
	if got == nil {
		t.Fatal("Get it_a 返回 nil，期望覆盖后的行")
	}
	if got.AccountID != "it_a" || got.UID != "uid-a2" {
		t.Fatalf("Get 元数据不符：account_id=%q uid=%q", got.AccountID, got.UID)
	}
	if got.UpdatedAt.IsZero() || got.UpdatedAt.Year() < 2000 {
		t.Fatal("Get 的 updated_at 未从库读出")
	}
	// JSONB 会重排键/去空白，不能做字节比较；用凭证层解析证明内容完整往返。
	creds, err := parseZACredentials(got.Credentials, "it_a", "ua-default", "vi")
	if err != nil {
		t.Fatalf("库里的 credentials 无法被凭证层解析: %v", err)
	}
	if creds.IMEI != "imei-a2" || creds.UserAgent != "ua-a2" {
		t.Fatal("JSONB 往返后 credentials 内容不符")
	}

	// 查无是 (nil, nil)。
	missing, err := store.Get(ctx, "it_missing")
	if err != nil {
		t.Fatalf("查无的 Get 不应返回错误: %v", err)
	}
	if missing != nil {
		t.Fatal("查无的 Get 应返回 nil 记录")
	}

	// List 只读元数据（zaSessionMeta 没有 credentials 字段，结构上保证不读凭据）。
	metas, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("List 行数=%d，期望 2", len(metas))
	}
	if metas[0].AccountID != "it_a" || metas[1].AccountID != "it_b" {
		t.Fatal("List 应按 account_id 排序")
	}
	if metas[0].UID != "uid-a2" || metas[1].UID != "uid-b" {
		t.Fatal("List 的 uid 元数据不符")
	}
	for _, m := range metas {
		if m.UpdatedAt.IsZero() {
			t.Fatalf("List 的 %s 行 updated_at 为空", m.AccountID)
		}
	}

	// Delete 幂等。
	if err := store.Delete(ctx, "it_a"); err != nil {
		t.Fatalf("Delete it_a 失败: %v", err)
	}
	if err := store.Delete(ctx, "it_a"); err != nil {
		t.Fatalf("重复 Delete 应幂等: %v", err)
	}
	gone, err := store.Get(ctx, "it_a")
	if err != nil {
		t.Fatalf("删除后 Get 不应返回错误: %v", err)
	}
	if gone != nil {
		t.Fatal("删除后 Get 应返回 (nil, nil)")
	}
	metas, err = store.List(ctx)
	if err != nil {
		t.Fatalf("删除后 List 失败: %v", err)
	}
	if len(metas) != 1 || metas[0].AccountID != "it_b" {
		t.Fatalf("删除后 List 行数=%d，期望只剩 it_b", len(metas))
	}

	// 非法 JSON 必须被 ::jsonb 拒掉，且错误文案不能回显凭据片段——这条是
	// 「凭据不顺着错误进日志」的唯一真库证据。
	bad := zaSessionRecord{
		AccountID:   "it_bad",
		Credentials: []byte(`{"imei":"MUST_NOT_LEAK"`),
	}
	badErr := store.Upsert(ctx, bad)
	if badErr == nil {
		t.Fatal("非法 JSON 应被库侧拒绝")
	}
	if strings.Contains(badErr.Error(), "MUST_NOT_LEAK") {
		t.Fatalf("错误文案泄漏了凭据片段: %v", badErr)
	}

	// 已取消的 ctx 必须把错误交上来，不能当成查无。
	canceledCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if _, err := store.Get(canceledCtx, "it_b"); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
	// 关库后的迟到调用给「已关闭」哨兵，而不是可重试的库故障。
	if _, err := store.Get(ctx, "it_b"); !errors.Is(err, errZAGStoreClosed) {
		t.Fatalf("Close 后 Get 应返回 errZAGStoreClosed，实得 %v", err)
	}
}
