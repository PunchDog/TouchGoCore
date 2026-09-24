package mysql

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// Tx 是事务封装
type Tx struct {
	*gorm.DB
	client *Client
	ctx    context.Context
}

// Commit 提交事务
func (t *Tx) Commit() error {
	t.ensureNotNil()
	if t.DB.Error != nil {
		return newError("Commit", t.DB.Error, classify(t.DB.Error), "", nil, 0)
	}
	if err := t.DB.Commit().Error; err != nil {
		return newError("Commit", err, classify(err), "", nil, 0)
	}
	return nil
}

// Rollback 回滚事务
func (t *Tx) Rollback() error {
	t.ensureNotNil()
	if err := t.DB.Rollback().Error; err != nil {
		return newError("Rollback", err, classify(err), "", nil, 0)
	}
	return nil
}

// Context 返回事务 ctx
func (t *Tx) Context() context.Context { return t.ctx }

// Repository 在事务内获取泛型 CRUD
func (t *Tx) Repository() *RepositoryFactory {
	return &RepositoryFactory{
		client: t.client,
		ctx:    t.ctx,
	}
}

// WithContext 替换事务 ctx
func (t *Tx) WithContext(ctx context.Context) *Tx {
	return &Tx{DB: t.DB.WithContext(ctx), client: t.client, ctx: ctx}
}

func (t *Tx) ensureNotNil() {
	if t == nil || t.DB == nil {
		panic(fmt.Errorf("mysql: nil transaction"))
	}
}
