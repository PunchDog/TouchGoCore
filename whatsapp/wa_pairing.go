package whatsapp

// 本文件是 QR 扫码配对流程：起一个未配对的一次性 Client，把库的 QR 通道摊平成
// 可订阅的会话事件，配对成功后设备由 sqlstore 自动落库（查自 WM/store/sqlstore/
// container.go:208-224「pairing 成功会自动调 Save」），**不自动 StartClient**——
// 用户裁决：扫码成功只代表「凭据到手」，账号什么时候上线由业务方显式决定，
// 自动拉起会让「刚扫完码就撞见 N 个账号同时连 WhatsApp」这种封号高危形态不可控。
//
// QR 通道的库侧口径（查自 WM/qrchan.go）：
//   - GetQRChannel 必须在 Connect 之前调（:221-228），已配对设备返回 ErrQRStoreContainsID；
//   - Item.Event 是普通 string（不是枚举类型），"code" 带 Code 字段（二维码原始串），
//     终局是 "success"/"timeout"/"error"/"err-*" 之一，终局项之后库立即 close 通道；
//   - 传给 GetQRChannel 的 ctx 被取消时，库自己关通道并 Disconnect（:124-132）——
//     这就是 Cancel 与 TTL 的实现机制，不需要另造一套停止信号。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"

	"touchgocore/vars"
)

// waQRTTL 是一次配对会话的最长存活时间。到点后 ctx 取消，库关 QR 通道并断连，
// 会话从活动表摘除。二维码本身约 20-60 秒一轮换、库侧耗尽后发 "timeout"，
// TTL 兜的是「Connect 卡住/事件不来」这类库侧终局缺失的形态。
// 变量而非常量：测试注入短 TTL 断言清理路径。
var waQRTTL = 5 * time.Minute

// QREvent 是一条配对会话事件（广播与 WebSocket 推送共用的载荷）。
//
// Event 取值即库侧 Item.Event 原值："code"（新二维码，Code 字段有值）、
// "success"（配对成功，JID 字段为新账号）、"timeout"、"error"（Error 字段有值）、
// "err-unexpected-state"/"err-client-outdated"/"err-scanned-without-multidevice"
// （查自 WM/qrchan.go:38-56）以及 passkey 两态（原样转发，本包不代答）。
type QREvent struct {
	SessionID string `json:"session_id"`
	Event     string `json:"event"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
	JID       string `json:"jid,omitempty"`
}

// QRSession 是一次进行中的 QR 配对。
//
// 库的 QR 通道是单消费者的（close 之后不可重读），而 WebSocket 端可能有多个订阅者
// （运维页面 + 业务后台同时看一个码），所以这里做一层扇出：泵协程读库通道，
// Subscribe 的各方拿自己的缓冲通道。订阅通道满时丢最旧不阻塞——QR 码是轮换的，
// 丢掉的旧码马上被新码覆盖，阻塞泵协程反而会把库通道憋死（缓冲 8 满了库侧直接
// 判「输出通道不收」而终止配对，查自 WM/qrchan.go:100-107）。
//
// 扇出只投「当时在册」的订阅者，而 StartPairing 返回后到订阅方连上来之间必然有空窗
// （业务链路就是「拿 session id → 前端再开 WebSocket」），这期间发出的事件若不补投，
// 迟到者会一直停在「等二维码」上直到下一轮轮换（约 20-60 秒），终局事件更会永久丢失。
// 所以这里留住最近一条事件（last/hasLast），Subscribe 时补投一次。
type QRSession struct {
	ID string

	cancel context.CancelFunc
	// done 在泵协程退出（会话已清理）时关闭。
	done chan struct{}

	mu     sync.Mutex
	subs   map[chan QREvent]struct{}
	closed bool
	// last 是最近一条已发出事件；hasLast 标记它是否存在（首个事件之前的零值不可当事件用）。
	last      QREvent
	hasLast   bool
	pairedJID string
	// sess 是本次配对持有的一次性会话（startPairing 建会话时填入）。
	// 码配对在泵里读到首个 code 项后经它调 PairPhone；QR 扫码流程不读这个字段。
	sess waPairSession

	// pairPhone 非空即手机号配对码模式：泵在首个 code 项就地 PairPhone。
	// pairResult 是给 WhatsappGetPairingCode 的同步出口（缓冲 1），先入通道再广播，
	// 扇出丢帧不影响门面返回值。
	pairPhone  string
	pairMasked string
	pairCtx    context.Context
	pairResult chan pairCodeResult
	pairIssued bool
}

// pairCodeResult 是码配对泵写给门面的一次结果。
type pairCodeResult struct {
	code string
	err  error
}

// hasIssuedPairCode 码是否已经交给门面（成功或失败都算已结算）。
// 已结算的同号会话允许被下一次 GetPairingCode 替换；未结算的第二呼仍拒绝。
func (s *QRSession) hasIssuedPairCode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pairIssued
}

// completePairCode 结算一次码配对：落「已出码」记账 → 扇出这条终帧 → 把结果交给同步门面。
//
// 三步顺序都不能倒：
//   - 记账先于扇出：同号第二呼靠 hasIssuedPairCode 判「码已交出、允许替换」，
//     反了会把已拿到码的号码继续当成「握手未完成」而拒绝替换；
//   - 扇出先于交付：门面一返回就对业务方宣告「码已到手／已失败」，此时终帧必须
//     已经进了事件流。否则迟到订阅者补投到的是上一条二维码帧，而业务方「先收到
//     同步返回、后收到 QR 帧」的次序会随调度抖动（真机 -race 连跑可复现）。
//
// 只生效一次（pairIssued 闸门）。扫码会话没有同步通道（pairResult==nil），不在本函数范围内。
func (s *QRSession) completePairCode(ev QREvent, code string, err error) {
	if s.pairResult == nil {
		return
	}
	s.mu.Lock()
	if s.pairIssued {
		s.mu.Unlock()
		return
	}
	s.pairIssued = true
	s.mu.Unlock()
	s.emit(ev)
	select {
	case s.pairResult <- pairCodeResult{code: code, err: err}:
	default:
	}
}

// Subscribe 登记一个订阅者，返回事件通道与退订函数。
//
// 统一口径是「拿到的第一条一定是最近一条事件」：会话还在跑就补投后继续收新事件，
// 已结束则补投终局那一条后关闭通道——只丢一个已关闭的通道会让迟到方分不清
// 「配对失败」与「没赶上」，排障时无从下手。没发过任何事件时（hasLast 为假）
// 仍然直接给关闭通道，订阅方按「读到 !ok 即散场」的同一口径处理。
//
// 补投与登记在同一个锁内完成，且补投先于登记：这样并发 emit 时要么走这里补投
// （当时还没登记，不会重复），要么走扇出（hasLast 那时还是 false），两条路互斥。
func (s *QRSession) Subscribe() (<-chan QREvent, func()) {
	ch := make(chan QREvent, 16)
	s.mu.Lock()
	if s.hasLast {
		ch <- s.last
	}
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

// Cancel 终止配对：ctx 取消 → 库关 QR 通道并断连 → 泵协程收尾清理。可重复调用。
func (s *QRSession) Cancel() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Done 返回泵协程的退出信号（会话已从活动表摘除、订阅通道已全部关闭）。
func (s *QRSession) Done() <-chan struct{} { return s.done }

// PairedJID 返回配对成功后的账号 JID；尚未成功为空串。
func (s *QRSession) PairedJID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pairedJID
}

// emit 向全部订阅者非阻塞投递，并按 kind=QR 广播给回调订阅方。
func (s *QRSession) emit(ev QREvent) {
	ev.SessionID = s.ID
	publishWA("QR", &ev)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 留住最近一条，供随后的 Subscribe 补投（类型注释里的空窗问题）。
	s.last = ev
	s.hasLast = true
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			// 订阅者消费不动：丢这一条。轮换码场景下下一条很快覆盖它；
			// 这里若阻塞，泵协程停摆会把库通道憋到强制终止配对（见类型注释）。
		}
	}
}

// finish 关闭全部订阅通道并落 closed 标记（幂等）。
func (s *QRSession) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for ch := range s.subs {
		close(ch)
		delete(s.subs, ch)
	}
}

// waPairSession 是配对用一次性 Client 的期望面（未导出 seam，测试注入假实现）。
type waPairSession interface {
	// QRChannel 即库的 GetQRChannel：必须在 Connect 之前调用。
	QRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	Connect() error
	Disconnect()
	Device() *store.Device
	// PairPhone 即库的 Client.PairPhone（pair-code.go:92）：手机号配对码入口，
	// 前置是连接已建立且 QR 通道首个 "code" 项已出现（pair-code.go:78-81）。
	// 只由 runPairing 在码配对模式的首个 code 项上调用。
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
	// LoggedIn 即库的 IsLoggedIn：配对 socket 被服务端拆掉之后，要用已落库凭据再 Connect 一次，
	// 这个标记变真才表示手机端会离开「正在登录」。
	LoggedIn() bool
}

// waPairSessionFactory 是「怎么造配对会话」的挂点：默认真实现，测试整体替换、用完还原。
var waPairSessionFactory = newWaPairConn

// waPairConn 是 waPairSession 的真实现。
type waPairConn struct{ cli *whatsmeow.Client }

func newWaPairConn(dev *store.Device, log waLog.Logger) waPairSession {
	cli := whatsmeow.NewClient(dev, log)
	// 关掉库的自动重连：配对期断线（二维码耗尽即 timeout）是终局而不是可重试故障，
	// 让库自己连回去只会烧出一串没人消费的二维码。
	cli.EnableAutoReconnect = false
	return &waPairConn{cli: cli}
}

func (c *waPairConn) QRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	return c.cli.GetQRChannel(ctx)
}
func (c *waPairConn) Connect() error        { return c.cli.Connect() }
func (c *waPairConn) Disconnect()           { c.cli.Disconnect() }
func (c *waPairConn) Device() *store.Device { return c.cli.Store }

// PairPhone 转发到库的 Client.PairPhone（pair-code.go:92），本包不加任何语义。
func (c *waPairConn) PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error) {
	return c.cli.PairPhone(ctx, phone, showPushNotification, clientType, clientDisplayName)
}
func (c *waPairConn) LoggedIn() bool { return c.cli.IsLoggedIn() }

// waPairReloginSettle 是重登成功后再留一会儿才拆连接的时间。
// 配对确认后服务端会拆掉配对 socket（pair.go 的 expectDisconnect），手机要等这只设备
// 用落库凭据重新登录才结束「正在登录」。拆得太快，预密钥还没传完，手机就停在那一屏，
// 之后再用数据库里的设备行去登也会失败。测试把它置 0。
var waPairReloginSettle = 2 * time.Second

var _ waPairSession = (*waPairConn)(nil)

// StartPairing 起一次新的 QR 配对，返回进行中的会话。
//
// 会话生命周期不挂在调用方 ctx 上：发起配对的多半是一次 HTTP/WS 请求，请求返回时
// ctx 即取消——若拿它当配对 ctx，二维码还没被扫就死了。ctx 形参只是门面惯例的
// 占位（调用方传 nil 也合法），配对会话跟着管理器运行上下文 + waQRTTL 走，
// 提前终止用 QRSession.Cancel()。
func (m *waManager) StartPairing(ctx context.Context) (*QRSession, error) {
	_ = ctx // 刻意不参与会话生命周期，理由见上
	return m.startPairing("")
}

// startPairing 建一次配对会话。phone 为空走扫码；非空则同一条泵在首个 code 项就地 PairPhone。
func (m *waManager) startPairing(phone string) (*QRSession, error) {
	dev := m.store.NewDevice()
	if dev == nil {
		return nil, errors.New("whatsapp 配对失败: 会话库未能创建设备")
	}
	sess := waPairSessionFactory(dev, m.log.Sub("pairing"))
	sessionCtx, cancel := context.WithTimeout(m.runCtx, waQRTTL)
	// 必须先 QRChannel 再 Connect：库在已连接时返回 ErrQRAlreadyConnected
	//（查自 WM/qrchan.go:221-228），顺序错了拿到的就不是二维码而是错误。
	qrChan, err := sess.QRChannel(sessionCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("whatsapp 配对失败: %w", err)
	}
	id, err := newQRSessionID()
	if err != nil {
		cancel()
		return nil, err
	}
	s := &QRSession{
		ID:         id,
		cancel:     cancel,
		done:       make(chan struct{}),
		subs:       make(map[chan QREvent]struct{}),
		sess:       sess,
		pairPhone:  phone,
		pairMasked: waMaskPhone(phone),
		pairCtx:    sessionCtx,
	}
	if phone != "" {
		s.pairResult = make(chan pairCodeResult, 1)
	}
	m.qr.Store(id, s)
	m.wg.Add(1)
	go m.runPairing(s, sess, qrChan, dev)
	if phone != "" {
		vars.Info("WhatsApp 配对码会话已创建: session=%s phone=%s（%s 内有效）", id, s.pairMasked, waQRTTL)
	} else {
		vars.Info("WhatsApp QR 配对会话已创建: session=%s（%s 内有效）", id, waQRTTL)
	}
	return s, nil
}

// LookupQRSession 按 id 取进行中的配对会话（WebSocket 端点用）。
func (m *waManager) LookupQRSession(id string) (*QRSession, bool) {
	v, ok := m.qr.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*QRSession), true
}

// runPairing 是配对会话的泵协程：连接、消费 QR 通道直到库关闭它、然后清理。
// 清理无条件执行（defer）：成功、失败、超时、取消四种终局都不许把会话留在活动表里，
// 否则活动表只涨不消，成了拿 id 也连不上的僵尸名录。
func (m *waManager) runPairing(s *QRSession, sess waPairSession, qrChan <-chan whatsmeow.QRChannelItem, dev *store.Device) {
	defer m.wg.Done()
	defer close(s.done)
	defer sess.Disconnect()
	defer func() {
		m.qr.Delete(s.ID)
		s.finish()
	}()
	defer func() {
		if s.pairPhone == "" || s.hasIssuedPairCode() {
			return
		}
		err := fmt.Errorf("whatsapp 配对码失败: 配对会话在首个二维码项之前结束（digits=%s）", s.pairMasked)
		s.completePairCode(QREvent{Event: whatsmeow.QRChannelEventError, Error: err.Error()}, "", err)
	}()

	if err := sess.Connect(); err != nil {
		s.emit(QREvent{Event: whatsmeow.QRChannelEventError, Error: "连接 WhatsApp 失败: " + err.Error()})
		vars.Warning("WhatsApp QR 配对连接失败: session=%s err=%v", s.ID, err)
		return
	}
	firstCode := false
	var paired *QREvent
	for item := range qrChan {
		ev := QREvent{Event: item.Event}
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			ev.Code = item.Code
			ev.TimeoutMS = item.Timeout.Milliseconds()
			s.emit(ev)
			if s.pairPhone != "" && !firstCode {
				firstCode = true
				code, err := sess.PairPhone(ctxOrBackground(s.pairCtx), s.pairPhone, waPairCodeShowPushNotification,
					waPairCodeClientType, waPairCodeClientDisplayName)
				if err != nil {
					err = waMaskErr(fmt.Errorf("whatsapp 配对码失败: PairPhone 被拒（digits=%s）: %w", s.pairMasked, err), s.pairPhone)
					s.completePairCode(QREvent{Event: whatsmeow.QRChannelEventError, Error: err.Error()}, "", err)
					return
				}
				s.completePairCode(QREvent{Event: QREventPairCode, Code: code}, code, nil)
				vars.Info("WhatsApp 配对码已生成: session=%s phone=%s（用户在手机上输码，与扫码共用同一条事件流）", s.ID, s.pairMasked)
			}
			continue
		case whatsmeow.QRChannelEventError:
			if item.Error != nil {
				ev.Error = item.Error.Error()
			}
		case whatsmeow.QRChannelSuccess.Event:
			// 凭据已落库，但服务端随即拆掉配对 socket（pair.go expectDisconnect）。
			// 先别对外报成功：等用这份库数据重登完成，手机才会离开「正在登录」。
			if dev.ID != nil {
				ev.JID = dev.ID.String()
				s.mu.Lock()
				s.pairedJID = ev.JID
				s.mu.Unlock()
				paired = &ev
				continue
			}
		}
		s.emit(ev)
	}
	if paired != nil {
		if m.reloginPaired(sess, dev) {
			vars.Info("WhatsApp 配对成功并已用落库凭据重登: session=%s jid=%s（未自动 StartClient）", s.ID, paired.JID)
			s.emit(*paired)
		} else {
			s.emit(QREvent{Event: whatsmeow.QRChannelEventError, Error: "配对凭据已落库，但用数据库数据重登失败"})
		}
	}
}

// reloginPaired 等配对后的登录完成。
//
// 库在配对确认后会收到 stream error 515，并自己 Disconnect 再 connect
//（connectionevents.go）。这段时间 socket 还在，再调 Connect 得到的是
// ErrAlreadyConnected，不是失败。若把这次错误当成终局，二维码和配对码都会被标成失败，
// 随后的 Disconnect 还会拆掉 515 正在进行的重连，手机就停在「正在登录」。
// 515 没来、socket 已经拆掉时，才由这里补 Connect，并在拆线窗口内重试。
// 不把这次连接登记进 clients 表：常驻上线仍由业务方显式 StartClient。
func (m *waManager) reloginPaired(sess waPairSession, dev *store.Device) bool {
	if sess == nil || dev == nil || dev.ID == nil {
		return false
	}
	jid := dev.ID.String()
	deadline := time.Now().Add(m.cfg.EffectiveConnectTimeout())
	needConnect := true
	var lastErr error
	for {
		if sess.LoggedIn() {
			if waPairReloginSettle > 0 {
				time.Sleep(waPairReloginSettle)
			}
			return true
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				vars.Warning("WhatsApp[%s] 用数据库凭据重登失败: %v", jid, lastErr)
			} else {
				vars.Warning("WhatsApp[%s] 用数据库凭据重登未在 %s 内完成", jid, m.cfg.EffectiveConnectTimeout())
			}
			return false
		}
		if needConnect {
			err := sess.Connect()
			switch {
			case err == nil || errors.Is(err, whatsmeow.ErrAlreadyConnected):
				// nil：握手已发起。AlreadyConnected：515 重连已经占着 socket。都等 IsLoggedIn。
				needConnect = false
				lastErr = nil
			default:
				lastErr = err
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// newQRSessionID 生成不可猜测的会话 id：它同时是 WebSocket 端点的寻址键，
// 顺序号或时间戳形态等于把「谁在配对」暴露给任何能连上端点的人。
// crypto/rand 失败时 fail-closed 返回错误，不降级成弱随机。
func newQRSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("whatsapp 配对失败: 无法生成会话 id: %w", err)
	}
	return "qr-" + hex.EncodeToString(buf), nil
}
