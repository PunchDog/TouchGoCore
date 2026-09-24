package cache

import (
	"context"
	"testing"
	"time"
)

// Layer 与根包 Service 接口同形（Name/Start/Stop）——根包侧有编译期断言，
// 这里以方法集断言防漂移。
type serviceShape interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

var _ serviceShape = (*Layer)(nil)

func TestLayer_RegisterStartStop(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	l := NewLayer(kv, WithLayerConfig(testConfig()))

	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 1}}, notFound: map[string]bool{}}
	sn := newRecSaver()
	c, err := Register[string, testVal](l, "t1",
		WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld),
		WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Register[string, testVal](l, "t1"); err == nil {
		t.Fatal("重名注册应报错")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Start(ctx); err != nil {
		t.Fatal("重复 Start 应幂等")
	}

	if v, err := c.GetOrLoad(ctx, "k1"); err != nil || v.N != 1 {
		t.Fatalf("读线: v=%v err=%v", v, err)
	}
	// 二次读必须命中缓存（Hits 只在缓存命中时计数）
	if v, err := c.GetOrLoad(ctx, "k1"); err != nil || v.N != 1 {
		t.Fatalf("读线二次命中: v=%v err=%v", v, err)
	}
	if err := c.Write(ctx, "k2", &testVal{N: 2}); err != nil {
		t.Fatal(err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := l.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := sn.get("k2"); got == nil || got.N != 2 {
		t.Fatal("Stop 的 final flush 必须落库")
	}
	if stats := l.Stats(); stats["t1"].Hits == 0 {
		t.Fatal("Stats 应含注册表计数")
	}
}

// 启动后注册：立即挂上定时器协程，Stop 仍能 final flush
func TestLayer_RegisterAfterStart(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	l := NewLayer(kv, WithLayerConfig(testConfig()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sn := newRecSaver()
	c, err := Register[string, testVal](l, "late",
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](sn),
		WithWriteBehind[string, testVal](time.Hour, 10),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, "k1", &testVal{N: 9}); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := l.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if got := sn.get("k1"); got == nil || got.N != 9 {
		t.Fatal("启动后注册的 Cache 也应被 Stop 落库")
	}
}

func TestLayer_NilSafe(t *testing.T) {
	var l *Layer
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	l2 := NewLayer(nil)
	if _, err := Register[string, testVal](l2, "x"); err == nil {
		t.Fatal("kv 为空应报错")
	}
}
