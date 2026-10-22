package telegram

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"touchgocore/pay"
)

// 本文件是 TON 侧供应商账户查询客户端的闸，与 tron/account_test.go 同构。
//
// 同构是刻意的：两份实现本来就是「各改各的不必回头看对方」，两边的闸也就
// 各钉各的。抽一层公共测试反而会让「某一边被改坏了」在两边一起变绿。

// expectedTonSign 是测试侧独立的 MD5 实现，只用来钉住「签了哪几个值、什么顺序」。
// 主闸是 TestTonSignMD5KnownVector 里的硬编码向量。
func expectedTonSign(secret string, values ...string) string {
	sum := md5.Sum([]byte(strings.Join(values, "") + secret))
	return hex.EncodeToString(sum[:])
}

// TestTonSignMD5KnownVector 签名算法逐字对得上供应商文档：
// 值直连拼接、无分隔符、末尾追加密钥、32 位小写。
// 向量由 .NET MD5 独立算出，不是拿被测实现的输出回填的。
func TestTonSignMD5KnownVector(t *testing.T) {
	const want = "12c0d40d00f1a74624432e0c3070b8fd"
	if got := tonSignMD5("ton-s3cr3t", "app_1", "merchant_9", "TON"); got != want {
		t.Fatalf("tonSignMD5 结果漂移: got %s want %s", got, want)
	}
	if got := tonSignMD5("k", "a", "b", "c"); len(got) != 32 {
		t.Fatalf("签名必须是 32 位小写十六进制，实得 %q", got)
	}
	if tonSignMD5("k", "a", "b", "c") == tonSignMD5("k", "c", "b", "a") {
		t.Fatal("换序后签名相同，说明拼接没有按登记顺序进行")
	}
	if tonSignMD5("k", "abc") == tonSignMD5("abc", "k") {
		t.Fatal("密钥必须追加在末尾，不能与字段值互换")
	}
}

// TestTonEnvelopeSucceeded 包络判定：有 code 按 code，否则退回 success 布尔。
func TestTonEnvelopeSucceeded(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		desc string
		env  tonEnvelope
		want bool
	}{
		{"code=0 成功", tonEnvelope{Code: "0"}, true},
		{"code 非 0 失败", tonEnvelope{Code: "E1001"}, false},
		{"无 code，success=true", tonEnvelope{Success: &yes}, true},
		{"无 code，success=false", tonEnvelope{Success: &no}, false},
		{"两者皆无", tonEnvelope{}, false},
		{"code 优先于 success", tonEnvelope{Code: "E1", Success: &yes}, false},
	}
	for _, c := range cases {
		if got := c.env.succeeded(); got != c.want {
			t.Errorf("%s: succeeded()=%v，期望 %v", c.desc, got, c.want)
		}
	}
}

// TestTonNon2xxBeatsSuccessEnvelope 非 2xx 先按传输层判掉：
// 502 页面里偶然出现 "code":"0" 不代表供应商受理了一次查询。
func TestTonNon2xxBeatsSuccessEnvelope(t *testing.T) {
	c := &tonAccountClient{name: "ton"}
	body := []byte(`{"code":"0","data":{"balance":1}}`)

	for _, status := range []int{500, 502, 503, 504, 429} {
		_, err := c.parse(status, nil, body)
		var pe *pay.ProviderError
		if !errors.As(err, &pe) {
			t.Errorf("%d: 期望 *pay.ProviderError，实得 %v", status, err)
			continue
		}
		if !pe.Retryable || pe.HTTPStatus != status {
			t.Errorf("%d: 应可重试且带回状态码，实得 %+v", status, pe)
		}
	}
	for _, status := range []int{400, 401, 403, 404} {
		_, err := c.parse(status, nil, body)
		var pe *pay.ProviderError
		if !errors.As(err, &pe) {
			t.Errorf("%d: 期望 *pay.ProviderError，实得 %v", status, err)
			continue
		}
		if pe.Retryable {
			t.Errorf("%d: 客户端错误不得重试", status)
		}
	}

	// 2xx 却拿到非 JSON：不能当成受理成功。
	if _, err := c.parse(200, nil, []byte("<html>维护中</html>")); err == nil {
		t.Fatal("2xx 非 JSON 应判失败")
	} else {
		var pe *pay.ProviderError
		if !errors.As(err, &pe) || pe.Retryable {
			t.Errorf("非 JSON 应带 ProviderError 且不可重试，实得 %v", err)
		}
	}

	// 业务失败带出 code 与 msg，未登记的码不得默认可重试。
	_, err := c.parse(200, nil, []byte(`{"code":"E1001","msg":"商户号无效"}`))
	var pe *pay.ProviderError
	if !errors.As(err, &pe) || pe.Code != "E1001" {
		t.Fatalf("业务失败未按 ProviderError 带出 code: %v", err)
	}
	if pe.Retryable {
		t.Error("未登记的业务码不得默认可重试")
	}
}

// TestTonCheckHeaderName 头名写错的表现是每次请求都在传输层失败。
func TestTonCheckHeaderName(t *testing.T) {
	for _, name := range []string{"X-Sign", "Authorization"} {
		if err := tonCheckHeaderName("ton", "auth_header", name); err != nil {
			t.Errorf("%q 应视为合法头名: %v", name, err)
		}
	}
	for _, name := range []string{"", "X Sign", "X:Sign", "X;Sign", "X\tSign", "X-Sign\r\n"} {
		if err := tonCheckHeaderName("ton", "auth_header", name); err == nil {
			t.Errorf("%q 应被拒绝", name)
		}
	}
}

// TestNewTonAccountClientFailsClosed 缺凭证/缺端点函数/坏头名/空基址都在构造期拒。
func TestNewTonAccountClientFailsClosed(t *testing.T) {
	cases := []struct {
		desc string
		opt  channelOptions
		want string
	}{
		{"空 secret_key", channelOptions{Name: "ton", Endpoint: tonAccountEndpoint}, "secret_key"},
		{"缺 endpoints 函数", channelOptions{Name: "ton", SecretKey: "k"}, "endpoints"},
		{"坏 auth_header 名", channelOptions{Name: "ton", SecretKey: "k", BaseURL: "http://127.0.0.1:1", AuthHeader: "X Sign", Endpoint: tonAccountEndpoint}, "auth_header"},
		{"空 base_url", channelOptions{Name: "ton", SecretKey: "k", BaseURL: "   ", Endpoint: tonAccountEndpoint}, "base_url"},
	}
	for _, c := range cases {
		got, err := newTonAccountClient(c.opt)
		if err == nil {
			t.Errorf("%s: 必须拒绝构造，实得 %+v", c.desc, got)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 错误文案应提到 %q，实得 %v", c.desc, c.want, err)
		}
	}

	c, err := newTonAccountClient(channelOptions{
		Name: "ton", SecretKey: "k", BaseURL: "http://127.0.0.1:1",
		AppID: " app_1 ", MerchantID: " m_9 ", Endpoint: tonAccountEndpoint,
	})
	if err != nil {
		t.Fatalf("合法配置应当构造成功: %v", err)
	}
	if c.opt.AuthHeader != tonDefaultAuthHeader {
		t.Errorf("缺省签名头应为 %s，实得 %q", tonDefaultAuthHeader, c.opt.AuthHeader)
	}
	if c.opt.AppID != "app_1" || c.opt.MerchantID != "m_9" {
		t.Errorf("凭证字段未去空白: %q / %q", c.opt.AppID, c.opt.MerchantID)
	}
}

// tonProbe 是假供应商：记下最后一次请求的路径、报文与签名头。
type tonProbe struct {
	mu       sync.Mutex
	hits     int
	lastPath string
	lastBody []byte
	lastSign string
}

func (p *tonProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.hits++
	p.lastPath = r.URL.Path
	p.lastBody = raw
	p.lastSign = r.Header.Get(tonDefaultAuthHeader)
	p.mu.Unlock()
	_, _ = io.WriteString(w, `{"code":"0","data":{"merchant_id":"merchant_9","currency":"TON",`+
		`"balance":100,"available":80,"frozen":20,"credit":5,"fee_rate_bps":30,"status":"frozen"}}`)
}

func (p *tonProbe) snapshot() (hits int, path string, body []byte, sign string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits, p.lastPath, p.lastBody, p.lastSign
}

func tonAccountEndpoint(name string) (string, bool) {
	if name == pay.EndpointAccount {
		return "/pay/account", true
	}
	return "", false
}

func newProbedTonAccountClient(t *testing.T, opt channelOptions) (*tonAccountClient, *tonProbe) {
	t.Helper()
	probe := &tonProbe{}
	srv := httptest.NewServer(probe)
	t.Cleanup(srv.Close)
	if opt.BaseURL == "" {
		opt.BaseURL = srv.URL
	}
	if opt.Timeout == 0 {
		opt.Timeout = 2 * time.Second
	}
	c, err := newTonAccountClient(opt)
	if err != nil {
		t.Fatalf("构造账户客户端失败: %v", err)
	}
	return c, probe
}

// TestTonQueryAccountSendsSignedRequest 端到端：路径、报文、签名、回执归一。
func TestTonQueryAccountSendsSignedRequest(t *testing.T) {
	c, probe := newProbedTonAccountClient(t, channelOptions{
		Name: "ton", AppID: "app_1", MerchantID: "merchant_9", SecretKey: "ton-s3cr3t",
		Endpoint: tonAccountEndpoint,
	})

	info, err := c.QueryAccount(context.Background(), &pay.AccountQuery{Currency: "TON"})
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Balance != 100 || info.Available != 80 || info.Frozen != 20 || info.Credit != 5 || info.FeeRateBps != 30 {
		t.Fatalf("回执字段未原样归一: %+v", info)
	}
	if info.Status != pay.AcctFrozen {
		t.Errorf("status=frozen 应归一为 frozen，实得 %q", info.Status)
	}
	if info.MerchantID != "merchant_9" || info.Currency != "TON" {
		t.Errorf("商户号/币种未带回: %+v", info)
	}

	hits, path, body, sign := probe.snapshot()
	if hits != 1 {
		t.Fatalf("读路径只该发一次，实发 %d 次", hits)
	}
	if path != "/pay/account" {
		t.Errorf("请求路径不符: %s", path)
	}
	const wantBody = `{"app_id":"app_1","merchant_id":"merchant_9","currency":"TON"}`
	if string(body) != wantBody {
		t.Errorf("报文不符:\n got %s\nwant %s", body, wantBody)
	}
	if want := expectedTonSign("ton-s3cr3t", "app_1", "merchant_9", "TON"); sign != want {
		t.Errorf("签名不符: got %s want %s", sign, want)
	}
}

// TestTonQueryAccountRejectsUnknownEndpoint 端点表里没有 account 时就地报错，不发请求。
func TestTonQueryAccountRejectsUnknownEndpoint(t *testing.T) {
	c, probe := newProbedTonAccountClient(t, channelOptions{
		Name: "ton", SecretKey: "k",
		Endpoint: func(string) (string, bool) { return "", false },
	})
	_, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
	if err == nil || !strings.Contains(err.Error(), "endpoints."+pay.EndpointAccount) {
		t.Fatalf("未配置端点应指名缺哪个端点，实得 %v", err)
	}
	if hits, _, _, _ := probe.snapshot(); hits != 0 {
		t.Errorf("缺端点时不该发出请求，实发 %d 次", hits)
	}
}

// TestTonToAccountInfoRejectsBadPayload 载荷解析不出来时报错而不是返回全零账户。
func TestTonToAccountInfoRejectsBadPayload(t *testing.T) {
	c := &tonAccountClient{name: "ton"}
	if _, err := c.toAccountInfo(nil, tonAccountRequest{}); err == nil {
		t.Error("空载荷必须报错")
	}
	if _, err := c.toAccountInfo(json.RawMessage(`"not-an-object"`), tonAccountRequest{}); err == nil {
		t.Error("载荷形态不符必须报错")
	}
}

// TestTonToAccountInfoFlagsUnknownStatus 未登记状态词归一为 unknown 并留痕，
// 不得滑到 active。回执缺商户号/币种时用请求值兜底。
func TestTonToAccountInfoFlagsUnknownStatus(t *testing.T) {
	c := &tonAccountClient{name: "ton"}

	info, err := c.toAccountInfo(json.RawMessage(`{"status":"mystery"}`), tonAccountRequest{})
	if err != nil {
		t.Fatalf("载荷可解析却报错: %v", err)
	}
	if info.Status != pay.AcctUnknown || !strings.Contains(info.RawNote, "mystery") {
		t.Errorf("未登记状态词应归一为 unknown 并留痕，实得 %q / %q", info.Status, info.RawNote)
	}

	info, err = c.toAccountInfo(json.RawMessage(`{"status":"1"}`), tonAccountRequest{MerchantID: "m_9", Currency: "TON"})
	if err != nil {
		t.Fatalf("载荷可解析却报错: %v", err)
	}
	if info.Status != pay.AcctActive {
		t.Errorf("status=1 应归一为 active，实得 %q", info.Status)
	}
	if info.MerchantID != "m_9" || info.Currency != "TON" {
		t.Errorf("回执缺字段未用请求值兜底: %+v", info)
	}
	if info.RawNote != "" {
		t.Errorf("已登记状态词不该留痕，实得 %q", info.RawNote)
	}
}
