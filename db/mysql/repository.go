package mysql

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

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

// repoHooks 缓存反射元信息
type repoHooks[T any] struct {
	instance   T
	gormSchema *schema.Schema
	cache      *sync.Map
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

// Create 插入单条
func (r *Repository[T]) Create(ctx context.Context, entity *T) error {
	if entity == nil {
		return wrap("Create", errNilEntity, KindInvalidColumn)
	}
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().Create(entity).Error; err != nil {
		return newError("Create", err, classify(err), "", nil, time.Since(start))
	}
	return nil
}

// CreateBatch 批量插入
func (r *Repository[T]) CreateBatch(ctx context.Context, entities []*T) error {
	if len(entities) == 0 {
		return nil
	}
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().Create(entities).Error; err != nil {
		return newError("CreateBatch", err, classify(err), "", nil, time.Since(start))
	}
	return nil
}

// FindByID 按主键查找
func (r *Repository[T]) FindByID(ctx context.Context, id any) (*T, error) {
	var entity T
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().First(&entity, id).Error; err != nil {
		return nil, newError("FindByID", err, classify(err), "", []any{id}, time.Since(start))
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
	if err := tx.First(&entity).Error; err != nil {
		return nil, newError("FindOne", err, classify(err), "", q.whereArgs, time.Since(start))
	}
	return &entity, nil
}

// FindAll 按 Query 查多条
func (r *Repository[T]) FindAll(ctx context.Context, q *Query) ([]*T, error) {
	r2 := r.WithContext(ctx)
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
	var rows []*T
	start := time.Now()
	if err := tx.Find(&rows).Error; err != nil {
		return nil, newError("FindAll", err, classify(err), "", nil, time.Since(start))
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
	countTx := r2.gormDB()
	if q != nil && q.whereExpr != "" {
		countTx = countTx.Where(q.whereExpr, q.whereArgs...)
	}
	if g := q.groupString(); g != "" {
		countTx = countTx.Group(g)
	}
	start := time.Now()
	if err := countTx.Count(&total).Error; err != nil {
		return nil, 0, newError("Page.Count", err, classify(err), "", q.whereArgs, time.Since(start))
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
	tx := r2.gormDB()
	if q != nil && q.whereExpr != "" {
		tx = tx.Where(q.whereExpr, q.whereArgs...)
	}
	var n int64
	start := time.Now()
	if err := tx.Count(&n).Error; err != nil {
		return 0, newError("Count", err, classify(err), "", q.whereArgs, time.Since(start))
	}
	return n, nil
}

// Exists 是否存在
func (r *Repository[T]) Exists(ctx context.Context, q *Query) (bool, error) {
	r2 := r.WithContext(ctx)
	tx := r2.gormDB()
	if q != nil && q.whereExpr != "" {
		tx = tx.Where(q.whereExpr, q.whereArgs...)
	}
	var n int64
	start := time.Now()
	if err := tx.Select("1").Limit(1).Count(&n).Error; err != nil {
		return false, newError("Exists", err, classify(err), "", q.whereArgs, time.Since(start))
	}
	return n > 0, nil
}

// Update 按实体主键更新
func (r *Repository[T]) Update(ctx context.Context, entity *T) error {
	if entity == nil {
		return wrap("Update", errNilEntity, KindInvalidColumn)
	}
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().Save(entity).Error; err != nil {
		return newError("Update", err, classify(err), "", nil, time.Since(start))
	}
	return nil
}

// UpdateFields 按主键更新指定字段
func (r *Repository[T]) UpdateFields(ctx context.Context, id any, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().Model(new(T)).Where("id = ?", id).Updates(fields).Error; err != nil {
		return newError("UpdateFields", err, classify(err), "", []any{id}, time.Since(start))
	}
	return nil
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
	start := time.Now()
	tx := r2.gormDB().Clauses(clause.OnConflict{
		Columns:   makeColumns(conflictColumns),
		UpdateAll: true,
	})
	if err := tx.Create(entity).Error; err != nil {
		return newError("Upsert", err, classify(err), "", nil, time.Since(start))
	}
	return nil
}

// Delete 软删
func (r *Repository[T]) Delete(ctx context.Context, id any) error {
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().Delete(new(T), id).Error; err != nil {
		return newError("Delete", err, classify(err), "", []any{id}, time.Since(start))
	}
	return nil
}

// DeleteHard 硬删
func (r *Repository[T]) DeleteHard(ctx context.Context, id any) error {
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().Unscoped().Delete(new(T), id).Error; err != nil {
		return newError("DeleteHard", err, classify(err), "", []any{id}, time.Since(start))
	}
	return nil
}

// AutoMigrate 自动建表
func (r *Repository[T]) AutoMigrate(ctx context.Context) error {
	r2 := r.WithContext(ctx)
	start := time.Now()
	if err := r2.gormDB().AutoMigrate(new(T)); err != nil {
		return newError("AutoMigrate", err, classify(err), "", nil, time.Since(start))
	}
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