// Package whatsapp 是 WhatsApp 通道的登录、充值与提现接入点。
//
// 分层口径与 telegram 包一致：本包只负责把请求送到供应商、把回执读成
// pay.PayResult，不碰 SQL、不碰业务积分。落库、额度与风控由下游业务工程负责，
// 它可以在启动前后按 util.CallWhatsappMsg+"Xxx" 注册回调接手结果。
//
// 本包是「接入点」而非「已完成对接」：供应商真实接口文档到位后，需要改的只有
// pay_sdks 里的 endpoints 路径、client.go 的签名域字段顺序、以及 auth.go/fund.go
// 里那几个报 Struct 的字段名。生命周期、门面函数签名与重试语义都不动。
//
// 与另两条资金通道相比，本包多一条登录链路：验证码网关与支付网关常常不是同一家，
// 所以 whatsapp.login 与 whatsapp.provider 各自引用 pay_sdks 的一段。
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

var (
	// 只读快照：WhatsappStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	whatsappRunCtx atomic.Pointer[context.Context]
	// fund 与 login 是两条独立的供应商链路：验证码网关与支付网关常常不是同一家。
	// nil 表示该链路未启动，对应操作会就地拒绝而不是发出请求。
	fund  atomic.Pointer[pay.Provider]
	login atomic.Pointer[pay.Provider]
)

func init() {
	setRunCtx(context.Background())
}

func setRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := ctx
	whatsappRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := whatsappRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

func whatsappCfg() *config.WhatsappConfig {
	cfg := corectx.CfgFrom(runCtx())
	if cfg == nil {
		return nil
	}
	return cfg.Whatsapp
}

// WhatsappStart 按配置装配两条供应商链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
// 两条链路各起各的——登录网关与支付网关常常不是同一家，一边配好一边没配是常态。
func WhatsappStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	cfg := whatsappCfg()
	if cfg == nil {
		vars.Info("不启动Whatsapp")
		return
	}
	all := corectx.CfgFrom(runCtx())
	if p, ok := openFund(all, cfg.Provider); ok {
		fund.Store(p)
	}
	if p, ok := openLogin(all, cfg.Login); ok {
		login.Store(p)
	}
	if fund.Load() == nil && login.Load() == nil {
		vars.Info("不启动Whatsapp")
	}
}

// WhatsappStop 摘掉供应商客户端。本包无常驻协程，因此清空指针即可；可重复调用。
func WhatsappStop(ctx context.Context) {
	fund.Store(nil)
	login.Store(nil)
}

// currentFund 取资金链路客户端；未启动时返回明确错误而不是 panic。
func currentFund() (*pay.Provider, error) {
	p := fund.Load()
	if p == nil {
		return nil, errors.New("whatsapp 资金通道未启动（检查 whatsapp.provider 是否引用了已开启的 pay_sdks 段与商户账户）")
	}
	syncSession(p)
	return p, nil
}

// syncSession 把登录链路拿到的会话凭证带进资金链路。
//
// 只有两条链路指向同一供应商基址时才带：基址不同还把 token 送出去，
// 等于把会话凭证交给另一家服务商。两段配置指向同一供应商是最常见的形态
// （同一家的登录网关与支付网关），此时构造出的是两个 Provider 对象，
// 凭证不同步就会出现「登录成功但每一笔下单都 401」。
func syncSession(fundP *pay.Provider) {
	loginP := login.Load()
	if loginP == nil || loginP == fundP || loginP.BaseURL() != fundP.BaseURL() {
		return
	}
	fundP.SetToken(loginP.Token())
}

// currentLogin 取登录链路客户端。
func currentLogin() (*pay.Provider, error) {
	if p := login.Load(); p != nil {
		return p, nil
	}
	return nil, errors.New("whatsapp 登录通道未启动（检查 whatsapp.login 是否引用了已开启的 pay_sdks 段）")
}

// publish 把归一后的资金回执广播给下游，键为 util.CallWhatsappMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func publish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallWhatsappMsg+kind, res); !ok {
		vars.Debug("whatsapp 回执无人订阅: kind=%s order_no=%s", kind, res.OrderNo)
	}
}

// orderCall 是三个下单/查单门面共用的收尾：调用→补通道前缀→广播回执。
//
// ctx 为 nil 时回落到包级运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本包的留痕口径。
func orderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp %s 失败: %w", kind, err)
	}
	publish(kind, res)
	return res, nil
}
