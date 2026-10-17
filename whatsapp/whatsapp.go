// Package whatsapp 是 WhatsApp 通道的消息接入点。
//
// 分层口径与 telegram 包一致：本包只负责「把 WhatsApp 账号跑起来并收发消息」，
// 不碰业务积分与额度账本。落库、风控由下游业务工程负责，它可以在启动前后按
// util.CallWhatsappMsg+"Xxx" 注册回调接手结果（消息事件 kind=Inbound/Receipt/
// Connection/QR/SendResult，载荷形态见 wa_client.go 与 wa_pairing.go）。
//
// 整通道由 whatsapp.enable 总闸控制：值不为 "on"（或压根没有 whatsapp 段）时
// WhatsappStart 不装配消息侧，本包所有门面调用就地拒绝。
//
// 消息侧走 go.mau.fi/whatsmeow 直连 WhatsApp 多设备协议（wa_client.go 门面 +
// wa_manager.go 会话管理 + wa_pairing.go 扫码配对 + wa_delete_device.go 吊销配对 +
// store_postgres.go 存储）：
// QR 配对、发文本、删会话框、吊销设备配对都在这里；会话密钥材料持久化在
// **本仓的 Postgres** 会话库里（whatsapp.sessions.dsn），多账号一个 Client 一条常驻协程。
// 非官方客户端有封号风险，责任与风控归业务方。
//
// 资金侧那套 HTTP + MD5 签名 + 商户账户的供应商装配（原 client.go 与 fund.go）已在
// 2026-10 的资金面收口里删掉：全仓真正会动钱的出款路径只剩 USDT-TRC20 一条
// （在 tron 包，本地签名后直接上链）。三个资金门面保留签名并返回 ErrPayoutNotWired，
// 理由与其余通道相同：它们是包外可见的调用面，留函数比删函数让上游当场编译失败
// 更容易收口。这条通道本来也不该自己动钱——WhatsApp 协议里没有任何可发起转账的
// API（库里支付消息只是 unsupported 占位），提现这类动作必须有持牌主体出账。
//
// util.CallWhatsappMsg 那组前缀还登记在 util 里，但本包只播消息事件（Inbound/Receipt/
// Connection/QR/SendResult）：Withdraw/Query 两个键不再有回执。下游收不到
// 资金回调时该读成「这条通道没接入」，而不是「单还在路上」。
//
// **本包有常驻协程**：每个已启动账号一条连接守护（wa_manager.go 的 run），
// 每次进行中的配对一条泵协程（wa_pairing.go 的 runPairing）。停机由 WhatsappStop
// 负责收掉，单协程等待上限 1.5s，压在框架 2s 服务停机预算（app.go minServiceStopBudget）之内。
package whatsapp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/vars"
)

var (
	// 只读快照：WhatsappStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	whatsappRunCtx atomic.Pointer[context.Context]
	// waStore 单独持一份指针只为停机时关库：管理器也引用它，但关闭动作
	// 必须在「全部协程收掉之后」发生，挂在生命周期上比挂在管理器里清晰。
	waStore atomic.Pointer[WAStore]
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

// ctxOrRun 把门面收到的 nil ctx 折到包级运行上下文：调用方没给 ctx 时，
// 「启动时快照的配置」仍然生效，而不是产生一个永不取消的后台请求。
func ctxOrRun(ctx context.Context) context.Context {
	if ctx == nil {
		return runCtx()
	}
	return ctx
}

// allCfg 取整机配置快照（ws_qr.go 的跨域白名单也从这里读）。
func allCfg() *config.Cfg {
	return corectx.CfgFrom(runCtx())
}

func whatsappCfg() *config.WhatsappConfig {
	cfg := allCfg()
	if cfg == nil {
		return nil
	}
	return cfg.Whatsapp
}

// WhatsappStart 装配消息侧。
//
// 总闸先看 whatsapp.enable：整段没配或值不为 "on"（大小写不敏感，两侧空白忽略）即
// 不装配、直接返回——whatsmeow 是非官方客户端，不该因为「dsn 恰好写了」就连上
// WhatsApp 服务器。闸打开后只看 whatsapp.sessions.dsn 配没配、Postgres 连不连得上，
// 连不上只记日志不阻断整机启动：「这个号不跑协议」是常见形态。
func WhatsappStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	all := allCfg()
	wcfg := whatsappCfg()
	if !wcfg.Enabled() {
		vars.Info("不启动Whatsapp（无 whatsapp 段或 whatsapp.enable 不为 on）")
		return
	}
	startWA(all)
	if waMgr.Load() == nil {
		vars.Info("不启动Whatsapp（enable 为 on 但消息侧没装起来：sessions.dsn 没配或会话库不可用）")
	}
}

// WhatsappStop 停掉消息侧全部常驻协程并关会话库。可重复调用。
func WhatsappStop(ctx context.Context) {
	if mgr := waMgr.Swap(nil); mgr != nil {
		mgr.StopAll(waStopWaitBudget)
	}
	if s := waStore.Swap(nil); s != nil {
		if err := s.Close(); err != nil {
			vars.Warning("关闭 whatsapp 会话库失败: %v", err)
		}
	}
}

// startWA 装配消息侧：开 Postgres 会话库 → 建管理器 → 注册 QR 端点。
//
// 已有设备**只装载不自动连**（用户裁决「配对后需业务方显式启动」）：
// 启动即恢复全部账号会让「重启一次服务」变成「N 个账号同时向 WhatsApp 重连」，
// 既绕过了显式启动的裁决，也是封号检测最敏感的形态。
//
// 这条裁决管的是「开机不批量拉起」，不等于「账号必须有人先点一下才能用」：
// 读写门面（发消息/发群消息/删框/列好友/列群组/批量发信）都按需自动登录并等到连上
// （wa_manager.go 的 ensureLoggedIn），所以冷启动后不必逐个预热；要长期在跑仍显式
// WhatsappStartClient。启动日志把库里设备数报出来，方便对账。
//
// 只在 whatsapp.enable 为 on 时被调用（总闸在 WhatsappStart）。
// 没配 DSN 或库连不上只记日志返回，不阻断整机。
func startWA(all *config.Cfg) {
	var scfg *config.WhatsappSessionsConfig
	if all != nil && all.Whatsapp != nil {
		scfg = all.Whatsapp.Sessions
	}
	if scfg == nil || strings.TrimSpace(scfg.DSN) == "" {
		vars.Info("不启动 WhatsApp 消息侧（未配置 whatsapp.sessions.dsn）")
		return
	}
	st, err := NewWAStore(runCtx(), scfg.DSN, waLogger{module: "store"})
	if err != nil {
		// err 文案已在 NewWAStore 里做过 DSN 脱敏，这里原样落日志。
		vars.Error("WhatsApp 会话库不可用，消息侧不启动: %v", err)
		return
	}
	mgr := newWaManager(st, scfg, waLogger{module: "wa"}, runCtx())
	if devs, derr := st.AllDevices(runCtx()); derr != nil {
		vars.Warning("WhatsApp 会话库枚举设备失败（不阻断启动）: %v", derr)
	} else {
		vars.Info("WhatsApp 会话库就绪: 已配对设备 %d 个（不自动上线，需显式 WhatsappStartClient）", len(devs))
	}
	registerQRRoutes()
	waStore.Store(st)
	waMgr.Store(mgr)
}

// ErrPayoutNotWired 本通道没有接入资金链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条通道」（部署缺陷，
// 该改走 USDT-TRC20 或补实现）与「供应商拒了这一单」（业务结果，该走状态机）。
// 三个门面一律返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("whatsapp 资金通道未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// WhatsappWithdraw 未接入：保留签名，不假装受理。
//
// nil 订单也一样返回哨兵：参数校验拦不住「没接入」这个事实，先报部署缺陷才轮得到报订单。
func WhatsappWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// WhatsappQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func WhatsappQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// WhatsappAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func WhatsappAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
