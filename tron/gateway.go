package tron

import (
	"context"
	"errors"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
)

// chainGateway 是链上通道对「公链」这件事的全部提问。
//
// 它存在的理由是可验证性：出款链路真正的危险不在组包与签名（那些能对着固定输入逐字测），
// 而在「广播之后发生了什么」。把节点访问收进一个窄接口，双付场景才能离线复现——
// 用假网关把广播打成超时，断言这里没有签出第二笔。
//
// 未导出的刻意性：通道外部（含下游 whatapp）不该拿到「直接广播一笔交易」的能力，
// 出款只有 chainChannel.Withdraw 一个入口。
type chainGateway interface {
	// buildTransfer 让节点组装一笔未签名的 TRC20 转账（from 是热钱包地址）。
	// 返回的交易自带 refBlock/expiration/fee_limit，这三项都不该由本地凭空填。
	buildTransfer(ctx context.Context, from, to, contract string, amount int64, feeLimit int64) (*core.Transaction, error)

	// broadcast 把已签名交易交给节点。
	//
	// Uncertain=true 表示「请求可能已经到达网络但结果未知」——调用方必须转成按 txid
	// 查证的分支，绝不允许回到「重新组一笔」（那是双付）。
	broadcast(ctx context.Context, tx *core.Transaction) (*broadcastResult, error)

	// lookupTx 按交易号查链上回执；未上链时 Found=false 而不是报错。
	lookupTx(ctx context.Context, txID string) (*txReceipt, error)

	// tokenBalance 读某地址在该合约下的代币余额（最小单位）。
	tokenBalance(ctx context.Context, addr, contract string) (int64, error)

	// account 读某地址的原生币状态（TRON 为 TRX，gas 收在它身上，与本单币种不同一种）。
	account(ctx context.Context, addr string) (*accountState, error)

	// headBlock 返回当前链头高度，用于把「已进块」换算成确认数。
	headBlock(ctx context.Context) (int64, error)

	// outboundTransfers 列出自 sinceMS（毫秒时间戳）以来本地址发出的该合约转账，
	// 用于台账丢失后反查「这一单是不是其实已经上过链」。
	outboundTransfers(ctx context.Context, addr, contract string, sinceMS int64) ([]outboundTransfer, error)

	// close 释放连接。
	close() error
}

// errWriteOverHTTP 是 REST 后端的写侧闸门。
//
// 出款广播只走 gRPC 一条路：多一条「没验过的路径也会动钱」，出事时连该信哪条都不知道。
var errWriteOverHTTP = errors.New("链上广播只走 gRPC 接入点，REST 侧不做写操作")

// broadcastResult 是广播的一次结论。
type broadcastResult struct {
	// Code 是节点给的响应码，逐字保留进 RawNote，不做二次映射。
	Code string
	// Accepted 表示节点受理了这一笔（进了池或已进块）：接下来只剩查证。
	Accepted bool
	// Rejected 表示节点在入池之前就拒了（签名、TAPOS、过期、超限一类）：
	// 这一笔不可能上链，同一单号可以重建，因此它不是终局。
	Rejected bool
	// Uncertain 见 chainGateway.broadcast。
	Uncertain bool
}

// txReceipt 是一条链上交易的归一回执。
type txReceipt struct {
	// Found=false 表示节点没有这条交易的回执：既可能是还没进块，也可能是压根没广播出去。
	// 这两态在本地分不开，只能靠台账里「有没有广播过」来定，见 touchgocore/ledger。
	Found bool
	// InBlock 是已进块（回执里有区块号）。
	InBlock bool
	// Success 是「合约真的把币转出去了」。判据只认回执里的执行结果
	// （gRPC 的 receipt.result / REST 的 receipt.result），不认 contractResult 字节：
	// USDT-TRC20 是非标准 TRC20，它的 transfer 不返回 bool，链上 contractResult
	// 是 32 字节全零——拿字节判会把每一笔真成功的 USDT 出款读成失败（真机已复现）。
	Success bool
	// VerdictKnown 是链上有没有给出明确判定。
	//
	// false 时调用方必须回 UNKNOWN 而不是 FAILED：回执读不懂却判失败，
	// 上游就会重下一单，而链上那一笔可能已经把币转出去了——那是业务层的双付。
	VerdictKnown bool
	// BlockNumber 是进块高度；未进块为 0。
	BlockNumber int64
	// FeeSun 是这一单实际烧掉的手续费（原生币最小单位）。
	FeeSun int64
	// ResultCode 是节点给的原始结果码，只用于留痕。
	ResultCode string
}

// accountState 是一个链上账户的原生币状态。
//
// 只留余额一项：出款前置闸要判的就是「gas 够不够」。这里刻意不放「账户是否已激活」
// 之类的字段——真机实测（2026-10-16，主网 USDT 合约账户）REST 侧读回的 create_time
// 并不可靠，一个持有 945 万 USDT 的账户会被读成「不存在」。放一个会骗人的字段，
// 比不放更糟；真要判收款地址是否激活，得另找可验证的口径再加分支。
type accountState struct {
	// BalanceSun 是可用的原生币余额（TRON 上即付 gas 的那一份）。
	BalanceSun int64
}

// outboundTransfer 是一条已经上了链的本地址转出记录（台账反查用）。
type outboundTransfer struct {
	TxID        string
	To          string
	Amount      int64
	TimestampMS int64
}
