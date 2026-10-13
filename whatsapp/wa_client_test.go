package whatsapp

// 消息侧门面的测试：fail-closed 口径、发信契约（空 id ⇔ 非 nil error）、JID 归一、
// 删会话 patch 的逐字断言、事件翻译（值/指针双形态）。全部走 seam，不联网。

import (
	"context"
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
	s.contacts = map[types.JID]types.ContactInfo{jidOf(t, "8613900000000@s.whatsapp.net"): {PushName: "F"}}
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

// TestListContactsFacade 好友列表门面：map 摊平成按 JID 排序的切片、字段逐一对应；
// 底层报错带通道前缀上抛；未启动账号在触达会话之前被拒。
func TestListContactsFacade(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	self := jidOf(t, "8613800000000@s.whatsapp.net")
	friend := jidOf(t, "8613900000000@s.whatsapp.net")
	s.mu.Lock()
	s.contacts = map[types.JID]types.ContactInfo{
		friend: {FirstName: "Fern", FullName: "Fernando", PushName: "Fer", BusinessName: "FerShop"},
		self:   {PushName: "Me"},
	}
	s.mu.Unlock()

	got, err := WhatsappListContacts(nil, "8613800000000")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].JID != self.String() || got[1].JID != friend.String() {
		t.Fatalf("列表应当按 JID 排序且两条: %+v", got)
	}
	if got[1].FirstName != "Fern" || got[1].FullName != "Fernando" ||
		got[1].PushName != "Fer" || got[1].BusinessName != "FerShop" {
		t.Fatalf("联系人字段映射不符: %+v", got[1])
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
// 第一条不等待、其后每条前睡一次注入的间隔；小时闸超限的那条被拒且不触达会话，
// 整批不中止；空正文/坏目标逐条带错误广播且不占闸额度。
func TestSendBatchAsync(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	cfg := &config.WhatsappSessionsConfig{SendMaxPerHour: 2, AntibanEnabled: waBool(false)}
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
		{To: "8613910000000", Body: "m3"}, // 第 3 条撞小时闸（max=2）
		{To: "8613920000000", Body: ""},   // 空正文：拒绝且不占闸额度
		{To: "abc", Body: "m5"},           // 拼不出号码
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
	wantErr := []string{"", "", "每小时", "消息内容为空", "没有任何数字"}
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
	if len(sent) != 2 || sent[0].to != "8613900000000@s.whatsapp.net" || sent[1].to != "12345@g.us" {
		t.Errorf("只有前两条该触达会话: %+v", sent)
	}

	// 已取消的 ctx：每条都广播「发送已取消」而不是静默蒸发，且零发送。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	mu.Lock()
	results = nil
	mu.Unlock()
	if err := WhatsappSendBatchAsync(canceled, "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "取消的 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 1
	})
	if !strings.Contains(results[0].Err, "发送已取消") {
		t.Errorf("取消形态 Err=%q", results[0].Err)
	}

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
