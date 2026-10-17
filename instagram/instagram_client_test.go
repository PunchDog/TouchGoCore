package instagram

import (
	"context"
	"encoding/json"
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
	"touchgocore/util"
)

// capturedRequest 是一次到达假供应商的请求摘要（不含凭证）。
type capturedRequest struct {
	path string
	sign string
	auth string
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
			path: r.URL.Path, sign: r.Header.Get("X-Sign"), auth: r.Header.Get("Authorization"), body: string(body),
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

// startWith 用给定配置走一遍真实启动路径（含 SDK 引用解析与密钥钩子）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	InstagramStop(nil)
	recorded()
	InstagramStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		InstagramStop(nil)
		recorded()
	})
}

func allEndpoints() map[string]string {
	return map[string]string{
		pay.EndpointSendCode: "/api/code",
		pay.EndpointLogin:    "/api/login",
	}
}

// TestLoginCarriesTokenOnLoginChain 登录后会话凭证只经请求头传给后续请求，不出现在报文里。
//
// 原用例名带 Recharge，测的是「登录后用同一登录态发充值」：充值侧随资金链路删除，
// 已无被测对象。这里保留它的登录态部分——后续请求改走还活着的登录链路（关联账号列表），
// 并补一条「资金门面零请求」的钉，防止有人把会话转手给资金通道。
func TestLoginCarriesTokenOnLoginChain(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9","phone":"86138"}}`
	f.response["/api/friends"] = `{"code":"0","data":{"list":[{"user_id":"U1"}]}}`
	startWith(t, payCfg(url, friendEndpoints()))
	recorded()

	got, err := InstagramLogin(nil, "86138", "123456")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if got.UserID != "U9" {
		t.Fatalf("登录结果=%+v", got)
	}
	// 登录请求本身还没有会话凭证，但必须已带签名。
	first := f.seen()
	if len(first) != 1 || first[0].sign == "" || first[0].auth != "" {
		t.Fatalf("登录请求不符: %+v", first)
	}
	if !strings.Contains(first[0].body, "123456") {
		t.Fatalf("登录报文缺一次性凭证: %s", first[0].body)
	}

	list, err := InstagramFriends(nil, "U9")
	if err != nil {
		t.Fatalf("关联账号查询失败: %v", err)
	}
	if len(list) != 1 || list[0].UserID != "U1" {
		t.Fatalf("关联账号列表=%+v", list)
	}
	req := f.seen()[1]
	if req.auth != "TK-1" {
		t.Fatalf("后续请求未带会话凭证: %q", req.auth)
	}
	// 签名覆盖 AppID+Account 两个登记字段，会话凭证只走请求头。
	if want := pay.SignMD5(testSecret, "app1", "U9"); req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
	if strings.Contains(req.body, "TK-1") || strings.Contains(req.body, testSecret) {
		t.Fatalf("凭证进了报文: %s", req.body)
	}
	if _, err := InstagramWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000, Currency: pay.CurrencyUSDT, Address: "a"}); !errors.Is(err, ErrPayoutNotWired) {
		t.Fatalf("提现应返回未接入哨兵: %v", err)
	}
	if n := len(f.seen()); n != 2 {
		t.Fatalf("资金门面却发出了请求，实得 %d 次", n)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("登录侧读操作不该广播回执: %+v", got)
	}
}

// TestSendCodeUsesConfiguredTemplate 模板名来自配置，供文档到位后按供应商模板改。
func TestSendCodeUsesConfiguredTemplate(t *testing.T) {
	f, url := newFakeSupplier(t)
	cfg := payCfg(url, allEndpoints())
	cfg.Instagram.Templates["auth_verify"] = "tpl_x"
	startWith(t, cfg)

	if err := InstagramSendCode(nil, " 86138 "); err != nil {
		t.Fatalf("发码失败: %v", err)
	}
	req := f.seen()[0]
	var got map[string]any
	if err := json.Unmarshal([]byte(req.body), &got); err != nil {
		t.Fatal(err)
	}
	if got["template"] != "tpl_x" || got["phone"] != "86138" {
		t.Fatalf("发码报文不符: %s", req.body)
	}
}

// TestLogoutClearsToken 退出后请求头不再带旧会话，避免拿已失效凭证继续发请求。
//
// 原用例的载体是资金查单，载体随链路删除；这里换成还活着的登录链路请求，
// 被测对象仍是 InstagramLogout 把两边会话一起清掉这条口径。
func TestLogoutClearsToken(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	f.response["/api/friends"] = `{"code":"0","data":{"list":[{"user_id":"U1"}]}}`
	startWith(t, payCfg(url, friendEndpoints()))

	if _, err := InstagramLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	InstagramLogout()
	if _, err := InstagramFriends(nil, "U9"); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if req := f.seen()[1]; req.auth != "" {
		t.Fatalf("退出后仍带会话凭证: %q", req.auth)
	}
}

// TestSessionNotSharedAcrossHosts 登录链路与资金段指向不同供应商时，会话凭证不得出现在
// 另一家的任何请求上：那等于把用户的登录态交给第三方。
//
// 原用例钉的是 syncSession 的「基址不同就不带 token」规则，该实现随资金链路一起删除，
// 连「两条链路各持一份会话」这件事都不再存在。同一条安全不变量现在钉的是可观察面：
// 资金段那家供应商收到 0 次请求，且登录请求的 token 只出现在登录那家身上。
func TestSessionNotSharedAcrossHosts(t *testing.T) {
	loginF, loginURL := newFakeSupplier(t)
	loginF.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	fundF, fundURL := newFakeSupplier(t)

	cfg := payCfg(fundURL, allEndpoints())
	// 登录链路换成另一家：独立 SDK 段、独立基址，只做下发所以没有商户账户。
	cfg.PaySDks[loginSection] = &config.PaySDKConfig{
		Enable: "on", Driver: pay.DriverGeneric, BaseURL: loginURL,
		AppID: "app1", SecretKey: testSecret,
		Endpoints: map[string]string{pay.EndpointLogin: "/api/login", pay.EndpointSendCode: "/api/code"},
	}
	cfg.Instagram.Login = &config.PaySDKRef{SDK: loginSection}
	startWith(t, cfg)
	if _, err := InstagramLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := InstagramWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000, Address: "a"}); !errors.Is(err, ErrPayoutNotWired) {
		t.Fatalf("提现应返回未接入哨兵: %v", err)
	}
	if n := len(fundF.seen()); n != 0 {
		t.Fatalf("会话凭证被送到另一家供应商（%d 次请求）", n)
	}
	if reqs := loginF.seen(); len(reqs) != 1 || reqs[0].auth != "" {
		t.Fatalf("登录链路请求序列不符: %+v", reqs)
	}
}

// TestSdkSectionKeyHookInjectsSecret 配置文件里不留密钥：下游按 SDK 段名注册一个钩子，
// 装配时注入凭证。与 telegram 包的 BotKey 钩子同源，差别在于键名跟着 SDK 段走——
// 一家供应商的密钥登记一次，而不是每条通道抄一遍。
func TestSdkSectionKeyHookInjectsSecret(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	cfg := payCfg(url, allEndpoints())
	cfg.PaySDks[testSection].SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+testSection, func(sk *string) { *sk = testSecret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+testSection, id)

	startWith(t, cfg)
	if _, err := InstagramLogin(nil, "86138", "123456"); err != nil {
		t.Fatalf("钩子注入密钥后登录应当可用: %v", err)
	}
	if err := InstagramSendCode(nil, "86138"); err != nil {
		t.Fatalf("钩子注入密钥后发码应当可用: %v", err)
	}
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("请求次数=%d，期望 2", len(reqs))
	}
	// 签名值必须出自钩子注入的那份密钥：没注入时装配当场失败，根本走不到发请求。
	if want := pay.SignMD5(testSecret, "app1", "86138", "123456"); reqs[0].sign != want {
		t.Fatalf("登录签名=%s，期望 %s", reqs[0].sign, want)
	}
	for i, req := range reqs {
		if req.sign == "" {
			t.Fatalf("第 %d 个请求未带上钩子注入密钥算出的签名", i+1)
		}
	}
}

// TestStartNilCfgDoesNotPanic 没有任何 instagram 配置时启动必须安静通过。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	InstagramStop(nil)
	InstagramStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	// 传 nil 回落到包级运行快照：上一条刚把「没有配置」写进快照，这里才测得准「未启动」。
	InstagramStart(nil)
	if _, err := InstagramFriends(nil, "86138"); err == nil {
		t.Fatal("未启动时查关联账号列表应当被拒")
	}
	if err := InstagramSendCode(nil, "86138"); err == nil {
		t.Fatal("未启动时发码应当被拒")
	}
	InstagramStop(nil)
	InstagramStop(nil) // 幂等
}

// TestLoginChainStartsWithoutAccounts 登录段没有商户账户时，真实启动路径也要把这条
// 链路装配起来（走 service 分支）：只关掉「必须点名出账主体」这一条，其余口径不变。
func TestLoginChainStartsWithoutAccounts(t *testing.T) {
	f, url := newFakeSupplier(t)
	cfg := payCfg(url, allEndpoints())
	cfg.PaySDks[loginSection] = &config.PaySDKConfig{
		Enable: "on", Driver: pay.DriverGeneric, BaseURL: url,
		AppID: "app1", SecretKey: testSecret,
		Endpoints: map[string]string{pay.EndpointSendCode: "/api/code"},
	}
	cfg.Instagram.Login = &config.PaySDKRef{SDK: loginSection}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("无账户的登录段应通过配置校验: %v", err)
	}
	startWith(t, cfg)

	if err := InstagramSendCode(nil, "86138"); err != nil {
		t.Fatalf("登录链路应当可用: %v", err)
	}
	if reqs := f.seen(); len(reqs) != 1 || reqs[0].path != "/api/code" {
		t.Fatalf("发码请求不符: %+v", reqs)
	}
}
