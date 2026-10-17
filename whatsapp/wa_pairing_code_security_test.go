package whatsapp

// 本文件是合流批（Lead）为 T8 安全审查的两条发现补的行为闸：
//   - WA-PC-01：码配对会话没有总量闸（max_concurrent 只数已启动账号，够不着配对这一支）；
//   - WA-PC-03：库的 IQError 在 Code==0 时整节点回显（errors.go:214-222），而 PairPhone
//     请求节点带明文 jid（pair-code.go:111），原样 %w 上抛会突破本包自己的号码掩码承诺。
//
// helper 前缀 pcs，与同包既有的 pc/pc 前缀互不重名（同包 *_test.go 共享一张符号表）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// pcsSetMaxInflight 注入在飞上限，t.Cleanup 还原（含 t.Fatalf 提前退出路径）。
func pcsSetMaxInflight(t *testing.T, n int) {
	t.Helper()
	old := waPairCodeMaxInflight
	waPairCodeMaxInflight = n
	t.Cleanup(func() { waPairCodeMaxInflight = old })
}

// pcsReleaseAll 让全部在飞假会话走终局，防跨用例残留把全局记账带脏。
func pcsReleaseAll(tr *pcPairTracker) {
	for _, f := range tr.all() {
		f.pcTimeout()
	}
}

// TestPairCodeSInflightCapRejectsExtraNumber 钉住 WA-PC-01 的闸：
// 名额占满后，换一个号码发起必须被拒、且一条会话都不许多建；名额释放后同号可进。
//
// 改坏哪一行会红：删掉 wa_pairing_code.go 里 `if n := waPairCodeInflight(); n >= waPairCodeMaxInflight`
// 整支 → 第三次调用会真去建会话，「built() 仍为 2」与「拒绝文案」两条断言同时红。
func TestPairCodeSInflightCapRejectsExtraNumber(t *testing.T) {
	_, st, _ := startFakeWA(t, nil, nil)
	// 在飞会话必须真的停在握手等待里，上限判定才有可观察的稳态。
	setPairCodeHandshakeTimeout(t, 30*time.Second)
	pcsSetMaxInflight(t, 2)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })

	const d1, d2, d3 = "8613800000081", "8613800000082", "8613800000083"
	var wg sync.WaitGroup
	for _, d := range []string{d1, d2} {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _, _ = WhatsappGetPairingCode(context.Background(), p)
		}(d)
	}
	// 前置非空跑：先证明两个名额真的被占住（否则「第三个被拒」是空闸）。
	waitUntil(t, 3*time.Second, "两个号码进入在飞", func() bool {
		return pcClaimed(d1) != "" && pcClaimed(d2) != "" && tr.built() == 2
	})
	if n := st.pcNewDeviceCount(); n != 2 {
		t.Fatalf("前置不成立：应已建 2 条一次性设备，实得 %d", n)
	}

	code, id, err := WhatsappGetPairingCode(context.Background(), d3)
	if err == nil {
		t.Fatalf("占满上限后第三个号码应被拒，实得 code=%q id=%q", code, id)
	}
	if code != "" || id != "" {
		t.Errorf("拒绝路径不得带回 code/session: %q %q", code, id)
	}
	if !strings.Contains(err.Error(), "在飞配对码会话已达上限 2") {
		t.Errorf("错误文案应命中在飞上限分支: %v", err)
	}
	pcWaitNoPhone(t, err, d3, waMaskPhone(d3))
	if n := tr.built(); n != 2 {
		t.Errorf("被拒的第三呼不该建会话，实得 %d 条", n)
	}
	if n := st.pcNewDeviceCount(); n != 2 {
		t.Errorf("被拒的第三呼不该 NewDevice，实得 %d 次", n)
	}

	// 放号：让两条在飞会话收场，记账清零后同一个号码应能重新进入握手。
	pcsReleaseAll(tr)
	wg.Wait()
	waitUntil(t, 3*time.Second, "上限闸随会话收场放号", func() bool { return pcActiveCount() == 0 })
	setPairCodeHandshakeTimeout(t, 80*time.Millisecond)
	if _, _, err = WhatsappGetPairingCode(context.Background(), d3); err == nil ||
		strings.Contains(err.Error(), "在飞配对码会话已达上限") {
		t.Fatalf("放号后应越过上限闸进入握手（这里的错应是握手超时），实得 err=%v", err)
	}
	if n := tr.built(); n != 3 {
		t.Errorf("放号后应新建第三条会话，实得 %d 条", n)
	}
	pcsReleaseAll(tr)
}

// TestPairCodeSCapHoldsUnderConcurrentBurst 钉住上限与占号同锁判定：
// 8 个互不相同的号码并发突发，在飞数任何时刻都不得越过上限（不锁则同时越过计数）。
//
// 改坏哪一行会红：去掉 waPairCodeClaimMu 的 Lock/Unlock（计数与占号不再串行）
// → 突发里多条同时通过 `n >= waPairCodeMaxInflight` 判定，built() 与 pcActiveCount()
// 的观测峰值越过 2，本用例红。
func TestPairCodeSCapHoldsUnderConcurrentBurst(t *testing.T) {
	_, st, _ := startFakeWA(t, nil, nil)
	setPairCodeHandshakeTimeout(t, 30*time.Second)
	pcsSetMaxInflight(t, 2)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })

	const n0 = 8
	phones := make([]string, n0)
	for i := range phones {
		phones[i] = fmt.Sprintf("8613800009%02d", i)
	}
	var wg sync.WaitGroup
	var done, rejected int64
	for i := range phones {
		wg.Add(1)
		go func(p string) {
			defer func() {
				atomic.AddInt64(&done, 1)
				wg.Done()
			}()
			if _, _, err := WhatsappGetPairingCode(context.Background(), p); err != nil &&
				strings.Contains(err.Error(), "在飞配对码会话已达上限") {
				atomic.AddInt64(&rejected, 1)
			}
		}(phones[i])
	}

	peak := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range []int{pcActiveCount(), tr.built()} {
			if n > peak {
				peak = n
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 先放号再判定，避免把还在握手里的协程算成残留。
	pcsReleaseAll(tr)
	waitUntil(t, 5*time.Second, "全部突发协程收场", func() bool { return atomic.LoadInt64(&done) == n0 })
	wg.Wait()

	if peak > 2 {
		t.Errorf("在飞峰值越过上限 2，实得 %d（计数与占号没串成单行道）", peak)
	}
	// 死闸自检：8 个号码全无拒绝即上限根本没生效。
	if atomic.LoadInt64(&rejected) == 0 {
		t.Error("8 个号码并发突发竟无一条被上限拒绝，闸是死的")
	}
	if n := pcActiveCount(); n != 0 {
		t.Errorf("突发收场后记账应清零，实得 %d 条", n)
	}
	if n := st.pcNewDeviceCount(); n > 2 {
		t.Errorf("被拒的呼叫不该 NewDevice，实得 %d 次", n)
	}
}

// TestPairCodeSSanitizesLibraryErrorText 钉住 WA-PC-03 的消毒：
// 库把带明文 jid 的节点整串回显时，门面返回值与广播帧都不得出现完整号码，
// 但错误分类（errors.Is 到库侧哨兵）必须原样保留。
//
// 改坏哪一行会红：把 waMaskErr 里的 strings.ReplaceAll 去掉（或改成直接 %w 上抛）
// → 错误文案与 error 帧含明文号码，两条断言同时红；
// 把返回值改成 errors.New(消毒后的串)（丢掉 Unwrap）→ errors.Is 断言红。
func TestPairCodeSSanitizesLibraryErrorText(t *testing.T) {
	_, st, _ := startFakeWA(t, nil, nil)
	pcsSetMaxInflight(t, 5)
	setPairCodeHandshakeTimeout(t, 2*time.Second)

	const digits = "8613800000091"
	masked := waMaskPhone(digits)
	sentinel := errors.New("pc-iq-sentinel")
	// 复刻库 IQError 在 Code==0 时的回显形态（errors.go:214-222），节点里带明文 jid。
	libErr := fmt.Errorf("info query returned unknown error: <iq from=\"%s\" type=\"error\">: %w", digits, sentinel)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.pairErr = libErr })
	events := pcCaptureQREvents(t)

	code, id, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0091")
	if err == nil {
		t.Fatalf("PairPhone 被拒必须返回错误，实得 code=%q id=%q", code, id)
	}
	if code != "" {
		t.Errorf("失败路径不得返回 code: %q", code)
	}
	if strings.Contains(err.Error(), digits) {
		t.Errorf("错误文案仍带完整号码（消毒没生效）: %v", err)
	}
	if !strings.Contains(err.Error(), masked) {
		t.Errorf("错误文案应带掩码形态 %s: %v", masked, err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("消毒不得吃掉错误分类，errors.Is 应仍命中库侧哨兵: %v", err)
	}
	if n := tr.last().pcPairCount(); n != 1 {
		t.Errorf("PairPhone 应恰好调用一次，实得 %d", n)
	}
	pcLeakScan(t, events(), digits)
	var sawError bool
	for _, ev := range events() {
		if ev.Event == "error" {
			sawError = true
			if strings.Contains(ev.Error, digits) {
				t.Errorf("广播的 error 帧带完整号码: %+v", ev)
			}
			if !strings.Contains(ev.Error, masked) {
				t.Errorf("广播的 error 帧应含掩码: %+v", ev)
			}
		}
	}
	if !sawError {
		t.Error("失败未经 error 帧广播（外部仓两条出口看不到原因）")
	}
	waitUntil(t, 3*time.Second, "失败后放号", func() bool { return pcActiveCount() == 0 })
}

// TestPairCodeSOnlyCodeItemTriggersPairPhone 是合流批补的闸（M1r 变异先 SURVIVED 后补）。
//
// 为什么要补：T7 的 TestPairCodeHPreCodeItems 让假件在 Connect 当场就把 preItems 与
// code 项连着推完，而门面是在 StartPairing 返回之后才 Subscribe——非 code 的中间项
// 早在订阅前就被泵吃掉，握手从来没见过它。于是把「只认 code 项」改成「认任何事件」
// （`ev.Event != ""`）整套仍全绿，属 fixture 没有判别力，不是实现正确。
// 本用例把时序钉死：先确认门面已进入握手（订阅已生效），**再**推中间项，
// 因此这条对生产条件是真判据。
//
// 改坏哪一行会红：wa_pairing_code.go 的 `if ev.Event == whatsmeow.QRChannelEventCode`
// 放宽成任意事件 → PairPhone 会在中间项后被调用（假件回 EARL-YBAD 坏码），
// 「中间项后不得取码」与「码值必须为 K3PD-7MZA」两条同时红。
func TestPairCodeSOnlyCodeItemTriggersPairPhone(t *testing.T) {
	_, st, _ := startFakeWA(t, nil, nil)
	setPairCodeHandshakeTimeout(t, 30*time.Second)
	pcsSetMaxInflight(t, 5)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })

	const digits = "8613800000093"
	type pcsRes struct {
		code, id string
		err      error
	}
	res := make(chan pcsRes, 1)
	go func() {
		c, id, e := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0093")
		res <- pcsRes{c, id, e}
	}()
	// 前置非空跑：门面已登记号码（说明过了单飞与上限，正停在握手等待里）。
	waitUntil(t, 3*time.Second, "门面进入握手等待", func() bool { return pcClaimed(digits) != "" })

	f := tr.last()
	if f == nil {
		t.Fatal("假会话未建出")
	}
	f.pcSend(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest}, false)
	time.Sleep(150 * time.Millisecond)
	if n := f.pcPairCount(); n != 0 {
		t.Fatalf("非 code 的中间项不该触发 PairPhone，实得 %d 次", n)
	}
	select {
	case r := <-res:
		t.Fatalf("中间项之后门面就返回了（把非 code 事件当成就了）: code=%q err=%v", r.code, r.err)
	default:
	}

	// 首个 code 项到达后应当立刻取码并同步返回。
	f.pcPushCode()
	var r pcsRes
	select {
	case r = <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("补推 code 项后门面 3s 内未返回")
	}
	if r.err != nil {
		t.Fatalf("成功路径不应报错: %v", r.err)
	}
	if r.code != pcFakePairCode {
		t.Errorf("配对码应为 %s（拿到 %s 说明握手误取了中间项）: %q", pcFakePairCode, pcEarlyPairCode, r.code)
	}
	if r.id == "" {
		t.Error("成功路径应带回会话 id")
	}
	if n := f.pcPairCount(); n != 1 {
		t.Errorf("PairPhone 应恰好一次，实得 %d", n)
	}
	if _, got, _, _, _ := f.pcArgs(); got != digits {
		t.Errorf("传给库的号码应是归一后的 %s，实得 %q", digits, got)
	}
	pcsReleaseAll(tr)
	waitUntil(t, 3*time.Second, "收场放号", func() bool { return pcActiveCount() == 0 })
}
