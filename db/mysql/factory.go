package mysql

import (
	"context"
)

// makeHooks 创建 hooks 并继承 Client 的 autoMigrate 开关
func makeHooks[T any](c *Client) *repoHooks[T] {
	h := newRepoHooks[T]()
	h.autoMigrate = c.AutoMigrateEnabled()
	return h
}

// 包级泛型工厂
func NewRepository[T any](c *Client) *Repository[T] {
	return &Repository[T]{
		client: c,
		ctx:    context.Background(),
		hooks:  makeHooks[T](c),
	}
}

// NewRepositoryWithContext 构造并绑定 ctx
func NewRepositoryWithContext[T any](c *Client, ctx context.Context) *Repository[T] {
	return &Repository[T]{
		client: c,
		ctx:    ctx,
		hooks:  makeHooks[T](c),
	}
}

// NewTxRepository 在事务内构造（事务内不进行自动建表，避免事务中 DDL 导致隐式提交）
func NewTxRepository[T any](t *Tx) *Repository[T] {
	h := newRepoHooks[T]() // 事务内 autoMigrate 强制 false
	return &Repository[T]{
		client: t.client,
		ctx:    t.ctx,
		db:     t.DB.WithContext(t.ctx),
		hooks:  h,
	}
}

// NewSessionRepository 在 Session 内构造
func NewSessionRepository[T any](s *Session) *Repository[T] {
	return &Repository[T]{
		client: s.client,
		ctx:    s.ctx,
		hooks:  makeHooks[T](s.client),
	}
}
