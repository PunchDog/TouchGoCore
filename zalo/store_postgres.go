package zalo

// 本文件是 Zalo 会话库的 Postgres 实现（zaStore 的唯一生产实现）。
//
// 方言口径与 whatsapp/store_postgres.go 一致：database/sql + pgx v5 的 stdlib 驱动
// （import 后注册驱动名 "pgx"）。pgx/v5 已是本仓直接依赖，不引 lib/pq，少一种
// 连接语义分叉。
//
// 凭据就是账号本体：credentials 列存的是 zcago.Credentials 的 JSON（含 cookie/IMEI），
// 等价于把浏览器里那只 Zalo 会话交出去。本包任何日志、error、zaSessionMeta 都不携带
// 这些材料；部署侧应收紧库权限或启用落盘加密。
//
// DSN 纪律：连接串只在构造时出现，所有构造期错误经 maskDSN 脱敏；运行期查询错误经
// zaSafeWrap 包装（Error() 只给固定文案，Unwrap 保留原始错误），避免 Postgres 的
// 报错细节（可能含 JSON 片段）顺着 error 进日志。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 database/sql 驱动名 "pgx"
)

const (
	// zaPingTimeout 是构造期的连通性探测预算：DSN 指不通要在这里失败，
	// 而不是拖住整机启动（sql.Open 本身不拨号）。
	zaPingTimeout = 5 * time.Second
	// zaCreateTableTimeout 是建表预算：库可达但 DDL 被锁/权限拒时要有限等待。
	zaCreateTableTimeout = 10 * time.Second
)

// zaPGStore 是 Postgres 上的 zalo_sessions 表访问器。
//
// table 只在构造时校验一次，之后用校验后的字符串拼 SQL；任何方法都不再从外部
// 接收表名，避免「某条路径漏校验」的注入面。
type zaPGStore struct {
	db     *sql.DB
	table  string
	closed atomic.Bool
}

// 编译期钉住：真实现满足管理器依赖的 seam。
var _ zaStore = (*zaPGStore)(nil)

// newZAPGStore 打开会话库并确保表存在。
//
// table 传空时用生产表名 zaSessionsTable；集成测试传一次性表名，跑完自己 DROP。
// ctx 为 nil 时按 context.Background() 处理（与 whatsapp.NewWAStore 同口径）。
func newZAPGStore(ctx context.Context, dsn, table string) (*zaPGStore, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("%w: zalo 会话库 DSN 为空", ErrZaloNotStarted)
	}
	if table == "" {
		table = zaSessionsTable
	}
	if !validZATableName(table) {
		// 表名是我们自己的常量或测试生成的名字，走到这里说明调用方拼错了；
		// 不回显表名原文（它来自参数，可能不可信）。
		return nil, errors.New("zalo 会话表名非法（只允许小写字母开头的 [a-z0-9_]，长度 <= 63）")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 zalo 会话库失败: %s", maskDSN(err.Error(), dsn))
	}
	// 连接池口径与 whatsapp 的 Postgres 库对齐：会话库是低频读写，
	// 4 条上限足以覆盖「启动列账号 + 按需读凭证」，也不会在库侧占满连接。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	// 云 PG / PgBouncer 常在空闲一段时间后单方面断连；给池一个短于服务端
	// idle 超时的回收上限，避免拿到已被对端关掉的连接。
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, zaPingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 zalo 会话库失败: %s", maskDSN(err.Error(), dsn))
	}

	ddlCtx, cancelDDL := context.WithTimeout(ctx, zaCreateTableTimeout)
	defer cancelDDL()
	if _, err := db.ExecContext(ddlCtx, fmt.Sprintf(zaCreateTableSQL, zaTableIdent(table))); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("创建 zalo 会话表失败: %s", maskDSN(err.Error(), dsn))
	}
	// CREATE TABLE IF NOT EXISTS 对「同名表已存在但列缺失/类型不同」是静默通过；
	// 补一次零行查询把结构错在启动期打出来，而不是等首次读写才报通用失败。
	probeCtx, cancelProbe := context.WithTimeout(ctx, zaPingTimeout)
	defer cancelProbe()
	rows, err := db.QueryContext(probeCtx, fmt.Sprintf(
		`SELECT account_id, uid, credentials, created_at, updated_at FROM %s LIMIT 0`,
		zaTableIdent(table),
	))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("zalo 会话表结构与预期不符: %s", maskDSN(err.Error(), dsn))
	}
	_ = rows.Close()
	return &zaPGStore{db: db, table: table}, nil
}

// zaCreateTableSQL 的表结构是计划里冻结的一段：account_id 为主键，
// credentials 用 JSONB（服务端可校验、体积小），updated_at 由写入路径刷新。
// created_at 只为排障保留，本包不读。
const zaCreateTableSQL = `CREATE TABLE IF NOT EXISTS %s (
  account_id  TEXT PRIMARY KEY,
  uid         TEXT NOT NULL DEFAULT '',
  credentials JSONB NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Upsert 写入或覆盖一个账号的会话。$3 传 JSON 字符串并由 SQL 侧 ::jsonb 校验，
// 非法 JSON 在库侧就被拒，不会以「半条会话」的形态留在表里。
func (s *zaPGStore) Upsert(ctx context.Context, rec zaSessionRecord) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := validateZAAccountID(rec.AccountID); err != nil {
		return err
	}
	if len(rec.Credentials) == 0 {
		return fmt.Errorf("%w: 凭证 JSON 为空", ErrZaloBadCredentials)
	}
	q := fmt.Sprintf(`INSERT INTO %s (account_id, uid, credentials, updated_at)
VALUES ($1, $2, $3::jsonb, now())
ON CONFLICT (account_id) DO UPDATE
SET uid = EXCLUDED.uid, credentials = EXCLUDED.credentials, updated_at = now()`, zaTableIdent(s.table))
	if _, err := s.db.ExecContext(ctx, q, rec.AccountID, rec.UID, string(rec.Credentials)); err != nil {
		// 原始错误保留在 Unwrap 里供排障，Error() 只给固定文案：
		// Postgres 的 JSON 解析错误可能回显值片段，而这里存的就是凭据。
		return zaSafeWrap("zalo 会话写入失败", err)
	}
	return nil
}

// Get 读取一个账号的完整会话；查无返回 (nil, nil)。
// 「查无」不是错误，是管理器区分「需要重新登录」与「库故障」的依据。
func (s *zaPGStore) Get(ctx context.Context, accountID string) (*zaSessionRecord, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := validateZAAccountID(accountID); err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT account_id, uid, credentials, updated_at FROM %s WHERE account_id = $1`, zaTableIdent(s.table))
	var (
		rec   zaSessionRecord
		creds []byte
	)
	err := s.db.QueryRowContext(ctx, q, accountID).Scan(&rec.AccountID, &rec.UID, &creds, &rec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, zaSafeWrap("zalo 会话读取失败", err)
	}
	rec.Credentials = creds
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	return &rec, nil
}

// List 只返回元数据列。credentials 不在这条查询里：列账号是高频操作，
// 没必要把每个账号的 cookie/IMEI 都搬进内存。
func (s *zaPGStore) List(ctx context.Context) ([]zaSessionMeta, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT account_id, uid, updated_at FROM %s ORDER BY account_id`, zaTableIdent(s.table))
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, zaSafeWrap("zalo 会话列表读取失败", err)
	}
	defer func() { _ = rows.Close() }()

	metas := make([]zaSessionMeta, 0, 8)
	for rows.Next() {
		var m zaSessionMeta
		if err := rows.Scan(&m.AccountID, &m.UID, &m.UpdatedAt); err != nil {
			return nil, zaSafeWrap("zalo 会话列表读取失败", err)
		}
		m.UpdatedAt = m.UpdatedAt.UTC()
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return nil, zaSafeWrap("zalo 会话列表读取失败", err)
	}
	return metas, nil
}

// Delete 删除一个账号的会话行；行不存在也返回 nil（幂等）。
// 删除只清库，不停该账号在内存里的连接——那是管理器的职责。
func (s *zaPGStore) Delete(ctx context.Context, accountID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := validateZAAccountID(accountID); err != nil {
		return err
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE account_id = $1`, zaTableIdent(s.table))
	if _, err := s.db.ExecContext(ctx, q, accountID); err != nil {
		return zaSafeWrap("zalo 会话删除失败", err)
	}
	return nil
}

// Close 释放连接池；nil 接收者与重复调用都安全（sql.DB.Close 自身幂等）。
// 关闭后 ready 一律返回 errZAGStoreClosed：manager 关停期的迟到调用应被明确拒绝，
// 而不是被当成可重试的库故障。
func (s *zaPGStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closed.Store(true)
	return s.db.Close()
}

// ready 拦住「零值 store」与「已关闭 store」：零值直接构造得到未打开哨兵，
// 关闭后得到已关闭哨兵；两者都不置 nil 字段，避免与并发调用竞态。
func (s *zaPGStore) ready() error {
	if s == nil || s.db == nil {
		return errZAGStoreUnopened
	}
	if s.closed.Load() {
		return errZAGStoreClosed
	}
	return nil
}
