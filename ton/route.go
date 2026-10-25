package ton

import (
	"context"

	"touchgocore/chainroute"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件只留 TON 侧门面实例与启停入口。门面机器（快照 → 调通道 → 广播回执、
// 装配 / 分派登记 / 重启关旧）已抽到 touchgocore/chainroute 由四链共用一份（见
// chainroute 包文档：此前四份逐字副本里，「重启关旧连接」的修复只落在 ton 一份）。
// 链侧差异只剩装配入口 newTONNativeClient（在 native.go）与几个常量，做成 Route 字段注入。

// tonNativeRoute 是原生 TON 门面实例：装配读 ton.native，回执走 CallTonMsg，
// 装配成功即登记到 pay 的 ton.native 种类。
var tonNativeRoute = chainroute.Route{
	Label:          "TON",
	ConfigKey:      "ton.native",
	NotStartedHint: "enable / chain 与热钱包是否齐备",
	Callback:       util.CallTonMsg,
	Currency:       pay.CurrencyTON,
	Kind:           pay.KindTON,
	NewClient:      newTONNativeClient,
}

func init() {
	// 宿主可能只 blank-import 本包就从别处敲门面，链路得有可读的运行上下文。
	tonNativeRoute.SetRunCtx(context.Background())
}

// TonStart 按 ton.native 装配 TON 资金链路。
//
// 配置缺失、通道没开、密钥未注入，都只记日志并返回：资金通道没配好不该阻断
// 整机启动，但必须留下可查的痕迹。
func TonStart(ctx context.Context) { tonNativeRoute.Start(ctx) }

// TonStop 摘掉 TON 资金通道并释放接入点。可重复调用。
func TonStop(ctx context.Context) { tonNativeRoute.Stop() }
