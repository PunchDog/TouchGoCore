package pay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sdkRecorder 记下假供应商收到的每一次请求，供断言报文与签名头。
type sdkRecorder struct {
	srv    *httptest.Server
	calls  atomic.Int32
	bodies atomic.Value // string
	signs  atomic.Value // string
	auth   atomic.Value // string
	resp   atomic.Value // string
}

func newSDKServer(t *testing.T) *sdkRecorder {
	t.Helper()
	r := &sdkRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.calls.Add(1)
		r.bodies.Store(string(body))
		r.signs.Store(req.Header.Get("X-Sign"))
		r.auth.Store(req.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		resp, _ := r.resp.Load().(string)
		if resp == "" {
			resp = `{"code":"0","msg":"ok"}`
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// endpoints 把逻辑名映射成真实路径，模拟配置里的 endpoints 表。
func endpoints(missing ...string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, bad := range missing {
			if bad == name {
				return "", false
			}
		}
		return "/" + name, true
	}
}

// TestRegistryRejectsBadRegistration 空名、nil 工厂、重名抢注都要被拒。
// 两处实现抢同一个名字时，生效的是初始化顺序而不是配置意图。
func TestRegistryRejectsBadRegistration(t *testing.T) {
	f := func(ProviderOptions) (Channel, error) { return nil, nil }
	if err := Register("  ", f); err == nil {
		t.Error("空 SDK 名应当被拒")
	}
	if err := Register("t_nil", nil); err == nil {
		t.Error("nil 工厂应当被拒")
	}
	if err := Register("t_dup", f); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	if err := Register("t_dup", f); err == nil {
		t.Error("重名登记应当被拒而不是覆盖")
	}
	if _, err := Open("t_dup", ProviderOptions{}); err == nil {
		t.Error("工厂返回 nil 通道时不得当成功")
	}
	if _, err := Open("  ", ProviderOptions{}); err == nil {
		t.Error("未指定 SDK 名应当报错")
	}
	if _, err := Open("no_such_sdk", ProviderOptions{}); err == nil {
		t.Error("未登记的 SDK 名应当报错")
	} else if !strings.Contains(err.Error(), DriverGeneric) {
		t.Errorf("报错里应列出可用驱动，实得 %v", err)
	}
}

// TestGenericDriverRegistered 内置驱动开箱可用，Drivers 排序稳定。
func TestGenericDriverRegistered(t *testing.T) {
	found := false
	for _, n := range Drivers() {
		if n == DriverGeneric {
			found = true
		}
	}
	if !found {
		t.Fatalf("内置驱动 %s 未登记，实得 %v", DriverGeneric, Drivers())
	}
}

// TestChannelOperationsSignAndSerialize 走注册表开出来的通道：
// 报文含商户号与整数金额，签名头按签名域算出，未配置的操作在发请求前就拒。
func TestChannelOperationsSignAndSerialize(t *testing.T) {
	r := newSDKServer(t)
	r.resp.Store(`{"code":"0","data":{"order_no":"O1","trade_no":"T9","status":"success","amount":1000}}`)
	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "ustd", BaseURL: r.srv.URL, SecretKey: "TOPSECRET",
		AppID: "app1", MerchantID: "MCH-1", Timeout: 2 * time.Second,
		Endpoint: endpoints(),
		Extras:   []ExtraField{{Name: "contract", Value: "TR7"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Driver() != DriverGeneric {
		t.Fatalf("SDK 名未回填: %s", ch.Driver())
	}
	res, err := ch.Recharge(context.Background(), &PayOrder{OrderNo: "O1", Amount: 1000, Currency: CurrencyUSDT})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSuccess || res.TradeNo != "T9" {
		t.Fatalf("回执归一不符: %+v", res)
	}
	body := r.bodies.Load().(string)
	for _, want := range []string{`"order_no":"O1"`, `"amount":1000`, `"merchant_id":"MCH-1"`, `"contract":"TR7"`, `"app_id":"app1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("报文缺 %s，实得 %s", want, body)
		}
	}
	if strings.Contains(body, ".") {
		t.Fatalf("金额出现浮点形态: %s", body)
	}
	// 签名域：十个定长标量（此单只有币种有值）+ 按登记顺序追加的通道特有字段。
	want := SignMD5("TOPSECRET",
		"app1", "MCH-1", "O1", "1000", CurrencyUSDT, "", "", "", "", "",
		"TR7")
	if got := r.signs.Load().(string); got != want {
		t.Fatalf("签名=%s，期望 %s（签名域=通用字段+Extras 登记顺序）", got, want)
	}

	// 提现缺地址：请求不该发出去。
	before := r.calls.Load()
	if _, err := ch.Withdraw(context.Background(), &PayOrder{OrderNo: "O2", Amount: 1}); err == nil {
		t.Fatal("提现缺地址应当被拒")
	}
	if r.calls.Load() != before {
		t.Error("前置校验失败却发出了请求")
	}

	// 账户端点未配置：查账户应在发请求前被拒。
	r2 := newSDKServer(t)
	ch2, err := Open(DriverGeneric, ProviderOptions{
		Name: "ton", BaseURL: r2.srv.URL, SecretKey: "s", Endpoint: endpoints(EndpointAccount),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch2.QueryAccount(context.Background(), nil); err == nil {
		t.Fatal("未配置 account 端点应当报错")
	} else if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("错误文案泄漏凭证: %v", err)
	}
	if r2.calls.Load() != 0 {
		t.Error("未配置端点却发出了请求")
	}
}

// TestQueryAccountNormalizes 账户查询：金额与费率按整数透出，状态词认不出算 UNKNOWN。
func TestQueryAccountNormalizes(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantErr bool
		check   func(*testing.T, *AccountInfo)
	}{
		{"正常账户", `{"merchant_id":"M1","currency":"USDT","balance":9,"available":7,"frozen":2,"credit":3,"fee_rate_bps":30,"status":"active"}`, false,
			func(t *testing.T, a *AccountInfo) {
				if a.Status != AcctActive || a.Balance != 9 || a.Available != 7 || a.Frozen != 2 || a.Credit != 3 || a.FeeRateBps != 30 {
					t.Fatalf("账户字段不符: %+v", a)
				}
				if !a.CanWithdraw(7) || a.CanWithdraw(8) {
					t.Fatalf("可提现判断不符: %+v", a)
				}
			}},
		{"冻结账户", `{"status":"frozen","available":100}`, false,
			func(t *testing.T, a *AccountInfo) {
				if a.Status != AcctFrozen || a.CanWithdraw(1) {
					t.Fatalf("冻结账户仍判可提现: %+v", a)
				}
			}},
		{"状态词未登记", `{"status":"weird","available":100}`, false,
			func(t *testing.T, a *AccountInfo) {
				if a.Status != AcctUnknown || a.CanWithdraw(1) {
					t.Fatalf("没看懂的状态不得当成正常: %+v", a)
				}
				if !strings.Contains(a.RawNote, "weird") {
					t.Fatalf("摘要未记录原始状态词: %+v", a)
				}
			}},
		{"载荷不可解析", `"not an object"`, true, func(*testing.T, *AccountInfo) {}},
	}
	for _, cs := range cases {
		r := newSDKServer(t)
		r.resp.Store(`{"code":"0","data":` + cs.data + `}`)
		ch, err := Open(DriverGeneric, ProviderOptions{
			Name: "ustd", BaseURL: r.srv.URL, SecretKey: "s", AppID: "app1", MerchantID: "MCH-1",
			Endpoint: endpoints(),
		})
		if err != nil {
			t.Fatal(err)
		}
		info, err := ch.QueryAccount(context.Background(), &AccountQuery{Currency: CurrencyUSDT})
		if cs.wantErr {
			if err == nil {
				t.Fatalf("%s: 期望报错，实得 %+v", cs.name, info)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", cs.name, err)
		}
		cs.check(t, info)
		if r.signs.Load().(string) == "" {
			t.Errorf("%s: 账户查询未带签名头", cs.name)
		}
	}
}

// TestMerchantIDPrecisionKept 大额最小单位整数不得经过浮点转换。
// 报文展开成 map 再回序列化时若用了默认解码，2^53 以上的金额会静默变形。
func TestMerchantIDPrecisionKept(t *testing.T) {
	r := newSDKServer(t)
	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "ustd", BaseURL: r.srv.URL, SecretKey: "s",
		Endpoint: endpoints(),
		Extras:   []ExtraField{{Name: "remark", Value: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const big int64 = 9_007_199_254_740_993 // 2^53 + 1
	if _, err := ch.Recharge(context.Background(), &PayOrder{OrderNo: "O9", Amount: big}); err != nil {
		t.Fatal(err)
	}
	body := r.bodies.Load().(string)
	if !strings.Contains(body, `"amount":9007199254740993`) {
		t.Fatalf("金额精度丢失: %s", body)
	}
	var back OrderRequest
	if err := json.Unmarshal([]byte(body), &back); err != nil {
		t.Fatal(err)
	}
	if back.Amount != big {
		t.Fatalf("回读金额=%d，期望 %d", back.Amount, big)
	}
}

// TestMarshalPayloadSkipsEmptyExtras 值为空的特有字段既不进报文也不进签名域，
// 否则供应商会把 null 当成显式清空指令；非空的两侧必须同时出现——
// 报文与签名域由 mergeExtras 一次遍历产出，这里同时钉住它两。
func TestMarshalPayloadSkipsEmptyExtras(t *testing.T) {
	base := OrderRequest{AppID: "a", OrderNo: "O1", Amount: 1}
	fields, signs, err := mergeExtras(
		[]ExtraField{{Name: "contract", Value: "c"}, {Name: "remark", Value: ""}},
		map[string]string{"operator": "op", "ticket": ""},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(signs) != 2 || signs[0] != "c" || signs[1] != "op" {
		t.Fatalf("签名域追加值不符: %#v", signs)
	}
	if _, ok := fields["remark"]; ok {
		t.Fatalf("空值字段进了报文: %#v", fields)
	}
	body, err := marshalPayload(base, fields)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, "remark") {
		t.Fatalf("空值字段仍进报文: %s", got)
	}
	if !strings.Contains(got, `"contract":"c"`) || !strings.Contains(got, `"amount":1`) {
		t.Fatalf("报文不符: %s", got)
	}
}
