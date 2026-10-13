package cache

import (
	"context"

	"touchgocore/db/postgres"
)

// ==================== PostgreSQL 后端适配器 ====================
// 基于泛型 postgres.Repository[T]，与 mysql_source.go 同构。T 需带主键/唯一列
// （idCols），否则 Upsert 退化为 Create 会造成重复行。Delete 走 Repository.Delete
// （软删，若模型带 deleted_at）；需要与硬删一致时自行包一层 SaverFunc 调 DeleteHard。

// PostgresSource 按主键回源：Repository.FindByID。
func PostgresSource[T any, K comparable](r *postgres.Repository[T], keyToID func(K) any) Loader[K, T] {
	return LoaderFunc[K, T](func(ctx context.Context, key K) (*T, error) {
		v, err := r.FindByID(ctx, keyToID(key))
		if err != nil {
			if postgres.IsNotFound(err) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		return v, nil
	})
}

// PostgresBatchSource 多主键一次回源：Repository.FindAll + IN 查询。
// idOf 从实体取回业务键，keyFn 把它转成裸键串；两者必须与该 Cache 的
// WithKeyFunc 逐字一致。回填 map 的键由 Cache.New 延迟绑定的 KeyOf 补上命名空间
// 前缀与 sanitize（见 KeyBinder），键一致性是批量回源唯一的正确性前提。
func PostgresBatchSource[T any, K comparable](
	r *postgres.Repository[T],
	keyToID func(K) any,
	idCol string,
	keyFn func(K) string,
	idOf func(*T) K,
) BatchLoader[K, T] {
	if keyFn == nil {
		keyFn = defaultKeyFunc[K]()
	}
	single := PostgresSource[T, K](r, keyToID)
	bl := &batchLoader[K, T]{Loader: single}
	bl.loadBatch = func(ctx context.Context, keys []K, keyOf func(string) string) (map[string]*T, error) {
		ids := make([]any, 0, len(keys))
		for _, k := range keys {
			ids = append(ids, keyToID(k))
		}
		q := postgres.NewQuery().In(idCol, ids...)
		rows, err := r.FindAll(ctx, q)
		if err != nil {
			return nil, err
		}
		return mapBatchRows(rows, keyFn, idOf, keyOf), nil
	}
	return bl
}

// PostgresSink 单条落库：Save→Upsert（实体自带主键），Delete→软删。
func PostgresSink[T any, K comparable](r *postgres.Repository[T], keyToID func(K) any, idCols ...string) Saver[K, T] {
	return SaverFunc[K, T]{
		SaveFn: func(ctx context.Context, _ K, val *T) error {
			return r.Upsert(ctx, val, idCols)
		},
		DeleteFn: func(ctx context.Context, key K) error {
			return r.Delete(ctx, keyToID(key))
		},
	}
}

// PostgresBatchSink 批量落库：Upsert→UpsertBatch 一条 SQL；Delete 逐条。
func PostgresBatchSink[T any, K comparable](r *postgres.Repository[T], keyToID func(K) any, idCols ...string) BatchSaver[K, T] {
	single := PostgresSink[T, K](r, keyToID, idCols...)
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
