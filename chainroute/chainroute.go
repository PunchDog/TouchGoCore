// Package chainroute 承载四条链上资金通道（tron / ton / bsc / sol）共用的门面机器：
// 「快照客户端 → 调通道 → 广播回执」，以及装配 / 分派登记 / 重启关旧的生命周期。
//
// 为什么抽出来而不是各抄一份：这套机器曾在四个包各写一份逐字副本，而重复已经产生
// 实证代价——「重启时关闭旧连接」的修复只落在 ton 一份上，tron/bsc/sol 继续泄漏
// gRPC/RPC 连接；同一条 QueryOrder 加锁要手工改四遍。现在 Route 只有一份，行为
// 修一次即可。链-specific 的知识（地址规则、链上查证、签名与装配）仍留在各链包，
// 这里只把它们抽象成 Channel 接口与 NewClient 装配钩子。
//
// 依赖方向：通道包 import chainroute，chainroute import pay/util/vars/config/corectx；
// pay 不 import 任何通道包，因此不成环。
package chainroute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// Channel 是门面对「一条链上资金通道」的全部要求。
//
// 与 pay.Channel 同一宽度（出款、查单、供应商账户、链上余额），另加 Close：
// 门面停服与重启要释放接入点连接，而 Close 不在 pay.Channel 里。实现了本接口的
// 通道自动满足 pay.Transfer 与 pay.Channel，可直接交给 pay.Bind 登记分派。
type Channel interface {
	Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error)
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
	QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error)
	Close() error
}

// Snapshot 把资金通道与「这条链路属于哪张网络」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换 network，会出现用新网关发旧链的单子。
type Snapshot struct {
	Ch      Channel
	Network string
}

// Route 是一条通道门面的全部可变状态与差异点。差异点做成字段由各链注入，
// 机器（装配、分派、回执、订单补全）四链共用一份。
type Route struct {
	// Label 出现在日志、错误前缀与回执文案里，必须与各链合并前的写法逐字一致。
	Label string
	// ConfigKey 是「未启动」报错里指向的配置段路径（如 tron.usdt / ton.native）。
	ConfigKey string
	// NotStartedHint 是「未启动」报错里的检查项提示，各链口径略有差异。
	NotStartedHint string
	// Callback 是回执广播的 util 前缀，下游按它 + "Xxx" 注册回调接手结果。
	Callback string
	// Currency 是调用方漏填币种时补的缺省值，避免发出一张「币种为空」的单。
	Currency string
	// Kind 是本通道在 pay 分派表里的种类。装配成功即登记、停服即摘掉：
	// 「这条通道起来了」与「分派找得到它」必须是同一件事，中间隔一段没登记的窗口期，
	// 上游那一单就会撞上「未装配」——而它其实只是晚了一拍。
	Kind pay.Kind
	// NewClient 是各链自己的装配入口：读哪一段配置、哪个字段先过校验、
	// 失败返回 ok=false。资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
	NewClient func(*config.Cfg) (*Snapshot, bool)

	// active 为 nil 表示链路未启动，对应操作会就地拒绝而不是发出请求。
	active atomic.Pointer[Snapshot]
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	runCtxRef atomic.Pointer[context.Context]
}

// SetRunCtx 设置这条链路的运行上下文；nil 归一为 context.Background()。
func (r *Route) SetRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := ctx
	r.runCtxRef.Store(&c)
}

// RunCtx 取运行上下文；从未设过则回落 context.Background()。
func (r *Route) RunCtx() context.Context {
	if p := r.runCtxRef.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// Start 按配置装配这一侧的资金链路。
//
// 配置段缺失、通道没开、密钥未注入，都只记日志并返回：资金通道没配好不该阻断
// 整机启动，但必须留下可查的痕迹。重复 Start 时旧快照在新快照上位并被分派表接管
// 之后再关——只清指针不关连接的话，停启几次就把进程挂在泄漏的 socket 上。
func (r *Route) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.SetRunCtx(ctx)
	c, ok := r.NewClient(corectx.CfgFrom(ctx))
	if !ok {
		vars.Info("不启动%s", r.Label)
		return
	}
	old := r.active.Swap(c)
	r.bind(c)
	if old != nil {
		if err := old.Ch.Close(); err != nil {
			vars.Info("%s 通道重启时释放旧连接失败: %v", r.Label, err)
		}
	}
}

// bind 把这条已装配的通道登记进 pay 的分派表。
//
// 重复登记时先 Unbind 再 Bind：Start 可以被调用两次（管线的重启路径、测试里的交叉
// 用例），而 active 指针已经被新快照换掉——分派表若还指着上一条，出款会走一条已经
// Close 的通道。换实现要显式，所以这里是「摘掉再加」而不是放宽 pay.Bind 的重复检查。
func (r *Route) bind(c *Snapshot) {
	if r.Kind == "" || c == nil {
		return
	}
	pay.Unbind(r.Kind)
	if err := pay.Bind(r.Kind, c.Ch); err != nil {
		vars.Info("%s 通道登记分派失败: %v", r.Label, err)
	}
}

// Stop 摘掉客户端，并释放它持有的连接；可重复调用。
//
// 先摘分派再关连接：反过来的话，正在分派的那一单会拿到一条正在关的通道，
// 报出来的是「连接已关闭」而不是「通道未启动」。
func (r *Route) Stop() {
	if r.Kind != "" {
		pay.Unbind(r.Kind)
	}
	c := r.active.Swap(nil)
	if c == nil {
		return
	}
	if err := c.Ch.Close(); err != nil {
		vars.Info("%s 通道停服时释放连接失败: %v", r.Label, err)
	}
}

// currentClient 取资金链路；未启动时返回明确错误而不是 panic。
func (r *Route) currentClient() (*Snapshot, error) {
	if c := r.active.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New(r.Label + " 通道未启动（检查 " + r.ConfigKey + " 的 " + r.NotStartedHint + "）")
}

// publish 把归一后的资金回执广播给下游，键为 Callback+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func (r *Route) publish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(r.Callback+kind, res); !ok {
		vars.Debug("%s 回执无人订阅: kind=%s order_no=%s", r.Label, kind, res.OrderNo)
	}
}

// orderCall 是下单 / 查单门面共用的收尾：补通道前缀 → 广播回执。
//
// ctx 为 nil 时回落到该通道的运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本通道的留痕口径。
func (r *Route) orderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = r.RunCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s %s 失败: %w", r.Label, kind, err)
	}
	r.publish(kind, res)
	return res, nil
}

// order 把链路配置补进订单里未指定的字段，缺省币种取 Route 而不是写死在订单构造里：
// 同一条链上「这个通道发什么币」是装配期的事实，调用方漏填时不该由下游猜。
//
// 网络补在 To.Network 上而不是订单顶层：网络是收款端那一侧的属性，「这笔该落到哪张网」
// 跟收款人一起才有意义。补的方式是深拷贝 To 后回填，不就地改调用方对象——调用方交来的
// PayOrder 可能还要用于重试与落库，门面把它改了就等于替上游决定了「这一单实际发出去的
// 是什么」。
//
// From 不在这里填：它的缺省值是通道的热钱包，而热钱包住在链上通道里而不是这个快照里；
// 空 From 由链上通道按「用配置默认出款方」解。
func (r *Route) order(c *Snapshot, o *pay.PayOrder) *pay.PayOrder {
	if o == nil {
		return nil
	}
	network := ""
	if o.To != nil {
		network = strings.TrimSpace(o.To.Network)
	}
	if network == "" {
		network = c.Network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = r.Currency
	}
	if o.To == nil {
		// To 为空时交给 pay.CheckOrder 拒单，不在这里凭空造一个收款方。
		if currency == o.Currency {
			return o
		}
		cp := *o
		cp.Currency = currency
		return &cp
	}
	if network == o.To.Network && currency == o.Currency {
		return o
	}
	cp := *o
	cp.Currency = currency
	to := *o.To
	to.Network = network
	cp.To = &to
	return &cp
}

// Withdraw 直接上链出款：补全订单缺省字段后交给通道，收尾广播回执。
//
// order.OrderNo 是幂等键，但保证它的是各链的出款台账（touchgocore/ledger），
// 不是本门面：同一单号永不签第二笔，广播结果说不清时回 UNKNOWN（带 TxHash 供对账）。
func (r *Route) Withdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	return r.orderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return c.Ch.Withdraw(ctx, r.order(c, order))
	})
}

// Query 按订单号查链上处置结果。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func (r *Route) Query(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New(r.Label + " Query 失败: 订单号（幂等键）为空")
	}
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	return r.orderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return c.Ch.QueryOrder(ctx, orderNo)
	})
}

// Account 查账户余额（各链口径不同：tron 读供应商额度，ton/bsc/sol 读链上余额）。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能发这么多），而真正的资金闸
// 在出款前置里。漏填币种时补通道缺省币种；ctx 为 nil 时回落运行上下文。
func (r *Route) Account(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if strings.TrimSpace(q.Currency) == "" {
		cp := *q
		cp.Currency = r.Currency
		q = &cp
	}
	if ctx == nil {
		ctx = r.RunCtx()
	}
	info, err := c.Ch.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s 账户查询失败: %w", r.Label, err)
	}
	return info, nil
}
