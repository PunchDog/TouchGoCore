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
	// kind 是本通道在 pay 分派表里的种类。装配成功即登记、停服即摘掉：
	// 「这条通道起来了」与「分派找得到它」必须是同一件事，中间隔一段没登记的窗口期，
	// 上游那一单就会撞上「未装配」——而它其实只是晚了一拍。
	kind pay.Kind

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
	ch      chainChannel
	network string
}

// chainChannel 是本包对「一条链上资金通道」的全部要求。
//
// 它曾经是 pay.Channel——住在契约层、被三家通道共用。那张接口的 Driver/MerchantID
// 两个方法只服务于驱动注册表：表没了，它们就只剩「接口上多两个没人调的方法」。
// 现在接口收在这里，宽度正好是本包门面真正要调的四件事；哪一天要加第五件，
// 得先想清楚它是不是真属于「通道」，而不是随手挂上去。
type chainChannel interface {
	Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error)
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
	QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error)
	Close() error
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
		r.bind(c)
		return
	}
	vars.Info("不启动%s", r.label)
}

// bind 把这条已装配的通道登记进 pay 的分派表。
//
// 重复登记时先 Unbind 再 Bind：Start 可以被调用两次（管线的重启路径、测试里的交叉用例），
// 而 active 指针已经被新快照换掉——分派表若还指着上一条，出款会走一条已经 Close 的通道。
// 换实现要显式，所以这里是「摘掉再加」而不是放宽 pay.Bind 的重复检查。
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
// 链上通道背后是一条 gRPC 连接：只清空指针的话，第二次 start 就多一条不会关的连接，
// 停启几次就把进程挂在泄漏的 socket 上。Close 已经收进 chainChannel，所以这里直接调，
// 不再需要「按可选能力断言」那一层——接口里没 Close 的实现根本进不来。
func (r *route) stop() {
	// 先摘分派再关连接：反过来的话，正在分派的那一单会拿到一条正在关的通道，
	// 报出来的是「连接已关闭」而不是「通道未启动」。
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
	return nil, errors.New(r.label + " 通道未启动（检查 tron." + r.channel + " 的 enable / chain / 商户账户是否齐备）")
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
// 网络补在 To.Network 上而不是订单顶层：网络是收款端那一侧的属性，「这笔该落到哪张网」
// 跟收款人一起才有意义。补的方式是深拷贝 To 后回填，不就地改调用方对象。
//
// From 不在这里填：它的缺省值是通道的热钱包，而热钱包住在链上通道里而不是这个快照里；
// 空 From 由链上通道按「用配置默认出款方」解。
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
	// 复制而不是就地改：调用方交来的 PayOrder 可能还要用于重试与落库，
	// 门面把它改了就等于替上游决定了「这一单实际发出去的是什么」。
	cp := *o
	cp.Currency = currency
	to := *o.To
	to.Network = network
	cp.To = &to
	return &cp
}
