// Package ton 是 TON 链的资金通道：原生 TON 的链上出款、查单、链上余额与
// 供应商无关的地址规则。
//
// 分层口径与 tron/bsc/sol 一致：
//   - 装配读自己的配置段 ton.native，Start 成功即 pay.Bind(pay.KindTON, ch)，
//     并使 pay.ChannelFor(pay.KindTON) 可用；
//   - 出款在本地组外部消息、用配置热钱包私钥签名，经 TonAPI 广播，结论按消息 hash 读回；
//   - 幂等由 touchgocore/ledger 保证：同一单号永不签第二笔，先记账再广播；
//   - 金额只接受 int64 最小单位（纳吨），amount_units 显式拒绝；TON 不支持按单私钥。
//
// 供应商侧账户查询已删除：QueryAccount 读默认热钱包链上余额，QueryOnchainAccount
// 可指定地址。两者都不返回授信/费率。
package ton
