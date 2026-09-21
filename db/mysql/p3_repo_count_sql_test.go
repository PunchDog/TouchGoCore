package mysql

// ============================================================================
// 聚合查询离线回归（S50 补充覆盖）：Page/Count/Exists 三处修复此前无用例，
// 因为仓库既无 MySQL 实例也无测试驱动。gorm 的 DryRun 只需要一个 ConnPool
// 桩就能把 SQL 生成阶段跑完，于是「语句上有没有绑 Model」「Count 被写成
// COUNT(`1`)」「nil Query 在 q.groupString() 上解引用」这三种缺陷都能在
// 不连库的前提下钉死。
//
// 判据取自 SQL 文本而非 RowsAffected：DryRun 从不执行语句，计数恒为 0，
// 「查到了几条」在这里没有任何信息量。
// ============================================================================

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// repoProbeRow 只提供 schema/表名，字段不参与本用例断言。
type repoProbeRow struct {
	ID   uint
	Name string
}

// noDialPool 满足 gorm.ConnPool 但一次都不许被调用：真去连库就说明用例越过了
// 「离线」这条线，此时 panic 比静默通过更有价值。
type noDialPool struct{ calls int }

func (p *noDialPool) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	p.calls++
	return nil, errors.New("noDialPool: 不应真连库")
}

func (p *noDialPool) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	p.calls++
	return nil, errors.New("noDialPool: 不应真连库")
}

func (p *noDialPool) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	p.calls++
	return nil, errors.New("noDialPool: 不应真连库")
}

func (p *noDialPool) QueryRowContext(context.Context, string, ...any) *sql.Row {
	p.calls++
	panic("noDialPool: 不应真连库")
}

// newDryRunRepo 造一个只跑 SQL 生成、绝不触网的 Repository。
// 表名固定为 repo_probe_rows（gorm 默认蛇形命名）。
func newDryRunRepo(t *testing.T) (*Repository[repoProbeRow], *[]string) {
	t.Helper()

	pool := &noDialPool{}
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      pool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		Logger:                 logger.Discard,
		NowFunc:                func() time.Time { return time.Unix(0, 0) },
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("构造 DryRun 会话失败: %v", err)
	}

	var sqls []string
	if err := db.Callback().Query().After("gorm:query").Register("touchgocore-test:capture", func(tx *gorm.DB) {
		sqls = append(sqls, tx.Statement.SQL.String())
	}); err != nil {
		t.Fatalf("注册采集回调失败: %v", err)
	}

	// r.db 非空时 ensureEngine 直接放行，无需 Client 与 engine。
	r := &Repository[repoProbeRow]{
		ctx:   context.Background(),
		db:    db,
		hooks: newRepoHooks[repoProbeRow](),
	}
	t.Cleanup(func() {
		if pool.calls != 0 {
			t.Errorf("DryRun 期间发生了 %d 次真实连接调用", pool.calls)
		}
	})
	return r, &sqls
}

func count(t *testing.T, sqls *[]string, i int) string {
	t.Helper()
	if len(*sqls) <= i {
		t.Fatalf("第 %d 条语句没被采集到，实际只有 %d 条: %v", i, len(*sqls), *sqls)
	}
	return (*sqls)[i]
}

// TestCountBindsModel：Count 必须绑上 Model，否则 gorm 把 Dest(&n) 当作 Model 解析，
// 直接报 "Table not set"，整条统计在真实库上根本发不出语句。
func TestRegression_CountBindsModel(t *testing.T) {
	r, sqls := newDryRunRepo(t)

	n, err := r.Count(context.Background(), NewQuery().Eq("id", 1))
	if err != nil {
		t.Fatalf("✘ Count 报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("✘ DryRun 不该有结果集，n=%d", n)
	}
	sql := count(t, sqls, 0)
	if !strings.HasPrefix(sql, "SELECT count(*) FROM `repo_probe_rows`") {
		t.Fatalf("✘ 计数语句缺表名或聚合写法不对: %q", sql)
	}
	if !strings.Contains(sql, "WHERE id = ?") {
		t.Fatalf("✘ 条件没下推到计数语句: %q", sql)
	}
}

// TestExistsUsesCountStar：原实现 Select("1") 会被 gorm 当列名加反引号下发成
// COUNT(`1`)，MySQL 侧报 "Unknown column '1'"。
func TestRegression_ExistsUsesCountStar(t *testing.T) {
	r, sqls := newDryRunRepo(t)

	ok, err := r.Exists(context.Background(), NewQuery().Eq("id", 7))
	if err != nil {
		t.Fatalf("✘ Exists 报错: %v", err)
	}
	if ok {
		t.Fatalf("✘ DryRun 无结果集，Exists 不该判为存在")
	}
	sql := count(t, sqls, 0)
	if strings.Contains(sql, "`1`") {
		t.Fatalf("✘ 把常量 1 当列名加引号下发了: %q", sql)
	}
	if !strings.Contains(sql, "count(*)") {
		t.Fatalf("✘ 计数未退化成 count(*): %q", sql)
	}
}

// TestPageAcceptsNilQuery：nil Query 必须在入口归一化。变更前 Page 在
// q.groupString() 上解引用空指针，且兜底语句被放在计数分支之后（够不着）。
func TestRegression_PageAcceptsNilQuery(t *testing.T) {
	r, sqls := newDryRunRepo(t)

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("✘ Page(nil Query) panic: %v", rec)
		}
	}()
	items, total, err := r.Page(context.Background(), nil, 2, 15)
	if err != nil {
		t.Fatalf("✘ Page 报错: %v", err)
	}
	if len(items) != 0 || total != 0 {
		t.Fatalf("✘ DryRun 不该有数据: items=%d total=%d", len(items), total)
	}
	if c := count(t, sqls, 0); !strings.Contains(c, "count(*)") {
		t.Fatalf("✘ Page 的计数语句形态不对: %q", c)
	}
	list := count(t, sqls, 1)
	if !strings.Contains(list, "LIMIT") || !strings.Contains(list, "OFFSET") {
		t.Fatalf("✘ 分页未落到列表语句（应为 page=2/size=15 → LIMIT 15 OFFSET 15）: %q", list)
	}
}

// TestCountAndExistsAcceptNilQuery：把「nil 视为空条件」钉成契约，
// 覆盖 Count/Exists 两条入口（变更前成功路径侥幸不炸，错误分支仍会解引用 q）。
func TestRegression_CountAndExistsAcceptNilQuery(t *testing.T) {
	r, sqls := newDryRunRepo(t)

	if _, err := r.Count(context.Background(), nil); err != nil {
		t.Fatalf("✘ Count(nil) 报错: %v", err)
	}
	if _, err := r.Exists(context.Background(), nil); err != nil {
		t.Fatalf("✘ Exists(nil) 报错: %v", err)
	}
	if len(*sqls) != 2 {
		t.Fatalf("✘ 应有两条语句被采集，实际 %d 条: %v", len(*sqls), *sqls)
	}
	for i, sql := range *sqls {
		if !strings.HasPrefix(sql, "SELECT count(*) FROM `repo_probe_rows`") {
			t.Fatalf("✘ 第 %d 条应为无条件全表计数: %q", i, sql)
		}
	}
}
