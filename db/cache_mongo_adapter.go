// Mongo 后端适配器。必须位于 db 包：DbOperate 属包 db，db/cache 不得 import db（成环）。
//
// 已知限制（源自 db/mongo_crud.go 现状）：全部 CRUD 走内部固定 5s 超时、
// 不接受调用方 ctx——超时不可配，写线 final flush 无法中断在途 Mongo 调用。
// 以 Mongo 为权威源时建议调大 MaxRetry、拉长 FlushInterval。
package db

import (
	"context"
	"errors"

	cachepkg "touchgocore/db/cache"

	"go.mongodb.org/mongo-driver/mongo"
)

// NewMongoCacheSource 按 filter 回源单条（FindOne → Decode 进 *T）。
// 未命中返回 cachepkg.ErrNotFound（触发空值缓存）。ctx 被忽略，见包注释限制。
func NewMongoCacheSource[T any, K comparable](dbo *DbOperate, coll string, keyToFilter func(K) any) cachepkg.Loader[K, T] {
	return cachepkg.LoaderFunc[K, T](func(_ context.Context, key K) (*T, error) {
		var out T
		if err := dbo.FindOne(coll, keyToFilter(key), &out); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, cachepkg.ErrNotFound
			}
			return nil, err
		}
		return &out, nil
	})
}

// NewMongoCacheSink 单条落库：Save→UpdateInsert（upsert 语义，实体需自带键字段）、
// Delete→RemoveOneByCond。
func NewMongoCacheSink[T any, K comparable](dbo *DbOperate, coll string, keyToFilter func(K) any) cachepkg.Saver[K, T] {
	return cachepkg.SaverFunc[K, T]{
		SaveFn: func(_ context.Context, key K, val *T) error {
			return dbo.UpdateInsert(coll, keyToFilter(key), val)
		},
		DeleteFn: func(_ context.Context, key K) error {
			return dbo.RemoveOneByCond(coll, keyToFilter(key))
		},
	}
}
