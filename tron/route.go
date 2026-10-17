package tron

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

// 本文件是 TRON 侧门面的共用机器：「快照客户端 → 调通道 → 广播回执」。
// 一条链两个币种曾各持一个 route 实例（TRX 与 USDT），差别只剩日志口径、pay 通道名、
// 回执前缀与缺省币种四个常量；TRX 的出款链路已删（见 tron.go 的哨兵），机器留着
// 因为 USDT 侧的装配与广播全靠它。
// 分成两个包各写一份有过实证代价：ChainRule 曾在 tron 与 usdt 各登记一次，
// 下游 main 同时 blank-import 两个包就在启动期 panic（见 chainrule.go）。

// route 是一条通道门面的全部可变状态与差异点。
type route struct {
	// label 出现在日志、错误前缀与回执文案里，必须与合并前两侧各自的写法逐字一致。
	label string
	// channel 是 pay 装配层的通道名，同时决定未启动报错里指向的配置键（usdt.provider）。
	channel string
	// callback 是回执广播的 util 前缀，下游按它 + "Xxx" 注册回调接手结果。
	callback string
	// currency 是调用方漏填币种时补的缺省值，避免发出一张「币种为空」的单。
	currency string
	// newClient 是各侧自己的装配差异：读哪一段配置、哪个字段先过校验、extras 怎么排。
	newClient func(*config.Cfg) (*client, bool)

	// active 为 nil 表示链路未启动，对应操作会就地拒绝而不是发出请求。
	active atomic.Pointer[client]
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	runCtxRef atomic.Pointer[context.Context]
}

// client 把资金通道与「这条链路属于哪条网络」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换 network，会出现用新网关发旧链的单子。
type client struct {
	ch      pay.Channel
	network string
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	usdtRoute.setRunCtx(context.Background())
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
// 配置段缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
func (r *route) start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.setRunCtx(ctx)
	if c, ok := r.newClient(corectx.CfgFrom(ctx)); ok {
		r.active.Store(c)
		return
	}
	vars.Info("不启动%s", r.label)
}

// stop 摘掉客户端，并释放它持有的连接。
//
// 链上通道背后是一条 gRPC 连接：只清空指针的话，第二次 start 就多一条不会关的连接，
// 停启几次就把进程挂在泄漏的 socket 上。供应商侧的 pay.Provider 没有需要释放的东西，
// 所以走接口断言而不是按类型开关——将来再加通道也不必改这里。
func (r *route) stop() {
	c := r.active.Swap(nil)
	if c == nil {
		return
	}
	if cl, ok := c.ch.(interface{ Close() error }); ok {
		if err := cl.Close(); err != nil {
			vars.Info("%s 通道停服时释放连接失败: %v", r.label, err)
		}
	}
}

// currentClient 取资金链路；未启动时返回明确错误而不是 panic。
func (r *route) currentClient() (*client, error) {
	if c := r.active.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New(r.label + " 通道未启动（检查 " + r.channel +
		".provider 是否引用了已开启的 pay_sdks 段与商户账户）")
}

// publish 把归一后的资金回执广播给下游，键为 callback+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func (r *route) publish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(r.callback+kind, res); !ok {
		vars.Debug("%s 回执无人订阅: kind=%s order_no=%s", r.label, kind, res.OrderNo)
	}
}

// orderCall 是三个下单/查单门面共用的收尾：补通道前缀 → 广播回执。
//
// ctx 为 nil 时回落到该通道的运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本通道的留痕口径。
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

// order 把链路配置补进订单里未指定的字段，缺省币种取 route 而不是写死在订单构造里：
// 同一条链上「这个通道发什么币」是装配期的事实，调用方漏填时不该由下游猜。
//
// 每个通道实例各跑一次 start，两条链路的快照互不串用（见 route.active）。
func (r *route) order(c *client, o *pay.PayOrder) *pay.PayOrder {
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = r.currency
	}
	if network == o.Network && currency == o.Currency {
		return o
	}
	// 复制而不是就地改：调用方交来的 PayOrder 可能还要用于重试与落库，
	// 门面把它改了就等于替上游决定了「这一单实际发出去的是什么」。
	cp := *o
	cp.Network, cp.Currency = network, currency
	return &cp
}
