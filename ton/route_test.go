package ton

import (
	"context"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
)

type stubChannel struct {
	closed bool
}

func (s *stubChannel) Withdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return &pay.PayResult{}, nil
}
func (s *stubChannel) QueryOrder(context.Context, string) (*pay.PayResult, error) {
	return &pay.PayResult{}, nil
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

// TestKindTONValue 配置段名、注入钩子键与台账链维度都从 Kind 派生，值不能漂移。
func TestKindTONValue(t *testing.T) {
	if string(pay.KindTON) != "ton.native" {
		t.Fatalf("pay.KindTON=%q，期望 ton.native", pay.KindTON)
	}
}

// TestTonRouteBindsUnifiedChannel 装配成功必须同时登记 Transfer 与 Channel 两副面孔。
func TestTonRouteBindsUnifiedChannel(t *testing.T) {
	t.Cleanup(func() { pay.Unbind(pay.KindTON) })
	ch := &stubChannel{}
	r := &route{
		label: "TON", channel: "native", callback: "TonMsg", currency: pay.CurrencyTON,
		kind: pay.KindTON,
		newClient: func(*config.Cfg) (*client, bool) {
			return &client{ch: ch, network: pay.NetworkMainnet}, true
		},
	}
	r.start(corectx.WithCfg(context.Background(), &config.Cfg{}))
	t.Cleanup(r.stop)

	got, err := pay.TransferFor(pay.KindTON)
	if err != nil || got != pay.Transfer(ch) {
		t.Fatalf("分派表没有落到新通道: %v", err)
	}
	chFor, err := pay.ChannelFor(pay.KindTON)
	if err != nil || chFor != pay.Channel(ch) {
		t.Fatalf("统一调用面没有落到同一条通道: ch=%v err=%v", chFor, err)
	}
}

// TestTonRouteRestartClosesOldClient Start 重复执行时旧 client 必须在新 client
// 接管分派之后被关掉；否则连接池与台账引用会随重启次数泄漏。
func TestTonRouteRestartClosesOldClient(t *testing.T) {
	t.Cleanup(func() { pay.Unbind(pay.KindTON) })
	oldCh, newCh := &stubChannel{}, &stubChannel{}
	call := 0
	r := &route{
		label: "TON", channel: "native", callback: "TonMsg", currency: pay.CurrencyTON,
		kind: pay.KindTON,
		newClient: func(*config.Cfg) (*client, bool) {
			call++
			if call == 1 {
				return &client{ch: oldCh, network: pay.NetworkMainnet}, true
			}
			return &client{ch: newCh, network: pay.NetworkMainnet}, true
		},
	}
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	r.start(ctx)
	r.start(ctx)
	t.Cleanup(r.stop)

	if !oldCh.closed {
		t.Error("第二次 Start 没有关闭旧 client")
	}
	if newCh.closed {
		t.Error("第二次 Start 关闭了当前 client")
	}
	chFor, err := pay.ChannelFor(pay.KindTON)
	if err != nil || chFor != pay.Channel(newCh) {
		t.Fatalf("分派表没有切到新通道: ch=%v err=%v", chFor, err)
	}
}

// TestTonRouteFailedRestartKeepsOldClient 新 client 装配失败时旧通道继续服务，
// 不能先摘掉旧 client 再把启动失败暴露给调用方。
func TestTonRouteFailedRestartKeepsOldClient(t *testing.T) {
	t.Cleanup(func() { pay.Unbind(pay.KindTON) })
	oldCh := &stubChannel{}
	call := 0
	r := &route{
		label: "TON", channel: "native", callback: "TonMsg", currency: pay.CurrencyTON,
		kind: pay.KindTON,
		newClient: func(*config.Cfg) (*client, bool) {
			call++
			if call == 1 {
				return &client{ch: oldCh, network: pay.NetworkMainnet}, true
			}
			return nil, false
		},
	}
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	r.start(ctx)
	r.start(ctx)
	t.Cleanup(r.stop)

	if oldCh.closed {
		t.Fatal("装配失败的重启关闭了仍在服务的旧 client")
	}
	if chFor, err := pay.ChannelFor(pay.KindTON); err != nil || chFor != pay.Channel(oldCh) {
		t.Fatalf("装配失败后分派表丢了旧通道: ch=%v err=%v", chFor, err)
	}
}
