package pay

import (
	"context"
	"strings"
	"testing"
)

// C-F3 用例：配置侧 Extras 与单内 Extra 同样过 payloadKeyNames 保留字校验。
// 撞保留字的表现是「配置改写报文金额/订单号，而签名域签的还是原值」——
// 校验和看着完全正确，供应商侧却对不上账，必须就地拒绝。

// TestMergeExtrasRejectsReservedConfigNames 配置侧 Extras 撞任一已签名量键名都要拒。
func TestMergeExtrasRejectsReservedConfigNames(t *testing.T) {
	reserved := []string{"app_id", "merchant_id", "order_no", "amount", "currency",
		"network", "address", "phone", "memo", "notify_url"}
	for _, name := range reserved {
		if len(payloadKeyNames) == 0 {
			t.Fatal("payloadKeyNames 为空，保留字名单失效")
		}
		if _, ok := payloadKeyNames[name]; !ok {
			t.Fatalf("%s 应在保留字名单里（OrderRequest 字段变了？同步本用例）", name)
		}
		_, _, err := mergeExtras([]ExtraField{{Name: name, Value: "x"}}, nil)
		if err == nil {
			t.Errorf("配置侧 Extras 撞保留字 %s 未被拒", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("错误文案应指名撞了的键 %s，实得: %v", name, err)
		}
	}
	// 非保留名照常通过，别把闸修成常闭。
	if _, signs, err := mergeExtras([]ExtraField{{Name: "contract", Value: "c"}}, nil); err != nil || len(signs) != 1 {
		t.Fatalf("合法配置侧 Extras 被误拒: signs=%v err=%v", signs, err)
	}
}

// TestConfigExtrasReservedNameSendsNothing 端到端：撞保留字的配置在发出任何请求之前被拒，
// 且错误文案不带出密钥。
func TestConfigExtrasReservedNameSendsNothing(t *testing.T) {
	r := newSDKServer(t)
	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "usdt", BaseURL: r.srv.URL, SecretKey: "TOPSECRET",
		Endpoint: endpoints(),
		Extras:   []ExtraField{{Name: "amount", Value: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ch.Recharge(context.Background(), &PayOrder{OrderNo: "O1", Amount: 1000, Currency: CurrencyUSDT})
	if err == nil {
		t.Fatal("配置 Extras 撞保留字应当报错")
	}
	if !strings.Contains(err.Error(), "amount") {
		t.Errorf("错误文案应指名撞了的键，实得: %v", err)
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("错误文案泄漏密钥: %v", err)
	}
	if n := r.calls.Load(); n != 0 {
		t.Errorf("撞保留字却发出了 %d 次请求", n)
	}
}
