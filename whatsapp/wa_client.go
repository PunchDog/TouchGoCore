package whatsapp

// 本文件是 whatsmeow 消息侧的门面：发一条文本消息、删掉代持账号客户端里那个会话框、
// 多账号 Client 的启停与列表、QR 配对的发起，以及把入站事件摊平成回调广播给业务方。
// 常驻协程与会话管理在 wa_manager.go，配对泵在 wa_pairing.go，存储在 store_postgres.go。
//
// 三条口径先写在这里：
//
//   - **合规与封号**：whatsmeow 是非官方客户端，用它收发等于以编程方式驱动一个真实
//     WhatsApp 账号，违反 WhatsApp 服务条款、账号可被封；多账号同时在线被检测的概率
//     更高。框架不为此担保，责任与风控归业务方。
//   - **密钥保管**：Device 里的 NoiseKey/IdentityKey/SignedPreKey/AdvSecretKey 就是
//     账号本体凭据（等价于把那只手机交出去），由 sqlstore 落在本仓 Postgres 会话库里。
//     本包任何日志、error 文案、SessionInfo、广播载荷都不携带这些材料。
//   - **提现不在这里**：库里支付消息只是 unsupported 占位（WM/armadillomessage.go:95-96，
//     无任何可发起转账的 API），资金动作一律留在 pay_sdks 供应商链路（fund.go）。
//
// 并发口径：同一 Client 可并发发信（whatsmeow 底层有请求表锁与 socket 锁，实践可行，
// 但库对多 goroutine 安全零表述，不是契约）；WhatsappStartClient / WhatsappStopClient /
// WhatsappStartPairing 属改状态动作，调用方必须串行化（管理器只兜「不 panic、不双启」）。
//
// 边界口径与资金侧一致：本包不持有按手机号/会话的业务状态。删会话要的 lastMessage
// 由调用方传入，消息落库、幂等去重、冷却与预算都归业务方。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// waServerUser 是普通用户会话的 JID 服务器名。收件人只给号码时按这个域拼 JID；
// 群聊（g.us）、频道等由调用方直接传完整 "user@server" 形态的 JID 串。
const waServerUser = "s.whatsapp.net"

// InboundMessage 是一条入站消息摊平后的样子，经 CallWhatsappMsg+"Inbound" 广播。
//
// Account 是「哪个已配对账号收到的」（多账号形态下订阅方靠它分流）。
// Type 告诉业务方这是什么形态（text/extended_text/image/...），Body 只在文本与带
// caption 的媒体形态里有内容；纯媒体载荷不在这里下载（媒体落盘归业务方，用 MsgID 回查）。
type InboundMessage struct {
	Account   string
	Chat      string
	Sender    string
	MsgID     string
	Timestamp time.Time
	Type      string
	Body      string
	IsFromMe  bool
	IsGroup   bool
}

// DeliveryReceipt 是一条投递回执，经 CallWhatsappMsg+"Receipt" 广播。
//
// MsgIDs 可能是多个（一次已读回执覆盖一批），Status 取库里的 types.ReceiptType 原值。
type DeliveryReceipt struct {
	Account   string
	Chat      string
	Sender    string
	MsgIDs    []string
	Status    string
	Timestamp time.Time
}

// ConnEvent 是连接状态跃迁，经 CallWhatsappMsg+"Connection" 广播。
// State 取值 connected / disconnected / logged_out / stream_replaced；
// 断线由该账号的常驻协程自动重连，logged_out 则不再重连（会话已被吊销，要重新扫码配对）。
type ConnEvent struct {
	Account string
	State   string
	Reason  string
}

// waSession 是本包对 whatsmeow 客户端的唯一期望面（未导出 seam）。
//
// 存在的理由有两个：一是让本包的测试不联网也能跑完门面与事件翻译；二是把「库未声明
// 多 goroutine 安全」这件事收在一个点上——真实现里每个方法都只是转发给同一个
// *whatsmeow.Client，底层有请求表锁与 socket 锁（WM/request.go、client.go），
// 发信并发实践可行但不是契约。
type waSession interface {
	// Login 建立连接并完成握手；已连接时按库的语义应就地返回。
	Login(ctx context.Context) error
	LoggedIn() bool
	Disconnect()
	Logout(ctx context.Context) error
	// SendText 发一条纯文本，返回消息 id 与服务器时间戳。
	SendText(ctx context.Context, to string, body string) (string, time.Time, error)
	// SendAppState 发一次会话级 app state 变异（当前唯一用途是删会话）。
	SendAppState(ctx context.Context, patch appstate.PatchInfo) error
	// OnEvent 注册事件回调；只应在连接之前调用一次。
	OnEvent(handler func(any))
}

// waSessionFactory 是「怎么从已配对 Device 造一个会话」的挂点。
// 默认真实现；测试整体替换，用完还原。
var waSessionFactory = newWaConn

// waConn 是 waSession 的真实现，包住 *whatsmeow.Client。
type waConn struct {
	cli *whatsmeow.Client
}

// newWaConn 装配真客户端。
//
// 库默认开着 EnableAutoReconnect（查自 WM/client.go:289），这里关掉：重连归本包的
// 常驻协程管，理由有两条——停机能确定（两套同时在抢，Disconnect 之后可能又被库自己
// 连回去，1.5 秒停机预算就没了意义），以及退避节奏可被测试注入。
func newWaConn(dev *store.Device, log waLog.Logger) waSession {
	cli := whatsmeow.NewClient(dev, log)
	cli.EnableAutoReconnect = false
	return &waConn{cli: cli}
}

func (c *waConn) Login(ctx context.Context) error  { return c.cli.ConnectContext(ctx) }
func (c *waConn) LoggedIn() bool                   { return c.cli.IsLoggedIn() }
func (c *waConn) Disconnect()                      { c.cli.Disconnect() }
func (c *waConn) Logout(ctx context.Context) error { return c.cli.Logout(ctx) }

func (c *waConn) SendText(ctx context.Context, to string, body string) (string, time.Time, error) {
	jid, err := types.ParseJID(to)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("收件人 JID 不可解析: %w", err)
	}
	// Conversation 是裸文本（不带 contextInfo）；要提及/回复/转发才走 ExtendedTextMessage
	//（纯文本口径查自 WM/proto/waE2E，SendMessage 入口在 WM/send.go:188）。
	resp, err := c.cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(body)})
	return string(resp.ID), resp.Timestamp, err
}

func (c *waConn) SendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	return c.cli.SendAppState(ctx, patch)
}

func (c *waConn) OnEvent(handler func(any)) {
	c.cli.AddEventHandler(handler)
}

// waLogger 把库的日志接口接到本仓的 vars 全局日志门面上。
//
// vars 在异步通道下要求「现取不存快照」（channel_logger_manager.go 里写明存快照会绑死
// 已关闭的 writer），所以每个方法都直接调 vars，不留 *slog.Logger 字段。
type waLogger struct{ module string }

func (l waLogger) Infof(f string, a ...any)  { vars.Info("whatsmeow["+l.module+"] "+f, a...) }
func (l waLogger) Warnf(f string, a ...any)  { vars.Warning("whatsmeow["+l.module+"] "+f, a...) }
func (l waLogger) Errorf(f string, a ...any) { vars.Error("whatsmeow["+l.module+"] "+f, a...) }
func (l waLogger) Debugf(f string, a ...any) { vars.Debug("whatsmeow["+l.module+"] "+f, a...) }
func (l waLogger) Sub(module string) waLog.Logger {
	return waLogger{module: l.module + "/" + module}
}

// 编译期钉住两个实现关系：库的 logger 接口与本包的会话接口。
var (
	_ waLog.Logger = waLogger{}
	_ waSession    = (*waConn)(nil)
)

// waMgr 是消息侧的在跑快照；nil 表示未启动，门面就地拒绝而不是 panic。
// 生命周期在 whatsapp.go 的 startWA/WhatsappStop。
var waMgr atomic.Pointer[waManager]

// currentWA 取消息侧管理器；未启动时返回明确错误（fail-closed）。
func currentWA() (*waManager, error) {
	m := waMgr.Load()
	if m == nil {
		return nil, errWaNotStarted
	}
	return m, nil
}

// WhatsappListClients 返回全部在跑会话的公开事实（扩展集见 SessionInfo 注释）。
// 未启动返回 nil 而不是报错：列表是只读展示面，「一个都没有」与「没启动」对调用方
// 是同一个处置——没东西可列；要区分就用任一写门面探一次错。
func WhatsappListClients() []SessionInfo {
	m := waMgr.Load()
	if m == nil {
		return nil
	}
	return m.ListClients()
}

// WhatsappStartClient 显式启动一个已配对账号（库里必须有设备行）。
//
// 幂等：已在跑直接返回 nil。配对成功后本包**不会**自动调它（用户裁决），
// 账号什么时候上线由业务方决定。jid 接受纯数字号码或完整 "user@server" 串。
// 与其它改状态动作一样，调用方要自己串行化「先停再启」这类时序。
func WhatsappStartClient(jid string) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	j, err := waJID(jid)
	if err != nil {
		return fmt.Errorf("whatsapp 启动会话失败: %w", err)
	}
	return m.StartClient(j)
}

// WhatsappStopClient 停掉一个在跑会话（断开连接、收掉常驻协程，等退出上限 1.5s）。
// 只下线不删库：设备行还在，重新 WhatsappStartClient 即恢复；
// 要吊销配对（删设备行、之后必须重新扫码）目前没有独立门面，走库侧 DeleteDevice。
func WhatsappStopClient(jid string) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	j, err := waJID(jid)
	if err != nil {
		return fmt.Errorf("whatsapp 停止会话失败: %w", err)
	}
	return m.StopClient(j)
}

// WhatsappStartPairing 发起一次新的 QR 扫码配对，返回会话 id。
//
// 业务方拿 id 连 GET /whatsapp/qr/{session_id} 的 WebSocket 端点收二维码串
// （端点在 WhatsappStart 时注册到 gin HTTP 端口），也可以按
// util.CallWhatsappMsg+"QR" 注册回调收同一份事件（载荷 *QREvent）。
// 配对成功后设备自动落库、**不自动上线**；拿着 QREvent.JID 显式调 WhatsappStartClient。
// 会话不挂在传入 ctx 上（发起请求返回不该杀掉还没扫的码），存活上限 waQRTTL，
// 期间未扫完可重新发起。
func WhatsappStartPairing(ctx context.Context) (string, error) {
	m, err := currentWA()
	if err != nil {
		return "", err
	}
	sess, err := m.StartPairing(ctx)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// WhatsappSendMessage 用 fromJID 指定的在跑账号向 to 发一条纯文本，返回服务器给的消息 id。
//
// fromJID 与 to 都接受纯数字号码（按 s.whatsapp.net 补全）或完整 "user@server" 串
// （群聊、频道走这一支），框架不代为清洗号码，只 trim 后判空。
//
// 契约：返回空串必伴随非 nil error——拿不到消息 id 就等于没受理，调用方不能把空串
// 当成「发出去了」，否则那条流水再也对不上账。
//
// 正文里的链接不会自动带预览：库侧没有 preview_url 这一层，需要预览就发富媒体形态，
// 那不在本门面里。
func WhatsappSendMessage(ctx context.Context, fromJID, to, body string) (string, error) {
	m, err := currentWA()
	if err != nil {
		return "", err
	}
	w, err := m.client(fromJID)
	if err != nil {
		return "", err
	}
	jid, err := waJID(to)
	if err != nil {
		return "", fmt.Errorf("whatsapp 发送消息失败: %w", err)
	}
	if body == "" {
		return "", errors.New("whatsapp 发送消息失败: 消息内容为空")
	}
	if ctx == nil {
		ctx = runCtx()
	}
	msgID, _, err := w.sess.SendText(ctx, jid.String(), body)
	if err != nil {
		return "", fmt.Errorf("whatsapp 发送消息失败: %w", err)
	}
	if strings.TrimSpace(msgID) == "" {
		// 连接成功却没给消息 id：不能交空串出去冒充「已发送」。
		return "", &pay.ProviderError{Channel: "whatsapp", Code: "no_message_id", Msg: "服务端未返回消息 id"}
	}
	return msgID, nil
}

// WhatsappDeleteChat 删除 fromJID 账号客户端里 chat 这个会话框的前端显示。
//
// chat 与 WhatsappSendMessage 的 to 同一口径（号码或完整 JID 串）。
//
// lastMsgID / lastMsgTime 是「该会话最后一条消息」的 id 与时间戳，用于划定删除范围。
// 库里没有查询这两个值的入口（没有消息表、没有 GetLastMessage），所以只能由调用方
// 从自己的流水账里传进来；两者留空的后果是库侧把时间戳退化成 time.Now()
// （查自 WM/appstate/encode.go:362-363），删除范围交由对端判定，不是本包能兜住的事：
// 要精确就传，接受不精确可以传零值。
// lastMsgID 非空时按「本人发出的那条」构造消息键（FromMe=true）；群组里若要删的是
// 成员发的最后一条，请留空 lastMsgID，只传时间戳。
//
// 前置：该账号的 app state 密钥必须已同步（新配对账号首连后库会自动同步，
// 没同步完就删会报库侧错误，这条错误不在本包兜住）。
func WhatsappDeleteChat(ctx context.Context, fromJID, chat, lastMsgID string, lastMsgTime time.Time, deleteMedia bool) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	w, err := m.client(fromJID)
	if err != nil {
		return err
	}
	jid, err := waJID(chat)
	if err != nil {
		return fmt.Errorf("whatsapp 删除会话失败: %w", err)
	}
	if ctx == nil {
		ctx = runCtx()
	}
	patch := waDeleteChatPatch(jid, lastMsgID, lastMsgTime, deleteMedia)
	if err := w.sess.SendAppState(ctx, patch); err != nil {
		return fmt.Errorf("whatsapp 删除会话失败: %w", err)
	}
	return nil
}

// waDeleteChatPatch 拼删除会话的 app state 变异（与门面分开写，便于单测逐字断言入参落点）。
// BuildDeleteChat 签名查自 WM/appstate/encode.go:340：产出的 PatchInfo.Type 是
// appstate.WAPatchRegularHigh，而 "deleteChat"（appstate.IndexDeleteChat，WM/appstate/keys.go:121）
// 落在 Mutations[0].Index[0]，Index 三段是 [deleteChat, 目标 JID, deleteMedia 0/1]。
func waDeleteChatPatch(chat types.JID, lastMsgID string, lastMsgTime time.Time, deleteMedia bool) appstate.PatchInfo {
	var key *waCommon.MessageKey
	if id := strings.TrimSpace(lastMsgID); id != "" {
		key = &waCommon.MessageKey{
			RemoteJID: proto.String(chat.String()),
			FromMe:    proto.Bool(true),
			ID:        proto.String(id),
		}
	}
	return appstate.BuildDeleteChat(chat, lastMsgTime, key, deleteMedia)
}

// waInbound 摊平一条入站消息（events.Message 字段查自 WM/types/events/events.go:322，
// MessageInfo/MessageSource 查自 WM/types/message.go:27-32、99-115）。
func waInbound(account string, e *events.Message) *InboundMessage {
	if e == nil {
		return nil
	}
	kind, body := waBodyText(e.Message)
	return &InboundMessage{
		Account:   account,
		Chat:      e.Info.Chat.String(),
		Sender:    e.Info.Sender.String(),
		MsgID:     string(e.Info.ID),
		Timestamp: e.Info.Timestamp,
		Type:      kind,
		Body:      body,
		IsFromMe:  e.Info.IsFromMe,
		IsGroup:   e.Info.IsGroup,
	}
}

// waReceipt 摊平一条投递回执（events.Receipt 字段查自 WM/types/events/events.go:443-452）。
func waReceipt(account string, e *events.Receipt) *DeliveryReceipt {
	if e == nil {
		return nil
	}
	ids := make([]string, 0, len(e.MessageIDs))
	for _, id := range e.MessageIDs {
		ids = append(ids, string(id))
	}
	return &DeliveryReceipt{
		Account:   account,
		Chat:      e.Chat.String(),
		Sender:    e.Sender.String(),
		MsgIDs:    ids,
		Status:    string(e.Type),
		Timestamp: e.Timestamp,
	}
}

// waBodyText 判定消息形态并取出可读正文。
//
// 只取文本与带 caption 的媒体；二进制载荷不在这里下载。解不开形态时返回 "other"
// 而不是丢弃——业务方按 Type 决定怎么处理，框架不替它判定「这条不重要」。
func waBodyText(m *waE2E.Message) (kind, body string) {
	if m == nil {
		return "empty", ""
	}
	switch {
	case m.GetConversation() != "":
		return "text", m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		return "extended_text", m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		return "image", m.GetImageMessage().GetCaption()
	case m.GetVideoMessage() != nil:
		return "video", m.GetVideoMessage().GetCaption()
	case m.GetDocumentMessage() != nil:
		return "document", m.GetDocumentMessage().GetCaption()
	case m.GetAudioMessage() != nil:
		return "audio", ""
	case m.GetStickerMessage() != nil:
		return "sticker", ""
	case m.GetContactMessage() != nil:
		return "contact", m.GetContactMessage().GetDisplayName()
	case m.GetLocationMessage() != nil:
		return "location", ""
	case m.GetProtocolMessage() != nil:
		return "protocol", ""
	default:
		return "other", ""
	}
}

// publishWA 把一类消息侧事件广播给业务方，键为 util.CallWhatsappMsg+kind，
// kind = Inbound / Receipt / Connection / QR。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
func publishWA(kind string, payload any) {
	if payload == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallWhatsappMsg+kind, payload); !ok {
		vars.Debug("whatsapp 事件无人订阅: kind=%s", kind)
	}
}

// waJID 把「纯数字号码」或「user@server 形态的 JID 串」归一成 types.JID。
//
// 收件人口径与资金侧一致：框架不代为清洗业务方给的号码，只保证两种写法都能用。
// Device>0 的 JID 库侧会直接拒（查自 WM/send.go:200），这里提前拒并说清是哪一段写错了。
func waJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, errors.New("目标为空")
	}
	if !strings.Contains(raw, "@") {
		digits := WhatsappNormalizePhone(raw)
		if digits == "" {
			return types.JID{}, fmt.Errorf("目标 %q 里没有任何数字，拼不出 WhatsApp 号码", raw)
		}
		return types.NewJID(digits, waServerUser), nil
	}
	jid, err := types.ParseJID(raw)
	if err != nil {
		return types.JID{}, fmt.Errorf("目标 %q 不是可用 JID: %w", raw, err)
	}
	if jid.Device != 0 {
		return types.JID{}, fmt.Errorf("目标 %q 指向具体设备，发消息只接受会话 JID（不带设备号）", raw)
	}
	return jid, nil
}

// WhatsappNormalizePhone 清洗号码：去掉所有非数字字符，得到 E.164 形态的纯数字。
//
// 入参接受空格、+、括号、连字符；号码应以国家码开头，不带 + 也可。
func WhatsappNormalizePhone(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
