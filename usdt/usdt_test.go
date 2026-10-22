package usdt

import (
	"context"
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/tron"
	"touchgocore/util"
)

// testSecret 是测试内用的签名密钥，与真实凭证无关。
const testSecret = "TEST_SECRET_KEY"

// testAccount 是测试里的商户账户名字。
const (
	testAccount = "default"
	merchantID  = "MCH-TEST"
	// testContract 用官方 USDT-TRC20 合约：它是广播时真正的收端之一，
	// 编一个「看着像」的字符串会让本包的启动校验与全部出款用例都失去意义。
	testContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	testNetwork  = pay.NetworkMainnet
	// testPrivKey / testWallet 是一对测试私钥与它的派生地址（用 gotron-sdk 现算，非抄自网络）。
	testPrivKey = "1111111111111111111111111111111111111111111111111111111111111111"
	testWallet  = "TCLBgkbfVkJroVBJVqBEsxtPNQEQMTQCLQ"
)

// recorder 收集 publish 广播出去的回执。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallUsdtMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallUsdtMsg+kind, func(res *pay.PayResult) {
			recorder.mu.Lock()
			recorder.calls = append(recorder.calls, res)
			recorder.mu.Unlock()
		})
	}
}

func recorded() []*pay.PayResult {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	got := recorder.calls
	recorder.calls = nil
	return got
}

// payCfg 组一份完整的内联形态配置：供应商字段、币种事实、链上接入点都写在 tron.usdt 一段里。
//
// 测试必须走真实的装配入口而不是直接喂 options 结构，否则就测不到
// 「驱动名写错」「商户号点不出来」这一整类错误。
func payCfg(baseURL string, endpoints map[string]string) *config.Cfg {
	return &config.Cfg{
		Tron: &config.TronConfig{
			Usdt: &config.TronUsdtConfig{
				ChannelSupplierConfig: config.ChannelSupplierConfig{
					Enable: "on", Driver: tron.DriverTronGrid, BaseURL: baseURL,
					AppID: "app1", SecretKey: testSecret, Endpoints: endpoints,
					Accounts: map[string]*config.ChannelAccountConfig{
						testAccount: {Enable: "on", MerchantID: merchantID},
					},
				},
				Network:  testNetwork,
				Contract: testContract,
				// USDT 出款恒定直接上链，所以夹具必须带 chain 子段：
				// 缺它时装配会当场拒绝（那正是「配置写着上链、实际发给别人」的拦点）。
				// 私钥与地址是一对可公开的测试私钥派生结果，用 gotron-sdk 现算得到。
				Chain: &config.TronChainConfig{
					Node: "127.0.0.1:50051", TLS: "off",
					WalletAddress: testWallet, PrivateKey: testPrivKey,
				},
			},
		},
	}
}

// startWith 用给定配置走一遍真实启动路径（含密钥钩子与 SDK 引用解析）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	UsdtStop(nil)
	recorded()
	UsdtStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		UsdtStop(nil)
		recorded()
	})
}

// 装配与报文用例（引用解析矩阵、缺省补齐、签名域逐位对齐）跟着实现搬到了
// tron/usdt_test.go：newClient 与 client.order 已是本包不存在的符号，留在原地
// 只能靠门面间接断言，间接到测不出「引用指不通」这一整类错误。
// 这里只留门面级行为：未启动即拒、前置校验先于发请求。

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	UsdtStop(nil)
	recorded()
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: "a"}}); err == nil {
		t.Error("未启动时提现应当被拒")
	}
	if _, err := UsdtQuery(nil, "O1"); err == nil {
		t.Error("未启动时查询应当被拒")
	}
	if _, err := UsdtAccount(nil, nil); err == nil {
		t.Error("未启动时账户查询应当被拒")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("未启动却广播了回执: %+v", got)
	}
	UsdtStop(nil) // 幂等
}

// TestOrderPreChecks 下单前置校验在发出请求之前就生效。
func TestOrderPreChecks(t *testing.T) {
	// 用一份引用齐备但没有任何端点的配置：真发出请求就会因缺端点报错，
	// 前置校验应当先于端点判定拦下来。
	startWith(t, payCfg("https://pay.invalid", nil))
	recorded()
	// 收款方整段缺失：断言落在「to 为空」上，而不是旧字段名 address——
	// 契约断代后空 To 与空地址是两种不同原因，这条闸盯的是前者。
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil ||
		!strings.Contains(err.Error(), "收款方") {
		t.Errorf("提现缺收款方未被拦下: %v", err)
	}
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{}}); err == nil ||
		!strings.Contains(err.Error(), "收款") {
		t.Errorf("收款方整段空白未被拦下: %v", err)
	}
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 0, To: &pay.Party{Address: "a"}}); err == nil {
		t.Error("零金额未被拦下")
	}
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{Amount: 1, To: &pay.Party{Address: "a"}}); err == nil {
		t.Error("空幂等键未被拦下")
	}
	if _, err := UsdtQuery(nil, "   "); err == nil {
		t.Error("空订单号未被拦下")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("前置校验失败却广播了回执: %+v", got)
	}
}
