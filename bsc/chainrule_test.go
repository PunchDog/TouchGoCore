package bsc

import (
	"strings"
	"testing"

	"touchgocore/pay"
)

// TestCheckOrderAddress 包内地址规则的四个口径。
//
// 这条闸代替了原来登记在 pay 注册表里的 ChainRule：那时「未登记即放行」，
// 币种名拼错一个字母就静默失效；现在出款路径只有一个调用点，规则就在那个点上。
func TestCheckOrderAddress(t *testing.T) {
	ok := []struct {
		desc string
		to   *pay.Party
	}{
		{"EIP-55 校验和形态", &pay.Party{Address: evmChecksumMixed}},
		{"全小写（无校验和语义）", &pay.Party{Address: evmAllLower}},
		{"全大写（无校验和语义）", &pay.Party{Address: evmAllUpper}},
		{"纯数字", &pay.Party{Address: evmAllDigits}},
		{"首尾空白", &pay.Party{Address: "  " + evmAllLower + "\n"}},
		{"空地址（非空检查在 pay.CheckOrder）", &pay.Party{}},
	}
	for _, cs := range ok {
		if err := checkOrderAddress(&pay.PayOrder{To: cs.to}); err != nil {
			t.Errorf("%s: 被拒: %v", cs.desc, err)
		}
	}

	bad := []struct {
		desc string
		to   *pay.Party
		want string
	}{
		{"校验和不过", &pay.Party{Address: evmBogusChecksum}, "不合法"},
		{"少一位", &pay.Party{Address: evmAllLower[:len(evmAllLower)-1]}, "不合法"},
		{"缺 0x 前缀", &pay.Party{Address: evmAllLower[2:]}, "不合法"},
		{"混入非法字符", &pay.Party{Address: "0xzz34567890123456789012345678901234567890"}, "不合法"},
		{"TRON 地址", &pay.Party{Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}, "不合法"},
		{"带 memo", &pay.Party{Address: evmAllLower, Memo: "123456"}, "不支持备注"},
		{"memo 只有空白", &pay.Party{Address: evmAllLower, Memo: "  "}, ""},
	}
	for _, cs := range bad {
		err := checkOrderAddress(&pay.PayOrder{To: cs.to})
		if cs.want == "" {
			if err != nil {
				t.Errorf("%s: 被拒: %v", cs.desc, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 未被拒", cs.desc)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 拒因文案不符（缺 %q）: %v", cs.desc, cs.want, err)
		}
	}

	// nil 订单/收款方放行：本函数只回答「格式对不对」，
	// 「有没有收款方」是 pay.CheckOrder 的职责，两处都判会让判据漂移。
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单被拒: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("nil 收款方被拒: %v", err)
	}
}
