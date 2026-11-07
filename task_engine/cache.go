package taskengine

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"touchgocore/db"
)

// 两张表名：绑定与读写共用同一常量，避免字符串散落。
const (
	// tableTaskDef 任务定义表。
	tableTaskDef = "task_def"
	// tableTaskProgress 会员进度表。
	tableTaskProgress = "task_progress"
)

// progressCond 是 task_progress 的查询条件；nil 字段表示不限。
type progressCond struct {
	// MemberID 会员 ID。
	MemberID *int64 `cache:"member_id"`
	// TaskID 任务定义 ID。
	TaskID *int64 `cache:"task_id"`
	// CycleKey 周期片。
	CycleKey *string `cache:"cycle_key"`
}

// defIDCond 是 task_def 的主键条件。
type defIDCond struct {
	// ID 任务定义主键。
	ID *int64 `cache:"id"`
}

// defEnabledCond 是 task_def 的启用条件。
type defEnabledCond struct {
	// Enabled 1=启用。
	Enabled *int8 `cache:"enabled"`
}

// 存储装配状态：绑定只做一次，重复调用返回首次结果。
var (
	// storageOnce 保证两张表只绑定一次。
	storageOnce sync.Once
	// storageErr 首次装配的错误；nil 表示成功。
	storageErr error
)

// OpenStorage 把 task_progress 与 task_def 绑定到统一缓存层。
//
// 调用前必须先 db.InitCache；client 仅用于绑定两张表的回源与落库。
// 重复调用返回首次装配结果，不会重复绑定。
//
// 行键：进度表由 member_id/task_id/cycle_key 三列组成，定义表用主键 id。
// task_progress 的落库依赖三列唯一键，所以这里不再提供 FlushFn。
func OpenStorage(client *db.Client) error {
	storageOnce.Do(func() {
		// 没有 MySQL 客户端就无法绑定回源与落库。
		if client == nil {
			storageErr = ErrCacheNotInitialized
			return
		}
		if err := db.CacheBindMySQL[TaskProgress](tableTaskProgress, client, db.CacheBindOption[TaskProgress]{
			KeyColumns:      []string{"member_id", "task_id", "cycle_key"},
			ConflictColumns: []string{"member_id", "task_id", "cycle_key"},
			OrderBy:         []string{"id DESC"},
		}); err != nil {
			storageErr = err
			return
		}
		if err := db.CacheBindMySQL[TaskDef](tableTaskDef, client, db.CacheBindOption[TaskDef]{
			KeyColumns:      []string{"id"},
			ConflictColumns: []string{"id"},
		}); err != nil {
			storageErr = err
		}
	})
	return storageErr
}

// loadProgressRow 按三列键读本周期的进度行；源库也没有时返回 (nil, nil)。
func loadProgressRow(ctx context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error) {
	var p TaskProgress
	err := db.CacheQuery[TaskProgress](ctx, tableTaskProgress, &progressCond{
		MemberID: &memberID,
		TaskID:   &taskID,
		CycleKey: &cycleKey,
	}, &p)
	// 回源确认不存在：视为没有进度行。
	if errors.Is(err, db.ErrCacheNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// hasDoneProgress 判断该会员该任务本周期是否已处于终态。
//
// 唯一键保证每个周期只有一行，因此按点查读取后判断状态即可。
func hasDoneProgress(ctx context.Context, memberID, taskID int64, cycleKey string) (bool, error) {
	p, err := loadProgressRow(ctx, memberID, taskID, cycleKey)
	if err != nil || p == nil {
		return false, err
	}
	return TaskProgressDone(p.Status), nil
}

// loadDef 按主键读任务定义；不存在返回 (nil, nil)。
func loadDef(ctx context.Context, id int64) (*TaskDef, error) {
	var d TaskDef
	err := db.CacheQuery[TaskDef](ctx, tableTaskDef, &defIDCond{ID: &id}, &d)
	if errors.Is(err, db.ErrCacheNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// listEnabledDefs 读取全部启用定义，并按 id 升序返回。
//
// 列表查询由缓存层直查 DB，不带排序，所以这里补一次内存排序，
// 保证 Reload 建链与 GetTask 的顺序稳定。
func listEnabledDefs(ctx context.Context) ([]TaskDef, error) {
	enabled := int8(1)
	var rows []TaskDef
	if err := db.CacheQuery[TaskDef](ctx, tableTaskDef, &defEnabledCond{Enabled: &enabled}, &rows); err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// CreateDef 新增任务定义，返回写入的主键 id。
//
// task_def 的行键由 id 组成，调用方必须给出非零 ID（不再依赖自增）。
// 走 CacheSave(mustSave=true)：同步写 Redis，并在返回前落库。
func CreateDef(ctx context.Context, def *TaskDef, now time.Time) (int64, error) {
	if def == nil {
		return 0, ErrDefMissing
	}
	// 行键缺主键时缓存层无法定位，提前给出明确错误。
	if def.ID <= 0 {
		return 0, db.ErrCacheKeyMissing
	}
	now = now.UTC()
	if def.CreatedAt.IsZero() {
		def.CreatedAt = now
	}
	def.UpdatedAt = now
	if err := db.CacheSave(ctx, tableTaskDef, "", def, true); err != nil {
		return 0, err
	}
	return def.ID, nil
}

// UpdateDef 更新任务定义；返回是否命中。
//
// 先 CacheQuery 读旧行确认存在并沿用 created_at，再用 CacheSave 全量写回，
// 保证缓存与数据库同步。返回值语义是「旧行是否存在」。
func UpdateDef(ctx context.Context, def *TaskDef, now time.Time) (bool, error) {
	if def == nil || def.ID <= 0 {
		return false, nil
	}
	cur, err := loadDef(ctx, def.ID)
	if err != nil {
		return false, err
	}
	// 定义不存在：与原先 RowsAffected=0 的语义一致。
	if cur == nil {
		return false, nil
	}
	// 创建时间沿用旧行，避免全量写回把 created_at 覆盖成零值。
	if def.CreatedAt.IsZero() {
		def.CreatedAt = cur.CreatedAt
	}
	if def.CreatedAt.IsZero() {
		def.CreatedAt = now.UTC()
	}
	def.UpdatedAt = now.UTC()
	if err := db.CacheSave(ctx, tableTaskDef, "", def, true); err != nil {
		return false, err
	}
	return true, nil
}
