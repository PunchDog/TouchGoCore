package whatsapp

// 外联凭证门闸与 463 恢复档的测试（wa_reachout.go）。
//
// 钉的是 2026-10-16 真机取证后立下的四条口径：
//   - 凭证在不在以读库为准：凭证齐备时门闸零动作（不拉 regular_high、不要历史）；
//   - 缺凭证时一批发信只向手机要一次历史（单飞 + 窗口），不逐条重发；
//   - 463 恢复档会强制增量 +（窗口外）擦版本全量重拉 regular_high，并在凭证落地后
//     让重试那一条真发出去；
//   - 等满一轮仍落空记落空账：窗口内后续消息快失败（不重等），而 HistorySync 事件
//     到达会作废落空账、恢复档重新等一轮。
// 全部走 seam 假件，不联网。

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	reachAccount = "8613800000000:1@s.whatsapp.net"
	reachFriend  = "8613900000001"
	reachGroup   = "123456789@g.us"
)

// setReachoutClocks 把恢复档的轮询预算换成测试尺度，用完还原。
func setReachoutClocks(t *testing.T, wait, tick time.Duration) {
	t.Helper()
	oldWait, oldTick := waReachoutWait, waReachoutPollTick
	waReachoutWait, waReachoutPollTick = wait, tick
	t.Cleanup(func() { waReachoutWait, waReachoutPollTick = oldWait, oldTick })
}

// wrapperOf 取门面快照里某账号的 wrapper（落空账等内部账直接读它）。
func wrapperOf(t *testing.T, m *waManager, jid string) *waClientWrapper {
	t.Helper()
	v, ok := m.clients.Load(jid)
	if !ok {
		t.Fatalf("账号 %s 不在册", jid)
	}
	return v.(*waClientWrapper)
}

// startReachWA 装配门闸测试用的门面快照：单账号、假会话按 configure 预置。
func startReachWA(t *testing.T, configure func(*fakeSession)) (*waManager, func() *fakeSession) {
	t.Helper()
	dev := fakeDevice(t, reachAccount, "android", "reach")
	m, _, get := startFakeWA(t, nil, configure, dev)
	return m, func() *fakeSession { return get(reachAccount) }
}

// TestReachoutCredsPresentSkipsGate 凭证齐备时门闸零动作：不拉 regular_high、
// 不向手机要历史，发送原样出去。这是「凭证在不在以读库为准」的反面钉死——
// 旧代码无论有没有凭证都会付一次快路同步。
func TestReachoutCredsPresentSkipsGate(t *testing.T) {
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.setReachout([]byte("salt-salt-salt-salt-salt-salt-32"), nil)
	})
	if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err != nil {
		t.Fatalf("凭证齐备时发送失败: %v", err)
	}
	f := get()
	rhSeq, histReqs := f.reachState()
	if len(rhSeq) != 0 || histReqs != 0 {
		t.Errorf("凭证齐备时门闸应当零动作: rhSeq=%v histReqs=%d", rhSeq, histReqs)
	}
}

// TestReachoutGateSingleFlightPerBatch 缺凭证时一批发信只向手机要一次历史：
// 门闸逐级补到「发请求」为止，窗口内后续消息不重发；regular_high 只付
// 快路+增量两级（全量留给 463 恢复档）。
func TestReachoutGateSingleFlightPerBatch(t *testing.T) {
	_, get := startReachWA(t, func(f *fakeSession) { f.loggedIn = true })
	for i := 0; i < 3; i++ {
		if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err != nil {
			t.Fatalf("第 %d 条发送失败: %v", i+1, err)
		}
	}
	rhSeq, histReqs := get().reachState()
	if histReqs != 1 {
		t.Errorf("三条发送应只向手机要一次历史: histReqs=%d", histReqs)
	}
	want := [][2]bool{{false, true}, {false, false}}
	if len(rhSeq) != len(want) {
		t.Fatalf("regular_high 调用序列应为快路+增量: got=%v", rhSeq)
	}
	for i := range want {
		if rhSeq[i] != want[i] {
			t.Errorf("regular_high 第 %d 次入参应为 %v: got=%v", i+1, want[i], rhSeq[i])
		}
	}
}

// TestReachoutGroupSendBypassesGate 群组发送不过门闸：库的群分支不挂任何 token，
// 463 只发生在单聊，群发送不该付凭证检查之外的任何代价。
func TestReachoutGroupSendBypassesGate(t *testing.T) {
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireCreds = true
	})
	if _, err := WhatsappSendGroupMessage(context.Background(), reachAccount, reachGroup, "hi"); err != nil {
		t.Fatalf("群发送失败: %v", err)
	}
	f := get()
	rhSeq, histReqs := f.reachState()
	if len(rhSeq) != 0 || histReqs != 0 {
		t.Errorf("群发送不应触发门闸: rhSeq=%v histReqs=%d", rhSeq, histReqs)
	}
}

// TestReachout463RecoversWhenTokenLands 463 恢复档的完整活闸：首条被拒后强制
// 增量+全量重拉 regular_high、轮询凭证；手机推送（这里由请求回调延时落地 token
// 仿真）在轮询窗口内到达 ⇒ 重试那一条真发出去，门面返回成功。
func TestReachout463RecoversWhenTokenLands(t *testing.T) {
	setReachoutClocks(t, 2*time.Second, 50*time.Millisecond)
	target := func() types.JID {
		j, err := waJID(reachFriend)
		if err != nil {
			t.Fatalf("收件人不可解析: %v", err)
		}
		return j
	}()
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireCreds = true
		f.onHistRequest = func() {
			time.AfterFunc(150*time.Millisecond, func() {
				f.setReachout(nil, map[types.JID][]byte{target: {1, 2, 3}})
			})
		}
	})
	if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err != nil {
		t.Fatalf("凭证在恢复档轮询窗口内落地后发送应成功: %v", err)
	}
	f := get()
	rhSeq, histReqs := f.reachState()
	if histReqs != 1 {
		t.Errorf("恢复档不应重复向手机要历史: histReqs=%d", histReqs)
	}
	// 快路 → 增量（门闸）→ 增量+全量（恢复档 force）。
	want := [][2]bool{{false, true}, {false, false}, {false, false}, {true, false}}
	if len(rhSeq) != len(want) {
		t.Fatalf("regular_high 序列应为 快路/增量/增量/全量: got=%v", rhSeq)
	}
	for i := range want {
		if rhSeq[i] != want[i] {
			t.Errorf("regular_high 第 %d 次入参应为 %v: got=%v", i+1, want[i], rhSeq[i])
		}
	}
	if _, _, _, sent, _ := f.snapshot(); len(sent) != 2 {
		t.Errorf("应为首条被拒+重试一条共两次发送尝试: got=%d", len(sent))
	}
}

// TestReachout463SaltPresentNoRetry 钉 :9/:10 真机形态：有盐（cstoken 已附上）仍被
// 463 拒 ⇒ 恢复档只等对方的 tctoken，等不到就**不重试**（重试同料只会再吃一个 463）。
// 盐在门闸阶段经历史请求回调落地 ⇒ 恢复档记 saltBefore=true，盐不算新凭证。
func TestReachout463SaltPresentNoRetry(t *testing.T) {
	setReachoutClocks(t, 2*time.Second, 50*time.Millisecond)
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireToken = true
		f.onHistRequest = func() {
			f.setReachout([]byte("salt-salt-salt-salt-salt-salt-32"), nil)
		}
	})
	_, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err == nil || !strings.Contains(err.Error(), "463") {
		t.Fatalf("有盐无 token 时仍应报 463: err=%v", err)
	}
	f := get()
	if _, _, _, sent, _ := f.snapshot(); len(sent) != 1 {
		t.Errorf("恢复档等不到对方 tctoken 就不该重试，只应发一次: sent=%d", len(sent))
	}
	if _, histReqs := f.reachState(); histReqs != 1 {
		t.Errorf("应已向手机要过一次历史: histReqs=%d", histReqs)
	}
}

// TestReachout463SaltLandingMidPollRetries 反向活闸：发送当时**没有盐**（首条是裸发），
// 轮询窗口内盐落了库 ⇒ cstoken 从无到有算新凭证，恢复档必须重试那一条。
func TestReachout463SaltLandingMidPollRetries(t *testing.T) {
	setReachoutClocks(t, 2*time.Second, 50*time.Millisecond)
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireToken = true
		time.AfterFunc(100*time.Millisecond, func() {
			f.setReachout([]byte("salt-salt-salt-salt-salt-salt-32"), nil)
		})
	})
	if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err == nil ||
		!strings.Contains(err.Error(), "463") {
		t.Fatalf("requireToken 形态下最终仍应 463: err=%v", err)
	}
	if _, _, _, sent, _ := get().snapshot(); len(sent) != 2 {
		t.Errorf("轮询期盐落地应触发一次重试（共两次发送尝试）: sent=%d", len(sent))
	}
}

// TestReachoutMissLedgerFailsFastAndRevives 落空账两态：等满一轮仍没凭证后，
// 窗口内第二条消息的恢复档不再重等（落空账时刻不变）；而 HistorySync 事件到达
// 作废落空账，第三条消息的恢复档重新等一轮（落空账时刻被推后）。
func TestReachoutMissLedgerFailsFastAndRevives(t *testing.T) {
	setReachoutClocks(t, 200*time.Millisecond, 50*time.Millisecond)
	m, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireCreds = true
	})

	_, err1 := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err1 == nil || !strings.Contains(err1.Error(), "463") {
		t.Fatalf("无凭证时首条应报 463: err=%v", err1)
	}
	w := wrapperOf(t, m, reachAccount)
	missAfterFirst := w.pollMissAt
	if missAfterFirst.IsZero() {
		t.Fatal("首条恢复档等满落空后应记落空账")
	}

	_, err2 := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err2 == nil || !strings.Contains(err2.Error(), "463") {
		t.Fatalf("落空账窗口内第二条仍应报 463: err=%v", err2)
	}
	if !w.pollMissAt.Equal(missAfterFirst) {
		t.Errorf("落空账窗口内且无新历史时不应重等: pollMissAt %v → %v", missAfterFirst, w.pollMissAt)
	}

	get().fire(&events.HistorySync{})
	_, err3 := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err3 == nil || !strings.Contains(err3.Error(), "463") {
		t.Fatalf("无凭证时第三条仍应报 463: err=%v", err3)
	}
	if !w.pollMissAt.After(missAfterFirst) {
		t.Errorf("HistorySync 到达后落空账应作废并重等一轮: pollMissAt 仍为 %v", w.pollMissAt)
	}
	if _, histReqs := get().reachState(); histReqs != 1 {
		t.Errorf("三轮恢复档都在历史请求窗口内，不应重发请求: histReqs=%d", histReqs)
	}
}
