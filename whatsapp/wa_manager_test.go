package whatsapp

// 消息侧测试的公共假件都收在本文件：fakeSession（waSession seam 的假实现）、
// fakeStore（waDeviceStore seam 的假实现）与装配 helper。
// 全部走 seam 注入，不联网、不连真 Postgres（测试惯例见 docs/testing-conventions.md）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"touchgocore/config"
)

// fakeSession 是 waSession 的假实现：记录调用、按预置值返回，事件由 fire 手动触发。
// 所有可变字段都过 mu：常驻协程与测试主体并发读写它。
type fakeSession struct {
	mu sync.Mutex

	dev *store.Device

	loginErr   error
	loggedIn   bool
	loginCalls int

	disconnects int
	logouts     int

	// logoutErr：服务端 unlink 谈不成时的兜底路径注入点（照 loginErr 那一族写法）。
	logoutErr error
	// deleteOnLogout：仿真库侧 Logout 成功时顺带走的 Device.Delete——它把 dev.ID 置 nil
	//（WM/store/store.go 的 Delete），用来钉住「绝不能把 ID 已空的设备再递给 DeleteDevice」。
	deleteOnLogout bool
	// fireDisconnectedOnLogout：仿真「unlink 没谈成、socket 已被远端拆掉」——库在这种
	// 情形下确实派发 events.Disconnected（手动断开才不派，WM/client.go 的 onDisconnect），
	// 常驻协程收到它就会从 waitUntilDown 醒来重连，正是 latch 必须在 Logout 之前的理由。
	fireDisconnectedOnLogout bool

	sendID   string // SendText 返回的消息 id；空串模拟「服务端没给 id」
	sendErr  error
	stateErr error
	sent     []fakeText
	patches  []appstate.PatchInfo
	handler  func(any)

	// 好友列表与群组查询的预置返回值（查好友 / 查可发言群 / 发群消息用例用）。
	contacts    map[types.JID]types.ContactInfo
	contactsErr error
	groups      []*types.GroupInfo
	groupsErr   error
}

type fakeText struct{ to, body string }

func (f *fakeSession) Login(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginCalls++
	if f.loginErr != nil {
		return f.loginErr
	}
	f.loggedIn = true
	return nil
}

func (f *fakeSession) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loggedIn
}

func (f *fakeSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects++
	f.loggedIn = false
}

// setDown 只把登录态按回 false，不记 disconnect 次数、不派发任何事件。
//
// 它仿真的是发信自愈要治的那一档窗口：socket 已经掉了、但库还没来得及派发
// events.Disconnected（或派发到了但登录态尚未刷新）。用 Disconnect() 造这个状态会
// 顺带把 downed 令牌递进去，常驻协程立刻自己重连，测不出 ensureReady 的催连是否真生效。
func (f *fakeSession) setDown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedIn = false
}

func (f *fakeSession) Logout(ctx context.Context) error {
	f.mu.Lock()
	f.logouts++
	if !f.loggedIn {
		// 库侧此时快速失败而不是静默成功：socket 已为 nil 就返回 ErrNotConnected
		//（WM/client.go），且 unlink 失败即提前返回、不会走到删自己那一步。
		// 假件照做，「先收协程再解绑」这种调序才会被测红而不是蒙混过去。
		f.mu.Unlock()
		return fmt.Errorf("whatsapp 假件: 连接已断，无法 unlink")
	}
	f.loggedIn = false
	if f.deleteOnLogout && f.dev != nil {
		f.dev.ID = nil
		f.dev.Deleted = true
	}
	h := f.handler
	err := f.logoutErr
	fire := f.fireDisconnectedOnLogout
	f.mu.Unlock()
	// 回调一律在 mu 之外触发（与 fire 同纪律）：onEvent 要拿 wrapper 自己的锁，
	// 在假会话锁内调用会把两个不相干的锁绑出一条锁序。
	if fire && h != nil {
		h(events.Disconnected{})
	}
	return err
}

func (f *fakeSession) SendText(ctx context.Context, to string, body string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, fakeText{to: to, body: body})
	return f.sendID, time.Now(), f.sendErr
}

func (f *fakeSession) SendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, patch)
	return f.stateErr
}

func (f *fakeSession) GetContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contacts, f.contactsErr
}

func (f *fakeSession) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups, f.groupsErr
}

func (f *fakeSession) OnEvent(handler func(any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
}

// fire 触发一次事件回调（等价于库的事件派发）。
func (f *fakeSession) fire(evt any) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(evt)
	}
}

func (f *fakeSession) snapshot() (loginCalls, disconnects int, loggedIn bool, sent []fakeText, patches []appstate.PatchInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent = append([]fakeText(nil), f.sent...)
	patches = append([]appstate.PatchInfo(nil), f.patches...)
	return f.loginCalls, f.disconnects, f.loggedIn, sent, patches
}

// countLogins/countLogouts 单独读这两个计数器：吊销用例要断言「设备没被重连回去」，
// 而 snapshot() 的五值形态已有十余处调用方，不为一个新断言去扩签名。
func (f *fakeSession) countLogins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls
}

func (f *fakeSession) countLogouts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logouts
}

var _ waSession = (*fakeSession)(nil)

// fakeStore 是 waDeviceStore 的假实现：设备表按 JID 串索引。
type fakeStore struct {
	mu       sync.Mutex
	devices  map[string]*store.Device
	newCount int
	deleted  []string
	saved    []string // 模拟库侧配对成功自动 Save 的落库记录
	getErr   error
}

func newFakeStore(devs ...*store.Device) *fakeStore {
	f := &fakeStore{devices: map[string]*store.Device{}}
	for _, d := range devs {
		if d.ID != nil {
			f.devices[d.ID.String()] = d
		}
	}
	return f
}

func (f *fakeStore) GetDevice(ctx context.Context, jid types.JID) (*store.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	// 与真库同口径：查无返回 (nil, nil)（WM/store/sqlstore/container.go:182-188）。
	return f.devices[jid.String()], nil
}

func (f *fakeStore) AllDevices(ctx context.Context) ([]*store.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.Device, 0, len(f.devices))
	for _, d := range f.devices {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeStore) NewDevice() *store.Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCount++
	return &store.Device{}
}

func (f *fakeStore) DeleteDevice(ctx context.Context, dev *store.Device) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dev == nil || dev.ID == nil {
		return fmt.Errorf("设备为空")
	}
	f.deleted = append(f.deleted, dev.ID.String())
	delete(f.devices, dev.ID.String())
	return nil
}

func (f *fakeStore) Close() error { return nil }

// saveDevice 模拟 sqlstore 在配对成功时的自动 Save（真实现里这一步发生在库内部，
// 生产代码不调 PutDevice；假件用它复刻「成功后设备可被 GetDevice 查回」的契约）。
func (f *fakeStore) saveDevice(dev *store.Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dev.ID != nil {
		f.devices[dev.ID.String()] = dev
		f.saved = append(f.saved, dev.ID.String())
	}
}

func (f *fakeStore) savedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.saved...)
}

// deletedList 读出假件收到过哪些 DeleteDevice 调用（与 savedList 同形，
// 吊销用例要靠它区分「行是本包删的」与「库侧 Logout 已经删过、本包没再递设备」）。
func (f *fakeStore) deletedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

var _ waDeviceStore = (*fakeStore)(nil)

// fakeDevice 造一个已配对形态的设备（ID 非 nil，即库里可查到的行）。
func fakeDevice(t *testing.T, jidStr, platform, pushName string) *store.Device {
	t.Helper()
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		t.Fatalf("测试 JID %q 不可解析: %v", jidStr, err)
	}
	return &store.Device{ID: &jid, Platform: platform, PushName: pushName}
}

// installFakeSessionFactory 换掉 waSessionFactory，返回「JID 串 → 假会话」的取件口。
// configure 在假会话创建当场（run 协程起跑前）调用，用于预置 loginErr 等分支。
func installFakeSessionFactory(t *testing.T, configure func(*fakeSession)) func(jid string) *fakeSession {
	t.Helper()
	var mu sync.Mutex
	sessions := map[string]*fakeSession{}
	old := waSessionFactory
	waSessionFactory = func(dev *store.Device, log waLog.Logger) waSession {
		s := &fakeSession{dev: dev, sendID: "MSG-1"}
		if configure != nil {
			configure(s)
		}
		mu.Lock()
		if dev.ID != nil {
			sessions[dev.ID.String()] = s
		}
		mu.Unlock()
		return s
	}
	t.Cleanup(func() { waSessionFactory = old })
	return func(jid string) *fakeSession {
		mu.Lock()
		defer mu.Unlock()
		return sessions[jid]
	}
}

// newTestManager 装配一个用假件的管理器，停机清理挂在 t.Cleanup 上。
func newTestManager(t *testing.T, st waDeviceStore, cfg *config.WhatsappSessionsConfig) *waManager {
	t.Helper()
	m := newWaManager(st, cfg, waLog.Noop, context.Background())
	t.Cleanup(func() { m.StopAll(500 * time.Millisecond) })
	return m
}

// waitUntil 以 5ms 步长轮询 cond，超时即失败。测试里的等待一律走它，
// 不用裸 sleep：裸 sleep 在慢机器上偶发红、快机器上白等（testing-conventions 的抖动面口径）。
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（%s）", what, timeout)
}

// waBool 取 bool 的地址：antiban_enabled 是 *bool 三态，测试里要显式开/关时用它。
func waBool(b bool) *bool { return &b }

// jidOf 是测试内构造 JID 的短手。
func jidOf(t *testing.T, raw string) types.JID {
	t.Helper()
	j, err := types.ParseJID(raw)
	if err != nil {
		t.Fatalf("JID %q 不可解析: %v", raw, err)
	}
	return j
}

// TestStartClientIdempotent 同一 JID 重复 StartClient 只起一个 Client：
// 两个 whatsmeow.Client 抢同一个 Device 会互踢会话（对端表现为 stream_replaced），
// 幂等不是便利，是防自伤。
func TestStartClientIdempotent(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, nil)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("首次启动失败: %v", err)
	}
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("重复启动应当幂等成功: %v", err)
	}
	if got := m.ListClients(); len(got) != 1 {
		t.Fatalf("重复启动后在跑会话数=%d，期望 1", len(got))
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记")
	}
	waitUntil(t, 2*time.Second, "首次连接", func() bool {
		calls, _, _, _, _ := s.snapshot()
		return calls >= 1
	})
	time.Sleep(100 * time.Millisecond) // 给「第二条连接」留暴露窗口
	if calls, _, _, _, _ := s.snapshot(); calls != 1 {
		// 幂等的实质判据：只有一条 run 协程在连——第二个 Client 压根没被创建。
		t.Fatalf("Login 调用次数=%d，重复启动不得再起第二条连接", calls)
	}
}

// TestStartClientUnpairedRejected 库里没有设备行的账号不得启动：
// 拿着空 Device 去握手只会在 WhatsApp 侧留下一串失败记录。
func TestStartClientUnpairedRejected(t *testing.T) {
	st := newFakeStore()
	installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, nil)

	err := m.StartClient(jidOf(t, "8613800000000@s.whatsapp.net"))
	if err == nil {
		t.Fatal("未配对账号启动应当被拒")
	}
	if got := m.ListClients(); len(got) != 0 {
		t.Fatalf("被拒后仍有会话在跑: %+v", got)
	}
}

// TestStartClientStoreErrorPropagates 读库失败必须原样上抛而不是当成「未配对」：
// 两种错的处置完全不同（重试 vs 重新扫码），归一错了业务方会走错分支。
func TestStartClientStoreErrorPropagates(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	st.getErr = fmt.Errorf("模拟库不可达")
	installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, nil)

	err := m.StartClient(*dev.ID)
	if err == nil {
		t.Fatal("读库失败应当上抛")
	}
	if got := err.Error(); !strings.Contains(got, "会话库失败") || strings.Contains(got, "未配对") {
		t.Fatalf("错误口径不对: %v", err)
	}
}

// TestStartClientMaxConcurrentGate max_concurrent>0 时是软上限闸；
// 缺省（nil 配置）不限制——两条口径都要钉住。
func TestStartClientMaxConcurrentGate(t *testing.T) {
	a := fakeDevice(t, "8613800000001@s.whatsapp.net", "android", "A")
	b := fakeDevice(t, "8613800000002@s.whatsapp.net", "android", "B")
	st := newFakeStore(a, b)
	installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{MaxConcurrent: 1, AntibanEnabled: waBool(false)})

	if err := m.StartClient(*a.ID); err != nil {
		t.Fatalf("第一个应当启动: %v", err)
	}
	if err := m.StartClient(*b.ID); err == nil {
		t.Fatal("超过 max_concurrent 应当被拒")
	}
	if got := m.ListClients(); len(got) != 1 {
		t.Fatalf("在跑会话数=%d，期望 1", len(got))
	}
}

// TestStopClientWaitsDone StopClient 返回时协程必须已经收完尾（Disconnect 已发生），
// 停机语义靠 done 通道而不是「发了信号就当停了」。
func TestStopClientWaitsDone(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, nil)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连接", func() bool {
		calls, _, _, _, _ := s.snapshot()
		return calls >= 1
	})
	if err := m.StopClient(*dev.ID); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if _, disconnects, _, _, _ := s.snapshot(); disconnects == 0 {
		t.Fatal("StopClient 返回时未断开连接（done 语义失守）")
	}
	if got := m.ListClients(); len(got) != 0 {
		t.Fatalf("停止后仍在列表里: %+v", got)
	}
	if err := m.StopClient(*dev.ID); err == nil {
		t.Fatal("停止不存在的会话应当报错而不是静默成功")
	}
}

// TestListClientsFields 列表字段逐项对账：JID/Platform/PushName 来自设备行，
// IsConnected 来自登录态，LastSeen 口径是「最近一次收到任何事件的时刻」，
// 且列表按 JID 排序（sync.Map 遍历序是乱的，不排序则快照不可比对）。
func TestListClientsFields(t *testing.T) {
	a := fakeDevice(t, "8613800000001@s.whatsapp.net", "android", "A")
	b := fakeDevice(t, "8613800000002@s.whatsapp.net", "ios", "B")
	st := newFakeStore(a, b)
	get := installFakeSessionFactory(t, func(s *fakeSession) {
		if s.dev.ID != nil && s.dev.ID.User == "8613800000002" {
			s.loginErr = fmt.Errorf("模拟连不上")
		}
	})
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 60, AntibanEnabled: waBool(false)})

	if err := m.StartClient(*a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.StartClient(*b.ID); err != nil {
		t.Fatal(err)
	}
	sa := get(a.ID.String())
	waitUntil(t, 2*time.Second, "A 连上", func() bool {
		_, _, loggedIn, _, _ := sa.snapshot()
		return loggedIn
	})
	// 任何一个事件都要刷新 LastSeen（口径见 SessionInfo 注释）。
	sa.fire(&events.Connected{})

	got := m.ListClients()
	if len(got) != 2 {
		t.Fatalf("列表长度=%d，期望 2", len(got))
	}
	if got[0].JID != a.ID.String() || got[1].JID != b.ID.String() {
		t.Fatalf("列表未按 JID 排序: %+v", got)
	}
	if got[0].Platform != "android" || got[0].PushName != "A" || !got[0].IsConnected {
		t.Fatalf("A 的字段不符: %+v", got[0])
	}
	if got[0].LastSeen.IsZero() {
		t.Fatal("收到事件后 LastSeen 仍为零值")
	}
	if got[1].Platform != "ios" || got[1].IsConnected {
		t.Fatalf("B 的字段不符: %+v", got[1])
	}
}

// TestLoggedOutStopsReconnect 收到 logged_out 后常驻协程收手：
// 会话已被吊销，重连只会烧出无限失败循环，还可能触发风控。
func TestLoggedOutStopsReconnect(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, AntibanEnabled: waBool(false)})

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连接", func() bool {
		calls, _, _, _, _ := s.snapshot()
		return calls >= 1
	})
	s.fire(&events.LoggedOut{})
	waitUntil(t, 2*time.Second, "协程退出", func() bool {
		_, disconnects, _, _, _ := s.snapshot()
		return disconnects >= 1
	})
	callsAtExit, _, _, _, _ := s.snapshot()
	time.Sleep(150 * time.Millisecond) // 退避下界 1s，150ms 足够暴露「还在重连」
	callsLater, _, _, _, _ := s.snapshot()
	if callsLater != callsAtExit {
		t.Fatalf("logged_out 后仍在重连: Login 调用 %d → %d", callsAtExit, callsLater)
	}
}

// TestBackoffSequenceWithFakeClock 退避序列按「min 起跳、逐轮翻倍、夹在 max」走。
// 时钟经 afterFor 注入：断言的是序列本身，不烧真实秒数。
func TestBackoffSequenceWithFakeClock(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, func(s *fakeSession) { s.loginErr = fmt.Errorf("模拟连不上") })
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 4, AntibanEnabled: waBool(false)})

	closed := make(chan time.Time)
	close(closed)
	var mu sync.Mutex
	var got []time.Duration
	m.afterFor = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		return closed
	}

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "四轮退避", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 4
	})
	_ = m.StopClient(*dev.ID)
	_ = get(dev.ID.String())

	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("退避序列=%v，期望 %v（min=1s max=4s，翻倍后夹住）", got[:4], want)
		}
	}
}

// TestWaNextBackoffClamps 纯函数口径：翻倍、夹上界、不落下界。
func TestWaNextBackoffClamps(t *testing.T) {
	if got := waNextBackoff(time.Second, time.Second, 8*time.Second); got != 2*time.Second {
		t.Errorf("翻倍=%s", got)
	}
	if got := waNextBackoff(8*time.Second, time.Second, 8*time.Second); got != 8*time.Second {
		t.Errorf("上界未夹住=%s", got)
	}
	if got := waNextBackoff(500*time.Millisecond, time.Second, 8*time.Second); got != time.Second {
		t.Errorf("下界未托住=%s", got)
	}
}

// TestConcurrentStartStopNoPanic 并发 Start/Stop 不 panic、不泄漏协程：
// sync.Map 的 LoadOrStore/LoadAndDelete 兜住竞态窗口，收口是 StopAll 之后 wg 归零。
func TestConcurrentStartStopNoPanic(t *testing.T) {
	a := fakeDevice(t, "8613800000001@s.whatsapp.net", "android", "A")
	b := fakeDevice(t, "8613800000002@s.whatsapp.net", "android", "B")
	st := newFakeStore(a, b)
	installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 60, AntibanEnabled: waBool(false)})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				dev := a
				if (g+i)%2 == 0 {
					dev = b
				}
				switch (g + i) % 3 {
				case 0:
					_ = m.StartClient(*dev.ID)
				case 1:
					_ = m.StopClient(*dev.ID)
				default:
					_ = m.ListClients()
				}
			}
		}(g)
	}
	wg.Wait()
	done := make(chan struct{})
	m.StopAll(3 * time.Second)
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StopAll 之后仍有常驻协程未退出（泄漏）")
	}
}

// TestStopAllCancelsPairing StopAll 除了收 Client，还要取消进行中的配对会话：
// 配对泵协程也挂在同一个 wg 上，停机漏掉它就会把退出预算烧在等二维码上。
func TestStopAllCancelsPairing(t *testing.T) {
	st := newFakeStore()
	m := newTestManager(t, st, nil)
	tr := installFakePairFactory(t, st, nil)
	if _, err := m.StartPairing(context.Background()); err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}
	m.StopAll(time.Second)
	waitUntil(t, time.Second, "配对泵协程收尾", func() bool {
		f := tr.last()
		return f != nil && f.disconnectCount() >= 1
	})
}
