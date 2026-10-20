package zalo

// 本文件是 Zalo 扫码配对：QRSession（历史补投 + 扇出 + 终态唯一）、
// ZaloStartPairing 入口、zcago QR 泵，以及登录成功后的落库与 conn 安装。
//
// 已核实的 zcago v0.3.0 事实（session/auth/login_qr.go）：
//   - EventQRCodeGenerated.Data.Image 是**原始 PNG 字节**：handleQRCallback 里
//     qrData.Image = string(imgBytes)。出 JSON 前必须自己 base64.Encode，不能
//     假设它已经是 base64（否则前端拿到的"图片"是一串乱码）；
//   - setupQRTimeout 在 cb != nil 时只回调 EventQRCodeExpired，**不会自动 Retry**；
//     回调必须显式 e.Actions.Retry(ctx)，否则会话一直挂到 ctx 取消；
//   - Retry/Abort 内部都是「带 default 的 buffered channel send」，非阻塞；
//     本文件的 emit 同样非阻塞（订阅缓冲满时直接丢弃当前帧，绝不让泵协程阻塞）。
//
// 会话生命周期挂在 manager root + limits.qrTTL 上，不挂调用方 ctx：发起配对的
// 多半是一次 HTTP 请求，请求返回 ctx 即取消——若拿它当会话 ctx，二维码还没被扫
// 就死了。提前终止走 cancelQRByAccount / Close。
//
// 二维码 code/image 与 WS 寻址用的 sessionID 等同登录凭据：本文件不把它们写进
// 任何日志，终态事件里的 Error 也只给固定安全文案，详细错误只进 vars.Warning。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amrakk/zcago"
	"github.com/amrakk/zcago/session/auth"

	"touchgocore/util"
	"touchgocore/vars"
)

const (
	// qrSessionHistoryMax 是会话保留的最近事件数。二维码图约几十 KB/帧，留住
	// 最近 8 条足以覆盖「重试出新码 → 扫描 → 终局」的迟到补投，又不会无限涨。
	qrSessionHistoryMax = 8
	// qrSessionSubBuffer 是每个订阅者的缓冲：历史最多 8 条 + 新事件突发余量。
	qrSessionSubBuffer = 16
	// qrSessionMaxSubs 限制单会话的 WS 订阅数：sessionID 是唯一鉴权，限制并发
	// 订阅可避免一个持有者开大量连接把每个订阅的缓冲内存叠起来。
	qrSessionMaxSubs = 8

	// 事件名与 za_types.go 的 QREvent.Event 注释一致。
	qrEventQR       = "qr"
	qrEventScanned  = "scanned"
	qrEventExpired  = "expired"
	qrEventDeclined = "declined"
	qrEventSuccess  = "success"
	qrEventError    = "error"
	qrEventTimeout  = "timeout"

	// qrSafeLoginError 是扫码失败终态对外的唯一文案：事件会经 WebSocket 发给
	// 任意连上来的客户端，详细错误（可能含上游报文）只进 vars.Warning。
	qrSafeLoginError = "扫码登录失败，请重试"
)

// QRSession 是一次进行中的扫码登录，也是事件扇出的唯一入口。
//
// 扇出模型沿用 whatsapp/wa_pairing.go：zcago 的回调只在单条泵协程上跑，而
// WebSocket 端可能有多个订阅者（运维页 + 业务后台同时看一个码），Subscribe
// 各自拿一条带缓冲的通道。投递一律非阻塞：订阅者消费不动就丢当前帧——二维码
// 是轮换的，丢一帧旧码马上被新码覆盖；阻塞泵协程反而会拖住 zcago 的登录流程。
//
// 历史补投解决「StartPairing 返回 → 前端连上 WebSocket」之间的空窗：这期间
// 发出的二维码图片若不补投，迟到连接会一直停在「等二维码」直到下一轮过期重试
// （约 100 秒）；终态事件更会永久丢失。所以保留最近 qrSessionHistoryMax 条，
// Subscribe 先按顺序补投历史再登记新事件。
//
// 终态唯一：success/declined/error/timeout 只能有一个（terminal 标志），发出后
// 立即关闭全部订阅通道；expired 不是终态（zcago 会 Retry 出新码）。
type QRSession struct {
	ID        string
	AccountID string

	// ctx 由 parent(context) + ttl 派生；Close cancel 它来打断 zcago 的登录流程。
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	history  []QREvent
	subs     map[chan QREvent]struct{}
	closed   bool
	terminal bool
	onClose  func()

	closeOnce sync.Once
}

// newQRSession 构造会话。parent 为 nil 时用 Background；onClose 只会被调用一次
// （由 Close 保证），用于从 manager 的活动表里摘除本对象。
func newQRSession(id, accountID string, parent context.Context, ttl time.Duration, onClose func()) *QRSession {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, ttl)
	return &QRSession{
		ID:        id,
		AccountID: accountID,
		ctx:       ctx,
		cancel:    cancel,
		subs:      make(map[chan QREvent]struct{}),
		onClose:   onClose,
	}
}

// Subscribe 登记一个订阅者，返回事件通道与退订函数。
//
// 补投与登记在同一个锁内完成，且补投先于登记：并发 emit 要么走这里补投
// （当时还没登记，不会重复），要么走扇出（历史在这里之后才有），两条路互斥。
// 已终局/已关闭的会话补投完历史后直接关闭通道，让迟到方按「读到 !ok 即散场」
// 的同一口径收到终态，而不是分不清「失败」与「没赶上」。
func (s *QRSession) Subscribe() (<-chan QREvent, func()) {
	ch := make(chan QREvent, qrSessionSubBuffer)
	s.mu.Lock()
	for _, ev := range s.history {
		ch <- ev
	}
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	if len(s.subs) >= qrSessionMaxSubs {
		// 订阅数到顶：给一个「补完历史即结束」的通道，WS 端会按终局 close 1000
		// 收场，而不是为一条连接再分配缓冲。
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

// emit 投递一条非终态事件（qr/scanned/expired）。终态之后一律丢弃。
func (s *QRSession) emit(ev QREvent) {
	s.mu.Lock()
	if s.closed || s.terminal {
		s.mu.Unlock()
		return
	}
	s.prepareLocked(&ev)
	s.appendHistoryLocked(ev)
	s.fanoutLocked(ev)
	s.mu.Unlock()
	// 锁外广播：订阅回调里若反过来问本会话状态不会自锁。
	publishZAQR(&ev)
}

// emitTerminal 投递唯一一条终态事件（success/declined/error/timeout），随后关闭
// 全部订阅通道。重复调用是空操作——终态只发一次。
func (s *QRSession) emitTerminal(ev QREvent) {
	s.mu.Lock()
	if s.closed || s.terminal {
		s.mu.Unlock()
		return
	}
	s.terminal = true
	s.closed = true
	s.prepareLocked(&ev)
	s.appendHistoryLocked(ev)
	s.fanoutLocked(ev)
	for ch := range s.subs {
		close(ch)
		delete(s.subs, ch)
	}
	s.mu.Unlock()
	publishZAQR(&ev)
}

// publishZAQR 把 QR 事件同步广播到 util.CallZaloMsg+"QR"（口径同 whatsapp 的
// publishWA）。QREvent 的冻结注释承诺了这条事件面；WS 与回调拿到的载荷一致。
func publishZAQR(ev *QREvent) {
	util.DefaultCallFunc.Do(util.CallZaloMsg+"QR", ev)
}

// Close 终止会话：cancel ctx（打断 zcago 登录）→ 关闭全部订阅通道 → 调一次
// onClose。可重复调用；终态事件已发过的会话再 Close 也只会补上 onClose。
func (s *QRSession) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		s.closed = true
		for ch := range s.subs {
			close(ch)
			delete(s.subs, ch)
		}
		onClose := s.onClose
		s.mu.Unlock()
		if onClose != nil {
			onClose()
		}
	})
}

// isClosed 判断会话是否已经不再接收事件（终态或 Close）。
func (s *QRSession) isClosed() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// prepareLocked 填充会话标识与剩余 TTL（毫秒，负值折 0）。调用方必须持 s.mu
// （ctx 在构造后不可变，但统一在锁内读，避免字段可见性问题）。
func (s *QRSession) prepareLocked(ev *QREvent) {
	ev.SessionID = s.ID
	ev.AccountID = s.AccountID
	ev.TimeoutMS = 0
	if dl, ok := s.ctx.Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 {
			ev.TimeoutMS = ms
		}
	}
}

// appendHistoryLocked 追加历史并裁剪到最近 qrSessionHistoryMax 条。
func (s *QRSession) appendHistoryLocked(ev QREvent) {
	if len(s.history) >= qrSessionHistoryMax {
		copy(s.history, s.history[len(s.history)-qrSessionHistoryMax+1:])
		s.history[qrSessionHistoryMax-1] = ev
		return
	}
	s.history = append(s.history, ev)
}

// fanoutLocked 向全部在册订阅者非阻塞投递。满缓冲丢当前帧，绝不阻塞泵协程。
func (s *QRSession) fanoutLocked(ev QREvent) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ZaloStartPairing 发起一次扫码登录，返回 WebSocket 寻址用的会话 id。
//
// 校验与错误口径：未启动 → ErrZaloNotStarted；accountID 非法 →
// ErrZaloBadAccountID（由 accountOrCreate 校验）；账号已有在线 conn →
// ErrZaloAccountBusy。同账号已有未结束的扫码会话时返回原 sessionID（幂等），
// 避免前端重连时不停造新码把旧码顶掉。sessionID 用 crypto/rand 16 字节 hex：
// 它是 WS 端点的寻址键，顺序号/时间戳等于把「谁在配对」暴露给任何能连上来的人；
// 生成失败 fail-closed 返回错误，不降级成弱随机。
func ZaloStartPairing(_ context.Context, accountID string) (string, error) {
	m, err := currentManager()
	if err != nil {
		return "", err
	}
	a, err := m.accountOrCreate(accountID)
	if err != nil {
		return "", err
	}
	if a.currentConn() != nil {
		return "", fmt.Errorf("%w: 账号 %s 已有在线会话，先登出再扫码", ErrZaloAccountBusy, accountID)
	}
	if m.sdk == nil || m.store == nil {
		return "", ErrZaloNotStarted
	}

	m.mu.Lock()
	if m.stopping.Load() {
		m.mu.Unlock()
		return "", ErrZaloNotStarted
	}
	if old, ok := m.qrByAccount[accountID]; ok && !old.isClosed() {
		m.mu.Unlock()
		return old.ID, nil
	}
	id, err := newZAQRSessionID()
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	// onClose 捕获 s 本身而不是 id：旧会话收尾时不能误删同账号的新会话
	// （两者 accountID 相同、ID 不同），指针相等才说明「表里仍是这一个」。
	var s *QRSession
	s = newQRSession(id, accountID, m.root, m.limits.qrTTL, func() { m.forgetQRSession(s, accountID) })
	m.qrByID[id] = s
	m.qrByAccount[accountID] = s
	// wg.Add 在 m.mu 内：与 Stop 的 stopping 置位同锁，保证 Wait 不会先于 Add。
	m.wg.Add(1)
	m.mu.Unlock()

	vars.Info("Zalo 扫码会话已创建: account=%s（%s 内有效）", accountID, m.limits.qrTTL)
	go m.runQRSession(s, a)
	return id, nil
}

// LookupQRSession 按 id 取扫码会话（WebSocket 端点用）。
func (m *zaManager) LookupQRSession(id string) (*QRSession, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	s, ok := m.qrByID[id]
	m.mu.RUnlock()
	if !ok || s == nil {
		return nil, false
	}
	return s, true
}

// cancelQRByAccount 取消某账号的进行中扫码会话（cookie 导入/删除账号时调用）。
//
// 必须在锁外 Close：onClose 会回来拿 m.mu，持锁调用就是自锁死。
func (m *zaManager) cancelQRByAccount(accountID string) {
	m.mu.Lock()
	s := m.qrByAccount[accountID]
	if s != nil {
		if m.qrByAccount[accountID] == s {
			delete(m.qrByAccount, accountID)
		}
		if m.qrByID[s.ID] == s {
			delete(m.qrByID, s.ID)
		}
	}
	m.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// closeAllQRSessions 关闭全部扫码会话（manager 停机时调用）。
// 先在锁内快照并清表，再在锁外逐个 Close（理由同 cancelQRByAccount）。
func (m *zaManager) closeAllQRSessions() {
	m.mu.Lock()
	all := make([]*QRSession, 0, len(m.qrByID))
	for id, s := range m.qrByID {
		all = append(all, s)
		delete(m.qrByID, id)
	}
	for accountID := range m.qrByAccount {
		delete(m.qrByAccount, accountID)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close()
	}
}

// forgetQRSession 是会话 onClose：只在表里仍是本对象时删除（指针相等），
// 避免旧会话收尾误删同账号新会话。
func (m *zaManager) forgetQRSession(s *QRSession, accountID string) {
	if s == nil {
		return
	}
	m.mu.Lock()
	if m.qrByID[s.ID] == s {
		delete(m.qrByID, s.ID)
	}
	if m.qrByAccount[accountID] == s {
		delete(m.qrByAccount, accountID)
	}
	m.mu.Unlock()
}

// newZAQRSessionID 生成不可猜测的会话 id：crypto/rand 16 字节 hex（32 字符）。
// 失败 fail-closed，不回退时间戳/序列号。
func newZAQRSessionID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("zalo 扫码失败: 无法生成会话 id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// runQRSession 是扫码泵协程：跑 zcago.LoginQR 直到成功/终局/取消，然后无条件
// 关闭会话（defer）：成功、失败、超时、取消四种结局都不许把会话留在活动表里。
func (m *zaManager) runQRSession(s *QRSession, a *zaAccount) {
	defer m.wg.Done()
	defer s.Close()

	if m.sdk == nil {
		s.emitTerminal(QREvent{Event: qrEventError, Error: qrSafeLoginError})
		return
	}

	opt := &zcago.LoginQROption{
		UserAgent: m.limits.userAgent,
		Language:  m.limits.language,
	}
	var cb zcago.LoginQRCallback = func(ev auth.LoginQREvent) { m.handleQREvent(s, ev) }
	conn, err := m.sdk.LoginQR(s.ctx, opt, cb)

	// LoginQR 的返回按「ctx 状态优先」归类：TTL 到点是 timeout；停机/导入 cookie/
	// 删除账号取消的 ctx 不是扫码失败，直接收场，不给对端发误导性终态。
	switch {
	case errors.Is(s.ctx.Err(), context.DeadlineExceeded):
		s.emitTerminal(QREvent{Event: qrEventTimeout})
		return
	case s.ctx.Err() != nil:
		return
	case err != nil:
		if s.isClosed() {
			// declined 已发过终态（随后 Abort 让 zcago 以 aborted 错误返回）：
			// 这是用户主动拒绝，不是登录失败，不再记一条误导性 Warning。
			return
		}
		vars.Warning("Zalo 扫码登录失败 account=%s（详情已隐藏）", s.AccountID)
		s.emitTerminal(QREvent{Event: qrEventError, Error: qrSafeLoginError})
		return
	case conn == nil:
		vars.Warning("Zalo 扫码登录返回空连接 account=%s", s.AccountID)
		s.emitTerminal(QREvent{Event: qrEventError, Error: qrSafeLoginError})
		return
	}
	m.finishQRLogin(s, a, conn)
}

// handleQREvent 把 zcago 的 QR 回调映射成本包 QREvent。回调在 zcago 的泵上
// 同步执行，这里只做非阻塞 emit 与 Retry/Abort（两者内部都是 non-blocking send）。
func (m *zaManager) handleQREvent(s *QRSession, ev zcago.LoginQREvent) {
	if s == nil {
		return
	}
	switch e := ev.(type) {
	case auth.EventQRCodeGenerated:
		// Data.Image 是原始 PNG 字节（已核实），这里才做 base64。
		s.emit(QREvent{
			Event:       qrEventQR,
			ImageBase64: base64.StdEncoding.EncodeToString([]byte(e.Data.Image)),
			QRCode:      e.Data.Code,
		})
	case auth.EventQRCodeScanned:
		s.emit(QREvent{Event: qrEventScanned, DisplayName: e.Data.DisplayName, Avatar: e.Data.Avatar})
	case auth.EventQRCodeExpired:
		// expired 不是终态：zcago 只回调、不自动重试，必须显式 Retry 才会出新码。
		s.emit(QREvent{Event: qrEventExpired})
		if s.ctx.Err() == nil && !s.isClosed() {
			if err := e.Actions.Retry(s.ctx); err != nil {
				vars.Warning("Zalo 二维码过期后重试失败 account=%s（详情已隐藏）", s.AccountID)
			}
		}
	case auth.EventQRCodeDeclined:
		s.emitTerminal(QREvent{Event: qrEventDeclined})
		if err := e.Actions.Abort(s.ctx); err != nil {
			vars.Warning("Zalo 扫码被拒后终止失败 account=%s（详情已隐藏）", s.AccountID)
		}
	case auth.EventGotLoginInfo:
		// zcago 只在这里回报新生成的 IMEI/UA。落库必须用成功后的
		// SnapshotCredentials（会话当前值是唯一权威），这里不需要动作。
	}
}

// finishQRLogin 在扫码成功后落库并安装 conn；任一步失败都发 error 终态且不安装。
//
// 落库 ctx 用 manager root + requestTimeout，不用 s.ctx：TTL 恰好卡在成功瞬间时，
// s.ctx 已到期，拿它写库会把「刚成功、cookie 已刷新」的会话丢掉，重启后又得重扫。
func (m *zaManager) finishQRLogin(s *QRSession, a *zaAccount, conn zaConn) {
	// 会话已被取消/终局（并发 cookie 导入、删除账号、停机或回调已发终态）：
	// 不再安装，避免把一条已宣告结束的扫码会话变成在线连接。
	if s.isClosed() {
		return
	}

	a.opMu.Lock()
	var terminal *QREvent
	// 终态广播必须在释放 opMu 之后：publishZAQR 是同步回调，回调里若再调
	// Login/Logout（要 opMu）会自锁。
	defer func() {
		a.opMu.Unlock()
		if terminal != nil {
			s.emitTerminal(*terminal)
		}
	}()

	// 等 opMu 期间会话可能已被 cookie 导入/删除/停机取消（s.Close 会置 closed）：
	// 必须在这里复查，否则一个已取消的扫码会把并发 cookie 导入装好的 conn 顶掉。
	if s.isClosed() {
		return
	}
	// 本次扫码是新的登录代际：在飞登录在写库/安装前会被代际检查拒绝。
	// ensureConn 创建 flight 也要 opMu，因此本节内不会再有新 flight 插进来。
	a.mu.Lock()
	a.loginEpoch++
	a.mu.Unlock()

	// 停机/登出/删除竞态：不安装，也不发误导性终态（对端已被关闭）。
	if !a.canLogin() {
		return
	}
	if m.store == nil {
		terminal = &QREvent{Event: qrEventError, Error: qrSafeLoginError}
		return
	}

	snap, err := conn.SnapshotCredentials()
	if err != nil {
		vars.Warning("Zalo 扫码会话凭证快照失败 account=%s（详情已隐藏）", s.AccountID)
		terminal = &QREvent{Event: qrEventError, Error: qrSafeLoginError}
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		vars.Warning("Zalo 扫码会话凭证序列化失败 account=%s（详情已隐藏）", s.AccountID)
		terminal = &QREvent{Event: qrEventError, Error: qrSafeLoginError}
		return
	}

	saveCtx, cancel := context.WithTimeout(m.root, m.limits.requestTimeout)
	defer cancel()
	rec := zaSessionRecord{AccountID: a.id, UID: conn.UID(), Credentials: data}
	if err := m.store.Upsert(saveCtx, rec); err != nil {
		vars.Warning("Zalo 扫码会话凭证写库失败 account=%s（详情已隐藏）", s.AccountID)
		terminal = &QREvent{Event: qrEventError, Error: qrSafeLoginError}
		return
	}
	if !a.installConnLocked(conn) {
		// canLogin 通过之后 installConnLocked 仍可能因并发停机返回 false。
		return
	}
	uid := conn.UID()
	terminal = &QREvent{Event: qrEventSuccess, UID: uid}
}
