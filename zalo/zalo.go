// Package zalo 是 Zalo 通道的登录链路接入点：验证码下发、验证码换会话、按账号查好友列表。
//
// 分层口径与 telegram 包一致：本包只负责把请求送到供应商、把回执读成本包的结构，
// 不碰 SQL、不碰业务积分。落库、额度与风控由下游业务工程负责。
//
// 提现/查单/账户三条资金门面的实现已在 2026-10 的资金面收口里删掉（充值/收款接口
// 整条删除，不再有门面）：全仓真正会动钱的出款路径只剩 USDT-TRC20 一条（在 tron 包，
// 本地签名后直接上链）。签名保留、一律返回 ErrPayoutNotWired，理由同 bsc：
// 删掉函数会让上游当场编译失败，留着哨兵更好收口。
// publish 回执广播随资金链路一起删除，本包不再按 util.CallZaloMsg+Kind 广播任何结果。
//
// 本包是「接入点」而非「已完成对接」：供应商真实接口文档到位后，需要改的只有
// pay_sdks 里的 endpoints 路径、client.go 的签名域字段顺序、以及 auth.go 里那几个
// 报 Struct 的字段名。生命周期与登录侧门面签名都不动。
//
// Zalo 是越南的即时通讯 App，我方登录的是供应商代持账号，账号归属与凭证有效期
// 由供应商决定，本包不为此加字段。
//
// 配置段的 zalo.provider 引用不再被装配（资金段），本包只接 zalo.login 一段。
package zalo

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
	// 只读快照：ZaloStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	zaloRunCtx atomic.Pointer[context.Context]
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
	zaloRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := zaloRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

func zaloCfg() *config.ZaloConfig {
	cfg := corectx.CfgFrom(runCtx())
	if cfg == nil {
		return nil
	}
	return cfg.Zalo
}

// ZaloStart 按配置装配登录链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 一条链路没配好不该阻断整机启动，但必须留下可查的痕迹。
func ZaloStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	cfg := zaloCfg()
	if cfg == nil {
		vars.Info("不启动Zalo")
		return
	}
	if p, ok := openLogin(corectx.CfgFrom(runCtx()), cfg.Login); ok {
		login.Store(p)
	}
	if login.Load() == nil {
		vars.Info("不启动Zalo")
	}
}

// ZaloStop 摘掉供应商客户端。本包无常驻协程，因此清空指针即可；可重复调用。
func ZaloStop(ctx context.Context) {
	login.Store(nil)
}

// currentLogin 取登录链路客户端。
func currentLogin() (*pay.Provider, error) {
	if p := login.Load(); p != nil {
		return p, nil
	}
	return nil, errors.New("zalo 登录通道未启动（检查 zalo.login 是否引用了已开启的 pay_sdks 段）")
}

// ErrPayoutNotWired 本通道没有接入出款链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条通道」（部署缺陷，
// 该修配置或补实现）与「供应商拒了这一单」（业务结果，该走状态机）。
// 把这三种情形都返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("Zalo(USDT) 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// ZaloWithdraw 未接入：保留签名，不假装受理。
func ZaloWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// ZaloQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func ZaloQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// ZaloAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func ZaloAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
