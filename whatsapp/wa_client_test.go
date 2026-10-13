package whatsapp

// 消息侧门面的测试：fail-closed 口径、发信契约（空 id ⇔ 非 nil error）、JID 归一、
// 删会话 patch 的逐字断言、事件翻译（值/指针双形态）。全部走 seam，不联网。

import (
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
)

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

// TestFacadesFailClosedWhenNotStarted 未启动时五条门面全部就地拒绝、列表为空，
// 且**不得广播任何回调**——「没启动却发了事件」意味着有代码绕过了快照判空。
func TestFacadesFailClosedWhenNotStarted(t *testing.T) {
	WhatsappStop(nil)
	seen := captureKinds(t, "Inbound", "Receipt", "Connection", "QR")

	if id, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err == nil || id != "" {
		t.Errorf("未启动时发送应当被拒: id=%q err=%v", id, err)
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

// TestDeleteChatPatchVerbatim 删会话 patch 的逐字断言。
//
// 与派发简报的假设有一处出入（以库源码为准）：PatchInfo.Type 不是 IndexDeleteChat，
// 而是 WAPatchRegularHigh——"deleteChat" 出现在 Mutations[0].Index[0]
// （查自 WM/appstate/encode.go:340-357、keys.go:121）。断言按真实结构写。
func TestDeleteChatPatchVerbatim(t *testing.T) {
	chat := jidOf(t, "8613900000000@s.whatsapp.net")
	ts := time.Unix(1700000000, 0)

	patch := waDeleteChatPatch(chat, " MSG-9 ", ts, true)
	if patch.Type != appstate.WAPatchRegularHigh {
		t.Fatalf("patch.Type=%s，期望 regular_high", patch.Type)
	}
	if len(patch.Mutations) != 1 {
		t.Fatalf("变异条数=%d，期望 1", len(patch.Mutations))
	}
	mut := patch.Mutations[0]
	if len(mut.Index) != 3 || mut.Index[0] != appstate.IndexDeleteChat ||
		mut.Index[1] != chat.String() || mut.Index[2] != "1" {
		t.Fatalf("Index=%v，期望 [deleteChat %s 1]", mut.Index, chat.String())
	}
	mr := mut.Value.GetDeleteChatAction().GetMessageRange()
	if mr == nil {
		t.Fatal("缺 MessageRange")
	}
	if mr.GetLastMessageTimestamp() != ts.Unix() {
		t.Fatalf("LastMessageTimestamp=%d，期望 %d", mr.GetLastMessageTimestamp(), ts.Unix())
	}
	msgs := mr.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("消息键条数=%d，期望 1", len(msgs))
	}
	key := msgs[0].GetKey()
	if key.GetRemoteJID() != chat.String() || !key.GetFromMe() || key.GetID() != "MSG-9" {
		t.Fatalf("MessageKey 不符: remote=%s fromMe=%v id=%s", key.GetRemoteJID(), key.GetFromMe(), key.GetID())
	}

	// lastMsgID 为空 → 不构造消息键（删除范围只按时间戳划）；deleteMedia=false → Index[2]=="0"。
	patch2 := waDeleteChatPatch(chat, "  ", ts, false)
	mut2 := patch2.Mutations[0]
	if mut2.Index[2] != "0" {
		t.Fatalf("deleteMedia=false 时 Index[2]=%s", mut2.Index[2])
	}
	if got := mut2.Value.GetDeleteChatAction().GetMessageRange().GetMessages(); len(got) != 0 {
		t.Fatalf("lastMsgID 为空时应当不带消息键，实得 %v", got)
	}
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
		t.Fatalf("删除会话失败: %v", err)
	}
	_, _, _, _, patches := s.snapshot()
	if len(patches) != 1 || patches[0].Mutations[0].Index[0] != appstate.IndexDeleteChat {
		t.Fatalf("送进会话的 patch 不符: %+v", patches)
	}

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
