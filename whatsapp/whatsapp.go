// Package whatsapp 是 WhatsApp 通道的消息与资金接入点。
//
// 分层口径与 telegram 包一致：本包只负责「把 WhatsApp 账号跑起来并收发消息」和
// 「把资金请求送到供应商、把回执读成 pay.PayResult」，不碰业务积分与额度账本。
// 落库、风控由下游业务工程负责，它可以在启动前后按 util.CallWhatsappMsg+"Xxx"
// 注册回调接手结果（资金回执 kind=Recharge/Withdraw/Query；消息事件
// kind=Inbound/Receipt/Connection/QR，载荷形态见 wa_client.go 与 wa_pairing.go）。
//
// 两条链路用的是两套传输，别混：
//   - 消息侧走 go.mau.fi/whatsmeow 直连 WhatsApp 多设备协议（wa_client.go 门面 +
//     wa_manager.go 会话管理 + wa_pairing.go 扫码配对 + store_postgres.go 存储）：
//     QR 配对、发文本、删会话框都在这里；会话密钥材料持久化在**本仓的 Postgres**
//     会话库里（whatsapp.sessions.dsn），多账号一个 Client 一条常驻协程。
//     非官方客户端有封号风险，责任与风控归业务方。
//   - 资金侧走 pay_sdks 供应商链路（fund.go），仍是 HTTP + MD5 签名 + 商户账户那一套。
//     提现这类动作必须有持牌主体出账，而 WhatsApp 协议里没有任何可发起转账的 API
//     （库里支付消息只是 unsupported 占位），所以资金动作不挂在消息侧那一支上。
//
// **本包有常驻协程**：每个已启动账号一条连接守护（wa_manager.go 的 run），
// 每次进行中的配对一条泵协程（wa_pairing.go 的 runPairing）。停机由 WhatsappStop
// 负责收掉，单协程等待上限 1.5s，压在框架 2s 服务停机预算（app.go minServiceStopBudget）之内。
package whatsapp

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

var (
	// 只读快照：WhatsappStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	whatsappRunCtx atomic.Pointer[context.Context]
	// fund 是资金供应商链路；nil 表示未启动，对应操作会就地拒绝而不是发出请求。
	fund atomic.Pointer[pay.Provider]
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

// WhatsappStart 装配消息侧与资金链路。
//
// 两边的判定各自独立：资金侧引用缺失、SDK 段没开、密钥未注入，都只记日志并返回，
// 不该阻断整机启动；消息侧只看 whatsapp.sessions.dsn 配没配、Postgres 连不连得上，
// 连不上同样只记日志——「只跑钱、不跑协议」是常见形态，反过来「只跑协议」也是。
func WhatsappStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	all := allCfg()
	startWA(all)
	if cfg := whatsappCfg(); cfg != nil {
		if p, ok := openFund(all, cfg.Provider); ok {
			fund.Store(p)
		}
	}
	if fund.Load() == nil && waMgr.Load() == nil {
		vars.Info("不启动Whatsapp")
	}
}

// WhatsappStop 停掉消息侧全部常驻协程、关会话库、摘资金客户端。可重复调用。
func WhatsappStop(ctx context.Context) {
	if mgr := waMgr.Swap(nil); mgr != nil {
		mgr.StopAll(waStopWaitBudget)
	}
	if s := waStore.Swap(nil); s != nil {
		if err := s.Close(); err != nil {
			vars.Warning("关闭 whatsapp 会话库失败: %v", err)
		}
	}
	fund.Store(nil)
}

// startWA 装配消息侧：开 Postgres 会话库 → 建管理器 → 注册 QR 端点。
//
// 已有设备**只装载不自动连**（用户裁决「配对后需业务方显式启动」）：
// 启动即恢复全部账号会让「重启一次服务」变成「N 个账号同时向 WhatsApp 重连」，
// 既绕过了显式启动的裁决，也是封号检测最敏感的形态。代价是重启后业务方要自己
// 把该上线的账号逐个 WhatsappStartClient——启动日志里把库里设备数报出来，方便对账。
//
// 没配 DSN 或库连不上只记日志返回，不阻断整机（资金侧照常装配）。
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

// currentFund 取资金链路客户端；未启动时返回明确错误而不是 panic。
func currentFund() (*pay.Provider, error) {
	p := fund.Load()
	if p == nil {
		return nil, errors.New("whatsapp 资金通道未启动（检查 whatsapp.provider 是否引用了已开启的 pay_sdks 段与商户账户）")
	}
	return p, nil
}

// publish 把归一后的资金回执广播给下游，键为 util.CallWhatsappMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。消息侧的事件广播在 wa_client.go 的 publishWA。
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
