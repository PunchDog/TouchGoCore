package taskengine

import (
	"context"
	"fmt"

	"touchgocore/db"
	cachepkg "touchgocore/db/cache"
)

// ProgressKey 进度缓存键：会员 + 任务 + 周期。
type ProgressKey struct {
	MemberID int64
	TaskID   int64
	CycleKey string
}

// OpenProgressCache 在统一两级缓存层上注册 task_progress 类型。
//
// 读线回源走 store.LoadProgress，写线由缓存层同步写 Redis 并定时回写 DB。
// opts 可覆盖默认 TTL/写缓冲等参数；同名类型重复注册会返回错误。
func OpenProgressCache(
	layer *db.CacheLayer,
	store Store,
	opts ...db.CacheOption[ProgressKey, TaskProgress],
) (*db.Cache[ProgressKey, TaskProgress], error) {
	base := []db.CacheOption[ProgressKey, TaskProgress]{
		// 键格式：memberID:taskID:cycleKey，周期切换即自然换键。
		cachepkg.WithKeyFunc[ProgressKey, TaskProgress](func(k ProgressKey) string {
			return fmt.Sprintf("%d:%d:%s", k.MemberID, k.TaskID, k.CycleKey)
		}),
		// 读线回源：DB 有行返回值，无行返回 ErrCacheNotFound 触发空值缓存。
		cachepkg.WithLoader[ProgressKey, TaskProgress](db.NewLoaderFunc(func(ctx context.Context, k ProgressKey) (*TaskProgress, error) {
			p, err := store.LoadProgress(ctx, k.MemberID, k.TaskID, k.CycleKey)
			if err != nil {
				return nil, err
			}
			if p == nil {
				return nil, db.ErrCacheNotFound
			}
			return p, nil
		})),
		// 写线落库：缓存层定时回写时调用 SaveProgress；进度不做删除。
		cachepkg.WithSaver[ProgressKey, TaskProgress](db.NewSaverFunc(
			func(ctx context.Context, _ ProgressKey, p *TaskProgress) error {
				return store.SaveProgress(ctx, p)
			},
			func(context.Context, ProgressKey) error { return nil },
		)),
	}
	// 调用方 opts 追加在后，可覆盖默认配置。
	return db.OpenCache[ProgressKey, TaskProgress](layer, "task_progress", append(base, opts...)...)
}
