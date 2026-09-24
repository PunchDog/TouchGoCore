package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Layer 是多类型 Cache 的注册表与生命周期容器：
// 实现 Name/Start/Stop（与根包 Service 接口同形，db/cache 不 import 根包避免成环，
// 由 db 包的 cacheService 做编译期断言），统一驱动各 Cache 的写线定时器。
type Layer struct {
	kv  KV
	cfg Config // Register 的默认配置

	mu      sync.RWMutex
	handles []*layerHandle
	names   map[string]struct{}

	ctx     context.Context
	cancel  context.CancelFunc
	started atomic.Bool
	closed  atomic.Bool
	wg      sync.WaitGroup
}

type layerHandle struct {
	name  string
	run   func(ctx context.Context) error
	flush func(ctx context.Context) error
	stats func() StatsSnapshot
}

// LayerOption Layer 构造选项
type LayerOption func(*Layer)

// WithLayerConfig 设置 Register 的默认 Config（各 Cache 可用 WithConfig 覆盖）
func WithLayerConfig(c Config) LayerOption {
	return func(l *Layer) { l.cfg = c }
}

// NewLayer 构造缓存层。kv 为一级缓存句柄（如 NewRedisStore(app.Redis.Get()))。
func NewLayer(kv KV, opts ...LayerOption) *Layer {
	l := &Layer{kv: kv, cfg: DefaultConfig(), names: map[string]struct{}{}}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Register 注册一个类型的 Cache（名称唯一）。启动后注册也允许：
// 立即为该 Cache 起写线定时器协程。
func Register[K comparable, V any](l *Layer, name string, opts ...Option[K, V]) (*Cache[K, V], error) {
	if l == nil {
		return nil, errors.New("cache: Layer 为空")
	}
	if l.closed.Load() {
		return nil, errors.New("cache: Layer 已关闭")
	}
	merged := append([]Option[K, V]{WithConfig[K, V](l.cfg)}, opts...)
	c, err := New[K, V](name, l.kv, merged...)
	if err != nil {
		return nil, err
	}
	h := &layerHandle{
		name:  name,
		run:   c.Run,
		flush: c.Flush,
		stats: func() StatsSnapshot { return c.Stats().Snapshot() },
	}
	l.mu.Lock()
	if _, dup := l.names[name]; dup {
		l.mu.Unlock()
		return nil, fmt.Errorf("cache: 名称 %q 已注册", name)
	}
	l.names[name] = struct{}{}
	l.handles = append(l.handles, h)
	started := l.started.Load()
	runCtx := l.ctx
	l.mu.Unlock()

	if started && runCtx != nil {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			_ = h.run(runCtx)
		}()
	}
	return c, nil
}

// Name 服务名（Service 形态）
func (l *Layer) Name() string { return "cache" }

// Start 启动各 Cache 的写线定时器（幂等）
func (l *Layer) Start(ctx context.Context) error {
	if l == nil || l.kv == nil {
		return nil
	}
	if !l.started.CompareAndSwap(false, true) {
		return nil
	}
	l.mu.Lock()
	l.ctx, l.cancel = context.WithCancel(ctx)
	runCtx := l.ctx
	handles := make([]*layerHandle, len(l.handles))
	copy(handles, l.handles)
	l.mu.Unlock()

	for _, h := range handles {
		l.wg.Add(1)
		go func(h *layerHandle) {
			defer l.wg.Done()
			_ = h.run(runCtx)
		}(h)
	}
	return nil
}

// Stop 取消定时器 → 等各 Run 完成 final flush → 兜底 FlushAll。
// 受 ctx 超时预算约束，超时返回错误不卡退出。
func (l *Layer) Stop(ctx context.Context) error {
	if l == nil || !l.started.Swap(false) {
		return nil
	}
	l.mu.Lock()
	cancel := l.cancel
	handles := make([]*layerHandle, len(l.handles))
	copy(handles, l.handles)
	l.mu.Unlock()

	if cancel != nil {
		cancel() // 各 Run 收到后执行 final flush 并退出
	}

	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	var errs []error
	for _, h := range handles {
		if err := h.flush(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cache[%s] flush: %w", h.name, err))
		}
	}
	l.closed.Store(true)
	return errors.Join(errs...)
}

// Close 等价 Stop（脱离 App 单独使用时的习惯命名）
func (l *Layer) Close(ctx context.Context) error { return l.Stop(ctx) }

// FlushAll 立即把全部 Cache 的写缓冲落库
func (l *Layer) FlushAll(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	handles := make([]*layerHandle, len(l.handles))
	copy(handles, l.handles)
	l.mu.RUnlock()
	var errs []error
	for _, h := range handles {
		if err := h.flush(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cache[%s] flush: %w", h.name, err))
		}
	}
	return errors.Join(errs...)
}

// Stats 全部 Cache 的计数快照
func (l *Layer) Stats() map[string]StatsSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]StatsSnapshot, len(l.handles))
	for _, h := range l.handles {
		out[h.name] = h.stats()
	}
	return out
}
