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

// TestRegister_FieldOptionsKeepLayerNamespace 字段级选项只改它自己那一个字段。
//
// 事故形状：调用方为了「这一类缓存存久一点」给了 WithConfig(自己拼的整份 Config)，
// 于是 Layer 的 Enabled/KeyPrefix/Group 一起被换掉 —— 表现成「Layer 明明关了缓存，
// 这一类却仍然读 Redis」，而且读的是别的命名空间。要覆盖 TTL 就用 WithTTL /
// WithNegativeTTL；WithConfig 保留为显式的整体替换（测试里合法，生产里由下游静态闸禁掉）。
func TestRegister_FieldOptionsKeepLayerNamespace(t *testing.T) {
	layerCfg := testConfig()
	layerCfg.KeyPrefix = "game"
	layerCfg.Group = "s1"
	layerCfg.Enabled = false
	l := NewLayer(newFakeKV(newFakeClock()), WithLayerConfig(layerCfg))

	c, err := Register[string, testVal](l, "ttl_only",
		WithTTL[string, testVal](5*time.Minute),
		WithNegativeTTL[string, testVal](30*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.TTL != 5*time.Minute || c.cfg.NegativeTTL != 30*time.Second {
		t.Fatalf("TTL 覆盖没生效: ttl=%v neg=%v", c.cfg.TTL, c.cfg.NegativeTTL)
	}
	if c.cfg.KeyPrefix != "game" || c.cfg.Group != "s1" || c.cfg.Enabled {
		t.Fatalf("命名空间/开关被字段级选项连带改掉了: %+v", c.cfg)
	}

	// 反证：WithConfig 确实是整体替换（否则上面那条「没被改掉」的断言只是空转）。
	// 顺带记一笔它的另一层危险：整份给一份不完整的 Config 会连校验参数一起清零，
	// New 直接拒绝注册 —— 生产里 per-cache 覆盖就该走字段级选项。
	bad := testConfig()
	bad.KeyPrefix = "other"
	c2, err := Register[string, testVal](l, "whole",
		WithConfig[string, testVal](bad),
		WithTTL[string, testVal](time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c2.cfg.KeyPrefix != "other" || c2.cfg.Group != "" {
		t.Fatalf("WithConfig 应整份替换（Layer 给的 Group 也会丢）: %+v", c2.cfg)
	}
	if _, err := Register[string, testVal](l, "half",
		WithConfig[string, testVal](Config{KeyPrefix: "other"})); err == nil {
		t.Fatal("半份 Config 应因 LogicalPct=0 被拒——这条反证没立住")
	}
}
