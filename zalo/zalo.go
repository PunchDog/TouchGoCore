// Package zalo 是 Zalo 通道：zcago 协议直连（扫码 / cookie 两种登录）+ Postgres 会话库。
//
// 分层口径与 whatsapp 包一致：本包自己持有多个 Zalo 账号的会话，登录材料落 Postgres 的
// `zalo_sessions` 表（`zalo.sessions.dsn`；该键留空时复用 `whatsapp.sessions.dsn`，两者都空
// 则不启动本通道）。扫码配对在本仓（ZaloStartPairing + WebSocket 端点
// GET /zalo/qr/{session_id}），也支持 ZaloLoginWithCookie 导入浏览器 cookie；配对成功
// 不自动上线，发消息/查询按需从库里读凭证登录，也可显式 ZaloStartClient。
//
// 发送只封装文本：zcago v0.3.0 的附件分支直接 panic("not implemented")，图片/文件明确不做。
//
// 限频口径（按 zca-js 社区风控口径取保守值，配置更小会被 Effective* 抬到下限）：
//   - 好友列表 / 群列表：两次拉取间隔 >=5 分钟，且必须走缓存；
//   - 用户资料 / 群资料：两次请求间隔 >=1.5 秒，缓存 TTL 10 分钟；
//   - 发消息：连续两条随机 3–5 秒间隔，另有滚动 1 小时配额。
//
// 风险：zcago 是非官方客户端，Zalo 可能随时风控甚至封号；通道默认 `zalo.enable` 不为 on
// 就不启动，限频与缓存都是封号风险的软闸，不要为了让业务跑快而调小。
//
// 运维口径：
//   - `zalo_sessions.credentials` 是会话凭据等价物（cookie/IMEI/UA），部署侧应收紧库权限、
//     使用独立库/角色或启用落盘加密；`zalo.sessions.dsn` 留空复用 WhatsApp DSN 时两者同库
//     同角色可见，生产应评估是否拆库。
//   - 限频/缓存状态在进程内、按 accountID 记账：多实例或重启会重置；同一 Zalo 账号不要用
//     多个 accountID 导入同一份 cookie，否则会得到多套配额（等于绕过限频）。
//
// 破坏性变更：旧的 HTTP 供应商登录链路（`zalo.login` 配置与 `ZaloSendCode` / `ZaloLogin` /
// `ZaloFriends` 三个门面）已随本次重构删除。旧配置里的 `login`/`templates` 字段只保留为
// 墓碑，`Cfg.Validate()` 发现非空即报错并指引迁移到 `zalo.enable` / `zalo.sessions`。
// 迁移后的登录入口是 ZaloStartPairing（扫码）与 ZaloLoginWithCookie（cookie 导入）。
//
// 资金侧：提现/查单/查账户三条门面的实现已在 2026-10 的资金面收口里删掉，签名保留、
// 一律返回 ErrPayoutNotWired（理由同其他通道：删函数会让上游当场编译失败，留哨兵更好
// 收口）。本包不再按 util.CallZaloMsg+Kind 广播任何资金回执；该前缀现在只承载消息与
// 扫码事件。
package zalo

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
	// 只读快照：ZaloStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	zaloRunCtx atomic.Pointer[context.Context]
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

// runCtx 返回 ZaloStart 收到的根 ctx；ws_zalo_qr.go 的配置读取依赖它。
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

// ZaloStart 打开会话库、装配多账号 manager 并注册扫码 WebSocket 路由。
//
// 不自动登录任何账号：账号按需从库里读凭证（ensureConn）或显式 ZaloStartClient。
// 以下情形都只记日志并返回（不阻断整机启动，但必须留下可查痕迹）：
// enable 不为 on、没有 zalo.sessions 段、DSN 解析为空、会话库打不开。
// 重复启动保留第一个 manager，后到的就地 Stop 并记日志。
//
// DSN 解析：`zalo.sessions.dsn` 非空用自己的；留空则复用 `whatsapp.sessions.dsn`
// （日志只记「复用 whatsapp.sessions.dsn」，绝不打印连接串）。登录材料与 DSN 口令
// 都不进日志、不进 error。
func ZaloStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	cfg := zaloCfg()
	if cfg == nil || !cfg.Enabled() {
		vars.Info("不启动Zalo")
		return
	}
	sessions := cfg.Sessions
	if sessions == nil {
		vars.Info("不启动Zalo: 未配置 zalo.sessions")
		return
	}

	appCfg := corectx.CfgFrom(runCtx())
	dsn := strings.TrimSpace(sessions.DSN)
	reused := false
	if dsn == "" && appCfg != nil && appCfg.Whatsapp != nil && appCfg.Whatsapp.Sessions != nil {
		if waDSN := strings.TrimSpace(appCfg.Whatsapp.Sessions.DSN); waDSN != "" {
			dsn = waDSN
			reused = true
		}
	}
	if dsn == "" {
		vars.Info("不启动Zalo: zalo.sessions.dsn 为空且没有可复用的 whatsapp.sessions.dsn")
		return
	}

	// newZAPGStore 内部的错误文案已按 maskDSN 脱敏，这里直接记即可。
	store, err := newZAPGStore(runCtx(), dsn, zaSessionsTable)
	if err != nil {
		vars.Error("Zalo 不启动: %v", err)
		return
	}

	mgr := newZAManager(runCtx(), zaLimitsFromConfig(sessions), store, newZASDK(), realZAClock{}, nil)
	// CAS 而不是 Store：并发/重复 ZaloStart 时只有一个 manager 生效，后到的
	// 必须把自己的 store 关掉，否则连接池与建表动作会泄漏一份。
	if !zaMgr.CompareAndSwap(nil, mgr) {
		mgr.Stop()
		vars.Info("Zalo 已在运行，忽略重复启动")
		return
	}

	registerQRRoutes()

	// 只读元数据（不带凭证）；读失败不影响通道已启动这一事实，但账号数未知。
	metas, err := store.List(runCtx())
	if err != nil {
		vars.Warning("Zalo 通道已启动，但读取账号数失败: %v", err)
		return
	}
	suffix := ""
	if reused {
		suffix = "（复用 whatsapp.sessions.dsn）"
	}
	vars.Info("Zalo 通道已启动: 账号数=%d%s", len(metas), suffix)
}

// ZaloStop 摘掉 manager：listener 关闭、活会话凭证尽力写回库、store 关闭都由
// manager.Stop 完成，本函数只负责从进程级快照里摘下它。可重复调用。
func ZaloStop(_ context.Context) {
	mgr := zaMgr.Swap(nil)
	if mgr == nil {
		return
	}
	mgr.Stop()
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
