package chainroute

import (
	"context"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
)

// 本文件是 Route 门面机器的唯一一份行为基线：装配登记、重启关旧、停服释放、
// 订单补全、未启动拒绝、三扇门面的收尾口径。四条链共用这台机器，所以这些断言
// 只在这里写一遍——各链的 route_test 只钉「接线正确性」（读对配置段、绑对 Kind）。

// stubChannel 是假通道：只用来验证门面机器，不实现任何链上行为。
type stubChannel struct {
	closed bool
}

func (s *stubChannel) Withdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return &pay.PayResult{Status: pay.StatusPending}, nil
}
func (s *stubChannel) QueryOrder(context.Context, string) (*pay.PayResult, error) {
	return &pay.PayResult{Status: pay.StatusUnknown}, nil
}
func (s *stubChannel) QueryAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return &pay.AccountInfo{}, nil
}
func (s *stubChannel) QueryOnchainAccount(context.Context, *pay.OnchainAccountQuery) (*pay.AccountInfo, error) {
	return &pay.AccountInfo{}, nil
}
func (s *stubChannel) Close() error {
	s.closed = true
	return nil
}

// testRoute 造一台挂在专属 Kind 上的门面，避免污染真实分派表。
func testRoute(kind pay.Kind, newClient func(*config.Cfg) (*Snapshot, bool)) *Route {
	return &Route{
		Label:          "TEST",
		ConfigKey:      "test.seg",
		NotStartedHint: "enable / chain 是否齐备",
		Callback:       "TestMsg",
		Currency:       pay.CurrencyUSDT,
		Kind:           kind,
		NewClient:      newClient,
	}
}

// TestRouteBindsUnifiedChannel 装配成功必须同时登记 Transfer 与 Channel 两副面孔，
// 停服必须摘掉并关连接。
func TestRouteBindsUnifiedChannel(t *testing.T) {
	const kind = pay.Kind("chainroute.test.binds")
	t.Cleanup(func() { pay.Unbind(kind) })
	ch := &stubChannel{}
	r := testRoute(kind, func(*config.Cfg) (*Snapshot, bool) {
		return &Snapshot{Ch: ch, Network: "mainnet"}, true
	})
	r.Start(corectx.WithCfg(context.Background(), &config.Cfg{}))
	t.Cleanup(r.Stop)

	got, err := pay.TransferFor(kind)
	if err != nil || got != pay.Transfer(ch) {
		t.Fatalf("分派表没有落到新通道: %v", err)
	}
	if chFor, err := pay.ChannelFor(kind); err != nil || chFor != pay.Channel(ch) {
		t.Fatalf("统一调用面没有落到同一条通道: ch=%v err=%v", chFor, err)
	}
}

// TestRouteStopUnbindsAndCloses 停服先摘分派再关连接。
func TestRouteStopUnbindsAndCloses(t *testing.T) {
	const kind = pay.Kind("chainroute.test.stop")
	t.Cleanup(func() { pay.Unbind(kind) })
	ch := &stubChannel{}
	r := testRoute(kind, func(*config.Cfg) (*Snapshot, bool) {
		return &Snapshot{Ch: ch, Network: "mainnet"}, true
	})
	r.Start(corectx.WithCfg(context.Background(), &config.Cfg{}))
	r.Stop()
	if _, err := pay.TransferFor(kind); err == nil {
		t.Error("停服后分派表里仍然找得到这条通道")
	}
	if !ch.closed {
		t.Error("停服时没有关掉接入点连接")
	}
}

// TestRouteRestartClosesOldClient Start 重复执行时旧 client 必须在新 client
// 接管分派之后被关掉；否则连接池与台账引用会随重启次数泄漏。
func TestRouteRestartClosesOldClient(t *testing.T) {
	const kind = pay.Kind("chainroute.test.restart")
	t.Cleanup(func() { pay.Unbind(kind) })
	oldCh, newCh := &stubChannel{}, &stubChannel{}
	call := 0
	r := testRoute(kind, func(*config.Cfg) (*Snapshot, bool) {
		call++
		if call == 1 {
			return &Snapshot{Ch: oldCh, Network: "mainnet"}, true
		}
		return &Snapshot{Ch: newCh, Network: "mainnet"}, true
	})
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	r.Start(ctx)
	r.Start(ctx)
	t.Cleanup(r.Stop)

	if !oldCh.closed {
		t.Error("第二次 Start 没有关闭旧 client")
	}
	if newCh.closed {
		t.Error("第二次 Start 关闭了当前 client")
	}
	if chFor, err := pay.ChannelFor(kind); err != nil || chFor != pay.Channel(newCh) {
		t.Fatalf("分派表没有切到新通道: ch=%v err=%v", chFor, err)
	}
}

// TestRouteFailedRestartKeepsOldClient 新 client 装配失败时旧通道继续服务，
// 不能先摘掉旧 client 再把启动失败暴露给调用方。
func TestRouteFailedRestartKeepsOldClient(t *testing.T) {
	const kind = pay.Kind("chainroute.test.failedrestart")
	t.Cleanup(func() { pay.Unbind(kind) })
	oldCh := &stubChannel{}
	call := 0
	r := testRoute(kind, func(*config.Cfg) (*Snapshot, bool) {
		call++
		if call == 1 {
			return &Snapshot{Ch: oldCh, Network: "mainnet"}, true
		}
		return nil, false
	})
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	r.Start(ctx)
	r.Start(ctx)
	t.Cleanup(r.Stop)

	if oldCh.closed {
		t.Fatal("装配失败的重启关闭了仍在服务的旧 client")
	}
	if chFor, err := pay.ChannelFor(kind); err != nil || chFor != pay.Channel(oldCh) {
		t.Fatalf("装配失败后分派表丢了旧通道: ch=%v err=%v", chFor, err)
	}
}

// TestRouteNotStartedRefuses 未启动时三扇门面都就地拒绝，错误里点明配置段与检查项。
func TestRouteNotStartedRefuses(t *testing.T) {
	r := testRoute(pay.Kind("chainroute.test.notstarted"), nil)
	if _, err := r.Withdraw(context.Background(), &pay.PayOrder{OrderNo: "O1"}); err == nil ||
		!strings.Contains(err.Error(), "TEST 通道未启动") ||
		!strings.Contains(err.Error(), "test.seg") ||
		!strings.Contains(err.Error(), "enable / chain 是否齐备") {
		t.Errorf("Withdraw 未启动拒绝口径不符: %v", err)
	}
	if _, err := r.Query(context.Background(), "O1"); err == nil || !strings.Contains(err.Error(), "通道未启动") {
		t.Errorf("Query 未启动拒绝口径不符: %v", err)
	}
	if _, err := r.Account(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "通道未启动") {
		t.Errorf("Account 未启动拒绝口径不符: %v", err)
	}
}

// TestRouteQueryRejectsEmptyOrderNo 空单号在取通道之前就被拒，不发出请求。
func TestRouteQueryRejectsEmptyOrderNo(t *testing.T) {
	r := testRoute(pay.Kind("chainroute.test.emptyorder"), nil)
	_, err := r.Query(context.Background(), "   ")
	if err == nil || !strings.Contains(err.Error(), "订单号（幂等键）为空") {
		t.Errorf("空单号未被拒或文案不符: %v", err)
	}
}

// TestRouteOrderFillsDefaultsWithoutMutating 门面补缺省值时必须深拷贝 To，
// 且不覆盖调用方显式给的值。
func TestRouteOrderFillsDefaultsWithoutMutating(t *testing.T) {
	r := testRoute(pay.Kind("chainroute.test.order"), nil)
	c := &Snapshot{Network: "mainnet"}
	o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: "addr"}}
	got := r.order(c, o)
	if got.To.Network != "mainnet" {
		t.Errorf("To.Network 未补成通道网络: %q", got.To.Network)
	}
	if got.Currency != pay.CurrencyUSDT {
		t.Errorf("Currency 未补成 %s: %q", pay.CurrencyUSDT, got.Currency)
	}
	if o.To.Network != "" || o.Currency != "" {
		t.Error("门面就地改了调用方的订单")
	}
	if got == o || got.To == o.To {
		t.Error("补缺省值时应当深拷贝，而不是复用同一个指针")
	}

	explicit := &pay.PayOrder{To: &pay.Party{Address: "addr", Network: "chapel"}, Currency: pay.CurrencyTON}
	if got := r.order(c, explicit); got.To.Network != "chapel" || got.Currency != pay.CurrencyTON {
		t.Errorf("显式网络/币种被改: net=%q cur=%q", got.To.Network, got.Currency)
	}

	empty := &pay.PayOrder{OrderNo: "O2"}
	got = r.order(c, empty)
	if got.To != nil {
		t.Error("缺 To 的订单被补出了一个收款方")
	}
	if got.Currency != pay.CurrencyUSDT {
		t.Errorf("缺 To 时币种仍未补缺省值: %q", got.Currency)
	}
	if r.order(c, nil) != nil {
		t.Error("nil 订单应原样返回 nil")
	}

	same := &pay.PayOrder{To: &pay.Party{Address: "addr", Network: "mainnet"}, Currency: pay.CurrencyUSDT}
	if got := r.order(c, same); got != same {
		t.Error("无需补缺省值时仍复制了订单")
	}
}

// TestRouteRunCtxFallback RunCtx 从未设过时回落 Background；SetRunCtx(nil) 同样归一。
func TestRouteRunCtxFallback(t *testing.T) {
	r := testRoute(pay.Kind("chainroute.test.runctx"), nil)
	if r.RunCtx() == nil {
		t.Fatal("RunCtx 回落不应为 nil")
	}
	r.SetRunCtx(nil)
	if r.RunCtx() == nil {
		t.Fatal("SetRunCtx(nil) 后 RunCtx 不应为 nil")
	}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	r.SetRunCtx(ctx)
	if r.RunCtx().Value(key{}) != "v" {
		t.Fatal("RunCtx 没有取回设进去的上下文")
	}
}
