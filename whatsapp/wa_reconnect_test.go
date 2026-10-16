package whatsapp

// 本文件是发信面「断开先自愈」的活闸（wa_manager.go 的 ensureLoggedIn/liveWrapper/
// waitConnected，消费点在 wa_client.go 的 WhatsappSendMessage / WhatsappSendGroupMessage）。
//
// 钉住的四件事：
//   1. 会话只是掉线（常驻协程还活着）⇒ 催一次立即重连，连上就发，不烧退避；
//   2. 会话已死透（logged_out 落标记、常驻协程退出，但死对象还占着 key）⇒ 摘掉重建再用；
//   3. 重建时设备行已经没了 ⇒ 明确拒绝并把处置说清楚，绝不发信；
//   4. 等待有上限且认调用方 ctx ⇒ 不把发信请求挂在无限等待上。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"touchgocore/config"
	"touchgocore/util"
)

// waitWrapperExited 等某个账号的常驻协程收手（wrapper 的 done 关闭）。
// 死对象仍挂在 clients 表里正是自愈要治的形态，所以这里不能拿「key 不在册」当判据。
func waitWrapperExited(t *testing.T, m *waManager, key string) {
	t.Helper()
	waitUntil(t, 2*time.Second, "常驻协程退出", func() bool {
		v, ok := m.clients.Load(key)
		if !ok {
			return true
		}
		select {
		case <-v.(*waClientWrapper).done:
			return true
		default:
			return false
		}
	})
}

// TestSendKicksReconnectWhenSocketDown 掉线窗口里发信：自愈链必须把常驻协程从
// 「守着一条自以为还在的连接」的状态里催醒去重连，连上后把这条发出去。
//
// 变异对照：把 liveWrapper 里的 kickNow 摘掉，run() 会一直卡在 waitUntilDown，
// waitConnected 烧满 15 秒预算后报「未能连上」，本用例即红。
func TestSendKicksReconnectWhenSocketDown(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	_, _, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	before := s.countLogins()
	s.setDown()

	id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("掉线后发信应当先重连再发出去: id=%q err=%v", id, err)
	}
	if got := s.countLogins(); got <= before {
		t.Fatalf("Login 调用数 %d → %d，发信没有催起重连", before, got)
	}
	if !s.LoggedIn() {
		t.Fatal("发信后仍处在未登录态")
	}
	_, _, _, sent, _ := s.snapshot()
	if len(sent) != 1 || sent[0].body != "hi" {
		t.Fatalf("自愈之后这条没落到会话上: %+v", sent)
	}
}

// TestSendRevivesZombieWrapper 吊销后的死对象不能挡住发信：logged_out 让常驻协程退出，
// 但 wrapper 还挂在 clients 表里，StartClient 的幂等分支会原样端出这具死对象。
// 发信面必须把它摘掉、按库里的设备行重建一份新会话，再在新会话上发。
func TestSendRevivesZombieWrapper(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	old := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", old.LoggedIn)

	// 库派发吊销事件：dispatch 当场落 loggedOut 标记，常驻协程随即收手（done 关闭）。
	old.fire(events.LoggedOut{OnConnect: false})
	waitWrapperExited(t, m, dev.ID.String())

	id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("死会话应被重建后发信成功: id=%q err=%v", id, err)
	}

	fresh := get(dev.ID.String())
	if fresh == old {
		t.Fatal("发信复用着那具已退出的 wrapper，没有重建")
	}
	if _, _, _, oldSent, _ := old.snapshot(); len(oldSent) != 0 {
		t.Fatalf("死会话上竟有 %d 条发送记录", len(oldSent))
	}
	if _, _, _, newSent, _ := fresh.snapshot(); len(newSent) != 1 {
		t.Fatalf("新会话发送记录数=%d，期望 1", len(newSent))
	}
}

// TestSendRefusesWhenRowGone 死会话重建时设备行已经不在了（已解绑/从未配对）：
// 必须拒绝并把「先扫码」的处置写在错误里，一条都不发。
func TestSendRefusesWhenRowGone(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, st, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	s.fire(events.LoggedOut{})
	// 真删行：等价于后台点了解绑（WhatsappDeleteDevice）之后再来一条发信请求。
	if err := st.DeleteDevice(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	// StopClient 会摘 key，这里不经它：直接停掉在跑的会话会让 liveWrapper 走
	// 「没有在跑的会话」那一支，测不到重建分支。等旧协程自己退出后 key 仍在册。
	waitWrapperExited(t, m, dev.ID.String())

	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err == nil ||
		!strings.Contains(err.Error(), "重登失败") || !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("行已删除时应拒绝并指向扫码，实得 %v", err)
	}
	if _, _, _, sent, _ := s.snapshot(); len(sent) != 0 {
		t.Fatalf("拒绝之后仍有 %d 条落到会话上", len(sent))
	}
	if n := m.countClients(); n != 0 {
		t.Fatalf("重建失败后不该留下在跑会话，实得 %d", n)
	}
}

// TestListGroupsReconnectsBeforeQuery 列可发言群组要向服务器实时查询，旧形态在断线时
// 直接抛「websocket not connected」，把后台群发卡死在这一步（现场日志实证）。
// 现在必须先自愈再查：断了就催连并等连上。
func TestListGroupsReconnectsBeforeQuery(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	_, _, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	before := s.countLogins()
	s.setDown()
	if _, err := WhatsappListSpeakableGroups(nil, "8613800000000@s.whatsapp.net"); err != nil {
		t.Fatalf("断线时列群组应先重连再查，实得 %v", err)
	}
	if got := s.countLogins(); got <= before {
		t.Fatalf("Login 调用数 %d → %d，列群组没有触发自愈", before, got)
	}
}

// TestDeleteChatReconnectsBeforeQuery 删会话框同样是要在线才办得成的动作，口径与发信一致。
func TestDeleteChatReconnectsBeforeQuery(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	_, _, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	before := s.countLogins()
	s.setDown()
	if err := WhatsappDeleteChat(nil, "8613800000000@s.whatsapp.net", "8613900000000@s.whatsapp.net", "", time.Time{}, false); err != nil {
		t.Fatalf("断线时删框应先重连，实得 %v", err)
	}
	if got := s.countLogins(); got <= before {
		t.Fatalf("Login 调用数 %d → %d，删框没有触发自愈", before, got)
	}
}

// TestBatchHealsEveryItem 批跑到一半掉线时，剩余条目要逐条自愈而不是全部撞在同一个断口上：
// 假会话在第 1 条发出后就把登录态按回 false，第 2 条必须靠 reconnectAndAwait 重新连上。
func TestBatchHealsEveryItem(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	// 防封闸显式关掉：配了 sessions 段就等于开防封（EffectiveAntibanEnabled 的 nil 三态
	// 读作 true），闸门会给首条 15 秒延迟，把这条用例的节奏整个吞掉。
	cfg := &config.WhatsappSessionsConfig{
		SendMinDelaySec: 1, SendMaxDelaySec: 1, AntibanEnabled: waBool(false),
	}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) { s.downAfterSends = 1 }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	var mu sync.Mutex
	var results []*SendResult
	rid := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", rid) })

	if err := WhatsappSendBatchAsync(context.Background(), "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "a"},
		{To: "8613900000001", Body: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 8*time.Second, "两条 SendResult 都落账", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	})
	mu.Lock()
	last := append([]*SendResult(nil), results...)
	mu.Unlock()
	if last[0].MsgID == "" || last[1].MsgID == "" {
		t.Fatalf("两条都应发出去，实得 %+v", last)
	}
	if got := s.countLogins(); got < 2 {
		t.Fatalf("Login 调用数=%d，期望 ≥2（第 2 条之前应催过一次重连）", got)
	}
}

// TestListContactsRevivesWithoutWaiting 查好友读的是本地会话库：死对象要复活
// （库把子存储换成了 NoopStore），但不许等登录建立——这条假件永远连不上，
// 若误用了等连接的分支，用例会烧满 5 秒预算并报「未能连上」。
func TestListContactsRevivesWithoutWaiting(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	other := jidOf(t, "8613900000000@s.whatsapp.net")
	cfg := &config.WhatsappSessionsConfig{ConnectTimeoutSec: 5}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) {
		s.loginErr = errBoom
		s.contacts = map[types.JID]types.ContactInfo{other: {FirstName: "F"}}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	old := get(dev.ID.String())
	// 吊销标记在 dispatch 里当场落位（同步于这次 fire 调用），retired() 即刻为真；
	// 不必再等常驻协程真的睡完一轮退避——复活由 ensureAlive 触发，它会先摘死对象。
	old.fire(events.LoggedOut{})

	start := time.Now()
	list, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("复活后的本地读应当直接成功，实得 %v", err)
	}
	if len(list) != 1 || list[0].JID != other.String() {
		t.Fatalf("好友列表不符: %+v", list)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("查好友竟等了 %s（本地读不该等登录）", elapsed)
	}
	if get(dev.ID.String()) == old {
		t.Fatal("死对象没有被复活")
	}
}

// TestSendContextCanceledIsNamed 调用方 ctx 已结束时，错误必须点名「上下文已结束」，
// 而不是把库那串「failed to get LID for PN ...: context canceled」原样递出去——
// 后者看起来像号码没注册/LID 查不到，值班会照字面去查号码，而真因是请求早就断了。
func TestSendContextCanceledIsNamed(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	_, _, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := WhatsappSendMessage(cctx, "8613800000000", "8613900000000", "hi")
	if err == nil || !strings.Contains(err.Error(), "调用方上下文已结束") ||
		!strings.Contains(err.Error(), "本条未发出") {
		t.Fatalf("取消类错误要单独点名，实得 %v", err)
	}
	if _, _, _, sent, _ := s.snapshot(); len(sent) != 0 {
		t.Fatalf("上下文已结束却仍递出了 %d 条", len(sent))
	}
}

// TestGateSleepHonorsContext 防封延迟可达数秒到十几秒：只 time.Sleep 的话，请求断了
// 这里还在睡，醒来又往已取消的 ctx 上发一条注定失败的发送。睡眠必须可被打断，
// 且打断要记进闸的失败账（AfterSendFailed），不然 Health 看不到这一格。
func TestGateSleepHonorsContext(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	fg := installFakeGate(t)
	fg.delayMs = 40_000
	cfg := &config.WhatsappSessionsConfig{AntibanEnabled: waBool(true)}
	_, _, get := startFakeWA(t, cfg, nil, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := WhatsappSendMessage(cctx, "8613800000000", "8613900000000", "hi")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "调用方上下文已结束") {
		t.Fatalf("应报上下文已结束，实得 %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("没有打断 %dms 的防封睡眠，耗时 %s", fg.delayMs, elapsed)
	}
	fg.mu.Lock()
	failed := fg.failed
	fg.mu.Unlock()
	if failed != 1 {
		t.Fatalf("AfterSendFailed 记账=%d，期望 1（被打断的发送要进 Health 的失败信号）", failed)
	}
	if _, _, _, sent, _ := s.snapshot(); len(sent) != 0 {
		t.Fatalf("睡眠被打断后不应发出，实得 %d 条", len(sent))
	}
}

// TestSendWaitsBoundedAndHonorsCtx 连不上时不能无限等：预算取 EffectiveConnectTimeout，
// 并且认调用方的 ctx。
func TestSendWaitsBoundedAndHonorsCtx(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	cfg := &config.WhatsappSessionsConfig{ConnectTimeoutSec: 1, ReconnectMinSec: 60, ReconnectMaxSec: 60}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) { s.loginErr = errBoom }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	start := time.Now()
	_, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "未能连上") {
		t.Fatalf("连不上应报「未能连上」，实得 %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("等待没有夹在预算内：耗时 %s（预算 1s）", elapsed)
	}
	// 预算内确实催过重连（而不是干等到超时）：StartClient 起首轮 1 次 + 发信催的至少 1 次。
	if got := s.countLogins(); got < 2 {
		t.Fatalf("Login 调用数=%d，期望 ≥2（发信催起重连才是这条路的本意）", got)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = WhatsappSendMessage(cctx, "8613800000000", "8613900000000", "hi"); err == nil ||
		!strings.Contains(err.Error(), "被取消") {
		t.Fatalf("ctx 已取消时应即刻撒手并报「被取消」，实得 %v", err)
	}
}
