package whatsapp

// 本文件钉住 wa_manager.go 的 waitLoggedIn 必须听 w.kick 这一条时序契约。
//
// 缺陷形态（wa_reconnect_test.go 的 TestSendKicksReconnectWhenSocketDown /
// TestListGroupsReconnectsBeforeQuery / TestDeleteChatReconnectsBeforeQuery 的偶红，
// 在 HEAD 纯净快照上 -count=12 -race 已复现，红形一律是 15.00s 到点）：
//   常驻协程卡在 waitLoggedIn（socket 已连上、登录回执还没落定）时，
//   门面 reconnectAndAwait→kickNow 催的那一次立即重连**没人接**——那支 select 只监听
//   stop/downed/ctx.Done/deadline/tick。于是门面的 waitConnected 计时器与库侧 waitLoggedIn
//   计时器同时起跑、互相赛跑，门面白等满 EffectiveConnectTimeout 后报「未能连上」。
//   红证据里成对出现的两行日志正是这条赛跑的两端：
//     --- FAIL: ... 在 15s 内未能连上
//     [WARN] 连接后 15s 内未完成登录（3s 后重试）
//
// 修法语义：waitLoggedIn 加一支 `case <-w.kick: return false`——「被催即弃本轮半开连接」，
// 让 run() 走既有的 `Disconnect + 退避重连` 路径，而不是让门面去跟一个不认 kick 的等待比谁先到点。
//
// 三条用例各钉一件事：
//   1. TestKickDuringLoginPendingReconnects —— 被催即弃连、退避后第二轮登录落定、门面在预算内成功；
//   2. TestKickDuringLoginPendingNoGoroutineLoss —— 三态（弃连/正常连上/停机）跑完协程归零、不留残余令牌；
//   3. TestKickNotConsumedWhenAlreadyLoggedIn —— 反向死闸：已登录态的门面不得产生「弃连」，
//      证明新分支只落在 waitLoggedIn，没有把 waitUntilDown 的语义误搬进来。
//
// 造窗口靠 wa_manager_test.go 里 fakeSession 的三个挂点（holdLoginPending / releaseAfter /
// loginSettleDelay），全部默认零值 ⇒ 既有假件行为逐字不变；不新增全局挂点。
//
// 全部走 seam，不联网、不连真 Postgres。

import (
	"sync"
	"testing"
	"time"

	"touchgocore/config"
)

// 赛跑参数（记号：T=klConnectTimeout、d=klLoginSettle、B=klBackoff、
// t0=首轮 Login 返回那一刻、t1=门面调用时刻、gap=t1-t0、ε=常驻协程串下一条连接的固有开销）。
//
// 关键前提：**T 是两边共用的同一个键**（门面 waitConnected 与库侧 waitLoggedIn 都读
// cfg.EffectiveConnectTimeout），而库侧永远比门面早起跑（t0 < t1）。所以「没修时门面先到点」
// 不能靠预算差造，只能靠链路耗时造：
//
//   - 没修：kick 没人接，令牌一直躺在缓冲通道里；库侧等自己那台 T 到点（t0+T）才去 Disconnect，
//     随后 sleepBackoff 被那个残留令牌**当场**叫醒（所以中间段是 ε 而不是 B——真凶是
//     「没人接的令牌不会丢，只是在下一轮 sleepBackoff 里被白捡一次」），接着第二轮 Login
//     还要花 d 才把登录态落定：登录落在 t0+T+ε+d，门面到点在 t1+T ⇒ **红 ⟺ d + ε > gap**。
//     klMaxGap 把 gap 夹到远小于 d ⇒ 红余量是几百毫秒级，而不是运气级。
//   - 修好：kick 被 waitLoggedIn 当场接走（通道随之清空，退避因此要真走满 B）⇒
//     登录落在 t1+B+d，门面到点在 t1+T ⇒ **绿 ⟺ B + d < T**，本文件取余量 T-B-d=1350ms
//     （再算上 100ms 轮询粒度仍有 1.25s）。
//
// B 靠 m.afterFor 注入而不是 cfg 的秒级配置：cfg 只能配整秒，1s 退避正好把绿余量吃光。
const (
	klConnectTimeout = 2 * time.Second
	klLoginSettle    = 600 * time.Millisecond
	klBackoff        = 50 * time.Millisecond
	klMaxGap         = 200 * time.Millisecond // 红余量下限 = d - klMaxGap = 400ms
	klStableWindow   = 200 * time.Millisecond
)

// klCfg 是三条用例的配置：连接预算压到秒级（绝不用缺省 15s 拖测试），防封闸显式关
// （配了 sessions 段就等于开，闸门会给首条十几秒延迟，把节奏整个吞掉）。
func klCfg() *config.WhatsappSessionsConfig {
	return &config.WhatsappSessionsConfig{
		ConnectTimeoutSec: 2,
		ReconnectMinSec:   1,
		ReconnectMaxSec:   1,
		AntibanEnabled:    waBool(false),
	}
}

// klClock 是退避时钟的注入件：忽略请求值、固定睡 delay，并记录每轮请求的时长。
type klClock struct {
	mu        sync.Mutex
	delay     time.Duration
	requested []time.Duration
}

func (c *klClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.requested = append(c.requested, d)
	c.mu.Unlock()
	return time.After(c.delay)
}

// rounds 读这一轮跑下来一共被要求睡过几次退避。
func (c *klClock) rounds() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requested)
}

// mustWrapper 取 clients 表里那具 wrapper（不在册即判前置不成立）。
// kick 用例钉的是「同一条常驻协程被催醒」，所以对象身份本身也是断言的一部分
//（复活重建会换对象，那种形态归 wa_reconnect_test.go）。
func mustWrapper(t *testing.T, m *waManager, key string) *waClientWrapper {
	t.Helper()
	v, ok := m.clients.Load(key)
	if !ok {
		t.Fatalf("前置不成立：账号 %s 不在 clients 表里", key)
	}
	return v.(*waClientWrapper)
}

// klPendingKicks 读 kick 通道里还没被消费的令牌数（len 对通道是合法的非破坏性观测）。
func klPendingKicks(w *waClientWrapper) int { return len(w.kick) }

// waitManagerWG 等管理器上的全部常驻协程归零（每 Client 一条 run + 每配对会话一条 pump）。
// 停机后 clients 已被摘，waitWrapperExited 对「不在册」直接判真，所以这里必须看 wg 本身。
func waitManagerWG(m *waManager, budget time.Duration) bool {
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

// klAssertWindowOpen 是双层硬闸：把「空过」和「失去判别力」都变成响亮失败，
// 而不是让用例绿着过去。
//
// 第一层（窗口真造住了）：Login 恰 1 次、仍未登录、协程没退出、kick 通道还是空的。
// 库侧若已经自己到点重连过，前两条当场失败——这两条同时也是「kick 会落在 waitLoggedIn 里」
// 的直接取证，不靠猜时序。
// 第二层（本用例真有判别力）：gap 必须远小于 d，否则没修时那次「到点即弃连」的重连
// 会赶在门面到点之前落定，用例退化成修没修都绿。
func klAssertWindowOpen(t *testing.T, s *fakeSession, w *waClientWrapper, gap time.Duration) {
	t.Helper()
	if got := s.countLogins(); got != 1 {
		t.Fatalf("前置不成立：门面还没催就已经有 %d 轮 Login，半开窗口没造住"+
			"（waitLoggedIn 早于门面到点自己撒过手，此时催不催连没区别）", got)
	}
	if s.LoggedIn() {
		t.Fatal("前置不成立：假会话已进入登录态，holdLoginPending 挂点没生效，waitLoggedIn 没在被卡")
	}
	select {
	case <-w.done:
		t.Fatal("前置不成立：常驻协程已退出，门面等的是 done 分支而不是 waitLoggedIn")
	default:
	}
	if got := klPendingKicks(w); got != 0 {
		t.Fatalf("前置不成立：kick 通道上已有 %d 个残留令牌，催连对象会被抢先接走", got)
	}
	if gap > klMaxGap {
		t.Fatalf("窗口位置不成立：从首轮 Login 到门面调用隔了 %s，超过 %s（一轮连接耗时 %s 的三分之一）——"+
			"没修时那次重连会赶在门面到点前落定，本用例失去判别力", gap, klMaxGap, klLoginSettle)
	}
}

// klStartPending 把「常驻协程正卡在 waitLoggedIn」做成一个可复核的稳态：
// 起会话 → 等首轮 Login 真发生（这一次调用没发生，后面所有断言都是空过）→ 当场复核窗口。
// 计时起点取在首轮 Login 返回之后：那正是库侧那台 waitLoggedIn 计时器起跑的时刻。
func klStartPending(t *testing.T, m *waManager, get func(string) *fakeSession, key string) (*fakeSession, *waClientWrapper) {
	t.Helper()
	if err := WhatsappStartClient(key); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	s := get(key)
	if s == nil {
		t.Fatalf("前置不成立：按 key=%s 取不到假会话", key)
	}
	waitUntil(t, 3*time.Second, "首轮 Login 发生", func() bool { return s.countLogins() >= 1 })
	t0 := time.Now()
	w := mustWrapper(t, m, key)
	klAssertWindowOpen(t, s, w, time.Since(t0))
	return s, w
}

// TestKickDuringLoginPendingReconnects 常驻协程卡在 waitLoggedIn 时被门面催：
// 必须「被催即弃本轮半开连接」，走既有 Disconnect + 退避重连，第二轮登录落定后
// 门面的等待在预算内成功返回。
//
// 判别对照（本批的核心红）：
//   - 把 wa_manager.go 里 waitLoggedIn 的 `case <-w.kick` 那一支注释掉 ⇒ 本用例红，
//     红信息是门面那侧的「…在 2s 内未能连上」（kick 没人接，两条计时器赛跑）；
//   - 若把那一支写成 `return true`（拿半开连接冒充已登录）⇒ run() 会守到 waitUntilDown
//     并且永不重连，登录态永远不落定，本用例同样红——红的方向锁死在「弃连」这一语义上。
func TestKickDuringLoginPendingReconnects(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	key := dev.ID.String()
	clock := &klClock{delay: klBackoff}
	m, _, get := startFakeWA(t, klCfg(), func(s *fakeSession) {
		s.holdLoginPending = true
		s.releaseAfter = 2 // 只有被催之后重连的那一轮才真正确认登录
		s.loginSettleDelay = klLoginSettle
	}, dev)
	m.afterFor = clock.after

	s, w := klStartPending(t, m, get, key)

	id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("被催即弃连后应在 %s 预算内连上并发出去: id=%q err=%v", klConnectTimeout, id, err)
	}
	if got := s.countLogins(); got != 2 {
		t.Fatalf("Login 调用数=%d，期望 2（首轮被弃 + 第二轮真连）", got)
	}
	if !s.LoggedIn() {
		t.Fatal("门面返回时仍处在未登录态")
	}
	if _, disconnects, _, _, _ := s.snapshot(); disconnects != 1 {
		t.Fatalf("弃本轮半开连接应恰好断开一次，实得 %d", disconnects)
	}
	if n := clock.rounds(); n != 1 {
		t.Fatalf("退避轮次=%d，期望 1（被催之后只应重连一轮就连上）", n)
	}
	if got := klPendingKicks(w); got != 0 {
		t.Fatalf("kick 令牌未被消费干净，残留 %d 个（会把下一轮唤醒逻辑打乱）", got)
	}
	if w2 := mustWrapper(t, m, key); w2 != w {
		t.Fatal("被催之后换了一具 wrapper：常驻协程被丢弃或重建，而不是弃连重连")
	}
	_, _, _, sent, _ := s.snapshot()
	if len(sent) != 1 || sent[0].body != "hi" {
		t.Fatalf("连上之后这条没落到会话上: %+v", sent)
	}
}

// TestKickDuringLoginPendingNoGoroutineLoss 同一账号跑完三态（被催弃连 / 正常连上 / stop 停机）
// 之后：常驻协程数归零、wrapper 不换代、每条状态边界上 kick 通道都不留残余令牌。
//
// 判别对照：若 kick 被两处 select 都消费（或消费后又塞回去），len(w.kick) 与 Login 调用数
// 会当场对不上；若弃连路径改成「摘 key 重建」，对象身份与 wg 归零两条即红。
func TestKickDuringLoginPendingNoGoroutineLoss(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	key := dev.ID.String()
	clock := &klClock{delay: 10 * time.Millisecond} // 这条不测预算，只要跑得快
	m, _, get := startFakeWA(t, klCfg(), func(s *fakeSession) {
		s.holdLoginPending = true
		s.releaseAfter = 2
		s.loginSettleDelay = klLoginSettle
	}, dev)
	m.afterFor = clock.after

	s, w := klStartPending(t, m, get, key)

	// ① 被催弃连：kick 被接走、协程不退、对象不换。
	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil || id != "MSG-1" {
		t.Fatalf("①被催弃连后应发出去: id=%q err=%v", id, err)
	}
	if got := klPendingKicks(w); got != 0 {
		t.Fatalf("①kick 令牌残留 %d 个", got)
	}
	select {
	case <-w.done:
		t.Fatal("①被催之后常驻协程退出了（应为弃连重连，不是收手）")
	default:
	}
	if w2 := mustWrapper(t, m, key); w2 != w {
		t.Fatal("①常驻协程被换成另一具 wrapper（协程丢了）")
	}

	// ② 正常连上：此时协程守在 waitUntilDown，再调门面不该催连、不该加轮次。
	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000001", "again"); err != nil || id != "MSG-1" {
		t.Fatalf("②已登录态发信应直接成功: id=%q err=%v", id, err)
	}
	if got := s.countLogins(); got != 2 {
		t.Fatalf("②已连上之后 Login 调用数=%d，期望停在 2（多出来的轮次说明守候语义被串了）", got)
	}
	if got := klPendingKicks(w); got != 0 {
		t.Fatalf("②kick 令牌残留 %d 个", got)
	}

	// ③ stop 停机：协程归零、clients 清空、不留待消费的唤醒令牌。
	// 此刻没有正在进行的 Login（协程守在 waitUntilDown），run() 收尾的 defer Disconnect
	// 即刻返回，远在 StopClient 的 1.5s 预算内。
	if err := WhatsappStopClient(key); err != nil {
		t.Fatal(err)
	}
	if !waitManagerWG(m, 2*time.Second) {
		t.Fatalf("③停机后仍有常驻协程未退出（协程泄漏）：kick=%d downed=%d",
			klPendingKicks(w), len(w.downed))
	}
	select {
	case <-w.done:
	default:
		t.Fatal("③done 未关闭：run() 没收尾")
	}
	if got := klPendingKicks(w); got != 0 {
		t.Fatalf("③停机后 kick 通道残留 %d 个令牌，会把下一轮唤醒逻辑打乱", got)
	}
	if n := m.countClients(); n != 0 {
		t.Fatalf("③停机后 clients 表仍有 %d 条", n)
	}
	if got := s.countLogins(); got != 2 {
		t.Fatalf("③停机不应再催起重连，Login 调用数=%d，期望停在 2", got)
	}
}

// TestKickNotConsumedWhenAlreadyLoggedIn 反向死闸：已登录态下门面不该产生「弃连」。
//
// 这条证明新增的 select 支只落在 waitLoggedIn（等登录那一档）里，没有把 waitUntilDown
// 的语义误搬进来、也没有反过来让「已连上时的催连」变成弃连：
// waitUntilDown 收 kick 之后 run() 会重开一轮 Login，本用例把边界钉死为
// 「已登录 ⇒ 一次 Login 都不多、一次 Disconnect 都不做」。
//
// 判别对照：删掉 reconnectAndAwait 的已登录快路短路（门面逢调必催），或让 run()
// 在已登录态也把 kick 当弃连处理，本用例即红。
func TestKickNotConsumedWhenAlreadyLoggedIn(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	key := dev.ID.String()
	clock := &klClock{delay: klBackoff}
	m, _, get := startFakeWA(t, klCfg(), nil, dev) // 常规假件：首轮就登录成功，三个挂点全默认零值
	m.afterFor = clock.after

	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	s := get(key)
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)
	w := mustWrapper(t, m, key)
	// 前置：Login 恰 1 次、登录态已落定、没要求过退避 ⇒ 常驻协程此刻只能守在
	// waitUntilDown（run() 里除了这条没有别处可停留），这正是「不该被弃连」的那一档。
	if got := s.countLogins(); got != 1 {
		t.Fatalf("前置不成立：Login 调用数=%d，期望 1", got)
	}
	if n := clock.rounds(); n != 0 {
		t.Fatalf("前置不成立：正常连上不该进退避，已要求睡眠 %d 次", n)
	}
	beforeLogins := s.countLogins()
	_, beforeDisconnects, _, _, _ := s.snapshot()

	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil || id != "MSG-1" {
		t.Fatalf("已登录态发信应直接成功: id=%q err=%v", id, err)
	}
	// 留一个稳定窗口：残留的 kick 令牌会被 waitUntilDown 立刻接走并多跑一轮 Login，
	// 只看瞬时值可能刚好错过，这里等它暴露（同 wa_manager_test.go 停机后的观察写法）。
	time.Sleep(klStableWindow)

	if got := s.countLogins(); got != beforeLogins {
		t.Fatalf("已登录态的门面产生了重连：Login 调用数 %d → %d", beforeLogins, got)
	}
	if _, disconnects, _, _, _ := s.snapshot(); disconnects != beforeDisconnects {
		t.Fatalf("已登录态的门面产生了弃连：Disconnect 次数 %d → %d", beforeDisconnects, disconnects)
	}
	if !s.LoggedIn() {
		t.Fatal("已登录态的门面把登录态弄掉了")
	}
	if got := klPendingKicks(w); got != 0 {
		t.Fatalf("kick 通道残留 %d 个令牌（门面在已登录时本就不该催连）", got)
	}
	if w2 := mustWrapper(t, m, key); w2 != w {
		t.Fatal("已登录态的门面把 wrapper 换成了另一具")
	}
}
