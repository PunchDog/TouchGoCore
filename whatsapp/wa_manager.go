package whatsapp

// 本文件是消息侧的多账号会话管理器：一个已配对账号对应一个 whatsmeow.Client，
// 每个 Client 由一条常驻协程守着（连接、断线退避重连、logged_out 收手），
// 生命周期范式照抄 telegram/telegram.go——每轮连接独立 cancel、停机走 stop/done
// 双通道、等待有上限，不被一条外队长连接拖死整机退出。
//
// 并发口径（写死在这里，门面注释同步引用）：同一个 Client 的 SendText/SendAppState
// 可并发调用（whatsmeow 底层有请求表锁与 socket 锁，实践可行；但库对多 goroutine
// 安全零表述，这不是契约）；StartClient/StopClient/StartPairing/DeleteDevice
// 属改状态动作，管理器内部用 sync.Map 的原子 LoadOrStore/LoadAndDelete 兜住
// 「不 panic、不双启」，但「先停再启」这类跨调用时序仍要调用方自己串行化。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"touchgocore/config"
	"touchgocore/vars"
)

// waStopWaitBudget 是停机等待单个常驻协程退出的上限。
//
// 压在框架给单个服务的 2 秒最小停机预算（app.go:325 minServiceStopBudget）之内：
// 超了就记日志放手，让整机继续走完退出路。
//
// DeleteDevice（wa_delete_device.go）复用 StopClient，因此也吃这一个预算；
// 但那条路不在整机停机路径上，超时只当「非致命残留」上报，不必再对 2 秒做算术。
const waStopWaitBudget = 1500 * time.Millisecond

// waDeviceStore 是管理器对会话存储的全部期望面（未导出 seam）。
//
// 存在的理由与 waSession 相同：测试不连真 Postgres 也能把管理器跑完；
// 真实现是 *WAStore（store_postgres.go），方法签名逐字照抄 sqlstore.Container
// 的查证结果（GetDevice 查无返回 nil,nil；DeleteDevice 收 *store.Device）。
type waDeviceStore interface {
	GetDevice(ctx context.Context, jid types.JID) (*store.Device, error)
	AllDevices(ctx context.Context) ([]*store.Device, error)
	NewDevice() *store.Device
	DeleteDevice(ctx context.Context, dev *store.Device) error
	Close() error
}

// SessionInfo 是一个在跑会话的可公开事实（扩展集）。
//
// 字段表即纪律：只有账号标识与连接状态，没有任何密钥材料。
// LastSeen 的口径是「本进程最近一次收到该账号任何事件的时刻」，不是 WhatsApp 的
// 在线状态：库里 events.Presence 有 LastSeen 字段（查自 WM/types/events/events.go:470-477），
// 但它只在订阅了对端 presence 且对端没隐藏最后在线时间时才有值，拿它当「会话活着」
// 的判据会大面积零值；events.Battery 在这一版库里根本不存在（全仓 grep 无此类型），
// 计划里的 Battery/UnreadCount 两项因此不落地——库里没有消息表，未读数无源可查。
type SessionInfo struct {
	JID      string
	Platform string
	PushName string
	// IsConnected 取自 Client.IsLoggedIn()（底层是 atomic.Bool，查自 WM/client.go:66）。
	IsConnected bool
	LastSeen    time.Time
}

// waManager 管理一组 whatsmeow Client 与进行中的 QR 配对会话。
type waManager struct {
	store waDeviceStore
	cfg   *config.WhatsappSessionsConfig
	log   waLog.Logger
	// runCtx 是整机运行上下文（WhatsappStart 传入）：只用于查库这类短操作，
	// 连接循环每轮自建带超时的 ctx，不共享这一份（照 telegram 的每轮独立 cancel 纪律）。
	runCtx context.Context

	clients sync.Map // map[jid string]*waClientWrapper
	qr      sync.Map // map[sessionID string]*QRSession
	// restarts 是「僵尸会话重建」的按键互斥锁：liveWrapper 会摘掉死对象再起一个新的，
	// 而门面是并发可调的（同账号可并发发信），不锁住就会有两个重建者互相抢 key。
	restarts sync.Map // map[jid string]*sync.Mutex

	// stateSync 是「联系人 app state 同步」的按键单飞与节流账（wa_contacts.go）：
	// key -> *waContactSync。库侧的 appStateSyncLock 只串行化、不去重，两个并发的
	// full 拉取会各自 DeleteAppStateVersion 一次、互踩版本，所以本包自己在上面再压一层。
	stateSync sync.Map // map[jid string]*waContactSync

	// wg 覆盖全部常驻协程：每 Client 一条 run + 每配对会话一条 pump。
	// StopAll 关完信号后按预算等它归零，等不到就放手（协程会自行收尾）。
	wg sync.WaitGroup

	// afterFor 是退避时钟的注入点：nil 时用 time.After，测试塞假时钟断言退避序列。
	// 挂在管理器上而不是 wrapper 上：wrapper 由 StartClient 构造，测试没有别的时机递进去。
	afterFor func(time.Duration) <-chan time.Time
}

// waClientWrapper 包住一个 Client 的会话与常驻协程控制。
type waClientWrapper struct {
	jid  types.JID
	dev  *store.Device
	sess waSession
	cfg  *config.WhatsappSessionsConfig

	stop chan struct{} // 关闭即要求常驻协程退出
	done chan struct{} // 常驻协程已退出
	// kick 是「催一次立即重连」的令牌：门面侧 reconnectAndAwait 在非登录态下必发
	//（waitUntilDown 与 waitLoggedIn 两支都消费它），所以它是发信/读面自愈的承重件，
	// 不是备用挂点。缓冲 1 ⇒ 多次催合并成一次，不会攒令牌。
	kick   chan struct{}
	downed chan struct{} // 事件回调塞断线令牌，常驻协程据此进入退避重连
	// loggedOut 置位后不再重连：会话已被吊销，重试只会一直失败，等重新扫码配对。
	loggedOut atomic.Bool
	// saltSynced 标记本轮连接是否已经跑过 regular_high 快路同步。断线后清掉，重连再跑。
	// 注意它只闸「同步动作」不闸「盐在不在」——凭证在不在库由 wa_reachout.go 逐次读库判定。
	saltSynced atomic.Bool
	// offlinePending 为真表示连接后还有离线回放（里面可能带着会话凭证）。
	offlinePending atomic.Bool
	// histSeen 累计收到的 HistorySync 事件数：轮询落空记账之后用它判断
	// 「之后有没有新历史到达」，决定下一条发送值不值得再等一轮。
	histSeen atomic.Uint64

	// reachMu 串行化「外联凭证获取」的冷路径：增量/全量 regular_high 与向手机要历史
	// 都是贵操作，并发发送不得各发一份；凭证齐备的发送不拿这把锁。
	reachMu sync.Mutex
	// lastRefreshAt / lastFullAt / lastHistAt 是三类贵操作的节流账，
	// pollMissAt+pollMissSeen 是「等满一轮仍没凭证」的落空账（口径见 wa_reachout.go）。
	lastRefreshAt time.Time
	lastFullAt    time.Time
	lastHistAt    time.Time
	pollMissAt    time.Time
	pollMissSeen  uint64

	closeOnce sync.Once

	mu          sync.Mutex
	lastEventAt time.Time
	// sendWindow 是滚动一小时的发送时刻表（小时闸记账），与 lastEventAt 共用 mu。
	sendWindow []time.Time
	// lastSends 是本账号自己的发送账：会话 JID 串 → 最近一条发往该会话的 id+时间。
	// 删框时调用方留空 lastMsgID 就回退到这里（WhatsappDeleteChat）；只记本包
	// 发送路径发出去的消息，入站与其他端发的不在账上。
	lastSends map[string]waLastSend

	// gate 是 antiban 防封闸（wa_antiban.go）；nil 表示未启用，发送路径零变化。
	// gateMu 把「问闸→睡眠→真发→记账」整段串成单行道：库的 GroupRate 无自带锁、
	// BeforeSend/AfterSend 跨调用不原子，并发穿透会写错账。
	gate   waSendGate
	gateMu sync.Mutex

	// after 是退避睡眠的时钟挂点：生产为 time.After，测试注入假时钟断言退避序列。
	after func(time.Duration) <-chan time.Time
}

// newWaManager 构造管理器。cfg 允许为 nil（Effective* 全部 nil 安全）。
func newWaManager(st waDeviceStore, cfg *config.WhatsappSessionsConfig, log waLog.Logger, runCtx context.Context) *waManager {
	if runCtx == nil {
		runCtx = context.Background()
	}
	if log == nil {
		log = waLog.Noop
	}
	return &waManager{store: st, cfg: cfg, log: log, runCtx: runCtx}
}

// StartClient 为指定 JID 启动一个 Client（该账号必须已配对，即库里有设备行）。
//
// 幂等：同一 JID 已在跑时直接返回 nil，不起第二条协程、不建第二个连接——
// 两个 Client 抢同一个 Device 会互踢会话（WhatsApp 侧表现为 stream_replaced）。
func (m *waManager) StartClient(jid types.JID) error {
	key := jid.String()
	if _, running := m.clients.Load(key); running {
		return nil
	}
	dev, err := m.store.GetDevice(m.runCtx, jid)
	if err != nil {
		return fmt.Errorf("whatsapp 读取会话库失败: %w", err)
	}
	// 已配对的判定口径是 dev.ID != nil：sqlstore 对 ID 为 nil 的设备拒绝落库
	//（PutDevice 返回 ErrDeviceIDMustBeSet，查自 WM/store/sqlstore/container.go:239-241），
	// 因此库里查得到的行必带 ID；Initialized 只是「本进程是否已接上内部 store」的
	// 内存标记，不表示配对态，不拿它当判据。
	if dev == nil || dev.ID == nil {
		return fmt.Errorf("whatsapp 账号 %s 未配对（会话库里没有设备），先走 WhatsappStartPairing 扫码", key)
	}
	if max := m.cfg.EffectiveMaxConcurrent(); max > 0 && m.countClients() >= max {
		// 文案要与「门面按需自动拉起账号」的新语义对得上：懒加载下这个上限可能先被
		// 业务调用撞响，而不是调用方自己 StartClient 撞的，所以处置写清楚怎么腾额度。
		return fmt.Errorf("whatsapp 在跑会话数已达上限 %d（max_concurrent），先停一个再启新的（先用 WhatsappStopClient 停一个不用的账号）", max)
	}
	w := &waClientWrapper{
		jid:      jid,
		dev:      dev,
		sess:     waSessionFactory(dev, m.log.Sub(jid.User)),
		cfg:      m.cfg,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		kick:     make(chan struct{}, 1),
		downed:   make(chan struct{}, 1),
		after:    m.afterFor,
	}
	if m.cfg.EffectiveAntibanEnabled() {
		w.gate = waGateFactory(m.cfg, key)
	}
	w.sess.OnEvent(w.onEvent)
	if _, loaded := m.clients.LoadOrStore(key, w); loaded {
		// 并发窗口里另一个调用已经启动了这个账号：丢弃自己这份，幂等返回。
		return nil
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		w.run()
	}()
	vars.Info("WhatsApp 会话已启动: jid=%s platform=%s", key, dev.Platform)
	return nil
}

// countClients 数当前在跑的 Client（MaxConcurrent 闸用）。
func (m *waManager) countClients() int { return waMapLen(&m.clients) }

// waMapLen 数 sync.Map 的条目（标准库没有 Len）。
func waMapLen(sm *sync.Map) int {
	n := 0
	sm.Range(func(_, _ any) bool { n++; return true })
	return n
}

// waGetOrNew 取 key 上的 T，缺失时建一份；并发下返回抢到先手的那一份
// （sync.Map.LoadOrStore 的既有语义，不重复建）。
func waGetOrNew[T any](sm *sync.Map, key string, mk func() T) T {
	if v, ok := sm.Load(key); ok {
		return v.(T)
	}
	v, _ := sm.LoadOrStore(key, mk())
	return v.(T)
}

// withinWindow 报告「距 t 还没过 window」；t 为零时刻视为从没做过，返回 false，
// 所以各节流账的零值天然放行第一次动作。
func withinWindow(t time.Time, window time.Duration) bool {
	return !t.IsZero() && time.Since(t) < window
}

// StopClient 停掉指定账号的常驻协程并断开连接，等 done 或超时。
// 未启动返回明确错误而不是静默成功——调用方拿它做「确保停了」的判据。
func (m *waManager) StopClient(jid types.JID) error {
	key := jid.String()
	v, ok := m.clients.LoadAndDelete(key)
	if !ok {
		return fmt.Errorf("whatsapp 账号 %s 没有在跑的会话", key)
	}
	w := v.(*waClientWrapper)
	w.closeOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
		return nil
	case <-time.After(waStopWaitBudget):
		vars.Warning("WhatsApp[%s] 未在 %s 内退出，放弃等待（常驻协程会自行收尾）", key, waStopWaitBudget)
		return fmt.Errorf("whatsapp 账号 %s 停机等待超时", key)
	}
}

// StopAll 停掉全部 Client 与进行中的配对会话，按预算等所有常驻协程退出。
//
// 先把所有 stop 一次性关完再统一等：逐个「关一个等一个」在 N 个账号时
// 会把 N×预算烧完，整机 2 秒的服务停机预算根本不够分。
func (m *waManager) StopAll(budget time.Duration) {
	m.clients.Range(func(_, v any) bool {
		if w, ok := v.(*waClientWrapper); ok {
			w.closeOnce.Do(func() { close(w.stop) })
		}
		return true
	})
	m.clients.Range(func(k, _ any) bool { m.clients.Delete(k); return true })
	m.qr.Range(func(_, v any) bool {
		if s, ok := v.(*QRSession); ok {
			s.Cancel()
		}
		return true
	})
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(budget):
		vars.Warning("WhatsApp 消息侧未在 %s 内全部退出，放弃等待（协程会自行收尾）", budget)
	}
}

// ListClients 返回全部在跑会话的公开事实，按 JID 排序（sync.Map 遍历序是乱的，
// 不排序则同一份状态两次调用给出两种顺序，下游没法做快照比对）。
func (m *waManager) ListClients() []SessionInfo {
	out := make([]SessionInfo, 0)
	m.clients.Range(func(_, v any) bool {
		w := v.(*waClientWrapper)
		w.mu.Lock()
		last := w.lastEventAt
		w.mu.Unlock()
		out = append(out, SessionInfo{
			JID:         w.jid.String(),
			Platform:    w.dev.Platform,
			PushName:    w.dev.PushName,
			IsConnected: w.sess.LoggedIn(),
			LastSeen:    last,
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].JID < out[j].JID })
	return out
}

// restartLock 取（并惰性建）某个账号的重建锁。
func (m *waManager) restartLock(key string) *sync.Mutex {
	return waGetOrNew(&m.restarts, key, func() *sync.Mutex { return &sync.Mutex{} })
}

// ensureLoggedIn 是全部读写门面在非登录态下的第三层自愈：在册且活着就等连上，
// 僵尸由 liveWrapper 复活，**压根不在册的账号先按需 StartClient 再等连上**。
//
// ensureAlive 那一层只管「已经在 clients 表里」的账号，而下游
// 普遍是先绑定账号、后续直接调门面，中间并没有「显式启动」这一步：
// liveWrapper 对不在册的 key 直接报「没有在跑的会话」，而 StartClient 的幂等分支
// 对「在册但未连」又什么都不做，两头都接不住，外部仓的 ensureClient 因此空转
// （症状是配对码绑的号「列好友 = 0、可发 = 0」，看起来像发不出去）。
//
// 与「配对成功不自动上线」裁决不冲突：那条裁决的对象是「配对成功那一刻自动拉起整库
// N 个账号」，这里是**按调用按需拉一条**，账号只在业务真要用它的时候上线，
// 「谁该长期在跑」的决策权仍在显式 WhatsappStartClient 手上。
//
// 错误串口径：自动启动失败时把既有文案**前缀化**而不是替换，保留「没有在跑」子串
// （wa_client_test.go 有六条门面断言钉这个关键词）；库里没有设备行时 StartClient 的
// 「未配对」原文照常带出，处置仍是去扫码。
func (m *waManager) ensureLoggedIn(ctx context.Context, fromJID string) (*waClientWrapper, error) {
	jid, err := waAccountJID(fromJID)
	if err != nil {
		return nil, fmt.Errorf("whatsapp 账号 JID 不可用: %w", err)
	}
	key := jid.String()
	if _, ok := m.clients.Load(key); !ok {
		if e := m.StartClient(jid); e != nil {
			return nil, fmt.Errorf("whatsapp 账号 %s 没有在跑的会话，自动启动失败: %w", key, e)
		}
	}
	w, err := m.liveWrapper(key, jid)
	if err != nil {
		return nil, err
	}
	if err := w.reconnectAndAwait(ctx); err != nil {
		return nil, err
	}
	return w, nil
}

// ensureAlive 只保证端出来的不是死对象（僵尸就复活重建），**不等连接建立**。
//
// 给「读本地会话库、命中就不必等登录」的门面用（查好友的本地读那一支，见
// WhatsappListContacts 的两段式）：这类调用也去等 15 秒登录，等于把一次本地读换成
// 网络等待；但死对象上的子存储已被库换成 NoopStore（WM/store/store.go 的 Delete 收尾），
// 不复活就连本地读都做不了。
//
// 需要在线才能办成事的读写面走 ensureLoggedIn（不在册会先按需启动）。
// wa_reconnect_test.go 的 TestListContactsRevivesWithoutWaiting 钉着本函数这条
// 「摘死对象但不等登录」的语义。
//
// 入参一律先过 waAccountJID：配套设备形态的 "号码:1@s.whatsapp.net" 必须放行，
// 设备行主键与 wrapper 键都带设备段，抹掉就再也查不到行。
func (m *waManager) ensureAlive(fromJID string) (*waClientWrapper, error) {
	jid, err := waAccountJID(fromJID)
	if err != nil {
		return nil, fmt.Errorf("whatsapp 账号 JID 不可用: %w", err)
	}
	return m.liveWrapper(jid.String(), jid)
}

// liveWrapper 返回一份「还活着」的 wrapper：健康就原样返回，死透就摘掉重建。
// 不催连、不等连——那两件事分别归 reconnectAndAwait 与调用方。
func (m *waManager) liveWrapper(key string, jid types.JID) (*waClientWrapper, error) {
	v, ok := m.clients.Load(key)
	if !ok {
		return nil, fmt.Errorf("whatsapp 账号 %s 没有在跑的会话（先 WhatsappStartClient，未配对先 WhatsappStartPairing）", key)
	}
	w := v.(*waClientWrapper)
	if !w.retired() {
		return w, nil
	}

	// 死对象重建：先锁后双检——并发里可能已经有别人换过一份活的，那就不必再摘。
	mu := m.restartLock(key)
	mu.Lock()
	defer mu.Unlock()
	if v2, ok := m.clients.Load(key); ok {
		w2 := v2.(*waClientWrapper)
		if !w2.retired() {
			return w2, nil
		}
		// StopClient 对已退出的 wrapper 是即刻返回的（done 已关），
		// 对「刚收到吊销事件、协程还在收尾」的则按 1.5 秒预算等它把 socket 拆干净。
		if serr := m.StopClient(jid); serr != nil {
			return nil, fmt.Errorf("whatsapp 账号 %s 摘除旧会话失败: %w", key, serr)
		}
	}
	if err := m.StartClient(jid); err != nil {
		return nil, fmt.Errorf("whatsapp 账号 %s 重登失败: %w", key, err)
	}
	v3, ok := m.clients.Load(key)
	if !ok {
		return nil, fmt.Errorf("whatsapp 账号 %s 重登后会话不在册（并发被摘除，请重试）", key)
	}
	return v3.(*waClientWrapper), nil
}

// retired 报告这具 wrapper 是否已经不可能再自己连上：已落 logged_out 标记，
// 或常驻协程已退出（done 关闭）。
//
// 「主动停掉的账号」不会走到这里：StopClient 是 LoadAndDelete 先摘 key 再关 stop，
// 所以还挂在 map 上却已经 done 关闭的，只可能是吊销或协程自己收手——正是发信面需要
// 复活的那一档。
func (w *waClientWrapper) retired() bool {
	if w.loggedOut.Load() {
		return true
	}
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// reconnectAndAwait 是「要在线才能办成事」的门面共用的自愈前奏：
// 已经连着就直接返回（快路，零成本），否则催一次立即重连再等连上。
//
// 批量发信在每条之前都调它一次：一批几十条的时间里 socket 可能中途掉，
// 逐条自愈才能让后面的条目不必全部撞在同一个断口上。
// sessionReady 是「现在就能发信」：已登录，且真连接还在。
// 库的 IsLoggedIn 在断线后不会自动清掉（onDisconnect 不改这个标记），只看它会把
// 一条已拆的 socket 当成在线，SendText 就挂到调用方 deadline。
func sessionReady(s waSession) bool {
	return s != nil && s.LoggedIn() && s.Connected()
}

func (w *waClientWrapper) reconnectAndAwait(ctx context.Context) error {
	if sessionReady(w.sess) {
		return nil
	}
	w.kickNow()
	return w.waitConnected(ctx)
}

// kickNow 非阻塞地催常驻协程立刻试一轮连接（退避睡眠会被这个令牌提前叫醒）。
func (w *waClientWrapper) kickNow() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// waitConnected 等本账号进入登录态，上限 EffectiveConnectTimeout。
//
// 判据用 sess.LoggedIn() 而不是「Login 返回 nil」：ConnectContext 只代表 socket 与
// Noise 握手完成，凭据要被服务端接受才算连上（见 waitLoggedIn 的真机实测注）。
// 每 100ms 轮询：登录本身是库的异步事件派发，没有可等的通道。
func (w *waClientWrapper) waitConnected(ctx context.Context) error {
	if sessionReady(w.sess) {
		return nil
	}
	ctx = ctxOrRun(ctx)
	deadline := time.NewTimer(w.cfg.EffectiveConnectTimeout())
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("whatsapp 账号 %s 等待连上时被取消: %w", w.jid.String(), ctx.Err())
		case <-w.done:
			if w.loggedOut.Load() {
				return fmt.Errorf("whatsapp 账号 %s 会话已被服务端吊销（logged_out），需重新扫码配对", w.jid.String())
			}
			return fmt.Errorf("whatsapp 账号 %s 的常驻协程已退出", w.jid.String())
		case <-deadline.C:
			return fmt.Errorf("whatsapp 账号 %s 在 %s 内未能连上", w.jid.String(), w.cfg.EffectiveConnectTimeout())
		case <-tick.C:
			if sessionReady(w.sess) {
				return nil
			}
		}
	}
}

// run 是单个 Client 的常驻协程：连上就守着，断了按退避重连，logged_out 收手。
//
// ctx 生命周期口径（真机实测出的坑）：whatsmeow 的 ConnectContext 把传入 ctx 直接
// 绑给 keepAliveLoop 与 handlerQueueLoop（WM/client.go:582-583），即这个 ctx 活多久
// 连接就活多久。所以这里只能用 WithCancel、在「本轮连接确认断开」之后才 cancel——
// Login 返回当场就 cancel 会把手势还没走完的连接立刻拆掉，WithTimeout 则会把
// 健康连接在超时点准时掐死，两者都不是「每轮独立」的本意。
func (w *waClientWrapper) run() {
	defer close(w.done)
	defer w.sess.Disconnect()

	minBackoff := w.cfg.EffectiveReconnectMin()
	maxBackoff := w.cfg.EffectiveReconnectMax()
	backoff := minBackoff

	for {
		select {
		case <-w.stop:
			return
		default:
		}
		if w.loggedOut.Load() {
			// 会话已被吊销（重新配对前不可能再连上）：留一条日志收手，别把退避循环烧成噪音。
			vars.Info("WhatsApp[%s] 会话已吊销，常驻协程退出，不再重连", w.jid.String())
			return
		}

		// 每轮连接一个独立 ctx：本轮确认断开后才 cancel，
		// 旧协程不会改盯新一轮的信号而永久泄漏（telegram.go 同款纪律）。
		ctx, cancel := context.WithCancel(context.Background())
		err := w.sess.Login(ctx)
		if err != nil {
			cancel()
			vars.Warning("WhatsApp[%s] 连接失败: %v（%s 后重试）", w.jid.String(), err, backoff)
		} else if w.waitLoggedIn(ctx) {
			backoff = minBackoff
			vars.Info("WhatsApp[%s] 会话已连接", w.jid.String())
			// 连上之后守到「停 / 断线 / 催一次重连」三者之一，再作废本轮 ctx。
			w.waitUntilDown()
			cancel()
			continue
		} else {
			// socket 连上了但登录没在预算内确认（或中途被拆）：主动断开走退避，
			// 不留半开连接占着设备会话。
			w.sess.Disconnect()
			cancel()
			vars.Warning("WhatsApp[%s] 连接后 %s 内未完成登录（%s 后重试）", w.jid.String(), w.cfg.EffectiveConnectTimeout(), backoff)
		}

		// 没连上：按退避等下一轮，期间可被 stop 终止或被 kick 提前催。
		if w.sleepBackoff(backoff) {
			return
		}
		backoff = waNextBackoff(backoff, minBackoff, maxBackoff)
	}
}

// waitLoggedIn 等库把登录（ref iq 回执）落定：ConnectContext 返回 nil 只代表
// socket 与 Noise 握手完成，IsLoggedIn 要等服务器接受凭据才翻真（真机实测
// connect ok 时 loggedin=false，约 1 秒后才 Connected 事件）。
// 返回 false 表示登录预算内没等到、或本轮期间收到断线/停机/吊销/催重连信号。
//
// 必须听 w.kick（下面那支），这不是可选优化而是门面自愈的前提：这一档状态是
// 「socket 已连上、登录回执还没落定」，门面 reconnectAndAwait→kickNow 催的正是
// 「别守着这条半开连接，弃了重来」。不接的话：
//   - 门面的 waitConnected 与库侧的 waitLoggedIn 读的是同一个 EffectiveConnectTimeout，
//     而库侧永远比门面早起跑，于是库侧先到点——门面只能干等到自己到点，
//     白烧满预算报「未能连上」（wa_reconnect_test.go 三条用例的偶红真形：
//     15.00s 到点 + 成对出现的「连接后 15s 内未完成登录」日志）；
//   - 没人消费的令牌也不会丢，它一直躺在缓冲通道里，到下一轮 sleepBackoff 被白捡一次，
//     把「催一次立即重连」变成「库侧自己到点之后顺手重连」，时机完全不由门面决定。
//
// 返回 false 让 run() 走既有的 Disconnect + 退避重连路径；这里绝不能 return true——
// 半开连接不是已登录，那样会把一条没被服务端接受的连接当活的交出去。
func (w *waClientWrapper) waitLoggedIn(ctx context.Context) bool {
	deadline := time.After(w.cfg.EffectiveConnectTimeout())
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if w.sess.LoggedIn() {
			return true
		}
		select {
		case <-w.stop:
			return false
		case <-w.downed:
			return false
		case <-w.kick:
			return false
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// waNextBackoff 翻倍退避并夹在上界内（纯函数，测试直接断言序列，不依赖时钟）。
func waNextBackoff(cur, min, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	if next < min {
		next = min
	}
	return next
}

// waitUntilDown 阻塞到断线、被催重连、或收到停机信号。
func (w *waClientWrapper) waitUntilDown() {
	select {
	case <-w.stop:
	case <-w.downed:
	case <-w.kick:
	}
}

// sleepBackoff 睡一个退避周期；返回 true 表示期间收到停机信号，调用方应立即收尾。
func (w *waClientWrapper) sleepBackoff(d time.Duration) bool {
	after := w.after
	if after == nil {
		after = time.After
	}
	select {
	case <-w.stop:
		return true
	case <-w.kick:
		return false
	case <-after(d):
		return false
	}
}

// onEvent 是事件回调的总入口：先记 LastSeen 时刻，再翻成本包的广播载荷。
// 回调里绝不阻塞（whatsmeow 在事件派发协程里同步调用它）。
func (w *waClientWrapper) onEvent(evt any) {
	w.mu.Lock()
	w.lastEventAt = time.Now()
	w.mu.Unlock()
	w.dispatch(evt)
}

// dispatch 把库事件翻成广播载荷，并给常驻协程递信号。
//
// 库侧同一类事件既可能以指针派发也可能以值派发（查自 WM/client.go、
// connectionevents.go），这里每种事件的处置只写一份、两种形态共用一个 case：
// 漏接一类不会编译报错，只会让业务方收不到本该广播的事件，是难查的静默失败，
// 处置写两遍更容易改一漏一。只有需要读事件字段的几类才按形态分岔，
// 分岔两支仍落到同一个处置上（wa_client_test.go 的双形态用例钉这一点）。
func (w *waClientWrapper) dispatch(evt any) {
	account := w.jid.String()
	switch e := evt.(type) {
	case *events.Message:
		publishWA("Inbound", waInbound(account, e))
	case events.Message:
		publishWA("Inbound", waInbound(account, &e))
	case *events.Receipt:
		publishWA("Receipt", waReceipt(account, e))
	case events.Receipt:
		publishWA("Receipt", waReceipt(account, &e))
	case *events.Connected, events.Connected:
		publishWA("Connection", &ConnEvent{Account: account, State: "connected"})
	case *events.OfflineSyncPreview, events.OfflineSyncPreview:
		w.offlinePending.Store(true)
	case *events.OfflineSyncCompleted, events.OfflineSyncCompleted:
		w.offlinePending.Store(false)
	case *events.HistorySync, events.HistorySync:
		w.histSeen.Add(1)
		vars.Info("WhatsApp[%s] 收到历史同步（对方 tctoken/外联盐可能随之落地）", account)
	case *events.Disconnected, events.Disconnected:
		w.saltSynced.Store(false)
		w.offlinePending.Store(false)
		w.markDown()
		publishWA("Connection", &ConnEvent{Account: account, State: "disconnected"})
	case *events.StreamReplaced, events.StreamReplaced:
		// 同一会话被另一端顶替：不重连也连不上旧账号，先记原因。
		w.markDown()
		publishWA("Connection", &ConnEvent{Account: account, State: "stream_replaced", Reason: "同一会话被另一端顶替"})
	case *events.LoggedOut:
		w.evLoggedOut(account, e)
	case events.LoggedOut:
		w.evLoggedOut(account, &e)
	}
}

// evLoggedOut 处置吊销：落标记后常驻协程不再重连，终态广播带出库给的吊销原因。
func (w *waClientWrapper) evLoggedOut(account string, e *events.LoggedOut) {
	w.loggedOut.Store(true)
	w.markDown()
	publishWA("Connection", &ConnEvent{Account: account, State: "logged_out", Reason: e.Reason.String()})
}

// markDown 非阻塞地给常驻协程递一个断线令牌。
//
// 用带缓冲的 1 号通道而不是广播：一个令牌足够唤醒一次重连，事件回调里绝不能阻塞。
func (w *waClientWrapper) markDown() {
	select {
	case w.downed <- struct{}{}:
	default:
	}
}

// takeSendSlot 是单账号滚动一小时发送闸：窗口内已记账数 < max 才放行并记上 now。
//
// max<=0 读作「不限」（调用方已用 Effective* 归一，正常不会走到）；
// 拒绝时不记账，超限的那条就地失败，排队等下一小时不属于本包语义。
func (w *waClientWrapper) takeSendSlot(now time.Time, max int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-time.Hour)
	kept := w.sendWindow[:0]
	for _, t := range w.sendWindow {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.sendWindow = kept
	if max > 0 && len(w.sendWindow) >= max {
		return false
	}
	w.sendWindow = append(w.sendWindow, now)
	return true
}

// waLastSend 是发送账的一条：最近一次经本包发往某会话的消息 id 与服务器时间戳。
type waLastSend struct {
	msgID string
	ts    time.Time
}

// recordLastSend 记一笔发送账（发信成功后调，键为解析后的会话 JID 串）。
func (w *waClientWrapper) recordLastSend(chat, msgID string, ts time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastSends == nil {
		w.lastSends = make(map[string]waLastSend)
	}
	w.lastSends[chat] = waLastSend{msgID: msgID, ts: ts}
}

// lastSend 取某会话最近一条经本包发出的消息的 id+时间；没有记录时 ok=false。
func (w *waClientWrapper) lastSend(chat string) (string, time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.lastSends[chat]
	if !ok || v.msgID == "" {
		return "", time.Time{}, false
	}
	return v.msgID, v.ts, true
}

// errWaNotStarted 是消息侧未启动时的统一错误（门面 fail-closed 用）。
var errWaNotStarted = errors.New("whatsapp 消息侧未启动（未配置 whatsapp.sessions.dsn 或会话库不可达）")
