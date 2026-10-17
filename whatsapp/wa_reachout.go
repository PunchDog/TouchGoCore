package whatsapp

// 本文件是单聊发送的「外联凭证门闸」与 463 恢复档。
//
// 背景（逐行核自 whatsmeow v0.0.0-20261007111105）：DM 出站节点必须二选一带上
//   - tctoken：对方会话的 privacy token，只来自手机推送的历史同步
//     （WM/message.go:1011-1055 把 HistorySync 会话里的 TcToken 写库）或
//     privacy_token 通知（WM/notification.go:286-335）；
//   - cstoken：HMAC-SHA256(nct 盐, 对方 LID)，盐来自 regular_high 的 nct_salt_sync
//     （WM/appstate.go:242-247）或 HistorySync 的 NctSalt 字段（WM/message.go:801），
//     且 PN 目标必须先有本地 PN→LID 映射（WM/cstoken.go:36-52，映射缺失直接返回 nil）。
// 两者皆无时服务器在 ack 上回 error 463（WM/send.go:459-461）。
//
// 真机取证（2026-10-16 本地会话库）：nct_salt 全表为空、目标账号 privacy_tokens 0 行，
// 而其它账号有 token 行——「手机推历史 → 落库」这条路本身是通的，只是这台手机没推。
// 旧门闸把「调过一次 regular_high 快路」当成「盐已在库」：快路在 version>0 时库内零网络
// 返回（WM/appstate.go:61-63），regular_high 快照里若本就没有 nct_salt_sync，盐就永远
// 缺位；批内每条消息又各自向手机要一次历史、各等 12 秒，3 条的批白烧 36 秒仍全灭。
//
// 本文件的口径：
//   - 凭证在不在以**读库**为准（NCTSalt / PrivacyToken 两次本地 SELECT），不以「调过同步」为准；
//   - 发送前门闸只做便宜事：等离线回放、regular_high 快路/增量、向手机**发一次**历史请求
//     （把手机推送尽早启动），不做长等待——长等待放在 463 恢复档，避免每条发送都预付预算；
//   - 恢复档的等待用**轮询到「有」为止**：库把历史写库放在异步 goroutine
//     （WM/message.go:815-817 doStorage），固定睡 1 秒与它赛跑是旧代码的另一处空转；
//   - 贵操作（增量 regular_high、全量 regular_high、向手机要历史）各自按窗口节流并单飞；
//     等满一轮仍落空记一笔落空账，窗口内后续消息（含它们的恢复档）直接快失败——
//     没有新历史到达时再等不会有信息增量；HistorySync 事件到达（histSeen 变化）作废落空账。
// 群组发送不过这道闸：sendGroup 不挂任何 token（WM/send.go 群分支），463 只发生在单聊。

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow/types"

	"touchgocore/vars"
)

// 门闸节奏的注入点（测试整体替换、用完还原，照 waBatchSleep 的范式）：
//   - waReachoutRefreshMinInterval：同一连接两次增量 regular_high 的最小间隔；
//   - waReachoutFullMinInterval：同一连接两次「擦版本全量重拉」的最小间隔
//     （与 waContactSyncFullMinInterval 同款理由：擦版本要重付全量快照）；
//   - waReachoutHistoryMinInterval：两次向手机要历史的最小间隔，也是落空账的有效期；
//   - waReachoutWait：恢复档一轮轮询等待凭证落地的上限；
//   - waReachoutPollTick：轮询间隔。
var (
	waReachoutRefreshMinInterval = 60 * time.Second
	waReachoutFullMinInterval    = 10 * time.Minute
	waReachoutHistoryMinInterval = 60 * time.Second
	waReachoutWait               = 12 * time.Second
	waReachoutPollTick           = 400 * time.Millisecond
)

// hasReachoutCreds 判定本连接对 target 是否已具备任一外联凭证（盐或对方 token）。
// 两次本地 SELECT 的代价远低于一次被拒的发送，所以逐条发送都问，不做内存缓存——
// 缓存会把「历史同步刚落地」读成「还没有」，又退回 463。
func (w *waClientWrapper) hasReachoutCreds(ctx context.Context, target types.JID) bool {
	return w.hasSalt(ctx) || w.hasTargetToken(ctx, target)
}

func (w *waClientWrapper) hasSalt(ctx context.Context) bool {
	salt, err := w.sess.NCTSalt(ctx)
	return err == nil && len(salt) > 0
}

func (w *waClientWrapper) hasTargetToken(ctx context.Context, target types.JID) bool {
	tok, err := w.sess.PrivacyToken(ctx, target)
	return err == nil && len(tok) > 0
}

// ensureReachoutCreds 是单聊发送前的门闸：凭证齐备时零额外动作直返；缺凭证时按
// 离线回放 → regular_high 快路 → 增量 → 向手机发一次历史请求 的顺序逐级补齐。
// 每一级之后都重问一次凭证：任一级落地就不付下一级的代价。
//
// 这里刻意**不等**历史落地：门闸在每条发送的必经路上，预付 12 秒会让手机离线时
// 每条发送都白等一轮；等待集中在 463 恢复档（recoverReachout463），一批发信里
// 由单飞与落空账保证只付一次。历史请求在这里先发出去，是为了让手机推送尽早启动——
// 真机上半数 463 在恢复档开始等之前就已经能等到凭证。
func (w *waClientWrapper) ensureReachoutCreds(ctx context.Context, target types.JID) {
	if w.hasReachoutCreds(ctx, target) {
		return
	}
	w.waitOfflineReplay(ctx)
	if w.hasReachoutCreds(ctx, target) {
		return
	}
	w.syncRegularHighOnce(ctx)
	if w.hasReachoutCreds(ctx, target) {
		return
	}
	w.refreshRegularHigh(ctx, false)
	if w.hasReachoutCreds(ctx, target) {
		return
	}
	w.requestHistory(ctx)
}

// recoverReachout463 是发送被 463 拒掉之后的恢复档：快路标记复位（首跑失败的那次
// 允许重试），增量强制立即跑并在全量窗口外补一次擦版本全量，再向手机要历史并轮询。
// 返回 true 表示轮询等到了**能改变服务器判定的新凭证**（对方的 tctoken；或发送当时
// 还没盐、轮询期盐落了库 ⇒ cstoken 从无到有），调用方这才值得重试那一条发送。
//
// 为什么谓词不能是「任一凭证在库」：463 本身已经证明服务器不接受当前凭证组合——
// 有盐还 463 的收件人是「手机上已存在的会话」，服务器要的是 tctoken，拿同一份盐
// 重发 cstoken 只会再吃一个 463（真机 2026-10-16 :9 批次实证：轮询被「盐已在库」
// 秒判命中，重试同料，日志里连等待告警都没出现）。
func (w *waClientWrapper) recoverReachout463(ctx context.Context, target types.JID) bool {
	w.saltSynced.Store(false)
	saltBefore := w.hasSalt(ctx)
	w.refreshRegularHigh(ctx, true)
	return w.pullHistoryAndWait(ctx, target, saltBefore)
}

// syncRegularHighOnce 每轮连接至多跑一次 regular_high 快路（version==0 时库内部
// 自动按全量走，首连靠它拿盐）。失败复位标记，下次发送再试。
func (w *waClientWrapper) syncRegularHighOnce(ctx context.Context) {
	if !w.saltSynced.CompareAndSwap(false, true) {
		return
	}
	if err := w.sess.SyncRegularHigh(ctx, false, true); err != nil {
		w.saltSynced.Store(false)
		vars.Warning("WhatsApp[%s] 同步 regular_high 快路失败（外联盐可能缺位，单聊或被 463 拒绝）: %v", w.jid.String(), err)
	}
}

// refreshRegularHigh 拉一次增量 regular_high（服务端后补/轮换盐走这一档）；
// force=true 时绕过增量窗口，并在全量窗口外补一次擦版本全量重拉。
// 增量失败不记账：下次发送还会再试，而断开态的发信根本走不到这里。
func (w *waClientWrapper) refreshRegularHigh(ctx context.Context, force bool) {
	w.reachMu.Lock()
	defer w.reachMu.Unlock()
	if !force && withinWindow(w.lastRefreshAt, waReachoutRefreshMinInterval) {
		return
	}
	if err := w.sess.SyncRegularHigh(ctx, false, false); err != nil {
		vars.Warning("WhatsApp[%s] 增量 regular_high 失败（外联盐可能仍缺位）: %v", w.jid.String(), err)
		return
	}
	w.lastRefreshAt = time.Now()
	if !force {
		return
	}
	if withinWindow(w.lastFullAt, waReachoutFullMinInterval) {
		return
	}
	if err := w.sess.SyncRegularHigh(ctx, true, false); err != nil {
		vars.Warning("WhatsApp[%s] 全量重拉 regular_high 失败: %v", w.jid.String(), err)
		return
	}
	w.lastFullAt = time.Now()
}

// requestHistory 向手机发一次最近 7 天历史同步请求（tctoken 与 NctSalt 都在里面），
// 窗口内不重发。只发不等：等待归 pullHistoryAndWait。
func (w *waClientWrapper) requestHistory(ctx context.Context) {
	w.reachMu.Lock()
	defer w.reachMu.Unlock()
	w.requestHistoryLocked(ctx)
}

func (w *waClientWrapper) requestHistoryLocked(ctx context.Context) {
	if withinWindow(w.lastHistAt, waReachoutHistoryMinInterval) {
		return
	}
	if err := w.sess.RequestRecentHistory(ctx); err != nil {
		vars.Warning("WhatsApp[%s] 向手机请求历史同步失败: %v", w.jid.String(), err)
		return
	}
	w.lastHistAt = time.Now()
	vars.Info("WhatsApp[%s] 已向手机发出历史同步请求（对方 tctoken/盐随手机在线推送到达）", w.jid.String())
}

// pullHistoryAndWait 恢复档的等待段：（窗口外）补一次历史请求，然后轮询**新凭证**
// （谓词理由见 recoverReachout463）。单飞在这把锁里：并发恢复档只有一份请求与一轮
// 等待。返回 true ⇒ 等到了能改变服务器判定的凭证，调用方才值得重试那一条。
//
// 落空账的语义：等满 waReachoutWait 仍没新凭证就记下此刻的 histSeen；窗口内再来
// （批内后续消息的恢复档）且 histSeen 没变 ⇒ 直接返回 false 不再等——没有新历史
// 到达，再等一轮不会有信息增量，把时间留给错误文案去解释。期间任何 HistorySync
// 事件到达都会抬 histSeen，落空账随即作废。
func (w *waClientWrapper) pullHistoryAndWait(ctx context.Context, target types.JID, saltBefore bool) bool {
	w.reachMu.Lock()
	defer w.reachMu.Unlock()
	seen := w.histSeen.Load()
	if withinWindow(w.pollMissAt, waReachoutHistoryMinInterval) && seen == w.pollMissSeen {
		return false
	}
	w.requestHistoryLocked(ctx)
	if w.pollReachoutCreds(ctx, target, saltBefore) {
		w.pollMissAt = time.Time{}
		return true
	}
	w.pollMissAt = time.Now()
	w.pollMissSeen = seen
	vars.Warning("WhatsApp[%s] 等历史同步 %s 后新外联凭证（对方 tctoken%s）仍未落地，本条不重试"+
		"（重试同料仍会被服务器回 463，需手机在线并打开 WhatsApp）", w.jid.String(), waReachoutWait,
		map[bool]string{true: "，发送当时已有盐故盐不算新", false: "/盐"}[saltBefore])
	return false
}

// pollReachoutCreds 轮询新凭证：对方的 tctoken 出现即命中；发送当时还没有盐的，
// 轮询期盐落了库（cstoken 从无到有）也算命中。等到命中、ctx 结束或 waReachoutWait 到点。
func (w *waClientWrapper) pollReachoutCreds(ctx context.Context, target types.JID, saltBefore bool) bool {
	improved := func() bool {
		if w.hasTargetToken(ctx, target) {
			return true
		}
		return !saltBefore && w.hasSalt(ctx)
	}
	if improved() {
		return true
	}
	deadline := time.NewTimer(waReachoutWait)
	defer deadline.Stop()
	tick := time.NewTicker(waReachoutPollTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return improved()
		case <-deadline.C:
			return improved()
		case <-tick.C:
			if improved() {
				return true
			}
		}
	}
}

// waitOfflineReplay 等连接后的离线回放结束。回放里可能有对方发来的会话凭证，
// 没等完就发会被 463 拒绝。没有回放时立刻返回。
func (w *waClientWrapper) waitOfflineReplay(ctx context.Context) {
	if w == nil || !w.offlinePending.Load() {
		return
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for w.offlinePending.Load() {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}
