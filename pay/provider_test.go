package pay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestSignMD5KnownVector 钉住签名算法形态：值直连拼接 + 末尾密钥，取 32 位小写 MD5。
// 样例取自 t1job 接口文档 §2.3（uname=suitxx, passwd=111111, SECRET_KEY 为字面量）。
func TestSignMD5KnownVector(t *testing.T) {
	got := SignMD5("SECRET_KEY", "suitxx", "111111")
	if want := "121115f0cfd4654599b0ed3fee771075"; got != want {
		t.Fatalf("签名=%s，期望 %s", got, want)
	}
	if len(got) != 32 || strings.ToLower(got) != got {
		t.Fatalf("签名形态不是 32 位小写十六进制: %s", got)
	}
	// 缺省字段按空串参与拼接，不得跳过——跳过等于签名域变了。
	if a, b := SignMD5("K", "x", "", "y"), SignMD5("K", "xy"); a != b {
		t.Fatalf("空串与缺省的拼接结果不一致: %s vs %s", a, b)
	}
	// 顺序即签名域：换序必须换出不同签名。
	if SignMD5("K", "a", "b") == SignMD5("K", "b", "a") {
		t.Fatal("字段换序后签名未变，说明拼接漏了字段")
	}
}

// TestEnvelopeSucceeded 有 code 按 code 判；只有 success 布尔形态才退回布尔。
func TestEnvelopeSucceeded(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		env  envelope
		want bool
	}{
		{"code=0", envelope{Code: "0"}, true},
		{"code失败", envelope{Code: "250008"}, false},
		{"无code+success=true", envelope{Success: &yes}, true},
		{"无code+success=false", envelope{Success: &no}, false},
		{"既无code也无success", envelope{Msg: "x"}, false},
		{"code优先于success", envelope{Code: "1", Success: &yes}, false},
	}
	for _, cs := range cases {
		if got := cs.env.succeeded(); got != cs.want {
			t.Errorf("%s: succeeded=%v，期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestRetryableCodesEmptyByDefault 业务码默认可重试性必须是「不可重试」。
// 哪天有人往表里塞了个出款相关码，这条测试会逼他写下理由。
func TestRetryableCodesEmptyByDefault(t *testing.T) {
	if len(RetryableCodes) != 0 {
		t.Fatalf("RetryableCodes 已登记 %d 项，确认每个码都可原单重发后再提交", len(RetryableCodes))
	}
}

// TestNormalizeStatus 未登记的状态词一律 UNKNOWN，不许滑到 PENDING 或 FAILED。
func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"success":     StatusSuccess,
		" SUCCESS ":   StatusSuccess,
		"1":           StatusSuccess,
		"failed":      StatusFailed,
		"-1":          StatusFailed,
		"pending":     StatusPending,
		"0":           StatusPending,
		"":            StatusUnknown,
		"weird_state": StatusUnknown,
		"供应商自造词":      StatusUnknown,
	}
	for raw, want := range cases {
		if got := NormalizeStatus(raw); got != want {
			t.Errorf("NormalizeStatus(%q)=%s，期望 %s", raw, got, want)
		}
	}
}

// TestResultNeverReportsSuccessOnGarbage 回执读不出来时只能给 UNKNOWN：
// 请求已经发出去了，判 FAILED 会诱发重发，判 SUCCESS 会凭空记账。
func TestResultNeverReportsSuccessOnGarbage(t *testing.T) {
	p, err := NewProvider(ProviderOptions{Name: "t", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: func(string) (string, bool) { return "/x", true }})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []json.RawMessage{nil, []byte(`{}`), []byte(`not json`), []byte(`{"status":"???"}`)} {
		res := p.Result(raw, "ORD-1")
		if res.Status != StatusUnknown {
			t.Fatalf("载荷 %s 归一成 %s，期望 unknown", raw, res.Status)
		}
		if res.OrderNo != "ORD-1" {
			t.Fatalf("订单号未回填: %+v", res)
		}
		if res.IsFinal() {
			t.Fatalf("UNKNOWN 不得是终态: %+v", res)
		}
		if strings.Contains(strings.ToLower(res.RawNote), "secret") {
			t.Fatalf("摘要含凭证字样: %s", res.RawNote)
		}
	}
	good := p.Result(json.RawMessage(`{"order_no":"ORD-2","trade_no":"T9","status":"success","amount":1000}`), "ORD-1")
	if good.Status != StatusSuccess || good.TradeNo != "T9" || good.OrderNo != "ORD-2" || good.Amount != 1000 {
		t.Fatalf("正常回执归一不符: %+v", good)
	}
}

// TestCheckOrderGuards 幂等键、正整数金额、提现地址三项前置校验。
func TestCheckOrderGuards(t *testing.T) {
	ok := &PayOrder{OrderNo: "O1", Amount: 1}
	if err := CheckOrder(ok, false); err != nil {
		t.Fatalf("合法订单被拒: %v", err)
	}
	cases := []struct {
		name    string
		order   *PayOrder
		address bool
	}{
		{"nil 订单", nil, false},
		{"缺订单号", &PayOrder{Amount: 1}, false},
		{"订单号空白", &PayOrder{OrderNo: "  ", Amount: 1}, false},
		{"零金额", &PayOrder{OrderNo: "O1"}, false},
		{"负金额", &PayOrder{OrderNo: "O1", Amount: -5}, false},
		{"提现缺收款地址", &PayOrder{OrderNo: "O1", Amount: 1}, true},
	}
	for _, cs := range cases {
		if err := CheckOrder(cs.order, cs.address); err == nil {
			t.Errorf("%s 未被拒", cs.name)
		}
	}
}

// TestNewProviderFailsClosed 缺密钥或缺端点取值函数时不得构造成功。
// 带着空密钥启动的通道会在供应商侧刷一堆验签失败，本地却看起来一切正常。
func TestNewProviderFailsClosed(t *testing.T) {
	ep := func(string) (string, bool) { return "/x", true }
	if _, err := NewProvider(ProviderOptions{Name: "t", BaseURL: "https://x", Endpoint: ep}); err == nil {
		t.Fatal("空 secret_key 应当拒绝构造")
	}
	if _, err := NewProvider(ProviderOptions{Name: "t", BaseURL: "https://x", SecretKey: "s"}); err == nil {
		t.Fatal("nil Endpoint 取值函数应当拒绝构造")
	}
	if _, err := NewProvider(ProviderOptions{Name: "t", SecretKey: "s", Endpoint: ep}); err == nil {
		t.Fatal("空 base_url 应当拒绝构造")
	}
}

// TestProviderErrorTextExcludesCredential 错误链上任何一环都不得带出密钥与会话凭证。
func TestProviderErrorTextExcludesCredential(t *testing.T) {
	p, err := NewProvider(ProviderOptions{Name: "wa", BaseURL: "https://x.example", SecretKey: "TOPSECRET",
		AppID: "app1", Endpoint: func(string) (string, bool) { return "/none", false }})
	if err != nil {
		t.Fatal(err)
	}
	p.SetToken("TK-abcdef")
	// 端点未登记：应当在发出任何请求之前就拒掉。
	if _, err := p.Call(nil, EndpointWithdraw, &OrderRequest{OrderNo: "O1"}, nil); err == nil {
		t.Fatal("未登记端点应当报错")
	} else if strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), "TK-abcdef") {
		t.Fatalf("错误文案泄漏凭证: %v", err)
	}

	_, perr := p.parse(200, nil, json.RawMessage(`{"code":"401002","msg":"签名失败"}`))
	var pe *ProviderError
	if !errors.As(perr, &pe) || pe.Code != "401002" {
		t.Fatalf("包络失败未转成 ProviderError: %v", perr)
	}
	if pe.Retryable {
		t.Fatal("未登记的业务码不得可重试")
	}
}

// TestGatewayStatusBeatsSuccessEnvelope 非 2xx 时包络写得再漂亮也不算受理成功：
// 网关/反代拦下来的 502 页面里偶然带 "code":"0"，认成功就等于把没出去的单记成已出款。
func TestGatewayStatusBeatsSuccessEnvelope(t *testing.T) {
	p, err := NewProvider(ProviderOptions{Name: "wa", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: func(string) (string, bool) { return "/x", true }})
	if err != nil {
		t.Fatal(err)
	}
	okBody := json.RawMessage(`{"code":"0","data":{"order_no":"O1","status":"success"}}`)
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		_, err := p.parse(status, nil, okBody)
		var pe *ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("%d 却解析成功: %v", status, err)
		}
		if !pe.Retryable {
			t.Errorf("%d 应当可原单重发", status)
		}
	}
	// 4xx 是明确的拒绝语义，重发只会再挨一次。
	if _, err := p.parse(http.StatusBadRequest, nil, okBody); err == nil {
		t.Fatal("400 不能算成功")
	} else if pe := (&ProviderError{}); errors.As(err, &pe) && pe.Retryable {
		t.Error("400 不得标记为可重试")
	}
	// 2xx 但正文不是 JSON：同样不能当受理。
	if _, err := p.parse(http.StatusOK, nil, json.RawMessage("<html>blocked</html>")); err == nil {
		t.Fatal("200 + 非 JSON 不能算成功")
	}
}

// TestBadHeaderNameRejectedAtConstruction 请求头名写错（带空格或冒号）会让每一次请求
// 都在传输层失败、报错还长在供应商侧，必须在构造期就拦掉。
func TestBadHeaderNameRejectedAtConstruction(t *testing.T) {
	ep := func(string) (string, bool) { return "/x", true }
	for _, opt := range []ProviderOptions{
		{AuthHeader: "X Sign"},
		{TokenHeader: "Authorization: Bearer"},
	} {
		opt.Name, opt.BaseURL, opt.SecretKey, opt.Endpoint = "t", "https://x", "s", ep
		if _, err := NewProvider(opt); err == nil {
			t.Fatalf("非法头名 %q 应当拒绝构造", opt.AuthHeader+opt.TokenHeader)
		}
	}
}

// TestTokenRoundTrip 会话凭证只经 SetToken/Token 出入，不出现在任何报文体里。
func TestTokenRoundTrip(t *testing.T) {
	p, err := NewProvider(ProviderOptions{Name: "wa", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: func(string) (string, bool) { return "/x", true }})
	if err != nil {
		t.Fatal(err)
	}
	if p.Token() != "" {
		t.Fatal("初始应为未登录")
	}
	p.SetToken(" TK-1 ")
	if got := p.Token(); got != "TK-1" {
		t.Fatalf("token=%q，期望去掉首尾空白", got)
	}
	p.SetToken("")
	if p.Token() != "" {
		t.Fatal("空串应当清除会话凭证")
	}
}
