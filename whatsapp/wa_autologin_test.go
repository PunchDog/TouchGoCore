package whatsapp

// 本文件是 T10（门面自动登录 + 联系人 app state 同步）的实现级用例：
// waManager.ensureLoggedIn 的三档自愈（不在册→按需启动、僵尸→复活、在册未连→等连上）、
// WhatsappSyncContacts 与 WhatsappListContacts 两段式的同步账（单飞、快路优先、
// 只在必要时升级 full=true、窗口限流），以及登录/同步阶段三种互斥失败文案。
//
// 全部走 seam，不联网、不连真 Postgres。helper 统一 al 前缀（同包防撞名）。
// 变异对照（每条注明「改坏生产哪一行会红」）写在各自用例的注释里，
// 与计划档 MA..MF 的编号一一对应。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"

	"touchgocore/config"
)

// setContactSyncWindow 临时改「全量重拉的最小间隔」这个注入点，用完还原。
// 窗口的两边都要有活闸：压到 1 小时证明「窗口内绝不擦版本」，压到 0 证明
// 「窗口外确实会升级 full=true」（不是把 full 恒关掉蒙过去）。
func setContactSyncWindow(t *testing.T, d time.Duration) {
	t.Helper()
	old := waContactSyncFullMinInterval
	waContactSyncFullMinInterval = d
	t.Cleanup(func() { waContactSyncFullMinInterval = old })
}

// alCountSyncs 读同步账并把 full 序列拼成可读文本（红信息要能直接看出序列不对）。
func alCountSyncs(t *testing.T, s *fakeSession) (calls, fullCalls int, seq string) {
	t.Helper()
	c, f, raw := s.countSyncs()
	parts := make([]string, 0, len(raw))
	for _, b := range raw {
		if b {
			parts = append(parts, "true")
		} else {
			parts = append(parts, "false")
		}
	}
	return c, f, strings.Join(parts, ",")
}

// alWantSeq 断言 full 调用序列逐字相符（顺序就是这条改动的语义本身）。
func alWantSeq(t *testing.T, s *fakeSession, want ...string) {
	t.Helper()
	_, _, got := alCountSyncs(t, s)
	wantStr := strings.Join(want, ",")
	if got != wantStr {
		t.Fatalf("SyncContactAppState 的 full 序列=[%s]，期望 [%s]", got, wantStr)
	}
}

// alContact 是「手机上的一个联系人」夹具条目。
func alContact(t *testing.T, jidStr, firstName string) (types.JID, types.ContactInfo) {
	t.Helper()
	return jidOf(t, jidStr), types.ContactInfo{FirstName: firstName}
}

// TestEnsureLoggedInStartsUnregisteredForList 设备行在库里、但从没人 StartClient
// （下游 `ensureClient` 空转的真实现场）：列好友必须自己把这个号拉起来、等到连上、
// 拉一次 app state，然后返回同步后的数据。
//
// 变异对照 MA：删掉 ensureLoggedIn 的 StartClient 分支 ⇒ liveWrapper 直接报
// 「没有在跑的会话」，本用例在 list 那一步即红。
func TestEnsureLoggedInStartsUnregisteredForList(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	// 配对码绑的号：在册之前一条联系人都没有（库里 contacts 是空的）。
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)

	// 一次都不显式启动：只调门面。
	list, err := WhatsappListContacts(nil, dev.ID.String())
	if err != nil {
		t.Fatalf("不在册的账号应被自动拉起后同步成功: %v", err)
	}
	if len(list) != 1 || list[0].JID != jid.String() || list[0].FirstName != "Alice" {
		t.Fatalf("同步后回读的好友列表不符: %+v", list)
	}
	if n := len(WhatsappListClients()); n != 1 {
		t.Fatalf("自动登录后在跑会话数=%d，期望 1（号确实被拉起来了）", n)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记（按需启动没走到 waSessionFactory）")
	}
	if calls, _, _ := alCountSyncs(t, s); calls != 1 {
		t.Fatalf("同步次数=%d，期望 1", calls)
	}
	if !s.LoggedIn() {
		t.Fatal("门面返回时账号仍未连上（ensureLoggedIn 没等到登录）")
	}
}

// TestEnsureLoggedInStartsUnregisteredForSend 同一条自愈对写门面生效：
// 「扫码的号能群发、配对码的号发不出去」的形态之一就是不显式启动直接发信。
//
// 变异对照 MA 的写侧对照：删掉 StartClient 分支 ⇒ 发送报「没有在跑」，本用例红。
func TestEnsureLoggedInStartsUnregisteredForSend(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, nil, dev)

	id, err := WhatsappSendMessage(nil, dev.ID.String(), "8613900000000", "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("不在册账号发信应自动登录后成功: id=%q err=%v", id, err)
	}
	if _, _, _, sent, _ := get(dev.ID.String()).snapshot(); len(sent) != 1 {
		t.Fatalf("会话上的发送记录=%+v，期望 1 条", sent)
	}
}

// TestEnsureLoggedInUnpairedKeepsNotRunningPhrase 库里连设备行都没有（从未配对）时，
// 自动启动失败的错误必须**同时**含「没有在跑」与「未配对」：前者是六条既有门面断言
// 钉着的稳定关键词，后者是唯一处置（去扫码）。
//
// 变异对照：把 ensureLoggedIn 的包装改成替换原文（丢掉「没有在跑」子串）⇒
// wa_client_test.go:147/375/730/780/861/943 六条同时红，本用例也红。
func TestEnsureLoggedInUnpairedKeepsNotRunningPhrase(t *testing.T) {
	startFakeWA(t, nil, nil) // 空设备库
	const acct = "8613711111111"

	if _, err := WhatsappSendMessage(nil, acct, "8613900000000", "hi"); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") || !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("未配对账号发信的错误串不符: %v", err)
	}
	if _, err := WhatsappListContacts(nil, acct); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") || !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("未配对账号列好友的错误串不符: %v", err)
	}
	if n, err := WhatsappSyncContacts(nil, acct, false); n != 0 || err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Fatalf("未配对账号同步联系人应被拒: n=%d err=%v", n, err)
	}
}

// TestRegisteredButDownWaitsAndSucceeds 在册但掉线（socket 掉了、库里还没派发事件）：
// 门面要催一轮重连并**等到连上**再取数，正治外部仓 ensureClient 的空转形态。
//
// 变异对照 MB：去掉 ensureLoggedIn 里的 reconnectAndAwait ⇒ 不催连、也不等，
// 门面在 loginCalls 仍是 1 时就越过自愈去取数，下面的 loginCalls 增量与耗时断言即红
// （另一侧的确定性对照见 TestRegisteredButDownWaitsBeforeSyncing）。
//
// 造「在册未连」的写法刻意避开 setDown 那一档竞态：常驻协程可能正卡在 waitLoggedIn 里
// （它只听 stop/downed/预算，**不听 kick**），只 setDown 时催连会白等满连接预算——
// wa_reconnect_test.go 钉着 setDown 形态的 TestSendKicksReconnectWhenSocketDown
// 就是本仓登记过的已知抖动。这里改成「首轮连接就失败」：协程必然停在 sleepBackoff，
// 而 sleepBackoff 是认 kick 令牌的，催连时机与状态因此确定。
func TestRegisteredButDownWaitsAndSucceeds(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	cfg := &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1, AntibanEnabled: waBool(false)}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) {
		s.loginErr = errBoom // 首轮连不上 ⇒ 协程停在退避睡眠里，等被 kick 催起
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首轮连接已尝试", func() bool { return s.countLogins() >= 1 })

	before := s.countLogins()
	s.mu.Lock()
	s.loginErr = nil // 下一轮 Login 会成功：此刻账号仍处在「在册未连」
	s.mu.Unlock()

	start := time.Now()
	list, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("在册未连的账号列好友应自愈成功: %v", err)
	}
	if len(list) != 1 || list[0].JID != jid.String() {
		t.Fatalf("取到的好友列表不符: %+v", list)
	}
	if got := s.countLogins(); got <= before {
		t.Fatalf("Login 调用数 %d → %d，门面没有催起重连（在册未连没被等连上）", before, got)
	}
	if !s.LoggedIn() {
		t.Fatal("返回时仍处在未登录态")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("自愈耗时 %s，催连没被退避睡眠接住", elapsed)
	}
}

// TestRegisteredButDownWaitsBeforeSyncing 同一档的反面：永远连不上时，
// 门面必须**先等连上、等不到就报错**，绝不带着一条没连上的 socket 去同步 app state。
//
// 这条是 MB 的确定性闸：把 reconnectAndAwait 摘掉 ⇒ 同步会在未登录态上照跑
// （syncCalls=1、错误也没有了），本用例两头断言同时红。
func TestRegisteredButDownWaitsBeforeSyncing(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	cfg := &config.WhatsappSessionsConfig{
		ConnectTimeoutSec: 1, ReconnectMinSec: 60, ReconnectMaxSec: 60, AntibanEnabled: waBool(false),
	}
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) {
		s.loginErr = errBoom
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首轮连接已尝试", func() bool { return s.countLogins() >= 1 })

	if list, err := WhatsappListContacts(nil, dev.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "未能连上") || len(list) != 0 {
		t.Fatalf("连不上时应报错且不交回数据: list=%+v err=%v", list, err)
	}
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 0 || fullCalls != 0 {
		t.Fatalf("未登录态上竟跑了同步（calls=%d full=%d），期望 0/0", calls, fullCalls)
	}
	if n, serr := WhatsappSyncContacts(nil, dev.ID.String(), false); n != 0 || serr == nil ||
		!strings.Contains(serr.Error(), "未能连上") {
		t.Fatalf("同步门面对未登录账号同样要报错: n=%d err=%v", n, serr)
	}
}

// TestListContactsSyncsWhenLocalEmpty 两段式的第二支：本地 0 条 ⇒ 走一次快路同步并返回
// 同步后的数据。这条是「通讯录=0 可发=0」的直接对照。
//
// 变异对照 MC 的反向：把两段式改成「无条件只读本地」⇒ 这里拿到空列表，红。
func TestListContactsSyncsWhenLocalEmpty(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	list, err := WhatsappListContacts(nil, dev.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].JID != jid.String() {
		t.Fatalf("本地空集时同步后的列表不符: %+v", list)
	}
	// 只走快路：full=false，绝不该擦版本。
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 1 || fullCalls != 0 {
		t.Fatalf("同步账 calls=%d full=%d，期望 1/0（本地空集只该走快路）", calls, fullCalls)
	}
}

// TestListContactsSkipsSyncWhenLocalHasData 两段式的第一支：本地已有联系人 ⇒ 直接返回，
// **一次同步都不做、不等登录**（保住 TestListContactsRevivesWithoutWaiting 的旧语义，
// 也让热路径每次群发不会都往服务器打一次快照）。
//
// 变异对照 MC：把两段式改成无条件同步 ⇒ syncCalls=1，本用例红。
func TestListContactsSkipsSyncWhenLocalHasData(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.contacts = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	start := time.Now()
	list, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].FirstName != "Bob" {
		t.Fatalf("本地命中却拿到: %+v", list)
	}
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 0 || fullCalls != 0 {
		t.Fatalf("本地已有联系人却同步了 %d 次（full=%d），期望 0/0", calls, fullCalls)
	}
	if elapsed >= time.Second {
		t.Fatalf("本地命中却耗时 %s（这条热路径不该有任何等待）", elapsed)
	}
}

// TestContactSyncSingleFlight 同账号 8 个并发请求只拉一次 app state：
// 拿锁后双检本地，后进者直接读本地结果。库侧 appStateSyncLock 只串行化、不去重，
// 不去重就会 8 次全量重拉互踩版本。
//
// 变异对照 MD：删掉 syncContactAppState 里的 s.mu.Lock/Unlock（单飞锁）⇒
// 多个 goroutine 同时看到本地 0 条，syncCalls>1，本用例红（-race 还会报 contacts 竞态）。
func TestContactSyncSingleFlight(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	// 先把号显式拉起来并等到连上：并发首调时多个 goroutine 会各自构造一份会话再
	// 丢掉输的那份（StartClient 既有形态），取件口此时可能指向被丢掉的那一条，
	// 会把「同步账」读成 0 次——那条路径由 TestEnsureLoggedInStartsUnregistered* 单独覆盖，
	// 这里要测的是同步单飞本身，必须只有一条会话可数。
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记")
	}
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	counts := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := WhatsappSyncContacts(context.Background(), dev.ID.String(), false)
			counts[i], errs[i] = n, err
		}(i)
	}
	wg.Wait()

	calls, fullCalls, seq := alCountSyncs(t, s)
	if calls != 1 {
		t.Fatalf("8 并发把同账号同步了 %d 次（full=%d, 序列=[%s]），期望单飞 1 次", calls, fullCalls, seq)
	}
	if fullCalls != 0 {
		t.Fatalf("快路却触发了全量重拉（full=%d）", fullCalls)
	}
	for i := range errs {
		if errs[i] != nil || counts[i] != 1 {
			t.Fatalf("并发结果[%d]不符: n=%d err=%v", i, counts[i], errs[i])
		}
	}
}

// TestContactFullSyncThrottledByWindow 「快路拉过但本地仍 0 条」才升级 full=true，
// 且升级受窗口约束：同一次调用里的序列必须是 [false, true]，第二次调用落在窗口内
// 就绝不再擦一次版本（full 次数不增）。
//
// 变异对照 ME：把 SyncContactAppState 的 full 实参恒传 true ⇒ 序列变成 [true, ...]，
// 第一条 alWantSeq 就红；把窗口判断删掉 ⇒ fullCalls 涨到 2，第二条断言红。
func TestContactFullSyncThrottledByWindow(t *testing.T) {
	setContactSyncWindow(t, time.Hour)
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	// 同步成功但补不进联系人：仿真「服务器快照本就为空」，逼出升级分支。
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	if _, err := WhatsappListContacts(nil, dev.ID.String()); err != nil {
		t.Fatalf("空快照不该报错: %v", err)
	}
	alWantSeq(t, s, "false", "true")
	calls, fullCalls, seq := alCountSyncs(t, s)
	if calls != 2 || fullCalls != 1 {
		t.Fatalf("首次列好友的同步账 calls=%d full=%d 序列=[%s]，期望 2/1/[false,true]", calls, fullCalls, seq)
	}

	// 第二次调用：快路照常可试，但升级落在窗口内 ⇒ 不再擦版本。
	if _, err := WhatsappListContacts(nil, dev.ID.String()); err != nil {
		t.Fatalf("第二次列好友不该报错: %v", err)
	}
	alWantSeq(t, s, "false", "true", "false")
	if calls, fullCalls, seq := alCountSyncs(t, s); fullCalls != 1 {
		t.Fatalf("窗口内又做了一次全量重拉：calls=%d full=%d 序列=[%s]，期望 full=1", calls, fullCalls, seq)
	}
}

// TestContactFullSyncAllowedWhenWindowPassed 窗口的另一侧：窗口放开后升级确实会执行
// （证明上一条不是「把 full 恒关掉」蒙过去的绿灯）。
//
// 变异对照：把升级分支（ListContacts 里 full=true 那一次调用）整段删掉 ⇒
// 这里的序列只有 [false, false]、fullCalls=0，红。
func TestContactFullSyncAllowedWhenWindowPassed(t *testing.T) {
	setContactSyncWindow(t, 0)
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	if _, err := WhatsappListContacts(nil, dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	alWantSeq(t, s, "false", "true")
	if _, fullCalls, _ := alCountSyncs(t, s); fullCalls != 1 {
		t.Fatalf("窗口放开时升级没执行：full=%d，期望 1", fullCalls)
	}
	// 第二次不再触发升级（本地 0 条会再试一次快路 + 一次升级），窗口已放开 ⇒ full 涨到 2。
	if _, err := WhatsappListContacts(nil, dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	alWantSeq(t, s, "false", "true", "false", "true")
}

// TestSyncContactsFacadeCountsAndThrottles 新门面本身：返回的是同步后回读的条数；
// 显式 full=true 首次立刻执行（擦版本重拉），第二次落在窗口内退化成快路。
//
// 变异对照：把 WhatsappSyncContacts 的返回值写成同步前的条数 ⇒ 第一条 n=0 红；
// 把窗口判断去掉 ⇒ 第二次仍 full=true，full 计数红。
func TestSyncContactsFacadeCountsAndThrottles(t *testing.T) {
	setContactSyncWindow(t, time.Hour)
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	a, ia := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	b, ib := alContact(t, "442071234568@s.whatsapp.net", "Beno")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{a: ia, b: ib}
	}, dev)

	n, err := WhatsappSyncContacts(nil, dev.ID.String(), true)
	if err != nil || n != 2 {
		t.Fatalf("显式全量同步: n=%d err=%v，期望 2 条", n, err)
	}
	s := get(dev.ID.String())
	alWantSeq(t, s, "true")

	// 显式 full=true 落在窗口内：不再擦版本，退化成快路（快路此时零网络）。
	n, err = WhatsappSyncContacts(nil, dev.ID.String(), true)
	if err != nil || n != 2 {
		t.Fatalf("窗口内的显式同步: n=%d err=%v", n, err)
	}
	alWantSeq(t, s, "true", "false")
	if _, fullCalls, _ := alCountSyncs(t, s); fullCalls != 1 {
		t.Fatalf("窗口内又擦了一次版本：full=%d，期望 1", fullCalls)
	}
}

// TestSyncContactsReportsKeyNotFound 密钥没共享时（配对码绑的号最常见的那一档）：
// 必须单独点名「密钥」，保留 %w 可判定，且**绝不返回「0 条 + nil」**。
//
// 变异对照 MF：把同步失败退化成「记日志后返回空集 + nil」⇒ 这里 err==nil 立刻红。
func TestSyncContactsReportsKeyNotFound(t *testing.T) {
	oldWait := waAppStateKeyWait
	waAppStateKeyWait = 0
	t.Cleanup(func() { waAppStateKeyWait = oldWait })

	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	keyErr := fmt.Errorf("failed to decode app state %s patches: %w",
		appstate.WAPatchCriticalUnblockLow, appstate.ErrKeyNotFound)
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncErr = keyErr }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	n, err := WhatsappSyncContacts(nil, dev.ID.String(), false)
	if n != 0 || err == nil {
		t.Fatalf("密钥缺失必须报错且不给条数: n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("错误文案要点名密钥问题: %v", err)
	}
	if !errors.Is(err, appstate.ErrKeyNotFound) {
		t.Fatalf("必须保留 %%w 让下游可判定: %v", err)
	}
	if strings.Contains(err.Error(), "上下文已结束") {
		t.Fatalf("密钥缺失不许被折成上下文已结束: %v", err)
	}

	// 列好友同一条纪律：同步失败要报错，不能交回空列表冒充「没好友」。
	if list, lerr := WhatsappListContacts(nil, dev.ID.String()); lerr == nil || len(list) != 0 ||
		!strings.Contains(lerr.Error(), "密钥") {
		t.Fatalf("列好友遇密钥缺失时必须报错: list=%+v err=%v", list, lerr)
	}
}

// TestSyncContactsWaitsForAppStateKey 第一次解码缺密钥、手机随后把密钥补上时，
// 同步必须等到联系人落库再返回，而不是把第一次的 ErrKeyNotFound 直接交给群发。
func TestSyncContactsWaitsForAppStateKey(t *testing.T) {
	oldWait := waAppStateKeyWait
	waAppStateKeyWait = 3 * time.Second
	t.Cleanup(func() { waAppStateKeyWait = oldWait })

	dev := fakeDevice(t, "447378152319:2@s.whatsapp.net", "android", "P")
	friend := jidOf(t, "447700900001@s.whatsapp.net")
	keyErr := fmt.Errorf("failed to decode app state %s patches: %w",
		appstate.WAPatchCriticalUnblockLow, appstate.ErrKeyNotFound)
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncErr = keyErr
		s.syncAdds = map[types.JID]types.ContactInfo{
			friend: {FullName: "One Friend"},
		}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	go func() {
		time.Sleep(200 * time.Millisecond)
		s.setSync(nil, map[types.JID]types.ContactInfo{
			friend: {FullName: "One Friend"},
		})
	}()

	n, err := WhatsappSyncContacts(nil, dev.ID.String(), false)
	if err != nil || n != 1 {
		t.Fatalf("密钥补上后应同步到 1 个好友: n=%d err=%v", n, err)
	}
	list, lerr := WhatsappListContacts(nil, dev.ID.String())
	if lerr != nil || len(list) != 1 || list[0].JID != friend.String() {
		t.Fatalf("列好友应直接读到已落库的 1 条: %+v err=%v", list, lerr)
	}
}

// TestListContactsLoginTimeoutIsNamed 连不上时的文案是「未能连上」，且不含
// 「上下文已结束」——两条串互斥，值班才不会被引去查请求超时。
//
// 变异对照：把 ensureLoggedIn 的 reconnectAndAwait 换成「不等直接返回」⇒
// 这里拿不到「未能连上」（同步会在未登录态上跑），本用例红。
func TestListContactsLoginTimeoutIsNamed(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	cfg := &config.WhatsappSessionsConfig{ConnectTimeoutSec: 1, ReconnectMinSec: 60, ReconnectMaxSec: 60}
	// 压根不在册 + 永远连不上：走 ensureLoggedIn 的按需启动，再撞连接预算。
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) { s.loginErr = errBoom }, dev)

	start := time.Now()
	_, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "未能连上") {
		t.Fatalf("连接预算到点应报「未能连上」: %v", err)
	}
	if strings.Contains(err.Error(), "上下文已结束") {
		t.Fatalf("两条串必须互斥，实得 %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("等待没夹在 1s 预算内：耗时 %s", elapsed)
	}
	if get(dev.ID.String()) == nil {
		t.Fatal("按需启动没建出会话（错误路径也不该留下半截状态则见上一条用例）")
	}
}

// TestListContactsCanceledIsNamed 调用方 ctx 已结束时报「上下文已结束（…本条未执行）」，
// 且不含「未能连上」；等待当场撒手，不烧完连接预算。
//
// 变异对照：把 waLoginErr 的 context.Canceled 折算删掉 ⇒ 第一条断言红
// （错误只剩库那串，值班会照字面去查号没注册）。
func TestListContactsCanceledIsNamed(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	cfg := &config.WhatsappSessionsConfig{ConnectTimeoutSec: 15, ReconnectMinSec: 60, ReconnectMaxSec: 60}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) { s.loginErr = errBoom }, dev)

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := WhatsappListContacts(cctx, dev.ID.String())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "上下文已结束") ||
		!strings.Contains(err.Error(), "本条未执行") {
		t.Fatalf("取消类失败要点名「上下文已结束/本条未执行」: %v", err)
	}
	if strings.Contains(err.Error(), "未能连上") {
		t.Fatalf("两条串必须互斥，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "被取消") {
		t.Fatalf("原始等待错误要留在链上: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("ctx 已取消却仍等了 %s（该当场撒手）", elapsed)
	}
	_ = get(dev.ID.String())
}

// TestSyncContactsFailClosedWhenNotStarted 未启动时新门面就地拒绝、返回 0 条，
// 与 wa_client_test.go 的 TestFacadesFailClosedWhenNotStarted 同一条 fail-closed 口径
// （那条用例本身是禁改面，这里补上新增门面对照）。
//
// 变异对照：把 currentWA 的判空去掉 ⇒ nil 管理器解引用 panic，本用例直接崩。
func TestSyncContactsFailClosedWhenNotStarted(t *testing.T) {
	WhatsappStop(nil)
	n, err := WhatsappSyncContacts(nil, "8613800000000", true)
	if err == nil || n != 0 {
		t.Fatalf("未启动时同步联系人应当被拒: n=%d err=%v", n, err)
	}
	if got := WhatsappListClients(); got != nil {
		t.Fatalf("未启动时会话列表应为 nil，实得 %+v", got)
	}
}
