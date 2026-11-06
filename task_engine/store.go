package taskengine

import (
	"context"
	"time"

	"gorm.io/gorm"

	"touchgocore/db"
)

// Store 引擎需要的非事务读写与事务入口。
type Store interface {
	// FindDefByID 按主键取任务定义；不存在返回 (nil, nil)。
	FindDefByID(ctx context.Context, id int64) (*TaskDef, error)
	// ListEnabledDefs 列出全部启用任务，按 id 升序。
	ListEnabledDefs(ctx context.Context) ([]TaskDef, error)
	// HasDoneProgress 该会员该任务本周期是否已有 completed/claimed 行（前置闸用）。
	HasDoneProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (bool, error)
	// ListProgressByMember 某会员全部进度行（GetTask 列表聚合用）。
	ListProgressByMember(ctx context.Context, memberID int64) ([]TaskProgress, error)
	// LoadProgress 只读加载同周期最新一行；不存在返回 (nil, nil)。缓存回源用。
	LoadProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error)
	// SaveProgress 落库一份进度（有行更新、无行插入）。缓存写线用。
	SaveProgress(ctx context.Context, p *TaskProgress) error
	// WithTx 开启事务，回调返回错误即回滚。
	WithTx(ctx context.Context, fn func(tx Tx) error) error
}

// Tx 事务内动作。
type Tx interface {
	// LoadProgress 同周期最新一行 FOR UPDATE。
	LoadProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error)
	// UpsertProgress 写一次进度：有行更新、无行插入。
	UpsertProgress(ctx context.Context, p *TaskProgress, now time.Time) error
}

// 编译期断言：内置实现满足 Store / Tx。
var (
	_ Store = (*MySQLStore)(nil)
	_ Tx    = (*mysqlTx)(nil)
)

// taskDefCols task_def 的查询列，顺序与 TaskDef 字段扫描一致。
const taskDefCols = `id, title, ` + "`desc`" + `, ` + "`trigger`" + `, cycle, grant_mode, progress_type,
 reward_json, cycle_limit, target_count, target_count_type, enabled, start_at, end_at, pre_task_id,
 ` + "`group`" + `, clear_on_cycle, next_day_claim, created_at, updated_at`

// taskProgressCols task_progress 的查询列。
const taskProgressCols = `id, member_id, task_id, cycle_key, progress, status, snapshot_json, created_at, updated_at`

// MySQLStore 基于 touchgocore/db 的 Store 实现。
type MySQLStore struct {
	client *db.Client
}

// NewMySQLStore 构造 MySQL 存储。
func NewMySQLStore(client *db.Client) *MySQLStore {
	return &MySQLStore{client: client}
}

// selectAll 执行查询并扫描进 dest；MySQLStore 未装配时返回 ErrStoreNotConfigured。
func (s *MySQLStore) selectAll(ctx context.Context, dest any, sql string, args ...any) error {
	// 未装配 DB：统一判定为配置缺失。
	if s == nil || s.client == nil {
		return ErrStoreNotConfigured
	}
	// 通过 db.Client.Raw 复用连接池与执行封装。
	return s.client.Raw(ctx, func(gx *gorm.DB) error {
		return gx.Raw(sql, args...).Scan(dest).Error
	})
}

// FindDefByID 按主键取任务定义。
func (s *MySQLStore) FindDefByID(ctx context.Context, id int64) (*TaskDef, error) {
	var rows []TaskDef
	// 主键命中至多一行。
	if err := s.selectAll(ctx, &rows, `SELECT `+taskDefCols+` FROM task_def WHERE id = ? LIMIT 1`, id); err != nil {
		return nil, err
	}
	// 不存在返回 (nil, nil)，由调用方映射 ErrDefMissing。
	if len(rows) == 0 {
		return nil, nil
	}
	out := rows[0]
	return &out, nil
}

// ListEnabledDefs 列出全部启用任务。
func (s *MySQLStore) ListEnabledDefs(ctx context.Context) ([]TaskDef, error) {
	var rows []TaskDef
	// 全量启用行，按 id 稳定排序。
	if err := s.selectAll(ctx, &rows,
		`SELECT `+taskDefCols+` FROM task_def
 WHERE enabled = 1
 ORDER BY id ASC`); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []TaskDef{}
	}
	return rows, nil
}

// HasDoneProgress 该会员在该任务当前周期内是否已有已完成/已领取行。
func (s *MySQLStore) HasDoneProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (bool, error) {
	var rows []TaskProgress
	// 只关心是否存在终态行，取 id 即可。
	if err := s.selectAll(ctx, &rows,
		`SELECT id FROM task_progress
 WHERE member_id = ? AND task_id = ? AND cycle_key = ?
 AND status IN ('completed','claimed') LIMIT 1`, memberID, taskID, cycleKey); err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// ListProgressByMember 某会员全部进度行。
func (s *MySQLStore) ListProgressByMember(ctx context.Context, memberID int64) ([]TaskProgress, error) {
	var rows []TaskProgress
	// GetTask 列表聚合用，调用方自行按周期过滤。
	if err := s.selectAll(ctx, &rows,
		`SELECT `+taskProgressCols+` FROM task_progress WHERE member_id = ?`, memberID); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []TaskProgress{}
	}
	return rows, nil
}

// LoadProgress 同周期最新一行（只读）。
func (s *MySQLStore) LoadProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error) {
	var rows []TaskProgress
	// 并发下可能有重复行，取 id 最大的一行。
	if err := s.selectAll(ctx, &rows,
		`SELECT `+taskProgressCols+` FROM task_progress
 WHERE member_id = ? AND task_id = ? AND cycle_key = ?
 ORDER BY id DESC LIMIT 1`, memberID, taskID, cycleKey); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := rows[0]
	return &out, nil
}

// SaveProgress 落库一份进度；缓存写线（定时回写）使用。
func (s *MySQLStore) SaveProgress(ctx context.Context, p *TaskProgress) error {
	// nil 视为无操作，方便缓存层空值路径。
	if p == nil {
		return nil
	}
	// 回写统一走事务内的 upsert。
	return s.WithTx(ctx, func(tx Tx) error {
		return tx.UpsertProgress(ctx, p, time.Now().UTC())
	})
}

// execTx 事务骨架：回调出错回滚，panic 先回滚再抛。
func (s *MySQLStore) execTx(ctx context.Context, fn func(tx *db.Tx) error) error {
	// 未装配 DB 直接报错。
	if s == nil || s.client == nil {
		return ErrStoreNotConfigured
	}
	tx, err := s.client.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// panic 时先回滚再上抛，避免连接泄漏。
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		// 业务错误：回滚并返回原错误。
		_ = tx.Rollback()
		return err
	}
	// 回调成功：提交。
	return tx.Commit()
}

// WithTx 事务入口。
func (s *MySQLStore) WithTx(ctx context.Context, fn func(tx Tx) error) error {
	return s.execTx(ctx, func(tx *db.Tx) error {
		return fn(&mysqlTx{tx: tx})
	})
}

// mysqlTx 把原生 *db.Tx 适配成引擎的 Tx 接口。
type mysqlTx struct {
	tx *db.Tx
}

// LoadProgress 同周期最新一行 FOR UPDATE。
//
// 先定位 id（无锁）再按主键加锁：直接 `ORDER BY id DESC LIMIT 1 FOR UPDATE` 会在
// status 二级索引上加锁，与并发 UPDATE 互相等 insert intention（实测 1213 死锁）。
func (t *mysqlTx) LoadProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error) {
	var idRows []TaskProgress
	// 第一步：无锁定位最新一行的 id。
	if err := t.tx.Raw(
		`SELECT id FROM task_progress
 WHERE member_id = ? AND task_id = ? AND cycle_key = ?
 ORDER BY id DESC LIMIT 1`,
		memberID, taskID, cycleKey).Scan(&idRows).Error; err != nil {
		return nil, err
	}
	if len(idRows) == 0 {
		return nil, nil
	}
	// 第二步：按主键 FOR UPDATE 锁住整行。
	var rows []TaskProgress
	if err := t.tx.Raw(
		`SELECT `+taskProgressCols+` FROM task_progress WHERE id = ? FOR UPDATE`,
		idRows[0].ID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := rows[0]
	return &out, nil
}

// UpsertProgress 有行更新、无行插入。
func (t *mysqlTx) UpsertProgress(ctx context.Context, p *TaskProgress, now time.Time) error {
	now = now.UTC()
	// 快照列 NOT NULL，空值落空对象。
	if p.SnapshotJSON == "" {
		p.SnapshotJSON = "{}"
	}
	cur, err := t.LoadProgress(ctx, p.MemberID, p.TaskID, p.CycleKey)
	if err != nil {
		return err
	}
	// 已有行：按主键更新进度/状态/快照。
	if cur != nil {
		res := t.tx.Exec(
			`UPDATE task_progress SET progress = ?, status = ?, snapshot_json = ?, updated_at = ? WHERE id = ?`,
			p.Progress, p.Status, p.SnapshotJSON, now, cur.ID)
		return res.Error
	}
	// 无行：插入本周期的第一条进度。
	res := t.tx.Exec(
		`INSERT INTO task_progress(member_id, task_id, cycle_key, progress, status, snapshot_json, created_at, updated_at)
 VALUES(?,?,?,?,?,?,?,?)`,
		p.MemberID, p.TaskID, p.CycleKey, p.Progress, p.Status, p.SnapshotJSON, now, now)
	return res.Error
}

// TaskDefWrite 任务定义写入形状（存储原语，不含业务校验）。
type TaskDefWrite struct {
	// ID 任务定义主键；CreateDef 忽略，UpdateDef 必填。
	ID int64
	// Title 展示标题。
	Title string
	// Desc 说明文案。
	Desc string
	// Trigger 事件触发点。
	Trigger string
	// Cycle 周期：ONCE/DAILY/WEEKLY/SCHEDULED。
	Cycle string
	// GrantMode 结算模式：AUTO/CLAIM。
	GrantMode string
	// ProgressType 进度口径：INVITE/ADD。
	ProgressType string
	// Reward 奖励配置（JSON），引擎不解析。
	Reward string
	// CycleLimit 保留字段；完成阈值以 TargetCount 为准。
	CycleLimit int64
	// TargetCount 达成阈值；<=0 按 1。
	TargetCount float64
	// TargetCountType 对比条件：> / = / <；空按 =。
	TargetCountType string
	// Enabled 1=启用。
	Enabled int8
	// StartAt 投放开始时间（UTC）。
	StartAt *time.Time
	// EndAt 投放结束时间（UTC）。
	EndAt *time.Time
	// PreTaskID 前置任务 ID，0=无。
	PreTaskID int64
	// Group 列表分组标签。
	Group string
	// ClearOnCycle 保留字段，引擎不消费。
	ClearOnCycle int8
	// NextDayClaim 保留字段，引擎不消费。
	NextDayClaim int8
}

// CreateDef 新增任务定义，返回自增 id。
func (s *MySQLStore) CreateDef(ctx context.Context, in *TaskDefWrite, now time.Time) (int64, error) {
	now = now.UTC()
	var id int64
	err := s.execTx(ctx, func(tx *db.Tx) error {
		// 插入定义行（id 自增、时间戳取业务时间）。
		res := tx.Exec(
			`INSERT INTO task_def(title, `+"`desc`"+`, `+"`trigger`"+`, cycle, grant_mode, progress_type,
 reward_json, cycle_limit, target_count, target_count_type, enabled, start_at, end_at, pre_task_id,
 `+"`group`"+`, clear_on_cycle, next_day_claim, created_at, updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			in.Title, in.Desc, in.Trigger, in.Cycle, in.GrantMode, in.ProgressType,
			in.Reward, in.CycleLimit, in.TargetCount, in.TargetCountType, in.Enabled, in.StartAt, in.EndAt,
			in.PreTaskID, in.Group, in.ClearOnCycle, in.NextDayClaim, now, now)
		if res.Error != nil {
			return res.Error
		}
		// 取回自增主键。
		return tx.Raw(`SELECT LAST_INSERT_ID()`).Scan(&id).Error
	})
	return id, err
}

// UpdateDef 更新任务定义；返回是否命中。
func (s *MySQLStore) UpdateDef(ctx context.Context, in *TaskDefWrite, now time.Time) (bool, error) {
	now = now.UTC()
	var n int64
	err := s.execTx(ctx, func(tx *db.Tx) error {
		// 全字段覆盖更新，id 由 in.ID 指定。
		res := tx.Exec(
			`UPDATE task_def SET title = ?, `+"`desc`"+` = ?, `+"`trigger`"+` = ?, cycle = ?, grant_mode = ?,
 progress_type = ?, reward_json = ?, cycle_limit = ?, target_count = ?, target_count_type = ?, enabled = ?,
 start_at = ?, end_at = ?, pre_task_id = ?, `+"`group`"+` = ?, clear_on_cycle = ?, next_day_claim = ?,
 updated_at = ?
 WHERE id = ?`,
			in.Title, in.Desc, in.Trigger, in.Cycle, in.GrantMode,
			in.ProgressType, in.Reward, in.CycleLimit, in.TargetCount, in.TargetCountType, in.Enabled,
			in.StartAt, in.EndAt, in.PreTaskID, in.Group, in.ClearOnCycle, in.NextDayClaim,
			now, in.ID)
		n = res.RowsAffected
		return res.Error
	})
	return n > 0, err
}
