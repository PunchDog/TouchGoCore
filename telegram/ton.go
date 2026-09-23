package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// TON 的收付入口在 Telegram 侧，所以代码留在本包；但它与 Bot 长连接完全是两回事：
// 不同的凭证、不同的供应商、不同的失败路径，因此配置也是一段独立的 telegram.ton。
//
// 而 telegram.ton 本身只是对 pay_sdks 一段的引用：凭证、端点表与商户账户集中在
// pay_sdks 登记，装配动作在 paysdk，本包只保留「这条链路面向哪条链、哪个 jetton」。

var (
	// tonActive 为 nil 表示资金链路未启动，对应操作会就地拒绝而不是发出请求。
	// 复用 telegramRunCtx 作为运行上下文，不新建第二个包级 ctx 快照：
	// 两份快照会在 TelegramStart 与 TonStart 先后写入时互相漂移。
	tonActive atomic.Pointer[tonClient]
)

// TonStart 按 telegram.ton 的引用装配 TON 资金链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// Bot 能不能起来与钱包通道配没配好互不牵制，两边都不该阻断整机启动。
func TonStart(ctx context.Context) {
	if ctx == nil {
		ctx = runCtx()
	}
	setRunCtx(ctx)
	if c, ok := newTonClient(corectx.CfgFrom(ctx)); ok {
		tonActive.Store(c)
		return
	}
	vars.Info("不启动TON")
}

// TonStop 摘掉 TON 资金通道。本文件无常驻协程，清空指针即可；可重复调用。
func TonStop(ctx context.Context) {
	tonActive.Store(nil)
}

// currentTon 取资金链路；未启动时返回明确错误而不是 panic。
func currentTon() (*tonClient, error) {
	if c := tonActive.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("TON 通道未启动（检查 telegram.ton 是否引用了已开启的 pay_sdks 段与商户账户）")
}

// tonPublish 把归一后的资金回执广播给下游，键为 util.CallTonMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func tonPublish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallTonMsg+kind, res); !ok {
		vars.Debug("TON 回执无人订阅: kind=%s order_no=%s", kind, res.OrderNo)
	}
}

// tonOrderCall 是三个下单/查单门面共用的收尾：调用→补通道前缀→广播回执。
//
// ctx 为 nil 时回落到包级运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本包的留痕口径。
func tonOrderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("TON %s 失败: %w", kind, err)
	}
	tonPublish(kind, res)
	return res, nil
}
