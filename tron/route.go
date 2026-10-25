package tron

import (
	"context"

	"touchgocore/chainroute"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件只留 TRON USDT 侧门面实例与启停入口。门面机器（快照 → 调通道 → 广播回执、
// 装配 / 分派登记 / 重启关旧）已抽到 touchgocore/chainroute 由四链共用一份：此前四个
// 包各写一份逐字副本，「重启关旧连接」的修复只落在其中一份、其余三份泄漏连接（见
// chainroute 包文档）。链侧差异只剩装配入口 newUSDTClient（在 usdt.go）与几个常量，
// 全部做成 Route 字段注入。
//
// TRX 那条供应商提现链路已删（见 tron.go 的哨兵），本文件只服务 USDT-TRC20 一条通道。

// usdtRoute 是 USDT 侧门面实例：装配读 tron.usdt，回执走 CallUsdtMsg，
// 装配成功即登记到 pay 的 tron.usdt 种类。
var usdtRoute = chainroute.Route{
	Label:          "USDT",
	ConfigKey:      "tron.usdt",
	NotStartedHint: "enable / chain / 商户账户是否齐备",
	Callback:       util.CallUsdtMsg,
	Currency:       pay.CurrencyUSDT,
	Kind:           pay.KindTronUSDT,
	NewClient:      newUSDTClient,
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	usdtRoute.SetRunCtx(context.Background())
}

// UsdtStart 按配置装配 USDT 资金链路。
//
// 配置段缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
func UsdtStart(ctx context.Context) { usdtRoute.Start(ctx) }

// UsdtStop 摘掉供应商客户端。本包无常驻协程，清空指针即可；可重复调用。
func UsdtStop(ctx context.Context) { usdtRoute.Stop() }
