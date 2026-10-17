// Package instagram 是 Instagram 通道的登录链路接入点：一次性凭证下发、换会话、按账号查关联账号列表。
//
// 分层口径与 telegram 包一致：本包只负责把请求送到供应商、把回执读成本包的结构，
// 不碰 SQL、不碰业务积分。落库、额度与风控由下游业务工程负责。
//
// 提现/查单/账户三条资金门面的实现已在 2026-10 的资金面收口里删掉（充值/收款接口
// 整条删除，不再有门面）：全仓真正会动钱的出款路径只剩 USDT-TRC20 一条（在 tron 包，
// 本地签名后直接上链）。签名保留、一律返回 ErrPayoutNotWired，理由同 bsc：
// 删掉函数会让上游当场编译失败，留着哨兵更好收口。
// publish 回执广播随资金链路一起删除，本包不再按 util.CallInstagramMsg+Kind 广播任何结果。
//
// 本包是「接入点」而非「已完成对接」：供应商真实接口文档到位后，需要改的只有
// pay_sdks 里的 endpoints 路径、client.go 的签名域字段顺序、以及 auth.go 里那几个
// 报 Struct 的字段名。生命周期与登录侧门面签名都不动。
//
// 配置段的 instagram.provider 引用不再被装配（资金段），本包只接 instagram.login 一段。
//
// 下面三条是本包特有的口径，与 config.InstagramConfig、util.CallInstagramMsg 的
// 注释同源，改这里要同步改那两处：
//
//   - Instagram 同属 Meta，真实世界走 OAuth 跳转授权，但本包登录的是供应商代持账号，
//     所以登录链路仍然只有「下发一次性凭证、拿凭证换会话」两步，门面签名与 whatsapp
//     完全同构。loginRequest.Code 是供应商侧一次性凭证——短信验证码、OAuth 授权码、
//     或回调 ticket，以供应商在 pay_sdks 的登记口径为准，本包不猜。换成 OAuth 形态，
//     改的是 SDK 段与供应商适配代码，不是这里的一个字段。
//   - 与 facebook 拆成两个独立配置段而不合并：两条通道的账号、商户账户与开关各自
//     独立，合并就等于让「只关 Instagram」这件事没法表达。
//   - Instagram 没有「好友」概念，但逻辑名仍沿用 pay.EndpointFriendList、门面仍叫
//     InstagramFriends，语义读作「按账号返回的关联账号列表」：四条社交通道各起一个
//     逻辑名的话，端点表就散成四个名字，而取路径那侧只按逻辑名查，多一个名字就是
//     多一处配漏了静默失败的地方。
package instagram

import (
	"context"
	"errors"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/vars"
)

var (
	// 只读快照：InstagramStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	instagramRunCtx atomic.Pointer[context.Context]
	// login 是验证码下发与登录链路；nil 表示该链路未启动，对应操作就地拒绝而不是发出请求。
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
	instagramRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := instagramRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

func instagramCfg() *config.InstagramConfig {
	cfg := corectx.CfgFrom(runCtx())
	if cfg == nil {
		return nil
	}
	return cfg.Instagram
}

// InstagramStart 按配置装配登录链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 一条链路没配好不该阻断整机启动，但必须留下可查的痕迹。
func InstagramStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	cfg := instagramCfg()
	if cfg == nil {
		vars.Info("不启动Instagram")
		return
	}
	if p, ok := openLogin(corectx.CfgFrom(runCtx()), cfg.Login); ok {
		login.Store(p)
	}
	if login.Load() == nil {
		vars.Info("不启动Instagram")
	}
}

// InstagramStop 摘掉供应商客户端。本包无常驻协程，因此清空指针即可；可重复调用。
func InstagramStop(ctx context.Context) {
	login.Store(nil)
}

// currentLogin 取登录链路客户端。
func currentLogin() (*pay.Provider, error) {
	if p := login.Load(); p != nil {
		return p, nil
	}
	return nil, errors.New("instagram 登录通道未启动（检查 instagram.login 是否引用了已开启的 pay_sdks 段）")
}

// ErrPayoutNotWired 本通道没有接入出款链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条通道」（部署缺陷，
// 该修配置或补实现）与「供应商拒了这一单」（业务结果，该走状态机）。
// 把这三种情形都返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("Instagram(USDT) 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// InstagramWithdraw 未接入：保留签名，不假装受理。
func InstagramWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// InstagramQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func InstagramQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// InstagramAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func InstagramAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
