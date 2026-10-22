// Package usdt 是 USDT（TRC20）通道的兼容门面：import 路径与函数签名留在原地，
// 实现全在 tron 包（装配 tron/usdt.go、地址算法 tron/address.go、链规则登记 tron/chainrule.go）。
//
// 地址算法与出款链路都在 tron，因为 TRC20 与原生 TRX 是同一条链上的两种币：
// 「地址怎么编码」「能不能带 memo」是链侧知识，两份实现迟早漂移，漂到后来就是
// 一侧拒掉的地址被另一侧广播出去。所以校验只留在 tron 包内一处（chainrule.go 的
// checkOrderAddress），本包只做转发：下游（whatapp 服务 cmd/t1jobserver/main.go）
// 同时 blank-import touchgocore/tron 与 touchgocore/usdt，也不会把同一份规则装上两回。
//
// 这个包留着只是因为下游把 usdt.UsdtXxx 与 util.CallUsdtMsg+"Xxx" 写死在了源码里。
// 本包不读配置、不拼报文、不做状态归一：trc20 当网络填、合约地址校验和不过这类误配
// 仍在启动时被拒，拦下的位置搬到了 tron 侧的装配里。
package usdt

import (
	"context"

	"touchgocore/tron"
)

// UsdtStart 按配置装配 USDT 资金链路，实现转调 tron 包的 USDT 门面。
//
// 配置段缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
func UsdtStart(ctx context.Context) { tron.UsdtStart(ctx) }

// UsdtStop 摘掉供应商客户端。链路上无常驻协程，清空快照指针即可；可重复调用。
func UsdtStop(ctx context.Context) { tron.UsdtStop(ctx) }
