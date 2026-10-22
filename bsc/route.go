package bsc

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

// 本文件是 BSC 侧门面的共用机器：「快照客户端 → 调通道 → 广播回执」。
//
// 与 tron/route.go 同构，但少一半：TRON 那边一条链上跑两个币种（TRX 与 USDT），
// 差别只剩几个常量，才需要把差异点抽成结构体字段。这里只有 USDT-BEP20 一条通道，
// 保留同样的形状是为了让两条链的门面可以用同一套读法去对照，而不是为了复用。

// route 是一条通道门面的全部可变状态与差异点。
type route struct {
	// label 出现在日志、错误前缀与回执文案里。
	label string
	// callback 是回执广播的 util 前缀，下游按它 + "Xxx" 注册回调接手结果。
	callback string
	// currency 是调用方漏填币种时补的缺省值。
	currency string
	// newClient 是装配入口（读 bsc.usdt 段）。
	newClient func(*config.Cfg) (*client, bool)
	// kind 是本通道在 pay 分派表里的种类。装配成功即登记、停服即摘掉：
	// 「这条通道起来了」与「分派找得到它」必须是同一件事。
	kind pay.Kind

	// active 为 nil 表示链路未启动，对应操作会就地拒绝而不是发出请求。
	active atomic.Pointer[client]
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	runCtxRef atomic.Pointer[context.Context]
}

// client 把资金通道与「这条链路属于哪条网络」绑成一个不可拆的快照。
type client struct {
	ch      chainChannel
	network string
}

// chainChannel 是本包对「一条链上资金通道」的全部要求。
//
// 收成四件事而不是直接持有 *onchainChannel：门面只该看到这四个动作，
// 多一个方法就多一条「从门面绕进实现细节」的路。宽度与 tron 侧逐字一致，
// 两条链的门面因此可以对着读。
type chainChannel interface {
	Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error)
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
	Close() error
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	bscRoute.setRunCtx(context.Background())
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
// 配置段缺失、驱动名不符、合约或链参数不可用、私钥未注入，都只记日志并返回：
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
func (r *route) bind(c *client) {
	if r.kind == "" || c == nil {
		return
	}
	pay.Unbind(r.kind)
	if err := pay.Bind(r.kind, c.ch); err != nil {
		vars.Info("%s 通道登记分派失败: %v", r.label, err)
	}
}

// stop 摘掉客户端，并释放它持有的接入点连接。
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
	return nil, errors.New(r.label + " 通道未启动（检查 bsc.usdt 的 enable / driver / chain / 合约是否齐备）")
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

// orderCall 是三个门面共用的收尾：补通道前缀 → 广播回执。
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
// 网络补在 To.Network 上而不是订单顶层：网络是收款端那一侧的属性。补的方式是
// 深拷贝 To 后回填，不就地改调用方对象——调用方交来的 PayOrder 可能还要用于重试
// 与落库，门面把它改了就等于替上游决定了「这一单实际发出去的是什么」。
//
// From 不在这里填：它的缺省值是通道的热钱包，而热钱包住在链上通道里而不是这个快照里。
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

// bscRoute 是 BSC 侧门面实例：装配读 bsc.usdt，回执走 CallBscMsg，
// 装配成功即登记到 pay 的 bsc.usdt 种类。
var bscRoute = route{
	label:     "BSC-USDT",
	callback:  util.CallBscMsg,
	currency:  pay.CurrencyUSDT,
	newClient: newBscUSDTClient,
	kind:      pay.KindBscUSDT,
}

// BscStart 按配置装配 BSC USDT 资金链路。
func BscStart(ctx context.Context) { bscRoute.start(ctx) }

// BscStop 摘掉客户端并关掉接入点连接；可重复调用。
func BscStop(ctx context.Context) { bscRoute.stop() }
