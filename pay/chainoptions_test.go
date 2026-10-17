package pay

import (
	"strings"
	"testing"
)

// gateCoin 单独占一个币种名：登记表是进程级全局的，借用别的用例的币种会替它把
// 「首次注册」抢走，让那个用例的重名拒绝先炸。
const gateCoin = "TESTCOIN-GATE"

// TestCheckChainRulesIsTheSameGate 链上驱动用的导出入口必须与提现链路走同一份规则表：
// 两处各校验一遍迟早漂移，而漂移的表现是「供应商链路拒了这个地址、链上链路把它广播出去了」。
func TestCheckChainRulesIsTheSameGate(t *testing.T) {
	// -count=3 时第二次注册会撞重名；重名不是本用例要判的事。
	if err := RegisterChainRule(gateCoin, fakeRule{}); err != nil && !strings.Contains(err.Error(), "不得覆盖") {
		t.Fatal(err)
	}
	good := &PayOrder{OrderNo: "O1", Amount: 10, Currency: gateCoin, Address: goodAddr}
	if err := CheckChainRules(good); err != nil {
		t.Fatalf("合法地址被导出入口拒掉: %v", err)
	}
	bad := &PayOrder{OrderNo: "O2", Amount: 10, Currency: gateCoin, Address: "x"}
	if err := CheckChainRules(bad); err == nil || !strings.Contains(err.Error(), "地址不合法") {
		t.Fatalf("坏地址应当被导出入口拒掉，实得 %v", err)
	}
	if err := CheckChainRules(&PayOrder{OrderNo: "O3", Amount: 10, Currency: "NO_RULE_COIN", Address: "anything"}); err != nil {
		t.Fatalf("未登记规则的币种应按既有口径放行格式校验，实得 %v", err)
	}
}

// TestGenericDriverRefusesChainConfig 带 chain 配置却用通用驱动时必须当场拒。
//
// 这一格错配的表现是「节点、热钱包、私钥都配好了，出款却仍然发给供应商」——
// 供应商不认识那个地址，而配置读起来像是已经上链了，比直接报错难查一个量级。
func TestGenericDriverRefusesChainConfig(t *testing.T) {
	const pk = "CHAINONLYPRIVATEKEY"
	_, err := NewProvider(ProviderOptions{
		Name: "usdt", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: endpoints(),
		Chain:    &ChainOptions{PrivateKey: pk, Node: "grpc.example:50051"},
	})
	if err == nil {
		t.Fatal("generic_md5 驱动不该接受带 chain 的配置")
	}
	if !strings.Contains(err.Error(), "driver") {
		t.Errorf("报错要指明是驱动名没换: %v", err)
	}
	if strings.Contains(err.Error(), pk) {
		t.Errorf("报错泄漏了私钥: %v", err)
	}
}
