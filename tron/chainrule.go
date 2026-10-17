package tron

import "touchgocore/pay"

// 本文件是 pay.ChainRule 的唯一登记点：TRX 与 USDT 共用 address.go 的 TRONRule
// （同一条链，地址形态与 memo 口径本来就一致，两份规则迟早漂移）。
//
// 登记点只能有一处：RegisterChainRule 重名即返回 error，而下游 main 会同时
// blank-import touchgocore/tron 与 touchgocore/usdt，两个包各登记一次就在启动期 panic。

func init() {
	// 登记失败只可能是本包把币种名写错了，让它炸在启动期。
	if err := pay.RegisterChainRule(pay.CurrencyTRX, TRONRule{}); err != nil {
		panic(err)
	}
	if err := pay.RegisterChainRule(pay.CurrencyUSDT, TRONRule{}); err != nil {
		panic(err)
	}
}
