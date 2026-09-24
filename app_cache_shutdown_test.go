package touchgocore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"touchgocore/db"
	cachepkg "touchgocore/db/cache"
)

// shutdownKV 内存版一级缓存替身：只验时序与不丢数据，不模拟真实过期。
type shutdownKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *shutdownKV) Get(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	if !ok {
		return "", cachepkg.ErrNoEntry
	}
	return v, nil
}

func (k *shutdownKV) MGet(ctx context.Context, keys []string) ([]string, error) {
	out := make([]string, len(keys))
	for i, key := range keys {
		v, err := k.Get(ctx, key)
		if err == cachepkg.ErrNoEntry {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (k *shutdownKV) Set(_ context.Context, key, value string, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

func (k *shutdownKV) Del(_ context.Context, keys ...string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, key := range keys {
		delete(k.m, key)
	}
	return nil
}

type shutdownProfile struct {
	N int `json:"n"`
}

// shutdownSaver 模拟「下层数据库」：记录最终落库内容。
type shutdownSaver struct {
	mu      sync.Mutex
	saved   map[string]shutdownProfile
	saves   int
	deletes int
}

func (s *shutdownSaver) Save(_ context.Context, key string, val *shutdownProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.saved[key] = *val
	return nil
}

func (s *shutdownSaver) Delete(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	return nil
}

func (s *shutdownSaver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// dbLikeService 扮演排在 cache 之前注册的服务（如数据库访问层）：
// Shutdown 反序停止时它必须最后停——它的 Stop 被调用的一刻，
// cache 的 final flush 必须已经完成，否则真实关库会丢掉残余脏数据。
type dbLikeService struct {
	name   string
	stopOK func() error // Stop 时执行的断言
	err    error
}

func (s *dbLikeService) Name() string                { return s.name }
func (s *dbLikeService) Start(context.Context) error { return nil }
func (s *dbLikeService) Stop(context.Context) error {
	if s.stopOK != nil {
		s.err = s.stopOK()
	}
	return nil
}

// TestAppShutdownFlushesAllCachedWritesToDB 端到端验证用户要求：
// App.Shutdown 时，写线里「已同步写进 Redis、但定时器还没来得及落库」的
// 全部脏数据必须经 final flush 存回数据库，一条不丢、值为最后一次写入。
func TestAppShutdownFlushesAllCachedWritesToDB(t *testing.T) {
	kv := &shutdownKV{m: map[string]string{}}
	sn := &shutdownSaver{saved: map[string]shutdownProfile{}}

	cfg := db.DefaultCacheConfig()
	c := cfg // 局部别名，避免误改
	c.TTL = time.Hour
	// FlushInterval 设为 1 小时：正常运行期间绝不落库，
	// 落库只可能来自 Shutdown 的 final flush——正是被测路径。
	c.FlushInterval = time.Hour
	c.BatchSize = 10 // 300 个 key 会被切成多批，验证批量路径

	layer := db.NewCacheLayer(kv, db.WithLayerConfig(c))
	cache, err := db.OpenCache[string, shutdownProfile](layer, "profile",
		cachepkg.WithSaver[string, shutdownProfile](sn),
	)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{Cache: layer}
	watchdog := &dbLikeService{name: "db", stopOK: func() error {
		if got := sn.count(); got != 300 {
			return fmt.Errorf("停「数据库」时脏数据未全部落库: got=%d want=300", got)
		}
		return nil
	}}
	// 与 registerServices 同构的顺序：db 在前、cache 在末尾（TestCacheServiceRegisteredLast 已断言真实列表）
	app.services = []Service{watchdog, &cacheService{app: app}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.ctx = ctx
	app.cancel = cancel
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}

	const total = 300
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("k%03d", i)
		if err := cache.Write(ctx, key, &shutdownProfile{N: i}); err != nil {
			t.Fatalf("Write %s: %v", key, err)
		}
	}
	// 覆盖写：dedup 后落库值必须是最后一次
	for i := 0; i < total; i += 3 {
		key := fmt.Sprintf("k%03d", i)
		if err := cache.Write(ctx, key, &shutdownProfile{N: 1000 + i}); err != nil {
			t.Fatal(err)
		}
	}
	// 前置不变量：Write 是同步写 Redis 的——此刻 kv 里必须已有全部值，
	// 而「数据库」还一条都没收到（周期 flush 被 1 小时间隔挡住）。
	if got := sn.count(); got != 0 {
		t.Fatalf("Shutdown 前不应有周期落库: got=%d", got)
	}
	if len(kv.m) != total {
		t.Fatalf("Write 必须同步写 Redis: kv=%d want=%d", len(kv.m), total)
	}

	if err := app.Shutdown(10 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if watchdog.err != nil {
		t.Fatalf("停止顺序错误（final flush 必须早于其它服务停止完成）: %v", watchdog.err)
	}

	// 终态断言：300 个 key 全部落库，且值等于最后一次写入（覆盖写生效）。
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if len(sn.saved) != total {
		t.Fatalf("Shutdown 后脏数据未全部回存: saved=%d want=%d", len(sn.saved), total)
	}
	for i := 0; i < total; i++ {
		want := i
		if i%3 == 0 {
			want = 1000 + i
		}
		if got := sn.saved[fmt.Sprintf("k%03d", i)]; got.N != want {
			t.Fatalf("k%03d 落库值: N=%d want=%d", i, got.N, want)
		}
	}
	if sn.saves < total {
		t.Fatalf("saves 计数异常: %d", sn.saves)
	}
}

// TestLayerStopWithoutAppStart 直接验证 Layer.Stop 自身的 final flush：
// 不经过 App 也应把残余脏数据全部落库（Close/Stop 幂等）。
func TestLayerStopWithoutAppStart(t *testing.T) {
	kv := &shutdownKV{m: map[string]string{}}
	sn := &shutdownSaver{saved: map[string]shutdownProfile{}}
	cfg := db.DefaultCacheConfig()
	cfg.TTL = time.Hour
	cfg.FlushInterval = time.Hour

	layer := db.NewCacheLayer(kv, db.WithLayerConfig(cfg))
	cache, err := db.OpenCache[string, shutdownProfile](layer, "x",
		cachepkg.WithSaver[string, shutdownProfile](sn),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := layer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := cache.Write(ctx, fmt.Sprintf("v%02d", i), &shutdownProfile{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := layer.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if got := sn.count(); got != 50 {
		t.Fatalf("Layer.Stop final flush 丢数据: saved=%d want=50", got)
	}
	// 幂等：二次 Stop 不报错、不重复落库
	if err := layer.Stop(stopCtx); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}
}
