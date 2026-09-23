package touchgocore

import (
	"context"

	"touchgocore/ai"
	"touchgocore/corectx"
	"touchgocore/gin"
	lua "touchgocore/golua"
	"touchgocore/localtimer"
	"touchgocore/mapmanager"
	"touchgocore/rpc"
	"touchgocore/telegram"
	"touchgocore/ustd"
	"touchgocore/websocket"
	"touchgocore/whatsapp"
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

// telegramService Telegram Bot服务适配器
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

// 三条资金通道各一个适配器：配置独立开关、故障彼此无关，合并成一个 Service
// 会让「钱包没配好」连带停掉另外两条能用的通道。
// Start 一律返回 nil：通道故障不阻断整机启动，不启动的判断与原因由包内留日志。

// whatsappService WhatsApp 登录与充值/提现服务适配器
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

// ustdService USDT(TRC20) 充值/提现服务适配器
type ustdService struct{}

func (s *ustdService) Name() string { return "ustd" }
func (s *ustdService) Start(ctx context.Context) error {
	ustd.UstdStart(ctx)
	return nil
}
func (s *ustdService) Stop(ctx context.Context) error {
	ustd.UstdStop(ctx)
	return nil
}

// tonService TON 充值/提现服务适配器（代码在 telegram 包，配置在 telegram.ton 段）
type tonService struct{}

func (s *tonService) Name() string { return "ton" }
func (s *tonService) Start(ctx context.Context) error {
	telegram.TonStart(ctx)
	return nil
}
func (s *tonService) Stop(ctx context.Context) error {
	telegram.TonStop(ctx)
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
