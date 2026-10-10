package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLockTimeout 在 LockWait 内未抢到分布式锁（另一个进程长期占用/回源过慢）。
// 调用方可按「源正忙」处理：重试或直接降级。
var ErrLockTimeout = errors.New("cache: lock timeout (another process holds the lock)")

// 释放锁的 Lua：仅当锁的 value 仍等于本次 token 才 DEL（原子「比较删除」），
// 防止误删持有锁的进程后来续上的锁（如锁已过期被别的进程抢走）。
const unlockScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
else
  return 0
end
`

// lockKey 分布式锁在 Redis 里的键，与数据键相邻但独立命名空间。
func (c *Cache[K, V]) lockKey(ks string) string {
	return ks + ":lock"
}

// lockEnabled 分布式锁是否生效：KV 支持 Locker 且配置了持有时长与等待窗口。
func (c *Cache[K, V]) lockEnabled() bool {
	return c.lock != nil && c.cfg.LockTTL > 0 && c.cfg.LockWait > 0
}

// lockToken 生成唯一的锁持有者标识（进程号 + 本 Cache 单调递增序号）。
// 释放锁时用它做比较删除，杜绝误删他人锁。
func (c *Cache[K, V]) lockToken() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), c.lockSeq.Add(1))
}

// tryAcquireLock 用 SETNX 抢锁；返回 true 表示本进程获得锁。
func (c *Cache[K, V]) tryAcquireLock(ctx context.Context, lockKey, token string) (bool, error) {
	sctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	return c.lock.SetNX(sctx, lockKey, token, c.cfg.LockTTL)
}

// releaseLock 原子释放锁（比较 token 后 DEL）。尽力而为：锁已过期/被他人抢走时
// 脚本返回 0 不误删，本函数也不报错——锁的 TTL 本来就是兜底。
func (c *Cache[K, V]) releaseLock(ctx context.Context, lockKey, token string) {
	sctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	_, _ = c.lock.Eval(sctx, unlockScript, []string{lockKey}, token)
}

// readFresh 只读 Redis：返回「未逻辑过期且非空」的值；miss/坏值/逻辑过期 → ErrMiss，
// 源确认不存在（空标记）→ ErrNotFound，Redis 错误原样返回。读线锁轮询用它判断
// 锁持有者是否已把值回填进 Redis。
func (c *Cache[K, V]) readFresh(ks string) (*V, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.ReadTimeout)
	defer cancel()
	raw, err := c.kv.Get(ctx, ks)
	if err != nil {
		if errors.Is(err, ErrNoEntry) {
			return nil, ErrMiss
		}
		return nil, err
	}
	e, derr := decode[V](c.codec, raw)
	if derr != nil {
		return nil, ErrMiss
	}
	if e.Null {
		return nil, ErrNotFound
	}
	if e.stale(c.clock().UnixMilli()) {
		return nil, ErrMiss
	}
	return &e.Value, nil
}

// reloadGuarded 读线回源的分布式锁包装：在 reload（回源→写 Redis→读回）之外加一层
// SETNX 锁，让同 key 的缓存击穿跨进程也只落在一个实例上。进程内的 singleflight
// 已把本进程并发 miss 合并到这一个 leader；这里的锁负责跨进程互斥。
//
// 未抢到锁时轮询 Redis（readFresh）——锁持有者回填完成后直接读到值，不再回源；
// 超过 LockWait 仍未就绪返回 ErrLockTimeout。锁操作自身失败（Redis 异常）时降级
// 直接回源，宁可多打一次 DB 也不阻塞业务。
func (c *Cache[K, V]) reloadGuarded(ks string, key K, ld Loader[K, V]) (any, error) {
	if !c.lockEnabled() {
		return c.reload(ks, key, ld)
	}
	lockKey := c.lockKey(ks)
	token := c.lockToken()
	deadline := c.clock().Add(c.cfg.LockWait)

	for {
		acquired, err := c.tryAcquireLock(context.Background(), lockKey, token)
		if err != nil {
			c.st.LockErr.Add(1)
			return c.reload(ks, key, ld) // 锁不可用：降级直接回源
		}
		if acquired {
			defer c.releaseLock(context.Background(), lockKey, token)
			// 抢到锁后，其他进程可能在轮询期间已回填：先读 Redis，有新鲜值就不再回源。
			if v, rerr := c.readFresh(ks); rerr == nil {
				return v, nil
			} else if errors.Is(rerr, ErrNotFound) {
				return nil, ErrNotFound
			}
			return c.reload(ks, key, ld)
		}
		// 未抢到锁：锁持有者正在回源，轮询 Redis 等值出现。
		if v, rerr := c.readFresh(ks); rerr == nil {
			return v, nil
		} else if errors.Is(rerr, ErrNotFound) {
			return nil, ErrNotFound
		}
		if c.clock().After(deadline) {
			c.st.LockTimeout.Add(1)
			return nil, fmt.Errorf("%w: cache=%s key=%s wait=%s", ErrLockTimeout, c.name, ks, c.cfg.LockWait)
		}
		select {
		case <-time.After(c.cfg.LockPollInterval):
		case <-context.Background().Done():
		}
	}
}

// writeWithLock 写路径的分布式锁包装：抢到锁才执行 fn，未抢到则轮询等待直到锁
// 释放或超时。锁操作失败时降级直接执行（保写可用性）。fn 返回 written 以保留
// 写线原有的「CAS 未生效即 Superseded」语义。
func (c *Cache[K, V]) writeWithLock(ctx context.Context, ks string, fn func() (bool, error)) (bool, error) {
	lockKey := c.lockKey(ks)
	token := c.lockToken()
	deadline := c.clock().Add(c.cfg.LockWait)

	for {
		acquired, err := c.tryAcquireLock(ctx, lockKey, token)
		if err != nil {
			c.st.LockErr.Add(1)
			return fn() // 锁不可用：降级直接写
		}
		if acquired {
			defer c.releaseLock(context.Background(), lockKey, token)
			return fn()
		}
		if c.clock().After(deadline) {
			c.st.LockTimeout.Add(1)
			return false, fmt.Errorf("%w: cache=%s key=%s", ErrLockTimeout, c.name, ks)
		}
		select {
		case <-time.After(c.cfg.LockPollInterval):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}