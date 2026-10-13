package whatsapp

// 防封闸集成的测试：假闸钉住「问闸→记账→拦截/失败回写」的接线与批内让位节奏，
// 真闸只做构造冒烟（不联网、不真发）。全部走 waGateFactory 挂点。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"touchgocore/config"
	"touchgocore/util"
)

type fakeBeforeCall struct {
	recipient string
	isGroup   bool
	text      string
}

type fakeAfterCall struct {
	recipient  string
	isGroup    bool
	wasNewChat bool
}

// fakeGate 是 waSendGate 的假实现：可编程延迟/拦截，记录三段调用序列。
type fakeGate struct {
	mu      sync.Mutex
	known   map[string]bool
	delayMs int
	block   bool
	before  []fakeBeforeCall
	after   []fakeAfterCall
	failed  int
}

func newFakeGate() *fakeGate { return &fakeGate{known: map[string]bool{}} }

func (g *fakeGate) IsKnownChat(jid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.known[jid]
}

func (g *fakeGate) BeforeSend(recipient, text string, isGroup bool, _ string, _ time.Time) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.before = append(g.before, fakeBeforeCall{recipient: recipient, isGroup: isGroup, text: text})
	if g.block {
		return 0, errWaAntibanBlocked
	}
	return g.delayMs, nil
}

func (g *fakeGate) AfterSend(recipient, _ string, isGroup bool, _ string, wasNew bool, _ time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.after = append(g.after, fakeAfterCall{recipient: recipient, isGroup: isGroup, wasNewChat: wasNew})
	g.known[recipient] = true
}

func (g *fakeGate) AfterSendFailed(_ time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failed++
}

var errWaAntibanBlocked = &antibanBlockedError{}

type antibanBlockedError struct{}

func (e *antibanBlockedError) Error() string { return "[antiban] message blocked" }

// installFakeGate 换掉 waGateFactory 返回假闸；sessions 段在而键缺省即开，用例仍显式写 true 表明意图。
func installFakeGate(t *testing.T) *fakeGate {
	t.Helper()
	fg := newFakeGate()
	old := waGateFactory
	waGateFactory = func(*config.WhatsappSessionsConfig, string) waSendGate { return fg }
	t.Cleanup(func() { waGateFactory = old })
	return fg
}

func waAcct(t *testing.T) (*waManager, *fakeSession, *fakeGate) {
	t.Helper()
	fg := installFakeGate(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, &config.WhatsappSessionsConfig{AntibanEnabled: waBool(true)}, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	return m, get(dev.ID.String()), fg
}

// TestAntibanWiredIntoSendPath 单发/群发都过闸：isGroup 判定、wasNewChat 首真后假、
// 延迟真睡、拦截回「防封闸拦截」且零发送、底层报错回写 AfterSendFailed。
func TestAntibanWiredIntoSendPath(t *testing.T) {
	_, s, fg := waAcct(t)

	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil {
		t.Fatal(err)
	}
	fg.mu.Lock()
	if len(fg.before) != 1 || fg.before[0].recipient != "8613900000000@s.whatsapp.net" || fg.before[0].isGroup {
		t.Fatalf("首条 BeforeSend 不符: %+v", fg.before)
	}
	if len(fg.after) != 1 || !fg.after[0].wasNewChat {
		t.Fatalf("首条 AfterSend 该记新会话: %+v", fg.after)
	}
	fg.mu.Unlock()

	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi2"); err != nil {
		t.Fatal(err)
	}
	fg.mu.Lock()
	wasNew := fg.after[1].wasNewChat
	fg.mu.Unlock()
	if wasNew {
		t.Error("第二条该已是已知会话")
	}

	if _, err := WhatsappSendGroupMessage(nil, "8613800000000", "12345@g.us", "hg"); err != nil {
		t.Fatal(err)
	}
	fg.mu.Lock()
	isGroup := fg.before[2].isGroup
	fg.mu.Unlock()
	if !isGroup {
		t.Error("群组收件人该按 isGroup=true 问闸")
	}

	// 延迟真睡：delayMs=250 时整条发送不该瞬间返回。
	fg.mu.Lock()
	fg.delayMs = 250
	fg.mu.Unlock()
	start := time.Now()
	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "slow"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Errorf("闸给的 250ms 延迟没睡够: %v", el)
	}

	// 拦截形态：错误带「防封闸拦截」，会话零触达。
	fg.mu.Lock()
	fg.block = true
	fg.mu.Unlock()
	_, _, _, sentBefore, _ := s.snapshot()
	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "nope"); err == nil ||
		!strings.Contains(err.Error(), "防封闸拦截") {
		t.Fatalf("拦截应回防封闸拦截错: %v", err)
	}
	_, _, _, sentAfter, _ := s.snapshot()
	if len(sentAfter) != len(sentBefore) {
		t.Errorf("被拦截的发送不该触达会话: %+v", sentAfter[len(sentBefore):])
	}

	// 放行后底层报错：AfterSendFailed 记账、AfterSend 不记。
	fg.mu.Lock()
	fg.block = false
	failsBefore, aftersBefore := fg.failed, len(fg.after)
	fg.mu.Unlock()
	s.mu.Lock()
	s.sendErr = errBoom
	s.mu.Unlock()
	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "boom"); err == nil {
		t.Fatal("底层报错该上抛")
	}
	fg.mu.Lock()
	if fg.failed != failsBefore+1 || len(fg.after) != aftersBefore {
		t.Fatalf("失败回写不符: failed=%d after=%d", fg.failed, len(fg.after))
	}
	fg.mu.Unlock()
}

// TestAntibanBatchSkipsOwnPacing 闸开启时批量不再叠加自己的条间睡眠：
// 节奏交给 BeforeSend 的延迟，批次只测接线不测等待。
func TestAntibanBatchSkipsOwnPacing(t *testing.T) {
	_, _, fg := waAcct(t)

	var mu sync.Mutex
	var sleeps []time.Duration
	oldSleep := waBatchSleep
	waBatchSleep = func(d time.Duration) { mu.Lock(); sleeps = append(sleeps, d); mu.Unlock() }
	t.Cleanup(func() { waBatchSleep = oldSleep })

	var results []*SendResult
	rid := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", rid) })
	if err := WhatsappSendBatchAsync(context.Background(), "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "a"},
		{To: "8613910000000", Body: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "两条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	})
	mu.Lock()
	n := len(sleeps)
	mu.Unlock()
	if n != 0 {
		t.Errorf("闸开启时批内不该再自己睡: sleeps=%d", n)
	}
	fg.mu.Lock()
	nBefore := len(fg.before)
	fg.mu.Unlock()
	if nBefore != 2 {
		t.Errorf("两条都该问过闸: %d", nBefore)
	}
}

// TestWaAntibanStatePath 状态文件名把 ":" "@" 全洗掉，带设备段的账号不撞主设备档。
func TestWaAntibanStatePath(t *testing.T) {
	a := waAntibanStatePath("st", "447378157347:1@s.whatsapp.net")
	b := waAntibanStatePath("st", "447378157347@s.whatsapp.net")
	if !strings.HasSuffix(a, "447378157347_1_s.whatsapp.net.antiban.json") {
		t.Errorf("洗名不符: %s", a)
	}
	if a == b {
		t.Error("同号码不同设备段该落不同档")
	}
}

// TestRealGateConstructionSmoke 真闸构造冒烟：NewGate 可用、新档首问不拦、
// 已知会话集从空开始。不联网、不发送；状态档落 TempDir，不污染仓库。
func TestRealGateConstructionSmoke(t *testing.T) {
	cfg := &config.WhatsappSessionsConfig{AntibanPreset: "moderate", AntibanStateDir: t.TempDir()}
	g := newWaAntibanGate(cfg, "8613800000000:1@s.whatsapp.net")
	if g == nil {
		t.Fatal("真闸构造为 nil")
	}
	target := types.NewJID("8613900000000", waServerUser).String()
	if g.IsKnownChat(target) {
		t.Error("新档不该有已知会话")
	}
	delay, err := g.BeforeSend(target, "hello", false, "", time.Now())
	if err != nil {
		t.Fatalf("moderate 档首条不该被拦: %v", err)
	}
	if delay < 0 {
		t.Errorf("延迟为负: %d", delay)
	}
	g.AfterSend(target, "hello", false, "", true, time.Now())
	if !g.IsKnownChat(target) {
		t.Error("AfterSend 后应进已知会话集")
	}
}
