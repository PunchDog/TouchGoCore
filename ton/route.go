package ton

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

// 本文件是 TON 侧门面的共用机器：「快照客户端 → 调通道 → 广播回执」。
// 结构照 tron/route.go 抄，差异是 TON 目前只有原生币一条通道；机器仍然独立成
// route，而不是再写一份包级 tonActive + bindTransfer：Start 可被调用两次，
// 旧快照必须在新快照上位后被关掉（见 start）。

// route 是一条通道门面的全部可变状态与差异点。
type route struct {
	// label 出现在日志、错误前缀与回执文案里。
	label string
	// channel 是配置子段名（native），未启动报错里指向 ton.native。
	channel string
	// callback 是回执广播的 util 前缀，下游按它 + "Xxx" 注册回调接手结果。
	callback string
	// currency 是调用方漏填币种时补的缺省值。
	currency string
	// newClient 是装配入口：读自己的配置段、失败返回 ok=false。
	newClient func(*config.Cfg) (*client, bool)
	// kind 是本通道在 pay 分派表里的种类。装配成功即登记、停服即摘掉。
	kind pay.Kind

	// active 为 nil 表示链路未启动，对应操作会就地拒绝而不是发出请求。
	active atomic.Pointer[client]
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致。
	// TON 现在有自己的运行上下文，不再复用 telegramRunCtx（Bot 与资金链路已拆开）。
	runCtxRef atomic.Pointer[context.Context]
}

// client 把资金通道与「这条链路属于哪条网络」绑成一个不可拆的快照。
type client struct {
	ch      chainChannel
	network string
}

// chainChannel 是本包对「一条链上资金通道」的全部要求。
//
// 与 pay.Channel 保持同一宽度：出款、查单、供应商账户、链上余额。
// Close 不在 pay.Channel 里，但门面停服要释放接入点，所以收在这张窄接口上。
type chainChannel interface {
	Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error)
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
	QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error)
	Close() error
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	tonNativeRoute.setRunCtx(context.Background())
}

func (r *route) setRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := ctx
	r.runCtxRef.Store(&c)
}

func (r *route) runCtx() context.Context {
	if p := r.runCtxRef.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// start 按配置装配这一侧的资金链路。
//
// 配置段缺失、通道没开、密钥未注入，都只记日志并返回：资金通道没配好不该阻断
// 整机启动，但必须留下可查的痕迹。重复 Start 时旧 client 在新 client 上位并被
// 分派表接管之后再关，避免「分派还指着已关闭的通道」。
func (r *route) start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.setRunCtx(ctx)
	c, ok := r.newClient(corectx.CfgFrom(ctx))
	if !ok {
		vars.Info("不启动%s", r.label)
		return
	}
	old := r.active.Swap(c)
	r.bind(c)
	if old != nil {
		if err := old.ch.Close(); err != nil {
			vars.Info("%s 通道重启时释放旧连接失败: %v", r.label, err)
		}
	}
}

// bind 把这条已装配的通道登记进 pay 的分派表。
//
// 重复登记时先 Unbind 再 Bind：Start 可以被调用两次（重启路径、测试里的交叉用例），
// 而 active 指针已经被新快照换掉——分派表若还指着上一条，出款会走一条已经 Close 的通道。
func (r *route) bind(c *client) {
	if r.kind == "" || c == nil {
		return
	}
	pay.Unbind(r.kind)
	if err := pay.Bind(r.kind, c.ch); err != nil {
		vars.Info("%s 通道登记分派失败: %v", r.label, err)
	}
}

// stop 摘掉客户端，并释放它持有的连接。
//
// 先摘分派再关连接：反过来的话，正在分派的那一单会拿到一条正在关的通道，
// 报出来的是「连接已关闭」而不是「通道未启动」。
func (r *route) stop() {
	if r.kind != "" {
		pay.Unbind(r.kind)
	}
	c := r.active.Swap(nil)
	if c == nil {
		return
	}
	if err := c.ch.Close(); err != nil {
		vars.Info("%s 通道停服时释放连接失败: %v", r.label, err)
	}
}

// currentClient 取资金链路；未启动时返回明确错误而不是 panic。
func (r *route) currentClient() (*client, error) {
	if c := r.active.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New(r.label + " 通道未启动（检查 ton." + r.channel + " 的 enable / chain 与热钱包是否齐备）")
}

// publish 把归一后的资金回执广播给下游，键为 callback+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
func (r *route) publish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(r.callback+kind, res); !ok {
		vars.Debug("%s 回执无人订阅: kind=%s order_no=%s", r.label, kind, res.OrderNo)
	}
}

// orderCall 是下单/查单门面共用的收尾：补通道前缀 → 广播回执。
func (r *route) orderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = r.runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s %s 失败: %w", r.label, kind, err)
	}
	r.publish(kind, res)
	return res, nil
}

// order 把链路配置补进订单里未指定的字段。
//
// 网络补在 To.Network 上而不是订单顶层：网络是收款端那一侧的属性。
// 复制（含 To 深拷贝）而不是就地改：调用方交来的 PayOrder 可能还要用于重试与落库。
func (r *route) order(c *client, o *pay.PayOrder) *pay.PayOrder {
	if o == nil {
		return nil
	}
	network := ""
	if o.To != nil {
		network = strings.TrimSpace(o.To.Network)
	}
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = r.currency
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
