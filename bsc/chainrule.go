package bsc

import "touchgocore/pay"

// 本文件是「收款地址与备注」在 BSC 侧的包内收口。
//
// 它曾经登记到 pay 的 ChainRule 表里（键是币种名，未登记即放行）。那张表随 pay
// 瘦身一起消失，而「未登记即放行」本来就是一道默认放行的闸：币种名拼错一个字母，
// 地址校验就静默失效，等发现时钱已经发去别处了。
//
// 现在把门收进包内：本包的出款路径只有一条（BEP20 transfer），地址规则也就只有
// 一个调用点。原生 BNB 与 USDT-BEP20 共用同一份规则——同一条链，地址形态与
// memo 口径本来就一致——所以这里只有一份实现。

// checkOrderAddress 用 BSC 的地址规则校验这一单的收款方。
//
// 调用点在链上通道的 Withdraw 里，且已先经 pay.CheckTarget{Wallet:true}——
// 走到这里收款方一定是钱包性质，因此不再重复判 kind：同一件事判两遍，
// 两处判据迟早漂移，而漂移的那一次通常是「新增一类收款方，只改了一处」。
func checkOrderAddress(o *pay.PayOrder) error {
	rule := bnbRule{}
	if err := rule.CheckAddress(o); err != nil {
		return err
	}
	return rule.CheckMemo(o)
}
