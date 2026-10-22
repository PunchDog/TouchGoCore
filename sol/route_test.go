package sol

import (
	"context"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"
)

// 本文件钉两件事：装配期「配置不全就静默不启动」，以及装配成功后
// 「分派表里找得到这一条」。后者是出款真正能被路由到的前提——
// 通道起来了但没登记，等于热钱包有私钥却没人把单交给它。

// TestNewSolUSDTClientRejectsIncomplete 缺任何一项都返回 ok=false，且不碰网络。
//
// 每一条都对应一种「带着未知上线」的形态；它们共同的特征是第一笔出款才暴露，
// 而那时钱已经开始动。装配期拒掉是唯一来得及的位置。
func TestNewSolUSDTClientRejectsIncomplete(t *testing.T) {
	wallet := solTestKey(t).PublicKey().String()
	good := func() *config.SolUsdtConfig {
		return &config.SolUsdtConfig{
			Enable:   "on",
			Driver:   driverSolRPC,
			Network:  "mainnet-beta",
			Contract: solBytes1To32,
			Decimals: 6,
			Chain:    &config.SolChainConfig{RPC: "https://example.invalid", WalletAddress: wallet},
		}
	}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"nil 配置", nil},
		{"缺 sol 段", &config.Cfg{}},
		{"缺 sol.usdt 段", &config.Cfg{Sol: &config.SolConfig{}}},
		{"enable 不是 on", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Enable = "off"
			return u
		}()}}},
		{"驱动名不符", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Driver = "some_other_driver"
			return u
		}()}}},
		{"mint 为空", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Contract = "  "
			return u
		}()}}},
		{"mint 非法", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Contract = "not-an-address"
			return u
		}()}}},
		{"网络为空", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Network = ""
			return u
		}()}}},
		{"缺 chain 段", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Chain = nil
			return u
		}()}}},
	}
	for _, cs := range cases {
		c, ok := newSolUSDTClient(cs.cfg)
		if ok {
			t.Errorf("%s: 装配成功了（应当拒绝）", cs.name)
			if c != nil {
				_ = c.ch.Close()
			}
		}
		if c != nil {
			t.Errorf("%s: 拒绝时仍返回了非 nil 客户端", cs.name)
		}
	}
}

// stubChannel 是本文件的假通道：只用来验证分派登记，不实现任何链上行为。
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
func (s *stubChannel) Close() error {
	s.closed = true
	return nil
}

// TestSolRouteBindsKind 装配成功必须登记分派，停服必须摘掉并关连接。
//
// 顺序同样是断言的一部分：先摘分派再关连接。反过来的话，正在分派的那一单
// 会拿到一条正在关的通道，报出来的是「连接已关闭」而不是「通道未启动」。
func TestSolRouteBindsKind(t *testing.T) {
	t.Cleanup(func() { pay.Unbind(pay.KindSolUSDT) })
	ch := &stubChannel{}
	cl := &client{ch: ch, network: "mainnet-beta"}
	// 与 start() 同序：先存快照再登记分派。stop 只会关掉快照里的那一条。
	solRoute.active.Store(cl)
	solRoute.bind(cl)

	got, err := pay.TransferFor(pay.KindSolUSDT)
	if err != nil {
		t.Fatalf("装配成功后分派表里找不到 %s: %v", pay.KindSolUSDT, err)
	}
	if got != pay.Transfer(ch) {
		t.Errorf("分派到的通道不是刚装的那一个")
	}

	solRoute.stop()
	if _, err := pay.TransferFor(pay.KindSolUSDT); err == nil {
		t.Error("停服后分派表里仍然找得到这条通道")
	}
	if !ch.closed {
		t.Error("停服时没有关掉接入点连接")
	}
}

// TestRouteOrderFillsDefaultsWithoutMutating 门面补缺省值时必须深拷贝 To。
//
// 调用方交来的 PayOrder 可能还要用于重试与落库：就地改它，等于替上游决定了
// 「这一单实际发出去的是什么」，而那条记录与真的发出去的东西已经不是一回事。
func TestRouteOrderFillsDefaultsWithoutMutating(t *testing.T) {
	c := &client{network: "mainnet-beta"}
	o := &pay.PayOrder{OrderNo: "SOL-O1", Amount: 1, To: &pay.Party{Address: solBytes1To32}}
	got := solRoute.order(c, o)
	if got.To.Network != "mainnet-beta" {
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

	// 已经写明的网络/币种不得被覆盖。
	explicit := &pay.PayOrder{To: &pay.Party{Address: solBytes1To32, Network: "devnet"}, Currency: pay.CurrencyUSDT}
	if got := solRoute.order(c, explicit); got.To.Network != "devnet" {
		t.Errorf("显式网络被改成 %q", got.To.Network)
	}

	// To 为空时不能凭空造一个收款方：留给 pay.CheckOrder 拒。
	empty := &pay.PayOrder{OrderNo: "SOL-O2"}
	got = solRoute.order(c, empty)
	if got.To != nil {
		t.Error("缺 To 的订单被补出了一个收款方")
	}
	if got.Currency != pay.CurrencyUSDT {
		t.Errorf("缺 To 时币种仍未补缺省值: %q", got.Currency)
	}
	if solRoute.order(c, nil) != nil {
		t.Error("nil 订单应原样返回 nil")
	}

	// 各字段都对上时不必复制（省一次分配，且保持调用方观察到的指针一致）。
	same := &pay.PayOrder{To: &pay.Party{Address: solBytes1To32, Network: "mainnet-beta"}, Currency: pay.CurrencyUSDT}
	if got := solRoute.order(c, same); got != same {
		t.Error("无需补缺省值时仍复制了订单")
	}
}

// TestNamespaceKeys 注入键名与分派种类同源：改一处不会漏掉另一处。
func TestNamespaceKeys(t *testing.T) {
	if solUSDTKey != string(pay.KindSolUSDT) {
		t.Errorf("注入键 %q 与分派种类 %q 不同源", solUSDTKey, pay.KindSolUSDT)
	}
	if !strings.Contains(string(pay.KindSolUSDT), "sol") {
		t.Errorf("分派种类 %q 里读不出链名", pay.KindSolUSDT)
	}
}
