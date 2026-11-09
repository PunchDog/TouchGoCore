package usdt

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/tron"
	"touchgocore/util"
)

// TRON 地址夹具：官方 USDT-TRC20 合约，以及用 20 字节账户哈希现算校验和造出的几个。
//
// 造向量而不是抄网上的：抄来的地址一旦本身是编的，测试就会把「校验和不过」这件事
// 永远藏住——本仓的示例配置就中过这一枪（原 usdt.contract 值 34 位、版本字节对、
// 校验和不通过）。
const (
	tronOfficialUSDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	tronAllZeroAcct  = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
	tronRangeAcct    = "T9yED5xMV5ARV98BexN97aLZ1UUq7eKSxm"
	tronFFAcct       = "TZJozAg1ruapycCicgz31GxvYJ1FraLjZa"
	// tronBogusChecksum 长度与版本字节全对，只有末尾 4 字节校验和不过。
	tronBogusChecksum = "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR"
)

// 本文件的用例都只走导出符号：地址判定走 usdt.IsValidTRONAddress（转发 tron），
// 规则直接取 tron.TRONRule——P5 删了 pay 的链规则注册表后，规则只以包内类型存在。
// 装配期的合约/网络校验用例（newClient 那两个）跟着实现搬到了 tron/usdt_test.go。

func TestIsValidTRONAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{tronOfficialUSDT, true, "官方 USDT-TRC20 合约"},
		{tronAllZeroAcct, true, "全零账户哈希"},
		{tronRangeAcct, true, "递增账户哈希"},
		{tronFFAcct, true, "全 F 账户哈希"},
		{tronBogusChecksum, false, "校验和不符的编造地址"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{tronRangeAcct[:33], false, "少一位"},
		{tronRangeAcct + "1", false, "多一位"},
		{strings.ReplaceAll(tronRangeAcct, "T", "1"), false, "首字符换成 1：前导零让解码长度变成 26 字节"},
		// 0OIl 这四个字符不在 Base58 字母表里，混进来必须当场拒，不能当成易混字符容错。
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6O", false, "含非法字符 O"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6l", false, "含非法字符 l"},
	}
	// 逐位篡改：每一位换成另一个合法字符都得被校验和判出来。
	tampered := []rune(tronRangeAcct)
	for i := range tampered {
		cases = append(cases, struct {
			addr string
			ok   bool
			desc string
		}{string(tampered[:i]) + "X" + string(tampered[i+1:]), false, "逐位篡改"})
	}
	for _, cs := range cases {
		if got := IsValidTRONAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidTRONAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestTRC20RuleRejectsBadAddressBeforeSending USDT 侧规则与链侧是同一份实现。
//
// usdt 包 import tron 就是为了这个：宿主只链接 usdt（不直接引 tron）时，
// 地址规则仍要生效——规则是函数调用，不是进程级登记。
func TestTRC20RuleRejectsBadAddressBeforeSending(t *testing.T) {
	rule := tron.TRONRule{}
	// 空地址放行是规则本层的口径：非空检查只在提现侧，由 pay.CheckOrder 负责。
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
	// 报错文案带原地址便于定位，但不得带密钥。
	err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tronBogusChecksum}})
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Errorf("报错文案不安全或未给出: %v", err)
	}
}

func TestTRC20RuleRejectsMemo(t *testing.T) {
	rule := tron.TRONRule{}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err == nil {
			t.Errorf("TRC20 侧的备注 %q 未被拦下", memo)
		}
	}
	// 纯空白按「没有备注」处理：真正不发出去的那一步在 NewOrderRequest 的 trim。
	if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: "   "}}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestUsdtRejectsBadChainFieldsWithoutSending 地址或备注不合链上规则的单，
// 必须一次请求都不发出、一条回执都不广播：发出去的最坏结果是钱动了但落错了地方。
func TestUsdtRejectsBadChainFieldsWithoutSending(t *testing.T) {
	f, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O9", Amount: 10, To: &pay.Party{Address: tronRangeAcct, Memo: "8890"}}); err == nil {
		t.Fatal("TRC20 带备注的提现应当被拒")
	}
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O8", Amount: 10, To: &pay.Party{Address: "TAddr1"}}); err == nil {
		t.Fatal("格式不对的收款地址应当被拒")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("被拒的单发出了 %d 次请求，期望 0", n)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("被拒的单广播了回执: %+v", got)
	}
	// 纯空白备注按「没有备注」处理：门禁不该因为它把钱拦下。
	// 出款已改为本地签名上链，这里断言的是「过了前置校验」而不是「供应商收到了报文」——
	// 节点是夹具里的本地不可达地址，所以到链上网关这一步才失败。
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O7", Amount: 10, To: &pay.Party{Address: tronRangeAcct, Memo: "  "}}); err == nil {
		t.Fatal("链上出款在节点不可达时应当报错")
	} else if strings.Contains(err.Error(), "备注") || strings.Contains(err.Error(), "memo") {
		t.Fatalf("空白备注被门禁当成有值拦下了: %v", err)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("被拒的单发出了 %d 次请求，期望 0", n)
	}
}

// expectedSignMD5 是供应商签名算法的测试侧实现：值直连拼接后追加密钥，取 32 位小写 MD5。
//
// 生产侧那份住在 tron/account.go 且不导出；这里独立重算一遍，恰好也是一道对照：
// 两边算不一致时，签名域或算法就被改过了。
func expectedSignMD5(secret string, values ...string) string {
	sum := md5.Sum([]byte(strings.Join(values, "") + secret))
	return hex.EncodeToString(sum[:])
}

// 本包没有装配代码了，这里钉的是「转发有没有转错」：
// 门面必须落到 USDT 那条 route 上（币种缺省、contract 进签名域、回执键前缀），
// 而不是顺手接到了 TRX 那条 route 上——两者现在共用同一套机器（tron/route.go），
// 接错的时候报错文案与回执前缀都会跟着换，下游按 CallUsdtMsg 注册的回调就收不到东西。
// 报文形态与签名逐位的完整用例在 tron/usdt_test.go。

// capturedRequest 是一次到达假供应商的请求摘要（不含凭证）。
type capturedRequest struct {
	path string
	sign string
	body string
}

// fakeSupplier 是假供应商：按路径返回预置响应，并记录收到的请求。
type fakeSupplier struct {
	mu       sync.Mutex
	requests []capturedRequest
	response map[string]string // 路径 -> 响应包络
	status   map[string]int    // 路径 -> HTTP 状态码（0 表示 200）
}

func newFakeSupplier(t *testing.T) (*fakeSupplier, string) {
	t.Helper()
	f := &fakeSupplier{response: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, capturedRequest{
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

func (f *fakeSupplier) seen() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func allEndpoints() map[string]string {
	return map[string]string{
		pay.EndpointAccount: "/api/account",
		pay.EndpointQuery:   "/api/query",
	}
}

// TestShimForwardsToUsdtRouteOnWire 转发的门面确实落在 USDT 那条 route 上：
// 币种缺省补 USDT、合约既进报文又追加在签名域末尾。账户查询是充值删除后仅存的
// 供应商侧调用，路由身份由它钉（快照字段的归一条例在 tron 包用例里）。
func TestShimForwardsToUsdtRouteOnWire(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"currency":"USDT","status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := UsdtAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if res.Currency != pay.CurrencyUSDT {
		t.Fatalf("币种缺省未按 USDT 口径补: %+v", res)
	}
	reqs := f.seen()
	if len(reqs) != 1 || reqs[0].path != "/api/account" {
		t.Fatalf("账户查询请求不符: %+v", reqs)
	}
	req := reqs[0]
	if !strings.Contains(req.body, `"currency":"`+pay.CurrencyUSDT+`"`) ||
		!strings.Contains(req.body, `"contract":"`+testContract+`"`) {
		t.Fatalf("报文未按 USDT 通道装配: %s", req.body)
	}
	want := expectedSignMD5(testSecret,
		"app1", merchantID, pay.CurrencyUSDT,
		testContract)
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s（签名域少一位合约就是把通道接到了另一条 route 上）", req.sign, want)
	}
	// 账户查询是读操作，不广播回执。
	if pub := recorded(); len(pub) != 0 {
		t.Fatalf("账户查询广播了回执: %+v", pub)
	}
}

// TestShimKeepsRoutesIndependent 启动 USDT 不该让 TRX 门面变成「能出钱」：
// 两条链路各有各的状态，共用一份快照时换配置会出现用 TRX 网关发 USDT 的单。
//
// TRX 侧现在恒返回未接入哨兵（供应商提现实现已删，链上原生币出款没有实现），
// 所以这里的判据从「文案是 TRON 通道未启动」换成「错误就是那个哨兵」：
// 保住的是同一条不变量——USDT 起了，TRX 也不会跟着动起来。
func TestShimKeepsRoutesIndependent(t *testing.T) {
	_, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := tron.TronWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); !errors.Is(err, tron.ErrPayoutNotWired) {
		t.Fatalf("USDT 启动后 TRX 门面却可用了: %v", err)
	}
	if _, err := UsdtQuery(nil, "O1"); err != nil {
		t.Fatalf("USDT 门面应当可用: %v", err)
	}
}

// TestShimErrorWordingUnchanged 错误文案逐字保持合并前的写法：
// 下游按文案做失败分类与告警归因，改一个字就是下游的归类漂移。
func TestShimErrorWordingUnchanged(t *testing.T) {
	UsdtStop(nil)
	recorded()
	_, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: "a"}})
	if err == nil || err.Error() != "USDT 通道未启动（检查 tron.usdt 的 enable / chain / 商户账户是否齐备）" {
		t.Fatalf("未启动文案变了: %v", err)
	}
	if _, err := UsdtQuery(nil, "  "); err == nil || err.Error() != "USDT Query 失败: 订单号（幂等键）为空" {
		t.Fatalf("空订单号文案变了: %v", err)
	}

	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"250008","msg":"余额不足"}`
	startWith(t, payCfg(url, allEndpoints()))
	if _, err := UsdtAccount(nil, nil); err == nil ||
		!strings.HasPrefix(err.Error(), "USDT 账户查询失败: ") {
		t.Fatalf("失败前缀不是 USDT 口径: %v", err)
	}
	// 失败语义（错误码原样、只发一次、不广播）由 tron 包的 TestUsdtBusinessFailureIsNotRetried 钉。
}

// TestStartNilCfgDoesNotPanic 没有任何 usdt 配置时启动必须安静通过，
// 且后续操作被拒而不是拿着空链路发请求。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	UsdtStop(nil)
	UsdtStart(corectx.WithCfg(context.Background(), nil))
	UsdtStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	UsdtStart(nil)
	UsdtStart(corectx.WithCfg(context.Background(), &config.Cfg{Tron: &config.TronConfig{Usdt: &config.TronUsdtConfig{}}}))
	if _, err := UsdtQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	UsdtStop(nil)
	UsdtStop(nil) // 幂等
}

// TestTimeoutAndRetryDefaults 未配超时与重试时落到共用层默认值，而不是 0 超时。
//
// 用账户查询链路测：那是本通道仍然经过 HTTP 供应商的那条路（出款与查单已改为直接上链，
// 它们的超时口径由链上网关自己管）。
func TestTimeoutAndRetryDefaults(t *testing.T) {
	cfg := payCfg("https://pay.invalid", map[string]string{pay.EndpointAccount: "/api/account"})
	cfg.Tron.Usdt.TimeoutSec = 0
	cfg.Tron.Usdt.MaxRetries = -1
	startWith(t, cfg)
	// 域名不可解析：一次请求 + 默认重试次数后返回错误即可，不 panic 也不无限等待。
	if _, err := UsdtAccount(nil, nil); err == nil {
		t.Fatal("不可达供应商应当报错")
	}
}

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
