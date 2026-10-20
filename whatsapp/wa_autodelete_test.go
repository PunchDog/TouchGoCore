package whatsapp

// 发信后清理的活闸：单聊落 LID、群组原样、批量逐条、清理失败不影响发送、
// 自聊/非聊天目标跳过，以及清理发生在防封闸释放之后。
// 清理语义是 clearChat（只清空消息）：不归档、不退群、不删好友，任何 patch 都不允许
// 出现 deleteChat（群聊天按 deleteChat 执行会退群）。全部走 seam 与假件，不联网。

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"

	"touchgocore/config"
	"touchgocore/util"
)

// assertNoDeleteOrArchive 断言一批 patch 里绝不出现 deleteChat 变异（退群根因）。
func assertNoDeleteOrArchive(t *testing.T, patches []appstate.PatchInfo) {
	t.Helper()
	for i, p := range patches {
		for j, m := range p.Mutations {
			if m.Index[0] == appstate.IndexDeleteChat {
				t.Fatalf("patch[%d].Mutations[%d] 出现 deleteChat（群聊会退群），Index=%v", i, j, m.Index)
			}
			if m.Index[0] == appstate.IndexArchive {
				t.Fatalf("patch[%d].Mutations[%d] 出现 archive（本批只清空、不归档），Index=%v", i, j, m.Index)
			}
		}
	}
}

// assertClearPatch 断言清空消息 patch：regular_high、Index 四段逐字、MessageRange 正确。
func assertClearPatch(t *testing.T, patch appstate.PatchInfo, wantTarget, wantMsgID string, wantTS time.Time, wantDeleteMedia string) {
	t.Helper()
	if patch.Type != appstate.WAPatchRegularHigh {
		t.Fatalf("清空 patch.Type=%v，期望 %v", patch.Type, appstate.WAPatchRegularHigh)
	}
	if len(patch.Mutations) != 1 {
		t.Fatalf("清空 patch 变异条数=%d，期望 1", len(patch.Mutations))
	}
	mut := patch.Mutations[0]
	if len(mut.Index) != 4 || mut.Index[0] != appstate.IndexClearChat ||
		mut.Index[1] != wantTarget || mut.Index[2] != "1" || mut.Index[3] != wantDeleteMedia {
		t.Fatalf("clearChat Index=%v，期望 [%s %s 1 %s]", mut.Index, appstate.IndexClearChat, wantTarget, wantDeleteMedia)
	}
	if mut.Version != 6 {
		t.Fatalf("clearChat Version=%d，期望 6", mut.Version)
	}
	mr := mut.Value.GetClearChatAction().GetMessageRange()
	if mr == nil {
		t.Fatal("清空 patch 缺 MessageRange")
	}
	if got := mr.GetLastMessageTimestamp(); got != wantTS.Unix() {
		t.Fatalf("清空时间戳=%d，期望 %d", got, wantTS.Unix())
	}
	msgs := mr.GetMessages()
	if wantMsgID == "" {
		if len(msgs) != 0 {
			t.Fatalf("无消息 id 时不应带消息键，实得 %v", msgs)
		}
		return
	}
	if len(msgs) != 1 {
		t.Fatalf("清空消息键条数=%d，期望 1", len(msgs))
	}
	key := msgs[0].GetKey()
	if key.GetRemoteJID() != wantTarget || key.GetID() != wantMsgID || !key.GetFromMe() {
		t.Fatalf("清空消息键不符: remote=%s id=%s fromMe=%v", key.GetRemoteJID(), key.GetID(), key.GetFromMe())
	}
}

// assertSingleClear 断言一次清理只产生 1 个 clearChat patch，且不含 deleteChat/archive。
func assertSingleClear(t *testing.T, patches []appstate.PatchInfo, wantTarget, wantMsgID string, wantTS time.Time) {
	t.Helper()
	if len(patches) != 1 {
		t.Fatalf("一次清理应只产生 1 个清空 patch，实得 %d", len(patches))
	}
	assertClearPatch(t, patches[0], wantTarget, wantMsgID, wantTS, "0")
	assertNoDeleteOrArchive(t, patches)
}

// TestSendMessageAutoClearsChat 单聊发信成功后自动清空消息：目标必须是发送时解析出的
// LID（与发送同款改写），消息键带刚发出的 id，清理不再二次查 LID。
func TestSendMessageAutoClearsChat(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	pn := jidOf(t, "447723171050@s.whatsapp.net")
	lid := jidOf(t, "54246208704608@lid")
	sendTS := time.Unix(1700000000, 0)
	s.mu.Lock()
	s.sendTS = sendTS
	s.lidMap = map[types.JID]types.JID{pn: lid}
	s.mu.Unlock()

	id, err := WhatsappSendMessage(nil, "8613800000000", pn.String(), "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("单聊发信: id=%q err=%v", id, err)
	}
	_, _, _, sent, patches := s.snapshot()
	if len(sent) != 1 {
		t.Fatalf("应只发一条，实得 %d", len(sent))
	}
	assertSingleClear(t, patches, lid.String(), "MSG-1", sendTS)

	s.mu.Lock()
	lidCalls := s.lidCalls
	s.mu.Unlock()
	if lidCalls != 1 {
		t.Fatalf("LID 解析应只发生 1 次（发送时），清理不得二次查询，实得 %d", lidCalls)
	}
}

// TestSendGroupMessageAutoClearsChat 群消息发信成功后自动清空消息：目标原样落群 JID，
// 绝不发 deleteChat（群里发 deleteChat 会被手机端按删群聊天执行而退群），也不归档、
// 不发 LID 查询。
func TestSendGroupMessageAutoClearsChat(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	const group = "1234567890@g.us"
	sendTS := time.Unix(1700000000, 0)
	s.mu.Lock()
	s.sendTS = sendTS
	s.mu.Unlock()

	id, err := WhatsappSendGroupMessage(nil, "8613800000000", group, "hi group")
	if err != nil || id != "MSG-1" {
		t.Fatalf("群发信: id=%q err=%v", id, err)
	}
	_, _, _, sent, patches := s.snapshot()
	if len(sent) != 1 || sent[0].to != group {
		t.Fatalf("群收件人不符: %+v", sent)
	}
	assertSingleClear(t, patches, group, "MSG-1", sendTS)

	s.mu.Lock()
	lidCalls := s.lidCalls
	s.mu.Unlock()
	if lidCalls != 0 {
		t.Fatalf("群组不应查 LID，实得 %d 次", lidCalls)
	}
}

// TestSendBatchAutoClearsPerSuccess 批内每条成功消息在广播 SendResult 前已清空消息
// （1 个 patch）；坏目标/空正文不产生 patch，整批不中止。
func TestSendBatchAutoClearsPerSuccess(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	cfg := &config.WhatsappSessionsConfig{AntibanEnabled: waBool(false), SendMaxPerHour: 30}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	pn := jidOf(t, "447723171050@s.whatsapp.net")
	lid := jidOf(t, "54246208704608@lid")
	s.mu.Lock()
	s.lidMap = map[types.JID]types.JID{pn: lid}
	s.mu.Unlock()

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(time.Duration) {}
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	var results []*SendResult
	var patchCounts []int
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		_, _, _, _, patches := s.snapshot()
		mu.Lock()
		results = append(results, r)
		patchCounts = append(patchCounts, len(patches))
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{
		{To: pn.String(), Body: "m1"},
		{To: "12345@g.us", Body: "m2"},
		{To: "abc", Body: "m3"},
		{To: "8613920000000", Body: ""},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "四条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 4
	})

	mu.Lock()
	gotResults := append([]*SendResult(nil), results...)
	gotCounts := append([]int(nil), patchCounts...)
	mu.Unlock()
	if len(gotResults) != 4 {
		t.Fatalf("SendResult 条数=%d", len(gotResults))
	}
	wantErr := []string{"", "", "没有任何数字", "消息内容为空"}
	for i, sub := range wantErr {
		if sub == "" && gotResults[i].Err != "" {
			t.Errorf("结果[%d].Err=%q，期望成功", i, gotResults[i].Err)
		}
		if sub != "" && gotResults[i].Err == "" {
			t.Errorf("结果[%d] 期望失败（含 %q），实得成功", i, sub)
		}
	}
	// 成功条在广播前已完成清空：前两条各 1 个 patch，后两条失败不产生 patch。
	wantCounts := []int{1, 2, 2, 2}
	for i, want := range wantCounts {
		if gotCounts[i] != want {
			t.Errorf("SendResult[%d] 广播时 patch 数=%d，期望 %d", i, gotCounts[i], want)
		}
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 2 {
		t.Fatalf("整批应有两个成功条目各产生 1 个清空 patch，实得 %d", len(patches))
	}
	assertNoDeleteOrArchive(t, patches)
}

// TestSendAutoCleanupFailureDoesNotFailSend 清理失败只记警告：消息已 ack 就仍返回 id+nil；
// 发送失败/空 id 时不得产生清理 patch。
func TestSendAutoCleanupFailureDoesNotFailSend(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	s.mu.Lock()
	s.stateErr = errors.New("no app state keys found, creating app state keys is not yet supported")
	s.mu.Unlock()

	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil || id != "MSG-1" {
		t.Fatalf("清理失败不得改变发送结果: id=%q err=%v", id, err)
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 1 {
		t.Fatalf("清空应尝试 1 次 SendAppState，实得 %d", len(patches))
	}
	assertNoDeleteOrArchive(t, patches)

	s.mu.Lock()
	s.stateErr = nil
	s.sendErr = errBoom
	s.mu.Unlock()
	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); id != "" || err == nil {
		t.Fatalf("发送失败应返回错误: id=%q err=%v", id, err)
	}
	if _, _, _, _, patches := s.snapshot(); len(patches) != 1 {
		t.Fatalf("发送失败不得产生清理 patch，实得 %d", len(patches))
	}

	s.mu.Lock()
	s.sendErr = nil
	s.sendID = ""
	s.mu.Unlock()
	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); id != "" || err == nil {
		t.Fatalf("空 id 应返回错误: id=%q err=%v", id, err)
	}
	if _, _, _, _, patches := s.snapshot(); len(patches) != 1 {
		t.Fatalf("空 id 不得产生清理 patch，实得 %d", len(patches))
	}
}

// TestSendAutoCleanupSkipsSelfAndNonChatTargets 发给自己（备忘录）、newsletter/broadcast
// 不属于「群/玩家会话」，发送成功也不清理。
func TestSendAutoCleanupSkipsSelfAndNonChatTargets(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	for _, to := range []string{"8613800000000", "123@newsletter", "status@broadcast"} {
		if id, err := WhatsappSendMessage(nil, "8613800000000", to, "hi"); err != nil || id != "MSG-1" {
			t.Fatalf("目标 %s 发送: id=%q err=%v", to, id, err)
		}
	}
	_, _, _, sent, patches := s.snapshot()
	if len(sent) != 3 {
		t.Fatalf("三条都应触达会话，实得 %d", len(sent))
	}
	if len(patches) != 0 {
		t.Fatalf("自聊/newsletter/broadcast 不应清理，实得 %d 个 patch", len(patches))
	}
}

// TestAutoCleanupRunsAfterGateUnlock 清空必须在防封闸的 gateMu 释放之后：
// 在 SendAppState 回调里 TryLock 成功才绿。把清理挪进 waSendText 的持锁段会当场变红。
func TestAutoCleanupRunsAfterGateUnlock(t *testing.T) {
	installFakeGate(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, &config.WhatsappSessionsConfig{AntibanEnabled: waBool(true)}, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	v, ok := m.clients.Load(dev.ID.String())
	if !ok {
		t.Fatal("账号未在册")
	}
	w, ok := v.(*waClientWrapper)
	if !ok {
		t.Fatalf("会话类型=%T", v)
	}
	s.mu.Lock()
	s.onState = func() {
		if !w.gateMu.TryLock() {
			t.Error("SendAppState 期间 gateMu 仍被持有（清理不应在防封闸临界区内）")
			return
		}
		w.gateMu.Unlock()
	}
	s.mu.Unlock()

	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil {
		t.Fatalf("发信失败: %v", err)
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 1 {
		t.Fatalf("应清空 1 次，实得 %d", len(patches))
	}
}
