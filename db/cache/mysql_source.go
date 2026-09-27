package cache

import (
	"context"
	"sync"

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
// idOf 从实体取回业务键，keyFn 把它转成裸键串；两者必须与该 Cache 的
// WithKeyFunc 逐字一致。回填 map 的键由「Cache.New 延迟绑定的 KeyOf」补上
// 命名空间前缀与 sanitize（见 KeyBinder）——旧实现直接用 keyFn(id) 做键，
// 与查询侧 KeyOf 对不上，批量回源会全部落空并静默退化。
func MysqlBatchSource[T any, K comparable](
	r *mysql.Repository[T],
	keyToID func(K) any,
	idCol string,
	keyFn func(K) string,
	idOf func(*T) K,
) BatchLoader[K, T] {
	if keyFn == nil {
		keyFn = defaultKeyFunc[K]()
	}
	single := MysqlSource[T, K](r, keyToID)
	bl := &batchLoader[K, T]{Loader: single}
	bl.loadBatch = func(ctx context.Context, keys []K, keyOf func(string) string) (map[string]*T, error) {
		ids := make([]any, 0, len(keys))
		for _, k := range keys {
			ids = append(ids, keyToID(k))
		}
		q := mysql.NewQuery().In(idCol, ids...)
		rows, err := r.FindAll(ctx, q)
		if err != nil {
			return nil, err
		}
		return mapBatchRows(rows, keyFn, idOf, keyOf), nil
	}
	return bl
}

// mapBatchRows 把批量回源的行按 keyOf(keyFn(idOf(row))) 建索引。
// 单独拆成函数：键一致性是批量回源唯一的正确性前提，得能被单测直接
// 覆盖而不依赖真实 DB 连接。
func mapBatchRows[T any, K comparable](rows []*T, keyFn func(K) string, idOf func(*T) K, keyOf func(string) string) map[string]*T {
	out := make(map[string]*T, len(rows))
	for _, row := range rows {
		out[keyOf(keyFn(idOf(row)))] = row
	}
	return out
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

// batchLoader 把「单键 Load + 批量 loadBatch 闭包」装成 BatchLoader。
// 用指针接收者：keyOf 由 Cache.New 通过 BindKeyOf 延迟注入（见 KeyBinder）。
type batchLoader[K comparable, V any] struct {
	Loader    Loader[K, V]
	loadBatch func(ctx context.Context, keys []K, keyOf func(string) string) (map[string]*V, error)

	mu    sync.RWMutex
	keyOf func(string) string
}

// BindKeyOf 实现 KeyBinder：由 Cache.New 注入 keyOfRaw（含前缀与 sanitize）。
func (b *batchLoader[K, V]) BindKeyOf(f func(rawKey string) string) {
	b.mu.Lock()
	b.keyOf = f
	b.mu.Unlock()
}

func (b *batchLoader[K, V]) boundKeyOf() func(string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.keyOf
}

func (b *batchLoader[K, V]) Load(ctx context.Context, key K) (*V, error) {
	return b.Loader.Load(ctx, key)
}

// LoadBatch 未绑定 keyOf 时直接报错：静默返回裸键结果会让调用方一个也用不上，
// 表现成「命中率异常低 + 每次都打 DB」的难查退化。
func (b *batchLoader[K, V]) LoadBatch(ctx context.Context, keys []K) (map[string]*V, error) {
	keyOf := b.boundKeyOf()
	if keyOf == nil {
		return nil, ErrKeyOfUnbound
	}
	return b.loadBatch(ctx, keys, keyOf)
}

// 编译期确认：batchLoader 既是 BatchLoader 也是 KeyBinder
var (
	_ BatchLoader[string, struct{}] = (*batchLoader[string, struct{}])(nil)
	_ KeyBinder                     = (*batchLoader[string, struct{}])(nil)
)

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
