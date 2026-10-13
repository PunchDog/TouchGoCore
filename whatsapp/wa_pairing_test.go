package whatsapp

// QR 配对流程的测试：假 waPairSession 复刻库侧 QR 通道的三条契约
//（查自 WM/qrchan.go）——GetQRChannel 先于 Connect、ctx 取消即关通道并断连
//（:124-132）、终局项之后通道关闭（:196-203）。断言对象是本包的泵协程：
// 事件扇出、终局清理不泄漏、Cancel/TTL 生效、成功后不自动 StartClient。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// fakePairSession 是配对用一次性会话的假实现。
// out 由测试推事件；closeOnce 复刻「库关通道且只关一次」。
type fakePairSession struct {
	dev   *store.Device
	store *fakeStore

	out       chan whatsmeow.QRChannelItem
	closeOnce sync.Once

	mu           sync.Mutex
	connected    int
	disconnected int
	connectErr   error
	ctx          context.Context
}

func (f *fakePairSession) QRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	if f.dev.ID != nil {
		f.mu.Unlock()
		return nil, whatsmeow.ErrQRStoreContainsID // 库口径：已配对不给二维码（qrchan.go:227）
	}
	f.ctx = ctx
	f.mu.Unlock()
	// 复刻库侧对 ctx 取消的处置：关通道 + 断连（qrchan.go:124-132）。
	go func() {
		<-ctx.Done()
		f.closeOut()
		f.Disconnect()
	}()
	return f.out, nil
}

func (f *fakePairSession) Connect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected++
	return f.connectErr
}

func (f *fakePairSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected++
}

func (f *fakePairSession) Device() *store.Device { return f.dev }

func (f *fakePairSession) disconnectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disconnected
}

func (f *fakePairSession) connectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakePairSession) closeOut() {
	f.closeOnce.Do(func() { close(f.out) })
}

// emit 推一条事件；terminal=true 时随后关通道（复刻库「终局项之后立即 close」）。
func (f *fakePairSession) emit(item whatsmeow.QRChannelItem, terminal bool) {
	f.out <- item
	if terminal {
		f.closeOut()
	}
}

// pairSuccess 模拟扫码成功：库在 PairSuccess 前已把 JID 写进 Device 并自动 Save，
// 假件在这里复刻这两步，然后推终局项。
func (f *fakePairSession) pairSuccess(jid string) {
	j, err := types.ParseJID(jid)
	if err != nil {
		panic(err) // 测试代码里的编程错，直接炸出来
	}
	f.dev.ID = &j
	if f.store != nil {
		f.store.saveDevice(f.dev)
	}
	f.emit(whatsmeow.QRChannelSuccess, true)
}

var _ waPairSession = (*fakePairSession)(nil)

// fakePairTracker 记录工厂造出的全部假配对会话（并发安全）。
type fakePairTracker struct {
	mu   sync.Mutex
	list []*fakePairSession
}

func (tr *fakePairTracker) last() *fakePairSession {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.list) == 0 {
		return nil
	}
	return tr.list[len(tr.list)-1]
}

// installFakePairFactory 换掉 waPairSessionFactory，返回取件口。
// configure 在创建当场调用，用于预置 connectErr 等分支。
func installFakePairFactory(t *testing.T, st *fakeStore, configure func(*fakePairSession)) *fakePairTracker {
	t.Helper()
	tr := &fakePairTracker{}
	old := waPairSessionFactory
	waPairSessionFactory = func(dev *store.Device, log waLog.Logger) waPairSession {
		f := &fakePairSession{dev: dev, store: st, out: make(chan whatsmeow.QRChannelItem, 8)}
		if configure != nil {
			configure(f)
		}
		tr.mu.Lock()
		tr.list = append(tr.list, f)
		tr.mu.Unlock()
		return f
	}
	t.Cleanup(func() { waPairSessionFactory = old })
	return tr
}

// TestPairingSuccessFlow 扫码成功的完整链路：二维码扇出给订阅者、success 事件带上
// 新账号 JID、设备落库、会话从活动表摘除、订阅通道关闭、且**不自动 StartClient**。
func TestPairingSuccessFlow(t *testing.T) {
	st := newFakeStore()
	m := newTestManager(t, st, nil)
	tr := installFakePairFactory(t, st, nil)

	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}
	if !strings.HasPrefix(sess.ID, "qr-") || len(sess.ID) != 35 {
		t.Fatalf("会话 id 形态不对: %q（期望 qr- + 32 位十六进制）", sess.ID)
	}
	sub, unsub := sess.Subscribe()
	defer unsub()
	fake := tr.last()
	waitUntil(t, 2*time.Second, "配对连接", func() bool { return fake.connectCount() >= 1 })

	fake.emit(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "QR-CODE-1", Timeout: 20 * time.Second,
	}, false)
	ev := readEvent(t, sub, "二维码事件")
	if ev.Event != "code" || ev.Code != "QR-CODE-1" || ev.TimeoutMS != 20000 || ev.SessionID != sess.ID {
		t.Fatalf("二维码事件不符: %+v", ev)
	}

	const newJID = "8613800000009@s.whatsapp.net"
	fake.pairSuccess(newJID)
	ev = readEvent(t, sub, "成功事件")
	if ev.Event != "success" || ev.JID != newJID {
		t.Fatalf("成功事件不符: %+v", ev)
	}

	// 泵协程收尾：订阅通道关闭、活动表摘除、Done 可等。
	if _, ok := <-sub; ok {
		t.Fatal("终局后订阅通道应当关闭")
	}
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("泵协程未退出")
	}
	if _, ok := m.LookupQRSession(sess.ID); ok {
		t.Fatal("会话结束后仍挂在活动表里（泄漏）")
	}
	if got := sess.PairedJID(); got != newJID {
		t.Fatalf("PairedJID=%q，期望 %q", got, newJID)
	}
	// 落库由库侧自动 Save 完成（假件在 pairSuccess 里复刻）：本包的生产代码
	// 不调 PutDevice，这里断言的是「成功后设备可被 GetDevice 查回」的契约结果。
	if saved := st.savedList(); len(saved) != 1 || saved[0] != newJID {
		t.Fatalf("配对成功后设备未落库: %v", saved)
	}
	// 用户裁决：配对成功不自动上线。
	if got := m.ListClients(); len(got) != 0 {
		t.Fatalf("配对成功后不得自动 StartClient，实得在跑会话: %+v", got)
	}
	if fake.disconnectCount() == 0 {
		t.Fatal("会话收尾应当断开配对用的一次性连接")
	}
}

// TestPairingFailureCleansSession 失败终局（error 事件）同样必须把会话从活动表摘掉：
// 失败路径漏清理，活动表就成了拿 id 也连不上的僵尸名录。
func TestPairingFailureCleansSession(t *testing.T) {
	st := newFakeStore()
	m := newTestManager(t, st, nil)
	tr := installFakePairFactory(t, st, nil)

	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sub, unsub := sess.Subscribe()
	defer unsub()
	tr.last().emit(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventError, Error: errors.New("pair refused"),
	}, true)

	ev := readEvent(t, sub, "失败事件")
	if ev.Event != "error" || !strings.Contains(ev.Error, "pair refused") {
		t.Fatalf("失败事件不符: %+v", ev)
	}
	if _, ok := <-sub; ok {
		t.Fatal("终局后订阅通道应当关闭")
	}
	waitUntil(t, 2*time.Second, "活动表清理", func() bool {
		_, ok := m.LookupQRSession(sess.ID)
		return !ok
	})
}

// TestPairingCancelWorks Cancel 通过 ctx 取消传导到库侧契约（关通道+断连），
// 泵协程随即收尾——不需要另一套停止信号。
func TestPairingCancelWorks(t *testing.T) {
	st := newFakeStore()
	m := newTestManager(t, st, nil)
	tr := installFakePairFactory(t, st, nil)

	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sub, unsub := sess.Subscribe()
	defer unsub()
	sess.Cancel()
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel 后泵协程未退出")
	}
	if _, ok := <-sub; ok {
		t.Fatal("Cancel 后订阅通道应当关闭")
	}
	if fake := tr.last(); fake.disconnectCount() == 0 {
		t.Fatal("Cancel 未断开配对连接")
	}
	if _, ok := m.LookupQRSession(sess.ID); ok {
		t.Fatal("Cancel 后会话仍在活动表里（泄漏）")
	}
}

// TestPairingTTLCleansUp 没人扫也没人取消的会话由 TTL 兜底清理：
// 到点 ctx 取消 → 库契约关通道断连 → 泵协程收尾摘表。
func TestPairingTTLCleansUp(t *testing.T) {
	old := waQRTTL
	waQRTTL = 80 * time.Millisecond
	defer func() { waQRTTL = old }()

	st := newFakeStore()
	m := newTestManager(t, st, nil)
	installFakePairFactory(t, st, nil)

	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("TTL 到点后泵协程未退出（会话泄漏）")
	}
	if _, ok := m.LookupQRSession(sess.ID); ok {
		t.Fatal("TTL 到点后会话仍在活动表里")
	}
}

// TestPairingConnectErrorSurfaces Connect 失败要作为 error 事件送达订阅者再清理，
// 静默消失会让前端停在「等二维码」上直到超时。
func TestPairingConnectErrorSurfaces(t *testing.T) {
	st := newFakeStore()
	m := newTestManager(t, st, nil)
	installFakePairFactory(t, st, func(f *fakePairSession) {
		f.connectErr = errors.New("network down")
	})

	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sub, unsub := sess.Subscribe()
	defer unsub()
	ev := readEvent(t, sub, "连接失败事件")
	if ev.Event != "error" || !strings.Contains(ev.Error, "network down") {
		t.Fatalf("连接失败事件不符: %+v", ev)
	}
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("连接失败后泵协程未退出")
	}
}

// TestSubscribeAfterSessionClosed 迟到订阅拿到的必须是一个已关闭的通道：
// 订阅方按「读到 !ok 即散场」的同一口径处理，不必为先订阅还是先结束加分支。
func TestSubscribeAfterSessionClosed(t *testing.T) {
	s := &QRSession{ID: "qr-x", done: make(chan struct{}), subs: map[chan QREvent]struct{}{}}
	s.finish()
	ch, unsub := s.Subscribe()
	defer unsub()
	if _, ok := <-ch; ok {
		t.Fatal("已结束会话的迟到订阅应当拿到已关闭通道")
	}
	unsub() // 重复退订不 panic
}

// readEvent 从订阅通道读一条事件，超时即失败。
func readEvent(t *testing.T, sub <-chan QREvent, what string) QREvent {
	t.Helper()
	select {
	case ev, ok := <-sub:
		if !ok {
			t.Fatalf("读 %s 时通道已关闭", what)
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 %s 超时", what)
	}
	return QREvent{} // 到不了：上面两条分支都 Fatalf；编译器不认 Fatalf 终结性
}

// TestSubscribeReplaysLastEvent 订阅者晚于事件到达时必须补投最近一条。
//
// 业务链路是「StartPairing 返回 id → 前端再开 WebSocket」，扇出只投当时在册的订阅者，
// 空窗里发出的首个二维码若不补投，前端会停在「等码」直到下一轮轮换（约 20-60 秒），
// 终局事件（success/error/timeout）更会永久丢失——这条闸钉的就是那个空窗。
func TestSubscribeReplaysLastEvent(t *testing.T) {
	s := &QRSession{ID: "qr-replay", done: make(chan struct{}), subs: map[chan QREvent]struct{}{}}
	s.emit(QREvent{Event: "code", Code: "RAW-CODE-1"})

	sub, unsub := s.Subscribe()
	defer unsub()
	ev := readEvent(t, sub, "补投的最近事件")
	if ev.Event != "code" || ev.Code != "RAW-CODE-1" || ev.SessionID != "qr-replay" {
		t.Fatalf("补投事件不符: %+v", ev)
	}

	// 补投之后新事件仍只投一次：同一锁内「先补投后登记」与扇出互斥，不得投两遍。
	s.emit(QREvent{Event: "success", JID: "86138@s.whatsapp.net"})
	next := readEvent(t, sub, "后续新事件")
	if next.Event != "success" || next.JID != "86138@s.whatsapp.net" {
		t.Fatalf("后续事件不符: %+v", next)
	}
	select {
	case dup := <-sub:
		t.Fatalf("事件被投了第二遍: %+v", dup)
	default:
	}
}

// TestSubscribeAfterTerminalKeepsReason 会话已终局后迟到订阅仍要拿得到终局事件：
// 只给一个已关闭的通道会让前端分不清「配对失败」与「没赶上」，排障时无从下手。
func TestSubscribeAfterTerminalKeepsReason(t *testing.T) {
	s := &QRSession{ID: "qr-term", done: make(chan struct{}), subs: map[chan QREvent]struct{}{}}
	s.emit(QREvent{Event: "timeout"})
	s.finish()

	sub, unsub := s.Subscribe()
	defer unsub()
	ev, ok := <-sub
	if !ok {
		t.Fatal("已结束会话的迟到订阅丢了终局事件，只拿到关闭的通道")
	}
	if ev.Event != "timeout" {
		t.Fatalf("终局事件不符: %+v", ev)
	}
	if _, ok := <-sub; ok {
		t.Fatal("补投之后通道应随即散场")
	}
}
