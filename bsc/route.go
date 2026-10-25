package bsc

import (
	"context"

	"touchgocore/chainroute"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件只留 BSC 侧门面实例与启停入口。门面机器（快照 → 调通道 → 广播回执、
// 装配 / 分派登记 / 重启关旧）已抽到 touchgocore/chainroute 由四链共用一份：
// 此前四个包各写一份逐字副本，「重启关旧连接」的修复只落在其中一份、其余三份
// 泄漏连接（见 chainroute 包文档）。链侧差异只剩装配入口 newBscUSDTClient 与
// 几个常量，全部做成 Route 字段注入。

// bscRoute 是 BSC 侧门面实例：装配读 bsc.usdt，回执走 CallBscMsg，
// 装配成功即登记到 pay 的 bsc.usdt 种类。
var bscRoute = chainroute.Route{
	Label:          "BSC-USDT",
	ConfigKey:      "bsc.usdt",
	NotStartedHint: "enable / driver / chain / 合约是否齐备",
	Callback:       util.CallBscMsg,
	Currency:       pay.CurrencyUSDT,
	Kind:           pay.KindBscUSDT,
	NewClient:      newBscUSDTClient,
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	bscRoute.SetRunCtx(context.Background())
}

// BscStart 按配置装配 BSC USDT 资金链路。
func BscStart(ctx context.Context) { bscRoute.Start(ctx) }

// BscStop 摘掉客户端并关掉接入点连接；可重复调用。
func BscStop(ctx context.Context) { bscRoute.Stop() }
