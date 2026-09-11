package mysql

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/vars"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

var (
	errNilEntity = errors.New("mysql: nil entity")
	errNilQuery  = errors.New("mysql: nil query")
)

// Repository[T] 是泛型 CRUD 封装
type Repository[T any] struct {
	client *Client
	ctx    context.Context
	db     *gorm.DB
	hooks  *repoHooks[T]
}

// repoHooks 缓存反射元信息 + 自动建表状态
type repoHooks[T any] struct {
	instance   T
	gormSchema *schema.Schema
	cache      *sync.Map
	autoMigrate bool
	migrated    atomic.Bool
}

// newRepoHooks 创建 hooks
func newRepoHooks[T any]() *repoHooks[T] {
	var zero T
	return &repoHooks[T]{instance: zero, cache: &sync.Map{}}
}

// WithContext 替换 ctx
func (r *Repository[T]) WithContext(ctx context.Context) *Repository[T] {
	cp := *r
	cp.ctx = ctx
	return &cp
}

// Client 返回底层 Client
func (r *Repository[T]) Client() *Client { return r.client }

// Context 返回当前 ctx
func (r *Repository[T]) Context() context.Context { return r.ctx }

// gormDB 返回带 ctx 的 gorm.DB
func (r *Repository[T]) gormDB() *gorm.DB {
	if r.db != nil {
		return r.db.WithContext(r.ctx)
	}
	return r.client.engine.WithContext(r.ctx)
}

// ensureSchema 懒加载 gorm schema
func (r *Repository[T]) ensureSchema() (*schema.Schema, error) {
	if r.hooks.gormSchema != nil {
		return r.hooks.gormSchema, nil
	}
	s, err := schema.Parse(&r.hooks.instance, r.hooks.cache, r.gormDB().Config.NamingStrategy)
	if err != nil {
		return nil, wrap("ensureSchema", err, KindInvalidColumn)
	}
	r.hooks.gormSchema = s
	return s, nil
}

// TableName 返回 T对应的表名
func (r *Repository[T]) TableName() string {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if tn, ok := reflect.New(typ).Interface().(interface{ TableName() string }); ok {
		return tn.TableName()
	}
	s, err := r.ensureSchema()
	if err == nil && s != nil {
		return s.Table
	}
	return toSnakeCase(typ.Name())
}

// runWithAutoMigrate 是表不存在自动建表的核心包装器。
//
//  - 未启用 autoMigrate 或已迁移 → 直接执行 fn
//  - 首次执行失败且错误为「表不存在」 → AutoMigrate(T) + 标记 migrated + 重试一次
//  - 其他错误 / 迁移失败 → 透传
func (r *Repository[T]) runWithAutoMigrate(ctx context.Context, op string, fn func() error) error {
	if !r.hooks.autoMigrate {
		return fn()
	}
	if r.hooks.migrated.Load() {
		return fn()
	}
	err := fn()
	if err == nil {
		return nil
	}
	if !IsTableNotExist(err) {
		return err
	}
	// 表不存在，尝试自动迁移
	migrateStart := time.Now()
	if migrateErr := r.gormDB().WithContext(ctx).AutoMigrate(new(T)); migrateErr != nil {
		return newError(op+".AutoMigrate", migrateErr, classify(migrateErr), "", nil, time.Since(migrateStart))
	}
	r.hooks.migrated.Store(true)
	vars.Info("MySQL 自动建表成功 table=%s op=%s", r.TableName(), op)
	// 重试一次原操作
	return fn()
}

// Create 插入单条
func (r *Repository[T]) Create(ctx context.Context, entity *T) error {
	if entity == nil {
		return wrap("Create", errNilEntity, KindInvalidColumn)
	}
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "Create", func() error {
		start := time.Now()
		if err := r2.gormDB().Create(entity).Error; err != nil {
			return newError("Create", err, classify(err), "", nil, time.Since(start))
		}
		return nil
	})
}

// CreateBatch 批量插入
func (r *Repository[T]) CreateBatch(ctx context.Context, entities []*T) error {
	if len(entities) == 0 {
		return nil
	}
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "CreateBatch", func() error {
		start := time.Now()
		if err := r2.gormDB().Create(entities).Error; err != nil {
			return newError("CreateBatch", err, classify(err), "", nil, time.Since(start))
		}
		return nil
	})
}

// FindByID 按主键查找
func (r *Repository[T]) FindByID(ctx context.Context, id any) (*T, error) {
	var entity T
	r2 := r.WithContext(ctx)
	var callErr error
	err := r2.runWithAutoMigrate(ctx, "FindByID", func() error {
		start := time.Now()
		callErr = r2.gormDB().First(&entity, id).Error
		if callErr != nil {
			return newError("FindByID", callErr, classify(callErr), "", []any{id}, time.Since(start))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &entity, nil
}

// FindOne 按 Query 查单条
func (r *Repository[T]) FindOne(ctx context.Context, q *Query) (*T, error) {
	if q == nil {
		return nil, wrap("FindOne", errNilQuery, KindInvalidColumn)
	}
	var entity T
	r2 := r.WithContext(ctx)
	var callErr error
	err := r2.runWithAutoMigrate(ctx, "FindOne", func() error {
		tx := r2.gormDB()
		if q.whereExpr != "" {
			tx = tx.Where(q.whereExpr, q.whereArgs...)
		}
		if s := q.orderString(); s != "" {
			tx = tx.Order(s)
		}
		if sel := q.selectString(); sel != "" {
			tx = tx.Select(sel)
		}
		start := time.Now()
		callErr = tx.First(&entity).Error
		if callErr != nil {
			return newError("FindOne", callErr, classify(callErr), "", q.whereArgs, time.Since(start))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &entity, nil
}

// FindAll 按 Query 查多条
func (r *Repository[T]) FindAll(ctx context.Context, q *Query) ([]*T, error) {
	r2 := r.WithContext(ctx)
	var rows []*T
	var callErr error
	err := r2.runWithAutoMigrate(ctx, "FindAll", func() error {
		tx := r2.gormDB()
		if q != nil {
			if q.whereExpr != "" {
				tx = tx.Where(q.whereExpr, q.whereArgs...)
			}
			if s := q.orderString(); s != "" {
				tx = tx.Order(s)
			}
			if sel := q.selectString(); sel != "" {
				tx = tx.Select(sel)
			}
			if g := q.groupString(); g != "" {
				tx = tx.Group(g)
			}
			if q.having != "" {
				tx = tx.Having(q.having, q.havingArgs...)
			}
			if q.limit > 0 {
				tx = tx.Limit(q.limit)
			}
			if q.offset > 0 {
				tx = tx.Offset(q.offset)
			}
		}
		start := time.Now()
		callErr = tx.Find(&rows).Error
		if callErr != nil {
			return newError("FindAll", callErr, classify(callErr), "", nil, time.Since(start))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Page 分页查询
func (r *Repository[T]) Page(ctx context.Context, q *Query, page, size int) (items []*T, total int64, err error) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	r2 := r.WithContext(ctx)
	countErr := r2.runWithAutoMigrate(ctx, "Page.Count", func() error {
		countTx := r2.gormDB()
		if q != nil && q.whereExpr != "" {
			countTx = countTx.Where(q.whereExpr, q.whereArgs...)
		}
		if g := q.groupString(); g != "" {
			countTx = countTx.Group(g)
		}
		start := time.Now()
		if cerr := countTx.Count(&total).Error; cerr != nil {
			return newError("Page.Count", cerr, classify(cerr), "", q.whereArgs, time.Since(start))
		}
		return nil
	})
	if countErr != nil {
		return nil, 0, countErr
	}
	if q == nil {
		q = NewQuery()
	}
	q.Limit(size).Offset((page - 1) * size)
	items, err = r2.FindAll(ctx, q)
	return items, total, err
}

// Count 统计
func (r *Repository[T]) Count(ctx context.Context, q *Query) (int64, error) {
	r2 := r.WithContext(ctx)
	var n int64
	var callErr error
	err := r2.runWithAutoMigrate(ctx, "Count", func() error {
		tx := r2.gormDB()
		if q != nil && q.whereExpr != "" {
			tx = tx.Where(q.whereExpr, q.whereArgs...)
		}
		start := time.Now()
		callErr = tx.Count(&n).Error
		if callErr != nil {
			return newError("Count", callErr, classify(callErr), "", q.whereArgs, time.Since(start))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Exists 是否存在
func (r *Repository[T]) Exists(ctx context.Context, q *Query) (bool, error) {
	r2 := r.WithContext(ctx)
	var n int64
	var callErr error
	err := r2.runWithAutoMigrate(ctx, "Exists", func() error {
		tx := r2.gormDB()
		if q != nil && q.whereExpr != "" {
			tx = tx.Where(q.whereExpr, q.whereArgs...)
		}
		start := time.Now()
		callErr = tx.Select("1").Limit(1).Count(&n).Error
		if callErr != nil {
			return newError("Exists", callErr, classify(callErr), "", q.whereArgs, time.Since(start))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Update 按实体主键更新
func (r *Repository[T]) Update(ctx context.Context, entity *T) error {
	if entity == nil {
		return wrap("Update", errNilEntity, KindInvalidColumn)
	}
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "Update", func() error {
		start := time.Now()
		if err := r2.gormDB().Save(entity).Error; err != nil {
			return newError("Update", err, classify(err), "", nil, time.Since(start))
		}
		return nil
	})
}

// UpdateFields 按主键更新指定字段
func (r *Repository[T]) UpdateFields(ctx context.Context, id any, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "UpdateFields", func() error {
		start := time.Now()
		if err := r2.gormDB().Model(new(T)).Where("id = ?", id).Updates(fields).Error; err != nil {
			return newError("UpdateFields", err, classify(err), "", []any{id}, time.Since(start))
		}
		return nil
	})
}

// Upsert 插入或更新
func (r *Repository[T]) Upsert(ctx context.Context, entity *T, conflictColumns []string) error {
	if entity == nil {
		return wrap("Upsert", errNilEntity, KindInvalidColumn)
	}
	if len(conflictColumns) == 0 {
		return r.Create(ctx, entity)
	}
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "Upsert", func() error {
		start := time.Now()
		tx := r2.gormDB().Clauses(clause.OnConflict{
			Columns:   makeColumns(conflictColumns),
			UpdateAll: true,
		})
		if err := tx.Create(entity).Error; err != nil {
			return newError("Upsert", err, classify(err), "", nil, time.Since(start))
		}
		return nil
	})
}

// Delete 软删
func (r *Repository[T]) Delete(ctx context.Context, id any) error {
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "Delete", func() error {
		start := time.Now()
		if err := r2.gormDB().Delete(new(T), id).Error; err != nil {
			return newError("Delete", err, classify(err), "", []any{id}, time.Since(start))
		}
		return nil
	})
}

// DeleteHard 硬删
func (r *Repository[T]) DeleteHard(ctx context.Context, id any) error {
	r2 := r.WithContext(ctx)
	return r2.runWithAutoMigrate(ctx, "DeleteHard", func() error {
		start := time.Now()
		if err := r2.gormDB().Unscoped().Delete(new(T), id).Error; err != nil {
			return newError("DeleteHard", err, classify(err), "", []any{id}, time.Since(start))
		}
		return nil
	})
}

// AutoMigrate 手动建表（不依赖 Option 开关）
func (r *Repository[T]) AutoMigrate(ctx context.Context) error {
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().AutoMigrate(new(T)); err != nil {
		return newError("AutoMigrate", err, classify(err), "", nil, time.Since(start))
	}
	r.hooks.migrated.Store(true)
	return nil
}

// makeColumns 构造 gorm clause.Column 列表
func makeColumns(names []string) []clause.Column {
	cols := make([]clause.Column, 0, len(names))
	for _, n := range names {
		cols = append(cols, clause.Column{Name: n})
	}
	return cols
}