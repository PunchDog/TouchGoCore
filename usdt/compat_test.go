// 兼容面的编译期钉子。
//
// 本包的导出符号是下游的编译依赖：whatapp 服务的 internal/service/payout/channel.go
// 直接调这三个门面，cmd/t1jobserver/main.go 还同时 blank-import 本包与 tron 落链规则。
// 实现搬去 tron 之后，这里被删掉一个函数或改一个参数都不会让本仓的测试变红，
// 只会在下游编译时才炸——所以把签名逐字钉成类型断言式的赋值，本仓 go vet 就能看见。
package usdt_test

import (
	"context"
	"testing"

	"touchgocore/pay"
	"touchgocore/usdt"
)

var (
	_ func(context.Context, *pay.PayOrder) (*pay.PayResult, error)       = usdt.UsdtWithdraw
	_ func(context.Context, string) (*pay.PayResult, error)              = usdt.UsdtQuery
	_ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = usdt.UsdtAccount
	_ func(context.Context)                                              = usdt.UsdtStart
	_ func(context.Context)                                              = usdt.UsdtStop
	_ func(string) bool                                                  = usdt.IsValidTRONAddress
)

// TestCompatSurfaceIsLive 证明上面钉住的不是一份幽灵签名：
// 门面在没启动时按 USDT 口径报错，地址判定与 tron 侧同一份实现结果一致。
func TestCompatSurfaceIsLive(t *testing.T) {
	usdt.UsdtStop(nil)
	if _, err := usdt.UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: "T9yED5xMV5ARV98BexN97aLZ1UUq7eKSxm"}); err == nil {
		t.Fatal("未启动时提现应当被拒")
	}
	if !usdt.IsValidTRONAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t") {
		t.Error("官方 USDT-TRC20 合约被判为非法地址")
	}
}
