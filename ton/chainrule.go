package ton

import "touchgocore/pay"

// checkOrderAddress 用 TON 的地址与备注规则校验这一单的收款方。
//
// 地址规则住在 address.go；本文件只负责「出款入口唯一那份调用」。
// 调用点在 tonChannel.Withdraw 里，已先经 pay.CheckTarget{Wallet:true}——
// 到这里收款方一定是钱包性质，不再重复判 kind。
func checkOrderAddress(o *pay.PayOrder) error {
	rule := tonRule{}
	if err := rule.CheckAddress(o); err != nil {
		return err
	}
	return rule.CheckMemo(o)
}
