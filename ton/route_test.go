package ton

import (
	"testing"

	"touchgocore/pay"
)

// 本文件只钉 TON 侧的「接线正确性」：分派种类的取值。门面机器本身（分派登记、
// 重启关旧、订单补全、未启动拒绝）的行为基线在 chainroute/route_test.go 只写一份，
// 四链共用；TON 侧那三条重启/分派用例已上移到那里。

// TestKindTONValue 配置段名、注入钩子键与台账链维度都从 Kind 派生，值不能漂移。
func TestKindTONValue(t *testing.T) {
	if string(pay.KindTON) != "ton.native" {
		t.Fatalf("pay.KindTON=%q，期望 ton.native", pay.KindTON)
	}
}
