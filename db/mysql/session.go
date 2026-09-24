package mysql

import "context"

// Session 是绑定 ctx 的客户端视图
type Session struct {
	client *Client
	ctx    context.Context
}

// Context 返回当前会话的 ctx
func (s *Session) Context() context.Context { return s.ctx }

// WithContext 返回新会话
func (s *Session) WithContext(ctx context.Context) *Session {
	return &Session{client: s.client, ctx: ctx}
}

// Repository 在当前 session 上获取泛型 CRUD
func (s *Session) Repository() *RepositoryFactory {
	return s.client.Repository().WithContext(s.ctx)
}

// Client 返回底层 Client
func (s *Session) Client() *Client { return s.client }
