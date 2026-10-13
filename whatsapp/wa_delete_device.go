package whatsapp

// 本文件是「吊销一个已配对账号」的编排：把 latch → 服务端 unlink → 收常驻协程 → 删设备行
// → 广播终态这五步串成一个不可拆的顺序，单独成文是因为它同时跨了三个子系统
// （会话管理 wa_manager.go、存储 store_postgres.go、回调广播 wa_client.go），
// 而顺序的每一条理由都得写在同一处才读得懂。公开门面 WhatsappDeleteDevice 在 wa_client.go。
//
// 与「只下线」的分界：StopClient 关掉连接但设备行还在，重新 StartClient 即恢复；
// 本文件删掉设备行，密钥材料随之级联清除（whatsmeow_device 的 14 张子表都是
// ON DELETE CASCADE，查自 WM/store/sqlstore/upgrades/00-latest-schema.sql），
// 之后这个账号必须重新扫码配对。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"touchgocore/vars"
)

// waLogoutTimeout 闸住服务端 unlink 那一次 IQ 的等待。
//
// 必须是本包自己的常量：库侧 md/set 不带 Timeout 时取 defaultRequestTimeout = 75 秒
// （查自 WM/request.go:150-154），且 Logout 没设 NoRetry，socket 中途被拆还要进
// retryFrame 再等 5 秒重连（WM/request.go:184-200）。不夹住的话，一次吊销最坏把
// 调用方堵 75 秒——sendIQ 认 ctx.Done()（WM/request.go:176），所以给 ctx 上闸即有效。
const waLogoutTimeout = 5 * time.Second

// 三条吊销文案：只说「做成的事 + 残留要谁处置」的分类事实，不拼库的原始错误串。
// 广播载荷不进敏感材料的纪律见 wa_client.go 头部；原始错误只进返回值与日志。
const (
	waReasonUnlinked = "本地设备行已删除，服务端已 unlink；该账号需重新扫码配对才能再用"
	// waReasonUnlinkFail 是「在跑但 unlink 没谈成」——手机端仍列着这台已关联设备，
	// 只有手机侧能取消它，本包已经没有凭据可以再谈一次。
	waReasonUnlinkFail = "本地设备行已删除，但服务端 unlink 未完成；手机端仍显示为已关联设备，需在 WhatsApp 里手动取消这台设备"
	// waReasonNotRunning 是「压根没在跑」——没连接就没法 unlink，
	// 本包不为一次破坏性调用起整条握手。
	waReasonNotRunning = "本地设备行已删除；该账号没有在跑，未做服务端 unlink，手机端仍显示为已关联设备，需在 WhatsApp 里手动取消这台设备"
)

// DeleteDevice 吊销 jid 对应的配对：删掉会话库里的设备行，在跑的账号先做服务端 unlink。
//
// 顺序逐条理由（改动前先读完，这几步的次序每一条都有对应的活闸测着）：
//
//   - ① 先查存在性再动任何东西：查无设备行就地拒绝，既不碰在跑会话也不广播——
//     「行状态未知」和「行已删除」必须是两种能区分开的读数。
//   - ② 在跑时先把 w.loggedOut 落位，再谈 unlink：库里 Logout 成功走的是「预期内断开」，
//     不发事件（查自 WM/client.go 的 onDisconnect），但 unlink 失败/超时那一路 socket
//     是被远端拆的，会派发 events.Disconnected → dispatch → markDown（wa_manager.go）→
//     常驻协程从 waitUntilDown 醒来重新 Login，把正要吊销的设备又连回去。
//     loggedOut 在 run() 每轮顶部检查（wa_manager.go StartClient 之后那段循环），
//     先落位就把这条重连路变成一次干净退出。
//   - ③ unlink 只在还握着活 socket 时做得到，所以必须在收尾之前。
//     反过来先 StopClient 会因 socket 已为 nil 而永久拿到 ErrNotConnected（WM/client.go），
//     服务端解绑这一趟就再也没机会走。
//   - ④ 收尾复用 StopClient：closeOnce 的纪律只留一处，且它等 <-done，
//     能保证 run() 的 defer sess.Disconnect() 已经执行完——删行时进程内不可能还有活 socket。
//     StopClient 超时按非致命残留处理：latch 已落，那条协程不可能再 Login，会自行收尾。
//   - ⑤ 设备行最后删，且**绝不把 w.dev 交出去**：库侧 Logout 成功时顺带调了
//     Device.Delete，那一步把 dev.ID 置成 nil（查自 WM/store/store.go 的 Delete），
//     再递给 Container.DeleteDevice 就撞它的 ID 闸（WM/store/sqlstore/container.go）。
//     改成按 JID 重读一次，一个 cur != nil && cur.ID != nil 守卫同时兜住两种「库已删过」形态：
//     真库查无返回 (nil,nil)，测试假件返回 ID 已置 nil 的同一对象。
//   - ⑥ 凡是真把行删掉的路径（含 ③ 失败和 ④ 超时）都恰好广播一条 logged_out：
//     库侧 Logout 全程不派发任何事件（WM/client.go），不自己广播，下游会一直往死账号路由。
//     删行本身失败时不广播——行可能还在，账号还能 StartClient 拉起来，谎报终态更糟。
//
// ctx 只用来闸 unlink；查库与删行走包级运行上下文。理由是行删除是数据完整性保证，
// 调用方取消不该留下「名义上吊销了、行还在」的半吊子配对。
func (m *waManager) DeleteDevice(ctx context.Context, jid types.JID) error {
	key := jid.String()

	dev, err := m.store.GetDevice(m.runCtx, jid)
	if err != nil {
		return fmt.Errorf("读取会话库失败: %w", err)
	}
	if dev == nil || dev.ID == nil {
		return fmt.Errorf("账号 %s 在会话库里没有设备行（本就未配对或已吊销过）", key)
	}
	if ctx == nil {
		ctx = runCtx()
	}

	var residual []string
	reason := waReasonNotRunning
	if v, running := m.clients.Load(key); running {
		w := v.(*waClientWrapper)
		w.loggedOut.Store(true)

		lctx, cancel := context.WithTimeout(ctx, waLogoutTimeout)
		lerr := w.sess.Logout(lctx)
		cancel()
		if lerr != nil {
			vars.Warning("WhatsApp[%s] 服务端 unlink 失败，改走本地删行兜底: %v", key, lerr)
			residual = append(residual, fmt.Sprintf("服务端 unlink 未完成: %v", lerr))
			reason = waReasonUnlinkFail
		} else {
			reason = waReasonUnlinked
		}

		if serr := m.StopClient(jid); serr != nil {
			vars.Warning("WhatsApp[%s] %v", key, serr)
			residual = append(residual, fmt.Sprintf("常驻协程未在 %s 内退出（已放手，它会自行收尾）", waStopWaitBudget))
		}
	}

	cur, err := m.store.GetDevice(m.runCtx, jid)
	if err != nil {
		return fmt.Errorf("读取会话库失败: %w", err)
	}
	if cur != nil && cur.ID != nil {
		if err := m.store.DeleteDevice(m.runCtx, cur); err != nil {
			return fmt.Errorf("吊销设备行失败: %w", err)
		}
	}

	publishWA("Connection", &ConnEvent{Account: key, State: "logged_out", Reason: reason})
	if len(residual) > 0 {
		return fmt.Errorf("设备行已删除，但%s", strings.Join(residual, "；"))
	}
	return nil
}
