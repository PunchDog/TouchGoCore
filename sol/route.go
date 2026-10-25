package sol

import (
	"context"

	"touchgocore/chainroute"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件只留 SOL 侧门面实例与启停入口。门面机器（快照 → 调通道 → 广播回执、
// 装配 / 分派登记 / 重启关旧）已抽到 touchgocore/chainroute 由四链共用一份：
// 此前四个包各写一份逐字副本，「重启关旧连接」的修复只落在其中一份、其余三份
// 泄漏连接（见 chainroute 包文档）。链侧差异只剩装配入口 newSolUSDTClient 与
// 几个常量，全部做成 Route 字段注入。

// solRoute 是 SOL 侧门面实例：装配读 sol.usdt，回执走 CallSolMsg，
// 装配成功即登记到 pay 的 sol.usdt 种类。
var solRoute = chainroute.Route{
	Label:          "SOL-USDT",
	ConfigKey:      "sol.usdt",
	NotStartedHint: "enable / driver / chain / mint 是否齐备",
	Callback:       util.CallSolMsg,
	Currency:       pay.CurrencyUSDT,
	Kind:           pay.KindSolUSDT,
	NewClient:      newSolUSDTClient,
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	solRoute.SetRunCtx(context.Background())
}

// SolStart 按配置装配 SOL USDT 资金链路。
func SolStart(ctx context.Context) { solRoute.Start(ctx) }

// SolStop 摘掉客户端并关掉接入点连接；可重复调用。
func SolStop(ctx context.Context) { solRoute.Stop() }
