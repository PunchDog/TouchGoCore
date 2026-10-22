package pay

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSecretNeverLeaks 私钥会跟着 PayOrder 穿过日志、回调与错误包装；
// 类型必须在 fmt 的四种常用动词与 JSON 两条出口上都只给掩码。
func TestSecretNeverLeaks(t *testing.T) {
	const plain = "0xdeadbeefcafebabe"
	s := Secret(plain)
	order := &PayOrder{
		OrderNo: "O1", Amount: 1,
		To:   &Party{Kind: PartyWallet, Address: "TAddr"},
		From: &Party{Kind: PartyWallet, Address: "TFrom", Contract: "TContract", PrivateKey: s},
	}

	for _, got := range []string{
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", order),
		fmt.Sprintf("%+v", order),
		fmt.Sprintf("%#v", order),
	} {
		if strings.Contains(got, plain) {
			t.Fatalf("私钥明文进了格式化输出: %s", got)
		}
	}

	b, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), plain) {
		t.Fatalf("私钥明文进了 JSON: %s", b)
	}
	if strings.Contains(string(b), "private_key") {
		t.Fatalf("私钥字段不该出现在 JSON 里: %s", b)
	}

	// 直接序列化 Secret 也不能泄露。
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatalf("Secret 直接序列化泄露: %s", raw)
	}

	if s.Reveal() != plain {
		t.Fatal("Reveal 应返回明文，供签名/派生地址使用")
	}
}

// TestPartyKeepsPerOrderContract 合约是可序列化的订单事实（不是密钥），
// 空值仍按 omitempty 消失。它进 JSON 才能让回执与审计看出这一单发的是哪张合约。
func TestPartyKeepsPerOrderContract(t *testing.T) {
	b, err := json.Marshal(&Party{Kind: PartyWallet, Address: "TAddr", Contract: "TContract"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"contract":"TContract"`) {
		t.Fatalf("合约未进 JSON: %s", b)
	}
	empty, err := json.Marshal(&Party{Kind: PartyWallet, Address: "TAddr"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "contract") {
		t.Fatalf("空合约不该出现在 JSON: %s", empty)
	}
}
