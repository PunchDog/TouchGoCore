package whatsapp

// 本文件是 whatsmeow 消息侧的门面：发一条文本消息（单聊或群组）、异步批量发信
// （后台 goroutine 逐条、随机间隔、小时闸）、查好友列表、
// 查可发言的群组列表、删掉代持账号客户端里那个会话框、
// 多账号 Client 的启停与列表、QR 配对的发起、吊销账号配对，以及把入站事件摊平成回调广播给业务方。
// 常驻协程与会话管理在 wa_manager.go，配对泵在 wa_pairing.go，存储在 store_postgres.go，
// 吊销配对在 wa_delete_device.go（它要串起 latch/Logout/停机/删行/广播五步，单独成文）。
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
// WhatsappStartPairing / WhatsappDeleteDevice 属改状态动作，调用方必须串行化
// （管理器只兜「不 panic、不双启」）。
//
// 边界口径与资金侧一致：本包不持有按手机号/会话的业务状态。删会话要的 lastMessage
// 优先由调用方传入，调用方留空时回退本包的发送账（只记经本包发出的最近一条）；
// 消息落库、幂等去重、冷却与预算都归业务方。

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
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

// WaContact 是一个好友（通讯录联系人）的可公开事实，字段取自库的 types.ContactInfo。
//
// 数据源是 app state 联系人同步的落库产物（cli.Store.Contacts），不是实时向服务器查询：
// 刚配对、同步未完成的账号列出来是空的，属正常态而不是错误。
// Name 系四个字段是用户自设的展示名，业务侧展示前自行判空兜底。
type WaContact struct {
	JID          string
	FirstName    string
	FullName     string
	PushName     string
	BusinessName string
}

// WaGroup 是一个「当前账号可发言」群组的可公开事实，字段取自库的 types.GroupInfo。
//
// 判定口径见 WhatsappListSpeakableGroups：只收「本人仍在群里且能发消息」的群组，
// IsAdmin / IsSuperAdmin 是本人在这群里（或社区）的管理员身份。
type WaGroup struct {
	JID          string
	Name         string
	Topic        string
	MemberCount  int
	IsAnnounce   bool // 仅管理员可发消息的群组（能进列表即因本人是管理员）
	IsLocked     bool // 仅管理员可改群设置（不影响发言）
	IsAdmin      bool
	IsSuperAdmin bool
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
	// GetLIDForPN 查 PN（s.whatsapp.net 号码）对应的 LID：whatsmeow 发送对单聊目标
	// 会做同款改写（WM/send.go:339-356），删框与对账必须落在同一条会话 JID 上。
	// 查不到映射时库返回空 JID（可能伴随 nil error），调用方自行退回 PN。
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
	// GetContacts 读该账号在库内联系人区的全量好友（app state 联系人同步的落库产物，
	// 不联网；真实现走 cli.Store.Contacts.GetAllContacts，WM/broadcast.go:59 同源用法）。
	GetContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
	// GetJoinedGroups 向服务器查询该账号已加入的群组（需在线，一次 IQ 往返，
	// 查自 WM/group.go:535）。
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
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

func (c *waConn) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	return c.cli.Store.LIDs.GetLIDForPN(ctx, pn)
}

func (c *waConn) GetContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return c.cli.Store.Contacts.GetAllContacts(ctx)
}

func (c *waConn) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	return c.cli.GetJoinedGroups(ctx)
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
// 账号什么时候上线由业务方决定。jid 要与会话库设备行的主键逐字一致：
// 配套设备形态普遍是 "号码:1@s.whatsapp.net"（带设备段），纯数字号码只命中主设备行。
// 与其它改状态动作一样，调用方要自己串行化「先停再启」这类时序。
func WhatsappStartClient(jid string) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	j, err := waAccountJID(jid)
	if err != nil {
		return fmt.Errorf("whatsapp 启动会话失败: %w", err)
	}
	return m.StartClient(j)
}

// WhatsappStopClient 停掉一个在跑会话（断开连接、收掉常驻协程，等退出上限 1.5s）。
// 只下线不删库：设备行还在，重新 WhatsappStartClient 即恢复；
// 要吊销配对（删设备行、之后必须重新扫码）用 WhatsappDeleteDevice。
func WhatsappStopClient(jid string) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	j, err := waAccountJID(jid)
	if err != nil {
		return fmt.Errorf("whatsapp 停止会话失败: %w", err)
	}
	return m.StopClient(j)
}

// WhatsappDeleteDevice 吊销 jid 的配对：删掉会话库里那一行设备，之后该账号必须重新扫码。
// jid 与 StartClient/StopClient 同一口径（与会话库设备行主键逐字一致，
// 配套设备形态带 :N 设备段；纯数字号码只命中主设备行）。
//
// 与 StopClient 的分界在「还要不要这个账号」：StopClient 只下线，行还在随时能拉起来；
// 本门面连行带密钥材料一并清除，账号留在本包之外。
//
// 在跑与否决定能不能拿到服务端解绑（whatsmeow 的 Logout 会向 WhatsApp 发
// md/set remove-companion-device，手机端「已关联设备」里那条随之消失）：
//   - 账号在跑：先服务端解绑，再收常驻协程，最后删行（顺序逐条理由见 wa_delete_device.go）。
//   - 账号没在跑：只删行。不在一个面向调用方的破坏性调用里起整条握手，代价是手机端
//     仍显示为已关联设备、要用户手动取消。**要拿到服务端解绑，先 WhatsappStartClient 再调本门面。**
//
// 返回契约（下游按文案分支，两种错误处置完全不同）：
//   - nil：设备行已删 + 服务端解绑成功 + 常驻协程已收，三者齐备。
//   - 「读取会话库失败 / 吊销设备行失败 / 没有设备行」：行可能还在，整体重试是安全的。
//   - 「设备行已删除，但服务端 unlink 未完成 …」：行已经没了，**别再重试整个删除**
//     （第二次必报「没有设备行」），只剩手机端那条残留关联要人工处置。
//
// 每真删掉一行的路径都恰好广播一条 ConnEvent{State:"logged_out"}（含解绑失败那条）——
// 库侧 Logout 全程不发事件，不广播就得让下游继续往死账号路由。logged_out 是终态，
// 与停机过程可能出现的 disconnected 之间没有先后保证；Reason 只给分类文案，
// 库的原始错误串只进返回值与日志。
//
// 最坏耗时 ≈ waLogoutTimeout(5s) + waStopWaitBudget(1.5s) + 三次库往返：解绑那一次 IQ
// 库默认等 75 秒，本包夹到 5 秒；删行走包级运行上下文，调用方取消不会留下「名义吊销、
// 行还在」的半吊子状态。要按这个预算设自己的上游超时。
//
// 不碰进行中的 QR 配对会话，也不删业务工程自己的绑定/账号行（本包不持有业务状态）。
// 与 StartClient/StopClient 同属改状态动作，调用方必须串行化。
func WhatsappDeleteDevice(ctx context.Context, jid string) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	j, err := waAccountJID(jid)
	if err != nil {
		return fmt.Errorf("whatsapp 删除设备失败: %w", err)
	}
	if err := m.DeleteDevice(ctx, j); err != nil {
		return fmt.Errorf("whatsapp 删除设备失败: %w", err)
	}
	return nil
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
// fromJID 按账号口径（waAccountJID）：与会话库设备行主键逐字一致，配套设备形态带
// :N 设备段；to 按会话 JID 口径（waJID）：纯数字号码（按 s.whatsapp.net 补全）或
// 完整 "user@server" 串（群聊、频道走这一支），带设备号的收件人会被拒。
// 框架不代为清洗号码，只 trim 后判空。
//
// 契约：返回空串必伴随非 nil error——拿不到消息 id 就等于没受理，调用方不能把空串
// 当成「发出去了」，否则那条流水再也对不上账。
//
// 正文里的链接不会自动带预览：库侧没有 preview_url 这一层，需要预览就发富媒体形态，
// 那不在本门面里。
//
// 连接态自愈：这个账号的会话断开时，本门面会先催一轮重连（退避睡眠被立即叫醒），
// 僵尸会话（常驻协程已收手或已收 logged_out）则先摘掉再重建，连上才发；
// 等待上限 EffectiveConnectTimeout（缺省 15 秒），到点或调用方 ctx 取消即返回错误，
// 不会把发信请求挂在无限等待上。会话真被服务端吊销了，错误文案是「需重新扫码配对」。
func WhatsappSendMessage(ctx context.Context, fromJID, to, body string) (string, error) {
	m, err := currentWA()
	if err != nil {
		return "", err
	}
	w, err := m.ensureReady(ctx, fromJID)
	if err != nil {
		return "", fmt.Errorf("whatsapp 发送消息失败: %w", err)
	}
	jid, err := waJID(to)
	if err != nil {
		return "", fmt.Errorf("whatsapp 发送消息失败: %w", err)
	}
	msgID, _, err := waSendText(ctx, w, jid, body, "发送消息")
	return msgID, err
}

// WhatsappSendGroupMessage 用 fromJID 指定的在跑账号向 groupJID 群组发一条纯文本，
// 返回服务器给的消息 id。与 WhatsappSendMessage 同一契约（空 id ⇔ 非 nil error），
// 差别只在目标必须先过「是群组 JID（@g.us）」这道闸——写成号码的会直接被拒，
// 避免业务方把群号传错域后静默发到某个素不相识的单聊里去。
//
// 能不能发（仅管理员可发言群、被禁言等）不在这里判定，先用
// WhatsappListSpeakableGroups 拿可发言清单；真发出去被服务器拒了会原样带错误上抛。
// 连接态自愈与等待上限同 WhatsappSendMessage。
func WhatsappSendGroupMessage(ctx context.Context, fromJID, groupJID, body string) (string, error) {
	m, err := currentWA()
	if err != nil {
		return "", err
	}
	w, err := m.ensureReady(ctx, fromJID)
	if err != nil {
		return "", fmt.Errorf("whatsapp 发送群组消息失败: %w", err)
	}
	jid, err := waJID(groupJID)
	if err != nil {
		return "", fmt.Errorf("whatsapp 发送群组消息失败: %w", err)
	}
	if jid.Server != types.GroupServer {
		return "", fmt.Errorf("whatsapp 发送群组消息失败: 目标 %q 不是群组 JID（服务端应为 %s）", groupJID, types.GroupServer)
	}
	msgID, _, err := waSendText(ctx, w, jid, body, "发送群组消息")
	return msgID, err
}

// waSendText 是发信门面共享的发送段：空正文就地拒绝，ctx 为空退化到包级运行
// 上下文，拿不到消息 id 一律折算成错误（label 决定错误文案的通道动作前缀）。
// 返回的 chat 是实际会话 JID 串（单聊 PN 会被库改写成 LID 投递，发送成功后解析；
// 未发出时为空串），成功的单聊同时记一笔发送账供删框回填。
//
// 防封闸（wa_antiban.go）开启时，「问闸→睡到点→真发→记账」整段持 gateMu：
// 同账号发送被串成单行道，这是防封的本意；闸拦下的发送以「防封闸拦截」错误返回。
func waSendText(ctx context.Context, w *waClientWrapper, jid types.JID, body, label string) (string, string, error) {
	if body == "" {
		return "", "", fmt.Errorf("whatsapp %s失败: 消息内容为空", label)
	}
	if ctx == nil {
		ctx = runCtx()
	}
	target := jid.String()
	if w.gate != nil {
		w.gateMu.Lock()
		defer w.gateMu.Unlock()
		wasNew := !w.gate.IsKnownChat(target)
		delayMs, gerr := w.gate.BeforeSend(target, body, waIsGroupOrNewsletter(jid), "", time.Now())
		if gerr != nil {
			return "", "", fmt.Errorf("whatsapp %s失败: 防封闸拦截: %w", label, gerr)
		}
		if delayMs > 0 {
			// 防封延迟可达十几秒，必须认 ctx：只 time.Sleep 的话，调用方（HTTP 请求）
			// 早就断了这里还在睡，醒来又往一个已取消的 ctx 上发一条注定失败的消息。
			if werr := waSleepCtx(ctx, time.Duration(delayMs)*time.Millisecond); werr != nil {
				w.gate.AfterSendFailed(time.Now())
				return "", "", waSendErr(label, werr)
			}
		}
		msgID, ts, err := w.sess.SendText(ctx, target, body)
		if err != nil {
			w.gate.AfterSendFailed(time.Now())
			return "", "", waSendErr(label, err)
		}
		w.gate.AfterSend(target, body, waIsGroupOrNewsletter(jid), "", wasNew, time.Now())
		if strings.TrimSpace(msgID) == "" {
			return "", "", &pay.ProviderError{Channel: "whatsapp", Code: "no_message_id", Msg: "服务端未返回消息 id"}
		}
		chat := waResolveChat(ctx, w.sess, jid).String()
		w.recordLastSend(chat, msgID, ts)
		return msgID, chat, nil
	}
	msgID, ts, err := w.sess.SendText(ctx, target, body)
	if err != nil {
		return "", "", waSendErr(label, err)
	}
	if strings.TrimSpace(msgID) == "" {
		// 连接成功却没给消息 id：不能交空串出去冒充「已发送」。
		return "", "", &pay.ProviderError{Channel: "whatsapp", Code: "no_message_id", Msg: "服务端未返回消息 id"}
	}
	chat := waResolveChat(ctx, w.sess, jid).String()
	w.recordLastSend(chat, msgID, ts)
	return msgID, chat, nil
}

// waSleepCtx 是可被 ctx 打断的睡眠。
func waSleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waSendErr 把底层发送错误折成门面无错。
//
// 专治一类误诊：库在 LID 解析、会话库读写里用的都是调用方传进来的 ctx，
// 请求被取消时错误串长这样——
// 「failed to get LID for PN 86xxx@s.whatsapp.net: context canceled」——
// 看上去像号码没注册/LID 查不到，实际是本机的 HTTP 请求已经断了、这条根本没发出去。
// 值班照字面去查号码就白跑一趟，所以这里把「上下文已取消/超时」这一类单独点名，
// 原始错误仍用 %w 留在链上可判。
func waSendErr(label string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("whatsapp %s失败: 调用方上下文已结束（请求超时或客户端断开），本条未发出: %w", label, err)
	}
	return fmt.Errorf("whatsapp %s失败: %w", label, err)
}

// waIsGroupOrNewsletter 判定收件 JID 是不是群/频道形态（防封闸按这个口径走群限速门）。
func waIsGroupOrNewsletter(jid types.JID) bool {
	return jid.Server == types.GroupServer || jid.Server == types.NewsletterServer
}

// OutboundMessage 是异步批量发信的一条待发消息。To 与 WhatsappSendMessage 的收件人
// 同一口径（纯数字号码或完整会话 JID 串，群组写 "xxx@g.us"，带设备号的拒）。
type OutboundMessage struct {
	To   string
	Body string
}

// SendResult 是异步批量发信里一条消息的落点，经 CallWhatsappMsg+"SendResult" 广播。
//
// Err 为空串即该条已受理（MsgID 非空，与同步门面的「空 id ⇔ 非 nil error」同一条契约）；
// 批内每条各广播一次，业务方按 Account+To+MsgID 对账自己的流水。
// To 保持调用方传入的原文；Chat 是实际落点的会话 JID——单聊会被 whatsmeow 改写成
// LID（发送挂在 LID 会话上），发后要删框/对账直接用 Chat，不必自己猜 LID。
type SendResult struct {
	Account string
	To      string
	Chat    string
	MsgID   string
	Err     string
}

// 批内节奏的两个注入点：生产用真睡眠与随机时长，测试整体替换（照 waSessionFactory 的换-还原范式）。
var (
	waBatchSleep = time.Sleep
	waBatchDelay = func(min, max time.Duration) time.Duration {
		if max <= min {
			return min
		}
		return min + time.Duration(rand.Int64N(int64(max-min)+1))
	}
)

// errWaSendRateLimited 是小时闸拒绝的固定文案（下游按 Err 串分支时它是稳定值）。
var errWaSendRateLimited = errors.New("超出单账号每小时发送上限（send_max_per_hour），本条已拒")

// WhatsappSendBatchAsync 把一批文本交给后台协程逐条发送：调用当场返回，
// 每条结果（成功与失败都算）经 util.CallWhatsappMsg+"SendResult" 广播一条 SendResult。
//
// 节奏与闸（三段配置见 whatsapp.sessions 段）：
//   - 第一条立即发；此后每条之前随机睡 [send_min_delay_sec, send_max_delay_sec]
//     （缺省 1~5 秒）。随机而不是固定间隔：固定节奏是风控最容易被识别的形态。
//   - 单账号滚动一小时最多发 send_max_per_hour 条（缺省 30），与同步门面无关，
//     只记本包发送路径的账；超限的那条广播拒绝结果后继续走剩余条目（不整批中止）。
//   - 批次终止只认两件事：调用方显式设的 deadline、以及进程停机。二者任一发生，
//     未发的条目逐条广播「发送已取消」而不是静默蒸发，业务方账面上每条都有落点。
//     调用方中途 cancel 不在此列（理由见 waBatchCtx）。
//
// 返回的 error 只覆盖「批次根本没被接走」：消息侧未启动、账号不在跑、列表为空。
// 接走之后单条的成败一律看广播，不看这里的返回值。
// 账号在批跑完之前被 WhatsappStopClient/吊销时，剩余条目会以发送失败形态广播出来
// （底层连接已断），本包不做跨停机的续发。
//
// ctx 的口径（2026-10-14 改，与旧版相反，理由在函数内）：**调用方的取消信号不传导**，
// 只有调用方设的 deadline 与进程停机信号会中止剩余条目。要主动停一批就用
// WhatsappStopClient（收协程 + 拆连接，剩余条目逐条广播失败），别指望 cancel ctx。
func WhatsappSendBatchAsync(ctx context.Context, fromJID string, msgs []OutboundMessage) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	w, err := m.ensureAlive(fromJID)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return errors.New("whatsapp 批量发信失败: 消息列表为空")
	}
	bctx, cancel := waBatchCtx(ctx)
	go func() {
		defer cancel()
		waRunBatch(bctx, w, msgs)
	}()
	return nil
}

// waBatchCtx 把「已受理」的批次从调用方的取消信号里摘出来，只保留两类终止条件：
// 调用方显式设的截止时间、以及进程停机（runCtx 被收掉）。
//
// 为什么要改这条契约（真机实证）：这个门面的语义是「当场返回、结果看广播」，
// 而防封闸把每条间隔拉到 1.5~5 秒（新会话再 +3 秒），一批几十条必然跑过任何
// HTTP 请求预算。旧契约把调用方 ctx 一路传到底，于是浏览器 40 秒放弃标签页 →
// Go 取消 request ctx → 剩下几十条以「调用方上下文已结束」逐条刷屏失败：
// 门面已经答过「已受理」，却因客户端挂断而半批蒸发，且调用方拿到的返回值是 nil，
// 从返回值上完全看不出出了事。防封节奏与请求生命周期本就不该互相绑定。
//
// deadline 仍然传导：调用方想给一批封顶就传 WithTimeout，这是明确的预算而不是
// 「谁顺手把标签页关了」。base 取 runCtx() 而不是 WithoutCancel(ctx)，
// 是为了让停机信号照常传导——进程都要退了，不该留一批还在往外发消息。
func waBatchCtx(caller context.Context) (context.Context, context.CancelFunc) {
	base := runCtx()
	if caller == nil {
		return base, func() {}
	}
	if dl, ok := caller.Deadline(); ok {
		return context.WithDeadline(base, dl)
	}
	return base, func() {}
}

// waRunBatch 是后台协程本体：逐条「睡眠→判定→发送→广播」，任何单条错误都不中止整批。
//
// 防封闸开启时批内不再自己睡间隔：BeforeSend 返回的随机延迟就是权威节奏，
// 两层再叠只会把批次拖慢一倍（小时闸 takeSendSlot 仍保留，作独立硬上限）。
func waRunBatch(ctx context.Context, w *waClientWrapper, msgs []OutboundMessage) {
	account := w.jid.String()
	minD := w.cfg.EffectiveSendMinDelay()
	maxD := w.cfg.EffectiveSendMaxDelay()
	maxHour := w.cfg.EffectiveSendMaxPerHour()
	for i, msg := range msgs {
		if i > 0 && w.gate == nil {
			waBatchSleep(waBatchDelay(minD, maxD))
		}
		res := SendResult{Account: account, To: msg.To}
		switch jid, perr := waJID(msg.To); {
		case ctx.Err() != nil:
			res.Err = "whatsapp 批量发信失败: 发送已取消: " + ctx.Err().Error()
		case perr != nil:
			res.Err = fmt.Sprintf("whatsapp 批量发信失败: %v", perr)
		case msg.Body == "":
			res.Err = "whatsapp 批量发信失败: 消息内容为空"
		case !w.takeSendSlot(time.Now(), maxHour):
			res.Err = "whatsapp 批量发信失败: " + errWaSendRateLimited.Error()
		default:
			// 逐条自愈：一批几十条的时间里 socket 可能中途掉（或半途收到吊销事件），
			// 不在这里 reconnect 的话，后面每一条都撞在同一个断口上，整批以同一条错误刷屏。
			if rerr := w.reconnectAndAwait(ctx); rerr != nil {
				res.Err = "whatsapp 批量发信失败: " + rerr.Error()
			} else {
				msgID, chat, serr := waSendText(ctx, w, jid, msg.Body, "批量发信")
				if serr != nil {
					res.Err = serr.Error()
				} else {
					res.MsgID = msgID
					res.Chat = chat
				}
			}
		}
		publishWA("SendResult", &res)
	}
}

// WhatsappListContacts 列出 fromJID 账号的全部好友，按 JID 串排序。
//
// 只读本地联系人库（app state 同步产物），不产生网络往返；同步尚未完成时返回空列表
// 而不是错误。「账号必须在跑」这条与发信门面一致——没在跑的账号先报「没有在跑」。
// 死会话（logged_out 后常驻协程收手）在这里会被复活重建：库把已删除设备的子存储换成了
// NoopStore，不重建就再也读不到联系人；但本门面**不等登录建立**（本地读不需要活 socket）。
func WhatsappListContacts(ctx context.Context, fromJID string) ([]WaContact, error) {
	m, err := currentWA()
	if err != nil {
		return nil, err
	}
	w, err := m.ensureAlive(fromJID)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = runCtx()
	}
	contacts, err := w.sess.GetContacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp 获取好友列表失败: %w", err)
	}
	out := make([]WaContact, 0, len(contacts))
	for jid, c := range contacts {
		out = append(out, WaContact{
			JID:          jid.String(),
			FirstName:    c.FirstName,
			FullName:     c.FullName,
			PushName:     c.PushName,
			BusinessName: c.BusinessName,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JID < out[j].JID })
	return out, nil
}

// WhatsappListSpeakableGroups 列出 fromJID 账号里「当前能发言」的群组，按 JID 串排序。
//
// 数据源是向服务器实时查询已加入群组（GetJoinedGroups，需在线），逐群判定：
//   - 已暂停/封禁的群（Suspended）剔除——发进去也不会送达；
//   - 仅管理员可发言的群（IsAnnounce）：只有本人按参与者名单命中且是管理员/超级管理员
//     才保留；名单里匹配不到本人（如隐名群只暴露 LID）时 fail-closed 剔除。
//   - IsLocked 只限制改群设置，不限制发消息，不作为剔除条件。
//
// 本人身份按「用户号 + 服务端域」逐字比对，忽略设备号/Agent 段；参与者条目的
// JID（LID 或号码）与 PhoneNumber 两个字段都拿来对，命中其一即视为本人。
//
// 连接态自愈同 WhatsappSendMessage：这条门面本身要向服务器实时查询，掉线时先催重连
// 并等连上（上限 EffectiveConnectTimeout），死会话先复活——旧形态下断线直接报
// 「websocket not connected」，把后台群发的整批卡在列群组这一步。
func WhatsappListSpeakableGroups(ctx context.Context, fromJID string) ([]WaGroup, error) {
	m, err := currentWA()
	if err != nil {
		return nil, err
	}
	w, err := m.ensureReady(ctx, fromJID)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = runCtx()
	}
	groups, err := w.sess.GetJoinedGroups(ctx)
	if err != nil {
		return nil, waSendErr("获取可发言群组列表", err)
	}
	out := make([]WaGroup, 0, len(groups))
	for _, g := range groups {
		if g == nil || g.Suspended {
			continue
		}
		isAdmin, isSuper := waSelfGroupAdmin(g, w.jid)
		if g.IsAnnounce && !isAdmin && !isSuper {
			continue
		}
		memberCount := g.ParticipantCount
		if memberCount == 0 {
			// 列表查询的响应里计数属性可能缺省，兜底按已解析的参与者名单数
			// （真机实测：GetJoinedGroups 返回的行 ParticipantCount=0 但名单齐全）。
			memberCount = len(g.Participants)
		}
		out = append(out, WaGroup{
			JID:          g.JID.String(),
			Name:         g.Name,
			Topic:        g.Topic,
			MemberCount:  memberCount,
			IsAnnounce:   g.IsAnnounce,
			IsLocked:     g.IsLocked,
			IsAdmin:      isAdmin,
			IsSuperAdmin: isSuper,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JID < out[j].JID })
	return out, nil
}

// waSelfGroupAdmin 在群参与者名单里找本人，返回其管理员标记；名单里没有本人时
// 两个值都是 false（调用方按 fail-closed 处理仅管理员可发言群）。
func waSelfGroupAdmin(g *types.GroupInfo, self types.JID) (isAdmin, isSuperAdmin bool) {
	for _, p := range g.Participants {
		if waSameWaUser(p.JID, self) || waSameWaUser(p.PhoneNumber, self) {
			return p.IsAdmin, p.IsSuperAdmin
		}
	}
	return false, false
}

// waSameWaUser 判定两个 JID 是否同一用户：用户号与域逐字相等，设备号/Agent 段忽略
// （会话 JID 与参与者条目带的设备段不同，比对时不该参与判等）。
func waSameWaUser(a, b types.JID) bool {
	return a.User != "" && a.User == b.User && a.Server == b.Server
}

// waResolveChat 对单聊（s.whatsapp.net）目标做与 whatsmeow 发送同款的 PN→LID 改写
// （库在 WM/send.go:339-356 用 Store.LIDs.GetLIDForPN 把收件人换成 LID 再发出，
// 手机端会话框因此挂在 LID 上）。查不到映射（未迁 LID 的老会话、缓存未同步）或
// 查询失败就退回 PN；群组/频道等其它域原样返回，不改写。
func waResolveChat(ctx context.Context, sess waSession, jid types.JID) types.JID {
	if jid.Server != waServerUser {
		return jid
	}
	lid, err := sess.GetLIDForPN(ctx, jid)
	if err != nil {
		vars.Debug("whatsapp LID 解析失败，退回 PN: pn=%s err=%v", jid.String(), err)
		return jid
	}
	if lid.IsEmpty() {
		return jid
	}
	return lid
}

// WhatsappDeleteChat 删除 fromJID 账号客户端里 chat 这个会话框的前端显示。
//
// chat 与 WhatsappSendMessage 的 to 同一口径（号码或完整 JID 串）。
// 单聊目标先做与发送同款的 PN→LID 改写（waResolveChat）：手机上的会话挂在 LID 上，
// patch 按 PN 提交会被对端静默丢掉（SendAppState 仍返回 nil，假绿），必须用解析后的
// LID 作为 BuildDeleteChat 的 target 与 MessageKey.RemoteJID；查不到映射才退回 PN。
//
// lastMsgID / lastMsgTime 是「该会话最后一条消息」的 id 与时间戳，用于划定删除范围。
// lastMsgID 留空时先回退本包发送账（该账号最近一条发往该会话的 id+时间，发信成功即记账；
// 调用方传了 lastMsgTime 则保留调用方的时间，只回填 id）；发送账也没有时维持库侧退化
// 行为（时间戳退化成 time.Now()、不带消息键，查自 WM/appstate/encode.go:362-363），
// 并记一条 Warning——删除范围交由对端判定，不在本包兜住。
// lastMsgID 非空时按「本人发出的那条」构造消息键（FromMe=true）；群组里若要删的是
// 成员发的最后一条，请留空 lastMsgID，只传时间戳。
//
// 前置：该账号的 app state 密钥必须已同步（新配对账号首连后库会自动同步，
// 没同步完就删会报库侧错误，这条错误不在本包兜住）。
// 连接态自愈同 WhatsappSendMessage（删框要往服务器发 app state 变异）。
func WhatsappDeleteChat(ctx context.Context, fromJID, chat, lastMsgID string, lastMsgTime time.Time, deleteMedia bool) error {
	m, err := currentWA()
	if err != nil {
		return err
	}
	w, err := m.ensureReady(ctx, fromJID)
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
	target := waResolveChat(ctx, w.sess, jid)
	if strings.TrimSpace(lastMsgID) == "" {
		id, ts, ok := w.lastSend(target.String())
		if !ok && target != jid {
			// 发送时 LID 尚未解析成功、账记在 PN 键上的旧条目也要捞得到。
			id, ts, ok = w.lastSend(jid.String())
		}
		if ok {
			lastMsgID = id
			if lastMsgTime.IsZero() {
				lastMsgTime = ts
			}
		} else {
			vars.Warning("whatsapp 删除会话: chat=%s 无消息键可回填（调用方未传 lastMsgID 且发送账无记录），删除范围交由对端判定", target.String())
		}
	}
	patch := waDeleteChatPatch(target, lastMsgID, lastMsgTime, deleteMedia)
	if err := w.sess.SendAppState(ctx, patch); err != nil {
		return waSendErr("删除会话", err)
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
// kind = Inbound / Receipt / Connection / QR / SendResult。
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
// 账号侧（启动/停止/吊销/查询按哪个账号执行）不要用本函数，用 waAccountJID：
// 配套设备形态的账号 JID 本来就带 :N 设备段。
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

// waAccountJID 归一「操作哪个已配对账号」的 JID，与 waJID 的分界在设备段：
// 会话库的设备行主键存的就是配对时服务器分配的完整 JID，配套设备形态普遍带
// :1 这类设备段（sqlstore 按 jid 逐字查行，查自 WM/store/sqlstore/container.go:123），
// 所以这里必须原样放行设备段，而不是替调用方抹掉——抹掉反而查不到行。
// 纯数字号码仍按 s.whatsapp.net 补全（设备段为 0，只命中主设备行）；
// 群组/频道等其它服务端的 JID 不可能是账号，就地拒绝。
func waAccountJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, errors.New("账号为空")
	}
	if !strings.Contains(raw, "@") {
		digits := WhatsappNormalizePhone(raw)
		if digits == "" {
			return types.JID{}, fmt.Errorf("账号 %q 里没有任何数字，拼不出 WhatsApp 号码", raw)
		}
		return types.NewJID(digits, waServerUser), nil
	}
	jid, err := types.ParseJID(raw)
	if err != nil {
		return types.JID{}, fmt.Errorf("账号 %q 不是可用 JID: %w", raw, err)
	}
	if jid.Server != waServerUser {
		return types.JID{}, fmt.Errorf("账号 %q 的域不是 %s，账号只能是用户 JID（可带 :N 设备段）", raw, waServerUser)
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
