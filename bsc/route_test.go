package bsc

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
//
// 与 sol 包的同名测试逐条对齐：两条新链的门面形状是同构的，
// 那么证明它们「接得进分派表」的证据也应当可以对着读。

// TestNewBscUSDTClientRejectsIncomplete 缺任何一项都返回 ok=false，且不碰网络。
//
// 每一条都对应一种「带着未知上线」的形态；它们共同的特征是第一笔出款才暴露，
// 而那时钱已经开始动。装配期拒掉是唯一来得及的位置。
//
// 最后一例是资金闸而不是缺项：配置写齐了但私钥没注入，同样拒绝。
// 签名者的存在与否不该由「配置看起来完整」来推断。
func TestNewBscUSDTClientRejectsIncomplete(t *testing.T) {
	good := func() *config.BscUsdtConfig {
		return &config.BscUsdtConfig{
			Enable:   "on",
			Driver:   driverBSCRPC,
			Network:  "mainnet",
			Contract: evmChecksumMixed,
			Decimals: 18,
			Chain: &config.BscChainConfig{
				RPC:           "https://example.invalid",
				WalletAddress: evmAllLower,
			},
		}
	}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"nil 配置", nil},
		{"缺 bsc 段", &config.Cfg{}},
		{"缺 bsc.usdt 段", &config.Cfg{Bsc: &config.BscConfig{}}},
		{"enable 不是 on", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Enable = "off"
			return u
		}()}}},
		{"驱动名不符", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Driver = "some_other_driver"
			return u
		}()}}},
		{"合约为空", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Contract = "  "
			return u
		}()}}},
		{"合约非法", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Contract = evmBogusChecksum
			return u
		}()}}},
		{"网络为空", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Network = ""
			return u
		}()}}},
		{"缺 chain 段", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Chain = nil
			return u
		}()}}},
		{"配置齐备但私钥未注入", &config.Cfg{Bsc: &config.BscConfig{Usdt: good()}}},
	}
	for _, cs := range cases {
		c, ok := newBscUSDTClient(cs.cfg)
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
func (s *stubChannel) QueryOnchainAccount(context.Context, *pay.OnchainAccountQuery) (*pay.AccountInfo, error) {
	return &pay.AccountInfo{}, nil
}
func (s *stubChannel) Close() error {
	s.closed = true
	return nil
}

// TestBscRouteBindsKind 装配成功必须登记分派，停服必须摘掉并关连接。
//
// 顺序同样是断言的一部分：先摘分派再关连接。反过来的话，正在分派的那一单
// 会拿到一条正在关的通道，报出来的是「连接已关闭」而不是「通道未启动」。
func TestBscRouteBindsKind(t *testing.T) {
	t.Cleanup(func() { pay.Unbind(pay.KindBscUSDT) })
	ch := &stubChannel{}
	cl := &client{ch: ch, network: "mainnet"}
	// 与 start() 同序：先存快照再登记分派。stop 只会关掉快照里的那一条。
	bscRoute.active.Store(cl)
	bscRoute.bind(cl)

	got, err := pay.TransferFor(pay.KindBscUSDT)
	if err != nil {
		t.Fatalf("装配成功后分派表里找不到 %s: %v", pay.KindBscUSDT, err)
	}
	if got != pay.Transfer(ch) {
		t.Errorf("分派到的通道不是刚装的那一个")
	}
	if chFor, err := pay.ChannelFor(pay.KindBscUSDT); err != nil || chFor != pay.Channel(ch) {
		t.Errorf("统一调用面没落在同一条通道上: ch=%v err=%v", chFor, err)
	}

	bscRoute.stop()
	if _, err := pay.TransferFor(pay.KindBscUSDT); err == nil {
		t.Error("停服后分派表里仍然找得到这条通道")
	}
	if !ch.closed {
		t.Error("停服时没有关掉接入点连接")
	}
}

// TestBscRouteOrderFillsDefaultsWithoutMutating 门面补缺省值时必须深拷贝 To。
//
// 调用方交来的 PayOrder 可能还要用于重试与落库：就地改它，等于替上游决定了
// 「这一单实际发出去的是什么」，而那条记录与真的发出去的东西已经不是一回事。
func TestBscRouteOrderFillsDefaultsWithoutMutating(t *testing.T) {
	c := &client{network: "mainnet"}
	o := &pay.PayOrder{OrderNo: "BSC-O1", Amount: 1, To: &pay.Party{Address: evmAllLower}}
	got := bscRoute.order(c, o)
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

	// 已经写明的网络/币种不得被覆盖。
	explicit := &pay.PayOrder{To: &pay.Party{Address: evmAllLower, Network: "chapel"}, Currency: pay.CurrencyUSDT}
	if got := bscRoute.order(c, explicit); got.To.Network != "chapel" {
		t.Errorf("显式网络被改成 %q", got.To.Network)
	}

	// To 为空时不能凭空造一个收款方：留给 pay.CheckOrder 拒。
	empty := &pay.PayOrder{OrderNo: "BSC-O2"}
	got = bscRoute.order(c, empty)
	if got.To != nil {
		t.Error("缺 To 的订单被补出了一个收款方")
	}
	if got.Currency != pay.CurrencyUSDT {
		t.Errorf("缺 To 时币种仍未补缺省值: %q", got.Currency)
	}
	if bscRoute.order(c, nil) != nil {
		t.Error("nil 订单应原样返回 nil")
	}

	// 各字段都对上时不必复制（省一次分配，且保持调用方观察到的指针一致）。
	same := &pay.PayOrder{To: &pay.Party{Address: evmAllLower, Network: "mainnet"}, Currency: pay.CurrencyUSDT}
	if got := bscRoute.order(c, same); got != same {
		t.Error("无需补缺省值时仍复制了订单")
	}
}

// TestBscNamespaceKeys 注入键名与分派种类同源：改一处不会漏掉另一处。
func TestBscNamespaceKeys(t *testing.T) {
	if bscUSDTKey != string(pay.KindBscUSDT) {
		t.Errorf("注入键 %q 与分派种类 %q 不同源", bscUSDTKey, pay.KindBscUSDT)
	}
	if !strings.Contains(string(pay.KindBscUSDT), "bsc") {
		t.Errorf("分派种类 %q 里读不出链名", pay.KindBscUSDT)
	}
}
