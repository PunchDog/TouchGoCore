package tron

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/internal/httpx"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件是 USDT 侧装配与报文的用例。它们原先住在 usdt 包里，实现搬过来后用例跟着搬：
// 装配（newUSDTClient）与缺省补齐（route.order）都是本包的私有符号，
// usdt 包只剩转调，摸不到这些符号，也就测不到「引用指不通」「合约没进签名域」这一整类错误。

// USDT 侧夹具与 tron_test.go 的 TRX 夹具刻意分开设名（usdt 前缀）：两套配置的
// 商户号、合约值即便暂时相同，语义也是一个说 USDT、一个说 TRX，
// 合并命名会让「改一边忘另一边」看起来像两边都改过了。
const (
	usdtTestSecret   = "TEST_SECRET_KEY"
	usdtTestAccount  = "default"
	usdtMerchantID   = "MCH-TEST"
	usdtTestContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	usdtTestNetwork  = pay.NetworkMainnet
)

// usdtRecorder 收集 USDT 门面按 CallUsdtMsg 广播出去的回执。
var usdtRecorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallUsdtMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallUsdtMsg+kind, func(res *pay.PayResult) {
			usdtRecorder.mu.Lock()
			usdtRecorder.calls = append(usdtRecorder.calls, res)
			usdtRecorder.mu.Unlock()
		})
	}
}

func usdtRecorded() []*pay.PayResult {
	usdtRecorder.mu.Lock()
	defer usdtRecorder.mu.Unlock()
	got := usdtRecorder.calls
	usdtRecorder.calls = nil
	return got
}

// usdtPayCfg 组一份完整的内联形态配置：供应商字段、币种事实、链上接入点都写在 tron.usdt 一段里。
//
// 测试必须走真实的装配入口而不是直接喂 ProviderOptions，否则就测不到
// 「驱动名写错」「商户号点不出来」这一整类错误。
func usdtPayCfg(baseURL string, endpoints map[string]string) *config.Cfg {
	return &config.Cfg{
		Tron: &config.TronConfig{
			Usdt: &config.TronUsdtConfig{
				ChannelSupplierConfig: config.ChannelSupplierConfig{
					Enable: "on", Driver: DriverTronGrid, BaseURL: baseURL,
					AppID: "app1", SecretKey: usdtTestSecret, Endpoints: endpoints,
					Accounts: map[string]*config.ChannelAccountConfig{
						usdtTestAccount: {Enable: "on", MerchantID: usdtMerchantID},
					},
				},
				Network:  usdtTestNetwork,
				Contract: usdtTestContract,
				Chain:    usdtChainCfg(),
			},
		},
	}
}

// usdtTestPrivKey / usdtTestWallet 是一把可公开的固定私钥与它的派生地址。
//
// 地址不是抄来的：用 gotron-sdk 的 keys.GetPrivateKeyFromHex + address.BTCECPrivkeyToAddress
// 现算得到。装配会核对「私钥派生地址 == chain.wallet_address」，抄错就当场拒绝启动，
// 所以这一对必须成对出现并写明来历。
const (
	usdtTestPrivKey = "1111111111111111111111111111111111111111111111111111111111111111"
	usdtTestWallet  = "TCLBgkbfVkJroVBJVqBEsxtPNQEQMTQCLQ"
)

// usdtChainCfg 给出链上接入点：节点写死成本地不可达地址即可——构造阶段只建连接对象
// 不拨号，真正会连节点的用例都由 double_pay_test.go 的假网关驱动。
func usdtChainCfg() *config.TronChainConfig {
	return &config.TronChainConfig{
		Node: "127.0.0.1:50051", TLS: "off", WalletAddress: usdtTestWallet,
		PrivateKey: usdtTestPrivKey, FeeLimit: 1000,
	}
}

// usdtStartWith 用给定配置走一遍 USDT 侧真实启动路径（含密钥钩子与 SDK 引用解析）。
//
// 只停 USDT 门面：两条链路各有快照，测 USDT 时不该顺手把 TRX 的客户端也摘掉。
func usdtStartWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	UsdtStop(nil)
	usdtRecorded()
	UsdtStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		UsdtStop(nil)
		usdtRecorded()
	})
}

// usdtRequest 是一次到达假供应商的请求摘要（不含凭证）。
type usdtRequest struct {
	path string
	sign string
	body string
}

// usdtSupplier 是假供应商：按路径返回预置响应，并记录收到的请求。
type usdtSupplier struct {
	mu       sync.Mutex
	requests []usdtRequest
	response map[string]string // 路径 -> 响应包络
	status   map[string]int    // 路径 -> HTTP 状态码（0 表示 200）
}

func newUsdtSupplier(t *testing.T) (*usdtSupplier, string) {
	t.Helper()
	f := &usdtSupplier{response: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, usdtRequest{
			path: r.URL.Path, sign: r.Header.Get("X-Sign"), body: string(body),
		})
		payload, code := f.response[r.URL.Path], f.status[r.URL.Path]
		f.mu.Unlock()
		if payload == "" {
			payload = `{"code":"0","data":{}}`
		}
		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			w.WriteHeader(code)
		}
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *usdtSupplier) seen() []usdtRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]usdtRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func usdtAllEndpoints() map[string]string {
	return map[string]string{
		pay.EndpointAccount: "/api/account",
		pay.EndpointQuery:   "/api/query",
	}
}

// TestUsdtNewClientFailClosed 配置指不通的链路一律不启动：通道段没开、驱动名写错、
// 商户账户点不出来、密钥/私钥没注入，只会在供应商侧留下验签失败，本地却看起来一切正常。
func TestUsdtNewClientFailClosed(t *testing.T) {
	full := func() *config.Cfg { return usdtPayCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 tron.usdt 段", func(c *config.Cfg) { c.Tron = nil }},
		{"通道段没开", func(c *config.Cfg) { c.Tron.Usdt.Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.Tron.Usdt.Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.Tron.Usdt.Driver = "no_such_driver" }},
		{"多账户没点名", func(c *config.Cfg) {
			c.Tron.Usdt.Accounts["reserve"] = &config.ChannelAccountConfig{Enable: "on", MerchantID: "M2"}
			c.Tron.Usdt.AccountID = ""
		}},
		{"商户账户停用", func(c *config.Cfg) { c.Tron.Usdt.Accounts[usdtTestAccount].Enable = "off" }},
		{"缺签名密钥", func(c *config.Cfg) { c.Tron.Usdt.SecretKey = "" }},
		{"缺链上接入点", func(c *config.Cfg) { c.Tron.Usdt.Chain = nil }},
		{"链上缺节点", func(c *config.Cfg) { c.Tron.Usdt.Chain.Node = "" }},
		{"私钥与出款地址不是同一个账户", func(c *config.Cfg) {
			c.Tron.Usdt.Chain.WalletAddress = tronFFAcct
		}},
	}
	for _, cs := range cases {
		cfg := full()
		cs.mutate(cfg)
		if c, ok := newUSDTClient(cfg); ok || c != nil {
			t.Errorf("%s 不应启动，实得 ok=%v", cs.name, ok)
		}
	}
	// 端点表为空但其余齐备：装配成功，但任何操作都应在发请求之前被拒。
	if _, ok := newUSDTClient(full()); !ok {
		t.Error("字段齐备时应当启动")
	}
	// 按单携带出款参数后，配置里的钱包/私钥/商户账户都可以不填：通道照常装配，
	// 但一笔没带出款私钥的单必须在碰网关之前被拒。
	perOrder := full()
	perOrder.Tron.Usdt.Accounts = nil
	perOrder.Tron.Usdt.SecretKey = ""
	perOrder.Tron.Usdt.Chain.WalletAddress = ""
	perOrder.Tron.Usdt.Chain.PrivateKey = ""
	perOrderClient, ok := newUSDTClient(perOrder)
	if !ok || perOrderClient == nil {
		t.Fatal("按单携带出款参数时，配置缺钱包/私钥/商户账户也应能装配")
	}
	_, err := perOrderClient.Ch.Withdraw(context.Background(), &pay.PayOrder{
		OrderNo: "O-ASSET-1", Amount: 1, Currency: pay.CurrencyUSDT,
		To: &pay.Party{Kind: pay.PartyWallet, Address: usdtTestContract},
	})
	if err == nil || !strings.Contains(err.Error(), "缺少出款私钥") {
		t.Fatalf("没带 From 私钥的单必须在签名前拒单，实得 %v", err)
	}
	// 没配 base_url 时照样能起：链上出款不依赖供应商，但商户账户查询必须就地拒绝，
	// 不能拿着一个 nil 客户端去发请求。
	noSupplier := full()
	noSupplier.Tron.Usdt.BaseURL = ""
	noClient, ok := newUSDTClient(noSupplier)
	if !ok || noClient == nil {
		t.Fatal("纯自托管（不配供应商）应当允许装配链上出款")
	}
	if _, err := noClient.Ch.QueryAccount(context.Background(), &pay.AccountQuery{}); err == nil {
		t.Error("没有供应商侧时账户查询必须明确拒绝")
	}
	// 只有一个账户时允许不点名；多个账户时点名即可启用备用商户号。
	cfg := full()
	cfg.Tron.Usdt.AccountID = ""
	if _, ok := newUSDTClient(cfg); !ok {
		t.Error("唯一账户时 account 留空应当可启动")
	}
	cfg = full()
	cfg.Tron.Usdt.Accounts["reserve"] = &config.ChannelAccountConfig{Enable: "on", MerchantID: "MCH-RESERVE"}
	cfg.Tron.Usdt.AccountID = "reserve"
	if _, ok := newUSDTClient(cfg); !ok {
		t.Error("点名备用账户后应当可启动")
	}
}

// TestUsdtClientCarriesChainNetwork newUSDTClient 装配出的网络标识来自链路配置。
//
// 门面补全订单缺省值（缺网络时深拷贝 To 回填、显式值不覆盖、交来的订单不就地改）
// 是 chainroute 机器的一部分，其行为基线在 chainroute/route_test.go 只写一份、四链
// 共用；这里只钉「TRON 侧的网络来自 tron.usdt.chain 配置」这条接线事实。
func TestUsdtClientCarriesChainNetwork(t *testing.T) {
	c, ok := newUSDTClient(usdtPayCfg("https://pay.invalid", nil))
	if !ok {
		t.Fatal("装配失败")
	}
	if c.Network != usdtTestNetwork {
		t.Fatalf("快照网络不是链路配置里的 %s: %q", usdtTestNetwork, c.Network)
	}
}

// TestUsdtNewClientRejectsBogusContract 合约地址非空时先过校验和再谈启动：
// 它是广播时的收端之一，写错要等到第一笔出款才暴露。
func TestUsdtNewClientRejectsBogusContract(t *testing.T) {
	cfg := usdtPayCfg("https://pay.invalid", nil)
	if _, ok := newUSDTClient(cfg); !ok {
		t.Fatal("夹具合约应当能通过启动校验，否则本用例说明不了任何事")
	}
	cfg = usdtPayCfg("https://pay.invalid", nil)
	cfg.Tron.Usdt.Contract = tronBogusChecksum
	if c, ok := newUSDTClient(cfg); ok || c != nil {
		t.Errorf("校验和不过的合约地址不应让通道启动，实得 ok=%v", ok)
	}
	// 原生 TRX 通道留空合约是合法形态。
	cfg = usdtPayCfg("https://pay.invalid", nil)
	cfg.Tron.Usdt.Contract = ""
	if _, ok := newUSDTClient(cfg); !ok {
		t.Error("合约留空（原生 TRX）应当可启动")
	}
}

// TestUsdtNewClientRejectsTokenStandardAsNetwork trc20 是代币标准不是公链网络，
// 混填进 network 会让换测试网时无从表达；未知写法不由本包断言，硬拦会把能用的配置拦死。
func TestUsdtNewClientRejectsTokenStandardAsNetwork(t *testing.T) {
	for _, net := range []string{"trc20", "TRC20", "erc20", "bep-20"} {
		cfg := usdtPayCfg("https://pay.invalid", nil)
		cfg.Tron.Usdt.Network = net
		if c, ok := newUSDTClient(cfg); ok || c != nil {
			t.Errorf("network=%s 是代币标准不是网络，不该让通道启动，实得 ok=%v", net, ok)
		}
	}
	for _, net := range []string{"", pay.NetworkMainnet, pay.NetworkShasta, pay.NetworkNile, "supplier_custom_net"} {
		cfg := usdtPayCfg("https://pay.invalid", nil)
		cfg.Tron.Usdt.Network = net
		if _, ok := newUSDTClient(cfg); !ok {
			t.Errorf("network=%q 应当放行（未知写法不由本包断言），实得 ok=false", net)
		}
	}
}

// TestUsdtWithdrawDoesNotReachSupplier 提现已改为本地签名上链，供应商侧不再收到提现报文。
//
// 这条断言留着是为了钉住「换了路径」这件事本身：一旦有人把门面改回 c.ch.Withdraw 走供应商，
// 这里就会看见报文出现在供应商侧（或报缺 endpoints.withdraw）——而出款究竟该由谁广播是
// 资损级的问题。链上出款的行为（签名一次、台账、双付）由 double_pay_test.go 逐条覆盖。
func TestUsdtWithdrawDoesNotReachSupplier(t *testing.T) {
	f, url := newUsdtSupplier(t)
	f.response["/api/withdraw"] = `{"code":"0","data":{"order_no":"O2","status":"pending"}}`
	usdtStartWith(t, usdtPayCfg(url, usdtAllEndpoints()))
	usdtRecorded()

	// 节点是本地不可达地址：出款走到链上网关就会失败，绝不会借用供应商的 HTTP 出口。
	_, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 500, To: &pay.Party{Address: tronFFAcct}})
	if err == nil {
		t.Fatal("链上出款在节点不可达时必须报错，不能假装受理")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("提现又回到供应商链路了，发出 %d 次请求", n)
	}
	// 前置校验仍然排在触网之前：收款地址不合法的一单连台账都不该进。
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O3", Amount: 500, To: &pay.Party{Address: tronBogusChecksum}}); err == nil {
		t.Fatal("校验和不过的收款地址必须被拒")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("被前置校验拒掉的单仍碰了供应商: %d 次", n)
	}
	// 广播给下游的键名口径没变（CallUsdtMsg+"Withdraw"），下游回调不必改注册。
	if got := usdtRecorded(); len(got) != 0 {
		t.Fatalf("失败的出款不该广播回执: %+v", got)
	}
}

// TestUsdtUnregisteredEndpointSendsNothing 端点没登记的操作必须就地拒绝：
// 发出去只会拿到一个含义不明的 404，还留下「请求可能已被受理」的疑点。
// 供应商侧只剩账户查询一条路（充值已删），这条闸由它扛。
func TestUsdtUnregisteredEndpointSendsNothing(t *testing.T) {
	f, url := newUsdtSupplier(t)
	usdtStartWith(t, usdtPayCfg(url, map[string]string{pay.EndpointQuery: "/api/query"}))

	if _, err := UsdtAccount(nil, nil); err == nil {
		t.Fatal("未登记端点应当报错")
	} else if !strings.Contains(err.Error(), "endpoints."+pay.EndpointAccount) {
		t.Fatalf("错误未指明缺哪个端点: %v", err)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("配置不全却发出了 %d 次请求", n)
	}
}

// TestUsdtBusinessFailureIsNotRetried 包络业务失败码只发一次请求，且错误文案不含凭证。
// 走账户查询链路：充值删除后它是唯一还经过 HTTP 供应商的操作，供应商侧行为由它钉。
func TestUsdtBusinessFailureIsNotRetried(t *testing.T) {
	f, url := newUsdtSupplier(t)
	f.response["/api/account"] = `{"code":"250008","msg":"余额不足"}`
	usdtStartWith(t, usdtPayCfg(url, usdtAllEndpoints()))
	usdtRecorded()

	_, err := UsdtAccount(nil, nil)
	if err == nil {
		t.Fatal("业务失败应当报错")
	}
	var pe *pay.ProviderError
	if !errors.As(err, &pe) || pe.Code != "250008" {
		t.Fatalf("错误类型不符: %v", err)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("业务失败重发了 %d 次", n)
	}
	if strings.Contains(err.Error(), usdtTestSecret) {
		t.Fatalf("错误文案泄漏密钥: %v", err)
	}
	if got := usdtRecorded(); len(got) != 0 {
		t.Fatalf("失败不该广播回执: %+v", got)
	}
}

// TestUsdtWhitelistedBizCodeRetryIsTransparent 白名单内的供应商业务码由共用层原单重发，门面只报告最终结果。
//
// 新契约（L3）：Withdraw 严格路径仅 RetryableCodes 白名单内的码可重试，5xx 不再默认 Retryable。
// 本用例用【虚构业务码 59901】验证白名单机制：登记后该码触发同一张单透明重发。
// 警示：严禁拿 502/504 这类网关状态码做演示——502=受理状态未知（请求可能已被供应商受理、
// 只是回执没穿回来），把它登记进提现白名单再原单重发就是双付；详见 httpx.SetRetryableCodes 文档。
func TestUsdtWhitelistedBizCodeRetryIsTransparent(t *testing.T) {
	// 虚构业务码走带锁入口临时登记，用后即撤，不裸写 map；不用真实网关码（502/504）以免背书双付风险配置。
	const fakeBizCode = "59901"
	httpx.SetRetryableCodes(fakeBizCode)
	defer httpx.DeleteRetryableCode(fakeBizCode)

	f, url := newUsdtSupplier(t)
	// 供应商以 2xx 包络回一个「明确拒绝、未受理」的业务码——这类码原单重发不会双花，
	// 正是白名单的合法用途（区别于受理状态未知的 502/504 网关码）。
	//
	// 走账户查询链路：提现已本地签名上链，供应商侧没有提现报文，白名单在门面这一层的
	// 活闸只能挂在仍然经过 HTTP 的那条路上。
	f.response["/api/account"] = `{"code":"59901","msg":"供应商明确拒绝，未受理"}`
	cfg := usdtPayCfg(url, usdtAllEndpoints())
	cfg.Tron.Usdt.MaxRetries = 1
	usdtStartWith(t, cfg)

	_, err := UsdtAccount(nil, nil)
	if err == nil {
		t.Fatal("白名单内业务码持续被拒应当报错")
	}
	seen := f.seen()
	if len(seen) != 2 {
		t.Fatalf("请求次数=%d，期望 2（首次 + 1 次重试）", len(seen))
	}
	if seen[0].body != seen[1].body {
		t.Fatalf("重试换了报文，幂等键失效:\n%s\n%s", seen[0].body, seen[1].body)
	}
}

// TestUsdtInlineKeyHookInjectsSecret 配置文件里不留密钥：下游按通道段名（tron.usdt）
// 注册钩子，装配时注入凭证。与 telegram 包的 BotKey 钩子同源，差别在键名不再跟旧表段名走。
func TestUsdtInlineKeyHookInjectsSecret(t *testing.T) {
	f, url := newUsdtSupplier(t)
	cfg := usdtPayCfg(url, usdtAllEndpoints())
	cfg.Tron.Usdt.SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+inlineUSDTKey, func(sk *string) { *sk = usdtTestSecret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+inlineUSDTKey, id)

	usdtStartWith(t, cfg)
	if _, err := UsdtAccount(nil, nil); err != nil {
		t.Fatalf("钩子注入密钥后账户查询链路应当可用: %v", err)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("请求次数=%d，期望 1", len(reqs))
	}
	if reqs[0].sign == "" {
		t.Fatal("请求未带上钩子注入密钥算出的签名")
	}
}

// TestUsdtAccountQueryReadsMerchantSnapshot 商户账户查询把供应商侧的余额、可用额、
// 授信与费率读成整数快照，并带上商户号与本通道的合约；这是读操作，不广播回执。
func TestUsdtAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newUsdtSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"active"}}`
	usdtStartWith(t, usdtPayCfg(url, usdtAllEndpoints()))
	usdtRecorded()

	info, err := UsdtAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctActive || info.Balance != 10 || info.Available != 7 ||
		info.Frozen != 3 || info.Credit != 1 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if info.Currency != pay.CurrencyUSDT {
		t.Fatalf("币种缺省未补: %+v", info)
	}
	if !info.CanWithdraw(7) || info.CanWithdraw(8) {
		t.Fatalf("可用额判断不符: %+v", info)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("账户查询发出 %d 次请求，期望 1", len(reqs))
	}
	if !strings.Contains(reqs[0].body, `"`+"merchant_id"+`":"`+usdtMerchantID) ||
		!strings.Contains(reqs[0].body, `"`+"contract"+`":"`+usdtTestContract) {
		t.Fatalf("账户报文缺路由字段: %s", reqs[0].body)
	}
	if strings.Contains(reqs[0].body, usdtTestSecret) {
		t.Fatal("密钥进了报文")
	}
	if got := usdtRecorded(); len(got) != 0 {
		t.Fatalf("账户查询不该广播资金回执: %+v", got)
	}
}

// TestUsdtAccountQueryFailureNotReportedAsZero 供应商拒了就只能报错：
// 返回一个全零账户会被下游读成「余额不足」，把查询故障伪装成业务拒绝。
func TestUsdtAccountQueryFailureNotReportedAsZero(t *testing.T) {
	f, url := newUsdtSupplier(t)
	f.response["/api/account"] = `{"code":"401002","msg":"签名失败"}`
	usdtStartWith(t, usdtPayCfg(url, usdtAllEndpoints()))
	if _, err := UsdtAccount(nil, &pay.AccountQuery{Currency: pay.CurrencyUSDT}); err == nil {
		t.Fatal("业务失败应当报错")
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("请求次数=%d，期望 1（业务失败不重试）", n)
	}
}

// TestUsdtChainRuleLivesInThisPackage 地址规则不再经 pay 的注册表：它就是以
// TRONRule 形式住在本包的同一个实现，USDT 与 TRX 共用一份判法。
//
// 注册表删掉之后，「忘了登记 ⇒ 未登记即放行」这条静默通道不存在了：调用点是
// checkOrderAddress(o) 的函数调用，漏接就是一个编译错误。
func TestUsdtChainRuleLivesInThisPackage(t *testing.T) {
	rule := TRONRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: ""}}); err != nil {
		t.Errorf("空地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tronRangeAcct}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	for _, bad := range []string{tronBogusChecksum, "UQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcY", "0x0000"} {
		if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: bad}}); err == nil {
			t.Errorf("非法地址 %s 未被拦下", bad)
		}
	}
	for _, memo := range []string{"123", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err == nil {
			t.Errorf("TRC20 侧的备注 %q 未被拦下", memo)
		}
	}
	// 同一个实现服务两个币种：USDT 与 TRX 的地址形态本来就是一样的。
	// 这里钉的是「两币不再各有一份规则」：同一个值经同一份实现判定，结论必须一致。
	trx := TRONRule{}
	addr := &pay.PayOrder{To: &pay.Party{Address: tronBogusChecksum}}
	if (rule.CheckAddress(addr) == nil) != (trx.CheckAddress(addr) == nil) {
		t.Error("两条币种对同一个地址判法不一致")
	}
}

// TestExampleConfigLeavesUsdtChannelDown 示例配置本身配不出「已启动」的 USDT 通道。
//
// 示例是给下游抄的起点：它必须过 Validate（否则用户第一反应是改配置而不是怀疑示例），
// 但它的 enable 不是 on、也没有 provider 引用，所以装配必须停在 ok=false。
// 少了这条闸，示例里多一句 enable=on 或 provider 就会被抄进一批会真出款的部署。
func TestExampleConfigLeavesUsdtChannelDown(t *testing.T) {
	raw, err := os.ReadFile("../config/example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg config.Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example.json 未通过 Validate: %v", err)
	}
	if cfg.Tron == nil || cfg.Tron.Usdt == nil {
		t.Fatal("示例缺 tron.usdt 段")
	}
	if ch, ok := newUSDTClient(&cfg); ok || ch != nil {
		t.Fatal("示例配置装配出了 USDT 通道：示例不该是「复制示例即出款」的形态")
	}
}

// TestUsdtRouteBindsUnifiedChannel 装配成功必须能以 pay.Channel 的统一调用面取回：
// whatapp 的 channelFor 只认 pay.ChannelFor(KindTronUSDT)，这里断了会在下游第一笔出款时
// 才暴露；而 Bind 是运行期类型断言，编译器帮不上忙。
func TestUsdtRouteBindsUnifiedChannel(t *testing.T) {
	usdtStartWith(t, usdtPayCfg("", nil))
	ch, err := pay.ChannelFor(pay.KindTronUSDT)
	if err != nil {
		t.Fatalf("已启动的 USDT 通道取不到统一调用面: %v", err)
	}
	if ch == nil {
		t.Fatal("统一调用面为 nil")
	}
	res, err := ch.QueryOrder(context.Background(), "NO-SUCH-ORDER")
	if err != nil {
		t.Fatalf("查单失败: %v", err)
	}
	if res == nil || res.Status != pay.StatusUnknown {
		t.Fatalf("台账没有的单号应回 UNKNOWN，实得 %+v", res)
	}
}

// TestWithdrawRejectsAmountUnits TRON 只消费 int64 最小单位；字符串金额必须显式拒绝，
// 否则 Amount=0 会被下游当成 0 元转账。
func TestWithdrawRejectsAmountUnits(t *testing.T) {
	c := &onchainChannel{}
	_, err := c.Withdraw(context.Background(), &pay.PayOrder{
		OrderNo: "O-UNITS", AmountUnits: "1000000",
		To: &pay.Party{Kind: pay.PartyWallet, Address: "TAddr1"},
	})
	if err == nil || !strings.Contains(err.Error(), "amount_units") {
		t.Fatalf("TRON 未拒绝 AmountUnits: %v", err)
	}
}
