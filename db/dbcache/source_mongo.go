package dbcache

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// MongoCollection 是缓存层依赖的 Mongo 操作集合；*db.DbOperate 直接满足。
type MongoCollection interface {
	FindOne(name string, query interface{}, ret interface{}) error
	FindAll(name string, query interface{}, resHandler func(*mongo.Cursor) error) error
	UpdateInsert(name string, cond interface{}, doc interface{}) error
	RemoveOneByCond(name string, cond interface{}) error
}

// BindMongo 把一张表绑定到 MongoDB 集合。
func BindMongo[T any](table string, dbo MongoCollection, opt BindOption[T]) error {
	if dbo == nil {
		return fmt.Errorf("%w: mongo client is nil", ErrTableUnbound)
	}
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table)
	if err != nil {
		return err
	}
	b, err := newBinding[T](table, backendMongo, opt, keys, e.cfg)
	if err != nil {
		return err
	}
	b.srcFindOne = func(_ context.Context, vals []condVal) (any, error) {
		var row T
		ferr := dbo.FindOne(table, mongoFilter(vals), &row)
		if errors.Is(ferr, mongo.ErrNoDocuments) {
			return nil, ErrNoRows
		}
		if ferr != nil {
			return nil, ferr
		}
		return &row, nil
	}
	b.srcFindList = func(_ context.Context, vals []condVal) ([]any, error) {
		var rows []any
		ferr := dbo.FindAll(table, mongoFilter(vals), func(cur *mongo.Cursor) error {
			for cur.Next(context.Background()) {
				var row T
				if derr := cur.Decode(&row); derr != nil {
					return derr
				}
				rows = append(rows, &row)
			}
			return cur.Err()
		})
		if errors.Is(ferr, mongo.ErrNoDocuments) {
			return rows, nil
		}
		if ferr != nil {
			return nil, ferr
		}
		return rows, nil
	}
	b.sinkBatch = func(ctx context.Context, rows []any) error {
		typed := make([]*T, len(rows))
		for i, row := range rows {
			typed[i] = row.(*T)
		}
		if opt.FlushFn != nil {
			return opt.FlushFn(ctx, typed)
		}
		for _, row := range rows {
			cond, cerr := mongoKeyCond(b.meta, row)
			if cerr != nil {
				return cerr
			}
			if uerr := dbo.UpdateInsert(table, cond, row); uerr != nil {
				return uerr
			}
		}
		return nil
	}
	b.sinkDelete = func(_ context.Context, vals []condVal) error {
		return dbo.RemoveOneByCond(table, mongoFilter(vals))
	}
	return e.register(b)
}

// mongoFilter 把条件转成 bson.M；多值为 $in。
func mongoFilter(vals []condVal) bson.M {
	filter := bson.M{}
	for _, v := range vals {
		if len(v.Values) == 1 {
			filter[v.Column] = v.Values[0]
			continue
		}
		filter[v.Column] = bson.M{"$in": v.Values}
	}
	return filter
}

// mongoKeyCond 从行对象提取行键列，作为 upsert 的过滤条件。
func mongoKeyCond(meta *tableMeta, row any) (bson.M, error) {
	rv := reflect.ValueOf(row)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	cond := bson.M{}
	for _, col := range meta.KeyCols {
		pos, ok := meta.ByColumn[col]
		if !ok {
			return nil, fmt.Errorf("%w: key column %s", ErrKeyMissing, col)
		}
		fv := rv.Field(meta.Fields[pos].Index)
		if isZeroValue(fv) {
			return nil, fmt.Errorf("%w: key column %s is zero", ErrKeyMissing, col)
		}
		cond[col] = fv.Interface()
	}
	return cond, nil
}
