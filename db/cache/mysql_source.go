package cache

import (
	"context"

	"touchgocore/db/mysql"
)

// ==================== MySQL 后端适配器 ====================
// 基于泛型 mysql.Repository[T]。T 需带主键/唯一列（idCols），否则 Upsert
// 退化为 Create 会造成重复行。Delete 走 Repository.Delete（软删，若模型带
// deleted_at）；需要与硬删一致时自行包一层 SaverFunc 调 DeleteHard。

// MysqlSource 按主键回源：Repository.FindByID。
func MysqlSource[T any, K comparable](r *mysql.Repository[T], keyToID func(K) any) Loader[K, T] {
	return LoaderFunc[K, T](func(ctx context.Context, key K) (*T, error) {
		v, err := r.FindByID(ctx, keyToID(key))
		if err != nil {
			if mysql.IsNotFound(err) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		return v, nil
	})
}

// MysqlBatchSource 多主键一次回源：Repository.FindAll + IN 查询。
// idOf 从实体取回业务键（用于结果 map 的 KeyOf 对齐）。
func MysqlBatchSource[T any, K comparable](
	r *mysql.Repository[T],
	keyToID func(K) any,
	idCol string,
	keyFn func(K) string,
	idOf func(*T) K,
) BatchLoader[K, T] {
	single := MysqlSource[T, K](r, keyToID)
	return batchLoader[K, T]{
		Loader: single,
		loadBatch: func(ctx context.Context, keys []K) (map[string]*T, error) {
			ids := make([]any, 0, len(keys))
			for _, k := range keys {
				ids = append(ids, keyToID(k))
			}
			q := mysql.NewQuery().In(idCol, ids...)
			rows, err := r.FindAll(ctx, q)
			if err != nil {
				return nil, err
			}
			out := make(map[string]*T, len(rows))
			for _, row := range rows {
				out[keyFn(idOf(row))] = row
			}
			return out, nil
		},
	}
}

// MysqlSink 单条落库：Save→Upsert（实体自带主键），Delete→软删。
func MysqlSink[T any, K comparable](r *mysql.Repository[T], keyToID func(K) any, idCols ...string) Saver[K, T] {
	return SaverFunc[K, T]{
		SaveFn: func(ctx context.Context, _ K, val *T) error {
			return r.Upsert(ctx, val, idCols)
		},
		DeleteFn: func(ctx context.Context, key K) error {
			return r.Delete(ctx, keyToID(key))
		},
	}
}

// MysqlBatchSink 批量落库：Upsert→UpsertBatch 一条 SQL；Delete 逐条。
func MysqlBatchSink[T any, K comparable](r *mysql.Repository[T], keyToID func(K) any, idCols ...string) BatchSaver[K, T] {
	single := MysqlSink[T, K](r, keyToID, idCols...)
	return batchSaver[K, T]{
		Saver: single,
		saveBatch: func(ctx context.Context, items []Item[K]) error {
			var ups []*T
			var dels []K
			for _, it := range items {
				switch it.Op {
				case OpUpsert:
					if v, ok := it.Value.(*T); ok && v != nil {
						ups = append(ups, v)
					}
				case OpDelete:
					dels = append(dels, it.Key)
				}
			}
			if len(ups) > 0 {
				if err := r.UpsertBatch(ctx, ups, idCols); err != nil {
					return err
				}
			}
			for _, k := range dels {
				if err := single.Delete(ctx, k); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// --- 内部装箱：让函数闭包实现 BatchLoader/BatchSaver 接口 ---

type batchLoader[K comparable, V any] struct {
	Loader    Loader[K, V]
	loadBatch func(ctx context.Context, keys []K) (map[string]*V, error)
}

func (b batchLoader[K, V]) Load(ctx context.Context, key K) (*V, error) {
	return b.Loader.Load(ctx, key)
}
func (b batchLoader[K, V]) LoadBatch(ctx context.Context, keys []K) (map[string]*V, error) {
	return b.loadBatch(ctx, keys)
}

type batchSaver[K comparable, V any] struct {
	Saver     Saver[K, V]
	saveBatch func(ctx context.Context, items []Item[K]) error
}

func (b batchSaver[K, V]) Save(ctx context.Context, key K, val *V) error {
	return b.Saver.Save(ctx, key, val)
}
func (b batchSaver[K, V]) Delete(ctx context.Context, key K) error {
	return b.Saver.Delete(ctx, key)
}
func (b batchSaver[K, V]) SaveBatch(ctx context.Context, items []Item[K]) error {
	return b.saveBatch(ctx, items)
}
