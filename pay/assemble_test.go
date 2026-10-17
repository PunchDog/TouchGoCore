package pay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/util"
)

const (
	section    = "supplier_a"
	account    = "default"
	secret     = "TEST_SDK_SECRET"
	merchantID = "MCH-0001"
)

// spyChannel 是注册表里的替身驱动：只负责把「装配出来的选项」原样交回来，
// 一次 HTTP 都不发。真实供应商链路（签名、报文、包络）在 provider 与三条通道包里测。
type spyChannel struct {
	opt ProviderOptions
}

func (s *spyChannel) Driver() string     { return s.opt.Driver }
func (s *spyChannel) MerchantID() string { return s.opt.MerchantID }

func (s *spyChannel) Withdraw(context.Context, *PayOrder) (*PayResult, error) {
	return nil, errors.New("替身驱动不发起请求")
}

func (s *spyChannel) QueryOrder(context.Context, string) (*PayResult, error) {
	return nil, errors.New("替身驱动不发起请求")
}

func (s *spyChannel) QueryAccount(context.Context, *AccountQuery) (*AccountInfo, error) {
	return nil, errors.New("替身驱动不发起请求")
}

// spySeq 让每次登记都有一个没被用过的名字。
//
// 驱动登记表是进程级全局且拒绝重名覆盖（那是设计，见 channel.go 的 Register），
// 所以固定名字的替身驱动在 `go test -count=N`（N>1）下第二轮就会被自己的重名拒掉，
// 表现为「用例红」但被测代码没问题。加序号后本包连跑三轮可复现。
// 链规则登记表（chainrule.go 的 RegisterChainRule）同此口径，chainrule_test.go 与
// channel_test.go 的注册表用例共用这一个计数器。
var spySeq atomic.Uint64

// registerSpy 登记一个唯一命名的替身驱动，返回驱动名与最后一次装配看到的选项。
func registerSpy(t *testing.T) (string, *spyChannel) {
	t.Helper()
	name := fmt.Sprintf("spy_%s_%d", t.Name(), spySeq.Add(1))
	spy := &spyChannel{}
	if err := Register(name, func(opt ProviderOptions) (Channel, error) {
		*spy = spyChannel{opt: opt}
		return spy, nil
	}); err != nil {
		t.Fatal(err)
	}
	return name, spy
}

// sdkCfg 组一份「pay_sdks 登记一段 + 通道段引用它」的最小配置。
func sdkCfg(driver string, accounts map[string]*config.PayMerchantAccount) *config.Cfg {
	return &config.Cfg{
		PaySDks: map[string]*config.PaySDKConfig{
			section: {
				Enable: "on", Driver: driver, BaseURL: "https://supplier.example",
				AppID: "app1", SecretKey: secret, TimeoutSec: 30, MaxRetries: 2,
				NotifyURL: "https://example.com/cb", Accounts: accounts,
			},
		},
	}
}

func oneAccount() map[string]*config.PayMerchantAccount {
	return map[string]*config.PayMerchantAccount{
		account: {Enable: "on", MerchantID: merchantID},
	}
}

func ref() *config.PaySDKRef { return &config.PaySDKRef{SDK: section, Account: account} }

// TestOpenRoutesByDriver 装配结果必须逐字段来自被引用的 SDK 段：
// 通道包只给引用，配置表里的任何一项漏转，到了 pay 侧都是「看起来配了、实际用默认值」。
func TestOpenRoutesByDriver(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := sdkCfg(driver, oneAccount())
	res, err := Open(cfg, "usdt", ref(), []ExtraField{{Name: "contract", Value: "TR7"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Section != section || res.Driver != driver || res.MerchantID != merchantID {
		t.Fatalf("装配结果不符: %+v", res)
	}
	opt := spy.opt
	if opt.Name != "usdt" || opt.BaseURL != "https://supplier.example" || opt.AppID != "app1" ||
		opt.SecretKey != secret || opt.NotifyURL != "https://example.com/cb" ||
		opt.MaxRetry != 2 || opt.Timeout != 30*time.Second {
		// 只列缺项，不整体打印选项：里面就有密钥。
		t.Fatalf("SDK 段字段未完整交给驱动: name=%s base_url=%s app_id=%s max_retry=%d timeout=%v",
			opt.Name, opt.BaseURL, opt.AppID, opt.MaxRetry, opt.Timeout)
	}
	if len(opt.Extras) != 1 || opt.Extras[0].Name != "contract" {
		t.Fatalf("通道特有字段未透传: %+v", opt.Extras)
	}
	if path, ok := opt.Endpoint(EndpointQuery); ok {
		t.Fatalf("端点表为空时不应取到路径，实得 %q", path)
	}
}

// TestOpenInjectsSecretBeforeConstruct 密钥钩子按 SDK 段名触发，且发生在构造之前：
// pay 侧见到空密钥会直接拒绝启动，晚一步注入这条链路就永远起不来。
func TestOpenInjectsSecretBeforeConstruct(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := sdkCfg(driver, oneAccount())
	cfg.PaySDks[section].SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+section, func(sk *string) { *sk = secret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+section, id)

	if _, err := Open(cfg, "usdt", ref(), nil); err != nil {
		t.Fatalf("钩子注入密钥后应当装配成功: %v", err)
	}
	if spy.opt.SecretKey != secret {
		t.Fatal("驱动拿到的密钥不是钩子注入的那一份")
	}
}

// TestOpenNeedsAccountButServiceDoesNot 出款链路必须点到一个可用商户账户；
// 只做验证码下发/登录的链路没有账户也要能用，否则配置里就会被塞进一个假商户号。
func TestOpenNeedsAccountButServiceDoesNot(t *testing.T) {
	driver, spy := registerSpy(t)
	noAccounts := sdkCfg(driver, nil)
	if _, err := Open(noAccounts, "usdt", ref(), nil); err == nil {
		t.Fatal("没有商户账户的出款链路应当拒绝装配")
	}
	res, err := OpenService(noAccounts, "whatsapp.login", &config.PaySDKRef{SDK: section})
	if err != nil {
		t.Fatalf("无账户的下发链路应当可装配: %v", err)
	}
	if res.MerchantID != "" {
		t.Fatalf("无账户时下发了商户号: %+v", res)
	}
	// 一旦配了账户，歧义规则与资金链路一致：多个账户必须点名。
	multi := sdkCfg(driver, map[string]*config.PayMerchantAccount{
		"default": {Enable: "on", MerchantID: "M1"},
		"reserve": {Enable: "on", MerchantID: "M2"},
	})
	if _, err := OpenService(multi, "whatsapp.login", &config.PaySDKRef{SDK: section}); err == nil {
		t.Fatal("多账户未点名应当拒绝装配")
	}
	if _, err := OpenService(multi, "whatsapp.login", &config.PaySDKRef{SDK: section, Account: "reserve"}); err != nil {
		t.Fatalf("点名后应当可装配: %v", err)
	}
	if spy.opt.MerchantID != "M2" {
		t.Fatalf("点名的账户没生效: %+v", spy.opt.MerchantID)
	}
}

// TestOpenFailClosedAndQuiet 每一类引用错误都要报错，且报错文案里不出现密钥：
// 错误会被上层记进日志，配置错不该顺带把凭证广播出去。
func TestOpenFailClosedAndQuiet(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Cfg
		ref  *config.PaySDKRef
	}{
		{"没有配置", nil, ref()},
		{"引用为空", sdkCfg(DriverGeneric, oneAccount()), &config.PaySDKRef{}},
		{"段不存在", sdkCfg(DriverGeneric, oneAccount()), &config.PaySDKRef{SDK: "ghost"}},
		{"段没开", func() *config.Cfg {
			c := sdkCfg(DriverGeneric, oneAccount())
			c.PaySDks[section].Enable = "off"
			return c
		}(), ref()},
		{"缺驱动标记", func() *config.Cfg {
			c := sdkCfg(DriverGeneric, oneAccount())
			c.PaySDks[section].Driver = ""
			return c
		}(), ref()},
		{"驱动未登记", sdkCfg("no_such_driver", oneAccount()), ref()},
		{"空密钥且无人注入", func() *config.Cfg {
			c := sdkCfg(DriverGeneric, oneAccount())
			c.PaySDks[section].SecretKey = ""
			return c
		}(), ref()},
		{"账户停用", func() *config.Cfg {
			c := sdkCfg(DriverGeneric, oneAccount())
			c.PaySDks[section].Accounts[account].Enable = "off"
			return c
		}(), ref()},
	}
	for _, cs := range cases {
		res, err := Open(cs.cfg, "usdt", cs.ref, nil)
		if err == nil || res != nil {
			t.Errorf("%s 应当拒绝装配，实得 res=%v err=%v", cs.name, res, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s 的报错泄漏了密钥: %v", cs.name, err)
		}
		if !strings.Contains(err.Error(), "usdt") {
			t.Errorf("%s 的报错没指出是哪条通道: %v", cs.name, err)
		}
	}
}

// TestSecondsLeavesDefaultsToPay 超时留空时交出 0，由 pay 用它的默认值：
// 这里再立一套默认秒数，两处默认迟早会对不上。
func TestSecondsLeavesDefaultsToPay(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := sdkCfg(driver, oneAccount())
	cfg.PaySDks[section].TimeoutSec = 0
	if _, err := Open(cfg, "usdt", ref(), nil); err != nil {
		t.Fatal(err)
	}
	if spy.opt.Timeout != 0 {
		t.Fatalf("超时应按配置留空交给 pay，实得 %v", spy.opt.Timeout)
	}
}

// chainCfg 组一份「SDK 段带链上接入点」的配置。
func chainCfg(driver string) *config.Cfg {
	cfg := sdkCfg(driver, oneAccount())
	cfg.PaySDks[section].Chain = &config.PayChainConfig{
		Node: "grpc.trongrid.io:50051", HTTPBase: "https://api.trongrid.io",
		APIKey: "TEST_API_KEY", WalletAddress: "TWalletAddr", PrivateKey: "TEST_PRIVATE_KEY",
		FeeLimit: 65000000, TimeoutSec: 20, MaxRetries: 3,
	}
	return cfg
}

const chainParams = "ChainParams{Contract: \"TR7\", Network: \"mainnet\"}"

// TestOpenChainMergesBothSides 链上选项必须是「段侧凭证 + 通道侧资产」两半合成：
// 段侧字段一个都不转，驱动就只能对着空节点空地址装配；通道侧字段丢了，
// 广播用的合约就不是通道校验过的那一个。
func TestOpenChainMergesBothSides(t *testing.T) {
	driver, spy := registerSpy(t)
	res, err := OpenChain(chainCfg(driver), "usdt", ref(),
		ChainParams{Contract: "TR7", Network: "mainnet"}, []ExtraField{{Name: "contract", Value: "TR7"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Section != section || res.Driver != driver {
		t.Fatalf("装配结果不符: %+v", res)
	}
	c := spy.opt.Chain
	if c == nil {
		t.Fatal("链上通道没拿到 chain 选项")
	}
	if c.Node != "grpc.trongrid.io:50051" || c.HTTPBase != "https://api.trongrid.io" ||
		c.APIKey != "TEST_API_KEY" || c.WalletAddress != "TWalletAddr" || c.PrivateKey != "TEST_PRIVATE_KEY" ||
		c.FeeLimitSun != 65000000 || c.Timeout != 20*time.Second || c.MaxRetry != 3 {
		t.Fatalf("chain 子段未完整交给驱动: node=%s http_base=%s wallet=%s fee_limit=%d timeout=%v retry=%d",
			c.Node, c.HTTPBase, c.WalletAddress, c.FeeLimitSun, c.Timeout, c.MaxRetry)
	}
	if c.Contract != "TR7" || c.Network != "mainnet" {
		t.Fatalf("通道侧资产维度未合进来: contract=%s network=%s", c.Contract, c.Network)
	}
	// 换驱动不该顺手改掉供应商侧已经在发的字段：extras 照旧透传。
	if len(spy.opt.Extras) != 1 || spy.opt.Extras[0].Value != "TR7" {
		t.Fatalf("extras 未透传，报文会少一个已签名量: %+v", spy.opt.Extras)
	}
}

// TestOpenChainInjectsPrivateKeyBeforeConstruct 私钥按段名过一次独立钩子，且发生在构造之前。
//
// 两个钩子必须分开触发：签名密钥与热钱包私钥是两份凭证，合成一格就会有人把
// 「供应商签名密钥」当成私钥注进去，而这要到第一笔出款签名时才暴露。
func TestOpenChainInjectsPrivateKeyBeforeConstruct(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := chainCfg(driver)
	cfg.PaySDks[section].Chain.PrivateKey = ""
	cfg.PaySDks[section].SecretKey = ""
	sdkID := util.DefaultCallFunc.Register(util.CallPaySDKMsg+section, func(sk *string) { *sk = secret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+section, sdkID)
	chainID := util.DefaultCallFunc.Register(util.CallPayChainMsg+section, func(pk *string) { *pk = "INJECTED_PK" })
	defer util.DefaultCallFunc.Unregister(util.CallPayChainMsg+section, chainID)

	if _, err := OpenChain(cfg, "usdt", ref(), ChainParams{Contract: "TR7", Network: "mainnet"}, nil); err != nil {
		t.Fatalf("两份凭证都该由钩子注入后装配成功: %v", err)
	}
	if spy.opt.SecretKey != secret {
		t.Fatal("签名密钥钩子没生效")
	}
	if spy.opt.Chain == nil || spy.opt.Chain.PrivateKey != "INJECTED_PK" {
		t.Fatal("私钥钩子注入的值没交给驱动")
	}
	if cfg.PaySDks[section].Chain.PrivateKey != "INJECTED_PK" {
		t.Fatal("钩子应当写回配置对象本身，而不是只改了副本")
	}
}

// TestOpenChainNeedsChainSection 通道声明了链上出款而段里没配 chain 子段必须拒绝装配：
// 静默回落成供应商链路等于「配置写着直接出款、实际把单发给别人」。
func TestOpenChainNeedsChainSection(t *testing.T) {
	driver, _ := registerSpy(t)
	res, err := OpenChain(sdkCfg(driver, oneAccount()), "usdt", ref(), ChainParams{Contract: "TR7"}, nil)
	if err == nil || res != nil {
		t.Fatalf("缺 chain 子段应当拒绝装配，实得 res=%v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "chain") || !strings.Contains(err.Error(), "usdt") {
		t.Fatalf("报错要指明缺的是 chain 子段与哪条通道: %v", err)
	}
}

// TestOpenIgnoresChainSection 没走 OpenChain 的通道拿不到链上选项，即使段里配了 chain：
// 同一段凭证可以被多条通道引用，其中一条只用于供应商侧——按段配置不等于按段强制上链。
func TestOpenIgnoresChainSection(t *testing.T) {
	driver, spy := registerSpy(t)
	if _, err := Open(chainCfg(driver), "usdt", ref(), nil); err != nil {
		t.Fatal(err)
	}
	if spy.opt.Chain != nil {
		t.Fatal("Open 不该把 chain 子段塞进供应商通道的选项")
	}
}

// TestChainTLSDefaultsOn 只有显式 "off" 才降级为明文：公共接入点要求 TLS，
// 让留空读成「明文」就是把热钱包签名链路挂到明文上的默认路径。
func TestChainTLSDefaultsOn(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := chainCfg(driver)
	if _, err := OpenChain(cfg, "usdt", ref(), ChainParams{Contract: "TR7"}, nil); err != nil {
		t.Fatal(err)
	}
	if spy.opt.Chain.PlaintextNode {
		t.Fatal("tls 留空却走了明文")
	}
	cfg.PaySDks[section].Chain.TLS = "off"
	if _, err := OpenChain(cfg, "usdt", ref(), ChainParams{Contract: "TR7"}, nil); err != nil {
		t.Fatal(err)
	}
	if !spy.opt.Chain.PlaintextNode {
		t.Fatal(`tls="off" 没有落成明文`)
	}
}

// TestOpenChainFailQuiet 链上装配的报错不得带出私钥与 API Key：
// 通道包会把装配错误原样记进启动日志。
func TestOpenChainFailQuiet(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"段不存在", func() *config.Cfg {
			c := chainCfg("ghost_driver")
			c.PaySDks[section].Driver = ""
			return c
		}()},
		{"驱动未登记", sdkCfg("no_such_driver", oneAccount())},
	}
	for _, cs := range cases {
		_, err := OpenChain(cs.cfg, "usdt", ref(), ChainParams{Contract: "TR7"}, nil)
		if err == nil {
			t.Errorf("%s 应当拒绝装配", cs.name)
			continue
		}
		for _, leak := range []string{"TEST_PRIVATE_KEY", "TEST_API_KEY", secret} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s 的报错泄漏了凭证: %v", cs.name, err)
			}
		}
	}
}
