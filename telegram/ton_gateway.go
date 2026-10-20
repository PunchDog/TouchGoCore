package telegram

import (
	"context"

	"github.com/tonkeeper/tongo/ton"
)

// tonGateway 是 TON 链上通道对「公链」这件事的全部提问。
//
// 它存在的理由与 tron/chainGateway 同一条：出款链路真正的危险不在组包与签名
// （那些能对着固定输入逐字测），而在「广播之后发生了什么」。把节点访问收进一个窄接口，
// 双付场景才能离线复现——用假网关把广播打成不确定态，断言这里没有签出第二笔。
//
// 未导出的刻意性：通道外部（含下游）不该拿到「直接广播一笔外部消息」的能力，
// 出款只有 pay.Channel.Withdraw 一个入口。
type tonGateway interface {
	// seqno 读钱包账户当前的序号（下一笔要用的号就是它）。
	// 未部署的账户由调用方按状态判，不在这里猜 0。
	seqno(ctx context.Context, account ton.AccountID) (uint32, error)

	// account 读某地址的原生币状态。TON 上出款用的币与付 gas 用的币是同一种，
	// 所以这里只有一份余额，不像 TRON 要分代币与 TRX 两次读。
	account(ctx context.Context, account ton.AccountID) (*tonAccount, error)

	// sendExtMessage 把序列化好的外部消息（BOC 字节）交给接入点。
	//
	// 结论必须按「钱有没有可能动过」给：Uncertain=true 表示请求可能已经到达网络但结果未知，
	// 调用方只能转成查证分支，绝不允许回到「重新组一笔」（那是双付）。
	sendExtMessage(ctx context.Context, payload []byte) (*tonBroadcast, error)

	// transactions 列本账户近期交易（按链上时间倒序），最多 limit 条。
	// 它同时服务两件事：按外部消息 hash 找回落链结论，以及台账丢失后按收端+金额反查。
	transactions(ctx context.Context, account ton.AccountID, limit int) ([]tonTx, error)

	// close 释放接入点持有的东西。TON 侧走的是普通 HTTP，没有长连接可关，
	// 但接口保留它：门面 stop 时统一断言 Close，将来换 lite server 接入不必改调用方。
	close() error
}

// tonAccount 是一个链上账户的原生币状态。
type tonAccount struct {
	// BalanceNanoton 是可用余额（纳吨）。
	BalanceNanoton int64
	// Status 是链上账户状态：nonexist / uninit / active / frozen，取值逐字来自接入点。
	// 留字符串而不是布尔：frozen 与 uninit 的处置不同（前者不该再往里发钱）。
	Status string
}

// tonBroadcast 是一次广播的结论。
type tonBroadcast struct {
	// Code 是接入点给的结论摘要（状态码与原文），逐字保留进 RawNote，不做二次映射。
	Code string
	// Accepted 表示接入点受理了这一笔：接下来只剩查证。
	Accepted bool
	// Rejected 表示接入点在投递之前就拒了（消息不合法、状态对不上）。
	// 这一笔不可能上链，同一单号可以重建，因此它不是终局。
	Rejected bool
	// Uncertain 见 tonGateway.sendExtMessage。
	Uncertain bool
}

// tonTx 是一条已落链交易的归一回执。
//
// 只留判定要用的几项：hash 与 in_msg 用来认领「是不是我们那一笔」，
// success/verdictKnown 用来决定能不能给终局，outs 用来反查金额与收端。
type tonTx struct {
	// Hash 是交易 hash（十六进制）。
	Hash string
	// UnixTime 是交易上链时间（秒）。
	UnixTime int64
	// TotalFeesNanoton 是这一笔实际烧掉的手续费（纳吨）。
	TotalFeesNanoton int64
	// InMsgHash 是触发这笔交易的消息 hash；外部消息触发时它就是台账里的 TxID。
	InMsgHash string
	// InMsgKind 是触发消息的类型（ext_in_msg / int_msg / …）。
	// 只认 ext_in_msg 的交易为「我们广播的那一笔」：内行消息触发的事务是别人的动作。
	InMsgKind string
	// Success 是链上执行成功（交易本身没被中止且动作阶段成功）。
	Success bool
	// VerdictKnown 是接入点有没有给出可判定的执行结果。
	//
	// false 时调用方必须回 UNKNOWN 而不是 FAILED：回执读不懂却判失败，上游就会重下一单，
	// 而链上那一笔可能已经把币转出去了——那是业务层的双付（口径同 tron/chainGateway）。
	VerdictKnown bool
	// Bounced 是这一笔的转账被退回给了发端（收款端不可达时链上会发回一条 bounced 消息）。
	// 钱没落到对方手上，但手续费已经烧掉。
	Bounced bool
	// Outs 是这一笔派生的内部消息（真正的资金去向）。
	Outs []tonOut
}

// tonOut 是一笔派生的内部转账。
type tonOut struct {
	// To 是收款端地址（用户友好形态或原始形态，取决于接入点给什么）。
	To string
	// AmountNanoton 是这一笔带过去的金额（纳吨）。
	AmountNanoton int64
}
