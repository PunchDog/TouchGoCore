package mysql

import (
	"context"

	"gorm.io/gorm"
)

// 包级泛型工厂
func NewRepository[T any](c *Client) *Repository[T] {
	return &Repository[T]{
		client: c,
		ctx:    context.Background(),
		hooks:  newRepoHooks[T](),
	}
}

// NewRepositoryWithContext 构造并绑定 ctx
func NewRepositoryWithContext[T any](c *Client, ctx context.Context) *Repository[T] {
	return &Repository[T]{
		client: c,
		ctx:    ctx,
		hooks:  newRepoHooks[T](),
	}
}

// NewTxRepository 在事务内构造
func NewTxRepository[T any](t *Tx) *Repository[T] {
	return &Repository[T]{
		client: t.client,
		ctx:    t.ctx,
		db:     t.DB.WithContext(t.ctx),
		hooks:  newRepoHooks[T](),
	}
}

// NewSessionRepository 在 Session 内构造
func NewSessionRepository[T any](s *Session) *Repository[T] {
	return &Repository[T]{
		client: s.client,
		ctx:    s.ctx,
		hooks:  newRepoHooks[T](),
	}
}

// ensureGormDB 内部助手
func (r *Repository[T]) ensureGormDB() *gorm.DB {
	if r.db != nil {
		return r.db
	}
	return r.client.engine
}