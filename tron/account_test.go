package tron

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

// 本文件是供应商账户查询客户端的闸。
//
// 它承接的是 pay/provider_test.go 与 pay/provider_fix_test.go 里仍然成立的那部分：
// 签名算法、包络判定、报文保留字、构造期缺凭证拒绝、非 2xx 压过成功包络。
// 随供应商 HTTP 提现路径一起消失的用例（SignValues 十位定长、回执对账、
// parseStrict 白名单）不再重建——它们守护的代码已经删了，重建就成了给不存在的
// 路径维持一份看起来很像的测试。

// expectedAccountSign 是测试侧独立的 MD5 实现，只用来钉住「签了哪几个值、什么顺序」。
// 主闸是 TestSignMD5KnownVector 里的硬编码向量：那个值由 .NET MD5 独立算出，
// 不是拿被测实现的输出回填的。
func expectedAccountSign(secret string, values ...string) string {
	sum := md5.Sum([]byte(strings.Join(values, "") + secret))
	return hex.EncodeToString(sum[:])
}

// TestSignMD5KnownVector 签名算法必须逐字对得上供应商文档：
// 值直连拼接、无分隔符、末尾追加密钥、32 位小写。
//
// 向量来源：secret="s3cr3t-k3y"、values=["app_1","merchant_9","USDT"]，
// 拼接后 "app_1merchant_9USDTs3cr3t-k3y" 的 MD5 由 .NET MD5 独立算得。
// 换掉任何一个环节（加分隔符、把密钥放前面、大小写转换）都会让这个值变。
func TestSignMD5KnownVector(t *testing.T) {
	const want = "1c2b2dd05745b25a385cf4ce09a24978"
	if got := signMD5("s3cr3t-k3y", "app_1", "merchant_9", "USDT"); got != want {
		t.Fatalf("signMD5 结果漂移: got %s want %s", got, want)
	}
	if got := signMD5("s3cr3t-k3y", "app_1", "merchant_9", "USDT"); len(got) != 32 {
		t.Fatalf("签名必须是 32 位小写十六进制，实得 %q", got)
	}

	// 顺序敏感：字段顺序错一位就是供应商全量拒签，而本地永远算得过。
	if a, b := signMD5("k", "a", "b", "c"), signMD5("k", "b", "a", "c"); a == b {
		t.Fatal("换序后签名相同，说明拼接没有按登记顺序进行")
	}
	// 空字段按空串占位：可选字段留空时供应商仍在签名域里留了位置。
	if a, b := signMD5("k", "a", "", "c"), signMD5("k", "a", "c"); a != b {
		t.Fatal("空字段应等价于空串占位，不得整体消失")
	}
	// 密钥在末尾：与「密钥当前缀」的形式必须不同。
	if signMD5("k", "abc") == signMD5("abc", "k") {
		t.Fatal("密钥必须追加在末尾，不能与字段值互换")
	}
}

// TestEnvelopeSucceeded 包络判定：有 code 就按 code，只有 success 布尔形态才退回布尔。
// code 与 success 同时出现时以 code 为准——两个字段打架时选那个信息更多的。
func TestEnvelopeSucceeded(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		desc string
		env  supplierEnvelope
		want bool
	}{
		{"code=0 成功", supplierEnvelope{Code: "0"}, true},
		{"code 非 0 失败", supplierEnvelope{Code: "E1001"}, false},
		{"无 code，success=true", supplierEnvelope{Success: &yes}, true},
		{"无 code，success=false", supplierEnvelope{Success: &no}, false},
		{"两者皆无", supplierEnvelope{}, false},
		{"code 优先于 success", supplierEnvelope{Code: "E1", Success: &yes}, false},
		{"code=0 压过 success=false", supplierEnvelope{Code: "0", Success: &no}, true},
	}
	for _, c := range cases {
		if got := c.env.succeeded(); got != c.want {
			t.Errorf("%s: succeeded()=%v，期望 %v", c.desc, got, c.want)
		}
	}
}

// TestNon2xxBeatsSuccessEnvelope 非 2xx 先按传输层判掉：
// 网关 502 页面里偶然出现 "code":"0" 不代表供应商受理了一次查询。
// 认成功就等于把一次读故障记成了一次成功查询，余额会看起来是对的。
func TestNon2xxBeatsSuccessEnvelope(t *testing.T) {
	c := &accountClient{name: "usdt"}
	body := []byte(`{"code":"0","data":{"balance":1}}`)

	for _, status := range []int{500, 502, 503, 504, 429} {
		_, err := c.parse(status, nil, body)
		var pe *pay.ProviderError
		if !errors.As(err, &pe) {
			t.Errorf("%d: 期望 *pay.ProviderError，实得 %v", status, err)
			continue
		}
		if !pe.Retryable {
			t.Errorf("%d: 网关类状态码应可重试", status)
		}
		if pe.HTTPStatus != status {
			t.Errorf("%d: HTTPStatus 未带回，实得 %d", status, pe.HTTPStatus)
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
}

// TestParseEnvelopeDataRejects2xxNonJSON 2xx 却拿到非 JSON 必须判失败：
// 典型形态是反代换了一个 HTML 错误页，把它当成功会让下游读到全零账户。
func TestParseEnvelopeDataRejects2xxNonJSON(t *testing.T) {
	_, err := parseEnvelopeData("usdt", 200, []byte("<html>维护中</html>"))
	var pe *pay.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("2xx 非 JSON 应判失败，实得 %v", err)
	}
	if pe.Retryable {
		t.Error("内容是坏的不代表重发能变好，不得标为可重试")
	}
	if !strings.Contains(pe.Msg, "JSON") {
		t.Errorf("错误文案应说明不是合法 JSON，实得 %q", pe.Msg)
	}
}

// TestParseEnvelopeDataBusinessFailure 业务失败必须带出 code 与 msg，
// 且按白名单判可重试性（未登记的码不得重试）。
func TestParseEnvelopeDataBusinessFailure(t *testing.T) {
	_, err := parseEnvelopeData("usdt", 200, []byte(`{"code":"E1001","msg":"商户号无效"}`))
	var pe *pay.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("业务失败应转成 ProviderError，实得 %v", err)
	}
	if pe.Code != "E1001" || !strings.Contains(pe.Msg, "商户号无效") {
		t.Errorf("code/msg 未原样带回: %+v", pe)
	}
	if pe.Retryable {
		t.Error("未登记的业务码不得默认可重试")
	}

	// 成功包络但没有 data：把整个包络交给上层，而不是返回空载荷。
	data, err := parseEnvelopeData("usdt", 200, []byte(`{"code":"0"}`))
	if err != nil {
		t.Fatalf("成功包络不该报错: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("无 data 时也必须有载荷返回")
	}
}

// TestCheckHeaderName 头名写错的表现是每次请求都在传输层失败，报错还长在供应商侧。
func TestCheckHeaderName(t *testing.T) {
	for _, name := range []string{"X-Sign", "Authorization", "X_Sign_1"} {
		if err := checkHeaderName("usdt", "auth_header", name); err != nil {
			t.Errorf("%q 应视为合法头名: %v", name, err)
		}
	}
	for _, name := range []string{"", "X Sign", "X:Sign", "X;Sign", "X\tSign", "X-Sign\r\n", "X-Sign\n"} {
		if err := checkHeaderName("usdt", "auth_header", name); err == nil {
			t.Errorf("%q 应被拒绝", name)
		}
	}
}

// TestNewAccountClientFailsClosed 缺凭证、缺端点取值函数、坏头名、空基址
// 都必须在构造期拒绝：带着空密钥出去只会在供应商侧留下一堆验签失败日志，
// 本地却看起来「启动成功」。
func TestNewAccountClientFailsClosed(t *testing.T) {
	cases := []struct {
		desc string
		opt  channelOptions
		want string
	}{
		{"空 secret_key", channelOptions{Name: "usdt", Endpoint: accountEndpoint}, "secret_key"},
		{"缺 endpoints 函数", channelOptions{Name: "usdt", SecretKey: "k"}, "endpoints"},
		{"坏 auth_header 名", channelOptions{Name: "usdt", SecretKey: "k", BaseURL: "http://127.0.0.1:1", AuthHeader: "X Sign", Endpoint: accountEndpoint}, "auth_header"},
		{"空 base_url", channelOptions{Name: "usdt", SecretKey: "k", BaseURL: "   ", Endpoint: accountEndpoint}, "base_url"},
	}
	for _, c := range cases {
		got, err := newAccountClient(c.opt)
		if err == nil {
			t.Errorf("%s: 必须拒绝构造，实得 %+v", c.desc, got)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 错误文案应提到 %q，实得 %v", c.desc, c.want, err)
		}
		if strings.Contains(err.Error(), "secret-key-value") {
			t.Errorf("%s: 错误文案泄漏凭证: %v", c.desc, err)
		}
	}

	// 成功路径：缺省头名归为 X-Sign，app_id/merchant_id 去首尾空白。
	c, err := newAccountClient(channelOptions{
		Name: "usdt", SecretKey: "k", BaseURL: "http://127.0.0.1:1",
		AppID: " app_1 ", MerchantID: " m_9 ", Endpoint: accountEndpoint,
	})
	if err != nil {
		t.Fatalf("合法配置应当构造成功: %v", err)
	}
	if c.opt.AuthHeader != defaultAuthHeader {
		t.Errorf("缺省签名头应为 %s，实得 %q", defaultAuthHeader, c.opt.AuthHeader)
	}
	if c.opt.AppID != "app_1" || c.opt.MerchantID != "m_9" {
		t.Errorf("凭证字段未去空白: %q / %q", c.opt.AppID, c.opt.MerchantID)
	}
}

// signProbe 是假供应商：记下最后一次请求的路径、报文与签名头。
type signProbe struct {
	mu       sync.Mutex
	hits     int
	status   int
	body     string
	lastPath string
	lastBody []byte
	lastSign string
}

func (p *signProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.hits++
	p.lastPath = r.URL.Path
	p.lastBody = raw
	p.lastSign = r.Header.Get(defaultAuthHeader)
	status, body := p.status, p.body
	p.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = `{"code":"0","data":{"merchant_id":"merchant_9","currency":"USDT",` +
			`"balance":100,"available":80,"frozen":20,"credit":5,"fee_rate_bps":30,"status":"normal"}}`
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (p *signProbe) snapshot() (hits int, path string, body []byte, sign string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits, p.lastPath, p.lastBody, p.lastSign
}

// accountEndpoint 是端点取值函数的测试替身：只认 account。
func accountEndpoint(name string) (string, bool) {
	if name == pay.EndpointAccount {
		return "/pay/account", true
	}
	return "", false
}

func newProbedAccountClient(t *testing.T, opt channelOptions) (*accountClient, *signProbe) {
	t.Helper()
	probe := &signProbe{}
	srv := httptest.NewServer(probe)
	t.Cleanup(srv.Close)
	if opt.BaseURL == "" {
		opt.BaseURL = srv.URL
	}
	if opt.Timeout == 0 {
		opt.Timeout = 2 * time.Second
	}
	c, err := newAccountClient(opt)
	if err != nil {
		t.Fatalf("构造账户客户端失败: %v", err)
	}
	return c, probe
}

// TestQueryAccountSendsSignedRequest 端到端钉三件事：
// 报文里有通道特有字段、签名域跟着追加了同一个值、回执按四态归一。
func TestQueryAccountSendsSignedRequest(t *testing.T) {
	c, probe := newProbedAccountClient(t, channelOptions{
		Name: "usdt", AppID: "app_1", MerchantID: "merchant_9", SecretKey: "s3cr3t-k3y",
		Endpoint: accountEndpoint,
		Extras:   []extraField{{Name: "contract", Value: "TContract"}},
	})

	info, err := c.QueryAccount(context.Background(), &pay.AccountQuery{Currency: "USDT"})
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Balance != 100 || info.Available != 80 || info.Frozen != 20 || info.Credit != 5 || info.FeeRateBps != 30 {
		t.Fatalf("回执字段未原样归一: %+v", info)
	}
	if info.Status != pay.AcctActive {
		t.Errorf("status=normal 应归一为 active，实得 %q", info.Status)
	}
	if info.MerchantID != "merchant_9" || info.Currency != "USDT" {
		t.Errorf("商户号/币种未带回: %+v", info)
	}

	hits, path, body, sign := probe.snapshot()
	if hits != 1 {
		t.Fatalf("读路径只该发一次，实发 %d 次", hits)
	}
	if path != "/pay/account" {
		t.Errorf("请求路径不符: %s", path)
	}
	// 报文整体相等（map 序列化键序为字典序）：特有字段必须在同一层。
	const wantBody = `{"app_id":"app_1","contract":"TContract","currency":"USDT","merchant_id":"merchant_9"}`
	if string(body) != wantBody {
		t.Errorf("报文不符:\n got %s\nwant %s", body, wantBody)
	}
	// 签名域 = 报文已签名量 + 特有字段值，顺序照登记。
	if want := expectedAccountSign("s3cr3t-k3y", "app_1", "merchant_9", "USDT", "TContract"); sign != want {
		t.Errorf("签名不符: got %s want %s", sign, want)
	}
}

// TestQueryAccountReservedExtraSendsNothing 特有字段撞上已签名量时，
// 一个请求都不许发出去：允许撞名的话 Extra{"merchant_id":"X"} 就能覆盖报文里的
// 商户号，而签名域里签的还是原来那个值，验签看着完全正确。
func TestQueryAccountReservedExtraSendsNothing(t *testing.T) {
	for _, name := range []string{"app_id", "merchant_id", "currency"} {
		c, probe := newProbedAccountClient(t, channelOptions{
			Name: "usdt", SecretKey: "s3cr3t-k3y", Endpoint: accountEndpoint,
			Extras: []extraField{{Name: name, Value: "X"}},
		})
		if _, err := c.QueryAccount(context.Background(), &pay.AccountQuery{}); err == nil {
			t.Errorf("特有字段 %q 撞保留字必须拒", name)
		} else if strings.Contains(err.Error(), "s3cr3t-k3y") {
			t.Errorf("错误文案泄漏密钥: %v", err)
		}
		if hits, _, _, _ := probe.snapshot(); hits != 0 {
			t.Errorf("撞保留字时不该发出任何请求，实发 %d 次", hits)
		}
	}
}

// TestQueryAccountRejectsUnknownEndpoint 端点表里没有 account 时就地报错，
// 不发请求：供应商侧少配一个端点属于装配错误，重试没有意义。
func TestQueryAccountRejectsUnknownEndpoint(t *testing.T) {
	c, probe := newProbedAccountClient(t, channelOptions{
		Name: "usdt", SecretKey: "k",
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

// TestToAccountInfoRejectsBadPayload 载荷解析不出来时报错而不是返回全零账户：
// 全零会被下游读成「余额不足」，把一次查询故障伪装成一次业务拒绝。
func TestToAccountInfoRejectsBadPayload(t *testing.T) {
	c := &accountClient{name: "usdt"}
	if _, err := c.toAccountInfo(nil, accountRequest{}); err == nil {
		t.Error("空载荷必须报错")
	}
	if _, err := c.toAccountInfo(json.RawMessage(`"not-an-object"`), accountRequest{}); err == nil {
		t.Error("载荷形态不符必须报错")
	}
}

// TestToAccountInfoFlagsUnknownStatus 未登记的状态词归一为 unknown 并留痕，
// 不得滑到 active：给一个被冻结的商户号继续送单是最贵的误判。
func TestToAccountInfoFlagsUnknownStatus(t *testing.T) {
	c := &accountClient{name: "usdt"}

	info, err := c.toAccountInfo(json.RawMessage(`{"merchant_id":"m","currency":"USDT","status":"mystery"}`), accountRequest{})
	if err != nil {
		t.Fatalf("载荷可解析却报错: %v", err)
	}
	if info.Status != pay.AcctUnknown {
		t.Errorf("未登记状态词应归一为 unknown，实得 %q", info.Status)
	}
	if !strings.Contains(info.RawNote, "mystery") {
		t.Errorf("留痕应带上原始状态词，实得 %q", info.RawNote)
	}

	info, err = c.toAccountInfo(json.RawMessage(`{"status":"locked"}`), accountRequest{})
	if err != nil {
		t.Fatalf("载荷可解析却报错: %v", err)
	}
	if info.Status != pay.AcctFrozen {
		t.Errorf("locked 应归一为 frozen，实得 %q", info.Status)
	}
	if info.RawNote != "" {
		t.Errorf("已登记状态词不该留痕，实得 %q", info.RawNote)
	}

	// 回执缺商户号/币种时用请求里的值兜底，避免下游拿到空键去对不上账。
	info, err = c.toAccountInfo(json.RawMessage(`{"status":"1"}`), accountRequest{MerchantID: "m_9", Currency: "USDT"})
	if err != nil {
		t.Fatalf("载荷可解析却报错: %v", err)
	}
	if info.MerchantID != "m_9" || info.Currency != "USDT" {
		t.Errorf("回执缺字段未用请求值兜底: %+v", info)
	}
}

// TestMergeExtraFields 特有字段的合并口径：空名/空值跳过、顺序即签名域顺序、
// 撞保留字与内部重名都在这里拒。
func TestMergeExtraFields(t *testing.T) {
	fields, signs, err := mergeExtraFields([]extraField{
		{Name: "", Value: "drop"},
		{Name: "drop2", Value: ""},
		{Name: "contract", Value: "T1"},
		{Name: "channel", Value: "usdt"},
	})
	if err != nil {
		t.Fatalf("合法特有字段不该报错: %v", err)
	}
	if len(fields) != 2 || fields["contract"] != "T1" || fields["channel"] != "usdt" {
		t.Fatalf("字段合并结果不符: %v", fields)
	}
	if strings.Join(signs, ",") != "T1,usdt" {
		t.Fatalf("签名域必须按登记顺序追加，实得 %v", signs)
	}

	if _, _, err := mergeExtraFields([]extraField{{Name: "merchant_id", Value: "X"}}); err == nil {
		t.Error("撞已签名量必须拒")
	}
	if _, _, err := mergeExtraFields([]extraField{
		{Name: "contract", Value: "T1"},
		{Name: "contract", Value: "T2"},
	}); err == nil {
		t.Error("内部重名必须拒：报文留一个值、签名域却双签")
	}

	// 保留字集合必须与实际报文键名一致；漏一个就是留了一个可覆盖的洞。
	for _, key := range []string{"app_id", "merchant_id", "currency"} {
		if _, ok := accountPayloadKeys[key]; !ok {
			t.Errorf("保留字集合漏了报文键 %q", key)
		}
	}
}

// TestMarshalPayloadKeepsInt64Precision 大额金额经过「展开再序列化」不得丢精度。
// 如果哪天有人把 UseNumber 删掉，int64 会先变成 float64，最小单位上直接抹零。
func TestMarshalPayloadKeepsInt64Precision(t *testing.T) {
	type payload struct {
		Amount int64 `json:"amount"`
	}
	raw, err := marshalPayload(payload{Amount: 9007199254740993}, map[string]string{"contract": "T1"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(raw), `"amount":9007199254740993`) {
		t.Fatalf("int64 精度丢失: %s", raw)
	}
	if !strings.Contains(string(raw), `"contract":"T1"`) {
		t.Fatalf("特有字段未并进同一层: %s", raw)
	}
}
