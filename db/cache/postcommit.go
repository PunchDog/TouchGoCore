package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"touchgocore/vars"
)

// ==================== post-commit 缓存动作队列 ====================
//
// 为什么需要它：缓存失效绝不能先于业务事务 COMMIT 逃逸——在事务里直接 DEL Redis，
// 事务回滚后 Redis 已经被动过（并发读会把旧值从数据库重新填回缓存，看起来「没坏」，
// 但如果失效动作是 SET 新值，回滚后 Redis 就留着一个库里不存在的值）。
// 于是把动作排进 ctx 上的队列，只有 Commit 返回 nil 才由调用方（infra.WithTx）运行。
// 回滚/panic 路径一条都不执行——这是结构性的，不依赖调用方记得清理。
//
// 队列执行失败**不该**回滚业务事务：库已提交，它就是真源；Redis 至多陈旧一个 TTL。
// 因此调用方对 RunPostCommit 的错误只记 Error 日志，不向上抛业务失败。

// PostCommitAction 一个只能在业务事务 COMMIT 成功之后执行的缓存动作。
type PostCommitAction func(ctx context.Context) error

type postCommitQueue struct {
	mu   sync.Mutex
	acts []PostCommitAction
	ran  bool
}

type postCommitCtxKey struct{}

// ErrNoPostCommitSink ctx 上没挂队列。
// 为什么不静默丢弃：忘记挂队列 == 忘记失效，那是最贵的静默失败模式。
var ErrNoPostCommitSink = errors.New("cache: ctx 未挂 post-commit 队列（事务 ctx 需先经 WithPostCommit 包裹）")

var ErrPostCommitNested = errors.New("cache: WithPostCommit 重复挂载（嵌套事务的失效归属不明确，请由最外层事务持有队列）")

var ErrPostCommitRan = errors.New("cache: post-commit 队列已执行，再排动作将永不被运行")

func queueOf(ctx context.Context) *postCommitQueue {
	if ctx == nil {
		return nil
	}
	q, _ := ctx.Value(postCommitCtxKey{}).(*postCommitQueue)
	return q
}

// WithPostCommit 给 ctx 挂一条 post-commit 队列（事务的失效动作暂存处）。
// 重复挂载返回 ErrPostCommitNested：内层事务若自己 commit，会把外层尚未提交的
// 失效动作提前跑掉——这在语义上就是错的，必须出声而不是悄悄复用。
func WithPostCommit(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("cache: WithPostCommit 需要非 nil ctx")
	}
	if queueOf(ctx) != nil {
		return ctx, ErrPostCommitNested
	}
	return context.WithValue(ctx, postCommitCtxKey{}, &postCommitQueue{}), nil
}

// AppendPostCommit 追加动作，保持给出顺序。未挂队列/队列已执行都返回错误。
func AppendPostCommit(ctx context.Context, acts ...PostCommitAction) error {
	q := queueOf(ctx)
	if q == nil {
		return ErrNoPostCommitSink
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ran {
		return ErrPostCommitRan
	}
	for _, a := range acts {
		if a == nil {
			return errors.New("cache: AppendPostCommit 不接受 nil 动作")
		}
		q.acts = append(q.acts, a)
	}
	return nil
}

// RunPostCommit 按登记顺序执行并清空队列，返回聚合错误（不因首个失败而中断后续）。
// 可安全地对未挂队列的 ctx 调用（返回 nil）——用于「本次事务没有失效需求」的正常路径。
func RunPostCommit(ctx context.Context) error {
	q := queueOf(ctx)
	if q == nil {
		return nil
	}
	q.mu.Lock()
	acts := q.acts
	q.acts = nil
	q.ran = true
	q.mu.Unlock()

	var errs []error
	for i, a := range acts {
		if err := a(ctx); err != nil {
			errs = append(errs, fmt.Errorf("post-commit[%d/%d]: %w", i+1, len(acts), err))
		}
	}
	return errors.Join(errs...)
}

// Invalidate 生成「只失效 Redis，不通知数据库」的动作。
// c==nil（本进程未启用缓存）时返回空动作而非 error：没缓存就没什么可失效，
// 调用方不该为「这次没得失效」写分支。
// 注意 Enabled=false 时仍然执行删除：Redis 里可能留着上一次开启时写入的值，删它零代价。
func Invalidate[K comparable, V any](_ context.Context, c *Cache[K, V], key K) PostCommitAction {
	if c == nil {
		return func(context.Context) error { return nil }
	}
	return func(ctx context.Context) error {
		if err := c.DeleteCache(ctx, key); err != nil {
			return fmt.Errorf("cache[%s] 失效失败 key=%v: %w", c.name, key, err)
		}
		return nil
	}
}

// InvalidateOnCommit 事务内的标准失效写法：队列在则排队（COMMIT 成功后才动 Redis），
// 队列不在则立即执行并告警——立即执行是对的（此时没有「未提交」需要保护），
// 但漏挂队列属于需要被看见的接线错误。
func InvalidateOnCommit[K comparable, V any](ctx context.Context, c *Cache[K, V], key K) error {
	act := Invalidate(ctx, c, key)
	if queueOf(ctx) == nil {
		vars.Warning("cache: InvalidateOnCommit 跑在非事务 ctx 上，改为立即失效 key=%v", key)
		return act(ctx)
	}
	return AppendPostCommit(ctx, act)
}
