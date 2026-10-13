package whatsapp

// 吊销设备配对（WhatsappDeleteDevice / waManager.DeleteDevice）的测试：
// 在跑先服务端解绑再收协程再删行、没在跑只删行、查无设备行 fail-closed 且不广播、
// 解绑失败仍兜底删行、以及两条时序闸（latch 必须早于 Logout、库已删过的设备不得再递一次）。
// 全部走 seam 假件，不联网、不连真 Postgres。

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store"

	"touchgocore/config"
)

// startConnected 启动一个在跑账号并等到它首次连上，返回假会话（计数器与事件都从它读）。
func startConnected(t *testing.T, m *waManager, get func(string) *fakeSession, dev *store.Device) *fakeSession {
	t.Helper()
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动账号失败: %v", err)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记")
	}
	waitUntil(t, 2*time.Second, "首次连接", func() bool { return s.countLogins() >= 1 })
	return s
}

// TestDeleteDeviceRunningUnlinksThenDeletesRow 在跑账号的完整吊销链：服务端解绑 →
// 收掉常驻协程 → 删设备行 → 广播终态，且此后这个账号再也起不来（必须重新扫码）。
// 顺序判据都在这条上：解绑没做成、协程没收、行没删、广播缺一条，都会红。
func TestDeleteDeviceRunningUnlinksThenDeletesRow(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	col := registerEventCollectors(t)
	m, st, get := startFakeWA(t, &config.WhatsappSessionsConfig{AntibanEnabled: waBool(false)}, nil, dev)
	s := startConnected(t, m, get, dev)
	before := s.countLogins()

	if err := WhatsappDeleteDevice(nil, dev.ID.String()); err != nil {
		t.Fatalf("在跑账号的正常吊销不该报错: %v", err)
	}
	if s.countLogouts() != 1 {
		t.Errorf("服务端 unlink 调用次数=%d，期望 1", s.countLogouts())
	}
	if got := st.deletedList(); len(got) != 1 || got[0] != dev.ID.String() {
		t.Errorf("设备行未删除或删错对象: %v", got)
	}
	if _, disconnects, _, _, _ := s.snapshot(); disconnects < 1 {
		t.Errorf("删行前连接未断开（disconnects=%d），可能留下引用已删行的活 socket", disconnects)
	}
	if got := m.ListClients(); len(got) != 0 {
		t.Errorf("吊销后仍有在跑会话: %+v", got)
	}
	if err := m.StartClient(*dev.ID); err == nil {
		t.Error("设备行已删，StartClient 应当报未配对")
	}

	_, _, conns := col.snapshot()
	if len(conns) != 1 {
		t.Fatalf("Connection 广播条数=%d，期望恰好 1 条 logged_out", len(conns))
	}
	if conns[0].State != "logged_out" || conns[0].Account != dev.ID.String() {
		t.Errorf("广播形态不符: %+v", conns[0])
	}
	if !strings.Contains(conns[0].Reason, "服务端已 unlink") {
		t.Errorf("解绑成功路径的 Reason 不该提示手动取消: %q", conns[0].Reason)
	}

	// 终态判据：latch 之后不得再有任何重连（给第二条连接留暴露窗口）。
	time.Sleep(150 * time.Millisecond)
	if after := s.countLogins(); after != before {
		t.Errorf("吊销后 Login 调用从 %d 变 %d，常驻协程把已吊销的设备又连回去了", before, after)
	}
}

// TestDeleteDeviceNotRunningDeletesRowOnly 没在跑的账号只删本地行：不在一次面向调用方
// 的破坏性调用里起整条握手。代价必须写进广播——手机端仍列着这台关联设备，只有手机侧能取消。
func TestDeleteDeviceNotRunningDeletesRowOnly(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	col := registerEventCollectors(t)
	_, st, get := startFakeWA(t, nil, nil, dev)

	if err := WhatsappDeleteDevice(nil, dev.ID.String()); err != nil {
		t.Fatalf("未跑账号的吊销不该报错: %v", err)
	}
	if got := st.deletedList(); len(got) != 1 || got[0] != dev.ID.String() {
		t.Errorf("设备行未删除: %v", got)
	}
	// 假会话从未构造 ⇒ 本包压根没为这个账号建 Client，也就没做过 unlink。
	if s := get(dev.ID.String()); s != nil {
		t.Errorf("未跑账号的吊销不该建 Client（logouts=%d）", s.countLogouts())
	}
	_, _, conns := col.snapshot()
	if len(conns) != 1 {
		t.Fatalf("Connection 广播条数=%d，期望恰好 1 条 logged_out", len(conns))
	}
	if conns[0].State != "logged_out" {
		t.Errorf("广播状态不符: %+v", conns[0])
	}
	if !strings.Contains(conns[0].Reason, "手动取消") {
		t.Errorf("没做服务端解绑却不说清残留，下游会以为手机端也干净了: %q", conns[0].Reason)
	}
}

// TestDeleteDeviceUnknownJIDFailsClosed 查无设备行时什么都不做：不碰在跑会话、不广播。
// 「行状态未知」与「行已删除」必须是两种分得开的读数，否则下游没法决定要不要重试。
func TestDeleteDeviceUnknownJIDFailsClosed(t *testing.T) {
	other := fakeDevice(t, "8613900000000@s.whatsapp.net", "android", "B")
	col := registerEventCollectors(t)
	m, st, get := startFakeWA(t, nil, nil, other)
	// 另一个账号在跑，吊销查无的账号绝不能把它带走。
	startConnected(t, m, get, other)

	err := WhatsappDeleteDevice(nil, "8613800000000")
	if err == nil || !strings.Contains(err.Error(), "没有设备行") {
		t.Fatalf("查无设备行应当被拒并说清缘由，实得: %v", err)
	}
	if !strings.Contains(err.Error(), "whatsapp 删除设备失败") {
		t.Errorf("错误未带通道前缀: %v", err)
	}
	if got := st.deletedList(); len(got) != 0 {
		t.Errorf("查无账号却删了行: %v", got)
	}
	if _, _, conns := col.snapshot(); len(conns) != 0 {
		t.Errorf("被拒的吊销不该广播终态: %+v", conns)
	}
	if got := m.ListClients(); len(got) != 1 {
		t.Errorf("在跑账号被误伤: %+v", got)
	}
}

// TestDeleteDeviceLogoutFailureFallsBackToRowDelete 服务端解绑谈不成（半开 socket、
// 对端拒答、超时都算）时，本地行必须照样删掉——否则「名义吊销、行还在」比啥都没做更难查。
// 返回值必须同时讲清两件事：行已删除（别再重试整个删除）+ 残留要人工处置。
func TestDeleteDeviceLogoutFailureFallsBackToRowDelete(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	col := registerEventCollectors(t)
	m, st, get := startFakeWA(t, nil, func(s *fakeSession) { s.logoutErr = errBoom }, dev)
	s := startConnected(t, m, get, dev)

	err := WhatsappDeleteDevice(nil, "8613800000000")
	if err == nil {
		t.Fatal("解绑没做成必须报错")
	}
	if !strings.Contains(err.Error(), "设备行已删除") || !strings.Contains(err.Error(), "unlink 未完成") {
		t.Errorf("错误文案没同时给出「行已删、别再重试」与「解绑未完成」: %v", err)
	}
	if s.countLogouts() != 1 {
		t.Errorf("unlink 调用次数=%d，期望 1", s.countLogouts())
	}
	if got := st.deletedList(); len(got) != 1 {
		t.Errorf("兜底删行未执行: %v", got)
	}
	_, _, conns := col.snapshot()
	if len(conns) != 1 || !strings.Contains(conns[0].Reason, "手动取消") {
		t.Errorf("解绑失败仍要广播终态并写清残留: %+v", conns)
	}
	// 库的原始错误串只进返回值，不进广播载荷。
	if strings.Contains(conns[0].Reason, errBoom.Error()) {
		t.Errorf("广播 Reason 拼进了库侧原始错误: %q", conns[0].Reason)
	}
}

// TestDeleteDeviceLogoutAlreadyDeletedRowSkipsSecondDelete 钉住库侧陷阱：
// whatsmeow 的 Logout 成功时顺带走 Device.Delete，那一步把 dev.ID 置成 nil
// （WM/store/store.go 的 Delete），而 Container.DeleteDevice 遇 ID 为空直接报错
// （WM/store/sqlstore/container.go）。朴素实现把 wrapper 手里的设备再递一次就撞上去；
// 正确实现按 JID 重读、认「查无或 ID 为空 = 库已删过」，跳过二次删除且仍报成功。
func TestDeleteDeviceLogoutAlreadyDeletedRowSkipsSecondDelete(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	// 提前按值取一份 JID：假件的 deleteOnLogout 会把 dev.ID 置 nil（与库侧 Device.Delete 同形），
	// 删完再解引用 dev.ID 只会 panic，测不到想测的东西。
	jid := *dev.ID
	key := jid.String()
	col := registerEventCollectors(t)
	m, st, get := startFakeWA(t, nil, func(s *fakeSession) { s.deleteOnLogout = true }, dev)
	startConnected(t, m, get, dev)

	if err := WhatsappDeleteDevice(nil, key); err != nil {
		t.Fatalf("库已删过行时不该报错: %v", err)
	}
	if got := st.deletedList(); len(got) != 0 {
		t.Errorf("库侧 Logout 已删行，本包又递了一次删除: %v", got)
	}
	_, _, conns := col.snapshot()
	if len(conns) != 1 || conns[0].State != "logged_out" {
		t.Errorf("终态广播缺失: %+v", conns)
	}
	if err := m.StartClient(jid); err == nil {
		t.Error("行已删，StartClient 应当报未配对")
	}
}

// TestDeleteDeviceSetsLoggedOutBeforeLogout 时序闸：latch 必须落在 unlink 之前。
// 解绑失败那一路 socket 是被远端拆的，库确实会派发 events.Disconnected（手动断开才不派），
// 常驻协程收到它就从 waitUntilDown 醒来重新 Login——把正要吊销的设备又连回去。
// 这是该顺序唯一能活的测法：删掉那句 loggedOut.Store(true) 本用例必红。
func TestDeleteDeviceSetsLoggedOutBeforeLogout(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	registerEventCollectors(t)
	m, st, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.fireDisconnectedOnLogout = true
		s.logoutErr = errBoom
	}, dev)
	s := startConnected(t, m, get, dev)
	before := s.countLogins()

	if err := WhatsappDeleteDevice(nil, "8613800000000"); err == nil {
		t.Fatal("解绑失败必须报错")
	}
	time.Sleep(150 * time.Millisecond)
	if after := s.countLogins(); after != before {
		t.Errorf("Login 调用从 %d 变 %d：latch 没先落位，被远端断开触发了重连", before, after)
	}
	if got := st.deletedList(); len(got) != 1 {
		t.Errorf("设备行应被兜底删掉: %v", got)
	}
}

// TestDeleteDeviceRejectsBadJIDAndMissingRow JID 归一在触达任何子系统之前就把
// 不可用写法拒掉：空串拼不出号码（waAccountJID 口径——设备行主键本来就带 :N
// 设备段，吊销按行逐字删，带设备段是常态写法而不是错误）。
// 格式合法但库里查无行的账号就地拒绝，且绝不留下删除痕迹。
func TestDeleteDeviceRejectsBadJIDAndMissingRow(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	_, st, _ := startFakeWA(t, nil, nil, dev)

	if err := WhatsappDeleteDevice(nil, ""); err == nil || !strings.Contains(err.Error(), "账号为空") {
		t.Errorf("空目标应当被拒: %v", err)
	}
	err := WhatsappDeleteDevice(nil, "8613800000000:3@s.whatsapp.net")
	if err == nil || !strings.Contains(err.Error(), "没有设备行") {
		t.Errorf("带设备号但库里无该行，应当报没有设备行而不是格式拒: %v", err)
	}
	if got := st.deletedList(); len(got) != 0 {
		t.Errorf("被拒的写法不该删行: %v", got)
	}
}
