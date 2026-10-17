// Package facebook 是 Facebook 通道的登录链路接入点：一次性凭证下发、换会话、按账号查好友列表。
//
// 分层口径与 telegram 包一致：本包只负责把请求送到供应商、把回执读成本包的结构，
// 不碰 SQL、不碰业务积分。落库、额度与风控由下游业务工程负责。
//
// 提现/查单/账户三条资金门面的实现已在 2026-10 的资金面收口里删掉（充值/收款接口
// 整条删除，不再有门面）：全仓真正会动钱的出款路径只剩 USDT-TRC20 一条（在 tron 包，
// 本地签名后直接上链）。签名保留、一律返回 ErrPayoutNotWired，理由同 bsc：
// 删掉函数会让上游当场编译失败，留着哨兵更好收口。
// publish 回执广播随资金链路一起删除，本包不再按 util.CallFacebookMsg+Kind 广播任何结果。
//
// 本包是「接入点」而非「已完成对接」：供应商真实接口文档到位后，需要改的只有
// pay_sdks 里的 endpoints 路径、client.go 的签名域字段顺序、以及 auth.go 里那几个
// 报 Struct 的字段名。生命周期与登录侧门面签名都不动。
//
// 配置段的 facebook.provider 引用不再被装配（资金段），本包只接 facebook.login 一段。
//
// 本包特有口径：真实世界 Facebook 走 OAuth 跳转授权，但本包登录的是供应商代持账号，
// 登录链路仍然只有「下发一次性凭证、拿凭证换会话」两步。门面签名因此与 whatsapp 同构
// （FacebookSendCode + FacebookLogin）；code 到底是短信验证码、OAuth 授权码还是回调
// ticket，由供应商在 pay_sdks 里的登记口径决定，本包不猜——换成 OAuth 形态时改的是
// SDK 段与供应商适配代码，不是这里的函数签名。供应商压根没有「下发验证码」这一环时，
// 处置是在该 SDK 段的 endpoints 里不登记 send_code：缺键的操作就地拒绝、不发请求，
// 而不是删掉 FacebookSendCode 这个门面（少一个签名是破坏性变更，少一个端点是配置）。
package facebook

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
	// 只读快照：FacebookStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	facebookRunCtx atomic.Pointer[context.Context]
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
	facebookRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := facebookRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

func facebookCfg() *config.FacebookConfig {
	cfg := corectx.CfgFrom(runCtx())
	if cfg == nil {
		return nil
	}
	return cfg.Facebook
}

// FacebookStart 按配置装配登录链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 一条链路没配好不该阻断整机启动，但必须留下可查的痕迹。
func FacebookStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	cfg := facebookCfg()
	if cfg == nil {
		vars.Info("不启动Facebook")
		return
	}
	if p, ok := openLogin(corectx.CfgFrom(runCtx()), cfg.Login); ok {
		login.Store(p)
	}
	if login.Load() == nil {
		vars.Info("不启动Facebook")
	}
}

// FacebookStop 摘掉供应商客户端。本包无常驻协程，因此清空指针即可；可重复调用。
func FacebookStop(ctx context.Context) {
	login.Store(nil)
}

// currentLogin 取登录链路客户端。
func currentLogin() (*pay.Provider, error) {
	if p := login.Load(); p != nil {
		return p, nil
	}
	return nil, errors.New("facebook 登录通道未启动（检查 facebook.login 是否引用了已开启的 pay_sdks 段）")
}

// ErrPayoutNotWired 本通道没有接入出款链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条通道」（部署缺陷，
// 该修配置或补实现）与「供应商拒了这一单」（业务结果，该走状态机）。
// 把这三种情形都返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("Facebook(USDT) 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// FacebookWithdraw 未接入：保留签名，不假装受理。
func FacebookWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// FacebookQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func FacebookQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// FacebookAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func FacebookAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
