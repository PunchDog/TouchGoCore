package usdt

import "touchgocore/tron"

// 本文件是 TRON 侧收款端校验在 usdt 包的落脚点，只剩一个转发。
//
// 地址算法（Base58 解码、版本字节、双 SHA256 校验和）的归属地在 tron/address.go：
// 「地址怎么编码」是那条链的知识，USDT-TRC20 与原生 TRX 共用同一个地址形态，
// 两份实现迟早漂移，所以校验口径全仓唯一。
//
// 这里以前还有一段 init，往 pay 的链规则表里登记 USDT 规则。那张表已随 pay 瘦身
// 一起删除：规则现在收在 tron 包的 checkOrderAddress 里，出款路径直接调用它，
// 不存在「忘了登记就默认放行」这条失效通道。

// IsValidTRONAddress 判断是否为格式与校验和都通过的 TRON 账户地址。
//
// 它只回答「这是不是一个 TRON 地址」，不回答「这个地址存在吗」——后者要问全节点，
// 而一条格式正确的地址指向无人持有的账户，钱同样是找不回来的。
//
// 保留在本包导出是对既有调用方的兼容（合约地址校验、下游业务都在用）；
// 实现转发 tron 包，两侧结果永远一致。
func IsValidTRONAddress(s string) bool {
	return tron.IsValidTRONAddress(s)
}
