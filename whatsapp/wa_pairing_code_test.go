package whatsapp

// 手机号配对码门面的测试（T5）。全部不联网：经 waPairSessionFactory 注入
// pcFakePairSession（「换-还原」范式照 wa_pairing_test.go，t.Cleanup 还原挂点）。
// 覆盖：PairPhone 五实参逐字断言、调用顺序（QRChannel→Connect→首个 code 项→
// PairPhone）、非法号码零建连、握手超时与 PairPhone 报错两条失败路径的会话/
// 单飞记账清理、同号可重发、单飞拒绝、成功路径的 pair_code 事件形态与
// 「不自动 StartClient」裁决、全链路号码脱敏。
//
// 本文件还带 fakePairSession（wa_pairing_test.go 既有假件）的 PairPhone 方法——
// 同包方法允许跨文件拆分，既有测试文件因此一个字节都不用动；QR 扫码路径
// 永远不该调它，被调到即 panic 暴露误用。

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"touchgocore/util"
)

// PairPhone 补齐既有假件对扩展后 waPairSession 接口的实现（见文件头说明）。
func (*fakePairSession) PairPhone(context.Context, string, bool, whatsmeow.PairClientType, string) (string, error) {
	panic("QR 扫码配对路径不得调用 PairPhone（码配对断言请用 pcFakePairSession）")
}

// pcFakePairSession 是码配对的假一次性会话。默认形态：Connect 被调时自动向
// QR 通道推首个 code 项（复刻「连接建立后库秒级出码」时序，门面同步等待时
// 不会卡死）；holdCode=true 时不出码，由测试手动推。
type pcFakePairSession struct {
	dev   *store.Device
	store *fakeStore

	out       chan whatsmeow.QRChannelItem
	closeOnce sync.Once

	mu        sync.Mutex
	order     []string // 调用序记录仪："QRChannel" / "Connect" / "PairPhone"
	closed    bool
	disconn   int
	ctx       context.Context
	holdCode  bool
	pairErr   error
	pairCode  string // PairPhone 返回值（假码形态与库一致：XXXX-XXXX）
	pairCalls int
	// PairPhone 五实参留档，供逐字断言。
	gotCtx   context.Context
	gotPhone string
	gotShow  bool
	gotType  whatsmeow.PairClientType
	gotName  string
}

func (f *pcFakePairSession) QRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	f.order = append(f.order, "QRChannel")
	f.ctx = ctx
	f.mu.Unlock()
	// 复刻库侧对 ctx 取消的处置：关通道 + 断连（qrchan.go:124-132）。
	go func() {
		<-ctx.Done()
		f.pcClose()
		f.Disconnect()
	}()
	return f.out, nil
}

func (f *pcFakePairSession) Connect() error {
	f.mu.Lock()
	f.order = append(f.order, "Connect")
	hold := f.holdCode
	f.mu.Unlock()
	if !hold {
		f.pcSend(whatsmeow.QRChannelItem{
			Event: whatsmeow.QRChannelEventCode, Code: "w1://pc-raw-qr", Timeout: 20 * time.Second,
		}, false)
	}
	return nil
}

func (f *pcFakePairSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconn++
}

func (f *pcFakePairSession) Device() *store.Device { return f.dev }

func (f *pcFakePairSession) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev != nil && f.dev.ID != nil && len(f.order) > 0
}

func (f *pcFakePairSession) PairPhone(ctx context.Context, phone string, showPushNotification bool,
	clientType whatsmeow.PairClientType, clientDisplayName string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "PairPhone")
	f.pairCalls++
	f.gotCtx, f.gotPhone, f.gotShow = ctx, phone, showPushNotification
	f.gotType, f.gotName = clientType, clientDisplayName
	if f.pairErr != nil {
		return "", f.pairErr
	}
	code := f.pairCode
	if code == "" {
		code = "Q4H5-JK7M"
	}
	return code, nil
}

// pcPairArgs 读出五实参留档快照。
func (f *pcFakePairSession) pcPairArgs() (context.Context, string, bool, whatsmeow.PairClientType, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotCtx, f.gotPhone, f.gotShow, f.gotType, f.gotName
}

func (f *pcFakePairSession) pcCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *pcFakePairSession) pcPairCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pairCalls
}

// pcClose 幂等关通道（锁内 closed 标记，杜绝「向已关闭通道发送」）。
func (f *pcFakePairSession) pcClose() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.out)
}

// pcSend 在锁内投递事件；通道已关闭时静默跳过（清理竞态下的迟到推送不该炸测试）。
// terminal=true 投完即关（库口径：终局项之后立即 close）。
func (f *pcFakePairSession) pcSend(item whatsmeow.QRChannelItem, terminal bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.out <- item
	if terminal {
		f.closed = true
		close(f.out)
	}
}

// pcPairSuccess 模拟「用户在手机上输完码」：库先写 JID 并自动 Save，再推终局
// success 项（与 wa_pairing_test.go 的 pairSuccess 同一复刻口径）。
func (f *pcFakePairSession) pcPairSuccess(jid string) {
	j, err := types.ParseJID(jid)
	if err != nil {
		panic(err) // 测试代码里的编程错，直接炸出来
	}
	f.mu.Lock()
	f.dev.ID = &j
	f.mu.Unlock()
	if f.store != nil {
		f.store.saveDevice(f.dev)
	}
	f.pcSend(whatsmeow.QRChannelSuccess, true)
}

// pcTimeout 推终局 timeout 项收尾会话，防活动表/记账/协程跨用例残留。
func (f *pcFakePairSession) pcTimeout() {
	f.pcSend(whatsmeow.QRChannelTimeout, true)
}

var _ waPairSession = (*pcFakePairSession)(nil)

// pcPairTracker 记录工厂造出的全部假码配对会话（并发安全）。
type pcPairTracker struct {
	mu   sync.Mutex
	list []*pcFakePairSession
}

func (tr *pcPairTracker) built() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.list)
}

func (tr *pcPairTracker) last() *pcFakePairSession {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.list) == 0 {
		return nil
	}
	return tr.list[len(tr.list)-1]
}

// pcInstallPairFactory 换掉 waPairSessionFactory，t.Cleanup 还原全局挂点。
// configure 在创建当场调用，用于预置 holdCode/pairErr 分支。
func pcInstallPairFactory(t *testing.T, st *fakeStore, configure func(*pcFakePairSession)) *pcPairTracker {
	t.Helper()
	tr := &pcPairTracker{}
	old := waPairSessionFactory
	oldSettle := waPairReloginSettle
	waPairReloginSettle = 0
	waPairSessionFactory = func(dev *store.Device, log waLog.Logger) waPairSession {
		f := &pcFakePairSession{dev: dev, store: st, out: make(chan whatsmeow.QRChannelItem, 8)}
		if configure != nil {
			configure(f)
		}
		tr.mu.Lock()
		tr.list = append(tr.list, f)
		tr.mu.Unlock()
		return f
	}
	t.Cleanup(func() {
		waPairSessionFactory = old
		waPairReloginSettle = oldSettle
	})
	return tr
}

// pcCaptureQREvents 给 kind=QR 的广播挂收集器（与 WebSocket 端点消费的是同一条
// publishWA 流），返回读取口；测完自动注销，不污染 util 全局表。
func pcCaptureQREvents(t *testing.T) func() []QREvent {
	t.Helper()
	var mu sync.Mutex
	var got []QREvent
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"QR", func(payload any) {
		ev, ok := payload.(*QREvent)
		if !ok {
			t.Errorf("QR 广播载荷类型不对: %T", payload)
			return
		}
		mu.Lock()
		cp := *ev
		got = append(got, cp)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"QR", id) })
	return func() []QREvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]QREvent(nil), got...)
	}
}

// pcClaimed 读出某号码的单飞记账（进行中返回会话 id，未登记返回空串）。
func pcClaimed(digits string) string {
	v, ok := waPairCodeActive.Load(digits)
	if !ok {
		return ""
	}
	id, _ := v.(string)
	return id
}

// pcNewDeviceCount 读 fakeStore 的 NewDevice 调用次数（断言「零建连」用）。
func (f *fakeStore) pcNewDeviceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newCount
}

// pcLeakScan 在事件流里扫完整号码（契约 ⑧：任何载荷不得出现明文号码）。
// 口径限定：success 事件的 JID 字段是既有泵语义——Lead 裁决「配对成功只广播
// JID 给业务方显式启动」（wa_pairing.go 文件头），广播 JID 正是该流程的设计
// 输出，不归本槽位脱敏约束；门面自己写入的字段（Code/Error）与全部失败事件
// 仍逐字禁明文。
func pcLeakScan(t *testing.T, events []QREvent, digits string) {
	t.Helper()
	if digits == "" {
		return
	}
	for _, ev := range events {
		fields := []string{ev.Code, ev.Error}
		if ev.Event != whatsmeow.QRChannelSuccess.Event {
			fields = append(fields, ev.JID)
		}
		for _, field := range fields {
			if strings.Contains(field, digits) {
				t.Errorf("事件载荷泄露完整号码 %s: %+v", digits, ev)
			}
		}
	}
}

// TestPairCodeSuccessFlow 成功主链路（契约 ①②⑦）：调用顺序逐字断言；PairPhone
// 五实参逐字断言；pair_code 事件码形态 XXXX-XXXX；终局 success 带 JID；不落 clients
// 表（不自动 StartClient）；事件走同一条 publishWA("QR") 流。
func TestPairCodeSuccessFlow(t *testing.T) {
	m, st, _ := startFakeWA(t, nil, nil)
	tr := pcInstallPairFactory(t, st, nil)
	events := pcCaptureQREvents(t)

	const digits = "8613800000001"
	code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0001")
	if err != nil {
		t.Fatalf("门面同步返回失败: %v", err)
	}
	if code == "" || sessionID == "" {
		t.Fatalf("code/sessionID 不能为空: %q %q", code, sessionID)
	}
	// 返回的 sessionID 必须能查回活动表里的会话——即真复用了 StartPairing 的注册。
	sess, ok := m.LookupQRSession(sessionID)
	if !ok {
		t.Fatal("返回的会话 id 不在活动表里（没走 StartPairing 的注册路径？）")
	}

	fake := tr.last()
	// ② 顺序：QRChannel 先于 Connect、Connect 先于 PairPhone，且各恰好一次。
	want := []string{"QRChannel", "Connect", "PairPhone"}
	calls := fake.pcCalls()
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] || calls[2] != want[2] {
		t.Fatalf("调用顺序不符: %v（期望 %v）", calls, want)
	}
	// ① 五实参逐字断言（号码为归一形态）。
	gotCtx, gotPhone, gotShow, gotType, gotName := fake.pcPairArgs()
	if gotPhone != digits {
		t.Errorf("PairPhone 号码应为归一形态 %q，实得 %q", digits, gotPhone)
	}
	if !gotShow {
		t.Error("showPushNotification 应为 true")
	}
	if gotType != whatsmeow.PairClientChrome {
		t.Errorf("clientType=%q，期望 %q", gotType, whatsmeow.PairClientChrome)
	}
	if gotName != "Chrome (Windows)" {
		t.Errorf("displayName=%q，期望 \"Chrome (Windows)\"", gotName)
	}
	if gotCtx == nil {
		t.Error("PairPhone 的 ctx 不得为空")
	}
	if n := fake.pcPairCount(); n != 1 {
		t.Errorf("PairPhone 应恰好调一次，实得 %d 次", n)
	}

	// ⑦ pair_code 事件经同一个会话扇出，码形态 XXXX-XXXX。
	sub, unsub := sess.Subscribe()
	defer unsub()
	ev := readEvent(t, sub, "pair_code 事件")
	if ev.Event != QREventPairCode || ev.SessionID != sessionID {
		t.Fatalf("pair_code 事件不符: %+v", ev)
	}
	if !regexp.MustCompile(`^[0-9A-Z]{4}-[0-9A-Z]{4}$`).MatchString(ev.Code) || ev.Code != code {
		t.Errorf("配对码事件形态/取值不符: %q vs 门面返回 %q", ev.Code, code)
	}

	// 用户在手机上输完码：假件复刻「库写 JID + 自动 Save + 终局 success」。
	const newJID = digits + "@s.whatsapp.net"
	fake.pcPairSuccess(newJID)
	ev = readEvent(t, sub, "success 事件")
	if ev.Event != "success" || ev.JID != newJID {
		t.Fatalf("终局 success 事件不符: %+v", ev)
	}
	waitUntil(t, 2*time.Second, "会话出活动表", func() bool {
		_, gone := m.LookupQRSession(sessionID)
		return !gone
	})
	if saved := st.savedList(); len(saved) != 1 || saved[0] != newJID {
		t.Fatalf("码配对成功后设备应经库侧自动 Save 落库: %v", saved)
	}
	// 既有裁决：配对成功不得自动 StartClient。
	if got := m.ListClients(); len(got) != 0 {
		t.Fatalf("码配对成功后不得自动 StartClient，实得: %+v", got)
	}
	// 记账随会话终局清除（号码不被锁死）。
	waitUntil(t, 2*time.Second, "单飞记账清理", func() bool { return pcClaimed(digits) == "" })
	// 广播口径：pair_code 与 success 都走了同一条 publishWA("QR") 流，且不露明文号码。
	var sawPairCode bool
	for _, e := range events() {
		if e.Event == QREventPairCode && e.Code == code {
			sawPairCode = true
		}
	}
	if !sawPairCode {
		t.Fatal("pair_code 事件未经 publishWA(QR) 广播（外部仓收不到）")
	}
	pcLeakScan(t, events(), digits)
}

// TestPairCodeFacadeNotStarted 消息侧未启动：原样返回 errWaNotStarted，不联网。
func TestPairCodeFacadeNotStarted(t *testing.T) {
	WhatsappStop(nil) // 与 TestFacadesFailClosedWhenNotStarted 同口径兜底
	st := newFakeStore()
	tr := pcInstallPairFactory(t, st, nil)
	code, id, err := WhatsappGetPairingCode(context.Background(), "8613800000002")
	if !errors.Is(err, errWaNotStarted) {
		t.Fatalf("未启动时应原样返回 errWaNotStarted: %v", err)
	}
	if code != "" || id != "" {
		t.Errorf("拒绝路径不得返回 code/sessionID: %q %q", code, id)
	}
	if tr.built() != 0 {
		t.Errorf("未启动时不得构造配对会话，实得 %d 个", tr.built())
	}
}

// TestPairCodeInvalidPhoneZeroNetwork 非法号码在建 Client 之前就拒（契约 ③），
// 文案含约定片段且只露掩码（契约 ⑧）。
func TestPairCodeInvalidPhoneZeroNetwork(t *testing.T) {
	cases := []struct {
		name   string
		phone  string
		digits string
		want   string
	}{
		{"过短", "+123 456", "123456", "号码过短"},
		{"恰好6位", "123456", "123456", "号码过短"},
		{"以0开头", "+0 138 0000 0003", "013800000003", "号码需为国际号码（不能以 0 开头）"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, st, _ := startFakeWA(t, nil, nil)
			tr := pcInstallPairFactory(t, st, nil)
			code, id, err := WhatsappGetPairingCode(context.Background(), tc.phone)
			if err == nil {
				t.Fatalf("非法号码应被拒，实得 code=%q session=%q", code, id)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误文案应含 %q: %v", tc.want, err)
			}
			if code != "" || id != "" {
				t.Errorf("拒绝路径不得返回 code/sessionID: %q %q", code, id)
			}
			if tr.built() != 0 {
				t.Errorf("非法号码不得构造配对会话，实得 %d 个", tr.built())
			}
			if n := st.pcNewDeviceCount(); n != 0 {
				t.Errorf("非法号码不得 NewDevice，实得 %d 次", n)
			}
			if strings.Contains(err.Error(), tc.digits) {
				t.Errorf("错误文案泄露完整号码: %v", err)
			}
		})
	}
}

// TestPairCodeHandshakeTimeout 首个 code 项迟迟不来（注入短握手上限）：门面报错
// （文案含「配对码握手超时」）、error 事件经同一会话广播、会话出活动表、
// 单飞记账被清（契约 ④），且事件/文案不露完整号码（契约 ⑧）。
func TestPairCodeHandshakeTimeout(t *testing.T) {
	setPairCodeHandshakeTimeout(t, 80*time.Millisecond)

	m, st, _ := startFakeWA(t, nil, nil)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })
	events := pcCaptureQREvents(t)

	const digits = "8613800000004"
	code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0004")
	if err == nil {
		t.Fatalf("握手超时应当报错，实得 code=%q", code)
	}
	if code != "" {
		t.Errorf("失败路径不得返回 code: %q", code)
	}
	if !strings.Contains(err.Error(), "配对码握手超时") {
		t.Errorf("错误文案应含「配对码握手超时」: %v", err)
	}
	if strings.Contains(err.Error(), digits) {
		t.Errorf("错误文案泄露完整号码: %v", err)
	}
	if sessionID == "" {
		t.Error("失败路径也应把会话 id 带回便于排障")
	}
	// 既有 defer 清理必须覆盖失败路径：会话出活动表 + 记账删除。
	waitUntil(t, 3*time.Second, "超时后活动表清理", func() bool {
		_, ok := m.LookupQRSession(sessionID)
		return !ok
	})
	waitUntil(t, 3*time.Second, "超时后单飞记账清理", func() bool { return pcClaimed(digits) == "" })
	// 终局 error 事件送达同一条流。
	var sawError bool
	for _, ev := range events() {
		if ev.Event == "error" && strings.Contains(ev.Error, "配对码握手超时") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("握手超时未广播 error 事件")
	}
	pcLeakScan(t, events(), digits)
	if fake := tr.last(); fake == nil || fake.pcPairCount() != 0 {
		t.Error("首个 code 项未到，不得调用 PairPhone")
	}
}

// TestPairCodePairPhoneErrorAndRetry PairPhone 被拒：门面报错、error 事件广播、
// 会话清理、记账删除，且同号可立即重发（契约 ⑤）。
func TestPairCodePairPhoneErrorAndRetry(t *testing.T) {
	m, st, _ := startFakeWA(t, nil, nil)
	reject := atomic.Bool{}
	reject.Store(true)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) {
		if reject.Load() {
			f.pairErr = errors.New("pair refused by server")
		}
	})
	events := pcCaptureQREvents(t)

	const digits = "8613800000005"
	code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0005")
	if err == nil {
		t.Fatalf("PairPhone 报错时门面应返回错误，实得 code=%q", code)
	}
	if !strings.Contains(err.Error(), "pair refused by server") {
		t.Errorf("错误应透传库侧原因: %v", err)
	}
	if strings.Contains(err.Error(), digits) {
		t.Errorf("错误文案泄露完整号码: %v", err)
	}
	var sawError bool
	for _, ev := range events() {
		if ev.Event == "error" && strings.Contains(ev.Error, "pair refused") {
			sawError = true
		}
		if ev.Event == QREventPairCode {
			t.Errorf("PairPhone 未成功不得广播 pair_code: %+v", ev)
		}
	}
	if !sawError {
		t.Fatal("PairPhone 报错未广播 error 事件")
	}
	waitUntil(t, 3*time.Second, "报错后活动表清理", func() bool {
		_, ok := m.LookupQRSession(sessionID)
		return !ok
	})
	waitUntil(t, 3*time.Second, "报错后单飞记账清理", func() bool { return pcClaimed(digits) == "" })

	// 同号立即重发：放开设拒后重试应成功拿码。
	reject.Store(false)
	code2, session2, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0005")
	if err != nil {
		t.Fatalf("失败清理后同号应可重发: %v", err)
	}
	if code2 == "" || session2 == "" {
		t.Fatalf("重发应同步返回码与会话: %q %q", code2, session2)
	}
	if tr.built() != 2 {
		t.Errorf("应先后构造两个假会话，实得 %d", tr.built())
	}
	tr.last().pcTimeout()
	waitUntil(t, 3*time.Second, "重发会话收尾后记账清理", func() bool { return pcClaimed(digits) == "" })
	pcLeakScan(t, events(), digits)
}

// TestPairCodeSingleFlight 同号码单飞（契约 ⑥）：第一个码配对进行中时，
// 同号第二呼直接拒绝且不得建第二个会话。
func TestPairCodeSingleFlight(t *testing.T) {
	setPairCodeHandshakeTimeout(t, 30*time.Second)

	_, st, _ := startFakeWA(t, nil, nil)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })

	const digits = "8613800000006"
	type result struct {
		code, id string
		err      error
	}
	first := make(chan result, 1)
	go func() {
		c, id, e := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0006")
		first <- result{c, id, e}
	}()
	// 等第一呼完成「占位 + StartPairing + 回填会话 id」。
	waitUntil(t, 3*time.Second, "第一呼登记会话 id", func() bool { return pcClaimed(digits) != "" })

	code, id, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0006")
	if err == nil {
		t.Fatalf("进行中同号第二呼应被拒，实得 code=%q id=%q", code, id)
	}
	if !strings.Contains(err.Error(), "该号码已有进行中的配对码会话") {
		t.Errorf("错误文案应含「该号码已有进行中的配对码会话」: %v", err)
	}
	if strings.Contains(err.Error(), digits) {
		t.Errorf("错误文案泄露完整号码: %v", err)
	}
	if n := tr.built(); n != 1 {
		t.Errorf("第二呼不得建第二个会话，实得 %d 个", n)
	}

	// 放行第一呼：推首个 code 项，让它走完 PairPhone 正常返回。
	fake := tr.last()
	fake.pcSend(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "w1://pc-raw-qr", Timeout: 20 * time.Second,
	}, false)
	select {
	case r := <-first:
		if r.err != nil || r.code == "" {
			t.Fatalf("第一呼应成功拿码: code=%q err=%v", r.code, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("第一呼未在放行后返回")
	}
	fake.pcTimeout()
	waitUntil(t, 3*time.Second, "单飞记账清理", func() bool { return pcClaimed(digits) == "" })
}

// TestPairCodeRefreshAfterIssued 码已交给调用方后，同号再取必须换新码、取消旧会话。
func TestPairCodeRefreshAfterIssued(t *testing.T) {
	m, st, _ := startFakeWA(t, nil, nil)
	seq := 0
	codes := []string{"AAAA-1111", "BBBB-2222"}
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) {
		f.pairCode = codes[seq]
		if seq < len(codes)-1 {
			seq++
		}
	})

	const digits = "8613800000099"
	code1, id1, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0099")
	if err != nil {
		t.Fatalf("首次取码失败: %v", err)
	}
	if code1 != "AAAA-1111" {
		t.Fatalf("首次码=%q", code1)
	}

	code2, id2, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0099")
	if err != nil {
		t.Fatalf("已出码后重取应成功: %v", err)
	}
	if code2 != "BBBB-2222" {
		t.Fatalf("重取码=%q（不得回旧码）", code2)
	}
	if id2 == "" || id2 == id1 {
		t.Fatalf("重取应换新会话: %q → %q", id1, id2)
	}
	if tr.built() != 2 {
		t.Errorf("应构造两个会话，实得 %d", tr.built())
	}
	waitUntil(t, 3*time.Second, "旧会话出活动表", func() bool {
		_, ok := m.LookupQRSession(id1)
		return !ok
	})
	if _, ok := m.LookupQRSession(id2); !ok {
		t.Fatal("新会话应仍在活动表")
	}
	tr.last().pcTimeout()
	waitUntil(t, 3*time.Second, "新会话收尾后记账清理", func() bool { return pcClaimed(digits) == "" })
}

// TestWaMaskPhone 脱敏 helper 的表驱动用例（契约⑧的取值口径）。
func TestWaMaskPhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"8613800000001", "86*********01"}, // 13 位：前 2 后 2，中间逐位 *
		{"1234567", "12***67"},             // 恰好 7 位走保留形态
		{"123456", "******"},               // <7 全 *
		{"0", "*"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := waMaskPhone(tc.in); got != tc.want {
			t.Errorf("waMaskPhone(%q)=%q，期望 %q", tc.in, got, tc.want)
		}
	}
}
