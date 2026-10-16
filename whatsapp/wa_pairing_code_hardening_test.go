package whatsapp

// 手机号配对码门面的「负例与竞态补强」测试（T7）。全部不联网：经
// waPairSessionFactory 注入 pchFakePairSession（挂点用完由 t.Cleanup 还原）。
//
// 本文件只补 T5（wa_pairing_code_test.go）未覆盖的残余形态，不重复也不修改它：
// 调用方 ctx 取消、会话在首个 code 项之前终局、非 code 中间项不得被误取、
// 迟到订阅者补投、pair_code 事件的 JSON 逐字形态、跨号码并发与同号并发、
// StopAll 停机路径释放号码、done 钩子协程不残留、掩码边界与错误文案联测。
//
// 命名空间纪律：T5 用 pc 前缀，本文件新增 helper/类型一律 pch 前缀（同包重名即
// 编译错误）；既有 helper（startFakeWA、waitUntil、pcClaimed、pcCaptureQREvents、
// pcLeakScan、readEvent、fakeStore 等）按原样复用，一个字节都不改。

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// pchFakePairSession 是码配对的一次性假会话（T7 自有假件）。
//
// 与 T5 的 pcFakePairSession 的差别只在「首个 code 项之前」这一段可编排：
//   - closeFirst：Connect 后立刻关通道，一个项都不推（库侧「通道先关」形态，
//     qrchan.go:88-91 的 closed 分支）；
//   - preItems：Connect 后按序推若干个**非 code 中间项**，之后可再补首个 code 项
//     （qrchan.go:37-40 的四种事件形态；passkey 两态与 error 项都不是终局，
//     见 qrchan.go:158-186 推完即 return、不 close）；
//   - terminalFirst：先推一个终局项（timeout/err-*）再关通道（qrchan.go:203-207
//     「终局项之后立即 close」）。
//
// PairPhone 的返回值随「首个 code 项是否已经投递过」而不同：没见到 code 项就被调
// （握手误取了中间项）返回 earlyCode，见到后才返回 pairCode。这样「把第一个事件
// 当 code 项」的实现改动会让用例变红，而不是依旧拿到一个看起来正常的码。
type pchFakePairSession struct {
	dev   *store.Device
	store *fakeStore

	out       chan whatsmeow.QRChannelItem
	closeOnce sync.Once

	mu    sync.Mutex
	order []string
	// 行为开关（创建当场由 configure 预置，泵协程读取时也在锁内）。
	closeFirst    bool
	terminalFirst bool
	holdCode      bool
	preItems      []whatsmeow.QRChannelItem
	codeAfterPre  bool
	pairErr       error
	pairCode      string
	earlyCode     string

	closed                bool
	disconn               int
	sessCtx               context.Context
	codeSent              bool
	pairCalls             int
	gotPhone              string
	gotShow               bool
	gotType               whatsmeow.PairClientType
	gotName               string
	gotPairCtxHasDeadline bool
}

// pchFakePairCode / pchEarlyPairCode 是两个可区分的假码（形态与库一致：XXXX-XXXX）。
const (
	pchFakePairCode  = "K3PD-7MZA"
	pchEarlyPairCode = "EARL-YBAD"
)

func (f *pchFakePairSession) QRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	f.order = append(f.order, "QRChannel")
	f.sessCtx = ctx
	f.mu.Unlock()
	// 复刻库侧对 ctx 取消的处置：关通道 + 断连（qrchan.go:124-132）。
	// StopAll/Cancel 路径的收敛依赖这条，假件不做就会把「停机释放号码」判成红。
	go func() {
		<-ctx.Done()
		f.pchClose()
		f.Disconnect()
	}()
	return f.out, nil
}

func (f *pchFakePairSession) Connect() error {
	f.mu.Lock()
	f.order = append(f.order, "Connect")
	closeFirst := f.closeFirst
	terminalFirst := f.terminalFirst
	holdCode := f.holdCode
	preItems := append([]whatsmeow.QRChannelItem(nil), f.preItems...)
	codeAfterPre := f.codeAfterPre
	f.mu.Unlock()

	switch {
	case closeFirst:
		f.pchClose()
	case terminalFirst:
		f.pchSend(whatsmeow.QRChannelTimeout, true)
	default:
		for _, it := range preItems {
			f.pchSend(it, false)
		}
		if codeAfterPre || (!holdCode && len(preItems) == 0) {
			f.pchSend(whatsmeow.QRChannelItem{
				Event: whatsmeow.QRChannelEventCode, Code: "w1://pch-raw-qr", Timeout: 20 * time.Second,
			}, false)
		}
	}
	return nil
}

func (f *pchFakePairSession) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev != nil && f.dev.ID != nil
}

func (f *pchFakePairSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconn++
}

func (f *pchFakePairSession) Device() *store.Device { return f.dev }

func (f *pchFakePairSession) PairPhone(ctx context.Context, phone string, showPushNotification bool,
	clientType whatsmeow.PairClientType, clientDisplayName string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "PairPhone")
	f.pairCalls++
	f.gotPhone, f.gotShow = phone, showPushNotification
	f.gotType, f.gotName = clientType, clientDisplayName
	_, f.gotPairCtxHasDeadline = ctx.Deadline()
	if f.pairErr != nil {
		return "", f.pairErr
	}
	if !f.codeSent {
		// 握手把非 code 的中间项当成了首个 code 项——返回一个可区分的坏码。
		return f.earlyCode, nil
	}
	return f.pairCode, nil
}

// pchClose 幂等关通道（锁内 closed 标记，杜绝「向已关闭通道发送」）。
func (f *pchFakePairSession) pchClose() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.out)
}

// pchSend 锁内投递事件；通道已关闭时静默跳过。terminal=true 投完即关。
// 出通道容量 8、单条用例最多投 5 项，泵尚未开读也不会写满阻塞。
func (f *pchFakePairSession) pchSend(item whatsmeow.QRChannelItem, terminal bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	if item.Event == whatsmeow.QRChannelEventCode {
		f.codeSent = true
	}
	f.out <- item
	if terminal {
		f.closed = true
		close(f.out)
	}
}

// pchPushCode 手动补投首个 code 项（单飞/并发用例里放行在途调用用）。
func (f *pchFakePairSession) pchPushCode() {
	f.pchSend(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "w1://pch-raw-qr", Timeout: 20 * time.Second,
	}, false)
}

// pchTimeout 推终局 timeout 项收尾会话，防活动表/记账/协程跨用例残留。
func (f *pchFakePairSession) pchTimeout() { f.pchSend(whatsmeow.QRChannelTimeout, true) }

func (f *pchFakePairSession) pchCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *pchFakePairSession) pchPairCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pairCalls
}

// pchDisconn 读出假件被 Disconnect 的次数（StopAll 收会话的判据）。
func (f *pchFakePairSession) pchDisconn() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disconn
}

func (f *pchFakePairSession) pchArgs() (string, bool, whatsmeow.PairClientType, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotPhone, f.gotShow, f.gotType, f.gotName, f.gotPairCtxHasDeadline
}

var _ waPairSession = (*pchFakePairSession)(nil)

// pchPairTracker 记录工厂造出的全部假会话（并发安全）。
type pchPairTracker struct {
	mu   sync.Mutex
	list []*pchFakePairSession
}

func (tr *pchPairTracker) built() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.list)
}

func (tr *pchPairTracker) last() *pchFakePairSession {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.list) == 0 {
		return nil
	}
	return tr.list[len(tr.list)-1]
}

func (tr *pchPairTracker) all() []*pchFakePairSession {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]*pchFakePairSession(nil), tr.list...)
}

// pchTotalPairCalls 汇总全部假会话的 PairPhone 调用次数（断言「没被误取」用）。
func (tr *pchPairTracker) pchTotalPairCalls() int {
	n := 0
	for _, f := range tr.all() {
		n += f.pchPairCount()
	}
	return n
}

// pchInstallPairFactory 换掉 waPairSessionFactory，t.Cleanup 还原全局挂点。
func pchInstallPairFactory(t *testing.T, st *fakeStore, configure func(*pchFakePairSession)) *pchPairTracker {
	t.Helper()
	tr := &pchPairTracker{}
	old := waPairSessionFactory
	oldSettle := waPairReloginSettle
	waPairReloginSettle = 0
	waPairSessionFactory = func(dev *store.Device, log waLog.Logger) waPairSession {
		f := &pchFakePairSession{dev: dev, store: st, out: make(chan whatsmeow.QRChannelItem, 8), pairCode: pchFakePairCode, earlyCode: pchEarlyPairCode}
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

// pchSetHandshakeTimeout 注入握手上限，t.Cleanup 还原（t.Fatalf 路径也能还原）。
func pchSetHandshakeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := waPairCodeHandshakeTimeout
	waPairCodeHandshakeTimeout = d
	t.Cleanup(func() { waPairCodeHandshakeTimeout = old })
}

// pchActiveCount 读出单飞记账表的全局条目数（「没留下第二个占位」判据）。
func pchActiveCount() int {
	n := 0
	waPairCodeActive.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// pchActiveHas 读出某号码是否在记账表里（含值为空串的占位形态）。
func pchActiveHas(digits string) bool {
	_, ok := waPairCodeActive.Load(digits)
	return ok
}

// pchSessionCount 读出管理器活动表里的会话数。
func pchSessionCount(m *waManager) int {
	n := 0
	m.qr.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// pchAllStacks 抓全进程 goroutine 栈（自增长的缓冲，直到一次装得下）。
func pchAllStacks(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8192)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		if len(buf) >= 1<<24 {
			t.Fatalf("goroutine 栈抓不完（缓冲已 %d 字节）", len(buf))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// pchCodeGoroutines 数出当前仍停在 wa_pairing_code.go 里的协程条数。
// 判据按「协程块」计数而不是字符串出现次数：一条协程在同一文件里可能有多个帧。
func pchCodeGoroutines(t *testing.T) int {
	t.Helper()
	n := 0
	for _, block := range strings.Split(pchAllStacks(t), "\n\ngoroutine ") {
		if strings.Contains(block, "wa_pairing_code.go") {
			n++
		}
	}
	return n
}

// pchWaitManagerWG 在给定预算内等管理器常驻协程归零（泵协程泄漏判据）。
// 返回 false 表示还没归零——与 StopAll 的等待口径同形，只是给测试用。
func pchWaitManagerWG(m *waManager, budget time.Duration) bool {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

// pchWaitNoPhone 断言错误文案里没有完整号码、且掩码形态逐字在内。
func pchWaitNoPhone(t *testing.T, err error, digits, masked string) {
	t.Helper()
	if err == nil {
		t.Fatal("期望返回错误，实得 nil")
	}
	msg := err.Error()
	if digits != "" && strings.Contains(msg, digits) {
		t.Errorf("错误文案泄露完整号码 %q: %s", digits, msg)
	}
	if !strings.Contains(msg, "digits="+masked) {
		t.Errorf("错误文案应含掩码形态 digits=%s，实得: %s", masked, msg)
	}
}

// TestPairCodeHCallerCtxCancel 补强缺口 1：调用方 ctx 在握手等待中被取消。
//
// 钉住的生产行为：wa_pairing_code.go:158 用**调用方 ctx** 派生 handshakeCtx，
// :177-178 从 handshakeCtx.Done() 返回错误；:120-125 失败路径广播 error 帧并 Cancel
// 会话；:114-117 的 done 钩子随泵收尾删除号码记账。
// 改坏哪行会红：把 :158 的 context.WithTimeout(ctx, ...) 换成 context.Background()
// （丢掉调用方取消信号）→ 门面要等注入的 30s 上限才返回，5s 内的等待断言直接红。
func TestPairCodeHCallerCtxCancel(t *testing.T) {
	// 上限取得远长于下面的等待闸：返回慢就说明握手没接调用方 ctx。
	pchSetHandshakeTimeout(t, 30*time.Second)

	m, st, _ := startFakeWA(t, nil, nil)
	tr := pchInstallPairFactory(t, st, func(f *pchFakePairSession) { f.holdCode = true })
	events := pcCaptureQREvents(t)

	const digits = "8613800000021"
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		code, id string
		err      error
	}
	first := make(chan result, 1)
	go func() {
		c, id, e := WhatsappGetPairingCode(ctx, "+86 138-0000-0021")
		first <- result{c, id, e}
	}()
	// 等门面真进入握手等待（会话已登记、记账已回填会话 id），取消才有意义。
	waitUntil(t, 3*time.Second, "握手等待中会话与记账已登记", func() bool { return pcClaimed(digits) != "" })
	if n := pchCodeGoroutines(t); n < 2 {
		t.Fatalf("前置不成立：取消前应有 ≥2 条协程停在配对码门面里（调用方 + done 钩子），实得 %d", n)
	}
	cancel()

	var r result
	select {
	case r = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("调用方 ctx 取消后门门面未在 5s 内返回（握手没接调用方 ctx？注入上限是 30s）")
	}
	if r.err == nil {
		t.Fatalf("ctx 取消必须返回错误，实得 code=%q", r.code)
	}
	if r.code != "" {
		t.Errorf("失败路径不得返回 code: %q", r.code)
	}
	if r.id == "" {
		t.Error("失败路径也应把会话 id 带回便于排障")
	}
	pchWaitNoPhone(t, r.err, digits, waMaskPhone(digits))
	// 合流批订正（Lead）：取消与到点已拆成两支文案（T7 报告观察 2 的成因不可区分缺陷）。
	// 本用例是「调用方取消」支，断言随之收紧：必须命中「被取消」，且不得混报「超时」。
	if !strings.Contains(r.err.Error(), "配对码握手被取消") || strings.Contains(r.err.Error(), "配对码握手超时") {
		t.Errorf("调用方 ctx 取消应走「配对码握手被取消」分支而非超时: %v", r.err)
	}

	// 会话出活动表 + 记账删除（同号可重发）。
	waitUntil(t, 3*time.Second, "取消后活动表清理", func() bool {
		_, ok := m.LookupQRSession(r.id)
		return !ok
	})
	waitUntil(t, 3*time.Second, "取消后单飞记账清理", func() bool { return !pchActiveHas(digits) })
	if n := pchActiveCount(); n != 0 {
		t.Errorf("取消后全局号码记账应为 0，实得 %d", n)
	}
	// error 帧广播得出来（外部仓两条出口同文案）。
	deadline := time.Now().Add(3 * time.Second)
	var sawError bool
	for time.Now().Before(deadline) && !sawError {
		for _, ev := range events() {
			if ev.Event == "error" && strings.Contains(ev.Error, "配对码握手被取消") {
				sawError = true
				if ev.SessionID != r.id {
					t.Errorf("error 帧 session_id 不符: %+v", ev)
				}
			}
			if ev.Event == QREventPairCode {
				t.Errorf("没拿到码不得广播 pair_code: %+v", ev)
			}
		}
	}
	if !sawError {
		t.Fatal("调用方 ctx 取消未广播 error 事件")
	}
	pcLeakScan(t, events(), digits)
	if n := tr.pchTotalPairCalls(); n != 0 {
		t.Errorf("首个 code 项未到，不得调用 PairPhone，实得 %d 次", n)
	}
	// 泵收尾后不得留下阻塞中的 done 钩子协程。
	waitUntil(t, 3*time.Second, "取消后无残留配对码协程", func() bool { return pchCodeGoroutines(t) == 0 })
}

// TestPairCodeHTerminalBeforeCode 补强缺口 2：会话在首个 code 项之前就终局。
//
// 两种库侧形态（qrchan.go:88-91「通道已先关」与 :203-207「终局项之后立即 close」）
// 都必须命中 wa_pairing_code.go:165 的「配对会话在首个二维码项之前结束」文案，
// 并且号码记账同样被清（:114-117 的 done 钩子）、会话出活动表、不调 PairPhone。
//
// 改坏哪行会红：把 :165 的 `if !ok` 判断去掉（把「通道已关闭」当「继续等」）→
// 已关闭通道会立刻反复交出零值项，握手白烧到注入的 15s 上限、最后落到「配对码握手
// 超时」分支，本子测试的文案逐字断言红（红得慢 15 秒，但一定红）；
// 把 :124 的 s.Cancel() 删掉 → 会话残留在活动表，出表断言红。
func TestPairCodeHTerminalBeforeCode(t *testing.T) {
	cases := []struct {
		name   string
		close  bool
		termin bool
	}{
		{"通道直接关闭零项", true, false},
		{"先推timeout终局项", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pchSetHandshakeTimeout(t, 15*time.Second)
			m, st, _ := startFakeWA(t, nil, nil)
			closeFirst, terminalFirst := tc.close, tc.termin
			tr := pchInstallPairFactory(t, st, func(f *pchFakePairSession) {
				f.closeFirst = closeFirst
				f.terminalFirst = terminalFirst
			})
			events := pcCaptureQREvents(t)

			const digits = "8613800000022"
			code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0022")
			if err == nil {
				t.Fatalf("会话先终局必须返回错误，实得 code=%q", code)
			}
			if code != "" {
				t.Errorf("失败路径不得返回 code: %q", code)
			}
			if sessionID == "" {
				t.Error("已建会话的失败路径应把会话 id 带回（契约 §2.2 实测回写）")
			}
			if !strings.Contains(err.Error(), "配对会话在首个二维码项之前结束") {
				t.Errorf("错误文案应含「配对会话在首个二维码项之前结束」（契约 §5.1）: %v", err)
			}
			pchWaitNoPhone(t, err, digits, waMaskPhone(digits))

			waitUntil(t, 3*time.Second, "终局后活动表清理", func() bool {
				_, ok := m.LookupQRSession(sessionID)
				return !ok
			})
			waitUntil(t, 3*time.Second, "终局后单飞记账清理", func() bool { return !pchActiveHas(digits) })
			if n := pchActiveCount(); n != 0 {
				t.Errorf("终局后全局号码记账应为 0，实得 %d", n)
			}
			if n := pchSessionCount(m); n != 0 {
				t.Errorf("终局后活动表会话数应为 0，实得 %d", n)
			}
			if n := tr.pchTotalPairCalls(); n != 0 {
				t.Errorf("没有首个 code 项，不得调用 PairPhone，实得 %d 次", n)
			}
			var sawError, sawPairCode bool
			for _, ev := range events() {
				switch ev.Event {
				case "error":
					if strings.Contains(ev.Error, "配对会话在首个二维码项之前结束") {
						sawError = true
					}
				case QREventPairCode:
					sawPairCode = true
				}
			}
			if !sawError {
				t.Fatal("失败终局未广播 error 事件（外部仓两条出口看不到原因）")
			}
			if sawPairCode {
				t.Error("没拿到码不得广播 pair_code")
			}
			pcLeakScan(t, events(), digits)
			waitUntil(t, 3*time.Second, "终局后无残留配对码协程", func() bool { return pchCodeGoroutines(t) == 0 })
		})
	}
}

// TestPairCodeHPreCodeItems 补强缺口 3：首个事件是非 code 中间项时握手必须继续等。
//
// qrchan.go:37-40 的四种事件形态（code / error / passkey-request /
// passkey-confirmation）各来一遍：非 code 的三种必须先被泵原样扇出（钉
// wa_pairing.go:275-295 的默认分支转发，即 R-03 登记的「两态原样转发」事实），
// 再由随后的 code 项放行握手；假件在「没见 code 项就被调」时返回一个可区分的坏码，
// 所以把中间项误当 code 项的实现改动一定落红，而不是依旧拿到一个看着正常的码。
//
// 改坏哪行会红：把 wa_pairing_code.go:167 的
// `ev.Event == whatsmeow.QRChannelEventCode` 换成 `ev.Event != ""`（首项即取码）
// → 门面拿到 pchEarlyPairCode，返回值逐字断言与「PairPhone 恰一次」全红；
// 把 wa_pairing.go:295 的 s.emit(ev) 前加一层「码配对场景丢掉非 code 帧」→
// 中间帧扇出断言红。
func TestPairCodeHPreCodeItems(t *testing.T) {
	cases := []struct {
		name string
		item whatsmeow.QRChannelItem
		// wantEvent 是泵应当扇出的事件名逐字值（活闸规则 3：字符串契约写字面量）。
		wantEvent string
		wantError string
		// isCode 标记该项本身就是首个 code 项（对照组）。
		isCode bool
	}{
		{
			name:      "首个即code项",
			item:      whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "w1://pch-raw-qr", Timeout: 20 * time.Second},
			wantEvent: "code",
			isCode:    true,
		},
		{
			name:      "error项先行非终局",
			item:      whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("failed to send passkey confirmation automatically: send failed")},
			wantEvent: "error",
			wantError: "failed to send passkey confirmation automatically: send failed",
		},
		{
			name:      "passkey-request先行",
			item:      whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest},
			wantEvent: "passkey-request",
		},
		{
			name:      "passkey-confirmation先行",
			item:      whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyResponse},
			wantEvent: "passkey-confirmation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pchSetHandshakeTimeout(t, 15*time.Second)
			m, st, _ := startFakeWA(t, nil, nil)
			pre := tc.item
			tr := pchInstallPairFactory(t, st, func(f *pchFakePairSession) {
				f.preItems = []whatsmeow.QRChannelItem{pre}
				f.codeAfterPre = !tc.isCode
			})
			events := pcCaptureQREvents(t)

			const digits = "8613800000023"
			code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0023")
			if err != nil {
				t.Fatalf("中间项之后出现 code 项应当成功拿码: %v", err)
			}
			if code != pchFakePairCode {
				t.Errorf("配对码应为 %q，实得 %q（非 code 中间项被误取时假件返回 %q）",
					pchFakePairCode, code, pchEarlyPairCode)
			}
			fake := tr.last()
			if fake == nil {
				t.Fatal("工厂没造出会话")
			}
			if n := fake.pchPairCount(); n != 1 {
				t.Errorf("PairPhone 应恰好一次（每个中间项各调一次说明没盯 code 项），实得 %d 次", n)
			}
			want := []string{"QRChannel", "Connect", "PairPhone"}
			if got := fake.pchCalls(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
				t.Errorf("调用顺序不符: %v（期望 %v）", got, want)
			}
			if phone, _, _, _, _ := fake.pchArgs(); phone != digits {
				t.Errorf("PairPhone 号码应为归一形态 %q，实得 %q", digits, phone)
			}

			// 中间帧必须由泵原样扇出，且严格排在 pair_code 之前。
			var sawPre, sawPairCode bool
			preIndex, pairCodeIndex := -1, -1
			for i, ev := range events() {
				if ev.SessionID != sessionID {
					t.Errorf("事件 session_id 不符: %+v", ev)
				}
				if ev.Event == tc.wantEvent && (tc.wantError == "" || ev.Error == tc.wantError) {
					sawPre = true
					preIndex = i
				}
				if ev.Event == QREventPairCode && ev.Code == code {
					sawPairCode = true
					pairCodeIndex = i
				}
			}
			if !sawPre {
				t.Fatalf("中间项 %q 未被泵扇出（错误项还应带 Error=%q）", tc.wantEvent, tc.wantError)
			}
			if !sawPairCode {
				t.Fatalf("pair_code 事件未广播: %+v", events())
			}
			preBeforePairCode := preIndex < pairCodeIndex
			if !preBeforePairCode {
				t.Errorf("中间帧（下标 %d）应先于 pair_code（下标 %d）到达", preIndex, pairCodeIndex)
			}
			// 首个 code 项照常作为二维码帧广播（契约 §3 表第 2 行：取码场景忽略其内容），
			// 但它与 pair_code 是两条不同的帧：码字段必须是二维码原始串，不是 8 位配对码。
			var codeFrames int
			for _, ev := range events() {
				if ev.Event == "code" {
					codeFrames++
					if ev.Code != "w1://pch-raw-qr" {
						t.Errorf("code 帧的 Code 应为二维码原始串，实得 %q", ev.Code)
					}
					if ev.Code == code {
						t.Errorf("code 帧与 pair_code 帧串了同一载荷: %+v", ev)
					}
				}
			}
			if codeFrames != 1 {
				t.Errorf("应当恰好广播一条 code 帧（中间项不得伪装成 code 帧），实得 %d 条", codeFrames)
			}
			pcLeakScan(t, events(), digits)

			fake.pchTimeout()
			waitUntil(t, 3*time.Second, "会话收尾后记账清理", func() bool { return !pchActiveHas(digits) })
			waitUntil(t, 3*time.Second, "会话收尾后活动表清理", func() bool { return pchSessionCount(m) == 0 })
		})
	}
}

// TestPairCodeHLateSubscriberReplay 补强缺口 4：门面已返回配对码之后才 Subscribe()。
//
// 外部仓的真实链路是「拿 session id → 前端再开 WebSocket」，所以码到手那一条帧必须
// 能被迟到订阅者第一时间补投（wa_pairing.go:97-99 的 hasLast 补投语义 + :141-142 的
// emit 留档，二者经 QRSession 这条跨包接线落到本能力上）。会话终局后再订阅也要补投
// 终局那一条再关闭通道，迟到方才分得清「配对失败」与「没赶上」。
//
// 改坏哪行会红：
//   - 删 wa_pairing.go:97-99 的 `if s.hasLast { ch <- s.last }` → 第一条读事件阻塞到
//     readEvent 的 2s 上限，红；
//   - 删 wa_pairing.go:141-142 的 `s.last = ev; s.hasLast = true` → 同上红；
//   - 把 wa_pairing_code.go:128 的 `s.emit(QREvent{Event: QREventPairCode, ...})` 换成
//     直接 publishWA("QR", ...)（绕开会话扇出）→ 补投读到的是泵留的 code 帧而不是
//     pair_code，事件名逐字断言红（这正是 Lead 变异清单 M5 关心的跨包接线漏项）。
func TestPairCodeHLateSubscriberReplay(t *testing.T) {
	m, st, _ := startFakeWA(t, nil, nil)
	tr := pchInstallPairFactory(t, st, nil)

	const digits = "8613800000024"
	code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0024")
	if err != nil {
		t.Fatalf("门面同步返回失败: %v", err)
	}
	// 迟到者按外部仓口径先查会话再订阅（WS 端点走的就是同一个 LookupQRSession）。
	sess, ok := m.LookupQRSession(sessionID)
	if !ok {
		t.Fatal("门面的会话不在活动表里（WS 端点会 404）")
	}

	sub, unsub := sess.Subscribe()
	defer unsub()
	ev := readEvent(t, sub, "迟到订阅的补投帧")
	if ev.Event != "pair_code" {
		t.Fatalf("迟到订阅首帧应为 pair_code，实得 %q（完整事件 %+v）", ev.Event, ev)
	}
	if ev.Code != code || ev.Code != pchFakePairCode {
		t.Errorf("补投的配对码应为 %q，实得 %q（门面返回 %q）", pchFakePairCode, ev.Code, code)
	}
	if ev.SessionID != sessionID {
		t.Errorf("补投帧 session_id 不符: %+v", ev)
	}

	// 补投之后订阅仍是在册的：后续轮换帧要继续收到（不是一次性补投）。
	tr.last().pchSend(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "w1://pch-raw-qr-2", Timeout: 20 * time.Second,
	}, false)
	ev2 := readEvent(t, sub, "补投之后的实时帧")
	if ev2.Event != "code" || ev2.Code != "w1://pch-raw-qr-2" {
		t.Fatalf("补投后应继续收到实时帧，实得 %+v", ev2)
	}

	// 终局之后再来一个迟到订阅者：补投终局那一条，随后通道关闭。
	tr.last().pchTimeout()
	waitUntil(t, 3*time.Second, "会话终局出活动表", func() bool { return pchSessionCount(m) == 0 })
	lateSub, lateUnsub := sess.Subscribe()
	defer lateUnsub()
	ev3 := readEvent(t, lateSub, "终局后的补投帧")
	if ev3.Event != "timeout" {
		t.Fatalf("终局后的迟到订阅应补投 timeout，实得 %+v", ev3)
	}
	select {
	case _, ok := <-lateSub:
		if ok {
			t.Error("已结束会话的补投之后应关闭通道，实得还能再读一条")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("已结束会话的迟到订阅通道未关闭")
	}
	waitUntil(t, 3*time.Second, "终局后记账清理", func() bool { return !pchActiveHas(digits) })
}

// TestPairCodeHEventJSONShape 补强缺口 5：pair_code 事件的序列化字段逐字契约。
//
// 下游前端按 json tag 分支（ws_qr.go:174 用 conn.WriteJSON(ev) 直接把 QREvent 值逐帧
// 写出去），所以序列化出来的键名与顺序就是对外契约：必须恰好是
// `{"session_id":...,"event":"pair_code","code":...}` 三个键，其余字段因 omitempty 缺席。
// 字符串契约按活闸规则 3 写成字面量，不引用常量拼（常量漂移时两边一起动、测不出来）。
//
// 改坏哪行会红：wa_pairing.go:45-50 里任一个 tag 改名（如 `json:"sessionId"`）→
// 逐字比对红；wa_pairing_code.go:44 的 QREventPairCode 由 "pair_code" 改成别的值 →
// 本用例开头的事件名字面量断言红；:128 漏填 Code 字段 → code 键缺席，红。
func TestPairCodeHEventJSONShape(t *testing.T) {
	m, st, _ := startFakeWA(t, nil, nil)
	tr := pchInstallPairFactory(t, st, nil)

	const digits = "8613800000025"
	code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0025")
	if err != nil {
		t.Fatalf("门面同步返回失败: %v", err)
	}
	defer tr.last().pchTimeout()

	var ev QREvent
	// pair_code 由门面在返回前同步 emit（wa_pairing_code.go:128），此后本用例不再推
	// 任何帧，所以会话留档的「最近一条」就是它——按补投口径取帧，取的正是 WS 端点
	// 迟到订阅者看到的那一条载荷。
	sess, ok := m.LookupQRSession(sessionID)
	if !ok {
		t.Fatal("门面返回后会话应仍在活动表里（WS 端点要按 id 查它）")
	}
	sub, unsub := sess.Subscribe()
	defer unsub()
	ev = readEvent(t, sub, "pair_code 事件帧")

	if QREventPairCode != "pair_code" {
		t.Fatalf("事件名常量漂移：QREventPairCode=%q，契约 §2 逐字要求 \"pair_code\"", QREventPairCode)
	}
	if ev.Event != "pair_code" {
		t.Fatalf("事件名应为字面量 pair_code，实得 %q", ev.Event)
	}

	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("序列化 pair_code 事件失败: %v", err)
	}
	got := string(out)
	want := `{"session_id":"` + sessionID + `","event":"pair_code","code":"` + pchFakePairCode + `"}`
	if got != want {
		t.Errorf("pair_code 事件序列化不符逐字契约\n实得: %s\n期望: %s", got, want)
	}
	if ev.Code != code {
		t.Errorf("事件里的码应与门面返回值逐字一致: %q vs %q", ev.Code, code)
	}
	if !regexp.MustCompile(`^qr-[0-9a-f]{32}$`).MatchString(sessionID) {
		t.Errorf("session_id 形态不符（契约 §2.2）: %q", sessionID)
	}

	// 键集必须恰好三条：error/timeout_ms/jid 因 omitempty 不得出现空键。
	var keys map[string]any
	if err := json.Unmarshal(out, &keys); err != nil {
		t.Fatalf("序列化结果不是合法 JSON: %v", err)
	}
	if len(keys) != 3 {
		t.Errorf("pair_code 帧应恰好 3 个键，实得 %d 个: %v", len(keys), keys)
	}
	for _, required := range []string{"session_id", "event", "code"} {
		if _, ok := keys[required]; !ok {
			t.Errorf("pair_code 帧缺键 %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{"error", "timeout_ms", "jid", "Error", "SessionId", "sessionID", "pair_code"} {
		if _, ok := keys[forbidden]; ok {
			t.Errorf("pair_code 帧不该出现键 %q: %s", forbidden, got)
		}
	}
	if strings.Contains(got, digits) {
		t.Errorf("序列化载荷泄露完整号码: %s", got)
	}
	pcLeakScan(t, []QREvent{ev}, digits)
	tr.last().pchTimeout()
	waitUntil(t, 3*time.Second, "收场后记账清理", func() bool { return !pchActiveHas(digits) })
}

// TestPairCodeHConcurrentPhones 补强缺口 6：并发形态两侧都钉。
//
// A 侧：不同号码并发发起不得互相锁死——两条握手各自拿到码、各占一条会话、
// 各调一次 PairPhone 且号码互不串（waPairCodeActive 的键是归一后的号码，
// 单飞只该锁同号，锁错就成全局串行）。
// B 侧：同号并发第二呼必被拒（wa_pairing_code.go:98-101 的 LoadOrStore 分支），
// 且**不会**留下第二个会话：活动表计数=1、号码记账计数=1（连空串占位都不许多一条）。
//
// 改坏哪行会红：把 :98 的 LoadOrStore 换成 Store（覆盖在册条目）→ B 侧「第二呼
// 没被拒」或「活动表出现 2 条会话」红；把键从 digits 换成 s.ID（按会话记账）→
// B 侧拒绝断言与「放号后可重发」红；A 侧若实现加了全局互斥锁，第二条号码拿不到码即红。
func TestPairCodeHConcurrentPhones(t *testing.T) {
	t.Run("不同号码并发互不锁死", func(t *testing.T) {
		pchSetHandshakeTimeout(t, 15*time.Second)
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pchInstallPairFactory(t, st, nil)
		if n := pchActiveCount(); n != 0 {
			t.Fatalf("前置不成立：号码记账表开局应为空，实得 %d 条（有用例漏放号）", n)
		}

		const digitsA = "8613800000041"
		const digitsB = "8613900000042"
		type result struct {
			code, id string
			err      error
		}
		chA, chB := make(chan result, 1), make(chan result, 1)
		go func() {
			c, id, e := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0041")
			chA <- result{c, id, e}
		}()
		go func() {
			c, id, e := WhatsappGetPairingCode(context.Background(), "+86 139-0000-0042")
			chB <- result{c, id, e}
		}()

		var rA, rB result
		select {
		case rA = <-chA:
		case <-time.After(8 * time.Second):
			t.Fatal("号码 A 并发发起时没拿到码（被别的号码锁死？）")
		}
		select {
		case rB = <-chB:
		case <-time.After(8 * time.Second):
			t.Fatal("号码 B 并发发起时没拿到码（被别的号码锁死？）")
		}
		if rA.err != nil || rA.code == "" || rA.id == "" {
			t.Fatalf("号码 A 应成功: code=%q id=%q err=%v", rA.code, rA.id, rA.err)
		}
		if rB.err != nil || rB.code == "" || rB.id == "" {
			t.Fatalf("号码 B 应成功: code=%q id=%q err=%v", rB.code, rB.id, rB.err)
		}
		if rA.id == rB.id {
			t.Errorf("两个号码不得共用一条会话: %q", rA.id)
		}
		if n := tr.built(); n != 2 {
			t.Errorf("应造出两条会话，实得 %d 条", n)
		}
		if n := pchSessionCount(m); n != 2 {
			t.Errorf("活动表应有两条会话，实得 %d 条", n)
		}
		if pcClaimed(digitsA) != rA.id || pcClaimed(digitsB) != rB.id {
			t.Errorf("号码记账应按各自号码分别登记: A=%q B=%q", pcClaimed(digitsA), pcClaimed(digitsB))
		}
		// 号码互不串：两条会话各自的 PairPhone 实参必须是自己那个号码。
		var sawA, sawB bool
		for _, f := range tr.all() {
			phone, _, _, _, _ := f.pchArgs()
			switch phone {
			case digitsA:
				sawA = true
			case digitsB:
				sawB = true
			default:
				t.Errorf("PairPhone 收到意外号码 %q", phone)
			}
			if n := f.pchPairCount(); n != 1 {
				t.Errorf("每条会话 PairPhone 应恰一次，实得 %d 次", n)
			}
		}
		if !sawA || !sawB {
			t.Fatalf("两个号码应各自调用 PairPhone（A=%v B=%v）", sawA, sawB)
		}

		for _, f := range tr.all() {
			f.pchTimeout()
		}
		waitUntil(t, 3*time.Second, "两号码收场后放号", func() bool { return pchActiveCount() == 0 })
		waitUntil(t, 3*time.Second, "两号码收场后活动表清空", func() bool { return pchSessionCount(m) == 0 })
	})

	t.Run("同号并发第二呼被拒且不留第二个会话", func(t *testing.T) {
		pchSetHandshakeTimeout(t, 30*time.Second)
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pchInstallPairFactory(t, st, func(f *pchFakePairSession) { f.holdCode = true })
		if n := pchActiveCount(); n != 0 {
			t.Fatalf("前置不成立：号码记账表开局应为空，实得 %d 条", n)
		}

		const digits = "8613800000043"
		type result struct {
			code, id string
			err      error
		}
		first := make(chan result, 1)
		go func() {
			c, id, e := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0043")
			first <- result{c, id, e}
		}()
		waitUntil(t, 3*time.Second, "第一呼登记会话 id", func() bool { return pcClaimed(digits) != "" })

		code, id, err := WhatsappGetPairingCode(context.Background(), "+8613800000043")
		if err == nil {
			t.Fatalf("同号进行中时第二呼必须被拒，实得 code=%q id=%q", code, id)
		}
		if code != "" || id != "" {
			t.Errorf("拒绝路径不得返回 code/sessionID: %q %q", code, id)
		}
		if !strings.Contains(err.Error(), "该号码已有进行中的配对码会话") {
			t.Errorf("错误文案应含「该号码已有进行中的配对码会话」（契约 §5.1）: %v", err)
		}
		pchWaitNoPhone(t, err, digits, waMaskPhone(digits))
		// 被拒的第二呼不许建会话、也不许多留一条记账（含空串占位形态）。
		if n := tr.built(); n != 1 {
			t.Errorf("第二呼不得建第二个会话，实得 %d 条", n)
		}
		if n := pchSessionCount(m); n != 1 {
			t.Errorf("活动表计数应为 1，实得 %d", n)
		}
		if n := pchActiveCount(); n != 1 {
			t.Errorf("号码记账计数应为 1，实得 %d（第二呼多留了占位）", n)
		}

		// 放行第一呼拿码，再收终局：号码必须可重发（记账随会话清理删除）。
		tr.last().pchPushCode()
		var r result
		select {
		case r = <-first:
		case <-time.After(5 * time.Second):
			t.Fatal("放行后第一呼未返回")
		}
		if r.err != nil || r.code != pchFakePairCode {
			t.Fatalf("第一呼应成功拿码: code=%q err=%v", r.code, r.err)
		}
		tr.last().pchTimeout()
		waitUntil(t, 3*time.Second, "同号收场后放号", func() bool { return pchActiveCount() == 0 })

		// 放号后同号立即可重发（记账没漏删才走得通）。
		// 重发段用短上限：只要证明它越过了单飞闸、进到握手等待即可，不必真等 30s。
		pchSetHandshakeTimeout(t, 80*time.Millisecond)
		tr2 := pchInstallPairFactory(t, st, func(f *pchFakePairSession) { f.holdCode = true })
		_, _, err = WhatsappGetPairingCode(context.Background(), "+86 138-0000-0043")
		if err == nil || !strings.Contains(err.Error(), "配对码握手超时") {
			t.Fatalf("放号后同号应重新进入握手等待（而不是被单飞拒绝），实得 err=%v", err)
		}
		if n := tr2.built(); n != 1 {
			t.Errorf("重发应新建一条会话，实得 %d 条", n)
		}
		tr2.last().pchTimeout()
		waitUntil(t, 3*time.Second, "重发收场后放号", func() bool { return pchActiveCount() == 0 })
	})
}

// TestPairCodeHStopAllReleasesNumber 补强缺口 7：停机路径（StopAll）不得把号码锁死。
//
// StopAll 对进行中的配对会话逐个 Cancel（wa_manager.go:230-235）→ 会话 ctx 取消 →
// 库关通道并断连（qrchan.go:124-132）→ 泵 defer 收尾 close(s.done)（wa_pairing.go:262）
// → wa_pairing_code.go:114-117 的 done 钩子删除号码记账。
// 两个时机各一遍：码已到手正在等用户输码（真实停机形态）、握手仍在等待中。
//
// 改坏哪行会红：删掉 wa_pairing_code.go:114-117 的钩子协程 → 停机后号码永久锁死，
// 「记账已删」与「同号可重发」两条断言红；删掉 wa_pairing.go:262 的
// `defer close(s.done)` → 钩子解开不了，除了上面两条红，还多一条协程残留被缺口 8
// 的计数闸抓到。
func TestPairCodeHStopAllReleasesNumber(t *testing.T) {
	t.Run("码到手后停机", func(t *testing.T) {
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pchInstallPairFactory(t, st, nil)
		events := pcCaptureQREvents(t)

		const digits = "8613800000051"
		code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0051")
		if err != nil {
			t.Fatalf("门面同步返回失败: %v", err)
		}
		if code != pchFakePairCode {
			t.Errorf("停机前应已拿到配对码 %q，实得 %q", pchFakePairCode, code)
		}
		if pcClaimed(digits) != sessionID {
			t.Fatalf("配对进行中号码应登记会话 id，实得 %q", pcClaimed(digits))
		}
		// 停机前钩子协程确实在 parked 状态（证明下面的清理不是空跑）。
		if n := pchCodeGoroutines(t); n < 1 {
			t.Fatalf("前置不成立：停机前应有 done 钩子协程停在配对码门面里，实得 %d", n)
		}

		m.StopAll(time.Second)

		waitUntil(t, 3*time.Second, "停机后活动表清空", func() bool { return pchSessionCount(m) == 0 })
		waitUntil(t, 3*time.Second, "停机后号码放号", func() bool { return !pchActiveHas(digits) })
		if n := pchActiveCount(); n != 0 {
			t.Errorf("停机后全局号码记账应为 0，实得 %d（悬挂条目会把号码永久锁死）", n)
		}
		if n := tr.last().pchDisconn(); n < 1 {
			t.Errorf("StopAll 应经 Cancel 断连，实得 Disconnect %d 次", n)
		}
		waitUntil(t, 3*time.Second, "停机后无残留配对码协程", func() bool { return pchCodeGoroutines(t) == 0 })
		pcLeakScan(t, events(), digits)

		// 停机收场后同号可重发（换一台进程继续配对不该被记账卡住）。
		tr2 := pchInstallPairFactory(t, st, func(f *pchFakePairSession) { f.holdCode = true })
		pchSetHandshakeTimeout(t, 60*time.Millisecond)
		_, _, err = WhatsappGetPairingCode(context.Background(), "+86 138-0000-0051")
		if err == nil || !strings.Contains(err.Error(), "配对码握手超时") {
			t.Fatalf("停机后同号应能重新发起握手，实得 err=%v", err)
		}
		if n := tr2.built(); n != 1 {
			t.Errorf("重发应新建一条会话，实得 %d 条", n)
		}
		waitUntil(t, 3*time.Second, "重发握手超时后放号", func() bool { return pchActiveCount() == 0 })
	})

	t.Run("握手等待中停机", func(t *testing.T) {
		pchSetHandshakeTimeout(t, 30*time.Second)
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pchInstallPairFactory(t, st, func(f *pchFakePairSession) { f.holdCode = true })

		const digits = "8613800000052"
		type result struct {
			code, id string
			err      error
		}
		done := make(chan result, 1)
		go func() {
			c, id, e := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0052")
			done <- result{c, id, e}
		}()
		waitUntil(t, 3*time.Second, "握手等待中登记会话 id", func() bool { return pcClaimed(digits) != "" })

		m.StopAll(time.Second)

		var r result
		select {
		case r = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("StopAll 后门门面未在 5s 内返回（握手等待没被会话清理解开？）")
		}
		if r.err == nil {
			t.Fatalf("握手未完成即停机必须返回错误，实得 code=%q", r.code)
		}
		if r.code != "" {
			t.Errorf("失败路径不得返回 code: %q", r.code)
		}
		if !strings.Contains(r.err.Error(), "配对会话在首个二维码项之前结束") {
			t.Errorf("停机打断握手应命中「配对会话在首个二维码项之前结束」分支，实得: %v", r.err)
		}
		pchWaitNoPhone(t, r.err, digits, waMaskPhone(digits))
		waitUntil(t, 3*time.Second, "停机后号码放号", func() bool { return !pchActiveHas(digits) })
		if n := pchSessionCount(m); n != 0 {
			t.Errorf("停机后活动表应为空，实得 %d 条", n)
		}
		if n := tr.last().pchDisconn(); n < 1 {
			t.Errorf("停机应断连，实得 Disconnect %d 次", n)
		}
		waitUntil(t, 3*time.Second, "停机后无残留配对码协程", func() bool { return pchCodeGoroutines(t) == 0 })
	})
}

// TestPairCodeHGoroutineLifecycle 补强缺口 8：成功/失败/超时三态都不留协程。
//
// 配对码门面自己起了一条不受 m.wg 记账的协程（wa_pairing_code.go:114-117 的 done
// 钩子），它阻塞在 <-s.Done() 上；泵协程收尾时 close(s.done)（wa_pairing.go:262）
// 把它解开。三态各跑完后必须：① 没有任何协程还停在 wa_pairing_code.go 里；
// ② 管理器 wg 归零（泵协程全退）。这里不用第三方 goleak（引它要改 go.mod，本任务禁止），
// 改用包内 runtime.Stack 计数断言，并按活闸规则 2 先在「码已到手、尚未终局」的窗口里
// 证明计数确实数得到那条钩子协程，再断言归零——不然「0」可能是空跑。
//
// 改坏哪行会红：删 wa_pairing.go:262 的 `defer close(s.done)` → done 永不关闭，
// 钩子协程永远停在 :115，计数归零断言红（泵协程本身仍会退出，所以只有这条抓得到）；
// 把 wa_pairing_code.go:116 的 Delete 写成空操作 → 记账残留，同号重发被永久拒绝。
func TestPairCodeHGoroutineLifecycle(t *testing.T) {
	cases := []struct {
		name      string
		digits    string
		configure func(*pchFakePairSession)
		timeout   bool
		wantErr   string
	}{
		{"成功拿码后终局", "8613800000071", nil, false, ""},
		{"PairPhone被拒", "8613800000072", func(f *pchFakePairSession) { f.pairErr = errors.New("pair refused by server") }, false, "PairPhone 被拒"},
		{"握手超时未到code项", "8613800000073", func(f *pchFakePairSession) { f.holdCode = true }, true, "配对码握手超时"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timeout {
				pchSetHandshakeTimeout(t, 80*time.Millisecond)
			} else {
				pchSetHandshakeTimeout(t, 15*time.Second)
			}
			m, st, _ := startFakeWA(t, nil, nil)
			configure := tc.configure
			tr := pchInstallPairFactory(t, st, configure)

			digits := tc.digits
			code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+"+digits)
			fake := tr.last()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("成功态不应报错: %v", err)
				}
				if code != pchFakePairCode {
					t.Errorf("成功态应拿到 %q，实得 %q", pchFakePairCode, code)
				}
				// 此刻 done 钩子必然还挂在 <-s.Done() 上（会话尚未终局）。
				if n := pchCodeGoroutines(t); n < 1 {
					t.Fatalf("前置不成立：应在协程栈里数到停在配对码门面的 done 钩子，实得 %d", n)
				}
				fake.pchTimeout()
			} else {
				if err == nil {
					t.Fatalf("失败态应报错，实得 code=%q", code)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("错误文案应含 %q: %v", tc.wantErr, err)
				}
				pchWaitNoPhone(t, err, digits, waMaskPhone(digits))
			}

			waitUntil(t, 3*time.Second, "三态收场后号码放号", func() bool { return !pchActiveHas(digits) })
			waitUntil(t, 3*time.Second, "三态收场后活动表清理", func() bool {
				_, ok := m.LookupQRSession(sessionID)
				return !ok
			})
			waitUntil(t, 3*time.Second, "三态收场后无停在配对码门面的协程", func() bool {
				return pchCodeGoroutines(t) == 0
			})
			if !pchWaitManagerWG(m, 3*time.Second) {
				t.Error("泵协程未全部退出（管理器 wg 没归零）")
			}
			if n := pchActiveCount(); n != 0 {
				t.Errorf("收场后全局号码记账应为 0，实得 %d", n)
			}
		})
	}
}

// TestPairCodeHMaskBoundaries 补强缺口 9：号码形态到掩码再到错误文案的整链。
//
// waMaskPhone 只看「归一后的数字串」，所以边界必须先过 WhatsappNormalizePhone：
// +86 带符号、空格/连字符/括号、恰好 7 位（走保留前 2 后 2 的下界）、6 位以下
// （走全 * 分支）、纯非数字（归一后空串）各来一遍，逐字断言掩码取值 +
// 「门面错误文案不含完整号码」（延续 pcLeakScan 思路，覆盖面放到文案侧与连续数字段）。
//
// 改坏哪行会红：wa_pairing_code.go:66 的 `len(digits) >= 7` 改成 `> 7` → 恰好 7 位
// 那一行落到全 * 分支，掩码逐字断言与文案断言红；:67 的切片边界改一格（如
// digits[len-1:]）→ 掩码多露一位，逐字断言红；:92/:95 的文案里 masked 换成 digits →
// 泄露完整号码断言与「5 位以上连续数字段」扫描红。
func TestPairCodeHMaskBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		phone    string
		digits   string
		mask     string
		wantFrag string // 错误文案必须含的分支片段
		precheck bool   // true = 联网前预检拒绝（零建连）
	}{
		{"带符号空格连字符", "+86 138-0000-0061", "8613800000061", "86*********61", "配对码握手超时", false},
		{"无分隔符", "+8613800000062", "8613800000062", "86*********62", "配对码握手超时", false},
		{"括号与国际前缀", "(+86) 138 0000 0063", "8613800000063", "86*********63", "配对码握手超时", false},
		{"恰好7位下界", "+1 234-567", "1234567", "12***67", "配对码握手超时", false},
		{"8位", "+12 345-678", "12345678", "12****78", "配对码握手超时", false},
		{"5位以下", "+123 45", "12345", "*****", "号码过短", true},
		{"恰好6位下界", "123-456", "123456", "******", "号码过短", true},
		{"纯非数字归一为空", "abc-def gh", "", "", "号码过短", true},
		{"前导0带空格", "+0 138 0000 0064", "013800000064", "01********64", "号码需为国际号码（不能以 0 开头）", true},
	}
	// 掩码取值逐字（不经门面，直接钉 helper 边界）。
	t.Run("waMaskPhone逐字", func(t *testing.T) {
		for _, tc := range cases {
			if got := waMaskPhone(tc.digits); got != tc.mask {
				t.Errorf("waMaskPhone(%q)=%q，期望 %q", tc.digits, got, tc.mask)
			}
		}
		if got := waMaskPhone("1234567890123456"); got != "12************56" {
			t.Errorf("长号码掩码中间位数应按 len-4 填充，实得 %q", got)
		}
	})

	// 归一口径逐字（掩码只看归一结果，归一错则整链错）。
	t.Run("归一形态逐字", func(t *testing.T) {
		for _, tc := range cases {
			if got := WhatsappNormalizePhone(tc.phone); got != tc.digits {
				t.Errorf("WhatsappNormalizePhone(%q)=%q，期望 %q", tc.phone, got, tc.digits)
			}
		}
	})

	t.Run("门面错误文案与号码隔离", func(t *testing.T) {
		pchSetHandshakeTimeout(t, 60*time.Millisecond)
		// 5 位以上连续数字段：掩码形态里不可能出现（前 2 后 2 之间全是 *），
		// 出现即说明有完整号码或过长片段泄进文案。
		longRun := regexp.MustCompile(`[0-9]{5,}`)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, st, _ := startFakeWA(t, nil, nil)
				tr := pchInstallPairFactory(t, st, func(f *pchFakePairSession) { f.holdCode = true })
				events := pcCaptureQREvents(t)

				code, sessionID, err := WhatsappGetPairingCode(context.Background(), tc.phone)
				if err == nil {
					t.Fatalf("该号码形态应返回错误，实得 code=%q", code)
				}
				if code != "" {
					t.Errorf("失败路径不得返回 code: %q", code)
				}
				msg := err.Error()
				if !strings.Contains(msg, tc.wantFrag) {
					t.Errorf("错误文案应含分支片段 %q: %s", tc.wantFrag, msg)
				}
				if !strings.Contains(msg, "digits="+tc.mask) {
					t.Errorf("错误文案应含掩码形态 digits=%s: %s", tc.mask, msg)
				}
				if tc.digits != "" && strings.Contains(msg, tc.digits) {
					t.Errorf("错误文案泄露完整号码 %q: %s", tc.digits, msg)
				}
				if m := longRun.FindString(msg); m != "" {
					t.Errorf("错误文案出现 5 位以上连续数字段 %q（掩码不该拼出长号码片段）: %s", m, msg)
				}
				if tc.precheck {
					if sessionID != "" {
						t.Errorf("预检拒绝不得有会话: %q", sessionID)
					}
					if n := tr.built(); n != 0 {
						t.Errorf("预检拒绝不得构造会话，实得 %d 条", n)
					}
					if n := st.pcNewDeviceCount(); n != 0 {
						t.Errorf("预检拒绝不得 NewDevice，实得 %d 次", n)
					}
					if n := pchActiveCount(); n != 0 {
						t.Errorf("预检拒绝不得留号码记账，实得 %d 条", n)
					}
					if len(events()) != 0 {
						t.Errorf("预检拒绝不该广播任何事件: %+v", events())
					}
				} else {
					if sessionID == "" {
						t.Error("过了预检的号码应带回会话 id")
					}
					if n := tr.built(); n != 1 {
						t.Errorf("过了预检的号码应建一条会话，实得 %d 条", n)
					}
					waitUntil(t, 3*time.Second, "握手超时后放号", func() bool { return pchActiveCount() == 0 })
					waitUntil(t, 3*time.Second, "握手超时后无残留协程", func() bool { return pchCodeGoroutines(t) == 0 })
					pcLeakScan(t, events(), tc.digits)
					for _, ev := range events() {
						if m := longRun.FindString(ev.Error); m != "" {
							t.Errorf("error 事件文案出现 5 位以上连续数字段 %q: %+v", m, ev)
						}
					}
				}
			})
		}
	})
}
