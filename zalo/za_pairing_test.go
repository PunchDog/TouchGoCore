package zalo

// 本文件是扫码配对的活闸：PNG 原始字节→base64、迟到订阅补投历史、
// Expired 显式 Retry、Declined 终态唯一、TTL→timeout、同账号幂等与在线拒绝、
// 成功落库/安装、Upsert 失败不安装、运行中取消/停机清表。
//
// 全部用 fake store/SDK/conn，不连网、不连库；TTL 用例用 20-50ms 的真实短等待，
// 其余等待一律用 mgrWaitTrue（Gosched 自旋）或带 5s 上限的通道读，不猜调度。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrakk/zcago"
	"github.com/amrakk/zcago/session/auth"

	"touchgocore/util"
)

// errPairBoom 是扫码测试注入的通用失败，用于断言安全文案不泄漏底层错误。
var errPairBoom = errors.New("pair test boom")

// pairFakeSDK 是可编程的 zaSDK 假件：LoginQR 把 option/callback 交给测试，
// 并按 block/hold/conn/loginErr 决定返回时机。Login 不在本文件使用。
type pairFakeSDK struct {
	mu       sync.Mutex
	qrCalls  int
	opt      *zcago.LoginQROption
	cb       zcago.LoginQRCallback
	conn     zaConn
	loginErr error
	block    bool
	hold     chan struct{}
	relOnce  sync.Once
}

// Login 不在扫码测试范围内。
func (s *pairFakeSDK) Login(context.Context, zcago.Credentials) (zaConn, error) {
	return nil, errPairBoom
}

// LoginQR 记录调用并等待测试放行（hold）或 ctx 结束（block）。
func (s *pairFakeSDK) LoginQR(ctx context.Context, opt *zcago.LoginQROption, cb zcago.LoginQRCallback) (zaConn, error) {
	s.mu.Lock()
	s.qrCalls++
	s.opt = opt
	s.cb = cb
	conn, retErr, block, hold := s.conn, s.loginErr, s.block, s.hold
	s.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return conn, retErr
}

// release 放行被 hold 的 LoginQR（可重复调用）。
func (s *pairFakeSDK) release() {
	s.relOnce.Do(func() {
		s.mu.Lock()
		h := s.hold
		s.mu.Unlock()
		if h != nil {
			close(h)
		}
	})
}

// qrCount 返回 LoginQR 调用次数。
func (s *pairFakeSDK) qrCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qrCalls
}

// pairFakeActions 实现 auth.GeneratedActions/CommonActions，记录 Retry/Abort 次数。
// zcago 的 qrController 未导出，测试用这个假件塞进事件里即可。
type pairFakeActions struct {
	mu      sync.Mutex
	retries int
	aborts  int
}

func (a *pairFakeActions) Retry(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retries++
	return nil
}

func (a *pairFakeActions) Abort(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborts++
	return nil
}

func (a *pairFakeActions) SaveToFile(context.Context, string) error { return nil }

// counts 返回 Retry/Abort 次数。
func (a *pairFakeActions) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.retries, a.aborts
}

// pairNewManager 构造带停机清理的 manager；qrTTL>0 时覆盖缺省 TTL。
func pairNewManager(t *testing.T, store *fakeZAStore, sdk *pairFakeSDK, qrTTL time.Duration) *zaManager {
	t.Helper()
	lim := mgrTestLimits()
	if qrTTL > 0 {
		lim.qrTTL = qrTTL
	}
	m := newZAManager(context.Background(), lim, store, sdk, nil, nil)
	t.Cleanup(m.Stop)
	return m
}

// pairInstallManager 把 manager 存进进程级快照（ZaloStartPairing 经 currentManager 取）。
func pairInstallManager(t *testing.T, m *zaManager) {
	t.Helper()
	old := zaMgr.Load()
	zaMgr.Store(m)
	t.Cleanup(func() { zaMgr.Store(old) })
}

// pairStart 发起配对并取回会话对象。
func pairStart(t *testing.T, m *zaManager, accountID string) *QRSession {
	t.Helper()
	sid, err := ZaloStartPairing(context.Background(), accountID)
	if err != nil {
		t.Fatalf("ZaloStartPairing(%q): %v", accountID, err)
	}
	s, ok := m.LookupQRSession(sid)
	if !ok {
		t.Fatalf("刚创建的会话 %s 查不到", sid)
	}
	return s
}

// pairWaitCallback 等到 LoginQR 注册回调。
func pairWaitCallback(t *testing.T, sdk *pairFakeSDK) zcago.LoginQRCallback {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sdk.mu.Lock()
		cb := sdk.cb
		sdk.mu.Unlock()
		if cb != nil {
			return cb
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 LoginQR 注册回调超时")
		}
		runtime.Gosched()
	}
}

// pairNext 读一条事件；通道关闭返回 ok=false。
func pairNext(t *testing.T, ch <-chan QREvent) (QREvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("等待事件超时")
		return QREvent{}, false
	}
}

// pairDrain 读到通道关闭为止（终态后会话关闭订阅）。
func pairDrain(t *testing.T, ch <-chan QREvent) []QREvent {
	t.Helper()
	out := make([]QREvent, 0, 4)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("等待事件通道关闭超时（已收到 %d 条）", len(out))
		}
	}
}

// pairHistory 返回会话事件历史快照。
func pairHistory(s *QRSession) []QREvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QREvent(nil), s.history...)
}

// pairEventCount 统计历史里某类事件的条数。
func pairEventCount(events []QREvent, name string) int {
	n := 0
	for _, ev := range events {
		if ev.Event == name {
			n++
		}
	}
	return n
}

// pairQRCounts 返回活动表规模（qrByID, qrByAccount）。
func pairQRCounts(m *zaManager) (int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.qrByID), len(m.qrByAccount)
}

// TestZAPairingQRImageRawPNGBase64 断言 zcago 的原始 PNG 字节被原样 base64
// （含非 UTF-8 字节），而不是把图片当成已是 base64 再编码一次。
func TestZAPairingQRImageRawPNGBase64(t *testing.T) {
	sdk := &pairFakeSDK{hold: make(chan struct{})}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	t.Cleanup(sdk.release)

	sess := pairStart(t, m, "acct")
	cb := pairWaitCallback(t, sdk)
	sub, _ := sess.Subscribe()

	// 事件面同时覆盖 util.CallZaloMsg+"QR"（冻结 QREvent 注释承诺的广播口径）。
	pubKey := util.CallZaloMsg + "QR"
	pubCh := make(chan QREvent, 1)
	pubID := util.DefaultCallFunc.Register(pubKey, func(ev *QREvent) {
		select {
		case pubCh <- *ev:
		default:
		}
	})
	defer util.DefaultCallFunc.Unregister(pubKey, pubID)

	// 前 8 字节是 PNG 魔数；0xff/0xfe/0x80 都不是合法 UTF-8 单字节，
	// 若实现走 string 再当 base64 解（或 JSON 直传）会在这里露馅。
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x7f, 0x80}
	cb(auth.EventQRCodeGenerated{Data: auth.QRGeneratedData{Code: "code-1", Image: string(raw)}})

	select {
	case pub := <-pubCh:
		if pub.Event != qrEventQR || pub.QRCode != "code-1" || pub.SessionID != sess.ID {
			t.Fatalf("QR 回调广播载荷不符: %+v", pub)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("util.CallZaloMsg+QR 未收到二维码事件")
	}

	ev, ok := pairNext(t, sub)
	if !ok {
		t.Fatal("收到通道关闭，期望二维码帧")
	}
	if ev.Event != qrEventQR || ev.QRCode != "code-1" {
		t.Fatalf("二维码帧不符: %+v", ev)
	}
	if ev.SessionID != sess.ID || ev.AccountID != "acct" {
		t.Fatalf("会话标识未填充: %+v", ev)
	}
	got, err := base64.StdEncoding.DecodeString(ev.ImageBase64)
	if err != nil {
		t.Fatalf("ImageBase64 不是合法 base64: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("base64 往返后字节不一致: got=%x want=%x", got, raw)
	}
	if !bytes.Contains(got, []byte{0xff, 0xfe}) || !bytes.Contains(got, []byte{0x80}) {
		t.Fatal("非 UTF-8 字节在编码中被改写")
	}
	if ev.TimeoutMS <= 0 {
		t.Fatalf("TimeoutMS=%d，期望正的剩余 TTL", ev.TimeoutMS)
	}

	sdk.mu.Lock()
	opt := sdk.opt
	sdk.mu.Unlock()
	if opt == nil || opt.UserAgent != "test-ua" || opt.Language != "vi" {
		t.Fatalf("LoginQROption=%+v，期望取自 limits 的 ua/vi", opt)
	}
}

// TestZAPairingLateSubscribeReplaysHistory 断言「先 emit 二维码、后 Subscribe」
// 的迟到连接仍能拿到二维码图片。
func TestZAPairingLateSubscribeReplaysHistory(t *testing.T) {
	sdk := &pairFakeSDK{hold: make(chan struct{})}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	t.Cleanup(sdk.release)

	sess := pairStart(t, m, "acct")
	cb := pairWaitCallback(t, sdk)
	raw := []byte("png-early")
	cb(auth.EventQRCodeGenerated{Data: auth.QRGeneratedData{Code: "code-early", Image: string(raw)}})
	mgrWaitTrue(t, "二维码事件入历史", func() bool { return len(pairHistory(sess)) == 1 })

	sub, _ := sess.Subscribe()
	ev, ok := pairNext(t, sub)
	if !ok {
		t.Fatal("迟到订阅拿到关闭通道，期望补投历史")
	}
	if ev.Event != qrEventQR || ev.QRCode != "code-early" {
		t.Fatalf("补投事件不符: %+v", ev)
	}
	if ev.ImageBase64 != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("补投的图片 base64=%q", ev.ImageBase64)
	}
}

// TestZAPairingExpiredRetriesAndDeclinedTerminal 覆盖两个回调控制流：
// expired 非终态且每个过期的码恰好 Retry 一次；declined 是终态且只发一次。
func TestZAPairingExpiredRetriesAndDeclinedTerminal(t *testing.T) {
	t.Run("expired_retries", func(t *testing.T) {
		sdk := &pairFakeSDK{hold: make(chan struct{})}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)
		t.Cleanup(sdk.release)

		sess := pairStart(t, m, "acct")
		cb := pairWaitCallback(t, sdk)
		sub, _ := sess.Subscribe()
		actions := &pairFakeActions{}

		cb(auth.EventQRCodeExpired{Actions: actions})
		ev, ok := pairNext(t, sub)
		if !ok || ev.Event != qrEventExpired {
			t.Fatalf("首个过期事件不符: %+v ok=%v", ev, ok)
		}
		mgrWaitTrue(t, "首个 Retry", func() bool { r, _ := actions.counts(); return r == 1 })
		if sess.isClosed() {
			t.Fatal("expired 是非终态，不应关闭会话")
		}

		cb(auth.EventQRCodeExpired{Actions: actions})
		ev, ok = pairNext(t, sub)
		if !ok || ev.Event != qrEventExpired {
			t.Fatalf("第二个过期事件不符: %+v ok=%v", ev, ok)
		}
		mgrWaitTrue(t, "第二个 Retry", func() bool { r, _ := actions.counts(); return r == 2 })
		if _, a := actions.counts(); a != 0 {
			t.Fatalf("expired 不应调 Abort，实得 %d 次", a)
		}
	})

	t.Run("declined_terminal_once", func(t *testing.T) {
		sdk := &pairFakeSDK{hold: make(chan struct{})}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)
		t.Cleanup(sdk.release)

		sess := pairStart(t, m, "acct")
		cb := pairWaitCallback(t, sdk)
		sub, _ := sess.Subscribe()
		actions := &pairFakeActions{}

		cb(auth.EventQRCodeDeclined{Actions: actions})
		events := pairDrain(t, sub)
		if len(events) != 1 || events[0].Event != qrEventDeclined {
			t.Fatalf("declined 终态不符: %+v", events)
		}
		if r, a := actions.counts(); r != 0 || a != 1 {
			t.Fatalf("Retry/Abort=%d/%d，期望 0/1", r, a)
		}
		if !sess.isClosed() {
			t.Fatal("declined 后会话应关闭")
		}

		// 第二次 declined：终态只发一次，不得再进历史。
		cb(auth.EventQRCodeDeclined{Actions: actions})
		if n := pairEventCount(pairHistory(sess), qrEventDeclined); n != 1 {
			t.Fatalf("declined 终态出现 %d 次，期望 1", n)
		}
	})
}

// TestZAPairingTTLTimeout 断言会话 TTL 到点时 LoginQR 被 ctx 打断，
// 终态是 timeout（唯一一条），且会话从活动表摘除。
func TestZAPairingTTLTimeout(t *testing.T) {
	sdk := &pairFakeSDK{block: true}
	m := pairNewManager(t, newFakeZAStore(), sdk, 40*time.Millisecond)
	pairInstallManager(t, m)

	sess := pairStart(t, m, "acct")
	sub, _ := sess.Subscribe()
	events := pairDrain(t, sub)
	if len(events) != 1 || events[0].Event != qrEventTimeout {
		t.Fatalf("TTL 终态不符: %+v", events)
	}
	if events[0].Error != "" || events[0].QRCode != "" {
		t.Fatalf("timeout 事件不应带错误/二维码: %+v", events[0])
	}
	if !errors.Is(sess.ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("会话 ctx err=%v，期望 DeadlineExceeded", sess.ctx.Err())
	}
	mgrWaitTrue(t, "会话从活动表移除", func() bool {
		_, ok := m.LookupQRSession(sess.ID)
		return !ok
	})
}

// TestZAPairingAccountIdempotentAndBusy 断言同账号二次 Pairing 返回原 sessionID、
// 不同账号互不影响、已在线账号返回 ErrZaloAccountBusy，以及非法账号 ID 透传。
func TestZAPairingAccountIdempotentAndBusy(t *testing.T) {
	sdk := &pairFakeSDK{hold: make(chan struct{})}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	t.Cleanup(sdk.release)

	sess := pairStart(t, m, "acct")
	mgrWaitTrue(t, "LoginQR 进入", func() bool { return sdk.qrCount() == 1 })

	sid2, err := ZaloStartPairing(context.Background(), "acct")
	if err != nil {
		t.Fatalf("同账号二次 Pairing: %v", err)
	}
	if sid2 != sess.ID {
		t.Fatalf("同账号二次 Pairing 返回新 id=%s，期望原 id=%s", sid2, sess.ID)
	}
	if got := sdk.qrCount(); got != 1 {
		t.Fatalf("幂等路径仍新建了 LoginQR，调用 %d 次", got)
	}

	if other := pairStart(t, m, "other"); other.ID == sess.ID {
		t.Fatal("不同账号复用了同一个 sessionID")
	}

	if _, err := ZaloStartPairing(context.Background(), ".."); !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("非法账号 err=%v，期望 ErrZaloBadAccountID", err)
	}

	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("账号不在内存")
	}
	acc.opMu.Lock()
	installed := acc.installConnLocked(&fakeZAConn{uid: "uid-live"})
	acc.opMu.Unlock()
	if !installed {
		t.Fatal("预置在线 conn 失败")
	}
	if _, err := ZaloStartPairing(context.Background(), "acct"); !errors.Is(err, ErrZaloAccountBusy) {
		t.Fatalf("已在线账号 Pairing err=%v，期望 ErrZaloAccountBusy", err)
	}
}

// TestZAPairingSuccessInstallsConn 断言成功路径：落库、安装 conn、success 带 UID、
// 会话从活动表摘除。
func TestZAPairingSuccessInstallsConn(t *testing.T) {
	store := newFakeZAStore()
	conn := &fakeZAConn{uid: "uid-qr", snap: zcago.Credentials{IMEI: "imei-snap", UserAgent: "ua-snap"}}
	sdk := &pairFakeSDK{conn: conn, hold: make(chan struct{})}
	m := pairNewManager(t, store, sdk, 0)
	pairInstallManager(t, m)

	sess := pairStart(t, m, "acct")
	sub, _ := sess.Subscribe()
	sdk.release()

	events := pairDrain(t, sub)
	if len(events) != 1 || events[0].Event != qrEventSuccess || events[0].UID != "uid-qr" {
		t.Fatalf("success 终态不符: %+v", events)
	}
	if store.upsertCount() != 1 {
		t.Fatalf("store.Upsert 调用 %d 次，期望 1", store.upsertCount())
	}
	rec, ok := store.record("acct")
	if !ok || rec.UID != "uid-qr" {
		t.Fatalf("落库行不符: %+v ok=%v", rec, ok)
	}
	var parsed zcago.Credentials
	if err := json.Unmarshal(rec.Credentials, &parsed); err != nil {
		t.Fatalf("落库凭证不是 Credentials JSON: %v", err)
	}
	if parsed.IMEI != "imei-snap" {
		t.Fatalf("落库 IMEI=%q，期望会话快照值", parsed.IMEI)
	}
	acc, ok := m.account("acct")
	if !ok || acc.currentConn() != conn {
		t.Fatal("成功后未安装 conn")
	}
	mgrWaitTrue(t, "会话从活动表移除", func() bool {
		_, found := m.LookupQRSession(sess.ID)
		return !found
	})
}

// TestZAPairingUpsertFailureNoInstall 断言写库失败发安全文案的 error 终态、
// 不安装 conn，且底层错误不进事件。
func TestZAPairingUpsertFailureNoInstall(t *testing.T) {
	store := newFakeZAStore()
	store.upsertErr = errPairBoom
	conn := &fakeZAConn{uid: "uid-qr", snap: zcago.Credentials{IMEI: "imei-snap", UserAgent: "ua-snap"}}
	sdk := &pairFakeSDK{conn: conn, hold: make(chan struct{})}
	m := pairNewManager(t, store, sdk, 0)
	pairInstallManager(t, m)

	sess := pairStart(t, m, "acct")
	sub, _ := sess.Subscribe()
	sdk.release()

	events := pairDrain(t, sub)
	if len(events) != 1 || events[0].Event != qrEventError {
		t.Fatalf("写库失败终态不符: %+v", events)
	}
	if events[0].Error != qrSafeLoginError {
		t.Fatalf("Error=%q，期望固定安全文案 %q", events[0].Error, qrSafeLoginError)
	}
	if strings.Contains(events[0].Error, "boom") {
		t.Fatal("底层错误泄漏进了终态事件")
	}
	if store.upsertCount() != 1 {
		t.Fatalf("store.Upsert 调用 %d 次，期望 1", store.upsertCount())
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("账号不在内存")
	}
	if acc.currentConn() != nil {
		t.Fatal("写库失败仍安装了 conn")
	}
}

// TestZAPairingCancelAndStopClosesSessions 断言运行中取消/停机都会关闭会话、
// 清空两张活动表，且不向订阅者发误导性终态。
func TestZAPairingCancelAndStopClosesSessions(t *testing.T) {
	t.Run("cancel_by_account", func(t *testing.T) {
		sdk := &pairFakeSDK{block: true}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)

		sess := pairStart(t, m, "acct")
		sub, _ := sess.Subscribe()
		m.cancelQRByAccount("acct")

		if events := pairDrain(t, sub); len(events) != 0 {
			t.Fatalf("取消不应发终态，实得 %+v", events)
		}
		if !sess.isClosed() {
			t.Fatal("取消后会话未关闭")
		}
		if ids, accs := pairQRCounts(m); ids != 0 || accs != 0 {
			t.Fatalf("取消后活动表未清空: byID=%d byAccount=%d", ids, accs)
		}
	})

	t.Run("stop_closes_all", func(t *testing.T) {
		sdk := &pairFakeSDK{block: true}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)

		sess := pairStart(t, m, "acct")
		sub, _ := sess.Subscribe()
		m.Stop()

		if events := pairDrain(t, sub); len(events) != 0 {
			t.Fatalf("停机不应发终态，实得 %+v", events)
		}
		if !sess.isClosed() {
			t.Fatal("停机后会话未关闭")
		}
		if ids, accs := pairQRCounts(m); ids != 0 || accs != 0 {
			t.Fatalf("停产后活动表未清空: byID=%d byAccount=%d", ids, accs)
		}
	})
}
