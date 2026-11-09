package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
)

// TestStartNilCfgDoesNotPanic 没有任何 whatsapp 配置时启动必须安静通过：
// 消息侧不装配、门面就地拒绝；资金门面本就未接入，与启没启动无关。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	WhatsappStop(nil)
	WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	// 传 nil 回落到包级运行快照：上一条刚把「没有配置」写进快照，这里才测得准「未启动」。
	WhatsappStart(nil)
	if _, err := WhatsappQuery(nil, "O1"); !errors.Is(err, ErrPayoutNotWired) {
		t.Fatalf("未接入的查询应返回哨兵而不是别的错误: %v", err)
	}
	if _, err := WhatsappSendMessage(nil, "86138", "86139", "hi"); err == nil {
		t.Fatal("消息侧未启动时发送应当被拒")
	}
	WhatsappStop(nil)
	WhatsappStop(nil) // 幂等
}

// fakeDeviceContainer 是 store.DeviceContainer 的记账假件（两方法，逐字照库接口）。
type fakeDeviceContainer struct {
	puts    int
	deletes int
	putErr  error
}

func (f *fakeDeviceContainer) PutDevice(context.Context, *store.Device) error {
	f.puts++
	return f.putErr
}

func (f *fakeDeviceContainer) DeleteDevice(context.Context, *store.Device) error {
	f.deletes++
	return nil
}

// newPinnedTestDevice 造一个「库里查得到的已配对设备」并套闸门。
func newPinnedTestDevice() (*store.Device, *fakeDeviceContainer) {
	inner := &fakeDeviceContainer{}
	jid := types.NewJID("8613800000000", types.DefaultUserServer)
	dev := &store.Device{ID: &jid, Container: inner}
	return pinWADevice(dev), inner
}

// TestPinBlocksLibraryAutoDelete 库侧删行被拦：底层一次都没被调用，
// 但库自己的内存收尾照走（ID 置 nil、Deleted 置位），死会话不会再往库里写。
func TestPinBlocksLibraryAutoDelete(t *testing.T) {
	dev, inner := newPinnedTestDevice()

	if err := dev.Delete(context.Background()); err != nil {
		t.Fatalf("被拦下的删行应当返回 nil（让库走完内存收尾），实得 %v", err)
	}
	if inner.deletes != 0 {
		t.Fatalf("闸门漏放：底层 DeleteDevice 被调了 %d 次，期望 0", inner.deletes)
	}
	if dev.ID != nil {
		t.Fatalf("库侧的 ID 收尾没走到（期望 nil，实得 %s），设备子存储仍是可写态", dev.ID.String())
	}
	if !dev.Deleted {
		t.Fatal("dev.Deleted 未置位，库会继续用这套内存态写数据")
	}
}

// TestPinForwardsPutDevice 落库这条路必须原样通：配对成功靠 Save→PutDevice 写行，
// 拦了它等于扫码配对永远不生效。
func TestPinForwardsPutDevice(t *testing.T) {
	dev, inner := newPinnedTestDevice()

	if err := dev.Save(context.Background()); err != nil {
		t.Fatalf("Save 应当透传成功，实得 %v", err)
	}
	if inner.puts != 1 {
		t.Fatalf("PutDevice 调用数=%d，期望 1（闸门只拦删、不拦落库）", inner.puts)
	}

	inner.putErr = errors.New("boom")
	if err := dev.Save(context.Background()); err == nil {
		t.Fatal("底层 PutDevice 的错误必须原样上抛，不能被闸门吞掉")
	}
}

// TestPinIdempotentAndNilSafe 重复套闸门不叠加（叠加会让日志与内层指针绕圈），
// 且 nil 设备/nil Container 就地返回而不是 panic——GetDevice 的「查无」形态是 (nil, nil)。
func TestPinIdempotentAndNilSafe(t *testing.T) {
	dev, inner := newPinnedTestDevice()
	again := pinWADevice(dev)
	if again != dev {
		t.Fatal("pinWADevice 必须原地返回同一对象（管理器持有的是这个指针）")
	}
	if _, ok := dev.Container.(waPinnedContainer); !ok {
		t.Fatalf("Container 类型=%T，期望 waPinnedContainer", dev.Container)
	}
	if pinWADevice(nil) != nil {
		t.Fatal("nil 设备必须原样返回 nil")
	}
	bare := &store.Device{}
	if got := pinWADevice(bare); got != bare || got.Container != nil {
		t.Fatal("Container 为空的设备不该被硬套闸门")
	}

	// 内层指针必须还是那个真容器：显式吊销（WAStore.DeleteDevice）不经闸门，
	// 这里从包装里取回内层，确证被拦的不是唯一出口。
	wrapped, ok := dev.Container.(waPinnedContainer)
	if !ok || wrapped.inner != store.DeviceContainer(inner) {
		t.Fatal("闸门丢了内层容器引用，显式删行将无路可走")
	}
}

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
	// SendRate 放宽：缺省 1|1 会让「同批第 2 条」撞速率闸，这些用例要连发多条。
	m, _, get := startFakeWA(t, &config.WhatsappSessionsConfig{AntibanEnabled: waBool(true), SendRate: "1|1000"}, nil, dev)
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

	// 延迟真睡：闸给 250ms 时被抬到发送窗口下界（缺省 1 秒），整条不该瞬间返回。
	fg.mu.Lock()
	fg.delayMs = 250
	fg.mu.Unlock()
	start := time.Now()
	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "slow"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < time.Second {
		t.Errorf("250ms 应被抬到发送窗口下界 1s，实耗 %v", el)
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

// TestSendDelayClampedToConfigWindow 闸给出的超长延迟（拓扑档按分钟加、封禁恢复再拉长）
// 必须被压进配置窗口（缺省 5 秒），不能让单条发送睡满闸的原值。
func TestSendDelayClampedToConfigWindow(t *testing.T) {
	_, _, fg := waAcct(t)
	fg.mu.Lock()
	fg.delayMs = 20_000
	fg.mu.Unlock()
	start := time.Now()
	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "cap"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 7*time.Second {
		t.Fatalf("20s 闸延迟应被截到 5s 窗口内，实耗 %s", el)
	}
	if el := time.Since(start); el < time.Second {
		t.Fatalf("正延迟应至少睡到下界 1s，实耗 %s", el)
	}
}

func TestClampSendDelayMs(t *testing.T) {
	cfg := &config.WhatsappSessionsConfig{SendMinDelaySec: 1, SendMaxDelaySec: 5}
	if got := clampSendDelayMs(cfg, 0); got != 0 {
		t.Errorf("0 应立刻发，实得 %d", got)
	}
	if got := clampSendDelayMs(cfg, 250); got != 1000 {
		t.Errorf("低于下界应抬到 1s，实得 %d", got)
	}
	if got := clampSendDelayMs(cfg, 3000); got != 3000 {
		t.Errorf("窗口内应保留原值，实得 %d", got)
	}
	if got := clampSendDelayMs(cfg, 60_000); got != 5000 {
		t.Errorf("高于上界应截到 5s，实得 %d", got)
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

// assertNoDeleteOrArchive 断言一批 patch 里绝不出现 deleteChat 变异（退群根因）。
func assertNoDeleteOrArchive(t *testing.T, patches []appstate.PatchInfo) {
	t.Helper()
	for i, p := range patches {
		for j, m := range p.Mutations {
			if m.Index[0] == appstate.IndexDeleteChat {
				t.Fatalf("patch[%d].Mutations[%d] 出现 deleteChat（群聊会退群），Index=%v", i, j, m.Index)
			}
			if m.Index[0] == appstate.IndexArchive {
				t.Fatalf("patch[%d].Mutations[%d] 出现 archive（本批只清空、不归档），Index=%v", i, j, m.Index)
			}
		}
	}
}

// assertClearPatch 断言清空消息 patch：regular_high、Index 四段逐字、MessageRange 正确。
func assertClearPatch(t *testing.T, patch appstate.PatchInfo, wantTarget, wantMsgID string, wantTS time.Time, wantDeleteMedia string) {
	t.Helper()
	if patch.Type != appstate.WAPatchRegularHigh {
		t.Fatalf("清空 patch.Type=%v，期望 %v", patch.Type, appstate.WAPatchRegularHigh)
	}
	if len(patch.Mutations) != 1 {
		t.Fatalf("清空 patch 变异条数=%d，期望 1", len(patch.Mutations))
	}
	mut := patch.Mutations[0]
	if len(mut.Index) != 4 || mut.Index[0] != appstate.IndexClearChat ||
		mut.Index[1] != wantTarget || mut.Index[2] != "1" || mut.Index[3] != wantDeleteMedia {
		t.Fatalf("clearChat Index=%v，期望 [%s %s 1 %s]", mut.Index, appstate.IndexClearChat, wantTarget, wantDeleteMedia)
	}
	if mut.Version != 6 {
		t.Fatalf("clearChat Version=%d，期望 6", mut.Version)
	}
	mr := mut.Value.GetClearChatAction().GetMessageRange()
	if mr == nil {
		t.Fatal("清空 patch 缺 MessageRange")
	}
	if got := mr.GetLastMessageTimestamp(); got != wantTS.Unix() {
		t.Fatalf("清空时间戳=%d，期望 %d", got, wantTS.Unix())
	}
	msgs := mr.GetMessages()
	if wantMsgID == "" {
		if len(msgs) != 0 {
			t.Fatalf("无消息 id 时不应带消息键，实得 %v", msgs)
		}
		return
	}
	if len(msgs) != 1 {
		t.Fatalf("清空消息键条数=%d，期望 1", len(msgs))
	}
	key := msgs[0].GetKey()
	if key.GetRemoteJID() != wantTarget || key.GetID() != wantMsgID || !key.GetFromMe() {
		t.Fatalf("清空消息键不符: remote=%s id=%s fromMe=%v", key.GetRemoteJID(), key.GetID(), key.GetFromMe())
	}
}

// assertSingleClear 断言一次清理只产生 1 个 clearChat patch，且不含 deleteChat/archive。
func assertSingleClear(t *testing.T, patches []appstate.PatchInfo, wantTarget, wantMsgID string, wantTS time.Time) {
	t.Helper()
	if len(patches) != 1 {
		t.Fatalf("一次清理应只产生 1 个清空 patch，实得 %d", len(patches))
	}
	assertClearPatch(t, patches[0], wantTarget, wantMsgID, wantTS, "0")
	assertNoDeleteOrArchive(t, patches)
}

// TestSendMessageAutoClearsChat 单聊发信成功后自动清空消息：目标必须是发送时解析出的
// LID（与发送同款改写），消息键带刚发出的 id，清理不再二次查 LID。
func TestSendMessageAutoClearsChat(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	pn := jidOf(t, "447723171050@s.whatsapp.net")
	lid := jidOf(t, "54246208704608@lid")
	sendTS := time.Unix(1700000000, 0)
	s.mu.Lock()
	s.sendTS = sendTS
	s.lidMap = map[types.JID]types.JID{pn: lid}
	s.mu.Unlock()

	id, err := WhatsappSendMessage(nil, "8613800000000", pn.String(), "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("单聊发信: id=%q err=%v", id, err)
	}
	_, _, _, sent, patches := s.snapshot()
	if len(sent) != 1 {
		t.Fatalf("应只发一条，实得 %d", len(sent))
	}
	assertSingleClear(t, patches, lid.String(), "MSG-1", sendTS)

	s.mu.Lock()
	lidCalls := s.lidCalls
	s.mu.Unlock()
	if lidCalls != 1 {
		t.Fatalf("LID 解析应只发生 1 次（发送时），清理不得二次查询，实得 %d", lidCalls)
	}
}

// TestSendGroupMessageAutoClearsChat 群消息发信成功后自动清空消息：目标原样落群 JID，
// 绝不发 deleteChat（群里发 deleteChat 会被手机端按删群聊天执行而退群），也不归档、
// 不发 LID 查询。
func TestSendGroupMessageAutoClearsChat(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	const group = "1234567890@g.us"
	sendTS := time.Unix(1700000000, 0)
	s.mu.Lock()
	s.sendTS = sendTS
	s.mu.Unlock()

	id, err := WhatsappSendGroupMessage(nil, "8613800000000", group, "hi group")
	if err != nil || id != "MSG-1" {
		t.Fatalf("群发信: id=%q err=%v", id, err)
	}
	_, _, _, sent, patches := s.snapshot()
	if len(sent) != 1 || sent[0].to != group {
		t.Fatalf("群收件人不符: %+v", sent)
	}
	assertSingleClear(t, patches, group, "MSG-1", sendTS)

	s.mu.Lock()
	lidCalls := s.lidCalls
	s.mu.Unlock()
	if lidCalls != 0 {
		t.Fatalf("群组不应查 LID，实得 %d 次", lidCalls)
	}
}

// TestSendBatchAutoClearsPerSuccess 批内每条成功消息在广播 SendResult 前已清空消息
// （1 个 patch）；坏目标/空正文不产生 patch，整批不中止。
func TestSendBatchAutoClearsPerSuccess(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	cfg := &config.WhatsappSessionsConfig{AntibanEnabled: waBool(false), SendRate: "1|30"}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	pn := jidOf(t, "447723171050@s.whatsapp.net")
	lid := jidOf(t, "54246208704608@lid")
	s.mu.Lock()
	s.lidMap = map[types.JID]types.JID{pn: lid}
	s.mu.Unlock()

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(time.Duration) {}
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	var results []*SendResult
	var patchCounts []int
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		_, _, _, _, patches := s.snapshot()
		mu.Lock()
		results = append(results, r)
		patchCounts = append(patchCounts, len(patches))
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{
		{To: pn.String(), Body: "m1"},
		{To: "12345@g.us", Body: "m2"},
		{To: "abc", Body: "m3"},
		{To: "8613920000000", Body: ""},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "四条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 4
	})

	mu.Lock()
	gotResults := append([]*SendResult(nil), results...)
	gotCounts := append([]int(nil), patchCounts...)
	mu.Unlock()
	if len(gotResults) != 4 {
		t.Fatalf("SendResult 条数=%d", len(gotResults))
	}
	wantErr := []string{"", "", "没有任何数字", "消息内容为空"}
	for i, sub := range wantErr {
		if sub == "" && gotResults[i].Err != "" {
			t.Errorf("结果[%d].Err=%q，期望成功", i, gotResults[i].Err)
		}
		if sub != "" && gotResults[i].Err == "" {
			t.Errorf("结果[%d] 期望失败（含 %q），实得成功", i, sub)
		}
	}
	// 成功条在广播前已完成清空：前两条各 1 个 patch，后两条失败不产生 patch。
	wantCounts := []int{1, 2, 2, 2}
	for i, want := range wantCounts {
		if gotCounts[i] != want {
			t.Errorf("SendResult[%d] 广播时 patch 数=%d，期望 %d", i, gotCounts[i], want)
		}
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 2 {
		t.Fatalf("整批应有两个成功条目各产生 1 个清空 patch，实得 %d", len(patches))
	}
	assertNoDeleteOrArchive(t, patches)
}

// TestSendAutoCleanupFailureDoesNotFailSend 清理失败只记警告：消息已 ack 就仍返回 id+nil；
// 发送失败/空 id 时不得产生清理 patch。
func TestSendAutoCleanupFailureDoesNotFailSend(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	s.mu.Lock()
	s.stateErr = errors.New("no app state keys found, creating app state keys is not yet supported")
	s.mu.Unlock()

	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil || id != "MSG-1" {
		t.Fatalf("清理失败不得改变发送结果: id=%q err=%v", id, err)
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 1 {
		t.Fatalf("清空应尝试 1 次 SendAppState，实得 %d", len(patches))
	}
	assertNoDeleteOrArchive(t, patches)

	s.mu.Lock()
	s.stateErr = nil
	s.sendErr = errBoom
	s.mu.Unlock()
	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); id != "" || err == nil {
		t.Fatalf("发送失败应返回错误: id=%q err=%v", id, err)
	}
	if _, _, _, _, patches := s.snapshot(); len(patches) != 1 {
		t.Fatalf("发送失败不得产生清理 patch，实得 %d", len(patches))
	}

	s.mu.Lock()
	s.sendErr = nil
	s.sendID = ""
	s.mu.Unlock()
	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); id != "" || err == nil {
		t.Fatalf("空 id 应返回错误: id=%q err=%v", id, err)
	}
	if _, _, _, _, patches := s.snapshot(); len(patches) != 1 {
		t.Fatalf("空 id 不得产生清理 patch，实得 %d", len(patches))
	}
}

// TestSendAutoCleanupSkipsSelfAndNonChatTargets 发给自己（备忘录）、newsletter/broadcast
// 不属于「群/玩家会话」，发送成功也不清理。
func TestSendAutoCleanupSkipsSelfAndNonChatTargets(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	for _, to := range []string{"8613800000000", "123@newsletter", "status@broadcast"} {
		if id, err := WhatsappSendMessage(nil, "8613800000000", to, "hi"); err != nil || id != "MSG-1" {
			t.Fatalf("目标 %s 发送: id=%q err=%v", to, id, err)
		}
	}
	_, _, _, sent, patches := s.snapshot()
	if len(sent) != 3 {
		t.Fatalf("三条都应触达会话，实得 %d", len(sent))
	}
	if len(patches) != 0 {
		t.Fatalf("自聊/newsletter/broadcast 不应清理，实得 %d 个 patch", len(patches))
	}
}

// TestAutoCleanupRunsAfterGateUnlock 清空必须在防封闸的 gateMu 释放之后：
// 在 SendAppState 回调里 TryLock 成功才绿。把清理挪进 waSendContent 的持锁段会当场变红。
func TestAutoCleanupRunsAfterGateUnlock(t *testing.T) {
	installFakeGate(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, &config.WhatsappSessionsConfig{AntibanEnabled: waBool(true)}, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	v, ok := m.clients.Load(dev.ID.String())
	if !ok {
		t.Fatal("账号未在册")
	}
	w, ok := v.(*waClientWrapper)
	if !ok {
		t.Fatalf("会话类型=%T", v)
	}
	s.mu.Lock()
	s.onState = func() {
		if !w.gateMu.TryLock() {
			t.Error("SendAppState 期间 gateMu 仍被持有（清理不应在防封闸临界区内）")
			return
		}
		w.gateMu.Unlock()
	}
	s.mu.Unlock()

	if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err != nil {
		t.Fatalf("发信失败: %v", err)
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 1 {
		t.Fatalf("应清空 1 次，实得 %d", len(patches))
	}
}

// setContactSyncWindow 临时改「全量重拉的最小间隔」这个注入点，用完还原。
// 窗口的两边都要有活闸：压到 1 小时证明「窗口内绝不擦版本」，压到 0 证明
// 「窗口外确实会升级 full=true」（不是把 full 恒关掉蒙过去）。
func setContactSyncWindow(t *testing.T, d time.Duration) {
	t.Helper()
	old := waContactSyncFullMinInterval
	waContactSyncFullMinInterval = d
	t.Cleanup(func() { waContactSyncFullMinInterval = old })
}

// setAppStateKeyWait 注入「解码 app state 缺密钥时最多等多久」这个包内预算，用完还原。
func setAppStateKeyWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := waAppStateKeyWait
	waAppStateKeyWait = d
	t.Cleanup(func() { waAppStateKeyWait = old })
}

// alCountSyncs 读同步账并把 full 序列拼成可读文本（红信息要能直接看出序列不对）。
func alCountSyncs(t *testing.T, s *fakeSession) (calls, fullCalls int, seq string) {
	t.Helper()
	c, f, raw := s.countSyncs()
	parts := make([]string, 0, len(raw))
	for _, b := range raw {
		if b {
			parts = append(parts, "true")
		} else {
			parts = append(parts, "false")
		}
	}
	return c, f, strings.Join(parts, ",")
}

// alWantSeq 断言 full 调用序列逐字相符（顺序就是这条改动的语义本身）。
func alWantSeq(t *testing.T, s *fakeSession, want ...string) {
	t.Helper()
	_, _, got := alCountSyncs(t, s)
	wantStr := strings.Join(want, ",")
	if got != wantStr {
		t.Fatalf("SyncContactAppState 的 full 序列=[%s]，期望 [%s]", got, wantStr)
	}
}

// alContact 是「手机上的一个联系人」夹具条目。
func alContact(t *testing.T, jidStr, firstName string) (types.JID, types.ContactInfo) {
	t.Helper()
	return jidOf(t, jidStr), types.ContactInfo{FirstName: firstName}
}

// TestEnsureLoggedInStartsUnregisteredForList 设备行在库里、但从没人 StartClient
// （下游 `ensureClient` 空转的真实现场）：列好友必须自己把这个号拉起来、等到连上、
// 拉一次 app state，然后返回同步后的数据。
//
// 变异对照 MA：删掉 ensureLoggedIn 的 StartClient 分支 ⇒ liveWrapper 直接报
// 「没有在跑的会话」，本用例在 list 那一步即红。
func TestEnsureLoggedInStartsUnregisteredForList(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	// 配对码绑的号：在册之前一条联系人都没有（库里 contacts 是空的）。
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)

	// 一次都不显式启动：只调门面。
	list, err := WhatsappListContacts(nil, dev.ID.String())
	if err != nil {
		t.Fatalf("不在册的账号应被自动拉起后同步成功: %v", err)
	}
	if len(list) != 1 || list[0].JID != jid.String() || list[0].FirstName != "Alice" {
		t.Fatalf("同步后回读的好友列表不符: %+v", list)
	}
	if n := len(WhatsappListClients()); n != 1 {
		t.Fatalf("自动登录后在跑会话数=%d，期望 1（号确实被拉起来了）", n)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记（按需启动没走到 waSessionFactory）")
	}
	if calls, _, _ := alCountSyncs(t, s); calls != 1 {
		t.Fatalf("同步次数=%d，期望 1", calls)
	}
	if !s.LoggedIn() {
		t.Fatal("门面返回时账号仍未连上（ensureLoggedIn 没等到登录）")
	}
}

// TestEnsureLoggedInStartsUnregisteredForSend 同一条自愈对写门面生效：
// 「扫码的号能群发、配对码的号发不出去」的形态之一就是不显式启动直接发信。
//
// 变异对照 MA 的写侧对照：删掉 StartClient 分支 ⇒ 发送报「没有在跑」，本用例红。
func TestEnsureLoggedInStartsUnregisteredForSend(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, nil, dev)

	id, err := WhatsappSendMessage(nil, dev.ID.String(), "8613900000000", "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("不在册账号发信应自动登录后成功: id=%q err=%v", id, err)
	}
	if _, _, _, sent, _ := get(dev.ID.String()).snapshot(); len(sent) != 1 {
		t.Fatalf("会话上的发送记录=%+v，期望 1 条", sent)
	}
}

// TestEnsureLoggedInUnpairedKeepsNotRunningPhrase 库里连设备行都没有（从未配对）时，
// 自动启动失败的错误必须**同时**含「没有在跑」与「未配对」：前者是六条既有门面断言
// 钉着的稳定关键词，后者是唯一处置（去扫码）。
//
// 变异对照：把 ensureLoggedIn 的包装改成替换原文（丢掉「没有在跑」子串）⇒
// wa_client_test.go:147/375/730/780/861/943 六条同时红，本用例也红。
func TestEnsureLoggedInUnpairedKeepsNotRunningPhrase(t *testing.T) {
	startFakeWA(t, nil, nil) // 空设备库
	const acct = "8613711111111"

	if _, err := WhatsappSendMessage(nil, acct, "8613900000000", "hi"); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") || !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("未配对账号发信的错误串不符: %v", err)
	}
	if _, err := WhatsappListContacts(nil, acct); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") || !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("未配对账号列好友的错误串不符: %v", err)
	}
	if n, err := WhatsappSyncContacts(nil, acct, false); n != 0 || err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Fatalf("未配对账号同步联系人应被拒: n=%d err=%v", n, err)
	}
}

// TestRegisteredButDownWaitsAndSucceeds 在册但掉线（socket 掉了、库里还没派发事件）：
// 门面要催一轮重连并**等到连上**再取数，正治外部仓 ensureClient 的空转形态。
//
// 变异对照 MB：去掉 ensureLoggedIn 里的 reconnectAndAwait ⇒ 不催连、也不等，
// 门面在 loginCalls 仍是 1 时就越过自愈去取数，下面的 loginCalls 增量与耗时断言即红
// （另一侧的确定性对照见 TestRegisteredButDownWaitsBeforeSyncing）。
//
// 造「在册未连」的写法刻意避开 setDown 那一档竞态：常驻协程可能正卡在 waitLoggedIn 里
// （它只听 stop/downed/预算，**不听 kick**），只 setDown 时催连会白等满连接预算——
// wa_reconnect_test.go 钉着 setDown 形态的 TestSendKicksReconnectWhenSocketDown
// 就是本仓登记过的已知抖动。这里改成「首轮连接就失败」：协程必然停在 sleepBackoff，
// 而 sleepBackoff 是认 kick 令牌的，催连时机与状态因此确定。
func TestRegisteredButDownWaitsAndSucceeds(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	cfg := &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1, AntibanEnabled: waBool(false)}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) {
		s.loginErr = errBoom // 首轮连不上 ⇒ 协程停在退避睡眠里，等被 kick 催起
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首轮连接已尝试", func() bool { return s.countLogins() >= 1 })

	before := s.countLogins()
	s.mu.Lock()
	s.loginErr = nil // 下一轮 Login 会成功：此刻账号仍处在「在册未连」
	s.mu.Unlock()

	start := time.Now()
	list, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("在册未连的账号列好友应自愈成功: %v", err)
	}
	if len(list) != 1 || list[0].JID != jid.String() {
		t.Fatalf("取到的好友列表不符: %+v", list)
	}
	if got := s.countLogins(); got <= before {
		t.Fatalf("Login 调用数 %d → %d，门面没有催起重连（在册未连没被等连上）", before, got)
	}
	if !s.LoggedIn() {
		t.Fatal("返回时仍处在未登录态")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("自愈耗时 %s，催连没被退避睡眠接住", elapsed)
	}
}

// TestRegisteredButDownWaitsBeforeSyncing 同一档的反面：永远连不上时，
// 门面必须**先等连上、等不到就报错**，绝不带着一条没连上的 socket 去同步 app state。
//
// 这条是 MB 的确定性闸：把 reconnectAndAwait 摘掉 ⇒ 同步会在未登录态上照跑
// （syncCalls=1、错误也没有了），本用例两头断言同时红。
func TestRegisteredButDownWaitsBeforeSyncing(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	cfg := &config.WhatsappSessionsConfig{
		ConnectTimeoutSec: 1, ReconnectMinSec: 60, ReconnectMaxSec: 60, AntibanEnabled: waBool(false),
	}
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) {
		s.loginErr = errBoom
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首轮连接已尝试", func() bool { return s.countLogins() >= 1 })

	if list, err := WhatsappListContacts(nil, dev.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "未能连上") || len(list) != 0 {
		t.Fatalf("连不上时应报错且不交回数据: list=%+v err=%v", list, err)
	}
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 0 || fullCalls != 0 {
		t.Fatalf("未登录态上竟跑了同步（calls=%d full=%d），期望 0/0", calls, fullCalls)
	}
	if n, serr := WhatsappSyncContacts(nil, dev.ID.String(), false); n != 0 || serr == nil ||
		!strings.Contains(serr.Error(), "未能连上") {
		t.Fatalf("同步门面对未登录账号同样要报错: n=%d err=%v", n, serr)
	}
}

// TestListContactsSyncsWhenLocalEmpty 两段式的第二支：本地 0 条 ⇒ 走一次快路同步并返回
// 同步后的数据。这条是「通讯录=0 可发=0」的直接对照。
//
// 变异对照 MC 的反向：把两段式改成「无条件只读本地」⇒ 这里拿到空列表，红。
func TestListContactsSyncsWhenLocalEmpty(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	list, err := WhatsappListContacts(nil, dev.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].JID != jid.String() {
		t.Fatalf("本地空集时同步后的列表不符: %+v", list)
	}
	// 只走快路：full=false，绝不该擦版本。
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 1 || fullCalls != 0 {
		t.Fatalf("同步账 calls=%d full=%d，期望 1/0（本地空集只该走快路）", calls, fullCalls)
	}
}

// TestListContactsSkipsSyncWhenLocalHasData 两段式的第一支：本地已有联系人 ⇒ 直接返回，
// **一次同步都不做、不等登录**（保住 TestListContactsRevivesWithoutWaiting 的旧语义，
// 也让热路径每次群发不会都往服务器打一次快照）。
//
// 变异对照 MC：把两段式改成无条件同步 ⇒ syncCalls=1，本用例红。
func TestListContactsSkipsSyncWhenLocalHasData(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.contacts = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	start := time.Now()
	list, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].FirstName != "Bob" {
		t.Fatalf("本地命中却拿到: %+v", list)
	}
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 0 || fullCalls != 0 {
		t.Fatalf("本地已有联系人却同步了 %d 次（full=%d），期望 0/0", calls, fullCalls)
	}
	if elapsed >= time.Second {
		t.Fatalf("本地命中却耗时 %s（这条热路径不该有任何等待）", elapsed)
	}
}

// TestContactSyncSingleFlight 同账号 8 个并发请求只拉一次 app state：
// 拿锁后双检本地，后进者直接读本地结果。库侧 appStateSyncLock 只串行化、不去重，
// 不去重就会 8 次全量重拉互踩版本。
//
// 变异对照 MD：删掉 syncContactAppState 里的 s.mu.Lock/Unlock（单飞锁）⇒
// 多个 goroutine 同时看到本地 0 条，syncCalls>1，本用例红（-race 还会报 contacts 竞态）。
func TestContactSyncSingleFlight(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	// 先把号显式拉起来并等到连上：并发首调时多个 goroutine 会各自构造一份会话再
	// 丢掉输的那份（StartClient 既有形态），取件口此时可能指向被丢掉的那一条，
	// 会把「同步账」读成 0 次——那条路径由 TestEnsureLoggedInStartsUnregistered* 单独覆盖，
	// 这里要测的是同步单飞本身，必须只有一条会话可数。
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记")
	}
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	counts := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := WhatsappSyncContacts(context.Background(), dev.ID.String(), false)
			counts[i], errs[i] = n, err
		}(i)
	}
	wg.Wait()

	calls, fullCalls, seq := alCountSyncs(t, s)
	if calls != 1 {
		t.Fatalf("8 并发把同账号同步了 %d 次（full=%d, 序列=[%s]），期望单飞 1 次", calls, fullCalls, seq)
	}
	if fullCalls != 0 {
		t.Fatalf("快路却触发了全量重拉（full=%d）", fullCalls)
	}
	for i := range errs {
		if errs[i] != nil || counts[i] != 1 {
			t.Fatalf("并发结果[%d]不符: n=%d err=%v", i, counts[i], errs[i])
		}
	}
}

// TestContactFullSyncThrottledByWindow 「快路拉过但本地仍 0 条」才升级 full=true，
// 且升级受窗口约束：同一次调用里的序列必须是 [false, true]，第二次调用落在窗口内
// 就绝不再擦一次版本（full 次数不增）。
//
// 变异对照 ME：把 SyncContactAppState 的 full 实参恒传 true ⇒ 序列变成 [true, ...]，
// 第一条 alWantSeq 就红；把窗口判断删掉 ⇒ fullCalls 涨到 2，第二条断言红。
func TestContactFullSyncThrottledByWindow(t *testing.T) {
	setContactSyncWindow(t, time.Hour)
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	// 同步成功但补不进联系人：仿真「服务器快照本就为空」，逼出升级分支。
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	if _, err := WhatsappListContacts(nil, dev.ID.String()); err != nil {
		t.Fatalf("空快照不该报错: %v", err)
	}
	alWantSeq(t, s, "false", "true")
	calls, fullCalls, seq := alCountSyncs(t, s)
	if calls != 2 || fullCalls != 1 {
		t.Fatalf("首次列好友的同步账 calls=%d full=%d 序列=[%s]，期望 2/1/[false,true]", calls, fullCalls, seq)
	}

	// 第二次调用：快路照常可试，但升级落在窗口内 ⇒ 不再擦版本。
	if _, err := WhatsappListContacts(nil, dev.ID.String()); err != nil {
		t.Fatalf("第二次列好友不该报错: %v", err)
	}
	alWantSeq(t, s, "false", "true", "false")
	if calls, fullCalls, seq := alCountSyncs(t, s); fullCalls != 1 {
		t.Fatalf("窗口内又做了一次全量重拉：calls=%d full=%d 序列=[%s]，期望 full=1", calls, fullCalls, seq)
	}
}

// TestSyncContactsFacadeCountsAndThrottles 新门面本身：返回的是同步后回读的条数；
// 显式 full=true 首次立刻执行（擦版本重拉），第二次落在窗口内退化成快路。
//
// 变异对照：把 WhatsappSyncContacts 的返回值写成同步前的条数 ⇒ 第一条 n=0 红；
// 把窗口判断去掉 ⇒ 第二次仍 full=true，full 计数红。
func TestSyncContactsFacadeCountsAndThrottles(t *testing.T) {
	setContactSyncWindow(t, time.Hour)
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	a, ia := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	b, ib := alContact(t, "442071234568@s.whatsapp.net", "Beno")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{a: ia, b: ib}
	}, dev)

	n, err := WhatsappSyncContacts(nil, dev.ID.String(), true)
	if err != nil || n != 2 {
		t.Fatalf("显式全量同步: n=%d err=%v，期望 2 条", n, err)
	}
	s := get(dev.ID.String())
	alWantSeq(t, s, "true")

	// 显式 full=true 落在窗口内：不再擦版本，退化成快路（快路此时零网络）。
	n, err = WhatsappSyncContacts(nil, dev.ID.String(), true)
	if err != nil || n != 2 {
		t.Fatalf("窗口内的显式同步: n=%d err=%v", n, err)
	}
	alWantSeq(t, s, "true", "false")
	if _, fullCalls, _ := alCountSyncs(t, s); fullCalls != 1 {
		t.Fatalf("窗口内又擦了一次版本：full=%d，期望 1", fullCalls)
	}
}

// TestSyncContactsReportsKeyNotFound 密钥没共享时（配对码绑的号最常见的那一档）：
// 必须单独点名「密钥」，保留 %w 可判定，且**绝不返回「0 条 + nil」**。
//
// 变异对照 MF：把同步失败退化成「记日志后返回空集 + nil」⇒ 这里 err==nil 立刻红。
func TestSyncContactsReportsKeyNotFound(t *testing.T) {
	setAppStateKeyWait(t, 0)

	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	keyErr := fmt.Errorf("failed to decode app state %s patches: %w",
		appstate.WAPatchCriticalUnblockLow, appstate.ErrKeyNotFound)
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncErr = keyErr }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	n, err := WhatsappSyncContacts(nil, dev.ID.String(), false)
	if n != 0 || err == nil {
		t.Fatalf("密钥缺失必须报错且不给条数: n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("错误文案要点名密钥问题: %v", err)
	}
	if !errors.Is(err, appstate.ErrKeyNotFound) {
		t.Fatalf("必须保留 %%w 让下游可判定: %v", err)
	}
	if strings.Contains(err.Error(), "上下文已结束") {
		t.Fatalf("密钥缺失不许被折成上下文已结束: %v", err)
	}

	// 列好友同一条纪律：同步失败要报错，不能交回空列表冒充「没好友」。
	if list, lerr := WhatsappListContacts(nil, dev.ID.String()); lerr == nil || len(list) != 0 ||
		!strings.Contains(lerr.Error(), "密钥") {
		t.Fatalf("列好友遇密钥缺失时必须报错: list=%+v err=%v", list, lerr)
	}
}

// TestSyncContactsWaitsForAppStateKey 第一次解码缺密钥、手机随后把密钥补上时，
// 同步必须等到联系人落库再返回，而不是把第一次的 ErrKeyNotFound 直接交给群发。
func TestSyncContactsWaitsForAppStateKey(t *testing.T) {
	setAppStateKeyWait(t, 3*time.Second)

	dev := fakeDevice(t, "447378152319:2@s.whatsapp.net", "android", "P")
	friend := jidOf(t, "447700900001@s.whatsapp.net")
	keyErr := fmt.Errorf("failed to decode app state %s patches: %w",
		appstate.WAPatchCriticalUnblockLow, appstate.ErrKeyNotFound)
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncErr = keyErr
		s.syncAdds = map[types.JID]types.ContactInfo{
			friend: {FullName: "One Friend"},
		}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	go func() {
		time.Sleep(200 * time.Millisecond)
		s.setSync(nil, map[types.JID]types.ContactInfo{
			friend: {FullName: "One Friend"},
		})
	}()

	n, err := WhatsappSyncContacts(nil, dev.ID.String(), false)
	if err != nil || n != 1 {
		t.Fatalf("密钥补上后应同步到 1 个好友: n=%d err=%v", n, err)
	}
	list, lerr := WhatsappListContacts(nil, dev.ID.String())
	if lerr != nil || len(list) != 1 || list[0].JID != friend.String() {
		t.Fatalf("列好友应直接读到已落库的 1 条: %+v err=%v", list, lerr)
	}
}

// TestListContactsLoginTimeoutIsNamed 连不上时的文案是「未能连上」，且不含
// 「上下文已结束」——两条串互斥，值班才不会被引去查请求超时。
//
// 变异对照：把 ensureLoggedIn 的 reconnectAndAwait 换成「不等直接返回」⇒
// 这里拿不到「未能连上」（同步会在未登录态上跑），本用例红。
func TestListContactsLoginTimeoutIsNamed(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	cfg := &config.WhatsappSessionsConfig{ConnectTimeoutSec: 1, ReconnectMinSec: 60, ReconnectMaxSec: 60}
	// 压根不在册 + 永远连不上：走 ensureLoggedIn 的按需启动，再撞连接预算。
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) { s.loginErr = errBoom }, dev)

	start := time.Now()
	_, err := WhatsappListContacts(nil, dev.ID.String())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "未能连上") {
		t.Fatalf("连接预算到点应报「未能连上」: %v", err)
	}
	if strings.Contains(err.Error(), "上下文已结束") {
		t.Fatalf("两条串必须互斥，实得 %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("等待没夹在 1s 预算内：耗时 %s", elapsed)
	}
	if get(dev.ID.String()) == nil {
		t.Fatal("按需启动没建出会话（错误路径也不该留下半截状态则见上一条用例）")
	}
}

// TestListContactsCanceledIsNamed 调用方 ctx 已结束时报「上下文已结束（…本条未执行）」，
// 且不含「未能连上」；等待当场撒手，不烧完连接预算。
//
// 变异对照：把 waLoginErr 的 context.Canceled 折算删掉 ⇒ 第一条断言红
// （错误只剩库那串，值班会照字面去查号没注册）。
func TestListContactsCanceledIsNamed(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	cfg := &config.WhatsappSessionsConfig{ConnectTimeoutSec: 15, ReconnectMinSec: 60, ReconnectMaxSec: 60}
	_, _, get := startFakeWA(t, cfg, func(s *fakeSession) { s.loginErr = errBoom }, dev)

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := WhatsappListContacts(cctx, dev.ID.String())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "上下文已结束") ||
		!strings.Contains(err.Error(), "本条未执行") {
		t.Fatalf("取消类失败要点名「上下文已结束/本条未执行」: %v", err)
	}
	if strings.Contains(err.Error(), "未能连上") {
		t.Fatalf("两条串必须互斥，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "被取消") {
		t.Fatalf("原始等待错误要留在链上: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("ctx 已取消却仍等了 %s（该当场撒手）", elapsed)
	}
	_ = get(dev.ID.String())
}

// TestSyncContactsFailClosedWhenNotStarted 未启动时新门面就地拒绝、返回 0 条，
// 与 wa_client_test.go 的 TestFacadesFailClosedWhenNotStarted 同一条 fail-closed 口径
// （那条用例本身是禁改面，这里补上新增门面对照）。
//
// 变异对照：把 currentWA 的判空去掉 ⇒ nil 管理器解引用 panic，本用例直接崩。
func TestSyncContactsFailClosedWhenNotStarted(t *testing.T) {
	WhatsappStop(nil)
	n, err := WhatsappSyncContacts(nil, "8613800000000", true)
	if err == nil || n != 0 {
		t.Fatalf("未启动时同步联系人应当被拒: n=%d err=%v", n, err)
	}
	if got := WhatsappListClients(); got != nil {
		t.Fatalf("未启动时会话列表应为 nil，实得 %+v", got)
	}
}

// startFakeWA 把假件管理器装进门面快照（waMgr），返回管理器与假会话取件口。
// 用完自动摘除并停机，测试之间不串状态。
func startFakeWA(t *testing.T, cfg *config.WhatsappSessionsConfig, configure func(*fakeSession), devs ...*store.Device) (*waManager, *fakeStore, func(string) *fakeSession) {
	t.Helper()
	st := newFakeStore(devs...)
	get := installFakeSessionFactory(t, configure)
	m := newTestManager(t, st, cfg)
	waMgr.Store(m)
	t.Cleanup(func() { waMgr.Store(nil) })
	return m, st, get
}

// captureKinds 给四个消息侧 kind 挂上记录器，返回「收到过哪些 kind」的读取口。
// 测完自动注销：util.DefaultCallFunc 是全局表，漏注销会污染别的用例。
func captureKinds(t *testing.T, kinds ...string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	for _, kind := range kinds {
		k := kind
		id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+k, func(payload any) {
			mu.Lock()
			got = append(got, k)
			mu.Unlock()
		})
		t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+k, id) })
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestFacadesFailClosedWhenNotStarted 未启动时九条门面全部就地拒绝、列表为空，
// 且**不得广播任何回调**——「没启动却发了事件」意味着有代码绕过了快照判空。
func TestFacadesFailClosedWhenNotStarted(t *testing.T) {
	WhatsappStop(nil)
	seen := captureKinds(t, "Inbound", "Receipt", "Connection", "QR")

	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err == nil || id != "" {
		t.Errorf("未启动时发送应当被拒: id=%q err=%v", id, err)
	}
	if id, err := WhatsappSendGroupMessage(nil, "8613800000000", "12345@g.us", "hi"); err == nil || id != "" {
		t.Errorf("未启动时群发应当被拒: id=%q err=%v", id, err)
	}
	if got, err := WhatsappListContacts(nil, "8613800000000"); err == nil || got != nil {
		t.Errorf("未启动时查好友应当被拒: got=%+v err=%v", got, err)
	}
	if got, err := WhatsappListSpeakableGroups(nil, "8613800000000"); err == nil || got != nil {
		t.Errorf("未启动时查群组应当被拒: got=%+v err=%v", got, err)
	}
	if err := WhatsappDeleteChat(nil, "8613800000000", "8613900000000", "M1", time.Time{}, false); err == nil {
		t.Error("未启动时删会话应当被拒")
	}
	if err := WhatsappStartClient("8613800000000"); err == nil {
		t.Error("未启动时 StartClient 应当被拒")
	}
	if err := WhatsappStopClient("8613800000000"); err == nil {
		t.Error("未启动时 StopClient 应当被拒")
	}
	if err := WhatsappDeleteDevice(nil, "8613800000000"); err == nil {
		t.Error("未启动时删设备应当被拒")
	}
	if id, err := WhatsappStartPairing(nil); err == nil || id != "" {
		t.Errorf("未启动时 StartPairing 应当被拒: id=%q err=%v", id, err)
	}
	if got := WhatsappListClients(); got != nil {
		t.Errorf("未启动时列表应当为 nil，实得 %+v", got)
	}
	if kinds := seen(); len(kinds) != 0 {
		t.Errorf("未启动却广播了回调: %v", kinds)
	}
}

// TestSendMessageContract 发信契约三连：正常路径返回非空 id 且收件人被归一成会话 JID；
// 「服务端没给 id」必须是错误而不是空串（空 id ⇔ 非 nil error 是活闸，
// 拿空串当已发送会让流水对不上账）；底层报错原样上抛并补通道前缀。
func TestSendMessageContract(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	id, err := WhatsappSendMessage(nil, "8613800000000", "+86 139-0000-0000", "hi")
	if err != nil || id != "MSG-1" {
		t.Fatalf("正常路径: id=%q err=%v", id, err)
	}
	_, _, _, sent, _ := s.snapshot()
	if len(sent) != 1 || sent[0].to != "8613900000000@s.whatsapp.net" || sent[0].body != "hi" {
		t.Fatalf("收件人/正文不符: %+v", sent)
	}

	// 空 id ⇔ 非 nil error 的活闸：把假会话调成「成功但没 id」。
	s.mu.Lock()
	s.sendID = ""
	s.mu.Unlock()
	id, err = WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi")
	if id != "" || err == nil {
		t.Fatalf("空 id 必须伴随错误: id=%q err=%v", id, err)
	}
	var pe *pay.ProviderError
	if !asProviderError(err, &pe) || pe.Code != "no_message_id" {
		t.Fatalf("空 id 错误形态不符: %v", err)
	}

	s.mu.Lock()
	s.sendID = "MSG-2"
	s.sendErr = errBoom
	s.mu.Unlock()
	if id, err = WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); id != "" || err == nil ||
		!strings.Contains(err.Error(), "whatsapp 发送消息失败") {
		t.Fatalf("底层报错应当带通道前缀上抛: id=%q err=%v", id, err)
	}

	// 空正文与未知账号都必须在触达会话之前被拒。
	if _, err = WhatsappSendMessage(nil, "8613800000000", "8613900000000", ""); err == nil {
		t.Error("空正文应当被拒")
	}
	if _, err = WhatsappSendMessage(nil, "8613711111111", "8613900000000", "hi"); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Errorf("未启动账号应当被拒并说清处置: %v", err)
	}
}

// asProviderError 是 errors.As 的窄包装，避免每个用例都引 errors。
func asProviderError(err error, target **pay.ProviderError) bool {
	for err != nil {
		if pe, ok := err.(*pay.ProviderError); ok {
			*target = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var errBoom = &boomError{}

type boomError struct{}

func (e *boomError) Error() string { return "boom" }

// TestWaJIDNormalization JID 归一的四种形态：纯数字补 s.whatsapp.net、
// 带装饰字符先洗号、含 @ 原样解析、指向具体设备（Device>0）就地拒绝
// （库侧 send.go:200 也拒，提前拒是为了说清是哪一段写错了）。
func TestWaJIDNormalization(t *testing.T) {
	cases := []struct {
		raw    string
		want   string // 期望的 JID 串；空表示期望报错
		errSub string
	}{
		{"8613900000000", "8613900000000@s.whatsapp.net", ""},
		{"+86 139-0000-0000", "8613900000000@s.whatsapp.net", ""},
		{" 12345@g.us ", "12345@g.us", ""},
		{"8613900000000:3@s.whatsapp.net", "", "具体设备"},
		{"", "", "为空"},
		{"abc", "", "没有任何数字"},
	}
	for _, cs := range cases {
		jid, err := waJID(cs.raw)
		if cs.want == "" {
			if err == nil {
				t.Errorf("%q 应当被拒，实得 %s", cs.raw, jid.String())
			} else if cs.errSub != "" && !strings.Contains(err.Error(), cs.errSub) {
				t.Errorf("%q 错误文案应含 %q: %v", cs.raw, cs.errSub, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不应被拒: %v", cs.raw, err)
		} else if jid.String() != cs.want {
			t.Errorf("%q 归一成 %s，期望 %s", cs.raw, jid.String(), cs.want)
		}
	}
}

// TestWhatsappNormalizePhone 洗号只留数字。
func TestWhatsappNormalizePhone(t *testing.T) {
	if got := WhatsappNormalizePhone("+86 (139) 0000-0000"); got != "8613900000000" {
		t.Errorf("洗号=%s", got)
	}
	if got := WhatsappNormalizePhone("abc"); got != "" {
		t.Errorf("无数字应当洗成空串，实得 %q", got)
	}
}

// TestWaAccountJID 账号侧归一：配套设备形态的 :N 设备段必须放行且原样保留
// （会话库设备行主键逐字含设备段，抹掉即查不到行）；
// 群组域与设备号之外的形态仍拒。
func TestWaAccountJID(t *testing.T) {
	cases := []struct {
		raw    string
		want   string // 空表示期望报错
		errSub string
	}{
		{"447378157347:1@s.whatsapp.net", "447378157347:1@s.whatsapp.net", ""},
		{" 8613800000000@s.whatsapp.net ", "8613800000000@s.whatsapp.net", ""},
		{"8613800000000", "8613800000000@s.whatsapp.net", ""},
		{"12345@g.us", "", "域不是"},
		{"", "", "账号为空"},
		{"abc", "", "没有任何数字"},
	}
	for _, cs := range cases {
		jid, err := waAccountJID(cs.raw)
		if cs.want == "" {
			if err == nil {
				t.Errorf("%q 应当被拒，实得 %s", cs.raw, jid.String())
			} else if cs.errSub != "" && !strings.Contains(err.Error(), cs.errSub) {
				t.Errorf("%q 错误文案应含 %q: %v", cs.raw, cs.errSub, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不应被拒: %v", cs.raw, err)
		} else if jid.String() != cs.want {
			t.Errorf("%q 归一成 %s，期望 %s", cs.raw, jid.String(), cs.want)
		}
	}
}

// TestDeviceSuffixedAccountThroughFacades 真实现里绑定账号普遍是 "号码:1@s.whatsapp.net"
// （配套设备，wa_pairing 落库与业务侧 wa_jid 存的就是这个形态）：
// 启动、发信、列好友、列群组、停号整条链路必须以带设备段的账号 JID 走通，
// 收件人侧仍按会话 JID 口径（带设备号拒）。
func TestDeviceSuffixedAccountThroughFacades(t *testing.T) {
	const acct = "447378157347:1@s.whatsapp.net"
	dev := fakeDevice(t, acct, "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := WhatsappStartClient(acct); err != nil {
		t.Fatalf("带设备段的账号应当能启动: %v", err)
	}
	if err := WhatsappStartClient("447378157347"); err == nil {
		t.Error("纯号码补成主设备 JID，库里没有这一行，应当报未配对")
	}
	s := get(acct)

	if _, err := WhatsappSendMessage(nil, acct, "8613900000000", "hi"); err != nil {
		t.Fatalf("带设备段账号发信失败: %v", err)
	}
	if _, err := WhatsappSendMessage(nil, acct, "8613900000000:2@s.whatsapp.net", "hi"); err == nil ||
		!strings.Contains(err.Error(), "具体设备") {
		t.Errorf("带设备段的收件人应当被拒: %v", err)
	}
	s.mu.Lock()
	// FirstName 而非 PushName：列好友只认「存过通讯录」的行（waContactList 的好友闸）。
	s.contacts = map[types.JID]types.ContactInfo{jidOf(t, "8613900000000@s.whatsapp.net"): {FirstName: "F"}}
	s.groups = []*types.GroupInfo{{JID: jidOf(t, "100@g.us"),
		Participants:  []types.GroupParticipant{{JID: jidOf(t, acct), IsAdmin: true}},
		GroupAnnounce: types.GroupAnnounce{IsAnnounce: true}}}
	s.mu.Unlock()
	contacts, err := WhatsappListContacts(nil, acct)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("带设备段账号列好友: n=%d err=%v", len(contacts), err)
	}
	groups, err := WhatsappListSpeakableGroups(nil, acct)
	if err != nil || len(groups) != 1 || !groups[0].IsAdmin {
		t.Fatalf("仅管理员可发言群应按本人（带设备段账号）命中管理员: %+v err=%v", groups, err)
	}
	if _, err = WhatsappSendGroupMessage(nil, acct, "100@g.us", "hi"); err != nil {
		t.Fatalf("带设备段账号群发失败: %v", err)
	}
	if err := WhatsappStopClient(acct); err != nil {
		t.Fatalf("带设备段账号停号失败: %v", err)
	}
	_ = m
	_ = s
}

// TestClearChatPatchVerbatim 清空消息 patch 的逐字断言。
//
// 编码来源：Baileys Utils/chat-utils.ts 的 clear 分支与 WM/appstate 常量。
// clearChat 属于 regular_high（Version 6），Index 四段为
// [clearChat, 目标 JID, "1"（0=保留星标）, deleteMedia 0/1]。
// 同时钉死清理不包含 deleteChat（群聊天按 deleteChat 执行会退群），也不归档。
func TestClearChatPatchVerbatim(t *testing.T) {
	chat := jidOf(t, "8613900000000@s.whatsapp.net")
	ts := time.Unix(1700000000, 0)

	clear := waClearChatPatch(chat, " MSG-9 ", ts, true)
	assertClearPatch(t, clear, chat.String(), "MSG-9", ts, "1")

	// lastMsgID 为空 → 不构造消息键（清空范围只按时间戳划）；deleteMedia=false → Index[3]=="0"。
	clear2 := waClearChatPatch(chat, "  ", ts, false)
	assertClearPatch(t, clear2, chat.String(), "", ts, "0")

	assertNoDeleteOrArchive(t, []appstate.PatchInfo{clear, clear2})
}

// TestDeleteChatThroughFacade 门面路径：patch 原样送进会话的 SendAppState，
// 底层报错带通道前缀上抛；未启动账号在触达会话之前被拒。
func TestDeleteChatThroughFacade(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	ts := time.Unix(1700000000, 0)
	if err := WhatsappDeleteChat(nil, "8613800000000", "8613900000000", "MSG-9", ts, true); err != nil {
		t.Fatalf("清理会话失败: %v", err)
	}
	_, _, _, _, patches := s.snapshot()
	target := "8613900000000@s.whatsapp.net"
	if len(patches) != 1 {
		t.Fatalf("一次清理应只送 1 个清空 patch，实得 %d: %+v", len(patches), patches)
	}
	assertClearPatch(t, patches[0], target, "MSG-9", ts, "1")
	assertNoDeleteOrArchive(t, patches)

	s.mu.Lock()
	s.stateErr = errBoom
	s.mu.Unlock()
	err := WhatsappDeleteChat(nil, "8613800000000", "8613900000000", "", ts, false)
	if err == nil || !strings.Contains(err.Error(), "whatsapp 删除会话失败") {
		t.Fatalf("底层报错应当带通道前缀: %v", err)
	}
	if err := WhatsappDeleteChat(nil, "8613711111111", "8613900000000", "", ts, false); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Fatalf("未启动账号应当被拒: %v", err)
	}
}

// TestDeleteChatRewritesPNToLID 交接文档（2026-10-14）的活闸：会话清理必须走发送同款的
// PN→LID 改写——送进 SendAppState 的 clearChat/archive target 与 MessageKey.RemoteJID
// 都是 LID 串；退回「Index 仍是 PN」⇒ 红（patch 会落在一个并不存在的会话上，变异被对端
// 丢掉而 SendAppState 假绿）。查不到映射/查询失败退回 PN；群组 @g.us 不改写、连查询都不发起，
// 且任何清理都不包含 deleteChat（群聊会退群）。
func TestDeleteChatRewritesPNToLID(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	pn := jidOf(t, "447723171050@s.whatsapp.net")
	lid := jidOf(t, "54246208704608@lid")
	ts := time.Unix(1700000000, 0)

	s.mu.Lock()
	s.lidMap = map[types.JID]types.JID{pn: lid}
	s.mu.Unlock()

	if err := WhatsappDeleteChat(nil, "8613800000000", pn.String(), "3EB087FD71978BFA7E32D9", ts, false); err != nil {
		t.Fatalf("清理会话失败: %v", err)
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 1 {
		t.Fatalf("一次清理应只送 1 个清空 patch，实得 %d", len(patches))
	}
	assertClearPatch(t, patches[0], lid.String(), "3EB087FD71978BFA7E32D9", ts, "0")
	assertNoDeleteOrArchive(t, patches)

	// 查不到映射（老会话未迁 LID / NoopStore）退回 PN，清理照旧提交。
	s.mu.Lock()
	s.lidMap = nil
	s.mu.Unlock()
	if err := WhatsappDeleteChat(nil, "8613800000000", pn.String(), "M2", ts, false); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, patches = s.snapshot()
	if got := patches[len(patches)-1].Mutations[0].Index[1]; got != pn.String() {
		t.Fatalf("查不到映射应退回 PN，实得 %s", got)
	}

	// 查询报错同样退回 PN，而不是把清理拒掉。
	s.mu.Lock()
	s.lidErr = errBoom
	s.mu.Unlock()
	if err := WhatsappDeleteChat(nil, "8613800000000", pn.String(), "M3", ts, false); err != nil {
		t.Fatalf("LID 查询失败应退回 PN 而不是拒绝清理: %v", err)
	}
	_, _, _, _, patches = s.snapshot()
	if got := patches[len(patches)-1].Mutations[0].Index[1]; got != pn.String() {
		t.Fatalf("查询失败应退回 PN，实得 %s", got)
	}

	// 群组会话不改写，也不该为它发起 LID 查询；且绝不出现 deleteChat（会退群）或 archive。
	before := s.countLidLookups()
	if err := WhatsappDeleteChat(nil, "8613800000000", "12345@g.us", "M4", ts, false); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, patches = s.snapshot()
	assertClearPatch(t, patches[len(patches)-1], "12345@g.us", "M4", ts, "0")
	assertNoDeleteOrArchive(t, patches[len(patches)-1:])
	if n := s.countLidLookups() - before; n != 0 {
		t.Errorf("群组目标不该发起 LID 查询（多查 %d 次）", n)
	}
}

// TestDeleteChatBackfillsFromSendLedger 发信后立刻清空消息（运营刚需）：调用方留空
// lastMsgID 时回退本包发送账，消息键带最近一条实际发出的 id、挂在实际会话 JID（LID）上；
// 发送账没有记录时不拒、不带键，维持库侧退化语义。
func TestDeleteChatBackfillsFromSendLedger(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	pn := jidOf(t, "447723171050@s.whatsapp.net")
	lid := jidOf(t, "54246208704608@lid")
	s.mu.Lock()
	s.lidMap = map[types.JID]types.JID{pn: lid}
	s.sendID = "3EB0AA"
	s.mu.Unlock()

	if id, err := WhatsappSendMessage(nil, "8613800000000", pn.String(), "hi"); err != nil || id != "3EB0AA" {
		t.Fatalf("前置发信失败: id=%q err=%v", id, err)
	}

	if err := WhatsappDeleteChat(nil, "8613800000000", pn.String(), "", time.Time{}, false); err != nil {
		t.Fatalf("清理会话失败: %v", err)
	}
	_, _, _, _, patches := s.snapshot()
	// 发信成功已自动清空 1 次；下面这次是调用方手工再清空（同键重复提交）。
	if len(patches) != 2 {
		t.Fatalf("发信自动清空 + 手工清空应各 1 个 patch，实得 %d", len(patches))
	}
	autoMsgs := patches[0].Mutations[0].Value.GetClearChatAction().GetMessageRange().GetMessages()
	if len(autoMsgs) != 1 || autoMsgs[0].GetKey().GetID() != "3EB0AA" ||
		autoMsgs[0].GetKey().GetRemoteJID() != lid.String() {
		t.Fatalf("发信后的自动清空键不符: %v", autoMsgs)
	}
	mr := patches[len(patches)-1].Mutations[0].Value.GetClearChatAction().GetMessageRange()
	msgs := mr.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("发送账应回填出消息键，实得 %d 条", len(msgs))
	}
	key := msgs[0].GetKey()
	if key.GetID() != "3EB0AA" || key.GetRemoteJID() != lid.String() || !key.GetFromMe() {
		t.Fatalf("回填的消息键不符: id=%s remote=%s fromMe=%v", key.GetID(), key.GetRemoteJID(), key.GetFromMe())
	}
	if mr.GetLastMessageTimestamp() == 0 {
		t.Error("发送账的时间戳应一并回填")
	}

	// 没发过、调用方也没传 id：不拒绝，不带消息键（清空范围交由对端判定）。
	if err := WhatsappDeleteChat(nil, "8613800000000", "8613920000000", "", time.Time{}, false); err != nil {
		t.Fatalf("发送账无记录不该拒绝清理: %v", err)
	}
	_, _, _, _, patches = s.snapshot()
	if len(patches) != 3 {
		t.Fatalf("无记录清理也应送 1 个清空 patch，累计应为 3，实得 %d", len(patches))
	}
	if got := patches[len(patches)-1].Mutations[0].Value.GetClearChatAction().GetMessageRange().GetMessages(); len(got) != 0 {
		t.Errorf("发送账无记录时不该带消息键，实得 %v", got)
	}
}

// TestSendResultChatResolved 批量广播的 SendResult：To 保持调用方原文，Chat 是实际
// 落点的会话 JID——单聊按库同款改写落 LID，群组原样；这样后台群发后清理不必猜 LID。
func TestSendResultChatResolved(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	// SendRate 放宽：本用例要连发两条，缺省 1|1 会让第 2 条撞速率闸。
	cfg := &config.WhatsappSessionsConfig{AntibanEnabled: waBool(false), SendRate: "1|1000"}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	s.mu.Lock()
	s.lidMap = map[types.JID]types.JID{jidOf(t, "8613900000000@s.whatsapp.net"): jidOf(t, "54246208704608@lid")}
	s.mu.Unlock()

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(time.Duration) {}
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	var results []*SendResult
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "m1"},
		{To: "12345@g.us", Body: "m2"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "两条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if r := results[0]; r.To != "8613900000000" || r.Chat != "54246208704608@lid" || r.Err != "" {
		t.Errorf("单聊结果不符: %+v（期望 To 原文、Chat 落 LID）", r)
	}
	if r := results[1]; r.Chat != "12345@g.us" {
		t.Errorf("群组结果 Chat=%q，期望原样 12345@g.us", r.Chat)
	}
}

// collectEvent 是一次事件翻译断言收到的载荷。
type collectEvent struct {
	mu       *sync.Mutex
	inbound  []*InboundMessage
	receipts []*DeliveryReceipt
	conns    []*ConnEvent
}

// registerEventCollectors 给三类 kind 挂上强类型记录器（下游业务的回调就是这个签名形态）。
func registerEventCollectors(t *testing.T) *collectEvent {
	t.Helper()
	c := &collectEvent{}
	var mu sync.Mutex
	idIn := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"Inbound", func(m *InboundMessage) {
		mu.Lock()
		c.inbound = append(c.inbound, m)
		mu.Unlock()
	})
	idRc := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"Receipt", func(r *DeliveryReceipt) {
		mu.Lock()
		c.receipts = append(c.receipts, r)
		mu.Unlock()
	})
	idCn := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"Connection", func(e *ConnEvent) {
		mu.Lock()
		c.conns = append(c.conns, e)
		mu.Unlock()
	})
	t.Cleanup(func() {
		util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"Inbound", idIn)
		util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"Receipt", idRc)
		util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"Connection", idCn)
	})
	c.mu = &mu
	return c
}

func (c *collectEvent) snapshot() ([]*InboundMessage, []*DeliveryReceipt, []*ConnEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*InboundMessage(nil), c.inbound...),
		append([]*DeliveryReceipt(nil), c.receipts...),
		append([]*ConnEvent(nil), c.conns...)
}

// TestEventTranslationBothForms 事件翻译：库侧多以指针派发，值形态也必须接住
// （漏一类不会编译报错，只会静默丢事件）。两种文本形态（Conversation 与
// ExtendedTextMessage.Text）都要翻出正文。
func TestEventTranslationBothForms(t *testing.T) {
	col := registerEventCollectors(t)
	// collectEvent 的 mu 字段在 registerEventCollectors 里赋值，这里补类型。
	account := jidOf(t, "8613800000000@s.whatsapp.net")
	w := &waClientWrapper{
		jid:    account,
		sess:   &fakeSession{},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		kick:   make(chan struct{}, 1),
		downed: make(chan struct{}, 1),
	}
	chat := jidOf(t, "8613900000000@s.whatsapp.net")
	ts := time.Unix(1700000000, 0)
	msgInfo := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: chat},
		ID:            "M1",
		Timestamp:     ts,
	}

	// 指针形态 + Conversation 文本。
	w.dispatch(&events.Message{Info: msgInfo, Message: &waE2E.Message{Conversation: proto.String("hello")}})
	// 值形态 + ExtendedTextMessage 文本。
	w.dispatch(events.Message{Info: msgInfo, Message: &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("world")},
	}})
	// 回执：指针 + 值。
	w.dispatch(&events.Receipt{
		MessageSource: types.MessageSource{Chat: chat, Sender: chat},
		MessageIDs:    []types.MessageID{"M1", "M2"},
		Type:          types.ReceiptTypeRead,
		Timestamp:     ts,
	})
	w.dispatch(events.Receipt{
		MessageSource: types.MessageSource{Chat: chat, Sender: chat},
		MessageIDs:    []types.MessageID{"M3"},
		Type:          types.ReceiptTypeDelivered,
	})
	// 连接态：指针 + 值都覆盖，断线要递 downed 令牌，logged_out 要落 loggedOut 标记。
	w.dispatch(&events.Connected{})
	w.dispatch(events.Disconnected{})
	w.dispatch(events.StreamReplaced{})
	w.dispatch(&events.LoggedOut{})

	inbound, receipts, conns := col.snapshot()
	if len(inbound) != 2 {
		t.Fatalf("入站条数=%d，期望 2", len(inbound))
	}
	first := inbound[0]
	if first.Account != account.String() || first.Chat != chat.String() || first.MsgID != "M1" ||
		first.Type != "text" || first.Body != "hello" || !first.Timestamp.Equal(ts) {
		t.Fatalf("Conversation 形态翻译不符: %+v", first)
	}
	if second := inbound[1]; second.Type != "extended_text" || second.Body != "world" {
		t.Fatalf("ExtendedTextMessage 形态翻译不符: %+v", second)
	}
	if len(receipts) != 2 {
		t.Fatalf("回执条数=%d，期望 2", len(receipts))
	}
	if receipts[0].Status != string(types.ReceiptTypeRead) || len(receipts[0].MsgIDs) != 2 ||
		receipts[0].Account != account.String() {
		t.Fatalf("已读回执不符: %+v", receipts[0])
	}
	if receipts[1].Status != string(types.ReceiptTypeDelivered) {
		t.Fatalf("送达回执不符: %+v", receipts[1])
	}
	if len(conns) != 4 {
		t.Fatalf("连接事件条数=%d，期望 4", len(conns))
	}
	wantStates := []string{"connected", "disconnected", "stream_replaced", "logged_out"}
	for i, want := range wantStates {
		if conns[i].State != want || conns[i].Account != account.String() {
			t.Fatalf("连接事件[%d]=%+v，期望 state=%s", i, conns[i], want)
		}
	}
	if len(w.downed) != 1 {
		// downed 是容量 1 的令牌通道：disconnected 与 stream_replaced 各递一次，
		// 第二次因满被丢——一个令牌足够唤醒一次重连，这是设计而非丢事件。
		t.Fatalf("downed 令牌数=%d，期望 1（容量上限）", len(w.downed))
	}
	if !w.loggedOut.Load() {
		t.Fatal("logged_out 未落标记")
	}
}

// TestPublishWANoSubscriberIsNotFailure 无人订阅不算失败：不 panic、不影响门面结果。
// publishWA 的 Do 返回 false 只落一条 Debug 日志，这条口径靠「跑完不炸」钉住。
func TestPublishWANoSubscriberIsNotFailure(t *testing.T) {
	w := &waClientWrapper{
		jid:    jidOf(t, "8613800000000@s.whatsapp.net"),
		sess:   &fakeSession{},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		kick:   make(chan struct{}, 1),
		downed: make(chan struct{}, 1),
	}
	// 没有注册任何 Inbound 订阅者：dispatch 必须安静跑完。
	w.dispatch(&events.Message{Message: &waE2E.Message{Conversation: proto.String("nobody listens")}})
	publishWA("ZzNoSubscriberKind", &ConnEvent{State: "connected"})
}

// TestSendGroupMessage 群组发信门面：@g.us 目标走通并原样送达会话；
// 单聊号码与设备 JID 都在触达会话之前被拒（「不是群组 JID」/ waJID 归一错误）；
// 空正文、未启动账号、空 id 契约与 WhatsappSendMessage 同口径。
func TestSendGroupMessage(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	id, err := WhatsappSendGroupMessage(nil, "8613800000000", "1234567890@g.us", "hi group")
	if err != nil || id != "MSG-1" {
		t.Fatalf("群组发信正常路径: id=%q err=%v", id, err)
	}
	_, _, _, sent, _ := s.snapshot()
	if len(sent) != 1 || sent[0].to != "1234567890@g.us" || sent[0].body != "hi group" {
		t.Fatalf("群组收件人/正文不符: %+v", sent)
	}

	// 单聊写法必须在触达会话之前被拒：sent 不能多出一条。
	if _, err = WhatsappSendGroupMessage(nil, "8613800000000", "8613900000000", "not a group"); err == nil ||
		!strings.Contains(err.Error(), "不是群组 JID") {
		t.Errorf("非群组目标应当被拒: %v", err)
	}
	if _, _, _, sent, _ := s.snapshot(); len(sent) != 1 {
		t.Errorf("被拒的目标不应触达会话: %+v", sent)
	}
	if _, err = WhatsappSendGroupMessage(nil, "8613800000000", "1234567890@g.us", ""); err == nil ||
		!strings.Contains(err.Error(), "消息内容为空") {
		t.Errorf("空正文应当被拒: %v", err)
	}
	if _, err = WhatsappSendGroupMessage(nil, "8613711111111", "1234567890@g.us", "hi"); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Errorf("未启动账号应当被拒: %v", err)
	}

	s.mu.Lock()
	s.sendID = ""
	s.mu.Unlock()
	if id, err = WhatsappSendGroupMessage(nil, "8613800000000", "1234567890@g.us", "hi"); id != "" || err == nil {
		t.Fatalf("空 id 必须伴随错误: id=%q err=%v", id, err)
	}
}

// TestListContactsFacade 好友列表门面：只列「手机上存过通讯录」的好友、按 JID 排序、
// 字段逐一对应；本人（手机也会把自己存进通讯录）、仅聊过的 pushName 行、只有企业号
// 展示名的行一律剔除（真机实测扫码号的列表混着本人 PN 与本人 LID，2026-10-16 用户
// 裁决「只获取好友」）；底层报错带通道前缀上抛；未启动账号在触达会话之前被拒。
func TestListContactsFacade(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	self := jidOf(t, "8613800000000@s.whatsapp.net")
	friend := jidOf(t, "8613900000000@s.whatsapp.net")
	chatted := jidOf(t, "8613722222222@s.whatsapp.net")
	biz := jidOf(t, "8613733333333@s.whatsapp.net")
	s.mu.Lock()
	s.contacts = map[types.JID]types.ContactInfo{
		friend:  {FirstName: "Fern", FullName: "Fernando", PushName: "Fer", BusinessName: "FerShop"},
		self:    {FullName: "Myself", PushName: "Me"},
		chatted: {PushName: "Stranger"},
		biz:     {BusinessName: "ShopCo"},
	}
	s.mu.Unlock()

	got, err := WhatsappListContacts(nil, "8613800000000")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].JID != friend.String() {
		t.Fatalf("列表应当只剩存过通讯录的好友一条: %+v", got)
	}
	if got[0].FirstName != "Fern" || got[0].FullName != "Fernando" ||
		got[0].PushName != "Fer" || got[0].BusinessName != "FerShop" {
		t.Fatalf("联系人字段映射不符: %+v", got[0])
	}

	s.mu.Lock()
	s.contactsErr = errBoom
	s.mu.Unlock()
	if _, err = WhatsappListContacts(nil, "8613800000000"); err == nil ||
		!strings.Contains(err.Error(), "whatsapp 获取好友列表失败") {
		t.Fatalf("底层报错应当带通道前缀: %v", err)
	}
	if _, err = WhatsappListContacts(nil, "8613711111111"); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Fatalf("未启动账号应当被拒: %v", err)
	}
}

// TestListContactsNonFriendsOnlyFallsThroughToSync 两段式的「命中」按过滤后的好友数
// 判定：本地只剩本人与「仅聊过」的行时不算命中，必须继续走同步补齐，否则真好友
// 一条都没拉下来的账号会永远列空且不再尝试。
func TestListContactsNonFriendsOnlyFallsThroughToSync(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	self := jidOf(t, "8613800000000@s.whatsapp.net")
	chatted := jidOf(t, "8613722222222@s.whatsapp.net")
	s.mu.Lock()
	s.contacts = map[types.JID]types.ContactInfo{
		self:    {FullName: "Myself"},
		chatted: {PushName: "Stranger"},
	}
	s.mu.Unlock()
	list, err := WhatsappListContacts(nil, "8613800000000")
	if err != nil {
		t.Fatalf("非好友行被滤空不是错误: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("应当返回空好友列表: %+v", list)
	}
	if calls, _, _ := s.countSyncs(); calls == 0 {
		t.Error("本地只剩非好友行时应视为未命中，走同步补齐（一次都没拉）")
	}
}

// TestListSpeakableGroupsFilter 可发言群组闸的四种剔除形态与两种保留形态：
// 普通群保留；仅管理员可发言群里本人是管理员的保留（JID 直命中或经 PhoneNumber 命中）、
// 是普通成员的剔除、名单里找不到本人的剔除（fail-closed）；已暂停群剔除。
// IsLocked 只限改群设置，不影响入选。
func TestListSpeakableGroupsFilter(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	self := jidOf(t, "8613800000000@s.whatsapp.net")
	other := jidOf(t, "8613900000000@s.whatsapp.net")
	lid := jidOf(t, "123456789@lid")

	p := func(jid types.JID, admin, super bool) types.GroupParticipant {
		return types.GroupParticipant{JID: jid, PhoneNumber: jid, IsAdmin: admin, IsSuperAdmin: super}
	}
	lidMember := types.GroupParticipant{JID: lid, PhoneNumber: self, IsSuperAdmin: true}

	s.mu.Lock()
	s.groups = []*types.GroupInfo{
		{JID: jidOf(t, "600@g.us"), GroupName: types.GroupName{Name: "locked-normal"},
			GroupLocked:  types.GroupLocked{IsLocked: true},
			Participants: []types.GroupParticipant{p(self, false, false)}, ParticipantCount: 2},
		{JID: jidOf(t, "100@g.us"), GroupName: types.GroupName{Name: "plain"},
			Participants: []types.GroupParticipant{p(self, false, false), p(other, true, false)}, ParticipantCount: 2},
		{JID: jidOf(t, "200@g.us"), GroupName: types.GroupName{Name: "announce-admin"},
			GroupAnnounce: types.GroupAnnounce{IsAnnounce: true},
			Participants:  []types.GroupParticipant{p(self, true, false)}, ParticipantCount: 1},
		{JID: jidOf(t, "300@g.us"), GroupName: types.GroupName{Name: "announce-member"},
			GroupAnnounce: types.GroupAnnounce{IsAnnounce: true},
			Participants:  []types.GroupParticipant{p(self, false, false)}, ParticipantCount: 1},
		{JID: jidOf(t, "400@g.us"), GroupName: types.GroupName{Name: "announce-stranger"},
			GroupAnnounce: types.GroupAnnounce{IsAnnounce: true},
			Participants:  []types.GroupParticipant{p(other, true, false)}, ParticipantCount: 1},
		{JID: jidOf(t, "500@g.us"), GroupName: types.GroupName{Name: "suspended"}, Suspended: true,
			Participants: []types.GroupParticipant{p(self, false, false)}},
		{JID: jidOf(t, "700@g.us"), GroupName: types.GroupName{Name: "announce-lid-super"},
			GroupAnnounce: types.GroupAnnounce{IsAnnounce: true},
			Participants:  []types.GroupParticipant{lidMember}, ParticipantCount: 1},
	}
	s.mu.Unlock()

	got, err := WhatsappListSpeakableGroups(nil, "8613800000000")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"100@g.us", "200@g.us", "600@g.us", "700@g.us"}
	if len(got) != len(wantIDs) {
		t.Fatalf("入选群组=%+v，期望 id %v", got, wantIDs)
	}
	for i, want := range wantIDs {
		if got[i].JID != want {
			t.Fatalf("排序后 id[%d]=%s，期望 %s", i, got[i].JID, want)
		}
	}
	plain := got[0]
	if plain.Name != "plain" || plain.MemberCount != 2 || plain.IsAdmin || plain.IsAnnounce {
		t.Fatalf("普通群字段不符: %+v", plain)
	}
	if admin := got[1]; !admin.IsAnnounce || !admin.IsAdmin {
		t.Fatalf("仅管理员可发言群的本人管理员标记不符: %+v", admin)
	}
	if lidSuper := got[3]; !lidSuper.IsSuperAdmin {
		t.Fatalf("经 PhoneNumber 命中本人的超级管理员应保留: %+v", lidSuper)
	}

	s.mu.Lock()
	s.groupsErr = errBoom
	s.mu.Unlock()
	if _, err = WhatsappListSpeakableGroups(nil, "8613800000000"); err == nil ||
		!strings.Contains(err.Error(), "whatsapp 获取可发言群组列表失败") {
		t.Fatalf("底层报错应当带通道前缀: %v", err)
	}
	if _, err = WhatsappListSpeakableGroups(nil, "8613711111111"); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Fatalf("未启动账号应当被拒: %v", err)
	}
}

// TestSendBatchAsync 异步批量发信：门面当场返回，批内逐条广播 SendResult；
// 第一条不等待、其后每条前睡一次注入的间隔；空正文/坏目标逐条带错误广播。
//
// 速率闸的排队语义不在这里测（`send_rate: "1|1000"` 直接把闸让开）：
// 2026-10-30 起窗口满不再拒单而是等到下一个槽位，用真配置压额度就意味着本用例
// 要等满一分钟 —— 那种时长只能靠 TestWaitSendSlotQueuesInsteadOfRejecting 用
// 毫秒级窗口参数去钉。
func TestSendBatchAsync(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	cfg := &config.WhatsappSessionsConfig{SendRate: "1|1000", AntibanEnabled: waBool(false)}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	var mu sync.Mutex
	var sleeps []time.Duration
	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(d time.Duration) { mu.Lock(); sleeps = append(sleeps, d); mu.Unlock() }
	waBatchDelay = func(min, max time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var results []*SendResult
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "m1"},
		{To: "12345@g.us", Body: "m2"},
		{To: "8613910000000", Body: "m3"},
		{To: "8613920000000", Body: ""}, // 空正文：拒绝且不占闸额度
		{To: "abc", Body: "m5"},         // 拼不出号码
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "五条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 5
	})
	// 断言段必须整体出锁再往下走：waitUntil 的条件函数也要拿这把 mu，
	// 在这里 defer mu.Unlock() 挂到函数尾会让下一段 waitUntil 自死锁。
	mu.Lock()
	gotSleeps := len(sleeps)
	gotResults := append([]*SendResult(nil), results...)
	mu.Unlock()
	if gotSleeps != 4 {
		t.Errorf("5 条消息之间应睡 4 次，实得 %d", gotSleeps)
	}
	wantErr := []string{"", "", "", "消息内容为空", "没有任何数字"}
	for i, sub := range wantErr {
		r := gotResults[i]
		if sub == "" && (r.Err != "" || r.MsgID != "MSG-1") {
			t.Errorf("结果[%d]=%+v，期望成功带 id", i, r)
		}
		if sub != "" && !strings.Contains(r.Err, sub) {
			t.Errorf("结果[%d].Err=%q，期望含 %q", i, r.Err, sub)
		}
		if r.Account != "8613800000000@s.whatsapp.net" {
			t.Errorf("结果[%d].Account=%q", i, r.Account)
		}
	}
	_, _, _, sent, _ := s.snapshot()
	if len(sent) != 3 || sent[0].to != "8613900000000@s.whatsapp.net" ||
		sent[1].to != "12345@g.us" || sent[2].to != "8613910000000@s.whatsapp.net" {
		t.Errorf("三条合法目标都该触达会话: %+v", sent)
	}

	// 调用方 cancel / deadline 对批次的影响另立用例（TestSendBatchAsyncCtxContract）。

	// 空列表与未启动账号在接走之前就地拒绝（不产生广播）。
	if err := WhatsappSendBatchAsync(nil, "8613800000000", nil); err == nil ||
		!strings.Contains(err.Error(), "消息列表为空") {
		t.Errorf("空列表应当被拒: %v", err)
	}
	if err := WhatsappSendBatchAsync(nil, "8613711111111", []OutboundMessage{{To: "8613900000000", Body: "x"}}); err == nil ||
		!strings.Contains(err.Error(), "没有在跑") {
		t.Errorf("未启动账号应当被拒: %v", err)
	}
}

// TestSendBatchAsyncCtxContract 批次终止条件的两向闸：调用方 cancel 不传导（已受理的
// 消息不因客户端挂断而蒸发），调用方 deadline 传导（预算用尽逐条给落点）。
//
// 变异对照：把 waBatchCtx 退回「直接用调用方 ctx」⇒ 第一段红（cancel 掐死批次）；
// 退回「WithoutCancel 且不取 deadline」⇒ 第二段红（过期预算不再中止剩余条目）。
func TestSendBatchAsyncCtxContract(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	cfg := &config.WhatsappSessionsConfig{AntibanEnabled: waBool(false), SendRate: "1|30"}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	var mu sync.Mutex
	var results []*SendResult
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })
	one := func(ctx context.Context, body string) *SendResult {
		t.Helper()
		mu.Lock()
		results = nil
		mu.Unlock()
		if err := WhatsappSendBatchAsync(ctx, "8613800000000", []OutboundMessage{
			{To: "8613900000000", Body: body},
		}); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 3*time.Second, "一条 SendResult (body="+body+")", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(results) == 1
		})
		mu.Lock()
		defer mu.Unlock()
		return results[0]
	}

	t.Run("调用方已 cancel 仍照常发出", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		r := one(canceled, "x")
		if r.Err != "" || r.MsgID != "MSG-1" {
			t.Fatalf("cancel 传导进了批次（已受理的消息被掐死）: %+v", r)
		}
		_, _, _, sent, _ := s.snapshot()
		if len(sent) != 1 {
			t.Fatalf("应真发 1 条，实得 %d", len(sent))
		}
	})

	t.Run("调用方 deadline 过期则中止", func(t *testing.T) {
		expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		r := one(expired, "y")
		if !strings.Contains(r.Err, "发送已取消") {
			t.Fatalf("过期预算该逐条广播取消，实得 Err=%q", r.Err)
		}
		_, _, _, sent, _ := s.snapshot()
		if len(sent) != 1 {
			t.Fatalf("过期批次不该再触达会话，sent 总数=%d", len(sent))
		}
	})
}

// TestWaBatchDelayRange 随机间隔取值器：min==max 塌成定值，正常区间落在 [min,max]。
func TestWaBatchDelayRange(t *testing.T) {
	if got := waBatchDelay(2*time.Second, 2*time.Second); got != 2*time.Second {
		t.Errorf("塌成区间外: %v", got)
	}
	for i := 0; i < 200; i++ {
		if got := waBatchDelay(time.Second, 5*time.Second); got < time.Second || got > 5*time.Second {
			t.Fatalf("取值 %v 越界 [1s,5s]", got)
		}
	}
}

// chContactsErr 是注入给 fakeSession.contactsErr 的哨兵错。
// 用包级哨兵而不是每次现造：断言要能用 errors.Is 钉住门面的 %w 链没被折成 %v。
var chContactsErr = errors.New("假件：好友表读取炸了")

// 窗口注入用 wa_autologin_test.go 的 setContactSyncWindow（同包共享）：本文件的
// 用例要在**一条用例中途**翻窗口（0 → 生效），中途那次直接赋值、由它登记的
// cleanup 统一还原，避免嵌套 cleanup 把窗口留在 0。

// chStateSyncEntry 只读地取某账号在 m.stateSync 里的单飞条目，不在册返回 nil。
// 绝不能用 m.stateSyncLock(key)：那个函数会惰性建条目，用它当判据会把「没建」测成「建了」。
func chStateSyncEntry(m *waManager, key string) *waContactSync {
	v, ok := m.stateSync.Load(key)
	if !ok {
		return nil
	}
	return v.(*waContactSync)
}

// chWrapper 只读地取 clients 表里某个账号的 wrapper，不在册返回 nil。
func chWrapper(m *waManager, key string) *waClientWrapper {
	v, ok := m.clients.Load(key)
	if !ok {
		return nil
	}
	return v.(*waClientWrapper)
}

// chStateSyncCount 数管理器单飞账里的条目总数（不限量增长的判据）。
func chStateSyncCount(m *waManager) int {
	n := 0
	m.stateSync.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// allGoroutineStacks 抓全进程 goroutine 栈（缓冲自增长直到一次装得下）。
// 协程泄漏计数一律用 runtime.Stack，不引 goleak 之类要改 go.mod 的依赖。
func allGoroutineStacks(t *testing.T) string {
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

// goroutinesInFile 按「协程块」数出仍停在某个源文件里的协程条数
// （一条协程在同一文件可能有多个栈帧，故按块计数而不是按出现次数）。
func goroutinesInFile(t *testing.T, file string) int {
	t.Helper()
	n := 0
	for _, block := range strings.Split(allGoroutineStacks(t), "\n\ngoroutine ") {
		if strings.Contains(block, file) {
			n++
		}
	}
	return n
}

// chWantNoSyncCall 断言错误串里点名的是「取好友列表」这一步，
// 而不是被掩盖成登录/上下文问题——这几条串互斥，值班才分得开诊。
func chWantReadContactsError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("取好友列表失败必须报错，实得 nil（静默退化就是把失败伪装成「这账号没好友」）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "获取好友列表失败") {
		t.Fatalf("错误串要点名「获取好友列表失败」，实得 %q", msg)
	}
	if !errors.Is(err, chContactsErr) {
		t.Fatalf("必须保留 %%w 让下游可判定，实得 %q", msg)
	}
	for _, bad := range []string{"没有在跑", "未配对", "未能连上", "上下文已结束", "同步联系人"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("本地读取失败被掩盖成 %q 那一类问题了: %q", bad, msg)
		}
	}
}

// TestChExplicitFullInWindowStillSendsOnce 钉 wa_contacts.go:117-123 的两半：
//
//	① 窗口内的**显式** full=true（escalate=false）：把 full 降成 false，但**照常真发一次**
//	   （:122 只改档位不 return；:124 的单飞双检又明确只对 !wantFull && !escalate 生效）。
//	   这条与 T10 的 TestSyncContactsFacadeCountsAndThrottles 的区别在判据：那条只看
//	   full 计数没涨（「一次都不发」也能绿灯），这里钉的是 calls 必须逐次 +1 且末位为 false，
//	   正治「窗口内把显式请求整段跳过」这种看起来更省的做法。
//	② 窗口内的**升级**（escalate=true）：反过来**一次都不发**，直接回读本地
//	   （:118-121——同一次调用里刚试过快路，重试没有信息增量，更不该白擦版本）。
//
// 改坏哪行会红：
//   - 把 :122 的 `full = false` 换成 `return readWaContacts(ctx, w)`（窗口内跳过显式请求）⇒
//     子用例①的 calls 停在 1，红。
//   - 删掉 :118-120 的 escalate 早退分支 ⇒ 子用例②的第三次同步照样发出去，calls 涨到 3、
//     序列多出末位 false，红。
//   - 把 :124 的双检条件写成 `!escalate`（漏掉 !wantFull）⇒ 子用例①的末位真值断言红
//     （显式请求被本地缓存短路，一次都不发）。
func TestChExplicitFullInWindowStillSendsOnce(t *testing.T) {
	t.Run("窗口内显式full=true仍真发一次", func(t *testing.T) {
		setContactSyncWindow(t, time.Hour)
		dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
		a, ia := alContact(t, "442071234567@s.whatsapp.net", "Alice")
		b, ib := alContact(t, "442071234568@s.whatsapp.net", "Beno")
		_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
			s.syncAdds = map[types.JID]types.ContactInfo{a: ia, b: ib}
		}, dev)
		if err := WhatsappStartClient(dev.ID.String()); err != nil {
			t.Fatal(err)
		}
		s := get(dev.ID.String())
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		// 第一次：零值 lastFullAt 对窗口不设限 ⇒ 显式 full=true 真发，并把窗口账记上。
		if n, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil || n != 2 {
			t.Fatalf("首次显式全量同步: n=%d err=%v，期望 2 条", n, err)
		}
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != 1 || fullCalls != 1 {
			t.Fatalf("首次显式同步的账 calls=%d full=%d 序列=[%s]，期望 1/1", calls, fullCalls, seq)
		}

		// 第二、第三次：都落在窗口内 —— 档位该降成 false，但请求**一次都不能少发**。
		for _, want := range []int{2, 3} {
			if n, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil || n != 2 {
				t.Fatalf("窗口内第 %d 次显式同步: n=%d err=%v，期望仍返回 2 条", want, n, err)
			}
			calls, fullCalls, raw := s.countSyncs()
			if calls != want {
				t.Fatalf("窗口内第 %d 次显式同步没真发：calls=%d（序列=%v），期望 %d", want, calls, raw, want)
			}
			if fullCalls != 1 {
				t.Fatalf("窗口内又擦了一次版本：full=%d，期望恒为 1", fullCalls)
			}
			if raw[len(raw)-1] {
				t.Fatalf("窗口内的显式请求没降档成快路：末次 full=true（序列=%v）", raw)
			}
		}
		alWantSeq(t, s, "true", "false", "false")
	})

	t.Run("窗口内升级一次都不发", func(t *testing.T) {
		setContactSyncWindow(t, time.Hour)
		dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
		// 同步成功但一条联系人也补不进来：仿真服务器快照本就为空，逼出第二段的升级分支。
		_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
		if err := WhatsappStartClient(dev.ID.String()); err != nil {
			t.Fatal(err)
		}
		s := get(dev.ID.String())
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		// 先用显式全量把 lastFullAt 写进窗口（这次该真发）。
		if n, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil || n != 0 {
			t.Fatalf("空快照的显式同步不该报错: n=%d err=%v", n, err)
		}
		alWantSeq(t, s, "true")

		// 列好友：快路（false）照常真发一次，升级（true,escalate）落在窗口内 ⇒ 一次都不发。
		list, err := WhatsappListContacts(nil, dev.ID.String())
		if err != nil || len(list) != 0 {
			t.Fatalf("空快照应交回空列表且无错: list=%+v err=%v", list, err)
		}
		alWantSeq(t, s, "true", "false")
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != 2 || fullCalls != 1 {
			t.Fatalf("升级分支被窗口挡住时该一次都不发：calls=%d full=%d 序列=[%s]，期望 2/1", calls, fullCalls, seq)
		}
	})
}

// TestContactChConcurrentListPullsOnce 钉两段式门面这一侧的单飞短路（wa_contacts.go:124-131 +
// wa_client.go:712-738）：同账号并发 N 条列好友，只有第一个把快照拉下来；
// 后进者要么在①段就看见本地 >0（一次同步都不做），要么拿锁后双检命中本地 >0 而**不再产生第二次同步**。
//
// 与 T10 的 TestContactSyncSingleFlight 的差别：那条打的是 WhatsappSyncContacts（①段不存在），
// 这条打的是真实热路径 WhatsappListContacts——它同时经过两段，
// 只有这条能证明「群发前的每次列好友不会把同账号打爆」。
//
// 改坏哪行会红：
//   - 删掉 wa_contacts.go:124-131 的单飞双检 ⇒ 后进者各自再拉一次，calls 涨到 >1，红；
//   - 删掉 wa_contacts.go:113-114 的 s.mu.Lock（单飞锁本体）⇒ 同上，且 -race 还会报
//     fakeSession.contacts 的并发读写；
//   - 把 wa_client.go:718 的 `len(contacts) > 0` 判据去掉（①段永远放行到②段）⇒
//     这条的 calls 仍为 1（靠双检兜住），但 TestListContactsSkipsSyncWhenLocalHasData 会红，
//     所以本用例的红信息专门描述「同步次数」，两条合起来才把两段都钉死。
func TestContactChConcurrentListPullsOnce(t *testing.T) {
	const concurrency = 8
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		// 第一个调用拉完就塞入 1 条联系人：后进者的双检/①段本地读因此有真数据可命中。
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记（waSessionFactory 没被走到）")
	}
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	counts := make([]int, concurrency)
	lists := make([][]WaContact, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让 N 条同时挤在单飞锁上
			list, err := WhatsappListContacts(nil, dev.ID.String())
			lists[i], errs[i], counts[i] = list, err, len(list)
		}(i)
	}
	close(start)
	wg.Wait()

	calls, fullCalls, seq := alCountSyncs(t, s)
	if calls != 1 {
		t.Fatalf("%d 并发列好友把同账号同步了 %d 次（full=%d，序列=[%s]），期望单飞 1 次",
			concurrency, calls, fullCalls, seq)
	}
	if fullCalls != 0 {
		t.Fatalf("并发热路径里出现了全量重拉（full=%d），期望 0", fullCalls)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("并发结果[%d] 报错: %v", i, errs[i])
		}
		if counts[i] != 1 || len(lists[i]) != 1 || lists[i][0].JID != jid.String() {
			t.Fatalf("并发结果[%d] 不符: n=%d %+v", i, counts[i], lists[i])
		}
	}
}

// TestChEnsureLoggedInMaxConcurrentGate 自动登录撞上 MaxConcurrent 闸：
// 错误串必须**同时**含「没有在跑」（wa_manager.go:334 的前缀，六条既有门面断言钉着这个关键词）
// 与上限文案（:169 的「在跑会话数已达上限 …/先用 WhatsappStopClient 停一个」），
// 让值班知道「不是没配对、也不是网络问题，是额度满了、要自己腾一个」；
// 且失败路径**不得留下半个 clients 条目**——否则下一条请求会把这个半截对象当成在跑会话端出去。
//
// 改坏哪行会红：
//   - 删掉 wa_manager.go:334 的包装前缀（只把 StartClient 的错原样递出去）⇒
//     「没有在跑」关键词消失，本用例与 wa_client_test.go 那六条一起红；
//   - 把 :166-170 的上限判断挪到 LoadOrStore 之后（先建对象再拒）⇒
//     下面的 clients/list/session/单飞账四条断言全红；
//   - 把上限文案改软（去掉「先用 WhatsappStopClient 停一个」）⇒ 处置关键词红。
func TestChEnsureLoggedInMaxConcurrentGate(t *testing.T) {
	running := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "A")
	blocked := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "B")
	cfg := &config.WhatsappSessionsConfig{MaxConcurrent: 1, AntibanEnabled: waBool(false)}
	m, _, get := startFakeWA(t, cfg, nil, running, blocked)
	if err := WhatsappStartClient(running.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(running.ID.String())
	waitUntil(t, 2*time.Second, "在跑账号连上", s.LoggedIn)

	// 不在册的第二个账号：设备行真的在库里，所以失败原因只可能是额度闸。
	list, err := WhatsappListContacts(nil, blocked.ID.String())
	if err == nil {
		t.Fatalf("额度已满时列好友不该成功，实得 %+v", list)
	}
	if len(list) != 0 {
		t.Fatalf("被额度闸挡下却交回了 %d 条好友数据: %+v", len(list), list)
	}
	msg := err.Error()
	for _, want := range []string{"没有在跑", "自动启动失败", "在跑会话数已达上限 1", "max_concurrent", "先用 WhatsappStopClient 停一个"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误串缺少 %q，实得 %q", want, msg)
		}
	}
	// 额度闸与「从未配对」是两件事，文案不许混读（处置完全不同）。
	if strings.Contains(msg, "未配对") {
		t.Errorf("库里明明有设备行，不许报成未配对: %q", msg)
	}
	if strings.Contains(msg, "上下文已结束") {
		t.Errorf("额度不足不是调用方断连，不许折成上下文已结束: %q", msg)
	}

	// 失败路径不留半截状态：clients 表仍只有那一个在跑账号，被挡的账号压根没建会话。
	if n := m.countClients(); n != 1 {
		t.Fatalf("额度闸挡下后在跑会话数=%d，期望仍为 1（没留下半个条目）", n)
	}
	if _, ok := m.clients.Load(blocked.ID.String()); ok {
		t.Fatal("被额度闸拒绝的账号竟在册（半截 clients 条目）")
	}
	if got := get(blocked.ID.String()); got != nil {
		t.Fatal("被额度闸拒绝的账号竟建出了假会话（waSessionFactory 被白走了一次）")
	}
	infos := WhatsappListClients()
	if len(infos) != 1 || infos[0].JID != running.ID.String() {
		t.Fatalf("在跑会话列表不符: %+v", infos)
	}
	// 同步一次都没发生：单飞账里既没有被挡账号的条目，在跑账号也没被拉过快照。
	if chStateSyncEntry(m, blocked.ID.String()) != nil {
		t.Fatal("被挡账号竟建出了同步单飞条目（说明请求掉进了同步阶段）")
	}
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 0 || fullCalls != 0 || seq != "" {
		t.Fatalf("额度闸挡下后仍发生了同步：calls=%d full=%d 序列=[%s]，期望 0/0/[]", calls, fullCalls, seq)
	}
	if n := chStateSyncCount(m); n != 0 {
		t.Fatalf("单飞账条目数=%d，期望 0（一次同步都没试，不该建锁）", n)
	}

	// 同一个额度闸对显式同步门面同样成立，且串口径一致。
	if n, serr := WhatsappSyncContacts(nil, blocked.ID.String(), true); n != 0 || serr == nil ||
		!strings.Contains(serr.Error(), "没有在跑") || !strings.Contains(serr.Error(), "在跑会话数已达上限") {
		t.Fatalf("显式同步门面撞额度闸时的表现不符: n=%d err=%v", n, serr)
	}
	if n := m.countClients(); n != 1 {
		t.Fatalf("两次被挡后在跑会话数=%d，期望 1", n)
	}
}

// TestChZombieRevivesThenAwaitsLogin 僵尸（在册但常驻协程已收手，done 已关）叠加自动登录：
// 门面必须先把这具死对象摘掉重建，再在新会话上等到连上，最后才同步取数。
//
// 造僵尸走既有确定性形态（fire LoggedOut + waitWrapperExited），**不碰 setDown()**：
// 后者会让常驻协程停在听不见 kick 的 waitLoggedIn（wa_manager.go:540-560），
// 正是 Lead 登记的三条已知抖动闸的同形根因，本文件绝不去挤那条时序。
//
// 改坏哪行会红：
//   - 去掉 liveWrapper 的 `if !w.retired()`（:376-378，死对象原样端出）⇒
//     reconnectAndAwait 在 waitConnected 里撞 `<-w.done` 报「常驻协程已退出」，
//     本用例第一步 err 即红；
//   - 去掉 ensureLoggedIn 的 reconnectAndAwait（:341-343，复活完就直接取数）⇒
//     新会话的协程还没跑完 Login，同步账落在没连上的会话上，
//     fresh.LoggedIn()/old.syncCalls==0 与「取到 1 条」三条里至少一条红；
//   - 把 liveWrapper 的重建改成「摘掉但不重起」（去掉 :395 的 StartClient）⇒
//     liveWrapper 报「重登后会话不在册」，红。
func TestChZombieRevivesThenAwaitsLogin(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	key := dev.ID.String()
	m, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	old := get(key)
	waitUntil(t, 2*time.Second, "首次连上", old.LoggedIn)
	beforeLogins := old.countLogins()

	// 库派发吊销：dispatch 当场落 loggedOut 标记，常驻协程随即收手，但 key 仍在册。
	old.fire(events.LoggedOut{OnConnect: false})
	waitWrapperExited(t, m, key)
	if zombie := chWrapper(m, key); zombie == nil || !zombie.retired() {
		t.Fatal("夹具没能造出僵尸形态（在册、done 已关、retired() 为真）")
	}

	start := time.Now()
	list, err := WhatsappListContacts(nil, key)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("僵尸账号列好友应被复活后成功: %v", err)
	}
	if len(list) != 1 || list[0].JID != jid.String() {
		t.Fatalf("复活后取到的好友列表不符: %+v", list)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("复活+等连上耗时 %s（没被自愈接住，会烧满连接预算）", elapsed)
	}

	fresh := get(key)
	if fresh == nil || fresh == old {
		t.Fatal("死对象没被换成新会话（复活没发生）")
	}
	if got := fresh.countLogins(); got < 1 {
		t.Fatalf("复活出的新会话 Login 调用数=%d，期望 ≥1（重建后没重连）", got)
	}
	if !fresh.LoggedIn() {
		t.Fatal("门面返回时新会话仍未连上（没等到登录就取了数）")
	}
	// 死对象保持原样：既没被重连回去，也没被拿去同步。
	if got := old.countLogins(); got != beforeLogins {
		t.Fatalf("死会话的 Login 调用数 %d → %d，被复用的竟是死对象", beforeLogins, got)
	}
	if old.LoggedIn() {
		t.Fatal("已退出的旧会话竟又回到登录态")
	}
	if calls, fullCalls, seq := alCountSyncs(t, old); calls != 0 || fullCalls != 0 || seq != "" {
		t.Fatalf("同步跑在了死会话上（calls=%d full=%d 序列=[%s]），期望 0/0/[]", calls, fullCalls, seq)
	}
	if calls, fullCalls, seq := alCountSyncs(t, fresh); calls != 1 || fullCalls != 0 || seq != "false" {
		t.Fatalf("新会话的同步账 calls=%d full=%d 序列=[%s]，期望 1/0/[false]", calls, fullCalls, seq)
	}
	// clients 表仍只有该 key 的一条，且指向复活后的那份。
	if n := m.countClients(); n != 1 {
		t.Fatalf("复活后在跑会话数=%d，期望 1（死对象没摘干净或多起了一条）", n)
	}
	v, ok := m.clients.Load(key)
	if !ok || v.(*waClientWrapper).sess != fresh {
		t.Fatalf("clients 表里该 key 的不是复活后的会话（ok=%v）", ok)
	}
	if infos := WhatsappListClients(); len(infos) != 1 || infos[0].JID != key {
		t.Fatalf("在跑会话列表不符: %+v", infos)
	}
}

// TestChUnpairedIsNotSwallowed 库里没有设备行（fakeStore.GetDevice 查无返 nil,nil，
// 与 sqlstore 同口径）时，自动登录不许把 liveWrapper 的「未配对」吞成一句「没有在跑」：
// 两条串必须在同一条错误里——前者是唯一处置（去扫码/配对），后者是六条既有门面断言的关键词。
// 同时这条请求必须**一次同步都不发**：账号压根不存在，跑同步就是把失败伪装成「这账号没好友」。
//
// 与 T10 的 TestEnsureLoggedInUnpairedKeepsNotRunningPhrase 的差别：那条只钉文案，
// 这里补的是「不发生任何同步调用」这一侧（拿一个真的在跑的账号当对照物，
// 用它自己的 syncCalls==0 与单飞账条目数 0 正面证明）。
//
// 改坏哪行会红：
//   - wa_manager.go:333-335 改成「StartClient 失败就自己起一个空会话继续走」⇒
//     在跑账号的同步账变成 >0，红；
//   - wa_client.go:724-727 把 ensureLoggedIn 的错误吞掉（继续往下同步）⇒ 同上；
//   - 去掉 :334 前缀 ⇒ 「没有在跑」消失，红（与那六条一起）。
func TestChUnpairedIsNotSwallowed(t *testing.T) {
	running := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, running)
	if err := WhatsappStartClient(running.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(running.ID.String())
	waitUntil(t, 2*time.Second, "对照账号连上", s.LoggedIn)

	// 447378152319 就是下游现场那个「配对码绑的号」，这里库里连设备行都没有。
	const unpaired = "447378152319"
	list, err := WhatsappListContacts(nil, unpaired)
	if err == nil {
		t.Fatalf("未配对账号列好友不该成功，实得 %+v", list)
	}
	if len(list) != 0 {
		t.Fatalf("未配对账号却交回 %d 条好友: %+v", len(list), list)
	}
	msg := err.Error()
	if !strings.Contains(msg, "未配对") {
		t.Errorf("错误串必须点名「未配对」（唯一处置是去扫码）: %q", msg)
	}
	if !strings.Contains(msg, "没有在跑") {
		t.Errorf("错误串必须留住「没有在跑」子串: %q", msg)
	}
	if !strings.Contains(msg, "扫码") {
		t.Errorf("错误串要给出处置（扫码/配对），实得 %q", msg)
	}
	if strings.Contains(msg, "上下文已结束") || strings.Contains(msg, "密钥") {
		t.Errorf("从未配对不许被读成断连或密钥缺失: %q", msg)
	}

	// 一次同步都没发生：进程里唯一存在的会话没有被拉过快照，单飞账也没建条目。
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 0 || fullCalls != 0 || seq != "" {
		t.Fatalf("未配对请求触发了同步：calls=%d full=%d 序列=[%s]，期望 0/0/[]", calls, fullCalls, seq)
	}
	if n := chStateSyncCount(m); n != 0 {
		t.Fatalf("单飞账条目数=%d，期望 0（没试同步就不该建锁）", n)
	}
	if n := m.countClients(); n != 1 {
		t.Fatalf("在跑会话数=%d，期望 1（未配对请求不该动 clients 表）", n)
	}
	if infos := WhatsappListClients(); len(infos) != 1 || infos[0].JID != running.ID.String() {
		t.Fatalf("在跑会话列表被改动: %+v", infos)
	}

	// 显式同步门面同一口径。
	if n, serr := WhatsappSyncContacts(nil, unpaired, false); n != 0 || serr == nil ||
		!strings.Contains(serr.Error(), "未配对") || !strings.Contains(serr.Error(), "没有在跑") {
		t.Fatalf("显式同步门面对未配对账号的表现不符: n=%d err=%v", n, serr)
	}
	if calls, _, seq := alCountSyncs(t, s); calls != 0 || seq != "" {
		t.Fatalf("显式同步门面也未配对时跑了同步（calls=%d），期望 0", calls)
	}
}

// TestChEmptySnapshotIsNotFailure 「同步成功但服务器快照本就 0 条」是本包唯一合法的
// 「0 条且无错」（wa_contacts.go:148-152 只记日志、不改返回值）：
// 列好友的第二段会因此升级一次 full=true（wa_client.go:732-738），最终仍交回空列表 + nil error，
// 而不是把「没好友」冒充成失败，也不许把升级烧成无穷次。
//
// 本用例刻意**不动窗口注入点**（用生产缺省 10 分钟）：零值 lastFullAt 对窗口不设限，
// 所以首次升级本就该执行——这条同时钉住「首次同步不需要显式 full=true、但快路拉空后必须升级一次」。
//
// 改坏哪行会红：
//   - 把 wa_client.go:732 的 `len(contacts) == 0` 判据删掉（永不升级）⇒
//     序列只剩 [false]、fullCalls=0，alWantSeq 红；
//   - 把 wa_contacts.go:148-151 的空快照分支改成 return error（把合法的 0 当失败）⇒
//     第一条 err 断言红，下游又会回到「看起来成功的 0」的反面：一个都发不出去；
//   - 去掉 :141-143 的 `s.lastFullAt = time.Now()` 记账 ⇒ 本用例仍绿，但下一条
//     （TestContactChFullEscalationTwiceThenWindowBack）的窗口挡不住，full 涨到 3 而红。
func TestChEmptySnapshotIsNotFailure(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	key := dev.ID.String()
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev) // 同步成功但一条也补不进来
	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	s := get(key)
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	list, err := WhatsappListContacts(nil, key)
	if err != nil {
		t.Fatalf("快照本就为空不是失败，不该报错: %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("空快照应交回非 nil 的空列表，实得 %+v", list)
	}
	alWantSeq(t, s, "false", "true")
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 2 || fullCalls != 1 {
		t.Fatalf("空快照的同步账 calls=%d full=%d 序列=[%s]，期望 2/1（快路一次 + 升级一次）", calls, fullCalls, seq)
	}

	// 显式同步门面对同一形态也是「0 条 + nil」：下游要能把它和「同步失败」分开读。
	if n, serr := WhatsappSyncContacts(nil, key, false); n != 0 || serr != nil {
		t.Fatalf("空快照的显式同步应返回 0 条且无错: n=%d err=%v", n, serr)
	}
	alWantSeq(t, s, "false", "true", "false")
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 3 || fullCalls != 1 {
		t.Fatalf("第三次同步的账 calls=%d full=%d，期望 3/1（快路零网络，不该再擦版本）", calls, fullCalls)
	}
}

// TestChReadContactsFailureIsNotSilent 钉 wa_contacts.go:82-88 与 :144-147：
// 取好友列表（GetContacts）失败时必须**报错**，一条一条形态钉死，不许静默成「0 条 + nil」，
// 也不许被掩盖成登录/上下文问题（那会把值班引去查网络，而真因是本地好友表读不出）。
//
// 三段的返回逐字钉住：
//   - ①段（ensureAlive 命中本地读，wa_client.go:713-717）：直接「获取好友列表失败」并返回，
//     **不得掉进②段**——用 syncCalls==0 与「单飞账没建条目」正面证明它没往下走；
//   - ②段同步后的回读（wa_contacts.go:144-147）：同步已经成功，回读失败仍要报错，
//     错误串里不许出现「同步联系人」（同步没失败）；
//   - 显式同步门面：同一串，且条数为 0。
//
// 改坏哪行会红：
//   - 把 wa_client.go:715-717 的 `if err != nil { return }` 改成「忽略错误继续走②段」⇒
//     ①子用例的 syncCalls 变成 >1、错误串变成带「同步联系人」或登录问题，红；
//   - 把 wa_contacts.go:145-147 的回读失败改成 `return contacts, nil`（静默交空集）⇒
//     ②子用例拿到 nil error，红（正是本次要治的「看起来成功的 0」）；
//   - 把 :85 的 %w 换成 %v ⇒ errors.Is(err, chContactsErr) 断言红。
func TestChReadContactsFailureIsNotSilent(t *testing.T) {
	t.Run("第一段本地读失败即返回", func(t *testing.T) {
		dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
		key := dev.ID.String()
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.contactsErr = chContactsErr }, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		s := get(key)
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		list, err := WhatsappListContacts(nil, key)
		chWantReadContactsError(t, err)
		if len(list) != 0 {
			t.Fatalf("本地读失败却交回 %d 条: %+v", len(list), list)
		}
		// 没掉进第二段：一次同步都没发，单飞锁条目也没建。
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != 0 || fullCalls != 0 || seq != "" {
			t.Fatalf("第一段失败后竟掉进第二段同步了：calls=%d full=%d 序列=[%s]，期望 0/0/[]", calls, fullCalls, seq)
		}
		if chStateSyncEntry(m, key) != nil {
			t.Fatal("第一段失败不该建出同步单飞条目")
		}
	})

	t.Run("第二段同步后回读失败", func(t *testing.T) {
		dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
		key := dev.ID.String()
		// 压根不在册：①段 ensureAlive 直接失败，请求走进②段（自动登录 + 同步 + 回读）。
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.contactsErr = chContactsErr }, dev)

		list, err := WhatsappListContacts(nil, key)
		chWantReadContactsError(t, err)
		if strings.Contains(err.Error(), "同步联系人") {
			t.Fatalf("同步本身是成功的，不许把回读失败报成同步失败: %q", err.Error())
		}
		if len(list) != 0 {
			t.Fatalf("回读失败却交回 %d 条: %+v", len(list), list)
		}
		s := get(key)
		if s == nil {
			t.Fatal("②段的按需启动没建出会话")
		}
		alWantSeq(t, s, "false")
		if chStateSyncEntry(m, key) == nil {
			t.Fatal("②段没经过同步单飞锁")
		}
	})

	t.Run("显式同步门面同样报错不静默", func(t *testing.T) {
		dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
		key := dev.ID.String()
		_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.contactsErr = chContactsErr }, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		s := get(key)
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		n, err := WhatsappSyncContacts(nil, key, false)
		chWantReadContactsError(t, err)
		if n != 0 {
			t.Fatalf("回读失败时条数必须是 0，实得 %d", n)
		}
		alWantSeq(t, s, "false")
	})
}

// TestChNoStateOrGoroutineLeak 三态（成功 / 同步失败 / 未配对）各重复跑若干次之后：
//   - 同账号在 m.stateSync 里的条目数恒为 1，且始终是**同一把锁对象**（复用而非重建），
//     失败路径也不许攒条目；压根没同步过的账号（未配对）条目数为 0；
//   - 没有任何协程残留在 wa_contacts.go 里阻塞（runtime.Stack 按协程块计数自证，
//     不引 goleak 之类要改 go.mod 的依赖）。
//
// 改坏哪行会红：
//   - 把 wa_manager.go:72-79 的 stateSyncLock 改成「每次新建一把锁、LoadOrStore 换成 Store」⇒
//     条目数随调用次数增长（且单飞语义失效），成功/失败两档子用例红；
//   - 把 wa_contacts.go:113-114 的 s.mu.Lock 换成没有 Unlock 的早退（或 defer 丢掉）⇒
//     第二次调用永久阻塞在锁上，本用例的协程计数与超时同时红；
//   - 让同步路径在失败时不返回（协程挂在 sctx 上）⇒ 协程计数红。
func TestChNoStateOrGoroutineLeak(t *testing.T) {
	t.Run("成功态复用同一把锁", func(t *testing.T) {
		dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
		key := dev.ID.String()
		jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) {
			s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
		}, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		s := get(key)
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		// 条目是惰性建的（wa_manager.go:72-79），第一次同步之前不该存在；
		// 所以「复用同一把锁」的基准必须从第一次调用之后取。
		var first *waContactSync
		for i := 0; i < 5; i++ {
			if n, err := WhatsappSyncContacts(nil, key, false); err != nil || n != 1 {
				t.Fatalf("第 %d 次同步门面: n=%d err=%v", i+1, n, err)
			}
			if got := chStateSyncCount(m); got != 1 {
				t.Fatalf("第 %d 次调用后单飞账条目数=%d，期望恒为 1", i+1, got)
			}
			e := chStateSyncEntry(m, key)
			if e == nil {
				t.Fatalf("第 %d 次调用后没有单飞条目（同步没经过 stateSyncLock）", i+1)
			}
			if first == nil {
				first = e
			} else if e != first {
				t.Fatalf("第 %d 次调用换了一把锁（单飞账被重建过）", i+1)
			}
		}
		if calls, _, _ := alCountSyncs(t, s); calls != 1 {
			t.Fatalf("重复调用把同账号同步了 %d 次，期望 1（本地已有数据后双检命中）", calls)
		}
		if n := goroutinesInFile(t, "wa_contacts.go"); n != 0 {
			t.Fatalf("残留 %d 条协程停在 wa_contacts.go：\n%s", n, allGoroutineStacks(t))
		}
	})

	t.Run("同步失败态不攒条目", func(t *testing.T) {
		dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
		key := dev.ID.String()
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncErr = errBoom }, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 2*time.Second, "首次连上", get(key).LoggedIn)

		for i := 0; i < 4; i++ {
			if n, err := WhatsappSyncContacts(nil, key, false); n != 0 || err == nil {
				t.Fatalf("第 %d 次同步失败应报 0 条 + 错误: n=%d err=%v", i+1, n, err)
			}
			if _, err := WhatsappListContacts(nil, key); err == nil {
				t.Fatalf("第 %d 次列好友应把同步失败报出去", i+1)
			}
			if got := chStateSyncCount(m); got != 1 {
				t.Fatalf("第 %d 轮失败后单飞账条目数=%d，期望恒为 1", i+1, got)
			}
		}
		if n := goroutinesInFile(t, "wa_contacts.go"); n != 0 {
			t.Fatalf("失败路径残留 %d 条协程停在 wa_contacts.go：\n%s", n, allGoroutineStacks(t))
		}
	})

	t.Run("未配对态零条目零协程", func(t *testing.T) {
		m, _, _ := startFakeWA(t, nil, nil) // 空设备库
		const unpaired = "447378152319"
		for i := 0; i < 4; i++ {
			if _, err := WhatsappListContacts(nil, unpaired); err == nil ||
				!strings.Contains(err.Error(), "未配对") {
				t.Fatalf("第 %d 次未配对请求的错误不符: %v", i+1, err)
			}
		}
		if got := chStateSyncCount(m); got != 0 {
			t.Fatalf("未配对请求建出了 %d 条单飞账条目，期望 0", got)
		}
		if n := m.countClients(); n != 0 {
			t.Fatalf("未配对请求建出了 %d 条会话，期望 0", n)
		}
		if n := goroutinesInFile(t, "wa_contacts.go"); n != 0 {
			t.Fatalf("未配对路径残留 %d 条协程停在 wa_contacts.go：\n%s", n, allGoroutineStacks(t))
		}
	})
}

// TestContactChFullEscalationTwiceThenWindowBack 钉「full 升级每轮只发生一次、且第二次落在
// 窗口内就不再发生」：窗口放开时连跑两次列好友 ⇒ fullCalls 恰为 2、序列 [false,true,false,true]；
// 把窗口调回生效 ⇒ 第三次只有快路那一次真发，升级被挡住后**不再补发**（序列末位多一个 false、
// full 仍为 2）。与 T10 保留的那条窗口用例（wa_autologin_test.go 的
// TestContactFullSyncThrottledByWindow）的分工：那条钉「第一次会升级 / 第二次被挡」，
// 这条钉「连续两轮都各升级一次」+「中途把窗口调回生效立刻开始挡」这个翻转。
// T10 原来还有一条「窗口放开后升级确实执行」（两轮 [false,true]→[false,true,false,true]、
// full 依次 1/2），判据与本用例前两轮逐字相同、夹具同字面量，已并入这里（删它不丢任何闸）。
//
// 改坏哪行会红：
//   - wa_contacts.go:141-143 不记 lastFullAt ⇒ 窗口永远挡不住，第三轮的 full 涨到 3，红；
//   - wa_contacts.go:117 的窗口判据写成 `<=`/取反 ⇒ 第二轮就被挡住（full=1），红；
//   - wa_client.go:735 的升级实参从 true 改成 false ⇒ 序列里出现不了 true，红；
//   - wa_contacts.go:118-121 的 escalate 早退被删 ⇒ 第三轮多真发一次（calls 涨到 6），红。
func TestContactChFullEscalationTwiceThenWindowBack(t *testing.T) {
	setContactSyncWindow(t, 0) // 窗口放开：升级一律允许，先证明「每轮各一次」不是假绿
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	key := dev.ID.String()
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	s := get(key)
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	for round, want := range []struct {
		seq   []string
		calls int
	}{
		{[]string{"false", "true"}, 2},
		{[]string{"false", "true", "false", "true"}, 4},
	} {
		if list, err := WhatsappListContacts(nil, key); err != nil || len(list) != 0 {
			t.Fatalf("第 %d 轮列好友: list=%+v err=%v", round+1, list, err)
		}
		alWantSeq(t, s, want.seq...)
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != want.calls || fullCalls != round+1 {
			t.Fatalf("第 %d 轮的同步账 calls=%d full=%d 序列=[%s]，期望 %d/%d",
				round+1, calls, fullCalls, seq, want.calls, round+1)
		}
	}

	// 窗口调回生效（t.Cleanup 登记的还原仍会把生产缺省值放回去）。
	waContactSyncFullMinInterval = time.Hour
	if list, err := WhatsappListContacts(nil, key); err != nil || len(list) != 0 {
		t.Fatalf("窗口生效后列好友: list=%+v err=%v", list, err)
	}
	alWantSeq(t, s, "false", "true", "false", "true", "false")
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 5 || fullCalls != 2 {
		t.Fatalf("窗口生效后升级该被挡住：calls=%d full=%d 序列=[%s]，期望 5/2", calls, fullCalls, seq)
	}
	if n := goroutinesInFile(t, "wa_contacts.go"); n != 0 {
		t.Fatalf("三轮之后残留 %d 条协程停在 wa_contacts.go：\n%s", n, allGoroutineStacks(t))
	}
}

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

// TestWaBatchLinkContent 带按钮的条目走 SendInteractive，不落进纯文本账。
func TestWaBatchLinkContent(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(_ time.Duration) {}
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	results := 0
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(*SendResult) {
		mu.Lock()
		results++
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{{
		To: "8613900000000", Body: "说明文字",
		LinkText: "立即参与", LinkURL: "https://landing.example.com/act/11",
	}}); err != nil {
		t.Fatalf("批量受理失败: %v", err)
	}
	waitUntil(t, 3*time.Second, "一条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return results == 1
	})

	s.mu.Lock()
	links := append([]fakeInteractive(nil), s.sentLinks...)
	texts := append([]fakeText(nil), s.sent...)
	s.mu.Unlock()
	if len(texts) != 0 {
		t.Fatalf("按钮消息不该落进纯文本账: %+v", texts)
	}
	if len(links) != 1 {
		t.Fatalf("应当恰好发出一条按钮消息，实得 %d", len(links))
	}
	got := links[0]
	if got.to != "8613900000000@s.whatsapp.net" || got.body != "说明文字" ||
		got.buttonText != "立即参与" || got.buttonURL != "https://landing.example.com/act/11" {
		t.Fatalf("按钮消息落点不符: %+v", got)
	}
}

// TestWaLinkFallbackWhenInteractiveRejected 交互消息被拒 ⇒ 摊成文本重发，且正文含链接。
func TestWaLinkFallbackWhenInteractiveRejected(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	s.mu.Lock()
	s.linkErr = errors.New("interactive message not supported")
	s.mu.Unlock()

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(_ time.Duration) {}
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	var results []*SendResult
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{{
		To: "8613900000000", Body: "说明文字",
		LinkText: "立即参与", LinkURL: "https://landing.example.com/act/11",
	}}); err != nil {
		t.Fatalf("批量受理失败: %v", err)
	}
	waitUntil(t, 3*time.Second, "一条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 1
	})

	s.mu.Lock()
	texts := append([]fakeText(nil), s.sent...)
	s.mu.Unlock()
	if len(texts) != 1 {
		t.Fatalf("降级后应发出一条文本，实得 %d", len(texts))
	}
	want := "说明文字\n\n立即参与\nhttps://landing.example.com/act/11"
	if texts[0].body != want {
		t.Fatalf("降级正文=%q，期望 %q", texts[0].body, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("降级成功后结果应为成功: %+v", results)
	}
}

// TestWaLinkFallbackBody 纯函数：三种组合的拼接形态。
func TestWaLinkFallbackBody(t *testing.T) {
	cases := []struct{ body, text, url, want string }{
		{"正文", "按钮", "https://a.example.com/x", "正文\n\n按钮\nhttps://a.example.com/x"},
		{"正文", "", "https://a.example.com/x", "正文\nhttps://a.example.com/x"},
		{"", "按钮", "https://a.example.com/x", "按钮\nhttps://a.example.com/x"},
	}
	for _, c := range cases {
		if got := waLinkFallbackBody(c.body, c.text, c.url); got != c.want {
			t.Fatalf("waLinkFallbackBody(%q,%q,%q)=%q，期望 %q", c.body, c.text, c.url, got, c.want)
		}
	}
	if got := waInteractiveBizNodes(); len(got) != 1 || got[0].Tag != "biz" {
		t.Fatalf("按钮消息必须带 biz 节点: %+v", got)
	}
	if !strings.Contains(waCtaButtonName, "cta_url") {
		t.Fatalf("按钮 name 应为 cta_url，实得 %q", waCtaButtonName)
	}
}

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
// （复活重建会换对象，那种形态归 wa_reconnect_test.go）。
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

	// 运行中切换代理的调用账（wa_proxy_test.go）：proxyAddrs 按调用顺序记地址，
	// proxyErr 非 nil 时让 SetProxy 失败（钉「失败就保留旧连接、不disconnect」）。
	proxyAddrs []string
	proxyErr   error

	// logoutErr：服务端 unlink 谈不成时的兜底路径注入点（照 loginErr 那一族写法）。
	logoutErr error
	// deleteOnLogout：仿真库侧 Logout 成功时顺带走的 Device.Delete——它把 dev.ID 置 nil
	//（WM/store/store.go 的 Delete），用来钉住「绝不能把 ID 已空的设备再递给 DeleteDevice」。
	deleteOnLogout bool
	// fireDisconnectedOnLogout：仿真「unlink 没谈成、socket 已被远端拆掉」——库在这种
	// 情形下确实派发 events.Disconnected（手动断开才不派，WM/client.go 的 onDisconnect），
	// 常驻协程收到它就会从 waitUntilDown 醒来重连，正是 latch 必须在 Logout 之前的理由。
	fireDisconnectedOnLogout bool

	sendID     string // SendText 返回的消息 id；空串模拟「服务端没给 id」
	sendErr    error
	sendTS     time.Time // 非零时作为 SendText 返回的服务器时间戳（删框时间戳断言用确定值）
	stateErr   error
	sent       []fakeText
	sentImages []fakeImage
	// sentLinks 记录 SendInteractive 的落点（CTA 按钮消息用例断言用）；
	// linkErr 非 nil 时 SendInteractive 直接失败，用来钉住「按钮被拒 ⇒ 降级」这一支。
	sentLinks []fakeInteractive
	linkErr   error
	patches   []appstate.PatchInfo
	handler   func(any)
	// onState：非 nil 时在 SendAppState 的锁外回调（删框用例钉「闸已释放」等时序）。
	onState func()
	// downAfterSends>0 时，发满该条数就把登录态按回 false（批量发信逐条自愈的用例用）。
	downAfterSends int

	// 好友列表与群组查询的预置返回值（查好友 / 查可发言群 / 发群消息用例用）。
	contacts    map[types.JID]types.ContactInfo
	contactsErr error
	groups      []*types.GroupInfo
	groupsErr   error

	// LID 映射的预置返回值（删框 PN→LID 改写 / SendResult.Chat 用例用）：
	// lidMap 没有该 PN 时返回空 JID，与真库「查不到映射」的形态一致。
	lidMap   map[types.JID]types.JID
	lidErr   error
	lidCalls int

	// 联系人 app state 同步的调用账（wa_contacts.go 的单飞与节流用例用）：
	// syncCalls 记同步总次数，fullCalls 记其中 full=true 的次数，syncSeq 按调用顺序
	// 记每次收到的 full 值——「快路优先、只在必要时升级」这条口径只能靠序列断言。
	syncCalls int
	fullCalls int
	syncSeq   []bool
	// syncErr：注入同步失败（appstate.ErrKeyNotFound 那一支用例用），非 nil 即不碰 contacts。
	syncErr error
	// syncAdds：同步「成功」时并进 contacts 的联系人，仿真库的全量快照落库
	// （WM/appstate.go:186 PutAllContactNames）。有了它，「后进者拿锁后双检本地、
	// 不再重复拉」的单飞语义才有可测的形态。
	syncAdds map[types.JID]types.ContactInfo
	// syncGate：非 nil 时同步在**拿锁之前**等它（永不关即等到 ctx 到点）。用于钉住
	// 「本包内部同步预算到点」与「调用方断连」两种失败必须报成两件事
	// （wa_sync_timeout_test.go，默认 nil ⇒ 既有行为逐字不变）。
	syncGate chan struct{}

	// 外联凭证门闸的预置与调用账（wa_reachout_test.go）：salt/tokens 是 NCTSalt 与
	// PrivacyToken 的现值（零值 ⇒ 凭证缺位），rhSeq 记 SyncRegularHigh 的入参序列，
	// histReqs 记向手机要历史的次数，onHistRequest 仿真手机推历史后的落库回调。
	salt          []byte
	tokens        map[types.JID][]byte
	rhSeq         [][2]bool
	histReqs      int
	onHistRequest func()
	// requireCreds：SendText 对无凭证的 DM 回 463（仿真服务器 ack）。
	requireCreds bool
	// requireToken：SendText 只认对方 tctoken，有盐带 cstoken 也回 463（:9 形态）。
	requireToken bool

	// ── 以下两字段是 wa_kick_liveness_test.go（前缀 kl）用的挂点，默认零值 ⇒ 既有行为逐字不变 ──
	// holdLoginPending：Login 返回 nil 但**不**把 loggedIn 置真。
	//
	// 它仿真的是 whatsmeow 真机上确实存在的那一档窗口：ConnectContext 返回 nil 只代表
	// socket 与 Noise 握手完成，IsLoggedIn 要等服务端接受凭据（ref iq 回执）才翻真
	// ——真机实测 connect ok 时 loggedin=false，约 1 秒后才派发 Connected
	// （口径见 wa_manager.go 的 waitLoggedIn 注释）。生产里这一档只有几十到上千毫秒，
	// 靠既有时序撞不出来；把它做成一个可置位的稳态，「常驻协程正卡在 waitLoggedIn」
	// 才有确定性窗口可测，而不是靠 nanosecond 级的运气（wa_reconnect_test.go 三条用例
	// 的偶红就是撞这个运气）。
	holdLoginPending bool
	// releaseAfter：第 N 次 Login 起恢复正常置真（N<=0 读作「永远挂起」）。
	// 取 2 就是「只有被催之后重连的那一轮才真正确认登录」——正好是门面等得到的那一档。
	releaseAfter int
	// loginSettleDelay：每轮 Login 开工前先睡这么久（0 ⇒ 既有行为逐字不变）。
	//
	// 仿真的是「发起连接 + Noise 握手 + 等回执」这一来一回本身要耗时：真机上一轮重连
	// 不是当场归零的。只有把它做成可控档位，「库侧等自己到点之后才去重连」与
	// 「被门面催之后才去重连」这两条路径的落定时刻才能差出一个可判别的量级
	// （赛跑推导见 wa_kick_liveness_test.go 文件头）。
	loginSettleDelay time.Duration
}

type fakeText struct{ to, body string }

// fakeImage 是 SendImage 的一条落点记录（图文混排用例断言用）。
type fakeImage struct{ to, url, caption string }

// fakeInteractive 是 SendInteractive 的一条落点记录（CTA 按钮消息用例断言用）。
type fakeInteractive struct{ to, body, url, buttonText, buttonURL string }

func (f *fakeSession) Login(ctx context.Context) error {
	// kl 挂点（默认零值不进入）：一轮连接的来回耗时。睡在**锁外**——睡在锁内会把门面
	// 与常驻协程两边的 LoggedIn() 轮询一起堵死，测出来的就不是赛跑而是互相阻塞。
	if d := f.loginSettleDelay; d > 0 {
		time.Sleep(d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginCalls++
	if f.loginErr != nil {
		return f.loginErr
	}
	// kl 挂点（wa_kick_liveness_test.go）：两个字段都是零值时**不进入**这个分支，
	// 既有行为逐字不变。进入时仿真相机形态——ConnectContext 已返回 nil（socket 与
	// Noise 握手完成），但登录回执还没落定，所以 loggedIn 保持 false：
	// 常驻协程会稳稳卡在 waitLoggedIn 里，而不是靠 Login 返回与首次检查之间的
	// nanosecond 级运气才撞上这一档。
	if f.holdLoginPending && (f.releaseAfter <= 0 || f.loginCalls < f.releaseAfter) {
		return nil
	}
	f.loggedIn = true
	return nil
}

func (f *fakeSession) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loggedIn
}

// Connected 与 LoggedIn 同源：假会话不区分「凭据被接受」与「socket 在」，
// 登录即视为已连（waSession 接口方法，sessionReady 消费）。
func (f *fakeSession) Connected() bool {
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
// 顺带把 downed 令牌递进去，常驻协程立刻自己重连，测不出 reconnectAndAwait 的催连是否真生效。
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
	// 真库在 LID 解析与会话库读写上都用调用方 ctx，取消时当场返回 ctx.Err()
	//（错误串形如 "failed to get LID for PN ...: context canceled"）。假件照做，
	// 门面那句「调用方上下文已结束」的折算才有活闸可测。
	if ctx != nil && ctx.Err() != nil {
		return "", time.Time{}, ctx.Err()
	}
	ts := f.sendTS
	if ts.IsZero() {
		ts = time.Now()
	}
	f.sent = append(f.sent, fakeText{to: to, body: body})
	// requireCreds（wa_reachout_test.go）：仿真服务器对「没带 tctoken/cstoken 的 DM」
	// 在 ack 上回 463（WM/send.go:459-461）。凭证现值与门闸读的是同一份 salt/tokens。
	// 群组不挂 token（WM/send.go 群分支），所以只拦用户域。
	if f.requireCreds {
		if jid, jerr := types.ParseJID(to); jerr == nil &&
			(jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer) &&
			len(f.salt) == 0 && len(f.tokens[jid]) == 0 {
			return f.sendID, ts, errors.New("server returned error 463")
		}
	}
	// requireToken：更严一档的服务器形态——**有盐带 cstoken 也照样 463**，只认对方
	// tctoken（真机 2026-10-16 :9 批次实证：已存在会话的收件人拒收 cstoken 凭证）。
	if f.requireToken {
		if jid, jerr := types.ParseJID(to); jerr == nil &&
			(jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer) &&
			len(f.tokens[jid]) == 0 {
			return f.sendID, ts, errors.New("server returned error 463")
		}
	}
	// downAfterSends：发满第 N 条就把登录态按回 false，仿「批跑到一半 socket 掉了」。
	if f.downAfterSends > 0 && len(f.sent) >= f.downAfterSends {
		f.loggedIn = false
	}
	return f.sendID, ts, f.sendErr
}

// SendImage 是图文分支的假件：只记录「发了哪张图、说明是什么」，不碰网络。
//
// 图片下载与上传都在 wa_media.go 的 waConn 里，假件这一层只对应
// 「已经拿到可发形态之后的那一次投递」，所以这里不解码、不校验 URL。
func (f *fakeSession) SendImage(ctx context.Context, to string, img OutboundImage) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return "", time.Time{}, ctx.Err()
	}
	ts := f.sendTS
	if ts.IsZero() {
		ts = time.Now()
	}
	f.sentImages = append(f.sentImages, fakeImage{to: to, url: img.URL, caption: img.Caption})
	return f.sendID, ts, f.sendErr
}

// SendInteractive 是 CTA 按钮分支的假件：只记录落点；linkErr 非 nil 时模拟
// 「服务端不接受交互消息」，用来钉住 waSendContent 的降级重发。
func (f *fakeSession) SendInteractive(ctx context.Context, to string, m OutboundInteractive) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return "", time.Time{}, ctx.Err()
	}
	if f.linkErr != nil {
		return "", time.Time{}, f.linkErr
	}
	ts := f.sendTS
	if ts.IsZero() {
		ts = time.Now()
	}
	f.sentLinks = append(f.sentLinks, fakeInteractive{
		to: to, body: m.Body, url: m.ImageURL, buttonText: m.ButtonText, buttonURL: m.ButtonURL,
	})
	return f.sendID, ts, f.sendErr
}

func (f *fakeSession) SendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	f.mu.Lock()
	f.patches = append(f.patches, patch)
	cb := f.onState
	err := f.stateErr
	f.mu.Unlock()
	// 回调在锁外（与 RequestRecentHistory 同纪律）：用例要在 SendAppState 期间
	// 检查别的锁（如 gateMu），不能让假件自己的锁参与锁序。
	if cb != nil {
		cb()
	}
	return err
}

func (f *fakeSession) GetContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 交副本，不交内部 map：真库的 GetAllContacts 是每次从表里读出一个新 map
	//（WM/store/sqlstore/contacts.go），调用方拿到的东西不会再被写。直接把 f.contacts
	// 端出去会让「并发读 + 同步写」的用例报出假件自己的 data race（真库不存在这条），
	// 反而把要测的东西盖住。
	if f.contacts == nil {
		return nil, f.contactsErr
	}
	out := make(map[types.JID]types.ContactInfo, len(f.contacts))
	for k, v := range f.contacts {
		out[k] = v
	}
	return out, f.contactsErr
}

func (f *fakeSession) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups, f.groupsErr
}

func (f *fakeSession) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lidCalls++
	if f.lidErr != nil {
		return types.JID{}, f.lidErr
	}
	return f.lidMap[pn], nil
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

// countLidLookups 读 GetLIDForPN 的调用次数（群组目标「不改写、不查询」的断言用）。
func (f *fakeSession) countLidLookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lidCalls
}

// SyncContactAppState 记一次同步调用账，成功时把 syncAdds 并进 contacts
// （仿真库的 PutAllContactNames 整表落库，见 wa_client.go 的真实现注释）。
func (f *fakeSession) SyncContactAppState(ctx context.Context, full bool) error {
	// syncGate 非空时先等它（**在拿 f.mu 之前**等）：仿「同步要往服务器发 IQ、一个来回
	// 迟迟不回」的形态，让门面那个内部同步预算先到点。不先释放锁就等的写法会把
	// LoggedIn() 一起堵死，测出来的就不是超时归因而是自造死锁。
	if f.syncGate != nil {
		select {
		case <-f.syncGate:
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	if full {
		f.fullCalls++
	}
	f.syncSeq = append(f.syncSeq, full)
	// 真库在这一步用的就是调用方 ctx（同步快照要往服务器发 IQ、落库要读写会话库），
	// 取消时当场返回 ctx.Err()——门面那句「上下文已结束」的折算靠这里才有活闸。
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if f.syncErr != nil {
		return f.syncErr
	}
	if len(f.syncAdds) == 0 {
		return nil
	}
	if f.contacts == nil {
		f.contacts = make(map[types.JID]types.ContactInfo, len(f.syncAdds))
	}
	for jid, c := range f.syncAdds {
		f.contacts[jid] = c
	}
	return nil
}

// countSyncs 读同步账：总次数、其中 full=true 的次数、按调用顺序的 full 序列副本。
func (f *fakeSession) countSyncs() (calls, fullCalls int, seq []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncCalls, f.fullCalls, append([]bool(nil), f.syncSeq...)
}

// setSync 预置同步的失败注入与「成功后补进来的联系人」（与 syncAdds 合并的语义见上）。
func (f *fakeSession) setSync(syncErr error, syncAdds map[types.JID]types.ContactInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncErr = syncErr
	f.syncAdds = syncAdds
}

// ── 外联凭证门闸挂点（wa_reachout_test.go 用；零值 ⇒ 凭证缺位、无历史推送，既有行为不变）──

// SyncRegularHigh 记一次 (full, onlyIfNotSynced) 调用账，恒成功。
func (f *fakeSession) SyncRegularHigh(ctx context.Context, full, onlyIfNotSynced bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rhSeq = append(f.rhSeq, [2]bool{full, onlyIfNotSynced})
	return nil
}

func (f *fakeSession) NCTSalt(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.salt, nil
}

func (f *fakeSession) PrivacyToken(ctx context.Context, jid types.JID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[jid], nil
}

// RequestRecentHistory 记一次向手机要历史的调用；onHistRequest 非 nil 时在锁外回调，
// 供用例仿真「手机推历史 → 凭证落库」（回调里再 setReachout 写盐/token）。
func (f *fakeSession) RequestRecentHistory(ctx context.Context) error {
	f.mu.Lock()
	f.histReqs++
	cb := f.onHistRequest
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

// setReachout 预置凭证现值：盐与按目标 JID 的 tctoken。
func (f *fakeSession) setReachout(salt []byte, tokens map[types.JID][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.salt = salt
	f.tokens = tokens
}

// reachState 读门闸调用账：regular_high 的 (full, onlyIfNotSynced) 序列与要历史的次数。
func (f *fakeSession) reachState() (rhSeq [][2]bool, histReqs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]bool(nil), f.rhSeq...), f.histReqs
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

// waTestPNG 造一张 w×h 的纯色 PNG。
func waTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("造图失败: %v", err)
	}
	return buf.Bytes()
}

// TestWaFetchImageGuards 图片下载的三道闸：协议白名单、状态码、内容为空。
//
// 三条都是「配置错了要响亮失败」的场景：后台把 file:// 路径或 404 链接存进文案库，
// 只有在这里拒绝，运营才能在保存时看到原因，而不是发出去一条永远转圈的消息。
func TestWaFetchImageGuards(t *testing.T) {
	ctx := context.Background()

	if _, _, err := waFetchImage(ctx, "file:///etc/passwd"); err == nil ||
		!strings.Contains(err.Error(), "http/https") {
		t.Fatalf("非 http(s) 直链必须被拒: %v", err)
	}
	if _, _, err := waFetchImage(ctx, "   "); err == nil {
		t.Fatal("空直链必须被拒")
	}

	pngData := waTestPNG(t, 8, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "text/plain") // 故意配错：MIME 必须按内容嗅探
			_, _ = w.Write(pngData)
		case "/empty.png":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	data, mime, err := waFetchImage(ctx, srv.URL+"/ok.png")
	if err != nil {
		t.Fatalf("正常图片应当取到: %v", err)
	}
	if !bytes.Equal(data, pngData) {
		t.Fatalf("下载内容与源不一致: got %d bytes want %d", len(data), len(pngData))
	}
	if mime != "image/png" {
		t.Fatalf("MIME 必须按内容嗅探为 image/png，实得 %q", mime)
	}

	if _, _, err := waFetchImage(ctx, srv.URL+"/missing.png"); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("非 200 必须被拒且带状态码: %v", err)
	}
	if _, _, err := waFetchImage(ctx, srv.URL+"/empty.png"); err == nil ||
		!strings.Contains(err.Error(), "0 字节") {
		t.Fatalf("0 字节必须被拒: %v", err)
	}
	if got := waSniffImageMime([]byte("GIF89a")); got != "" {
		t.Fatalf("不认识的格式应当返回空 MIME，实得 %q", got)
	}
}

// TestWaMakeThumbnail 缩略图：等比缩到最长边 72、产出可解码的 JPEG、并回带原图尺寸。
func TestWaMakeThumbnail(t *testing.T) {
	src := waTestPNG(t, 200, 100)
	thumb, w, h := waMakeThumbnail(src)
	if w != 200 || h != 100 {
		t.Fatalf("原图尺寸应回带 200x100，实得 %dx%d", w, h)
	}
	if len(thumb) == 0 {
		t.Fatal("横图应当能生成缩略图")
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("缩略图必须是可解码的 JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != waImageThumbMaxSide || b.Dy() != waImageThumbMaxSide/2 {
		t.Fatalf("等比缩放结果应为 72x36，实得 %dx%d", b.Dx(), b.Dy())
	}

	// 小图不放大：本来就是 40x20，缩略图保持原尺寸。
	small := waTestPNG(t, 40, 20)
	thumb2, w2, h2 := waMakeThumbnail(small)
	if w2 != 40 || h2 != 20 || len(thumb2) == 0 {
		t.Fatalf("小图应当原尺寸出缩略图: %dx%d len=%d", w2, h2, len(thumb2))
	}
}

// TestWhatsappSendImageUsesImageBranch 图文消息走 SendImage 分支：
// 正文进入 Caption（不是 Conversation），目标归一成会话 JID，且不落进文本账。
func TestWhatsappSendImageUsesImageBranch(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	const imgURL = "https://admin.example.com/static/chatimg_abc.jpg"
	id, err := WhatsappSendImage(nil, "8613800000000", "+86 139-0000-0000", imgURL, " 你好，欢迎加入 ")
	if err != nil || id != "MSG-1" {
		t.Fatalf("图文发送: id=%q err=%v", id, err)
	}
	s.mu.Lock()
	imgs := append([]fakeImage(nil), s.sentImages...)
	texts := append([]fakeText(nil), s.sent...)
	s.mu.Unlock()
	if len(texts) != 0 {
		t.Fatalf("图文消息不得落进纯文本账: %+v", texts)
	}
	if len(imgs) != 1 {
		t.Fatalf("应当恰好发出一条图片消息，实得 %d", len(imgs))
	}
	if imgs[0].to != "8613900000000@s.whatsapp.net" {
		t.Fatalf("收件人未归一: %q", imgs[0].to)
	}
	if imgs[0].url != imgURL {
		t.Fatalf("图片直链不符: %q", imgs[0].url)
	}
	// 说明文字原样下传（trim 在 waConn.SendImage 组装 protobuf 时做）：
	// 框架层不做静默清洗，否则「发出去的文案」与「库里存的文案」会悄悄不一致。
	if imgs[0].caption != " 你好，欢迎加入 " {
		t.Fatalf("说明文字应当原样传递: %q", imgs[0].caption)
	}

	// 图与文都空：整条拒绝（不是「发了个空气」）。
	if id, err := WhatsappSendImage(nil, "8613800000000", "8613900000000", "", ""); err == nil || id != "" {
		t.Fatalf("图与文都空必须被拒: id=%q err=%v", id, err)
	}

	// 只有图没有说明是合法的（Caption 为空）。
	if _, err := WhatsappSendImage(nil, "8613800000000", "8613900000000", imgURL, ""); err != nil {
		t.Fatalf("纯图片消息应当被允许: %v", err)
	}
}

// TestWaBatchImageContent 批内每条按自己的 ImageURL 决定形态：
// 同一批里既有纯文本又有图文，两条必须落到各自的分支（自动发送逐条随机文案时，
// 有的文案带图、有的不带，这条闸就是那个混合场景）。
func TestWaBatchImageContent(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	// 速率闸放到 2 条：默认 send_rate 是 1|1，第二条会被闸拒而根本走不到发送分支，
	// 那样测出来的就不是「图文分支」而是「速率闸」了。
	cfg := &config.WhatsappSessionsConfig{SendRate: "1|2", AntibanEnabled: waBool(false)}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(_ time.Duration) {} // 批内间隔对本用例无意义，别真睡
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	var results []*SendResult
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "纯文本"},
		{To: "8613900000001", Body: "带图说明", ImageURL: "https://x/y.jpg"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "两条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	})

	s.mu.Lock()
	texts := append([]fakeText(nil), s.sent...)
	imgs := append([]fakeImage(nil), s.sentImages...)
	s.mu.Unlock()
	if len(texts) != 1 || texts[0].body != "纯文本" {
		t.Fatalf("纯文本条目应走文本分支: %+v", texts)
	}
	if len(imgs) != 1 || imgs[0].caption != "带图说明" || imgs[0].url != "https://x/y.jpg" {
		t.Fatalf("带图条目应走图片分支: %+v", imgs)
	}
}

// setPairCodeHandshakeTimeout 注入握手上限，t.Cleanup 还原（t.Fatalf 路径也能还原）。
// 码配对三支用例（T5/T7/T8）统一走这里，不再手搓「换-还原」——手搓的 defer 在
// t.Fatalf 路径上不还原，会把窗口留给后续用例。
func setPairCodeHandshakeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := waPairCodeHandshakeTimeout
	waPairCodeHandshakeTimeout = d
	t.Cleanup(func() { waPairCodeHandshakeTimeout = old })
}

// pcActiveCount 读出单飞记账表的全局条目数（「没留下第二个占位」判据）。
func pcActiveCount() int {
	n := 0
	waPairCodeActive.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// pcActiveHas 读出某号码是否在记账表里（含值为空串的占位形态）。
func pcActiveHas(digits string) bool {
	_, ok := waPairCodeActive.Load(digits)
	return ok
}

// pcSessionCount 读出管理器活动表里的会话数。
func pcSessionCount(m *waManager) int {
	n := 0
	m.qr.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// pcCodeGoroutines 数出当前仍停在 wa_pairing_code.go 里的协程条数。
// 判据按「协程块」计数而不是字符串出现次数：一条协程在同一文件里可能有多个帧。
// 抓栈本身复用 wa_contacts_hardening_test.go 的 goroutinesInFile（同包共享）。
func pcCodeGoroutines(t *testing.T) int {
	t.Helper()
	return goroutinesInFile(t, "wa_pairing_code.go")
}

// pcWaitNoPhone 断言错误文案里没有完整号码、且掩码形态逐字在内。
func pcWaitNoPhone(t *testing.T, err error, digits, masked string) {
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
	setPairCodeHandshakeTimeout(t, 30*time.Second)

	m, st, _ := startFakeWA(t, nil, nil)
	tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })
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
	if n := pcCodeGoroutines(t); n < 2 {
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
	pcWaitNoPhone(t, r.err, digits, waMaskPhone(digits))
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
	waitUntil(t, 3*time.Second, "取消后单飞记账清理", func() bool { return !pcActiveHas(digits) })
	if n := pcActiveCount(); n != 0 {
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
	if n := tr.pcTotalPairCalls(); n != 0 {
		t.Errorf("首个 code 项未到，不得调用 PairPhone，实得 %d 次", n)
	}
	// 泵收尾后不得留下阻塞中的 done 钩子协程。
	waitUntil(t, 3*time.Second, "取消后无残留配对码协程", func() bool { return pcCodeGoroutines(t) == 0 })
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
			setPairCodeHandshakeTimeout(t, 15*time.Second)
			m, st, _ := startFakeWA(t, nil, nil)
			closeFirst, terminalFirst := tc.close, tc.termin
			tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) {
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
			pcWaitNoPhone(t, err, digits, waMaskPhone(digits))

			waitUntil(t, 3*time.Second, "终局后活动表清理", func() bool {
				_, ok := m.LookupQRSession(sessionID)
				return !ok
			})
			waitUntil(t, 3*time.Second, "终局后单飞记账清理", func() bool { return !pcActiveHas(digits) })
			if n := pcActiveCount(); n != 0 {
				t.Errorf("终局后全局号码记账应为 0，实得 %d", n)
			}
			if n := pcSessionCount(m); n != 0 {
				t.Errorf("终局后活动表会话数应为 0，实得 %d", n)
			}
			if n := tr.pcTotalPairCalls(); n != 0 {
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
			waitUntil(t, 3*time.Second, "终局后无残留配对码协程", func() bool { return pcCodeGoroutines(t) == 0 })
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
// → 门面拿到 pcEarlyPairCode，返回值逐字断言与「PairPhone 恰一次」全红；
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
			item:      whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "w1://pc-raw-qr", Timeout: 20 * time.Second},
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
			setPairCodeHandshakeTimeout(t, 15*time.Second)
			m, st, _ := startFakeWA(t, nil, nil)
			pre := tc.item
			tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) {
				f.preItems = []whatsmeow.QRChannelItem{pre}
				f.codeAfterPre = !tc.isCode
			})
			events := pcCaptureQREvents(t)

			const digits = "8613800000023"
			code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0023")
			if err != nil {
				t.Fatalf("中间项之后出现 code 项应当成功拿码: %v", err)
			}
			if code != pcFakePairCode {
				t.Errorf("配对码应为 %q，实得 %q（非 code 中间项被误取时假件返回 %q）",
					pcFakePairCode, code, pcEarlyPairCode)
			}
			fake := tr.last()
			if fake == nil {
				t.Fatal("工厂没造出会话")
			}
			if n := fake.pcPairCount(); n != 1 {
				t.Errorf("PairPhone 应恰好一次（每个中间项各调一次说明没盯 code 项），实得 %d 次", n)
			}
			want := []string{"QRChannel", "Connect", "PairPhone"}
			if got := fake.pcCalls(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
				t.Errorf("调用顺序不符: %v（期望 %v）", got, want)
			}
			if _, phone, _, _, _ := fake.pcArgs(); phone != digits {
				t.Errorf("PairPhone 号码应为归一形态 %q，实得 %q", digits, phone)
			}

			// 中间帧必须由泵原样扇出，且严格排在 pair_code 之前。
			// 帧是泵协程扇出的，收集器读到它们与门面返回之间没有 happens-before，
			// 立刻快照会随调度漂（HEAD 上 -race 连跑约 1/3 命中，命中的行也不固定），
			// 所以按「应有帧到齐」有界等待后再逐字断言：等到才判、等不到响亮红。
			// 生产侧 completePairCode 已把扇出排在交付门面之前，正常路径第一轮即过。
			waitUntil(t, 2*time.Second, "泵扇出中间项与 pair_code 帧", func() bool {
				var pre, pair bool
				for _, ev := range events() {
					if ev.Event == tc.wantEvent && (tc.wantError == "" || ev.Error == tc.wantError) {
						pre = true
					}
					if ev.Event == QREventPairCode && ev.Code == code {
						pair = true
					}
				}
				return pre && pair
			})
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
					if ev.Code != "w1://pc-raw-qr" {
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

			fake.pcTimeout()
			waitUntil(t, 3*time.Second, "会话收尾后记账清理", func() bool { return !pcActiveHas(digits) })
			waitUntil(t, 3*time.Second, "会话收尾后活动表清理", func() bool { return pcSessionCount(m) == 0 })
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
	tr := pcInstallPairFactory(t, st, nil)

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
	if ev.Code != code || ev.Code != pcFakePairCode {
		t.Errorf("补投的配对码应为 %q，实得 %q（门面返回 %q）", pcFakePairCode, ev.Code, code)
	}
	if ev.SessionID != sessionID {
		t.Errorf("补投帧 session_id 不符: %+v", ev)
	}

	// 补投之后订阅仍是在册的：后续轮换帧要继续收到（不是一次性补投）。
	tr.last().pcSend(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "w1://pc-raw-qr-2", Timeout: 20 * time.Second,
	}, false)
	ev2 := readEvent(t, sub, "补投之后的实时帧")
	if ev2.Event != "code" || ev2.Code != "w1://pc-raw-qr-2" {
		t.Fatalf("补投后应继续收到实时帧，实得 %+v", ev2)
	}

	// 终局之后再来一个迟到订阅者：补投终局那一条，随后通道关闭。
	tr.last().pcTimeout()
	waitUntil(t, 3*time.Second, "会话终局出活动表", func() bool { return pcSessionCount(m) == 0 })
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
	waitUntil(t, 3*time.Second, "终局后记账清理", func() bool { return !pcActiveHas(digits) })
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
	tr := pcInstallPairFactory(t, st, nil)

	const digits = "8613800000025"
	code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0025")
	if err != nil {
		t.Fatalf("门面同步返回失败: %v", err)
	}
	defer tr.last().pcTimeout()

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
	want := `{"session_id":"` + sessionID + `","event":"pair_code","code":"` + pcFakePairCode + `"}`
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
	tr.last().pcTimeout()
	waitUntil(t, 3*time.Second, "收场后记账清理", func() bool { return !pcActiveHas(digits) })
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
		setPairCodeHandshakeTimeout(t, 15*time.Second)
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pcInstallPairFactory(t, st, nil)
		if n := pcActiveCount(); n != 0 {
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
		if n := pcSessionCount(m); n != 2 {
			t.Errorf("活动表应有两条会话，实得 %d 条", n)
		}
		if pcClaimed(digitsA) != rA.id || pcClaimed(digitsB) != rB.id {
			t.Errorf("号码记账应按各自号码分别登记: A=%q B=%q", pcClaimed(digitsA), pcClaimed(digitsB))
		}
		// 号码互不串：两条会话各自的 PairPhone 实参必须是自己那个号码。
		var sawA, sawB bool
		for _, f := range tr.all() {
			_, phone, _, _, _ := f.pcArgs()
			switch phone {
			case digitsA:
				sawA = true
			case digitsB:
				sawB = true
			default:
				t.Errorf("PairPhone 收到意外号码 %q", phone)
			}
			if n := f.pcPairCount(); n != 1 {
				t.Errorf("每条会话 PairPhone 应恰一次，实得 %d 次", n)
			}
		}
		if !sawA || !sawB {
			t.Fatalf("两个号码应各自调用 PairPhone（A=%v B=%v）", sawA, sawB)
		}

		for _, f := range tr.all() {
			f.pcTimeout()
		}
		waitUntil(t, 3*time.Second, "两号码收场后放号", func() bool { return pcActiveCount() == 0 })
		waitUntil(t, 3*time.Second, "两号码收场后活动表清空", func() bool { return pcSessionCount(m) == 0 })
	})

	t.Run("同号并发第二呼被拒且不留第二个会话", func(t *testing.T) {
		setPairCodeHandshakeTimeout(t, 30*time.Second)
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })
		if n := pcActiveCount(); n != 0 {
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
		pcWaitNoPhone(t, err, digits, waMaskPhone(digits))
		// 被拒的第二呼不许建会话、也不许多留一条记账（含空串占位形态）。
		if n := tr.built(); n != 1 {
			t.Errorf("第二呼不得建第二个会话，实得 %d 条", n)
		}
		if n := pcSessionCount(m); n != 1 {
			t.Errorf("活动表计数应为 1，实得 %d", n)
		}
		if n := pcActiveCount(); n != 1 {
			t.Errorf("号码记账计数应为 1，实得 %d（第二呼多留了占位）", n)
		}

		// 放行第一呼拿码，再收终局：号码必须可重发（记账随会话清理删除）。
		tr.last().pcPushCode()
		var r result
		select {
		case r = <-first:
		case <-time.After(5 * time.Second):
			t.Fatal("放行后第一呼未返回")
		}
		if r.err != nil || r.code != pcFakePairCode {
			t.Fatalf("第一呼应成功拿码: code=%q err=%v", r.code, r.err)
		}
		tr.last().pcTimeout()
		waitUntil(t, 3*time.Second, "同号收场后放号", func() bool { return pcActiveCount() == 0 })

		// 放号后同号立即可重发（记账没漏删才走得通）。
		// 重发段用短上限：只要证明它越过了单飞闸、进到握手等待即可，不必真等 30s。
		setPairCodeHandshakeTimeout(t, 80*time.Millisecond)
		tr2 := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })
		_, _, err = WhatsappGetPairingCode(context.Background(), "+86 138-0000-0043")
		if err == nil || !strings.Contains(err.Error(), "配对码握手超时") {
			t.Fatalf("放号后同号应重新进入握手等待（而不是被单飞拒绝），实得 err=%v", err)
		}
		if n := tr2.built(); n != 1 {
			t.Errorf("重发应新建一条会话，实得 %d 条", n)
		}
		tr2.last().pcTimeout()
		waitUntil(t, 3*time.Second, "重发收场后放号", func() bool { return pcActiveCount() == 0 })
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
		tr := pcInstallPairFactory(t, st, nil)
		events := pcCaptureQREvents(t)

		const digits = "8613800000051"
		code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+86 138-0000-0051")
		if err != nil {
			t.Fatalf("门面同步返回失败: %v", err)
		}
		if code != pcFakePairCode {
			t.Errorf("停机前应已拿到配对码 %q，实得 %q", pcFakePairCode, code)
		}
		if pcClaimed(digits) != sessionID {
			t.Fatalf("配对进行中号码应登记会话 id，实得 %q", pcClaimed(digits))
		}
		// 停机前钩子协程确实在 parked 状态（证明下面的清理不是空跑）。
		if n := pcCodeGoroutines(t); n < 1 {
			t.Fatalf("前置不成立：停机前应有 done 钩子协程停在配对码门面里，实得 %d", n)
		}

		m.StopAll(time.Second)

		waitUntil(t, 3*time.Second, "停机后活动表清空", func() bool { return pcSessionCount(m) == 0 })
		waitUntil(t, 3*time.Second, "停机后号码放号", func() bool { return !pcActiveHas(digits) })
		if n := pcActiveCount(); n != 0 {
			t.Errorf("停机后全局号码记账应为 0，实得 %d（悬挂条目会把号码永久锁死）", n)
		}
		if n := tr.last().pcDisconn(); n < 1 {
			t.Errorf("StopAll 应经 Cancel 断连，实得 Disconnect %d 次", n)
		}
		waitUntil(t, 3*time.Second, "停机后无残留配对码协程", func() bool { return pcCodeGoroutines(t) == 0 })
		pcLeakScan(t, events(), digits)

		// 停机收场后同号可重发（换一台进程继续配对不该被记账卡住）。
		tr2 := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })
		setPairCodeHandshakeTimeout(t, 60*time.Millisecond)
		_, _, err = WhatsappGetPairingCode(context.Background(), "+86 138-0000-0051")
		if err == nil || !strings.Contains(err.Error(), "配对码握手超时") {
			t.Fatalf("停机后同号应能重新发起握手，实得 err=%v", err)
		}
		if n := tr2.built(); n != 1 {
			t.Errorf("重发应新建一条会话，实得 %d 条", n)
		}
		waitUntil(t, 3*time.Second, "重发握手超时后放号", func() bool { return pcActiveCount() == 0 })
	})

	t.Run("握手等待中停机", func(t *testing.T) {
		setPairCodeHandshakeTimeout(t, 30*time.Second)
		m, st, _ := startFakeWA(t, nil, nil)
		tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })

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
		pcWaitNoPhone(t, r.err, digits, waMaskPhone(digits))
		waitUntil(t, 3*time.Second, "停机后号码放号", func() bool { return !pcActiveHas(digits) })
		if n := pcSessionCount(m); n != 0 {
			t.Errorf("停机后活动表应为空，实得 %d 条", n)
		}
		if n := tr.last().pcDisconn(); n < 1 {
			t.Errorf("停机应断连，实得 Disconnect %d 次", n)
		}
		waitUntil(t, 3*time.Second, "停机后无残留配对码协程", func() bool { return pcCodeGoroutines(t) == 0 })
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
		configure func(*pcFakePairSession)
		timeout   bool
		wantErr   string
	}{
		{"成功拿码后终局", "8613800000071", nil, false, ""},
		{"PairPhone被拒", "8613800000072", func(f *pcFakePairSession) { f.pairErr = errors.New("pair refused by server") }, false, "PairPhone 被拒"},
		{"握手超时未到code项", "8613800000073", func(f *pcFakePairSession) { f.holdCode = true }, true, "配对码握手超时"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timeout {
				setPairCodeHandshakeTimeout(t, 80*time.Millisecond)
			} else {
				setPairCodeHandshakeTimeout(t, 15*time.Second)
			}
			m, st, _ := startFakeWA(t, nil, nil)
			configure := tc.configure
			tr := pcInstallPairFactory(t, st, configure)

			digits := tc.digits
			code, sessionID, err := WhatsappGetPairingCode(context.Background(), "+"+digits)
			fake := tr.last()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("成功态不应报错: %v", err)
				}
				if code != pcFakePairCode {
					t.Errorf("成功态应拿到 %q，实得 %q", pcFakePairCode, code)
				}
				// 此刻 done 钩子必然还挂在 <-s.Done() 上（会话尚未终局）。
				if n := pcCodeGoroutines(t); n < 1 {
					t.Fatalf("前置不成立：应在协程栈里数到停在配对码门面的 done 钩子，实得 %d", n)
				}
				fake.pcTimeout()
			} else {
				if err == nil {
					t.Fatalf("失败态应报错，实得 code=%q", code)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("错误文案应含 %q: %v", tc.wantErr, err)
				}
				pcWaitNoPhone(t, err, digits, waMaskPhone(digits))
			}

			waitUntil(t, 3*time.Second, "三态收场后号码放号", func() bool { return !pcActiveHas(digits) })
			waitUntil(t, 3*time.Second, "三态收场后活动表清理", func() bool {
				_, ok := m.LookupQRSession(sessionID)
				return !ok
			})
			waitUntil(t, 3*time.Second, "三态收场后无停在配对码门面的协程", func() bool {
				return pcCodeGoroutines(t) == 0
			})
			if !waitManagerWG(m, 3*time.Second) {
				t.Error("泵协程未全部退出（管理器 wg 没归零）")
			}
			if n := pcActiveCount(); n != 0 {
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
		setPairCodeHandshakeTimeout(t, 60*time.Millisecond)
		// 5 位以上连续数字段：掩码形态里不可能出现（前 2 后 2 之间全是 *），
		// 出现即说明有完整号码或过长片段泄进文案。
		longRun := regexp.MustCompile(`[0-9]{5,}`)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, st, _ := startFakeWA(t, nil, nil)
				tr := pcInstallPairFactory(t, st, func(f *pcFakePairSession) { f.holdCode = true })
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
					if n := pcActiveCount(); n != 0 {
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
					waitUntil(t, 3*time.Second, "握手超时后放号", func() bool { return pcActiveCount() == 0 })
					waitUntil(t, 3*time.Second, "握手超时后无残留协程", func() bool { return pcCodeGoroutines(t) == 0 })
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

// PairPhone 补齐既有假件对扩展后 waPairSession 接口的实现（见文件头说明）。
func (*fakePairSession) PairPhone(context.Context, string, bool, whatsmeow.PairClientType, string) (string, error) {
	panic("QR 扫码配对路径不得调用 PairPhone（码配对断言请用 pcFakePairSession）")
}

// pcFakePairSession 是码配对的一次性假会话，T5/T7/T8 三支用例共用这一份。
// 默认形态：Connect 被调时自动向 QR 通道推首个 code 项（复刻「连接建立后库秒级
// 出码」时序，门面同步等待时不会卡死）；holdCode=true 时不出码，由测试手动推。
//
// 「首个 code 项之前」这一段可编排，供 T7 的负例形态使用：
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
type pcFakePairSession struct {
	dev   *store.Device
	store *fakeStore

	out chan whatsmeow.QRChannelItem

	mu    sync.Mutex
	order []string // 调用序记录仪："QRChannel" / "Connect" / "PairPhone"
	// 行为开关（创建当场由 configure 预置，泵协程读取时也在锁内）。
	closeFirst    bool
	terminalFirst bool
	holdCode      bool
	preItems      []whatsmeow.QRChannelItem
	codeAfterPre  bool
	pairErr       error
	pairCode      string // PairPhone 返回值（假码形态与库一致：XXXX-XXXX）
	earlyCode     string

	closed    bool
	disconn   int
	codeSent  bool
	pairCalls int
	// PairPhone 五实参留档，供逐字断言（T5 钉 ctx 非空与号码归一）。
	gotCtx   context.Context
	gotPhone string
	gotShow  bool
	gotType  whatsmeow.PairClientType
	gotName  string
}

// pcFakePairCode / pcEarlyPairCode 是两个可区分的假码（形态与库一致：XXXX-XXXX），
// 由安装子创建当场预置，configure 再覆盖成用例自己的码。
// pcRawQRCode 是假件自动推的首个二维码串（取码场景只要求它是「一条二维码帧」）。
const (
	pcFakePairCode  = "K3PD-7MZA"
	pcEarlyPairCode = "EARL-YBAD"
	pcRawQRCode     = "w1://pc-raw-qr"
)

func (f *pcFakePairSession) QRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	f.order = append(f.order, "QRChannel")
	f.mu.Unlock()
	// 复刻库侧对 ctx 取消的处置：关通道 + 断连（qrchan.go:124-132）。
	// StopAll/Cancel 路径的收敛依赖这条，假件不做就会把「停机释放号码」判成红。
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
	closeFirst := f.closeFirst
	terminalFirst := f.terminalFirst
	holdCode := f.holdCode
	preItems := append([]whatsmeow.QRChannelItem(nil), f.preItems...)
	codeAfterPre := f.codeAfterPre
	f.mu.Unlock()

	switch {
	case closeFirst:
		f.pcClose()
	case terminalFirst:
		f.pcSend(whatsmeow.QRChannelTimeout, true)
	default:
		for _, it := range preItems {
			f.pcSend(it, false)
		}
		if codeAfterPre || (!holdCode && len(preItems) == 0) {
			f.pcPushCode()
		}
	}
	return nil
}

func (f *pcFakePairSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconn++
}

func (f *pcFakePairSession) Device() *store.Device { return f.dev }

// LoggedIn 复刻库侧 IsLoggedIn 的口径：设备 JID 已写入即算登录。泵在配对成功后
// 靠它决定要不要用落库凭据重登（wa_pairing.go 的 reloginPaired）。
func (f *pcFakePairSession) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev != nil && f.dev.ID != nil
}

// pcDisconn 读出假件被 Disconnect 的次数（StopAll 收会话的判据）。
func (f *pcFakePairSession) pcDisconn() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disconn
}

// PairPhone 记录五实参留档；没见到 code 项就被调时返回可区分的坏码（earlyCode）。
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
	if !f.codeSent {
		// 握手把非 code 的中间项当成了首个 code 项——返回一个可区分的坏码。
		return f.earlyCode, nil
	}
	return f.pairCode, nil
}

// pcArgs 读出五实参留档快照。
func (f *pcFakePairSession) pcArgs() (context.Context, string, bool, whatsmeow.PairClientType, string) {
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
// terminal=true 投完即关（库口径：终局项之后立即 close）。见 code 项即置 codeSent，
// PairPhone 的返回值据此区分「真见到码」与「误取中间项」。
func (f *pcFakePairSession) pcSend(item whatsmeow.QRChannelItem, terminal bool) {
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

// pcPushCode 手动补投首个 code 项（单飞/并发用例里放行在途调用用）。
func (f *pcFakePairSession) pcPushCode() {
	f.pcSend(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: pcRawQRCode, Timeout: 20 * time.Second,
	}, false)
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

func (tr *pcPairTracker) all() []*pcFakePairSession {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]*pcFakePairSession(nil), tr.list...)
}

// pcTotalPairCalls 汇总全部假会话的 PairPhone 调用次数（断言「没被误取」用）。
func (tr *pcPairTracker) pcTotalPairCalls() int {
	n := 0
	for _, f := range tr.all() {
		n += f.pcPairCount()
	}
	return n
}

// pcInstallPairFactory 换掉 waPairSessionFactory，t.Cleanup 还原全局挂点。
// configure 在创建当场调用，用于预置 holdCode/pairErr/preItems 等分支。
func pcInstallPairFactory(t *testing.T, st *fakeStore, configure func(*pcFakePairSession)) *pcPairTracker {
	t.Helper()
	tr := &pcPairTracker{}
	old := waPairSessionFactory
	oldSettle := waPairReloginSettle
	waPairReloginSettle = 0
	waPairSessionFactory = func(dev *store.Device, log waLog.Logger) waPairSession {
		f := &pcFakePairSession{dev: dev, store: st, out: make(chan whatsmeow.QRChannelItem, 8),
			pairCode: pcFakePairCode, earlyCode: pcEarlyPairCode}
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
	gotCtx, gotPhone, gotShow, gotType, gotName := fake.pcArgs()
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
	// secondConnectErr 复刻配对成功后 515 重连占着 socket：第二次 Connect 返回
	// ErrAlreadyConnected，不能当成失败。loginAfter 到期后 forceLoggedIn 变真。
	secondConnectErr error
	loginAfter       time.Duration
	secondUsed       bool
	forceLoggedIn    bool
	ctx              context.Context
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
	if f.connected >= 1 && f.secondConnectErr != nil && !f.secondUsed {
		f.secondUsed = true
		delay := f.loginAfter
		f.mu.Unlock()
		if delay > 0 {
			go func() {
				time.Sleep(delay)
				f.mu.Lock()
				f.forceLoggedIn = true
				f.mu.Unlock()
			}()
		}
		return f.secondConnectErr
	}
	f.connected++
	err := f.connectErr
	f.mu.Unlock()
	return err
}

func (f *fakePairSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected++
}

func (f *fakePairSession) Device() *store.Device { return f.dev }

func (f *fakePairSession) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceLoggedIn {
		return true
	}
	return f.dev != nil && f.dev.ID != nil && f.connected >= 2
}

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
	oldSettle := waPairReloginSettle
	waPairReloginSettle = 0
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
	t.Cleanup(func() {
		waPairSessionFactory = old
		waPairReloginSettle = oldSettle
	})
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

// TestPairingSuccessIgnoresAlreadyConnected 配对成功时库的 515 重连还占着 socket，
// 补 Connect 会得到 ErrAlreadyConnected。这不能把扫码标成失败，要等到 IsLoggedIn。
func TestPairingSuccessIgnoresAlreadyConnected(t *testing.T) {
	st := newFakeStore()
	m := newTestManager(t, st, nil)
	tr := installFakePairFactory(t, st, func(f *fakePairSession) {
		f.secondConnectErr = whatsmeow.ErrAlreadyConnected
		f.loginAfter = 150 * time.Millisecond
	})

	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}
	sub, unsub := sess.Subscribe()
	defer unsub()
	fake := tr.last()
	waitUntil(t, 2*time.Second, "配对连接", func() bool { return fake.connectCount() >= 1 })

	const newJID = "8613800000009@s.whatsapp.net"
	fake.pairSuccess(newJID)
	ev := readEvent(t, sub, "成功事件")
	if ev.Event != "success" || ev.JID != newJID {
		t.Fatalf("515 占线不应把扫码标成失败: %+v", ev)
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

// resetWAProxy 把全局代理状态与构造期挂点还原到测试前，避免用例互相污染。
func resetWAProxy(t *testing.T) {
	t.Helper()
	oldURL := waProxyURL.Load()
	oldHook := waApplyClientProxy
	t.Cleanup(func() {
		waProxyURL.Store(oldURL)
		waApplyClientProxy = oldHook
	})
	waProxyURL.Store(nil)
}

// installWAProxyTestManager 把管理器挂到包级快照上（WhatsappSetProxy 靠它找在跑会话）。
func installWAProxyTestManager(t *testing.T, m *waManager) {
	t.Helper()
	old := waMgr.Swap(m)
	t.Cleanup(func() { waMgr.Store(old) })
}

// SetProxy 是 waProxySetter 的假实现：记录地址；proxyErr 非 nil 时模拟库侧失败。
func (f *fakeSession) SetProxy(addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proxyErr != nil {
		return f.proxyErr
	}
	f.proxyAddrs = append(f.proxyAddrs, addr)
	return nil
}

func (f *fakeSession) proxyLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.proxyAddrs...)
}

func TestWaNormalizeProxy(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		port    int
		want    string
		wantErr bool
	}{
		{"裸host带端口参数", "127.0.0.1", 7890, "http://127.0.0.1:7890", false},
		{"host:port无scheme", "127.0.0.1:8080", 0, "http://127.0.0.1:8080", false},
		{"裸host无端口", "proxy.local", 0, "http://proxy.local", false},
		{"https端口覆盖", "https://proxy.local", 443, "https://proxy.local:443", false},
		{"socks5带认证", "socks5://user:pass@proxy.local", 1080, "socks5://user:pass@proxy.local:1080", false},
		{"认证且端口覆盖", "http://user:pass@127.0.0.1:8080", 7890, "http://user:pass@127.0.0.1:7890", false},
		{"IPv6", "[::1]", 9000, "http://[::1]:9000", false},
		{"空串", "   ", 0, "", true},
		{"不支持的协议", "ftp://host:21", 0, "", true},
		{"URL端口越界", "http://host:70000", 0, "", true},
		{"参数端口越界", "host", 65536, "", true},
		{"参数端口负数", "host", -1, "", true},
		{"带path", "http://host:8080/x", 0, "", true},
		{"带查询串", "http://host:8080?x=1", 0, "", true},
		{"缺host", "http://", 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeWAProxy(tc.raw, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("规范化结果 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestWaProxyDisplayMasksPassword(t *testing.T) {
	got := waProxyDisplay("http://user:s3cret@127.0.0.1:8080")
	if strings.Contains(got, "s3cret") {
		t.Fatalf("展示串泄露了口令: %q", got)
	}
	if want := "http://user@127.0.0.1:8080（含认证）"; got != want {
		t.Fatalf("展示串 = %q，期望 %q", got, want)
	}
	if waProxyDisplay("") != "" {
		t.Fatalf("空代理的展示串应为空，得到 %q", waProxyDisplay(""))
	}
}

func TestWaProxyConstructorHookAppliesAddr(t *testing.T) {
	resetWAProxy(t)
	var got []string
	waApplyClientProxy = func(_ *whatsmeow.Client, addr string) error {
		got = append(got, addr)
		return nil
	}

	newWaConn(&store.Device{}, waLog.Noop)
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("未设置代理时应以空地址交给 hook（走环境变量），实际 %v", got)
	}

	addr, err := normalizeWAProxy("127.0.0.1", 7890)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	waProxyURL.Store(&addr)
	newWaConn(&store.Device{}, waLog.Noop)
	newWaPairConn(&store.Device{}, waLog.Noop)
	if len(got) != 3 || got[1] != addr || got[2] != addr {
		t.Fatalf("发信/配对构造都该拿到 %q，实际 %v", addr, got)
	}
}

func TestWaProxyConstructorFailureBlocksLogin(t *testing.T) {
	resetWAProxy(t)
	boom := context.Canceled
	waApplyClientProxy = func(_ *whatsmeow.Client, _ string) error { return boom }

	sess := newWaConn(&store.Device{}, waLog.Noop)
	if err := sess.Login(context.Background()); err != boom {
		t.Fatalf("装代理失败时应拒绝连接并返回原错误，实际 %v", err)
	}

	pair := newWaPairConn(&store.Device{}, waLog.Noop)
	if err := pair.Connect(); err != boom {
		t.Fatalf("配对会话装代理失败时应拒绝连接，实际 %v", err)
	}
}

func TestWaProxySetReappliesRunningClients(t *testing.T) {
	resetWAProxy(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1})
	installWAProxyTestManager(t, m)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	f := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "会话登录", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		return lc >= 1 && logged
	})

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	waitUntil(t, 5*time.Second, "代理重放并重连", func() bool {
		lc, dc, logged, _, _ := f.snapshot()
		log := f.proxyLog()
		return len(log) == 1 && log[0] == "http://127.0.0.1:1080" && dc >= 1 && lc >= 2 && logged
	})
}

func TestWaProxySetFailureKeepsConnection(t *testing.T) {
	resetWAProxy(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, func(s *fakeSession) {
		s.proxyErr = context.Canceled
	})
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1})
	installWAProxyTestManager(t, m)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	f := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "会话登录", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		return lc >= 1 && logged
	})

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	lc, dc, logged, _, _ := f.snapshot()
	if dc != 0 || lc != 1 || !logged {
		t.Fatalf("单账号切代理失败时应保留旧连接（login=%d disconnect=%d logged=%v）", lc, dc, logged)
	}
}

func TestWaProxyClearRestoresEnvBehavior(t *testing.T) {
	resetWAProxy(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1})
	installWAProxyTestManager(t, m)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	f := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "会话登录", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		return lc >= 1 && logged
	})

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	waitUntil(t, 5*time.Second, "首次重放", func() bool {
		lc, _, _, _, _ := f.snapshot()
		return len(f.proxyLog()) == 1 && lc >= 2
	})

	if err := WhatsappClearProxy(); err != nil {
		t.Fatalf("清除代理失败: %v", err)
	}
	if got := WhatsappProxy(); got != "" {
		t.Fatalf("清除后 WhatsappProxy 应为空，实际 %q", got)
	}
	waitUntil(t, 5*time.Second, "清除后重放空地址并重连", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		log := f.proxyLog()
		return len(log) == 2 && log[1] == "" && lc >= 3 && logged
	})
}

func TestWaProxyBeforeStartOK(t *testing.T) {
	resetWAProxy(t)
	old := waMgr.Swap(nil)
	t.Cleanup(func() { waMgr.Store(old) })

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("未启动时设置代理应成功（留给之后构造的客户端）: %v", err)
	}
	if got := WhatsappProxy(); got != "http://127.0.0.1:1080" {
		t.Fatalf("WhatsappProxy = %q", got)
	}
	if err := WhatsappClearProxy(); err != nil {
		t.Fatalf("清除代理失败: %v", err)
	}
	if got := WhatsappProxy(); got != "" {
		t.Fatalf("清除后 WhatsappProxy 应为空，实际 %q", got)
	}
}

func TestWaProxyConfigDefaultOnlyWhenUnset(t *testing.T) {
	t.Run("配置装配", func(t *testing.T) {
		resetWAProxy(t)
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{ProxyURL: "socks5://proxy.local"}); err != nil {
			t.Fatalf("配置装配失败: %v", err)
		}
		if got := waProxyAddr(); got != "socks5://proxy.local" {
			t.Fatalf("代理地址 = %q", got)
		}
	})
	t.Run("运行时优先于配置", func(t *testing.T) {
		resetWAProxy(t)
		if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
			t.Fatalf("设置代理失败: %v", err)
		}
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{ProxyURL: "http://other:1"}); err != nil {
			t.Fatalf("已有运行时值时配置装配应静默跳过: %v", err)
		}
		if got := waProxyAddr(); got != "http://127.0.0.1:1080" {
			t.Fatalf("配置覆盖了运行时值: %q", got)
		}
	})
	t.Run("非法配置报错", func(t *testing.T) {
		resetWAProxy(t)
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{ProxyURL: "ftp://host:21"}); err == nil {
			t.Fatal("非法配置应报错（startWA 据此 fail-closed 不启动消息侧）")
		}
		if got := waProxyAddr(); got != "" {
			t.Fatalf("非法配置不应落状态，实际 %q", got)
		}
	})
	t.Run("空配置不动作", func(t *testing.T) {
		resetWAProxy(t)
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{}); err != nil {
			t.Fatalf("空配置应静默: %v", err)
		}
		if waProxyURL.Load() != nil {
			t.Fatal("空配置不应把状态从 nil（未设置）改成显式空串")
		}
	})
}

func TestWaProxyReapplySkipsPairing(t *testing.T) {
	resetWAProxy(t)
	st := newFakeStore()
	tr := installFakePairFactory(t, st, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{})
	installWAProxyTestManager(t, m)

	if _, err := m.StartPairing(context.Background()); err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}
	ps := tr.last()
	if ps == nil {
		t.Fatal("配对会话未创建")
	}
	var sess waPairSession = ps
	if _, ok := sess.(waProxySetter); ok {
		t.Fatal("配对会话不应实现 waProxySetter：换代理重连会打断 QR 泵")
	}
	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	if got := ps.disconnectCount(); got != 0 {
		t.Fatalf("进行中的配对会话被切换代理断开了 %d 次", got)
	}
}

const (
	reachAccount = "8613800000000:1@s.whatsapp.net"
	reachFriend  = "8613900000001"
	reachGroup   = "123456789@g.us"
)

// setReachoutClocks 把恢复档的轮询预算换成测试尺度，用完还原。
func setReachoutClocks(t *testing.T, wait, tick time.Duration) {
	t.Helper()
	oldWait, oldTick := waReachoutWait, waReachoutPollTick
	waReachoutWait, waReachoutPollTick = wait, tick
	t.Cleanup(func() { waReachoutWait, waReachoutPollTick = oldWait, oldTick })
}

// 取某账号的 wrapper 复用 wa_kick_liveness_test.go 的 mustWrapper（同包共享），
// 落空账等内部账直接读它。

// startReachWA 装配门闸测试用的门面快照：单账号、假会话按 configure 预置。
func startReachWA(t *testing.T, configure func(*fakeSession)) (*waManager, func() *fakeSession) {
	t.Helper()
	dev := fakeDevice(t, reachAccount, "android", "reach")
	m, _, get := startFakeWA(t, nil, configure, dev)
	return m, func() *fakeSession { return get(reachAccount) }
}

// TestReachoutCredsPresentSkipsGate 凭证齐备时门闸零动作：不拉 regular_high、
// 不向手机要历史，发送原样出去。这是「凭证在不在以读库为准」的反面钉死——
// 旧代码无论有没有凭证都会付一次快路同步。
func TestReachoutCredsPresentSkipsGate(t *testing.T) {
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.setReachout([]byte("salt-salt-salt-salt-salt-salt-32"), nil)
	})
	if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err != nil {
		t.Fatalf("凭证齐备时发送失败: %v", err)
	}
	f := get()
	rhSeq, histReqs := f.reachState()
	if len(rhSeq) != 0 || histReqs != 0 {
		t.Errorf("凭证齐备时门闸应当零动作: rhSeq=%v histReqs=%d", rhSeq, histReqs)
	}
}

// TestReachoutGateSingleFlightPerBatch 缺凭证时一批发信只向手机要一次历史：
// 门闸逐级补到「发请求」为止，窗口内后续消息不重发；regular_high 只付
// 快路+增量两级（全量留给 463 恢复档）。
func TestReachoutGateSingleFlightPerBatch(t *testing.T) {
	_, get := startReachWA(t, func(f *fakeSession) { f.loggedIn = true })
	for i := 0; i < 3; i++ {
		if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err != nil {
			t.Fatalf("第 %d 条发送失败: %v", i+1, err)
		}
	}
	rhSeq, histReqs := get().reachState()
	if histReqs != 1 {
		t.Errorf("三条发送应只向手机要一次历史: histReqs=%d", histReqs)
	}
	want := [][2]bool{{false, true}, {false, false}}
	if len(rhSeq) != len(want) {
		t.Fatalf("regular_high 调用序列应为快路+增量: got=%v", rhSeq)
	}
	for i := range want {
		if rhSeq[i] != want[i] {
			t.Errorf("regular_high 第 %d 次入参应为 %v: got=%v", i+1, want[i], rhSeq[i])
		}
	}
}

// TestReachoutGroupSendBypassesGate 群组发送不过门闸：库的群分支不挂任何 token，
// 463 只发生在单聊，群发送不该付凭证检查之外的任何代价。
func TestReachoutGroupSendBypassesGate(t *testing.T) {
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireCreds = true
	})
	if _, err := WhatsappSendGroupMessage(context.Background(), reachAccount, reachGroup, "hi"); err != nil {
		t.Fatalf("群发送失败: %v", err)
	}
	f := get()
	rhSeq, histReqs := f.reachState()
	if len(rhSeq) != 0 || histReqs != 0 {
		t.Errorf("群发送不应触发门闸: rhSeq=%v histReqs=%d", rhSeq, histReqs)
	}
}

// TestReachout463RecoversWhenTokenLands 463 恢复档的完整活闸：首条被拒后强制
// 增量+全量重拉 regular_high、轮询凭证；手机推送（这里由请求回调延时落地 token
// 仿真）在轮询窗口内到达 ⇒ 重试那一条真发出去，门面返回成功。
func TestReachout463RecoversWhenTokenLands(t *testing.T) {
	setReachoutClocks(t, 2*time.Second, 50*time.Millisecond)
	target := func() types.JID {
		j, err := waJID(reachFriend)
		if err != nil {
			t.Fatalf("收件人不可解析: %v", err)
		}
		return j
	}()
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireCreds = true
		f.onHistRequest = func() {
			time.AfterFunc(150*time.Millisecond, func() {
				f.setReachout(nil, map[types.JID][]byte{target: {1, 2, 3}})
			})
		}
	})
	if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err != nil {
		t.Fatalf("凭证在恢复档轮询窗口内落地后发送应成功: %v", err)
	}
	f := get()
	rhSeq, histReqs := f.reachState()
	if histReqs != 1 {
		t.Errorf("恢复档不应重复向手机要历史: histReqs=%d", histReqs)
	}
	// 快路 → 增量（门闸）→ 增量+全量（恢复档 force）。
	want := [][2]bool{{false, true}, {false, false}, {false, false}, {true, false}}
	if len(rhSeq) != len(want) {
		t.Fatalf("regular_high 序列应为 快路/增量/增量/全量: got=%v", rhSeq)
	}
	for i := range want {
		if rhSeq[i] != want[i] {
			t.Errorf("regular_high 第 %d 次入参应为 %v: got=%v", i+1, want[i], rhSeq[i])
		}
	}
	if _, _, _, sent, _ := f.snapshot(); len(sent) != 2 {
		t.Errorf("应为首条被拒+重试一条共两次发送尝试: got=%d", len(sent))
	}
}

// TestReachout463SaltPresentNoRetry 钉 :9/:10 真机形态：有盐（cstoken 已附上）仍被
// 463 拒 ⇒ 恢复档只等对方的 tctoken，等不到就**不重试**（重试同料只会再吃一个 463）。
// 盐在门闸阶段经历史请求回调落地 ⇒ 恢复档记 saltBefore=true，盐不算新凭证。
func TestReachout463SaltPresentNoRetry(t *testing.T) {
	setReachoutClocks(t, 2*time.Second, 50*time.Millisecond)
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireToken = true
		f.onHistRequest = func() {
			f.setReachout([]byte("salt-salt-salt-salt-salt-salt-32"), nil)
		}
	})
	_, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err == nil || !strings.Contains(err.Error(), "463") {
		t.Fatalf("有盐无 token 时仍应报 463: err=%v", err)
	}
	f := get()
	if _, _, _, sent, _ := f.snapshot(); len(sent) != 1 {
		t.Errorf("恢复档等不到对方 tctoken 就不该重试，只应发一次: sent=%d", len(sent))
	}
	if _, histReqs := f.reachState(); histReqs != 1 {
		t.Errorf("应已向手机要过一次历史: histReqs=%d", histReqs)
	}
}

// TestReachout463SaltLandingMidPollRetries 反向活闸：发送当时**没有盐**（首条是裸发），
// 轮询窗口内盐落了库 ⇒ cstoken 从无到有算新凭证，恢复档必须重试那一条。
func TestReachout463SaltLandingMidPollRetries(t *testing.T) {
	setReachoutClocks(t, 2*time.Second, 50*time.Millisecond)
	_, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireToken = true
		time.AfterFunc(100*time.Millisecond, func() {
			f.setReachout([]byte("salt-salt-salt-salt-salt-salt-32"), nil)
		})
	})
	if _, err := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi"); err == nil ||
		!strings.Contains(err.Error(), "463") {
		t.Fatalf("requireToken 形态下最终仍应 463: err=%v", err)
	}
	if _, _, _, sent, _ := get().snapshot(); len(sent) != 2 {
		t.Errorf("轮询期盐落地应触发一次重试（共两次发送尝试）: sent=%d", len(sent))
	}
}

// TestReachoutMissLedgerFailsFastAndRevives 落空账两态：等满一轮仍没凭证后，
// 窗口内第二条消息的恢复档不再重等（落空账时刻不变）；而 HistorySync 事件到达
// 作废落空账，第三条消息的恢复档重新等一轮（落空账时刻被推后）。
func TestReachoutMissLedgerFailsFastAndRevives(t *testing.T) {
	setReachoutClocks(t, 200*time.Millisecond, 50*time.Millisecond)
	m, get := startReachWA(t, func(f *fakeSession) {
		f.loggedIn = true
		f.requireCreds = true
	})

	_, err1 := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err1 == nil || !strings.Contains(err1.Error(), "463") {
		t.Fatalf("无凭证时首条应报 463: err=%v", err1)
	}
	w := mustWrapper(t, m, reachAccount)
	missAfterFirst := w.pollMissAt
	if missAfterFirst.IsZero() {
		t.Fatal("首条恢复档等满落空后应记落空账")
	}

	_, err2 := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err2 == nil || !strings.Contains(err2.Error(), "463") {
		t.Fatalf("落空账窗口内第二条仍应报 463: err=%v", err2)
	}
	if !w.pollMissAt.Equal(missAfterFirst) {
		t.Errorf("落空账窗口内且无新历史时不应重等: pollMissAt %v → %v", missAfterFirst, w.pollMissAt)
	}

	get().fire(&events.HistorySync{})
	_, err3 := WhatsappSendMessage(context.Background(), reachAccount, reachFriend, "hi")
	if err3 == nil || !strings.Contains(err3.Error(), "463") {
		t.Fatalf("无凭证时第三条仍应报 463: err=%v", err3)
	}
	if !w.pollMissAt.After(missAfterFirst) {
		t.Errorf("HistorySync 到达后落空账应作废并重等一轮: pollMissAt 仍为 %v", w.pollMissAt)
	}
	if _, histReqs := get().reachState(); histReqs != 1 {
		t.Errorf("三轮恢复档都在历史请求窗口内，不应重发请求: histReqs=%d", histReqs)
	}
}

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
		// SendRate 放宽：本用例要连发两条，缺省 1|1 会让第 2 条撞速率闸。
		SendRate: "1|1000",
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

// TestTakeSendSlotRollingWindow 滚动窗口：窗口内第 N+1 条拒，最早一条滑出后放行。
//
// 这条闸的窗口来自配置 `send_rate`（`秒|次数`），所以窗口长度必须真的被用上 ——
// 写死 1 小时的话，「300|3」会变成 1 小时内 3 条，节奏慢 12 倍。
func TestTakeSendSlotRollingWindow(t *testing.T) {
	w := &waClientWrapper{}
	base := time.Now()

	if !w.takeSendSlot(base, 5*time.Minute, 3) {
		t.Fatal("第 1 条应放行")
	}
	if !w.takeSendSlot(base.Add(1*time.Minute), 5*time.Minute, 3) {
		t.Fatal("第 2 条应放行")
	}
	if !w.takeSendSlot(base.Add(2*time.Minute), 5*time.Minute, 3) {
		t.Fatal("第 3 条应放行")
	}
	if w.takeSendSlot(base.Add(3*time.Minute), 5*time.Minute, 3) {
		t.Fatal("第 4 条应被拒（5 分钟内已满 3 条）")
	}
	// 第 1 条（base）在 base+5min 之后滑出窗口，此时窗口内只剩 2 条。
	if !w.takeSendSlot(base.Add(5*time.Minute+time.Second), 5*time.Minute, 3) {
		t.Fatal("最早一条滑出后应放行")
	}
}

// TestTakeSendSlotDefaultRate 缺省速率 60 秒 1 条：窗口内第二条拒，满 60 秒后放行。
func TestTakeSendSlotDefaultRate(t *testing.T) {
	w := &waClientWrapper{}
	base := time.Now()
	if !w.takeSendSlot(base, time.Minute, 1) {
		t.Fatal("第 1 条应放行")
	}
	if w.takeSendSlot(base.Add(59*time.Second), time.Minute, 1) {
		t.Fatal("同一分钟内第 2 条应被拒")
	}
	if !w.takeSendSlot(base.Add(61*time.Second), time.Minute, 1) {
		t.Fatal("下一分钟应放行")
	}
}

// TestTakeSendSlotUnlimited window/max 非正读作不限（调用方已用 Effective* 归一，
// 这里只钉住「不会把所有消息都拒掉」这个安全方向）。
func TestTakeSendSlotUnlimited(t *testing.T) {
	w := &waClientWrapper{}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !w.takeSendSlot(now, 0, 0) {
			t.Fatalf("window/max 非正时应不限，第 %d 条被拒", i+1)
		}
	}
}

// 单测默认关掉 send_rate 闸（Window/Max 都为 0 = 不限）。
//
// 【为什么需要】2026-11-02 起单条发送也进速率账（awaitSendSlot），缺省速率是
// 60 秒 1 条。既有用例里大量「同一账号连发两三条」的场景（发信后清空、防封闸、
// 自动登录、JID 归一）本意都不是测节奏 —— 留着闸会让它们每条白等一个窗口，
// 测试从秒级变成分钟级，而它们要断言的东西与节奏毫无关系。
//
// 节奏本身由 wa_sendslot_test.go 专测：那里显式注入速率（withSendRate），
// 断言的正是「第二条要等满窗口」。
//
// 用 override 而不是改配置：override 是业务侧真实的注入通道
// （whatsapp.SetSendRateOverride），测的就是它。
func TestMain(m *testing.M) {
	SetSendRateOverride(&config.WaSendRate{})
	code := m.Run()
	SetSendRateOverride(nil)
	os.Exit(code)
}

// TestSendSlotWaitDuration 等待时长必须精确落在「第 (len-max) 条过期」那一刻。
//
// 不写成「固定等一个 window」：配置调小时那样会白等，配置调大时又等不够
// （等不够就会立刻重试、再被拒，空转一轮）。这个下标计算是排队语义的心脏。
func TestSendSlotWaitDuration(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	window := 60 * time.Second
	now := t0.Add(25 * time.Second)
	// 窗口内三条历史记账（按时间升序，与真实追加顺序一致）。
	w := &waClientWrapper{sendWindow: []time.Time{t0, t0.Add(10 * time.Second), t0.Add(20 * time.Second)}}

	cases := []struct {
		name string
		max  int
		want time.Duration
	}{
		// max=1：要让窗口内只剩 0 条 ⇒ 等最后一条（idx=2）过期 ⇒ t0+20s+60s = t0+80s。
		{"配额 1 要等最后一条过期", 1, 55 * time.Second},
		// max=3：只剩 2 条即可 ⇒ 等第一条（idx=0）过期 ⇒ t0+60s。
		{"配额 3 等最早一条过期", 3, 35 * time.Second},
		// max 比窗口内条数还大：clamp 到 idx=0，不必等满。
		{"配额大于在途条数", 5, 35 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := w.sendSlotWaitDuration(now, window, c.max); got != c.want {
				t.Fatalf("✘ 等待时长 = %s，期望 %s", got, c.want)
			}
		})
	}

	// 空窗口不该算出任何等待（否则每次首条都要白睡）。
	empty := &waClientWrapper{}
	if got := empty.sendSlotWaitDuration(now, window, 1); got != 0 {
		t.Fatalf("✘ 空窗口等待 = %s，期望 0", got)
	}
}

// TestWaitSendSlotQueuesInsteadOfRejecting 窗口满时必须等，而不是返回失败。
//
// 这是本次改动的核心断言：先占满唯一配额，再要一个槽位 —— 旧实现立刻
// 返回「超出上限」，新实现睡到第一条过期后放行。同时验证记账被剪枝
// （等待后窗口里只剩新的那一条，不是两条）。
func TestWaitSendSlotQueuesInsteadOfRejecting(t *testing.T) {
	const window = 200 * time.Millisecond
	w := &waClientWrapper{}

	if !w.takeSendSlot(time.Now(), window, 1) {
		t.Fatal("首条应当放行")
	}
	// 紧接着要第二个槽位：窗口里已有 1 条、配额 1 ⇒ 必须等。
	start := time.Now()
	if err := w.waitSendSlot(context.Background(), window, 1); err != nil {
		t.Fatalf("✘ 等待应成功，实得 %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < window/2 {
		t.Fatalf("✘ 只等了 %s 就放行（窗口 %s）：配额还没让出来，这是超发", elapsed, window)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("✘ 等了 %s，远超窗口 %s", elapsed, window)
	}

	w.mu.Lock()
	n := len(w.sendWindow)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("✘ 等待后窗口内应只剩新记的 1 条（旧的已过期剪掉），实得 %d 条", n)
	}
}

// TestWaitSendSlotHonorsCtx ctx 被打断时必须立刻返回错误，不能把协程钉死。
//
// 窗口上限是 86400 秒（24 小时）。不认 ctx 的话，一个配错的大窗口会让批次
// 协程睡一整天，而账号串行闸还被它占着 —— 那个号整天发不出任何东西。
func TestWaitSendSlotHonorsCtx(t *testing.T) {
	w := &waClientWrapper{}
	if !w.takeSendSlot(time.Now(), time.Hour, 1) {
		t.Fatal("首条应当放行")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := w.waitSendSlot(ctx, time.Hour, 1)
	if err == nil {
		t.Fatal("✘ ctx 已取消，等待应返回错误")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("✘ ctx 已取消却等了 %s：等待没有认 ctx", d)
	}
}

// TestWaitSendSlotUnlimited 两种「不限」写法都必须直接放行、不记账。
//
// window<=0 / max<=0 是 Effective* 归一后的「关闭限速」形态（例如后台把条数
// 配成 0 的极端情形）。这种情况下若还去等，会把一个明确关掉的闸变成永久阻塞。
func TestWaitSendSlotUnlimited(t *testing.T) {
	for _, c := range []struct {
		name   string
		window time.Duration
		max    int
	}{
		{"窗口为 0", 0, 1},
		{"条数为 0", time.Minute, 0},
		{"两者都为 0", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := &waClientWrapper{}
			start := time.Now()
			if err := w.waitSendSlot(context.Background(), c.window, c.max); err != nil {
				t.Fatalf("✘ 不限形态不该返回错误：%v", err)
			}
			if d := time.Since(start); d > 500*time.Millisecond {
				t.Fatalf("✘ 不限形态不该等待，实等 %s", d)
			}
			w.mu.Lock()
			n := len(w.sendWindow)
			w.mu.Unlock()
			if n != 0 {
				t.Fatalf("✘ 不限形态不该记账，窗口内实得 %d 条", n)
			}
		})
	}
}

// TestSendRateOverrideWinsOverConfig 注入的速率必须压过配置文件的 send_rate。
//
// 这是「后台设置优先」在核心侧的唯一落点：业务侧解析出最终值后注入，核心只认它。
// 不生效的后果是最难查的那种 —— 业务侧按后台值分批、核心按文件值放行，
// 表现是「后台明明调了、实际节奏没变」，且不报任何错。
func TestSendRateOverrideWinsOverConfig(t *testing.T) {
	w := &waClientWrapper{cfg: &config.WhatsappSessionsConfig{SendRate: "60|1"}}
	// 本用例必须从「没有注入」开始，而 TestMain 给整个包注入了「不限」——
	// 先清掉再还原成不限，否则第一条断言读到的就是 TestMain 那份。
	SetSendRateOverride(nil)
	t.Cleanup(func() { SetSendRateOverride(&config.WaSendRate{}) })

	// 未注入：按配置文件。
	if got := w.sendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Fatalf("✘ 未注入时应读配置文件，实得 window=%s max=%d", got.Window, got.Max)
	}

	// 注入后台值：必须压过文件。
	injected := config.WaSendRate{Window: 5 * time.Minute, Max: 3}
	SetSendRateOverride(&injected)
	if got := w.sendRate(); got.Window != 5*time.Minute || got.Max != 3 {
		t.Fatalf("✘ 注入后应读后台值，实得 window=%s max=%d", got.Window, got.Max)
	}

	// 清除注入：回落到配置文件。
	SetSendRateOverride(nil)
	if got := w.sendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Fatalf("✘ 清除后应回落配置文件，实得 window=%s max=%d", got.Window, got.Max)
	}
}

// TestSendRateOverrideIsACopy 注入必须按值拷贝。
//
// 业务侧注入后立刻复用同一个结构体变量（waSendRate 里就是 &r），若这里存指针，
// 调用方后续改 r 会悄悄改掉全局速率 —— 那种 bug 只有跑起来才会以「节奏莫名变了」出现。
func TestSendRateOverrideIsACopy(t *testing.T) {
	// 还原成 TestMain 的「不限」，不是 nil：nil 会回落到缺省 60|1，
	// 把闸留给后面那些「连发两条」的用例，让它们莫名等满一分钟。
	t.Cleanup(func() { SetSendRateOverride(&config.WaSendRate{}) })
	w := &waClientWrapper{cfg: &config.WhatsappSessionsConfig{SendRate: "60|1"}}

	src := config.WaSendRate{Window: 2 * time.Minute, Max: 7}
	SetSendRateOverride(&src)
	src.Window = 9 * time.Minute // 调用方复用变量
	src.Max = 99

	if got := w.sendRate(); got.Window != 2*time.Minute || got.Max != 7 {
		t.Fatalf("✘ 注入后被调用方改动污染：window=%s max=%d", got.Window, got.Max)
	}
}

// withSendRate 临时注入速率，用完还原成 TestMain 的口径（不限）。
//
// 还原成 nil 会让后续用例回落到缺省 60|1，等于把闸偷偷打开 —— 那时下一条用例
// 若是「连发两条」，就会莫名等满 60 秒。
func withSendRate(t *testing.T, window time.Duration, max int) {
	t.Helper()
	SetSendRateOverride(&config.WaSendRate{Window: window, Max: max})
	t.Cleanup(func() { SetSendRateOverride(&config.WaSendRate{}) })
}

// TestSendSlotWaitProbe 探测「还要等多久」必须与真实占槽位同源、且不记账。
//
// 这条探针是业务侧「点发送按钮后要不要等」的唯一依据（t1job 的 AdmWaSend）。
// 它必须满足三件事：没发过时报 0；刚发过时报「还要等一个窗口」；记账过期后
// 自动回到 0（不剪窗口就会永远报「还要等」，按钮永久变灰）。
func TestSendSlotWaitProbe(t *testing.T) {
	window := 90 * time.Second
	withSendRate(t, window, 1)

	w := &waClientWrapper{}
	if d, rate := w.sendSlotWait(); d != 0 || rate.Window != window {
		t.Fatalf("✘ 空窗口应立刻可发并回带窗口 %s，实得 wait=%s window=%s", window, d, rate.Window)
	}
	if !w.takeSendSlot(time.Now(), window, 1) {
		t.Fatal("✘ 首条应当放行")
	}
	d, _ := w.sendSlotWait()
	if d <= 0 || d > window {
		t.Fatalf("✘ 刚记账后应报「还要等 ~%s」，实得 %s", window, d)
	}

	// 记账已滑出窗口：探测必须回到 0，否则后台的发送按钮会永久拒绝。
	stale := &waClientWrapper{sendWindow: []time.Time{time.Now().Add(-2 * window)}}
	if d, _ := stale.sendSlotWait(); d != 0 {
		t.Fatalf("✘ 窗口外的旧记账应被剪掉，实得等待 %s", d)
	}
	// 探测本身不得**追加**记账（否则每点一次按钮都会往后推一个窗口）。
	// 剪掉窗口外的旧条目是允许的：那是记账本自身的清理，不改变「谁占着槽位」。
	fresh := &waClientWrapper{sendWindow: []time.Time{time.Now()}}
	if d, _ := fresh.sendSlotWait(); d <= 0 {
		t.Fatalf("✘ 窗口内已有一条时应报「还要等」，实得 %s", d)
	}
	fresh.mu.Lock()
	n := len(fresh.sendWindow)
	fresh.mu.Unlock()
	if n != 1 {
		t.Fatalf("✘ 探测不该追加记账，实得 %d 条", n)
	}
}

// TestAwaitSendSlotRecordsSingleSend 单条发送必须进同一本账，第二条要等窗口。
//
// 这是 2026-11-02 修的那条缝：WhatsappSendMessage 不过闸，于是自动发刚发完
// 3 秒、手动点一次还能再发一条 —— 90|1 对「点按钮」完全无效。
func TestAwaitSendSlotRecordsSingleSend(t *testing.T) {
	window := 200 * time.Millisecond
	withSendRate(t, window, 1)

	w := &waClientWrapper{}
	if err := w.awaitSendSlot(context.Background()); err != nil {
		t.Fatalf("✘ 首条不该等：%v", err)
	}
	w.mu.Lock()
	n := len(w.sendWindow)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("✘ 单条发送必须记账，窗口内实得 %d 条", n)
	}

	start := time.Now()
	if err := w.awaitSendSlot(context.Background()); err != nil {
		t.Fatalf("✘ 等窗口后应放行：%v", err)
	}
	if d := time.Since(start); d < window {
		t.Fatalf("✘ 第二条必须等满窗口 %s，实等 %s", window, d)
	}
}

// TestBatchPacingComesOnlyFromSendRate 速率闸有效时，批内不再叠 send_min/max_delay。
//
// 2026-11-02 收敛：关掉防封闸后，出网节奏的唯一来源就是 send_rate。旧写法在批内
// 无条件睡 1~5 秒（且睡在 waitSendSlot 之前），于是 90|1 实际是 91~95 秒一条，
// 慢多少还取决于这条在批内的位置 —— 配置写着 90、日志里是 93，没人说得清为什么。
//
// 断言两侧：① 一次都不睡（节奏只由排队决定）；② 第二条确实等满了窗口
// （证明「不睡」不是把闸也一起让开了）。
func TestBatchPacingComesOnlyFromSendRate(t *testing.T) {
	window := 150 * time.Millisecond
	withSendRate(t, window, 1)

	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	// 配置里留着 1~5 秒的批内间隔：它只有在速率闸「不限」时才该生效。
	cfg := &config.WhatsappSessionsConfig{
		AntibanEnabled:  waBool(false),
		SendMinDelaySec: 1,
		SendMaxDelaySec: 5,
	}
	m, _, _ := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}

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

	start := time.Now()
	if err := WhatsappSendBatchAsync(context.Background(), "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "a"},
		{To: "8613910000000", Body: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, "两条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	})
	elapsed := time.Since(start)

	mu.Lock()
	got := append([]time.Duration(nil), sleeps...)
	mu.Unlock()
	if len(got) != 0 {
		t.Errorf("✘ 速率闸有效时批内不该自己睡（会叠在排队等待之上）：%v", got)
	}
	if elapsed < window {
		t.Errorf("✘ 第二条必须等满窗口 %s，实等 %s", window, elapsed)
	}
}

func TestSyncSelfTimeoutIsNotBlamedOnCaller(t *testing.T) {
	dev := fakeDevice(t, "447378152319:21@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncGate = make(chan struct{}) }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	s := get(dev.ID.String())
	// 前置非空跑：账号必须真连着，否则测的就不是同步阶段而是登录阶段。
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	old := waContactSyncTimeout
	waContactSyncTimeout = 60 * time.Millisecond
	t.Cleanup(func() { waContactSyncTimeout = old })

	n, err := WhatsappSyncContacts(context.Background(), dev.ID.String(), false)
	if n != 0 || err == nil {
		t.Fatalf("本包同步预算到点必须报错且不给条数: n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "同步预算") {
		t.Errorf("错误文案要点名是本包的同步预算到点: %v", err)
	}
	if strings.Contains(err.Error(), "上下文已结束") {
		t.Errorf("内部同步超时不得报成调用方断连: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("必须保留 %%w 让下游仍可判定超时: %v", err)
	}
	if c, _, _ := s.countSyncs(); c != 1 {
		t.Errorf("同步应恰好发起一次，实得 %d", c)
	}
}

func TestSyncCallerCancelStillBlamedOnCaller(t *testing.T) {
	dev := fakeDevice(t, "447378152319:22@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncGate = make(chan struct{}) }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	// 同步门闩永不打开：调用方先结束，本包预算（缺省 20s）远没到点。
	n, err := WhatsappSyncContacts(ctx, dev.ID.String(), false)
	if n != 0 || err == nil {
		t.Fatalf("调用方取消必须报错: n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "上下文已结束") {
		t.Errorf("调用方断连仍要如实报成调用方断连: %v", err)
	}
	if strings.Contains(err.Error(), "同步预算") {
		t.Errorf("这条不是本包超时，不许出现同步预算文案: %v", err)
	}
}

// TestStExplicitFullConcurrentPaysOneFullSync 是为 MD 变异补的闸（Lead 合流批）。
//
// 为什么必须单独有这条：T10/T11 的「并发只拉一次」用例其实测不到 `s.mu`——那条走的是
// 快路，压住重复拉的是「拿锁后双检本地」，把锁换成 `TryLock()` 它照样绿（MD 首打 SURVIVED
// 就是这么暴露的）。真正只有锁能保护的是 **full=true 的账**：`lastFullAt` 由第一个真全量
// 写下，后进者必须在锁内看到它并退化成快路；没有锁，8 个并发各自都看到零值，
// 于是 8 次 `DeleteAppStateVersion` 互踩版本、8 份全量快照重复付。
func TestStExplicitFullConcurrentPaysOneFullSync(t *testing.T) {
	dev := fakeDevice(t, "447378152319:23@s.whatsapp.net", "android", "P")
	jid := types.JID{User: "8613900000023", Server: types.DefaultUserServer}
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: {FirstName: "st"}}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	waitUntil(t, 2*time.Second, "首次连上", get(dev.ID.String()).LoggedIn)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil {
				t.Errorf("并发显式全量同步不该报错: %v", err)
			}
		}()
	}
	wg.Wait()

	_, full, _ := get(dev.ID.String()).countSyncs()
	if full != 1 {
		t.Errorf("8 个并发显式 full=true 必须被单飞压成 1 次全量（其余退快路），实得 %d 次", full)
	}
}

// ghostSessions 是一个形状合法但必然连不上的会话库 DSN（127.0.0.1:1 直接拒连）。
// 带上它是为了复现「配置齐备但没开闸」这一现网形态：闸失效时 startWA 会一路走到
// 开会话库那一步（随后因连不上而放弃），本用例钉的是这一步绝不能留下装配产物。
func ghostSessions() *config.WhatsappSessionsConfig {
	return &config.WhatsappSessionsConfig{DSN: "postgres://u:p@127.0.0.1:1/ghost?sslmode=disable"}
}

// TestChannelEnableGateBlocksStart whatsapp.enable 不为 on 时消息侧一律不装配：
// 不开会话库、不建管理器（因此绝不会去连 WhatsApp 服务器），门面全部 fail-closed。
// enable 缺失与显式 off 必须同一个结果——「忘了开」不该表现成「连上了但发不出去」。
//
// 口径变更（资金链路删除时留下的口子，写明白）：原来这圈用例的正向对照是
// 「enable 为 on 时 fund 非空」，那是唯一能证明「闸确实放行过」的可观测产物。
// 消息侧要装配成功必须连得上真 Postgres，测试里造不出来，所以正向对照退到
// 闸的判据本身（on 的大小写/空白形态），负向那半退到「没有装配产物 + 门面拒」。
func TestChannelEnableGateBlocksStart(t *testing.T) {
	off := []struct {
		name string
		wa   *config.WhatsappConfig
	}{
		{"没有 whatsapp 段", nil},
		{"enable 键没写", &config.WhatsappConfig{Sessions: ghostSessions()}},
		{"enable 为空串", &config.WhatsappConfig{Enable: "  ", Sessions: ghostSessions()}},
		{"enable 为 off", &config.WhatsappConfig{Enable: "off", Sessions: ghostSessions()}},
		{"enable 写了非 on 的值", &config.WhatsappConfig{Enable: "true", Sessions: ghostSessions()}},
	}
	for _, cs := range off {
		WhatsappStop(nil)
		WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{Whatsapp: cs.wa}))
		if m := waMgr.Load(); m != nil {
			t.Errorf("%s：消息侧管理器却装配了（enable 闸失效）", cs.name)
		}
		if s := waStore.Load(); s != nil {
			t.Errorf("%s：会话库却打开了（enable 闸失效）", cs.name)
		}
		if got := WhatsappListClients(); len(got) != 0 {
			t.Errorf("%s：未启动却有 %d 个在跑会话", cs.name, len(got))
		}
		if err := WhatsappStartClient("8613800000000@s.whatsapp.net"); err == nil {
			t.Errorf("%s：未启动时启动账号应当被拒", cs.name)
		}
		if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err == nil {
			t.Errorf("%s：未启动时发信应当被拒", cs.name)
		}
	}
	WhatsappStop(nil)

	for _, on := range []string{"on", "ON", "On", " on "} {
		wa := &config.WhatsappConfig{Enable: on}
		if !wa.Enabled() {
			t.Errorf("enable=%q 应当放行消息侧装配", on)
		}
		// 闸放行但 sessions 未配：消息侧仍按「没配 dsn 即不装配」独立判定，安静返回不 panic。
		WhatsappStop(nil)
		WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{Whatsapp: wa}))
		if waMgr.Load() != nil {
			t.Errorf("enable=%q 但 sessions 未配，消息侧不该装配", on)
		}
	}
	WhatsappStop(nil)
}

// TestPayoutInterfaceStaysWired 三个资金门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = WhatsappWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = WhatsappQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = WhatsappAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := WhatsappWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := WhatsappQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := WhatsappAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单与查询参数为 nil/空也一样先拒：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := WhatsappWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
	if _, err := WhatsappQuery(nil, ""); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空单号的拒绝口径不符: %v", err)
	}
}

// TestPayoutNotWiredDoesNotBroadcast 未接入的门面不得广播任何回执。
//
// 广播键（CallWhatsappMsg+Withdraw/Query）在下游挂着落库回调，
// 误广播一次就给账本记一笔不存在的资金动作。
func TestPayoutNotWiredDoesNotBroadcast(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []*pay.PayResult
		kind []string
	)
	for _, k := range []string{"Withdraw", "Query"} {
		id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+k, func(res *pay.PayResult) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, res)
			kind = append(kind, k)
		})
		defer util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+k, id)
	}
	o := &pay.PayOrder{OrderNo: "O9", Amount: 1}
	_, _ = WhatsappWithdraw(nil, o)
	_, _ = WhatsappQuery(nil, "O9")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("未接入的链路广播了回执: kinds=%v %+v", kind, got)
	}
}

// qrTestEnv 是一套端到端测试现场：假管理器 + 假配对会话 + httptest 服务。
type qrTestEnv struct {
	mgr     *waManager
	fake    *fakePairSession
	session *QRSession
	url     string // ws://.../whatsapp/qr/<sid>
	srv     *httptest.Server
}

// startQREnv 装配现场并把运行配置快照（web.allow_origins 从这里读）写进门面。
func startQREnv(t *testing.T, allowOrigins []string) *qrTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	st := newFakeStore()
	tr := installFakePairFactory(t, st, nil)
	m := newTestManager(t, st, nil)
	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}

	waMgr.Store(m)
	setRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Web: &config.WebConfig{AllowOrigins: allowOrigins},
	}))
	t.Cleanup(func() {
		waMgr.Store(nil)
		setRunCtx(context.Background())
	})

	engine := gin.New()
	r := &waQRRouter{}
	engine.GET(waQRWSPath, r.Stream)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	return &qrTestEnv{
		mgr:     m,
		fake:    tr.last(),
		session: sess,
		url:     "ws" + strings.TrimPrefix(srv.URL, "http") + "/whatsapp/qr/" + sess.ID,
		srv:     srv,
	}
}

// dial 带指定 Origin 头拨号；origin 为空则不带头。
func (e *qrTestEnv) dial(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	var hdr http.Header
	if origin != "" {
		hdr = http.Header{"Origin": []string{origin}}
	}
	return websocket.DefaultDialer.Dial(e.url, hdr)
}

// TestQRWSAllowedOriginReceivesCodes 白名单内的 Origin：握手成功，二维码与终局事件
// 逐帧送达，会话结束后服务端按 1000 正常关闭。
func TestQRWSAllowedOriginReceivesCodes(t *testing.T) {
	env := startQREnv(t, []string{"https://ok.example"})
	conn, _, err := env.dial(t, "https://ok.example")
	if err != nil {
		t.Fatalf("白名单 Origin 握手失败: %v", err)
	}
	defer conn.Close()
	waitUntil(t, 2*time.Second, "服务端订阅就绪", func() bool {
		env.session.mu.Lock()
		defer env.session.mu.Unlock()
		return len(env.session.subs) > 0
	})

	env.fake.emit(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "QR-WS-1", Timeout: 20 * time.Second,
	}, false)
	var ev QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("读取二维码帧失败: %v", err)
	}
	if ev.Event != "code" || ev.Code != "QR-WS-1" || ev.SessionID != env.session.ID {
		t.Fatalf("帧内容不符: %+v", ev)
	}

	// 终局：success 之后泵协程关订阅，服务端应当写 close(1000)。
	env.fake.pairSuccess("8613800000009@s.whatsapp.net")
	var last QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&last); err != nil {
		t.Fatalf("读取成功帧失败: %v", err)
	}
	if last.Event != "success" || last.JID != "8613800000009@s.whatsapp.net" {
		t.Fatalf("成功帧不符: %+v", last)
	}
	_, _, err = conn.ReadMessage()
	var ce *websocket.CloseError
	if err == nil || !asCloseError(err, &ce) || ce.Code != websocket.CloseNormalClosure {
		t.Fatalf("终局后应当收到 1000 正常关闭，实得 err=%v", err)
	}
}

// asCloseError 是 errors.As 的窄包装（本文件只有这一处用）。
func asCloseError(err error, target **websocket.CloseError) bool {
	ce, ok := err.(*websocket.CloseError)
	if ok {
		*target = ce
	}
	return ok
}

// TestQRWSOriginPolicy 跨域白名单的三档裁决，三条原用例并成一张表（同一条
// CheckOrigin 分支，只差「白名单 + 握手 Origin」两个输入）：
//   - 白名单外拒 403：CheckOrigin 写 return true 会让任何网页连进来偷二维码，
//     而二维码等于配对凭据，谁扫上谁就是账号的联合设备；
//   - 不带 Origin 放行：原生客户端/服务间调用不发 Origin，浏览器跨站握手必带，
//     与 websocket 包 originPolicy 的口径一致；
//   - 白名单显式 "*" 放行任意 Origin：配置口径与 web.allow_origins 一致。
func TestQRWSOriginPolicy(t *testing.T) {
	cases := []struct {
		name   string
		allow  []string
		origin string
		wantOK bool
	}{
		{"白名单外拒绝", []string{"https://ok.example"}, "https://evil.example", false},
		{"不带 Origin 放行", nil, "", true},
		{"通配放行任意 Origin", []string{"*"}, "https://anywhere.example", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := startQREnv(t, c.allow)
			conn, resp, err := env.dial(t, c.origin)
			if c.wantOK {
				if err != nil {
					t.Fatalf("Origin %q 应当放行: %v", c.origin, err)
				}
				conn.Close()
				return
			}
			if err == nil {
				conn.Close()
				t.Fatal("白名单外 Origin 竟然握手成功")
			}
			if resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("拒绝应当是 403，实得 resp=%v err=%v", resp, err)
			}
		})
	}
}

// TestQRWSUnknownSession404 查无会话按 404 JSON 拒绝，且响应体干净
// （不被框架 sendResponse 的 "success" 追加污染——hijackSafeWriter 的行为断言）。
func TestQRWSUnknownSession404(t *testing.T) {
	env := startQREnv(t, nil)
	resp, err := http.Get(env.srv.URL + "/whatsapp/qr/qr-does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("查无会话应当 404，实得 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("404 响应不是合法 JSON（可能被 sendResponse 追加污染）: %v", err)
	}
	if body["error"] != "session not found" {
		t.Fatalf("404 响应体不符: %v", body)
	}
}

// TestQRWSManagerDown503 消息侧未启动时端点报 503 而不是 panic。
func TestQRWSManagerDown503(t *testing.T) {
	env := startQREnv(t, nil)
	waMgr.Store(nil) // 模拟未启动；t.Cleanup 会再置一次 nil，无副作用
	resp, err := http.Get(env.srv.URL + "/whatsapp/qr/" + env.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未启动应当 503，实得 %d", resp.StatusCode)
	}
}
