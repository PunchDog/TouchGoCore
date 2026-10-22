package pay

import (
	"context"
	"fmt"
)

// Channel 是一条已装配资金通道的统一调用面：出款、查单、供应商账户、链上余额。
//
// 它不替代 Transfer，而是在它之上补齐查询能力。分派仍按 Kind 走运行期登记：
// pay 不能 import 任何通道包（通道包 import pay，编译期成环），所以「按 type
// 调到 tron/bsc/sol/telegram」靠各包 Start 成功后的 Bind 完成，调用方只认 Kind。
//
// 为什么查询也收进同一个接口：上游拿到的是一条通道，不该在「出款走 A、查单走 B、
// 查余额走 C」三套入口之间自己拼。四个通道包都已实现这四件事；只实现 Withdraw
// 的旧装配方仍可 Bind，但 ChannelFor 会明确拒绝。
type Channel interface {
	Transfer
	QueryOrder(ctx context.Context, orderNo string) (*PayResult, error)
	QueryAccount(ctx context.Context, q *AccountQuery) (*AccountInfo, error)
	QueryOnchainAccount(ctx context.Context, q *OnchainAccountQuery) (*AccountInfo, error)
}

// OnchainAccountQuery 是「读热钱包链上余额」的输入。
//
// 与 AccountQuery 刻意分开：后者读的是供应商侧的商户额度（可用/冻结/授信），
// 前者读的是公链上那个地址还剩多少币。两者的数据源、量纲与故障处置都不同，
// 混成一个结构会让「余额读不出来」到底该找供应商还是找节点变得含糊。
//
// Address/Contract 留空表示用通道配置里的默认热钱包/合约。whatapp 这类
// 按单携带出款参数的调用方应显式填，避免通道默认值与这一单实际使用的钱包不一致。
type OnchainAccountQuery struct {
	Address  string `json:"address,omitempty"`
	Contract string `json:"contract,omitempty"`
	Network  string `json:"network,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// ChannelFor 取出某个种类的统一调用面。
//
// 未装配与「只实现了 Transfer」是两种不同错误：前者是通道没启动，后者是装配方
// 没提供查询能力。把后者也报成「未装配」会让运维去查配置，而问题在代码。
func ChannelFor(k Kind) (Channel, error) {
	bindingsMu.RLock()
	b, ok := bindings[k]
	bindingsMu.RUnlock()
	if !ok {
		return nil, unboundErr(k)
	}
	if b.channel == nil {
		return nil, fmt.Errorf("资金通道 %s 已装配，但实现未提供统一查询面（只有 Withdraw）", k)
	}
	return b.channel, nil
}

// QueryOrderTo 按种类查单。
func QueryOrderTo(ctx context.Context, k Kind, orderNo string) (*PayResult, error) {
	ch, err := ChannelFor(k)
	if err != nil {
		return nil, err
	}
	return ch.QueryOrder(ctx, orderNo)
}

// QueryAccountTo 按种类查供应商侧账户。
func QueryAccountTo(ctx context.Context, k Kind, q *AccountQuery) (*AccountInfo, error) {
	ch, err := ChannelFor(k)
	if err != nil {
		return nil, err
	}
	return ch.QueryAccount(ctx, q)
}

// QueryOnchainAccountTo 按种类查热钱包链上余额。
func QueryOnchainAccountTo(ctx context.Context, k Kind, q *OnchainAccountQuery) (*AccountInfo, error) {
	ch, err := ChannelFor(k)
	if err != nil {
		return nil, err
	}
	return ch.QueryOnchainAccount(ctx, q)
}
