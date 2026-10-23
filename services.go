package touchgocore

import (
	"context"

	"touchgocore/ai"
	"touchgocore/bsc"
	"touchgocore/corectx"
	"touchgocore/db"
	"touchgocore/gin"
	lua "touchgocore/golua"
	"touchgocore/localtimer"
	"touchgocore/mapmanager"
	"touchgocore/nft"
	"touchgocore/rpc"
	"touchgocore/sol"
	"touchgocore/telegram"
	"touchgocore/ton"
	"touchgocore/usdt"
	"touchgocore/websocket"
	"touchgocore/whatsapp"
	"touchgocore/zalo"
)

// ==================== 服务适配器 ====================

// timerService 定时器服务适配器
type timerService struct{}

func (s *timerService) Name() string { return "timer" }
func (s *timerService) Start(ctx context.Context) error {
	localtimer.Run(ctx)
	return nil
}
func (s *timerService) Stop(ctx context.Context) error {
	localtimer.TimeStop(ctx)
	return nil
}

// websocketService WebSocket服务适配器
type websocketService struct{}

func (s *websocketService) Name() string { return "websocket" }
func (s *websocketService) Start(ctx context.Context) error {
	return websocket.Run(ctx)
}
func (s *websocketService) Stop(ctx context.Context) error {
	websocket.Stop(ctx)
	return nil
}

// luaService Lua脚本服务适配器
type luaService struct{}

func (s *luaService) Name() string { return "lua" }
func (s *luaService) Start(ctx context.Context) error {
	return lua.Run(ctx)
}
func (s *luaService) Stop(ctx context.Context) error {
	lua.Stop(ctx)
	return nil
}

// rpcService gRPC服务适配器
type rpcService struct{}

func (s *rpcService) Name() string { return "rpc" }
func (s *rpcService) Start(ctx context.Context) error {
	return rpc.Run(ctx)
}
func (s *rpcService) Stop(ctx context.Context) error {
	rpc.Stop(ctx)
	return nil
}

// telegramService Telegram Bot服务适配器（只有 Bot 长连接；TON 已独立成 tonService）
type telegramService struct{}

func (s *telegramService) Name() string { return "telegram" }
func (s *telegramService) Start(ctx context.Context) error {
	telegram.TelegramStart(ctx)
	return nil
}
func (s *telegramService) Stop(ctx context.Context) error {
	telegram.TelegramStop(ctx)
	return nil
}

// tonService TON（原生 TON）出款/查单/链上余额服务适配器。
//
// 与 telegramService 分开：TON 通道有自己的配置段 ton.native 与失败面，
// Bot 起不起来与钱包通道配没配好互不牵制。
type tonService struct{}

func (s *tonService) Name() string { return "ton" }
func (s *tonService) Start(ctx context.Context) error {
	ton.TonStart(ctx)
	return nil
}
func (s *tonService) Stop(ctx context.Context) error {
	ton.TonStop(ctx)
	return nil
}

// 资金通道各一个适配器：配置独立开关、故障彼此无关，合并成一个 Service
// 会让「钱包没配好」连带停掉其它能用的通道。
// Start 一律返回 nil：通道故障不阻断整机启动，不启动的判断与原因由包内留日志。

// whatsappService WhatsApp 登录与提现门面服务适配器
type whatsappService struct{}

func (s *whatsappService) Name() string { return "whatsapp" }
func (s *whatsappService) Start(ctx context.Context) error {
	whatsapp.WhatsappStart(ctx)
	return nil
}
func (s *whatsappService) Stop(ctx context.Context) error {
	whatsapp.WhatsappStop(ctx)
	return nil
}

// zaloService Zalo 消息与登录门面服务适配器（zcago 直连 + Postgres 会话库）
type zaloService struct{}

func (s *zaloService) Name() string { return "zalo" }
func (s *zaloService) Start(ctx context.Context) error {
	zalo.ZaloStart(ctx)
	return nil
}
func (s *zaloService) Stop(ctx context.Context) error {
	zalo.ZaloStop(ctx)
	return nil
}

// usdtService USDT(TRC20) 提现/账户查询服务适配器
type usdtService struct{}

func (s *usdtService) Name() string { return "usdt" }
func (s *usdtService) Start(ctx context.Context) error {
	usdt.UsdtStart(ctx)
	return nil
}
func (s *usdtService) Stop(ctx context.Context) error {
	usdt.UsdtStop(ctx)
	return nil
}

// bscService BSC(BEP20 USDT) 出款/查单/链上余额服务适配器
//
// 与 usdt 服务并列而不是合并：两条链的配置段、故障面与启停时间都彼此无关，
// 合并成一个 Service 会让「BSC 的 RPC 不通」连带停掉 TRON 那条本来能用的通道。
type bscService struct{}

func (s *bscService) Name() string { return "bsc" }
func (s *bscService) Start(ctx context.Context) error {
	bsc.BscStart(ctx)
	return nil
}
func (s *bscService) Stop(ctx context.Context) error {
	bsc.BscStop(ctx)
	return nil
}

// solService SOL(SPL USDT) 出款/查单/链上余额服务适配器
//
// 与 usdt / bsc 并列而不是合并：三条链的配置段、故障面与启停时间都彼此无关，
// 合并成一个 Service 会让「SOL 的 RPC 不通」连带停掉另外两条本来能用的通道。
type solService struct{}

func (s *solService) Name() string { return "sol" }
func (s *solService) Start(ctx context.Context) error {
	sol.SolStart(ctx)
	return nil
}
func (s *solService) Stop(ctx context.Context) error {
	sol.SolStop(ctx)
	return nil
}

// nftService NFT 查询/铸造/转账/对账服务适配器（口径同其余通道：Start 返回 nil，
// nft 段没配好只记日志、不阻断整机；它是资产通道，不与数字货币资金链路共用配置）。
type nftService struct{}

func (s *nftService) Name() string { return "nft" }
func (s *nftService) Start(ctx context.Context) error {
	nft.NftStart(ctx)
	return nil
}
func (s *nftService) Stop(ctx context.Context) error {
	nft.NftStop(ctx)
	return nil
}

// mapService 地图管理服务适配器
type mapService struct{}

func (s *mapService) Name() string { return "map" }
func (s *mapService) Start(ctx context.Context) error {
	return mapmanager.RunMap(ctx)
}
func (s *mapService) Stop(ctx context.Context) error {
	mapmanager.StopMap(ctx)
	return nil
}

// ginService Gin HTTP服务适配器
type ginService struct{}

func (s *ginService) Name() string { return "gin" }
func (s *ginService) Start(ctx context.Context) error {
	return gin.Run(ctx)
}
func (s *ginService) Stop(ctx context.Context) error {
	return gin.Stop(ctx)
}

// metricsService Prometheus监控服务适配器
type metricsService struct{}

func (s *metricsService) Name() string { return "metrics" }
func (s *metricsService) Start(ctx context.Context) error {
	cfg := corectx.CfgFrom(ctx)
	if cfg != nil {
		StartMetrics(cfg.Metrics)
	}
	return nil
}
func (s *metricsService) Stop(ctx context.Context) error {
	ShutdownMetrics(ctx)
	return nil
}

// modelAPIService 模型API服务适配器
type modelAPIService struct{}

func (s *modelAPIService) Name() string { return "modelapi" }
func (s *modelAPIService) Start(ctx context.Context) error {
	return ai.Run(ctx)
}
func (s *modelAPIService) Stop(ctx context.Context) error {
	ai.Stop(ctx)
	return nil
}

// Layer 与 Service 同形断言：db/cache 不能 import 根包（成环），断言放在这里做。
var _ Service = (*db.CacheLayer)(nil)

// cacheService 两级缓存层服务适配器。
// cache 段未配置或未启用时 App.Cache 为 nil，Start/Stop 直接跳过（与 telegram/usdt
// 等「没配就不启动」的既有约定一致）。注册在 services 列表末尾：
// Shutdown 反序停止保证它的 final flush 早于 closeDatabase。
type cacheService struct{ app *App }

func (s *cacheService) Name() string { return "cache" }
func (s *cacheService) Start(ctx context.Context) error {
	if s.app == nil || s.app.Cache == nil {
		return nil
	}
	return s.app.Cache.Start(ctx)
}
func (s *cacheService) Stop(ctx context.Context) error {
	if s.app == nil || s.app.Cache == nil {
		return nil
	}
	return s.app.Cache.Stop(ctx)
}
