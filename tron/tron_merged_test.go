package tron

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	sdkkeys "github.com/fbsobreira/gotron-sdk/pkg/keys"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/fbsobreira/gotron-sdk/pkg/standards/trc20enc"
	"github.com/fbsobreira/gotron-sdk/pkg/txcore"
	"golang.org/x/crypto/sha3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/internal/httpx"
	"touchgocore/ledger"
	"touchgocore/pay"
	"touchgocore/util"
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

// 本文件钉的是包内收口的 `checkOrderAddress`——它取代了原来登记在 pay 的
// ChainRule 表里的那条规则。
//
// 那张表的病不在「多一次查表」，而在「未登记即放行」：币种名拼错一个字母，
// 地址校验就静默失效，钱照样发得出去。现在规则是函数调用，调用点漏掉就是
// 编译错误。所以这里断言的对象必须是这个函数本身，而不是它内部用到的某一个
// rule——只测 rule 的话，把 checkOrderAddress 改成 `return nil` 依然全绿。

// TestCheckOrderAddressRejectsBadAddress 坏地址一定拒。
//
// 这一条同时也是「未登记不再放行」的替身：旧结构下忘了登记会让校验整段消失，
// 现在唯一的失效方式是有人把函数体改空，而这个用例会当场红。
func TestCheckOrderAddressRejectsBadAddress(t *testing.T) {
	bad := []struct {
		desc string
		addr string
	}{
		{"校验和不过", tronBogusChecksum},
		{"空串之外的垃圾", "not-a-tron-address"},
		{"版本字节不对的 BTC 地址", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"},
		{"长度断了一截", tronRangeAcct[:len(tronRangeAcct)-1]},
		{"含非 Base58 字符", strings.Replace(tronRangeAcct, "T", "0", 1)},
	}
	for _, c := range bad {
		o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: c.addr}}
		if err := checkOrderAddress(o); err == nil {
			t.Errorf("%s: 地址 %q 未被拦下", c.desc, c.addr)
		}
	}

	// 合法地址必须放行，否则上面那组「全拒」也可能来自一个永远报错的实现。
	if err := checkOrderAddress(&pay.PayOrder{OrderNo: "O2", Amount: 1, To: &pay.Party{Address: tronRangeAcct}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
}

// TestCheckOrderAddressRejectsMemo TRON 没有 memo 概念，填了就拒。
//
// 地址合法、只有 memo 有问题时仍然必须拒：这条链上 memo 送不出去，
// 吞掉它等于让上游以为「备注已经带上了」，而对方永远收不到那串认款码。
func TestCheckOrderAddressRejectsMemo(t *testing.T) {
	for _, memo := range []string{"123456", " 1 ", "复转到交易所", "12\n34"} {
		o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tronRangeAcct, Memo: memo}}
		if err := checkOrderAddress(o); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	// 纯空白（含换行）按没有备注：真正不发出去的那一步在构造报文时 trim。
	for _, memo := range []string{"   ", "\n", "\t\r"} {
		o := &pay.PayOrder{OrderNo: "O2", Amount: 1, To: &pay.Party{Address: tronRangeAcct, Memo: memo}}
		if err := checkOrderAddress(o); err != nil {
			t.Errorf("纯空白备注 %q 被当成有值: %v", memo, err)
		}
	}
}

// TestCheckOrderAddressLeavesEmptinessToCheckOrder 空地址在这一层放行，
// 因为「收款方必填」是契约层的事（pay.CheckOrder）。
//
// 两个地方都判一遍不是冗余加分，而是判据漂移的入口；但「分层之后是否还有人在判」
// 必须钉住——否则某天有人把 pay.CheckOrder 的那条删掉，两端都会安安静静地放行。
func TestCheckOrderAddressLeavesEmptinessToCheckOrder(t *testing.T) {
	empty := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{}}
	if err := checkOrderAddress(empty); err != nil {
		t.Fatalf("格式层不该判非空: %v", err)
	}
	if err := pay.CheckOrder(empty); err == nil {
		t.Fatal("契约层必须拦住空收款地址，否则空地址会一路发到出款")
	}

	// nil 订单同样不 panic：格式规则只回答「这个地址对不对」。
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单不该报错: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("无收款方的订单不该在格式层报错: %v", err)
	}
}

// TestCheckOrderAddressCoversBothRules 收口函数必须同时用上地址与 memo 两条规则。
//
// 手工组合一遍 TRONRule 再对比：两条规则各自都有正例与反例，缺哪一条都会有一格对不上。
// 明天有人来往规则里加第三条时，这里也会立刻变成「收口函数漏了一条」。
func TestCheckOrderAddressCoversBothRules(t *testing.T) {
	rule := TRONRule{}
	cases := []*pay.PayOrder{
		{To: &pay.Party{Address: tronRangeAcct}},
		{To: &pay.Party{Address: tronBogusChecksum}},
		{To: &pay.Party{Address: "garbage"}},
		{To: &pay.Party{Address: tronRangeAcct, Memo: "m"}},
		{To: &pay.Party{}},
		{},
	}
	for i, o := range cases {
		want := rule.CheckAddress(o)
		if want == nil {
			want = rule.CheckMemo(o)
		}
		got := checkOrderAddress(o)
		if (got == nil) != (want == nil) {
			t.Errorf("case %d: checkOrderAddress 与两条规则的组合不等价: got %v want %v", i, got, want)
		}
	}
}

// 本文件只测一件事：钱会不会被动两次。
//
// 每一个用例都断言「签名次数」和「广播次数」，而不只是返回值：链上出款的正确性
// 不看接口返回了什么状态码，看的是链上最终有几笔以本地址、本单号署名的转账。

const (
	fakeAPIKey  = "FAKE_TRONGRID_API_KEY"
	fakePrivKey = "1111111111111111111111111111111111111111111111111111111111111111"
)

// protoMarshalTx 把交易序列化成台账里那份字节，供「重广播的是不是同一份」比对。
func protoMarshalTx(tx *core.Transaction) ([]byte, error) { return proto.Marshal(tx) }

var testClock = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// fakeGateway 是可编程的链上网关替身，并记录每一类动作的次数。
type fakeGateway struct {
	buildTx    *core.Transaction
	buildErr   error
	buildCalls int

	broadcastRes  *broadcastResult
	broadcastErr  error
	broadcastRaw  [][]byte
	broadcastTxns []*core.Transaction

	receipt     *txReceipt
	receiptErr  error
	lookupCalls int

	tokenBal    int64
	tokenBalErr error
	acct        *accountState
	acctErr     error
	head        int64
	headErr     error

	outbound    []outboundTransfer
	outboundErr error
	closeCalls  int
}

func (f *fakeGateway) buildTransfer(context.Context, string, string, string, int64, int64) (*core.Transaction, error) {
	f.buildCalls++
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	// 每次回一份「新的」交易：真实节点会带上新的 refBlock 与 expiration，
	// 返回同一个指针会让「重建产出的是另一笔」这条断言变成假绿。
	raw := *f.buildTx.GetRawData()
	raw.RefBlockNum += int64(f.buildCalls)
	raw.Expiration += int64(f.buildCalls) * int64(time.Minute/time.Millisecond)
	return &core.Transaction{RawData: &raw}, nil
}

func (f *fakeGateway) broadcast(_ context.Context, tx *core.Transaction) (*broadcastResult, error) {
	raw, err := protoMarshalTx(tx)
	if err != nil {
		return nil, err
	}
	f.broadcastRaw = append(f.broadcastRaw, raw)
	f.broadcastTxns = append(f.broadcastTxns, tx)
	if f.broadcastErr != nil {
		return nil, f.broadcastErr
	}
	if f.broadcastRes != nil {
		return f.broadcastRes, nil
	}
	return &broadcastResult{Code: "SUCCESS", Accepted: true}, nil
}

func (f *fakeGateway) lookupTx(context.Context, string) (*txReceipt, error) {
	f.lookupCalls++
	if f.receiptErr != nil {
		return nil, f.receiptErr
	}
	if f.receipt != nil {
		return f.receipt, nil
	}
	return &txReceipt{}, nil
}

func (f *fakeGateway) tokenBalance(context.Context, string, string) (int64, error) {
	return f.tokenBal, f.tokenBalErr
}

func (f *fakeGateway) account(context.Context, string) (*accountState, error) {
	if f.acctErr != nil {
		return nil, f.acctErr
	}
	if f.acct != nil {
		return f.acct, nil
	}
	return &accountState{BalanceSun: defaultTRC20FeeLimitSun}, nil
}

func (f *fakeGateway) headBlock(context.Context) (int64, error) { return f.head, f.headErr }

func (f *fakeGateway) outboundTransfers(context.Context, string, string, int64) ([]outboundTransfer, error) {
	if f.outboundErr != nil {
		return nil, f.outboundErr
	}
	return f.outbound, nil
}

func (f *fakeGateway) close() error {
	f.closeCalls++
	return nil
}

// counters 记签名次数，是这批用例的核心断言对象。
type counters struct {
	mu    sync.Mutex
	calls int
}

func (c *counters) signer() signFunc {
	return func(tx *core.Transaction) (*core.Transaction, error) {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		// 复刻真实签名的形态：只往 signature 上追加，raw_data 一字不动
		//（txID 因此稳定，「签名后即可确定交易号」这条前提才测得到）。
		out := &core.Transaction{RawData: tx.GetRawData(), Signature: [][]byte{{0xAA}}}
		return out, nil
	}
}

func newTestChannel(gw chainGateway, j ledger.Journal, sign signFunc) *onchainChannel {
	// 默认让热钱包「钱够」：本文件测的是出资次数，不是余额门槛——后者单独有用例。
	if f, ok := gw.(*fakeGateway); ok && f.tokenBal == 0 && f.tokenBalErr == nil {
		f.tokenBal = 1 << 50
	}
	led := ledger.New("tron")
	if j != nil {
		led.SetJournal(j)
	}
	return &onchainChannel{
		gw:     gw,
		sign:   sign,
		opt:    channelOptions{Name: "usdt", Driver: DriverTronGrid},
		chain:  &chainConfig{Node: "grpc.example:50051", WalletAddress: tronRangeAcct, Contract: tronOfficialUSDT, Network: pay.NetworkMainnet, APIKey: fakeAPIKey, PrivateKey: fakePrivKey},
		ledger: led,
		wallet: tronRangeAcct,
		nowFn:  func() time.Time { return testClock },
	}
}

func buildableTx() *core.Transaction {
	return &core.Transaction{RawData: &core.TransactionRaw{
		RefBlockNum: 12345,
		Expiration:  testClock.Add(time.Minute).UnixMilli(),
		Timestamp:   testClock.UnixMilli(),
		FeeLimit:    defaultTRC20FeeLimitSun,
	}}
}

func order() *pay.PayOrder {
	return &pay.PayOrder{OrderNo: "WD-001", Amount: 3_000_000_000, Currency: pay.CurrencyUSDT, To: &pay.Party{Address: tronAllZeroAcct}}
}

func sameRaw(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUncertainBroadcastNeverResigns 广播结果未知时，重入同一单号只能查证，不得再签一笔。
func TestUncertainBroadcastNeverResigns(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), broadcastErr: errors.New("rpc error: deadline exceeded")}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash == "" {
		t.Fatalf("不确定必须给 UNKNOWN 并交出 txid，实得 %+v", res)
	}
	firstTxid := res.TxHash

	for i := 0; i < 3; i++ {
		gw.broadcastErr = errors.New("transport: connection reset")
		again, err := ch.Withdraw(context.Background(), order())
		if err != nil {
			t.Fatal(err)
		}
		if again.TxHash != firstTxid {
			t.Fatalf("第 %d 次重入换了交易号：%s vs %s", i+1, again.TxHash, firstTxid)
		}
	}
	if c.calls != 1 {
		t.Fatalf("同一单号签了 %d 笔，只能 1 笔", c.calls)
	}
	if gw.buildCalls != 1 {
		t.Fatalf("同一单号构造了 %d 次，只能 1 次", gw.buildCalls)
	}
	if len(gw.broadcastRaw) != 1 {
		t.Fatalf("广播了 %d 次，不确定态只能等查证", len(gw.broadcastRaw))
	}
}

// TestPreparedEntryRebroadcastsIdenticalBytes 台账里 prepared（签名完成但没交出去）的条目
// 重入时广播的是逐字相同的那一份，因此节点侧天然去重、不会产生第二笔。
func TestPreparedEntryRebroadcastsIdenticalBytes(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	ch := newTestChannel(gw, nil, c.signer())

	tx, err := gw.buildTransfer(context.Background(), ch.wallet, order().To.Address, ch.chain.Contract, order().Amount, ch.feeLimit())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := ch.assetOf(order())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ch.prepare(order(), tx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != ledger.StatePrepared {
		t.Fatalf("新条目应为 prepared，实得 %s", entry.State)
	}
	if _, err := ch.broadcastEntry(context.Background(), entry, testClock); err != nil {
		t.Fatal(err)
	}
	if len(gw.broadcastRaw) != 1 || !sameRaw(gw.broadcastRaw[0], entry.RawTx) {
		t.Fatalf("广播的不是台账里那一份: %x / %x", gw.broadcastRaw[0], entry.RawTx)
	}
	// 重入同一单号：已进 stateBroadcast，只能查证，不再广播。
	if _, err := ch.Withdraw(context.Background(), order()); err != nil {
		t.Fatal(err)
	}
	if len(gw.broadcastRaw) != 1 || c.calls != 1 {
		t.Fatalf("重入后广播 %d 次、签名 %d 次", len(gw.broadcastRaw), c.calls)
	}
}

// TestSettledOrderRefusesSecondAttempt 链上已有终局的单号再被调用时必须拒绝，而不是再出资。
func TestSettledOrderRefusesSecondAttempt(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{Found: true, InBlock: true, Success: true, BlockNumber: 100}, head: 106}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusSuccess {
		t.Fatalf("应当成功，实得 %+v", res)
	}
	if res.Confirmations == nil || *res.Confirmations != 6 {
		t.Fatalf("确认数应为 6，实得 %+v", res.Confirmations)
	}
	if res.FeeCurrency != pay.CurrencyTRX {
		t.Fatalf("gas 计价币种应为 TRX，实得 %s", res.FeeCurrency)
	}
	again, err := ch.Withdraw(context.Background(), order())
	if !errors.Is(err, ErrOrderAlreadySettled) || again != nil {
		t.Fatalf("终局单号再出资必须被拒，实得 res=%v err=%v", again, err)
	}
	if len(gw.broadcastRaw) != 1 || c.calls != 1 {
		t.Fatalf("终局后又动了钱: 广播 %d 次、签名 %d 次", len(gw.broadcastRaw), c.calls)
	}
}

// TestNodeRejectionAllowsRebuild 节点在入池前拒收时钱一定没动，同单号允许重建。
//
// 这条是「永不签第二笔」的唯一例外，必须显式测出来：把它也一并按死会让所有被拒的单号卡死，
// 而把它放开得太大就是双付。
func TestNodeRejectionAllowsRebuild(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), broadcastRes: &broadcastResult{Code: "TAPOS_ERROR", Rejected: true}}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed {
		t.Fatalf("入池前被拒应给 FAILED（钱没动），实得 %+v", res)
	}
	gw.broadcastRes = &broadcastResult{Code: "SUCCESS", Accepted: true}
	if _, err := ch.Withdraw(context.Background(), order()); err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 || len(gw.broadcastRaw) != 2 {
		t.Fatalf("被拒后重入应重建一次: 签名 %d、广播 %d", c.calls, len(gw.broadcastRaw))
	}
	// 两次广播的是不同字节（第二笔是新的 refBlock/expiration），但都只有一笔在链上：
	// 第一笔被节点明确拒于池外，不存在双付。
	if sameRaw(gw.broadcastRaw[0], gw.broadcastRaw[1]) {
		t.Error("重建应当产出新的交易，而不是逐字重发被拒那一笔")
	}
}

// TestPrechecksRunBeforeSigning 余额与 gas 的门槛排在签名之前：一条都不许漏过去。
func TestPrechecksRunBeforeSigning(t *testing.T) {
	cases := map[string]struct {
		gw   *fakeGateway
		want string
	}{
		"代币余额不足":  {&fakeGateway{buildTx: buildableTx(), tokenBal: 1}, "insufficient_token"},
		"读不出代币余额": {&fakeGateway{buildTx: buildableTx(), tokenBalErr: errors.New("boom")}, "balance_unread"},
		"gas 不足":  {&fakeGateway{buildTx: buildableTx(), acct: &accountState{BalanceSun: 1}}, "insufficient_gas"},
		"读不出账户":   {&fakeGateway{buildTx: buildableTx(), acctErr: errors.New("boom")}, "account_unread"},
	}
	for name, cs := range cases {
		c := &counters{}
		ch := newTestChannel(cs.gw, nil, c.signer())
		res, err := ch.Withdraw(context.Background(), order())
		if err == nil || res != nil {
			t.Errorf("%s: 应当拒绝，实得 res=%v err=%v", name, res, err)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 报错里找不到 %s: %v", name, cs.want, err)
		}
		if c.calls != 0 || cs.gw.buildCalls != 0 || len(cs.gw.broadcastRaw) != 0 {
			t.Errorf("%s: 前置闸没拦住，签名 %d 次、构造 %d 次、广播 %d 次", name, c.calls, cs.gw.buildCalls, len(cs.gw.broadcastRaw))
		}
	}
}

// TestJournalWriteFailureAbortsBroadcast 台账写不进去时不许动钱。
func TestJournalWriteFailureAbortsBroadcast(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	ch := newTestChannel(gw, failingJournal{}, c.signer())
	_, err := ch.Withdraw(context.Background(), order())
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("记账失败必须放弃广播，实得 %v", err)
	}
	if len(gw.broadcastRaw) != 0 || c.calls != 1 {
		t.Fatalf("签名 %d 次、广播 %d 次：应当签完就停在记账前", c.calls, len(gw.broadcastRaw))
	}
}

type failingJournal struct{}

func (failingJournal) Put(*ledger.Entry) error { return errors.New("库挂了") }
func (failingJournal) Get(string, string) (*ledger.Entry, bool) {
	return nil, false
}

// firstPutFailsJournal 只让第一次记账失败，后面的写入照旧成功。
//
// 存在的必要是把「签名后先记账」这一道单独钉住：出款路径上有两道记账闸
// （prepare 的 prepared 落账、broadcastEntry 的 broadcast 落账），任一道失效另一道都能
// 让「记不上就别动钱」这条不变量仍然成立，于是一把全失败的替身分不出是哪道在起作用——
// 用第一道失败、第二道成功的替身才能把第一道单独杀掉。
type firstPutFailsJournal struct {
	mu   sync.Mutex
	puts int
	seen []*ledger.Entry
}

func (j *firstPutFailsJournal) Put(entry *ledger.Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.puts++
	j.seen = append(j.seen, entry)
	if j.puts == 1 {
		return errors.New("第一次落账失败")
	}
	return nil
}

func (j *firstPutFailsJournal) Get(string, string) (*ledger.Entry, bool) { return nil, false }

// TestPrepareMustBeJournaledBeforeBroadcast 签名完成后落账失败，就不许把这一笔交出去。
func TestPrepareMustBeJournaledBeforeBroadcast(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	j := &firstPutFailsJournal{}
	ch := newTestChannel(gw, j, c.signer())

	_, err := ch.Withdraw(context.Background(), order())
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("第一道记账闸失守时应当报错，实得 %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("签名次数 %d：记账失败前就该只签一次", c.calls)
	}
	if len(gw.broadcastRaw) != 0 {
		t.Fatalf("记不上账仍然广播了 %d 次", len(gw.broadcastRaw))
	}
	// 失败的那一次必须是被拒的 prepared 落账，而不是后面那次状态推进。
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.puts != 1 || j.seen[0].State != ledger.StatePrepared {
		t.Fatalf("应当停在第一次 prepared 落账，实得 puts=%d state=%s", j.puts, j.seen[0].State)
	}
}

// TestRebuildAfterLedgerLossLooksUpChainFirst 台账丢了（重启）时，先按收端+金额反查链上。
//
// F2：命中只能证明「历史上有过同址同额的一笔」，不能证明就是这一单。正确处置是
// 禁止广播并回 UNKNOWN（带候选 txid），候选不落账、不查证、不结算。
func TestRebuildAfterLedgerLossLooksUpChainFirst(t *testing.T) {
	c := &counters{}
	o := order()
	gw := &fakeGateway{buildTx: buildableTx(), outbound: []outboundTransfer{{
		TxID: strings.Repeat("ab", 32), To: o.To.Address, Amount: o.Amount, TimestampMS: testClock.UnixMilli(),
	}}}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(gw.broadcastRaw) != 0 {
		t.Fatalf("反查命中后仍广播了 %d 次，这就是双付", len(gw.broadcastRaw))
	}
	if c.calls != 0 || gw.buildCalls != 0 {
		t.Fatalf("反查命中后仍组了笔: 签名 %d、构造 %d", c.calls, gw.buildCalls)
	}
	if res.TxHash != gw.outbound[0].TxID {
		t.Fatalf("结果应带回候选的 txid 供对账，实得 %+v", res)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("候选不能说明属于本单，必须回 UNKNOWN，实得 %s", res.Status)
	}
	if !strings.Contains(res.RawNote, "无法证明") {
		t.Fatalf("回执没有说明候选不能归属本单: %q", res.RawNote)
	}
	// 候选绝不能落账：写成 broadcast 后，下一次 query/confirm 会把它结算成 SUCCESS。
	if _, found := ch.ledger.Get(o.OrderNo); found {
		t.Error("反查候选被写进了台账，下一次 confirm 会把别人的转账结算成本单成功")
	}
}

// TestReverseLookupFailureAbortsBroadcast 反查读不动时按「不能确认」处理：宁可不发。
func TestReverseLookupFailureAbortsBroadcast(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), outboundErr: errNoTransferListing}
	c := &counters{}
	ch := newTestChannel(gw, nil, c.signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err == nil || res != nil {
		t.Fatalf("反查不可用应当拒绝广播，实得 res=%v err=%v", res, err)
	}
	if !pay.IsProviderRetryable(err) {
		t.Error("这一类失败应标记为可重试（下一次重入会再查一遍）")
	}
	if len(gw.broadcastRaw) != 0 {
		t.Fatal("反查不可用却广播了")
	}
}

// TestReverseLookupTruncatedPageAbortsBroadcast 反查取满一页却没命中时，按「没能查全」
// 拒绝广播：第 outboundLimit 条之后的转出读不到，「没在这一页里」不等于「没有」——
// 台账丢失场景下照发就是赌双付，与读不懂同等处理。
func TestReverseLookupTruncatedPageAbortsBroadcast(t *testing.T) {
	full := make([]outboundTransfer, outboundLimit)
	for i := range full {
		// 每一条都对不上本单（收端与金额都不同），而页面是满的。
		full[i] = outboundTransfer{
			TxID: strings.Repeat("cd", 32), To: "TSomebodyElse", Amount: int64(i + 1),
			TimestampMS: testClock.UnixMilli(),
		}
	}
	gw := &fakeGateway{buildTx: buildableTx(), outbound: full}
	c := &counters{}
	ch := newTestChannel(gw, nil, c.signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err == nil || res != nil {
		t.Fatalf("一页取满且未命中应当拒绝广播，实得 res=%v err=%v", res, err)
	}
	if !pay.IsProviderRetryable(err) {
		t.Error("查不全应标记为可重试（下一次重入会把窗口或分页再查一遍）")
	}
	if len(gw.broadcastRaw) != 0 || c.calls != 0 || gw.buildCalls != 0 {
		t.Fatal("查不全却动了链上")
	}
}

// TestQueryOrderReportsLedgerStateHonestly 查单按台账状态给结论：签了没广播的不能说成
// 「广播已交出」，节点拒过的要给 FAILED（与广播那一次的结论一致）而不是永远 PENDING。
func TestQueryOrderReportsLedgerStateHonestly(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{}}
	ch := newTestChannel(gw, nil, (&counters{}).signer())

	prepared := &ledger.Entry{
		OrderNo: "WD-P", TxID: strings.Repeat("11", 32), State: ledger.StatePrepared,
		Amount: 1, ExpiresAtMS: testClock.Add(time.Minute).UnixMilli(),
	}
	if err := ch.ledger.Put(prepared); err != nil {
		t.Fatal(err)
	}
	res, err := ch.QueryOrder(context.Background(), "WD-P")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending || !strings.Contains(res.RawNote, "未广播") {
		t.Fatalf("prepared 条目的查单结论应说明未广播，实得 %+v", res)
	}

	rejected := &ledger.Entry{OrderNo: "WD-R", TxID: strings.Repeat("22", 32), State: ledger.StateRejected, Amount: 1}
	if err := ch.ledger.Put(rejected); err != nil {
		t.Fatal(err)
	}
	res, err = ch.QueryOrder(context.Background(), "WD-R")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed || !strings.Contains(res.RawNote, "拒") {
		t.Fatalf("rejected 条目的查单结论应为失败且说明节点拒过，实得 %+v", res)
	}
}

// TestConcurrentWithdrawSignsOnce 同单号并发进来时，分段锁必须让第二个人看见第一个人的台账。
func TestConcurrentWithdrawSignsOnce(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	ch := newTestChannel(gw, nil, c.signer())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ch.Withdraw(context.Background(), order())
		}()
	}
	wg.Wait()
	if c.calls != 1 {
		t.Fatalf("8 个并发调用签了 %d 笔", c.calls)
	}
	if len(gw.broadcastRaw) != 1 {
		t.Fatalf("8 个并发调用广播了 %d 次", len(gw.broadcastRaw))
	}
}

// TestExpiredPendingBroadcastSettlesFailed 过期且链上查不到时给 FAILED：这一笔已不可能再上链。
func TestExpiredPendingBroadcastSettlesFailed(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx()}
	c := &counters{}
	ch := newTestChannel(gw, nil, c.signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending {
		t.Fatalf("刚广播完应是 pending，实得 %+v", res)
	}
	ch.nowFn = func() time.Time { return testClock.Add(2 * time.Minute) }
	again, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != pay.StatusFailed {
		t.Fatalf("过期未上链应给 FAILED，实得 %+v", again)
	}
	if len(gw.broadcastRaw) != 1 || c.calls != 1 {
		t.Fatalf("过期后仍动了笔: 广播 %d、签名 %d", len(gw.broadcastRaw), c.calls)
	}
}

// TestReceiptReadFailureGivesUnknown 回执读不动时不能推断成败。
func TestReceiptReadFailureGivesUnknown(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receiptErr: errors.New("node unavailable")}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash == "" {
		t.Fatalf("查证失败必须给 UNKNOWN 并带 txid，实得 %+v", res)
	}
}

// TestFailedReceiptIsTerminalButGasBurned 进了块却没转成功 → FAILED，且写明 gas 已烧。
func TestFailedReceiptIsTerminalButGasBurned(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{Found: true, InBlock: true, Success: false, VerdictKnown: true, BlockNumber: 10, FeeSun: 13_000_000, ResultCode: "FAILED"}}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed || res.Fee != 13_000_000 || !strings.Contains(res.RawNote, "gas 已消耗") {
		t.Fatalf("回执判定为未成功时要把烧掉的手续费一起交出去: %+v", res)
	}
}

// TestUnreadableVerdictIsUnknownNotFailed 进了块但读不出执行结果时，只能给 UNKNOWN。
//
// 这条闸来自真机复现：USDT 的 transfer 不返回 bool，回执里的执行结果字段会随节点
// 版本省略。判成 FAILED 上游就会重下一单，而链上那一笔可能已经把币转出去了——
// 那是业务层的双付，比卡一单贵得多。
func TestUnreadableVerdictIsUnknownNotFailed(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{
		Found: true, InBlock: true, BlockNumber: 10, Success: false, VerdictKnown: false, ResultCode: "/",
	}}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash == "" {
		t.Fatalf("判定不明必须给 UNKNOWN 并带 txid，实得 %+v", res)
	}
	// 不落终局：等节点把执行结果回填后，同单号再查一次就能翻成成功。
	entry, found := ch.ledger.Get(order().OrderNo)
	if !found || entry.Terminal() {
		t.Fatalf("判定不明就被打了终局，这一单再也没机会翻案: %+v", entry)
	}
	gw.receipt = &txReceipt{Found: true, InBlock: true, BlockNumber: 10, Success: true, VerdictKnown: true, ResultCode: "SUCCESS"}
	gw.head = 12
	again, err := ch.QueryOrder(context.Background(), order().OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != pay.StatusSuccess {
		t.Fatalf("回执补齐后应翻成成功，实得 %+v", again)
	}
	if c.calls != 1 || len(gw.broadcastRaw) != 1 {
		t.Fatalf("翻案过程中不该再动钱: 签名 %d 次、广播 %d 次", c.calls, len(gw.broadcastRaw))
	}
}

// TestQueryOrderWithoutLedgerStaysUnknown 查单查不到单号时给 UNKNOWN，不给成败结论。
func TestQueryOrderWithoutLedgerStaysUnknown(t *testing.T) {
	ch := newTestChannel(&fakeGateway{}, nil, (&counters{}).signer())
	res, err := ch.QueryOrder(context.Background(), "WD-999")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("台账没有这一单号时应给 UNKNOWN，实得 %+v", res)
	}
	if _, err := ch.QueryOrder(context.Background(), "  "); err == nil {
		t.Fatal("空单号必须报错")
	}
}

// TestWithdrawGatesRunBeforeAnyChainCall 地址格式、memo、币种、网络四道闸都排在链上调用之前。
func TestWithdrawGatesRunBeforeAnyChainCall(t *testing.T) {
	mutate := func(f func(*pay.PayOrder)) *pay.PayOrder {
		o := order()
		f(o)
		return o
	}
	cases := map[string]*pay.PayOrder{
		"收款地址校验和不过": mutate(func(o *pay.PayOrder) { o.To.Address = tronBogusChecksum }),
		"带了备注":      mutate(func(o *pay.PayOrder) { o.To.Memo = "order ref" }),
		"网络与通道不符":   mutate(func(o *pay.PayOrder) { o.To.Network = pay.NetworkShasta }),
		"币种不是 USDT": mutate(func(o *pay.PayOrder) { o.Currency = pay.CurrencyTRX }),
		"金额为 0":     mutate(func(o *pay.PayOrder) { o.Amount = 0 }),
		"收款地址为空":    mutate(func(o *pay.PayOrder) { o.To.Address = "" }),
	}
	for name, o := range cases {
		gw := &fakeGateway{buildTx: buildableTx()}
		c := &counters{}
		ch := newTestChannel(gw, nil, c.signer())
		if _, err := ch.Withdraw(context.Background(), o); err == nil {
			t.Errorf("%s: 应当被拒", name)
			continue
		}
		if gw.buildCalls != 0 || c.calls != 0 || gw.lookupCalls != 0 {
			t.Errorf("%s: 拒单前碰了链上: build=%d sign=%d lookup=%d", name, gw.buildCalls, c.calls, gw.lookupCalls)
		}
	}
}

// TestNoSecretsInAnyOutcome 私钥与 API Key 不得出现在返回值、错误或回执摘要里。
func TestNoSecretsInAnyOutcome(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), broadcastErr: fmt.Errorf("dial %s failed: connection refused", fakeAPIKey)}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	res, err := ch.Withdraw(context.Background(), order())
	for _, s := range []string{fakePrivKey, fakeAPIKey} {
		if err != nil && strings.Contains(err.Error(), s) {
			t.Errorf("错误文案含敏感值 %s: %v", s, err)
		}
		if res != nil && strings.Contains(res.RawNote, s) {
			t.Errorf("RawNote 含敏感值 %s: %s", s, res.RawNote)
		}
	}
	if res == nil {
		t.Fatal("广播不确定也要给出回执，好让对账有 txid 可用")
	}
}

// TestAssemblyRejectsHalfConfiguredChannel 装配是包内直接构造，没有注册表可漏；
// 但「半成品通道」这道闸必须还在：缺 chain/node 时就地拒。热钱包地址与私钥
// 现在是按单参数：配置里不填允许装配，但没带 From 私钥的单必须在签名前拒。
func TestAssemblyRejectsHalfConfiguredChannel(t *testing.T) {
	if _, err := newOnchainChannel(channelOptions{Name: "usdt", Driver: DriverTronGrid}); err == nil {
		t.Fatal("缺 chain 选项时必须拒绝装配，不能返回半成品通道")
	}
	ch, err := newOnchainChannel(channelOptions{Name: "usdt", Driver: DriverTronGrid,
		Chain: &chainConfig{Node: "grpc.example:50051"}})
	if err != nil {
		t.Fatalf("按单携带出款参数时，配置无钱包/私钥也应允许装配: %v", err)
	}
	_, werr := ch.Withdraw(context.Background(), &pay.PayOrder{
		OrderNo: "O-ASSET-NOKEY", Amount: 1, Currency: pay.CurrencyUSDT,
		To: &pay.Party{Kind: pay.PartyWallet, Address: tronRangeAcct},
	})
	if werr == nil || !strings.Contains(werr.Error(), "缺少出款私钥") {
		t.Fatalf("没带 From 私钥的单必须在签名前拒单，实得 %v", werr)
	}
}

// TestQueryOrderTakesLedgerLock 查单必须取同单号的台账锁：与同一单号的 Withdraw 串行，
// 否则 confirm 里的 settle 会与出款的状态迁移并发盖写台账（把 settled 覆盖回 broadcast 一类）。
// 本用例持住同单号的台账锁，查单应被阻塞到锁释放为止；不同单号不受影响。
// 旧实现（QueryOrder 直接 Get+confirm、不取 WithLock）会让同单号查单立即返回 → 本用例红。
func TestQueryOrderTakesLedgerLock(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{}}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	for _, on := range []string{"WD-LOCK", "WD-OTHER"} {
		if err := ch.ledger.Put(&ledger.Entry{
			OrderNo: on, TxID: strings.Repeat("11", 32), State: ledger.StatePrepared,
			Amount: 1, ExpiresAtMS: testClock.Add(time.Minute).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = ch.ledger.WithLock("WD-LOCK", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// 同单号：查单必须被台账锁挡住。
	sameDone := make(chan struct{})
	go func() {
		_, _ = ch.QueryOrder(context.Background(), "WD-LOCK")
		close(sameDone)
	}()
	select {
	case <-sameDone:
		t.Fatal("同单号查单没有被台账锁挡住（绕锁查证，settle 会与 Withdraw 并发盖写）")
	case <-time.After(150 * time.Millisecond):
	}

	// 不同单号：不能被这把锁挡住。
	otherDone := make(chan error, 1)
	go func() {
		_, err := ch.QueryOrder(context.Background(), "WD-OTHER")
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("不同单号查单失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("不同单号查单被同单号的台账锁误挡")
	}

	close(release)
	select {
	case <-sameDone:
	case <-time.After(2 * time.Second):
		t.Fatal("释放台账锁后同单号查单没有完成")
	}
}

// 本文件全部离线。calldata、选择子、交易号与地址形态各有两到三份独立来源可比：
// 本包实现、gotron-sdk 的 TRC20 编码件（pkg/standards/trc20enc）、SDK 的 txID 计算，
// 以及从函数签名字面量现算的 keccak256。参照物一律取 TRON 侧的件，不借以太坊的 ABI
// 编码器：这条链上的标准叫 TRC20，拿 ERC20 的库当 oracle 等于把「同构」误当成「同源」，
// 而真出问题时该被怀疑的正是这层假设。编码错的代价是钱进了没人拥有的地址，
// 所以这里不留「看着像对」的余地。

// 夹具沿用本包 tron_test.go 里造好的向量：tronOfficialUSDT（主网合约）、
// tronRangeAcct（20 字节账户哈希造出的合法地址，用来证明编码没把地址写死）、
// tronBogusChecksum（长度与版本字节全对、只有校验和不过）。

func TestSelectorMatchesKeccakOfSignature(t *testing.T) {
	// 选择子的来历只能由函数签名字面量现算来验：写死的常量可能抄自一份错文档，
	// 而那要等到第一笔转账才会暴露。
	for _, cs := range []struct {
		signature string
		want      string
		sdk       string
	}{
		{"transfer(address,uint256)", trc20TransferSelector, trc20enc.SelectorTransfer},
		{"balanceOf(address)", trc20BalanceSelector, trc20enc.SelectorBalanceOf},
	} {
		h := sha3.NewLegacyKeccak256()
		h.Write([]byte(cs.signature))
		got := hex.EncodeToString(h.Sum(nil)[:4])
		if got != cs.want {
			t.Errorf("%s 算出的选择子是 %s，代码里写的是 %s", cs.signature, got, cs.want)
		}
		if got != cs.sdk {
			t.Errorf("%s 的选择子与 SDK 的 TRC20 常量不符: 本包 %s / trc20enc %s", cs.signature, got, cs.sdk)
		}
	}
}

func TestTRC20CalldataAgainstReferenceEncoder(t *testing.T) {
	for _, amount := range []int64{1, 1_000_000, 3_000_000_000, 9223372036854775807} {
		for _, to := range []string{tronOfficialUSDT, tronRangeAcct} {
			want, err := referenceTransferCalldata(t, to, amount)
			if err != nil {
				t.Fatal(err)
			}
			got, err := trc20TransferCalldata(to, amount)
			if err != nil {
				t.Fatalf("amount=%d: %v", amount, err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("to=%s amount=%d 的调用数据与参照编码器不一致\n got=%x\nwant=%x", to, amount, got, want)
			}
		}
	}
}

// referenceTransferCalldata 用 SDK 的 TRC20 编码件算同一笔转账，作为独立参照物。
func referenceTransferCalldata(t *testing.T, to string, amount int64) ([]byte, error) {
	t.Helper()
	raw, err := decodeTRONAddress(to)
	if err != nil {
		return nil, err
	}
	hexed, err := trc20enc.EncodeTransferCall(address.BytesToAddress(raw[:21]), big.NewInt(amount))
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(hexed, "0x"))
}

func TestTRC20CalldataShape(t *testing.T) {
	data, err := trc20TransferCalldata(tronOfficialUSDT, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != trc20CalldataLen {
		t.Fatalf("transfer 调用数据应为 %d 字节，实得 %d", trc20CalldataLen, len(data))
	}
	if hex.EncodeToString(data[:4]) != trc20TransferSelector {
		t.Errorf("前 4 字节不是选择子: %x", data[:4])
	}
	if !bytes.Equal(data[4:16], make([]byte, 12)) {
		t.Errorf("地址字左侧没补零: %x", data[4:16])
	}
	want, err := address.Base58ToAddress(tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	if got, want2 := hex.EncodeToString(data[16:36]), strings.TrimPrefix(want.Hex(), "0x41"); got != want2 {
		t.Errorf("地址字 %s 与合约地址的 EVM 形态 %s 不符", got, want2)
	}
	// 金额字必须是最小单位原样编码：这里倍乘一次就是 10^6 倍的事故。
	if got := new(big.Int).SetBytes(data[36:]); got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("金额字被改写成 %s", got)
	}
}

func TestTRC20CalldataRejectsBadInput(t *testing.T) {
	if _, err := trc20TransferCalldata(tronOfficialUSDT, -1); err == nil {
		t.Fatal("负金额必须拒绝：它会补成一串全 F 的 32 字节")
	}
	if _, err := trc20TransferCalldata("not-a-tron-address", 1); err == nil {
		t.Fatal("非法收款地址必须拒绝")
	}
}

func TestTxIDMatchesSDKImplementation(t *testing.T) {
	// 本包的 sha256(raw_data) 与 SDK 的 RecomputeTxID 来自不同代码库，必须逐字相等。
	// 广播结果不确定时手里这个号就是查证的唯一把手，它不能与链上那个号是两个。
	for i, tx := range sampleTransactions() {
		got, err := txIDOf(tx)
		if err != nil {
			t.Fatal(err)
		}
		ext := &api.TransactionExtention{Transaction: tx}
		if err := txcore.RecomputeTxID(ext); err != nil {
			t.Fatal(err)
		}
		if want := hex.EncodeToString(ext.GetTxid()); got != want {
			t.Errorf("样本 %d 的 txID 两份实现不一致: 本包 %s / SDK %s", i, got, want)
		}
	}
}

func TestTxIDIsStableAcrossSignature(t *testing.T) {
	// 「签名后、广播前就能确定交易号」是整条防双付链的前提：
	// txID 只由 raw_data 决定，加上签名不应改变它。
	tx := sampleTransactions()[0]
	idA, err := txIDOf(tx)
	if err != nil {
		t.Fatal(err)
	}
	signed := &core.Transaction{RawData: tx.GetRawData(), Signature: [][]byte{{1, 2, 3}}}
	idB, err := txIDOf(signed)
	if err != nil {
		t.Fatal(err)
	}
	if idA != idB {
		t.Fatalf("签名改变了 txID：%s vs %s，台账与查证将指向不同的笔", idA, idB)
	}
}

func TestTxIDOfRejectsMissingRawData(t *testing.T) {
	if _, err := txIDOf(&core.Transaction{}); err == nil {
		t.Fatal("没有 raw_data 的交易必须报错，不能回一个空号")
	}
}

func sampleTransactions() []*core.Transaction {
	mk := func(num, exp, amt int64, withContract bool) *core.Transaction {
		data, err := trc20TransferCalldata(tronOfficialUSDT, amt)
		if err != nil {
			panic(err)
		}
		raw := &core.TransactionRaw{
			RefBlockNum: num, RefBlockBytes: []byte{byte(num), byte(num >> 8)},
			Expiration: exp, Timestamp: exp - 60_000, FeeLimit: defaultTRC20FeeLimitSun, Data: data,
		}
		if withContract {
			value, err := anypb.New(&core.TriggerSmartContract{
				OwnerAddress:    addrBytes(tronOfficialUSDT),
				ContractAddress: addrBytes(tronRangeAcct),
				Data:            data,
			})
			if err != nil {
				panic(err)
			}
			raw.Contract = []*core.Transaction_Contract{{
				Type:      core.Transaction_Contract_TriggerSmartContract,
				Parameter: value,
			}}
		}
		return &core.Transaction{RawData: raw}
	}
	return []*core.Transaction{mk(1, 1700000000000, 1, false), mk(999999, 1700000060000, 3_000_000_000, true), mk(0, 0, 0, false)}
}

func addrBytes(s string) []byte {
	raw, err := decodeTRONAddress(s)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestDecodeTRONAddressMatchesSDK(t *testing.T) {
	for _, s := range []string{tronOfficialUSDT, tronRangeAcct} {
		got, err := decodeTRONAddress(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		want, err := address.Base58ToAddress(s)
		if err != nil {
			t.Fatal(err)
		}
		// 本包解出完整 25 字节（含 4 字节校验和），SDK 的 Address 是 21 字节（不含校验和）：
		// 前 21 字节必须逐字相等，这才是「两侧对同一个地址的理解一致」。
		if !bytes.Equal(got[:21], want.Bytes()) {
			t.Errorf("%s 解出的地址字节与 SDK 不一致: %x / %x", s, got[:21], want.Bytes())
		}
		if len(got) != 25 {
			t.Errorf("%s 应解出 25 字节（含校验和），实得 %d", s, len(got))
		}
	}
}

func TestDecodeTRONAddressRejectsBadChecksum(t *testing.T) {
	// 改掉末位字符就是校验和错，必须被拒：这是「格式看着对、实际是别人地址」那类错里
	// 唯一能在本地拦下的一段。
	if _, err := decodeTRONAddress(tronBogusChecksum); err == nil {
		t.Fatal("校验和被改坏的地址应当被拒")
	}
	if _, err := decodeTRONAddress(""); err == nil {
		t.Fatal("空地址应当被拒")
	}
}

func TestTrc20AddressParameterPadding(t *testing.T) {
	got, err := trc20AddressParameter(tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != tvmWordLen*2 {
		t.Fatalf("balanceOf 参数应为 %d 个十六进制字符，实得 %d", tvmWordLen*2, len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("0", 24)) {
		t.Errorf("地址参数左侧没补零: %s", got)
	}
}

func TestBigToInt64Bounds(t *testing.T) {
	if _, err := bigToInt64(new(big.Int).Lsh(big.NewInt(1), 63)); err == nil {
		t.Fatal("超出 int64 的链上数值必须报错，截断会被读成「余额够用」")
	}
	if _, err := bigToInt64(big.NewInt(-1)); err == nil {
		t.Fatal("负余额必须报错")
	}
	if v, err := bigToInt64(big.NewInt(3_000_000_000)); err != nil || v != 3_000_000_000 {
		t.Fatalf("正常值被改动: %d %v", v, err)
	}
}

func TestReceiptVerdict(t *testing.T) {
	// 判据只认回执里的执行结果，不认 contractResult 字节：USDT-TRC20 的 transfer
	// 不返回 bool，真机上那笔成功转账的 contractResult 是 32 字节全零。
	// 拿字节判会把每一笔真成功的出款读成失败，上游据此重下一单就是业务层双付。
	zeroWord := make([]byte, tvmWordLen)
	cases := []struct {
		name        string
		txResult    core.TransactionInfoCode
		contractRet core.Transaction_ResultContractResult
		hasReceipt  bool
		wantOK      bool
		wantKnown   bool
	}{
		{"成功", core.TransactionInfo_SUCESS, core.Transaction_Result_SUCCESS, true, true, true},
		{"合约回滚", core.TransactionInfo_SUCESS, core.Transaction_Result_REVERT, true, false, true},
		{"能量耗尽", core.TransactionInfo_SUCESS, core.Transaction_Result_OUT_OF_ENERGY, true, false, true},
		{"交易级失败", core.TransactionInfo_FAILED, core.Transaction_Result_SUCCESS, true, false, true},
		{"没有回执", core.TransactionInfo_SUCESS, core.Transaction_Result_DEFAULT, false, false, false},
		{"回执未回填执行结果", core.TransactionInfo_SUCESS, core.Transaction_Result_DEFAULT, true, false, false},
	}
	for _, cs := range cases {
		ok, known := receiptVerdict(cs.txResult, cs.contractRet, cs.hasReceipt)
		if ok != cs.wantOK || known != cs.wantKnown {
			t.Errorf("%s: 判成 success=%v known=%v，期望 %v/%v", cs.name, ok, known, cs.wantOK, cs.wantKnown)
		}
	}
	// 真机形态回归：USDT 成功转账的 contractResult 是全零字，判定必须仍为成功。
	ok, known := receiptVerdict(core.TransactionInfo_SUCESS, core.Transaction_Result_SUCCESS, true)
	if !ok || !known {
		t.Fatalf("全零 contractResult 的成功转账被判成 success=%v known=%v（%x）", ok, known, zeroWord)
	}
}

// 广播响应码的分类是防双付的第一道闸：判错一类，要么把钱再动一次，要么让一单永远卡死。

func TestBroadcastOutcomeClassification(t *testing.T) {
	cases := []struct {
		name          string
		result        bool
		code          api.ReturnResponseCode
		wantAccepted  bool
		wantRejected  bool
		wantUncertain bool
	}{
		{"受理", true, api.Return_SUCCESS, true, false, false},
		{"签名错", false, api.Return_SIGERROR, false, true, false},
		{"TAPOS 过期块", false, api.Return_TAPOS_ERROR, false, true, false},
		{"交易已过期", false, api.Return_TRANSACTION_EXPIRATION_ERROR, false, true, false},
		{"交易过大", false, api.Return_TOO_BIG_TRANSACTION_ERROR, false, true, false},
		{"合约校验不过", false, api.Return_CONTRACT_VALIDATE_ERROR, false, true, false},
		{"带宽不足", false, api.Return_BANDWITH_ERROR, false, true, false},
		{"重复交易", false, api.Return_DUP_TRANSACTION_ERROR, false, false, true},
		{"服务繁忙", false, api.Return_SERVER_BUSY, false, false, true},
		{"节点无连接", false, api.Return_NO_CONNECTION, false, false, true},
		{"区块未固化", false, api.Return_BLOCK_UNSOLIDIFIED, false, false, true},
		{"合约执行异常", false, api.Return_CONTRACT_EXE_ERROR, false, false, true},
		{"其它错误", false, api.Return_OTHER_ERROR, false, false, true},
		{"自相矛盾（result 假而码为成功）", false, api.Return_SUCCESS, false, false, true},
	}
	for _, cs := range cases {
		got := broadcastOutcome(&api.Return{Result: cs.result, Code: cs.code})
		if got.Accepted != cs.wantAccepted || got.Rejected != cs.wantRejected || got.Uncertain != cs.wantUncertain {
			t.Errorf("%s: 分成 accepted=%v rejected=%v uncertain=%v，期望 %v/%v/%v",
				cs.name, got.Accepted, got.Rejected, got.Uncertain, cs.wantAccepted, cs.wantRejected, cs.wantUncertain)
		}
		if got.Code == "" {
			t.Errorf("%s: 响应码没留痕，对账时无从判断", cs.name)
		}
	}
	// 空响应（节点回了 nil）不能当成受理。
	if got := broadcastOutcome(nil); got.Accepted || !got.Uncertain {
		t.Errorf("空响应被读成 %+v", got)
	}
}

func TestIsDefinitelyUnsent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"参数非法（请求没被处理）", status.Error(codes.InvalidArgument, "bad"), true},
		{"前置条件不满足", status.Error(codes.FailedPrecondition, "no"), true},
		{"资源超限", status.Error(codes.ResourceExhausted, "rate"), true},
		{"超时（可能已入池）", status.Error(codes.DeadlineExceeded, "slow"), false},
		{"服务不可用（可能已入池）", status.Error(codes.Unavailable, "down"), false},
		{"连接被重置", errors.New("read: connection reset by peer"), false},
		{"裸错误一律按可能已发出", errors.New("boom"), false},
	}
	for _, cs := range cases {
		if got := isDefinitelyUnsent(cs.err); got != cs.want {
			t.Errorf("%s: 判成 %v，期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestCrossCheckTxID 节点回的交易号与本地按 sha256(raw_data) 算出的必须逐字一致：
// 广播结果不确定时，手里那个号必须与链上那个号是同一个；节点没回号时跳过
// （那是节点的缺失，不是分歧）。
func TestCrossCheckTxID(t *testing.T) {
	tx := buildableTx()
	want, err := txIDOf(tx)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := hex.DecodeString(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := crossCheckTxID(nodeID, tx); err != nil {
		t.Fatalf("两份实现算出的交易号一致，却报了对不上: %v", err)
	}
	bad := append([]byte{}, nodeID...)
	bad[0] ^= 0xFF
	if err := crossCheckTxID(bad, tx); err == nil {
		t.Fatal("交易号不一致必须就地拒绝")
	}
	if err := crossCheckTxID(nil, tx); err != nil {
		t.Fatalf("节点没回交易号时应当跳过而不是报错: %v", err)
	}
}

func TestNewGRPCGatewayRequiresNode(t *testing.T) {
	if _, err := newGRPCGateway("", fakeAPIKey, time.Second, true); err == nil {
		t.Fatal("没配节点时必须拒绝建网关")
	}
	// 超时留空要落到一个正的缺省值：SDK 把它直接交给 context.WithTimeout，
	// 传 0 就是「一出生就过期」，表现是每个链上调用都超时。
	gw, err := newGRPCGateway("127.0.0.1:50051", fakeAPIKey, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if gw.g == nil {
		t.Fatal("网关没持有客户端")
	}
	// 传输安全必须显式声明：grpc-go 对「没给凭据」的拨号直接拒绝建连。
	if _, err := newGRPCGateway("grpc.trongrid.io:50051", fakeAPIKey, time.Second, false); err != nil {
		t.Fatalf("缺省（TLS）应当能建连，实得 %v", err)
	}
}

func TestGRPCGatewayStopsFailingClosed(t *testing.T) {
	gw, err := newGRPCGateway("127.0.0.1:50051", "", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.close(); err != nil {
		t.Fatal(err)
	}
	// 关停之后每个方法都要明确失败，而不是 panic 或返回空成功。
	if _, err := gw.buildTransfer(context.Background(), tronRangeAcct, tronAllZeroAcct, tronOfficialUSDT, 1, 1); err == nil {
		t.Error("关停后仍能构造交易")
	}
	if _, err := gw.broadcast(context.Background(), &core.Transaction{}); err == nil {
		t.Error("关停后仍能广播")
	}
	if _, err := gw.lookupTx(context.Background(), "x"); err == nil {
		t.Error("关停后仍能查证")
	}
	if _, err := gw.tokenBalance(context.Background(), tronRangeAcct, tronOfficialUSDT); err == nil {
		t.Error("关停后仍能读余额")
	}
	if _, err := gw.account(context.Background(), tronRangeAcct); err == nil {
		t.Error("关停后仍能读账户")
	}
	if _, err := gw.headBlock(context.Background()); err == nil {
		t.Error("关停后仍能读链头")
	}
}

func TestGRPCGatewayHasNoTransferListing(t *testing.T) {
	// 全节点 gRPC 没有「按地址列转账历史」这一接口，反查只能靠接入点读侧。
	// 这条能力边界必须显式报错，不能让调用方以为「查过了，没有」。
	gw, err := newGRPCGateway("127.0.0.1:50051", "", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.outboundTransfers(context.Background(), tronRangeAcct, tronOfficialUSDT, 1); !errors.Is(err, errNoTransferListing) {
		t.Fatalf("应当回能力边界错误，实得 %v", err)
	}
}

// 读侧兜底的可信度取决于它把节点响应读成了什么：这里逐字段验，全离线打假 TronGrid。

func newTestHTTP(t *testing.T, handler http.HandlerFunc) (*httpGateway, *int) {
	t.Helper()
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if got := r.Header.Get(tronAPIKeyHeader); got != fakeAPIKey {
			t.Errorf("%s 未带 %s 头，实得 %q", r.URL.Path, tronAPIKeyHeader, got)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	gw, err := newHTTPGateway(srv.URL, fakeAPIKey, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	return gw, hits
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestHTTPLookupTxReadsReceipt(t *testing.T) {
	gw, hits := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/walletsolidity/gettransactioninfobyid" {
			t.Errorf("路径应为钱包读接口，实得 %s", r.URL.Path)
		}
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["value"] != "abc123" {
			t.Errorf("查询条件没带交易号: %+v", body)
		}
		// 字段位置按真机响应：合约执行结果在 receipt.result，总扣费在顶层 fee。
		writeJSON(w, map[string]any{
			"id": "abc123", "blockNumber": 42, "fee": 13_000_100,
			"receipt": map[string]any{"energy_fee": 13_000_000, "net_fee": 100, "result": "SUCCESS"},
		})
	})
	rec, err := gw.lookupTx(context.Background(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Found || !rec.InBlock || !rec.Success || !rec.VerdictKnown || rec.BlockNumber != 42 || rec.FeeSun != 13_000_100 {
		t.Fatalf("回执读错: %+v", rec)
	}
	if *hits != 1 {
		t.Fatalf("应当只发一次请求，实得 %d", *hits)
	}
}

func TestHTTPLookupTxVerdictFromReceipt(t *testing.T) {
	// 真机形态：成功的 USDT 转账顶层 result 直接省略，只有 receipt.result=SUCCESS
	// （2026-10-16 主网 txid ae07c52c…bba9）。两层都没表态时必须是「判定不明」，
	// 因为判成 FAILED 会让上游重下一单，而链上那一笔可能已经把币转出去了。
	cases := map[string]struct {
		txResult, receiptResult string
		wantOK, wantKnown       bool
	}{
		"真机成功形态":  {"", "SUCCESS", true, true},
		"两层都成功":   {"SUCCESS", "SUCCESS", true, true},
		"合约回滚":    {"", "REVERTED", false, true},
		"能量耗尽":    {"", "OUT_OF_ENERGY", false, true},
		"交易级失败":   {"FAILED", "SUCCESS", false, true},
		"只有交易级成功": {"SUCCESS", "", true, true},
		"两层都没表态":  {"", "", false, false},
	}
	for name, cs := range cases {
		gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"id": "x", "result": cs.txResult, "blockNumber": 1,
				"receipt": map[string]any{"result": cs.receiptResult}})
		})
		rec, err := gw.lookupTx(context.Background(), "x")
		if err != nil {
			t.Fatal(err)
		}
		if rec.Success != cs.wantOK || rec.VerdictKnown != cs.wantKnown {
			t.Errorf("%s: 判成 success=%v known=%v，期望 %v/%v（%+v）",
				name, rec.Success, rec.VerdictKnown, cs.wantOK, cs.wantKnown, rec)
		}
	}
}

// TestHTTPLookupTxFeeFallsBackToReceipt 顶层 fee 缺失时用 energy_fee+net_fee 兜底。
func TestHTTPLookupTxFeeFallsBackToReceipt(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "x", "blockNumber": 3,
			"receipt": map[string]any{"result": "SUCCESS", "energy_fee": 4_957_100, "net_fee": 345_000}})
	})
	rec, err := gw.lookupTx(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.FeeSun != 5_302_100 {
		t.Fatalf("手续费兜底算错: %d（真机同形态那笔是 5302100 sun）", rec.FeeSun)
	}
}

func TestHTTPLookupTxEmptyMeansNotFound(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	rec, err := gw.lookupTx(context.Background(), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Found || rec.Success {
		t.Fatalf("空回执必须读成「没上链」而不是「成功」: %+v", rec)
	}
}

func TestHTTPRetriesOnServerError(t *testing.T) {
	var n int
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"id": "x", "result": "SUCCESS", "blockNumber": 7})
	})
	rec, err := gw.lookupTx(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.InBlock || n != 2 {
		t.Fatalf("5xx 应当由共用 HTTP 层原样重发一次: hits=%d rec=%+v", n, rec)
	}
}

func TestHTTPRefusesWrites(t *testing.T) {
	// 兜底路径只读不写：会动钱的第二条路径必须不存在，而不是「配了才生效」。
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("读侧兜底不该发出任何请求，路径 %s", r.URL.Path)
	})
	if _, err := gw.buildTransfer(context.Background(), "T1", "T2", "T3", 1, 1); !errors.Is(err, errWriteOverHTTP) {
		t.Errorf("构造交易必须被拒，实得 %v", err)
	}
	if _, err := gw.broadcast(context.Background(), nil); !errors.Is(err, errWriteOverHTTP) {
		t.Errorf("广播必须被拒，实得 %v", err)
	}
}

func TestHTTPOutboundTransfersQueryAndParse(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/accounts/") || !strings.HasSuffix(r.URL.Path, "/transactions/trc20") {
			t.Errorf("反查路径不对: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("only_from") != "true" || q.Get("contract_address") != tronOfficialUSDT || q.Get("min_timestamp") != "1700000000000" {
			t.Errorf("反查条件不对: %v", q)
		}
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"transaction_id": "tx1", "to": tronAllZeroAcct, "value": "3000000000", "block_timestamp": 1, "type": "Transfer"},
			{"transaction_id": "tx2", "to": tronAllZeroAcct, "value": "1", "block_timestamp": 2, "type": "Approval"},
			{"transaction_id": "", "to": tronAllZeroAcct, "value": "9", "block_timestamp": 3, "type": "Transfer"},
		}})
	})
	out, err := gw.outboundTransfers(context.Background(), tronRangeAcct, tronOfficialUSDT, 1700000000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TxID != "tx1" || out[0].Amount != 3_000_000_000 {
		t.Fatalf("反查结果不对（Approval 与非 Transfer 必须被滤掉）: %+v", out)
	}
}

func TestHTTPOutboundRejectsFractionalAmount(t *testing.T) {
	// 链上 value 是最小单位整数字符串。把它当小数读，比对就永远不相等，
	// 于是台账丢失后本该拦下的那一笔会被放行——这是双付路径上最便宜的一道闸。
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"transaction_id": "tx1", "to": tronAllZeroAcct, "value": "3000.0", "type": "Transfer"},
		}})
	})
	if _, err := gw.outboundTransfers(context.Background(), tronRangeAcct, tronOfficialUSDT, 1); err == nil {
		t.Fatal("非整数金额必须报错，不能静默跳过")
	}
}

func TestHTTPTokenBalance(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wallet/triggersmartcontract" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["function_selector"] != "balanceOf(address)" || body["constant"] != true {
			t.Errorf("常量调用参数不对: %+v", body)
		}
		if len(body["parameter"].(string)) != tvmWordLen*2 {
			t.Errorf("balanceOf 参数长度不对: %v", body["parameter"])
		}
		// owner_address 缺了真机就回 OTHER_ERROR，而那条码看不出是请求体缺字段。
		if body["owner_address"] == "" {
			t.Error("常量调用缺 owner_address")
		}
		writeJSON(w, map[string]any{"result": map[string]any{"result": true},
			"constant_result": []string{padHex(3_000_000_000)}})
	})
	v, err := gw.tokenBalance(context.Background(), tronRangeAcct, tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	if v != 3_000_000_000 {
		t.Fatalf("余额读成 %d", v)
	}
}

func TestHTTPTokenBalanceNodeRejection(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"result": map[string]any{"code": "CONTRACT_VALIDATE_ERROR", "message": "contract not exists"}})
	})
	if _, err := gw.tokenBalance(context.Background(), tronRangeAcct, tronOfficialUSDT); err == nil {
		t.Fatal("节点拒了常量调用必须报错，不能读成 0 余额")
	}
}

// padHex 造一个 32 字节左对齐的十六进制载荷，模拟合约的常量返回值。
func padHex(v int64) string {
	h := hexOf(v)
	return strings.Repeat("0", tvmWordLen*2-len(h)) + h
}

func hexOf(v int64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	out := make([]byte, 0, 16)
	for v > 0 {
		out = append([]byte{digits[v%16]}, out...)
		v /= 16
	}
	return string(out)
}

func TestHTTPHeadBlockAndAccount(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wallet/getnowblock":
			writeJSON(w, map[string]any{"block_header": map[string]any{"raw_data": map[string]any{"number": 12345678}}})
		case "/wallet/getaccount":
			writeJSON(w, map[string]any{"address": tronRangeAcct, "balance": 500_000_000, "create_time": 1})
		default:
			t.Errorf(" unexpected path %s", r.URL.Path)
		}
	})
	head, err := gw.headBlock(context.Background())
	if err != nil || head != 12345678 {
		t.Fatalf("链头读错: %d %v", head, err)
	}
	acct, err := gw.account(context.Background(), tronRangeAcct)
	if err != nil || acct.BalanceSun != 500_000_000 {
		t.Fatalf("账户读错: %+v %v", acct, err)
	}
}

func TestHTTPAccountNotActivatedIsNotAnError(t *testing.T) {
	// 未激活账户是链上事实：回空对象要读成「不存在」，不能报故障，也不能报余额 0 之后照样发。
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	acct, err := gw.account(context.Background(), tronAllZeroAcct)
	if err != nil {
		t.Fatal(err)
	}
	if acct.BalanceSun != 0 {
		t.Fatalf("未激活账户读成了 %+v", acct)
	}
}

func TestHTTPNewGatewayRequiresBase(t *testing.T) {
	if _, err := newHTTPGateway("", fakeAPIKey, time.Second, 0); err == nil {
		t.Fatal("没配 http_base 时必须拒绝构造")
	}
}

func TestAPIKeyNeverLeaksIntoErrors(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	_, err := gw.lookupTx(context.Background(), "x")
	if err != nil && strings.Contains(err.Error(), fakeAPIKey) {
		t.Fatalf("错误文案含 API Key: %v", err)
	}
}

// ---- 分层网关：写只走正门，读只在「接入点没答上来」时换路 ----

type stubRead struct {
	recv    *txReceipt
	rerr    error
	calls   int
	fellOff bool
}

func (s *stubRead) buildTransfer(context.Context, string, string, string, int64, int64) (*core.Transaction, error) {
	s.fellOff = true
	return nil, errWriteOverHTTP
}

func (s *stubRead) broadcast(context.Context, *core.Transaction) (*broadcastResult, error) {
	s.fellOff = true
	return nil, errWriteOverHTTP
}

func (s *stubRead) lookupTx(context.Context, string) (*txReceipt, error) {
	s.calls++
	return s.recv, s.rerr
}

func (s *stubRead) tokenBalance(context.Context, string, string) (int64, error) {
	s.calls++
	return 0, s.rerr
}

func (s *stubRead) account(context.Context, string) (*accountState, error) {
	return &accountState{}, s.rerr
}
func (s *stubRead) headBlock(context.Context) (int64, error) { return 0, s.rerr }
func (s *stubRead) outboundTransfers(context.Context, string, string, int64) ([]outboundTransfer, error) {
	return nil, s.rerr
}
func (s *stubRead) close() error { return nil }

func TestTieredFallsBackOnlyOnTransportErrors(t *testing.T) {
	cases := map[string]struct {
		primaryErr error
		wantFall   bool
	}{
		"超时换读路":     {errors.New("context deadline exceeded"), true},
		"连接不可用换读路":  {errors.New("rpc error: code = Unavailable desc: connection refused"), true},
		"节点没答上来换读路": {errors.New("client not connected"), true},
		"回执解析不对不换":  {errors.New("链上回执不可解析: bad json"), false},
		"业务拒绝不换":    {errors.New("节点拒绝常量调用 code=XYZ"), false},
	}
	for name, cs := range cases {
		primary := &stubRead{rerr: cs.primaryErr}
		reader := &stubRead{recv: &txReceipt{Found: true, InBlock: true, Success: true}}
		tiered := &tieredGateway{primary: primary, reader: reader}
		_, err := tiered.lookupTx(context.Background(), "x")
		if cs.wantFall && (err != nil || !reader.recv.Found) {
			t.Errorf("%s: 应当回落到读侧，实得 err=%v", name, err)
		}
		if !cs.wantFall && reader.calls != 0 {
			t.Errorf("%s: 不该回落，却调了读侧 %d 次", name, reader.calls)
		}
		if primary.calls != 1 {
			t.Errorf("%s: 正门应当只碰一次，实得 %d", name, primary.calls)
		}
	}
}

func TestTieredNeverRoutesWritesToReader(t *testing.T) {
	reader := &stubRead{}
	tiered := &tieredGateway{primary: &stubRead{}, reader: reader}
	if _, err := tiered.broadcast(context.Background(), nil); err == nil {
		t.Fatal("正门的广播错误必须原样抛出")
	}
	if reader.fellOff {
		t.Fatal("写操作落到了读侧兜底上")
	}
}

// 本文件钉住「按单出款参数」与「链上余额查询」两条新口径。
//
// whatapp 的多线路热钱包不在配置里：每一单的 From 携带 地址+私钥+合约+网络。
// 这里要证明三件事：解析出的资产确实是 From 那一份；同一单号不得换钱包/换币；
// 余额查询读的是查询指定的地址/合约，而不是配置默认值。

const perOrderPrivKey = "2222222222222222222222222222222222222222222222222222222222222222"

func perOrderWallet(t *testing.T) string {
	t.Helper()
	key, err := sdkkeys.GetPrivateKeyFromHex(perOrderPrivKey)
	if err != nil {
		t.Fatalf("测试私钥不可解析: %v", err)
	}
	return address.BTCECPrivkeyToAddress(key).String()
}

func perOrderChannel(gw chainGateway) *onchainChannel {
	if f, ok := gw.(*fakeGateway); ok && f.tokenBal == 0 && f.tokenBalErr == nil {
		f.tokenBal = 1 << 50
	}
	return &onchainChannel{
		gw:     gw,
		sign:   (&counters{}).signer(),
		opt:    channelOptions{Name: "usdt", Driver: DriverTronGrid},
		chain:  &chainConfig{Node: "grpc.example:50051", WalletAddress: tronRangeAcct, Contract: tronOfficialUSDT, Network: pay.NetworkMainnet, APIKey: fakeAPIKey, PrivateKey: fakePrivKey},
		ledger: ledger.New("tron"),
		wallet: tronRangeAcct,
		nowFn:  func() time.Time { return testClock },
	}
}

func TestAssetOfPrefersPerOrderParams(t *testing.T) {
	ch := perOrderChannel(&fakeGateway{})
	// 按单网络是这一组的主变量；配置网络留空，避免交叉校验挡住覆盖行为。
	ch.chain.Network = ""
	wallet := perOrderWallet(t)

	asset, err := ch.assetOf(&pay.PayOrder{
		OrderNo: "O1", Amount: 1,
		To: &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{
			Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT,
			Network: "shasta", PrivateKey: pay.Secret(perOrderPrivKey),
		},
	})
	if err != nil {
		t.Fatalf("按单参数解析失败: %v", err)
	}
	if asset.wallet != wallet || asset.contract != tronOfficialUSDT || asset.network != "shasta" || asset.sign == nil {
		t.Fatalf("按单参数没有覆盖配置缺省值: %+v", asset)
	}
	if asset.secret != perOrderPrivKey {
		t.Fatal("用于脱敏的按单私钥没有带进 asset")
	}

	// 私钥与 From.Address 不匹配：必须在签名前拒。
	_, err = ch.assetOf(&pay.PayOrder{
		OrderNo: "O2", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: tronRangeAcct, PrivateKey: pay.Secret(perOrderPrivKey)},
	})
	if err == nil || !strings.Contains(err.Error(), "不是同一个账户") {
		t.Fatalf("私钥与地址不匹配应在签名前拒，实得 %v", err)
	}

	// 只给地址、不给私钥：只有配置热钱包能签。
	asset, err = ch.assetOf(&pay.PayOrder{
		OrderNo: "O3", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: tronRangeAcct},
	})
	if err != nil || asset.wallet != tronRangeAcct || asset.sign == nil {
		t.Fatalf("From 只给配置热钱包地址时应沿用配置私钥: asset=%+v err=%v", asset, err)
	}
	_, err = ch.assetOf(&pay.PayOrder{
		OrderNo: "O4", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: wallet},
	})
	if err == nil || !strings.Contains(err.Error(), "无法签名") {
		t.Fatalf("From 指定非配置钱包却无按单私钥时应拒，实得 %v", err)
	}

	// 旧库把 trc20 填在 chain_network 里：按空处理，回落配置网络。
	mainnetCh := perOrderChannel(&fakeGateway{})
	asset, err = mainnetCh.assetOf(&pay.PayOrder{
		OrderNo: "O5", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT, Network: "TRC20", PrivateKey: pay.Secret(perOrderPrivKey)},
	})
	if err != nil || asset.network != pay.NetworkMainnet {
		t.Fatalf("代币标准名不应被当成网络: asset=%+v err=%v", asset, err)
	}

	// 配置节点属于主网、订单写着 shasta：必须在签名前拒，不能把测试网合约发进主网节点。
	_, err = mainnetCh.assetOf(&pay.PayOrder{
		OrderNo: "O6", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT, Network: "shasta", PrivateKey: pay.Secret(perOrderPrivKey)},
	})
	if err == nil || !strings.Contains(err.Error(), "换网就是换币") {
		t.Fatalf("配置网络与按单网络不一致应拒，实得 %v", err)
	}
}

func TestWithdrawPinsFromAndContract(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{Found: true, InBlock: true, Success: true, BlockNumber: 100}, head: 106}
	ch := perOrderChannel(gw)
	wallet := perOrderWallet(t)
	base := &pay.PayOrder{
		OrderNo: "WD-PIN-1", Amount: 1, Currency: pay.CurrencyUSDT,
		To: &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{
			Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT,
			PrivateKey: pay.Secret(perOrderPrivKey),
		},
	}
	if _, err := ch.Withdraw(context.Background(), base); err != nil {
		t.Fatalf("按单私钥出款失败: %v", err)
	}
	entry, found := ch.ledger.Get(base.OrderNo)
	if !found || entry.From != wallet || entry.Contract != tronOfficialUSDT {
		t.Fatalf("台账没有钉住按单资产: %+v found=%v", entry, found)
	}

	// 同一单号换钱包：即使第一个参数都合法，也必须在签第二笔之前拒。
	other := *base
	other.From = &pay.Party{
		Kind: pay.PartyWallet, Address: usdtTestWallet, Contract: tronOfficialUSDT,
		PrivateKey: pay.Secret(fakePrivKey),
	}
	if _, err := ch.Withdraw(context.Background(), &other); err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同一单号换钱包必须拒，实得 %v", err)
	}

	// 同一单号换合约：拒。
	other = *base
	other.From = &pay.Party{
		Kind: pay.PartyWallet, Address: wallet, Contract: tronAllZeroAcct,
		PrivateKey: pay.Secret(perOrderPrivKey),
	}
	if _, err := ch.Withdraw(context.Background(), &other); err == nil || !strings.Contains(err.Error(), "不得换币") {
		t.Fatalf("同一单号换合约必须拒，实得 %v", err)
	}
}

func TestQueryOnchainAccountReadsRequestedAsset(t *testing.T) {
	gw := &fakeGateway{tokenBal: 123, acct: &accountState{BalanceSun: 456}}
	ch := perOrderChannel(gw)
	info, err := ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{
		Address: tronAllZeroAcct, Contract: tronOfficialUSDT, Currency: pay.CurrencyUSDT,
	})
	if err != nil {
		t.Fatalf("链上余额查询失败: %v", err)
	}
	if info.Available != 123 || info.Balance != 123 || info.Status != pay.AcctActive {
		t.Fatalf("链上余额不符: %+v", info)
	}
	if !strings.Contains(info.RawNote, "456") {
		t.Fatalf("gas 余额没有进摘要: %q", info.RawNote)
	}

	if _, err := ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Address: "not-a-tron-address"}); err == nil {
		t.Fatal("非法地址应被拒")
	}
	if _, err := ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Currency: pay.CurrencyTON}); err == nil {
		t.Fatal("非 USDT 币种应被拒")
	}

	gw.tokenBalErr = context.DeadlineExceeded
	_, err = ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Address: tronAllZeroAcct, Contract: tronOfficialUSDT})
	pe, ok := pay.ProviderErrorOf(err)
	if !ok || pe.Code != "balance_unread" || !pe.Retryable {
		t.Fatalf("余额读失败应为可重试 ProviderError，实得 %v", err)
	}
}

// TRON 地址夹具：官方 USDT-TRC20 合约（版本字节 0x41、校验和已核），
// 以及用 20 字节账户哈希现算校验和造出的几个。造向量而不是抄网上的：
// 抄来的地址一旦本身是编的，测试就会把「校验和不过」永远藏住。
const (
	tronOfficialUSDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	tronAllZeroAcct  = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
	tronRangeAcct    = "T9yED5xMV5ARV98BexN97aLZ1UUq7eKSxm"
	tronFFAcct       = "TZJozAg1ruapycCicgz31GxvYJ1FraLjZa"
	// tronBogusChecksum 长度与版本字节全对，只有末尾 4 字节校验和不过。
	tronBogusChecksum = "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR"
)

// TestIsValidTRONAddress 地址校验的正负用例：合法向量全过，
// 长度、版本字节、字母表、校验和任一不合都当场拒。
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
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6O", false, "含非法字符 O"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6l", false, "含非法字符 l"},
		{"0x76d5820a24a2089091721d40a4d20680804b6229", false, "EVM 地址不是 TRON 地址"},
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

// TestTRXRuleLivesInThisPackage 地址规则归属于链包，且不再经 pay 的注册表：
// checkOrderAddress 的调用点就是接线，漏接是编译错误而不是「未登记即放行」。
func TestTRXRuleLivesInThisPackage(t *testing.T) {
	rule := TRONRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tronRangeAcct}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tronBogusChecksum}}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestTRXRuleRejectsMemo TRON 原生转账没有 memo 概念：填了就拒而不是忽略，
// 上游把 TON 的认款机制套到这条链上是错配，静默吞掉等于放它去别处发作。
func TestTRXRuleRejectsMemo(t *testing.T) {
	rule := TRONRule{}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	// 纯空白按「没有备注」处理：真正不发出去的那一步在 NewOrderRequest 的 trim。
	if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: "   "}}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestPayoutInterfaceStaysWired TRX 侧门面仍是包外可见调用面，且一律返回未接入哨兵。
//
// 与 usdt 侧的存续闸同型：签名不许被顺手改掉（下游按它编译），
// 而「没接入」绝不允许被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = TronWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = TronQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = TronAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, To: &pay.Party{Address: tronAllZeroAcct}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := TronWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := TronQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := TronAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 单号为空也一样先拒：不能把参数错误伪装成一次「查询未果」的 UNKNOWN。
	if _, err := TronQuery(nil, ""); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空单号的拒绝口径不符: %v", err)
	}
}

// TestPayoutNotWiredDoesNotBroadcast 未接入的门面不得广播任何回执。
//
// 广播键（CallTronMsg+Xxx）在下游挂着落库回调，误广播一次就给账本记一笔不存在的动作。
func TestPayoutNotWiredDoesNotBroadcast(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []*pay.PayResult
		kind []string
	)
	for _, k := range []string{"Withdraw", "Query"} {
		id := util.DefaultCallFunc.Register(util.CallTronMsg+k, func(res *pay.PayResult) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, res)
			kind = append(kind, k)
		})
		defer util.DefaultCallFunc.Unregister(util.CallTronMsg+k, id)
	}
	o := &pay.PayOrder{OrderNo: "O9", Amount: 1, To: &pay.Party{Address: tronAllZeroAcct}}
	_, _ = TronWithdraw(nil, o)
	_, _ = TronQuery(nil, "O9")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("未接入的链路广播了回执: kinds=%v %+v", kind, got)
	}
}

// 本文件钉住内联装配（tron.usdt 一段写全）的两道闸：密钥注入钩子的键名迁移，
// 以及驱动名必须自己把关。这两条在旧装配层（pay.OpenChain 按注册表取驱动）里
// 是白拿的，内联之后没人替本包挡，所以必须各有一条用例。

// mustOnchain 走一遍真实装配入口并取回链上通道内部结构。
func mustOnchain(t *testing.T, cfg *config.Cfg) *onchainChannel {
	t.Helper()
	c, ok := newUSDTClient(cfg)
	if !ok || c == nil {
		t.Fatal("装配失败")
	}
	ch, ok := c.Ch.(*onchainChannel)
	if !ok {
		t.Fatalf("装配出的通道类型不是 *onchainChannel: %T", c.Ch)
	}
	return ch
}

// TestUsdtInlineKeyHooks 注入钩子的键名：只认段名 tron.usdt，旧段名 supplier_a 不再被读取。
//
// 双读期（P2→P5）已经结束：两个键都能把密钥塞进来时，「密钥从哪来」是猜的。
// 这条闸把两边都腕死：新键必须生效，旧键必须不再生效——只断言前者的话，
// 回落分支就算被删错（比如把新键写成旧键）也看不出来。
func TestUsdtInlineKeyHooks(t *testing.T) {
	// 新键生效：密钥与私钥各走各的前缀。
	// 钩子注册与注销成对贴着这一小段（不用 defer）：同一个键名留在全局钩子表里，
	// 下一段用例就会把它的值当成自己那段注入进来的结果。
	cfg := usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
	cfg.Tron.Usdt.SecretKey = ""
	cfg.Tron.Usdt.Chain.PrivateKey = ""
	idNew := util.DefaultCallFunc.Register(util.CallPaySDKMsg+inlineUSDTKey, func(sk *string) { *sk = usdtTestSecret })
	idKey := util.DefaultCallFunc.Register(util.CallPayChainMsg+inlineUSDTKey, func(pk *string) { *pk = usdtTestPrivKey })
	ch := mustOnchain(t, cfg)
	util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+inlineUSDTKey, idNew)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+inlineUSDTKey, idKey)
	if ch.opt.SecretKey != usdtTestSecret {
		t.Fatalf("新键未生效: SecretKey=%q", ch.opt.SecretKey)
	}
	if ch.chain.PrivateKey != usdtTestPrivKey {
		t.Fatalf("私钥钩子未按新键注入: PrivateKey=%q", ch.chain.PrivateKey)
	}

	// 旧键不再被读取：只注册旧键时，密钥不得落进段里，也不得进到通道快照。
	// 注意不能拿「通道不启动」当判据：供应商密钥为空只让账户查询不可用，
	// 链上出款照样能装配——那种失败形态跟密钥从哪来无关。
	cfg = usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
	cfg.Tron.Usdt.SecretKey = ""
	idOld := util.DefaultCallFunc.Register(util.CallPaySDKMsg+"supplier_a", func(sk *string) { *sk = "OLD_SECRET" })
	c, _ := newUSDTClient(cfg)
	util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+"supplier_a", idOld)
	if got := strings.TrimSpace(cfg.Tron.Usdt.SecretKey); got != "" {
		t.Fatalf("旧键 supplier_a 仍能注入密钥：段里的 secret_key=%q", got)
	}
	if c != nil {
		if ch, ok := c.Ch.(*onchainChannel); ok && ch.opt.SecretKey != "" {
			t.Fatalf("旧键 supplier_a 仍能注入密钥：通道快照 SecretKey=%q", ch.opt.SecretKey)
		}
	}
}

// TestUsdtInlineRejectsWrongDriver 内联装配不经驱动注册表，驱动名必须自己把关。
//
// 写错驱动名在这里放行，等于让「配置写着换实现、实际还是这一套」静默通过。
// 只测真的不同的名字：前后空白由 TrimSpace 归一（那是同一份意图，不是另一个驱动）。
func TestUsdtInlineRejectsWrongDriver(t *testing.T) {
	for _, driver := range []string{"", "generic_md5", "tron_trongrid_v2", "tron"} {
		cfg := usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
		cfg.Tron.Usdt.Driver = driver
		if c, ok := newUSDTClient(cfg); ok || c != nil {
			t.Errorf("driver=%q 不应让内联通道启动", driver)
		}
	}
	// 前后空白是同一份意图：TrimSpace 之后等价，不该被拒。
	cfg := usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
	cfg.Tron.Usdt.Driver = " " + DriverTronGrid + " "
	if _, ok := newUSDTClient(cfg); !ok {
		t.Error("driver 前后空白应当被归一而不是当成另一个驱动")
	}
}

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
