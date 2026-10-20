package zalo

// 本文件是 Zalo 多账号管理器：按需登录（单飞）、conn 生命周期、Stop、账号列表/
// 删除/登出、cookie 导入、好友/群/资料缓存封装与群版本差分。公开门面与扫码只调用
// 这里的名字，不直接碰 conn。
//
// 锁序固定 opMu → mu；info/缓存锁与它们互不嵌套。mu 内不做网络、不写库、不
// StopListener——mu 只保护 conn/uid/stopped/login/listenerCancel 这组字段，任何
// 阻塞动作都在释放它之后做。opMu 是「改 conn 的决策」串行锁：契约要求导入/登录
// 在其内完成写库与安装，因此 store.Upsert/installConnLocked 允许在持 opMu 时执行；
// 反过来，等待在飞登录时绝不能持 opMu，否则 runLogin 安装 conn 时抢 opMu 会互锁。
//
// 命名偏差：zaAccount 的资料缓存字段叫 infoCache 而不是 info——Go 不允许同名字段
// 与方法，而契约要求保留 a.info() 方法。
//
// QR 会话表与钩子由 za_pairing.go 实现。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrakk/zcago"
	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"
)

const (
	// zaMgrStopDrainBudget 是 Stop 等待后台协程退出的预算，压在 app 2s 停机预算内。
	zaMgrStopDrainBudget = 1500 * time.Millisecond
	// zaMgrStopSnapshotBudget 是停机时把活会话凭证快照写回库的超时。
	zaMgrStopSnapshotBudget = 2 * time.Second
)

// errZAMgrNilConn 表示 SDK 登录「成功」但没有交出连接。
// 把它当失败：nil conn 装进账号会让后续每次收发都 panic 或静默空转。
var errZAMgrNilConn = errors.New("zalo SDK 返回空连接")

// errZALoginSuperseded 表示这次在飞登录在等待期间被登出/删除/cookie 导入/扫码
// 成功取代：它的结果不能再写库或安装 conn，否则会覆盖后发起的会话。
var errZALoginSuperseded = errors.New("zalo 登录已被新的登录/登出取代")

// zaLimits 是 manager 运行时限频参数的内部快照，生产值来自
// config.ZaloSessionsConfig 的 Effective*（已做下限 clamp）。
type zaLimits struct {
	qrTTL          time.Duration
	requestTimeout time.Duration
	listener       bool
	friendsTTL     time.Duration
	groupsTTL      time.Duration
	infoMin        time.Duration
	infoTTL        time.Duration
	sendMin        time.Duration
	sendMax        time.Duration
	sendMaxPerHour int
	userAgent      string
	language       string
}

// zaLimitsFromConfig 逐项取 Effective*；c 为 nil 时全部走缺省值（方法本身 nil-safe）。
func zaLimitsFromConfig(c *config.ZaloSessionsConfig) zaLimits {
	return zaLimits{
		qrTTL:          c.EffectiveQRTTL(),
		requestTimeout: c.EffectiveRequestTimeout(),
		listener:       c.EffectiveListenerEnabled(),
		friendsTTL:     c.EffectiveFriendsCacheTTL(),
		groupsTTL:      c.EffectiveGroupsCacheTTL(),
		infoMin:        c.EffectiveInfoMinInterval(),
		infoTTL:        c.EffectiveInfoCacheTTL(),
		sendMin:        c.EffectiveSendMinDelay(),
		sendMax:        c.EffectiveSendMaxDelay(),
		sendMaxPerHour: c.EffectiveSendMaxPerHour(),
		userAgent:      c.EffectiveUserAgent(),
		language:       c.EffectiveLanguage(),
	}
}

// zaRandomDelay 返回 [min,max] 内的随机时长；max<=min 时返回 min。
// 用 math/rand/v2：发送闸只关心分布，不需要密码学随机。
func zaRandomDelay(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)+1))
}

// zaMgr 是进程级 manager 快照。Store/Swap 的职责在 ZaloStart/ZaloStop；
// 未启动时 currentManager 返回 ErrZaloNotStarted。
var zaMgr atomic.Pointer[zaManager]

// currentManager 返回当前启用的 manager；nil 即通道未启动。
func currentManager() (*zaManager, error) {
	if m := zaMgr.Load(); m != nil {
		return m, nil
	}
	return nil, ErrZaloNotStarted
}

// zaConnFlight 是一次进行中的按需登录。done 关闭前 err 不可读；关闭后由 channel
// 的 happens-before 保证安全。
type zaConnFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
	// epoch 是发起本次登录时的账号登录代际。安装/写库前必须与当前值一致，
	// 否则说明期间发生过登出/删除/导入/扫码成功，本次结果作废。
	epoch uint64
}

// zaManager 是全部账号与后台协程的所有者。
//
// root 是 manager 的生命周期 ctx：登录与 listener 都从它派生，Stop 一次 cancel
// 即可打断所有在飞工作。stopping 在 Stop 一开始就置位，供各路径在落库/安装前
// 做最后一道检查（Stop 不持 opMu，否则会被一个长登录拖满停机预算）。
type zaManager struct {
	limits    zaLimits
	store     zaStore
	sdk       zaSDK
	root      context.Context
	cancel    context.CancelFunc
	clock     zaClock
	randDelay func(min, max time.Duration) time.Duration

	mu       sync.RWMutex
	accounts map[string]*zaAccount

	wg       sync.WaitGroup
	stopping atomic.Bool
	stopOnce sync.Once

	// QR 会话表与生命周期钩子由 za_pairing.go 实现；访问一律走 m.mu。
	qrByID      map[string]*QRSession
	qrByAccount map[string]*QRSession
	qrCancel    func(accountID string)
	qrCloseAll  func()
}

// zaAccount 是一个账号的全部可变状态。
//
// opMu 串行化「登录/导入/登出/删除」这类改 conn 的决策；mu 只保护字段本身。
// friends/groups 的缓存锁、infoCache 的两把锁都不与 opMu/mu 嵌套。
type zaAccount struct {
	id  string
	mgr *zaManager

	opMu sync.Mutex
	mu   sync.Mutex

	conn           zaConn
	uid            string
	stopped        bool
	loginEpoch     uint64
	login          *zaConnFlight
	listenerCancel context.CancelFunc

	friends *zaCache[UserInfo]
	groups  *zaCache[GroupInfo]

	groupVer   map[string]string
	groupVerMu sync.Mutex

	// infoCache 是用户/群资料缓存（契约里的 info 字段：与 a.info() 方法重名，
	// Go 不允许，故改名为 infoCache 保留方法签名）。
	infoCache *zaInfoCache

	// 发送闸字段由 za_facade.go 使用。
	sendMu     sync.Mutex
	lastSend   time.Time
	sendWindow []time.Time
}

// newZAManager 构造 manager。parent 为 nil 时用 Background；clock 为 nil 时用
// 真实时钟；randDelay 为 nil 时用 zaRandomDelay。
func newZAManager(parent context.Context, limits zaLimits, store zaStore, sdk zaSDK, clock zaClock, randDelay func(min, max time.Duration) time.Duration) *zaManager {
	if parent == nil {
		parent = context.Background()
	}
	if clock == nil {
		clock = realZAClock{}
	}
	if randDelay == nil {
		randDelay = zaRandomDelay
	}
	root, cancel := context.WithCancel(parent)
	m := &zaManager{
		limits:      limits,
		store:       store,
		sdk:         sdk,
		root:        root,
		cancel:      cancel,
		clock:       clock,
		randDelay:   randDelay,
		accounts:    make(map[string]*zaAccount),
		qrByID:      make(map[string]*QRSession),
		qrByAccount: make(map[string]*QRSession),
	}
	m.qrCancel = m.cancelQRByAccount
	m.qrCloseAll = m.closeAllQRSessions
	return m
}

// Stop 关停 manager：置 stopping、cancel root、关 QR 会话、逐账号停登录/listener
// 并尽力把活会话凭证写回库，最后在 1.5s 预算内等后台协程并关闭会话库。
// 可重复调用；第二次起是空操作。
func (m *zaManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		m.stopping.Store(true)
		m.cancel()
		if m.qrCloseAll != nil {
			m.qrCloseAll()
		}

		// 所有账号共用一个停机预算：逐账号各给 2s 会让多账号实例远超 app 的
		// 2s 服务停机预算。stopAccount 的快照写库也吃这个 ctx，超时即放弃。
		stopCtx, stopCancel := context.WithTimeout(context.Background(), zaMgrStopDrainBudget)
		defer stopCancel()

		var stopWG sync.WaitGroup
		for _, acc := range m.snapshotAccounts() {
			stopWG.Add(1)
			go func(a *zaAccount) {
				defer stopWG.Done()
				a.stopAccount(stopCtx)
			}(acc)
		}

		drained := make(chan struct{})
		go func() {
			stopWG.Wait()
			m.wg.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-stopCtx.Done():
			vars.Warning("Zalo 停机等待后台协程超时（%s），继续关闭会话库", zaMgrStopDrainBudget)
		}

		if m.store != nil {
			if err := m.store.Close(); err != nil {
				vars.Warning("Zalo 会话库关闭失败: %v", err)
			}
		}
	})
}

// snapshotAccounts 返回账号指针快照；调用方在锁外逐个停。
func (m *zaManager) snapshotAccounts() []*zaAccount {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*zaAccount, 0, len(m.accounts))
	for _, acc := range m.accounts {
		out = append(out, acc)
	}
	return out
}

// ListAccounts 返回库里的账号元数据，并合并内存在线状态（LoggedIn/UID），
// 按账号 ID 升序。查库失败返回错误而不是半份列表。
func (m *zaManager) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.store == nil {
		return nil, ErrZaloNotStarted
	}
	metas, err := m.store.List(ctx)
	if err != nil {
		if errors.Is(err, errZAGStoreUnopened) || errors.Is(err, errZAGStoreClosed) {
			return nil, ErrZaloNotStarted
		}
		return nil, zaSafeWrap("zalo 读取账号列表失败", err)
	}

	out := make([]AccountInfo, 0, len(metas))
	for _, meta := range metas {
		info := AccountInfo{ID: meta.AccountID, UID: meta.UID, UpdatedAt: meta.UpdatedAt}
		if acc, ok := m.account(meta.AccountID); ok {
			if conn, uid := acc.online(); conn != nil {
				info.LoggedIn = true
				if uid != "" {
					info.UID = uid
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// accountOrCreate 校验 ID 后在 manager 锁下双检创建账号。Stop 开始后拒绝新建：
// 停机后不可能再有账号进入服务。
func (m *zaManager) accountOrCreate(id string) (*zaAccount, error) {
	if err := validateZAAccountID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping.Load() {
		return nil, ErrZaloNotStarted
	}
	if acc, ok := m.accounts[id]; ok {
		return acc, nil
	}
	acc := &zaAccount{
		id:        id,
		mgr:       m,
		friends:   &zaCache[UserInfo]{},
		groups:    &zaCache[GroupInfo]{},
		groupVer:  make(map[string]string),
		infoCache: newZAInfoCache(),
	}
	m.accounts[id] = acc
	return acc, nil
}

// account 只查内存；不触发任何加载。
func (m *zaManager) account(id string) (*zaAccount, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	acc, ok := m.accounts[id]
	return acc, ok
}

// DeleteAccount 删除账号：停 listener、清 conn、删库行、从内存移除；幂等。
//
// 顺序保证「删掉之后不会被在飞登录复活」：先置 stopped 并取消登录，再等它结束，
// 最后在 opMu 内做库删除——LoginWithCookie 在 opMu 上排队，轮到它时 canLogin
// 已经是 false。
func (m *zaManager) DeleteAccount(ctx context.Context, id string) error {
	if err := validateZAAccountID(id); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.cancelQRForAccount(id)

	acc, ok := m.account(id)
	if !ok {
		return m.deleteStoredAccount(ctx, id)
	}

	acc.mu.Lock()
	acc.stopped = true
	acc.loginEpoch++
	conn := acc.conn
	login := acc.login
	listenerCancel := acc.listenerCancel
	acc.conn = nil
	acc.uid = ""
	acc.listenerCancel = nil
	acc.mu.Unlock()

	if login != nil {
		login.cancel()
		select {
		case <-login.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	acc.opMu.Lock()
	err := m.deleteStoredAccount(ctx, id)
	if err == nil {
		m.mu.Lock()
		if m.accounts[id] == acc {
			delete(m.accounts, id)
		}
		m.mu.Unlock()
	} else {
		// 删库失败：账号保留，但必须解除 stopped，否则这条账号在本进程里
		// 永久不可用（只能重启）。conn/listener 已停，下次使用会按需重登。
		acc.mu.Lock()
		if !m.stopping.Load() {
			acc.stopped = false
		}
		acc.mu.Unlock()
	}
	acc.opMu.Unlock()

	if listenerCancel != nil {
		listenerCancel()
	}
	if conn != nil {
		conn.StopListener()
	}
	return err
}

// deleteStoredAccount 删除库行并把存储句柄哨兵映射为 ErrZaloNotStarted。
func (m *zaManager) deleteStoredAccount(ctx context.Context, id string) error {
	if m.store == nil {
		return ErrZaloNotStarted
	}
	if err := m.store.Delete(ctx, id); err != nil {
		if errors.Is(err, errZAGStoreUnopened) || errors.Is(err, errZAGStoreClosed) {
			return ErrZaloNotStarted
		}
		return zaSafeWrap("zalo 删除账号会话失败", err)
	}
	return nil
}

// Logout 停 listener、清 conn，但保留库行（下次使用会按需重新登录）；幂等，
// 未知账号返回 nil。
func (m *zaManager) Logout(ctx context.Context, id string) error {
	if err := validateZAAccountID(id); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	acc, ok := m.account(id)
	if !ok {
		return nil
	}

	// 先取消在飞登录：不能持 opMu 等它（runLogin 安装 conn 也要 opMu）。
	acc.opMu.Lock()
	acc.mu.Lock()
	acc.loginEpoch++ // 让在飞登录的 Upsert/install 代际检查失败
	inflight := acc.login
	acc.mu.Unlock()
	if inflight != nil {
		inflight.cancel()
	}
	acc.opMu.Unlock()
	if inflight != nil {
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	acc.opMu.Lock()
	if acc.canLogin() {
		conn, cancel := acc.detachConn()
		acc.opMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			conn.StopListener()
		}
		return nil
	}
	acc.opMu.Unlock()
	return nil
}

// LoginWithCookie 解析并导入 cookie 凭证：写库成功后才安装 conn；解析失败
// 不打任何接口、不创建账号。
func (m *zaManager) LoginWithCookie(ctx context.Context, id string, raw []byte) (*AccountInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateZAAccountID(id); err != nil {
		return nil, err
	}
	cred, err := parseZACredentials(raw, id, m.limits.userAgent, m.limits.language)
	if err != nil {
		return nil, err
	}
	acc, err := m.accountOrCreate(id)
	if err != nil {
		return nil, err
	}
	m.cancelQRForAccount(id)

	// 先取消在飞登录，且不持 opMu 等它（互锁原因见文件头）。
	acc.opMu.Lock()
	inflight := acc.login
	if inflight != nil {
		inflight.cancel()
	}
	acc.opMu.Unlock()
	if inflight != nil {
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	acc.opMu.Lock()
	defer acc.opMu.Unlock()
	if !acc.canLogin() {
		return nil, ErrZaloNotStarted
	}
	if m.sdk == nil || m.store == nil {
		return nil, ErrZaloNotStarted
	}
	// 本次导入是新的登录代际：在飞登录（若有）在写库/安装前会被代际检查拒绝。
	// ensureConn 创建 flight 也要 opMu，因此导入期间不会再有新 flight 插进来。
	acc.mu.Lock()
	acc.loginEpoch++
	acc.mu.Unlock()

	loginCtx, cancel := context.WithTimeout(ctx, m.limits.requestTimeout)
	conn, err := m.sdk.Login(loginCtx, cred)
	cancel()
	if err != nil {
		return nil, zaSafeWrap("zalo 登录失败（cookie 可能已过期，请重新扫码或导入）", err)
	}
	if conn == nil {
		return nil, zaSafeWrap("zalo 登录失败（cookie 可能已过期，请重新扫码或导入）", errZAMgrNilConn)
	}
	snap, err := conn.SnapshotCredentials()
	if err != nil {
		return nil, zaSafeWrap("zalo 会话快照失败，请重新扫码或导入", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, zaSafeWrap("zalo 会话凭证序列化失败", err)
	}
	// Stop 已开始：不写库、不安装，避免留下一条进程内已失效的会话行。
	if !acc.canLogin() {
		return nil, ErrZaloNotStarted
	}
	rec := zaSessionRecord{AccountID: id, UID: conn.UID(), Credentials: data}
	if err := m.store.Upsert(ctx, rec); err != nil {
		if errors.Is(err, errZAGStoreUnopened) || errors.Is(err, errZAGStoreClosed) {
			return nil, ErrZaloNotStarted
		}
		return nil, zaSafeWrap("zalo 会话凭证写库失败", err)
	}
	if !acc.installConnLocked(conn) {
		return nil, ErrZaloNotStarted
	}
	info := acc.info()
	return &info, nil
}

// StartClient 显式拉起账号：按需从库读凭证登录，成功后返回账号信息。
func (m *zaManager) StartClient(ctx context.Context, id string) (*AccountInfo, error) {
	acc, err := m.accountOrCreate(id)
	if err != nil {
		return nil, err
	}
	if _, err := acc.ensureConn(ctx); err != nil {
		return nil, err
	}
	info := acc.info()
	return &info, nil
}

// cancelQRForAccount 调 za_pairing.go 设置的 QR 取消钩子；未设置时是空操作。
func (m *zaManager) cancelQRForAccount(id string) {
	if m == nil || m.qrCancel == nil {
		return
	}
	m.qrCancel(id)
}

// ensureConn 返回可用连接；没有则单飞登录。
//
// 快路直接返回现成 conn。否则在 mu 下：stopped → ErrZaloNotStarted；已有 flight
// 则等待（调用方 ctx 取消只影响自己）；否则建 flight，用 manager root +
// requestTimeout 派生 detached loginCtx（不跟随调用方 ctx），后台登录。
func (a *zaAccount) ensureConn(ctx context.Context) (zaConn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if conn := a.currentConn(); conn != nil {
		return conn, nil
	}

	for {
		// 用 opMu 串行化「创建 flight」：Logout/Delete/LoginWithCookie 都在 opMu
		// 下改代际并取消在飞登录，创建 flight 也进同一把锁，才不会出现
		// 「Logout 读完后又有新 flight 起来、并在 Logout 返回后安装 conn」。
		a.opMu.Lock()
		a.mu.Lock()
		if a.stopped || a.mgr.stopping.Load() {
			a.mu.Unlock()
			a.opMu.Unlock()
			return nil, ErrZaloNotStarted
		}
		if a.conn != nil {
			conn := a.conn
			a.mu.Unlock()
			a.opMu.Unlock()
			return conn, nil
		}
		if f := a.login; f != nil {
			a.mu.Unlock()
			a.opMu.Unlock()
			select {
			case <-f.done:
				if f.err != nil {
					return nil, f.err
				}
				if conn := a.currentConn(); conn != nil {
					return conn, nil
				}
				// 登录成功但 conn 被并发清掉（登出/被踢/停机竞态）：重走一轮，
				// stopped 检查会兜住停机。
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if a.mgr.sdk == nil || a.mgr.store == nil {
			a.mu.Unlock()
			a.opMu.Unlock()
			return nil, ErrZaloNotStarted
		}

		loginCtx, cancel := context.WithTimeout(a.mgr.root, a.mgr.limits.requestTimeout)
		f := &zaConnFlight{done: make(chan struct{}), cancel: cancel, epoch: a.loginEpoch}
		a.login = f
		// Add 在 mu 内：与 stopAccount 的 stopped=true 同锁，保证不会在 Stop 的
		// Wait 开始后才 Add（WaitGroup 的 Add/Wait 顺序要求）。
		a.mgr.wg.Add(1)
		a.mu.Unlock()
		a.opMu.Unlock()

		go a.runLogin(loginCtx, f)

		select {
		case <-f.done:
			if f.err != nil {
				return nil, f.err
			}
			if conn := a.currentConn(); conn != nil {
				return conn, nil
			}
			if a.stoppedOrStopping() {
				return nil, ErrZaloNotStarted
			}
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// runLogin 是一次单飞登录的后台执行体。err 必须在 close(done) 前写入。
func (a *zaAccount) runLogin(ctx context.Context, f *zaConnFlight) {
	defer a.mgr.wg.Done()
	defer f.cancel()

	err := a.loginFromStore(ctx, f)

	a.mu.Lock()
	f.err = err
	if a.login == f {
		a.login = nil
	}
	a.mu.Unlock()
	close(f.done)
}

// loginFromStore 走完整登录链：store.Get → 解码凭证 → sdk.Login → 快照 →
// 序列化 → store.Upsert → 安装 conn。
//
// 写库失败不得安装 conn：否则会出现「本进程能用、重启后没有」的假成功。
func (a *zaAccount) loginFromStore(ctx context.Context, f *zaConnFlight) error {
	if a.stoppedOrStopping() {
		return ErrZaloNotStarted
	}
	if a.mgr.store == nil || a.mgr.sdk == nil {
		return ErrZaloNotStarted
	}

	rec, err := a.mgr.store.Get(ctx, a.id)
	switch {
	case err != nil:
		if errors.Is(err, errZAGStoreUnopened) || errors.Is(err, errZAGStoreClosed) {
			return ErrZaloNotStarted
		}
		return zaSafeWrap("zalo 读取会话库失败", err)
	case rec == nil:
		return fmt.Errorf("%w: 会话库里没有该账号的凭证", ErrZaloNotLoggedIn)
	}

	var cred zcago.Credentials
	if err := json.Unmarshal(rec.Credentials, &cred); err != nil {
		return zaSafeWrap("zalo 会话凭证损坏，请重新扫码或导入", err)
	}

	conn, err := a.mgr.sdk.Login(ctx, cred)
	if err != nil {
		return zaSafeWrap("zalo 登录失败（cookie 可能已过期，请重新扫码或导入）", err)
	}
	if conn == nil {
		return zaSafeWrap("zalo 登录失败（cookie 可能已过期，请重新扫码或导入）", errZAMgrNilConn)
	}
	snap, err := conn.SnapshotCredentials()
	if err != nil {
		return zaSafeWrap("zalo 会话快照失败，请重新扫码或导入", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return zaSafeWrap("zalo 会话凭证序列化失败", err)
	}

	// 写库与安装必须在 opMu 内，并与发起时的登录代际比对：
	// Delete/Logout/导入/扫码成功会递增 loginEpoch；代际不符说明本次登录已被
	// 取代，不能再写库或安装，否则会把已删账号复活、或覆盖后发起的会话。
	// Upsert 也放进 opMu：Delete 删行与旧登录回写不会再交错。
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.Lock()
	stopped := a.stopped || a.mgr.stopping.Load()
	epochChanged := a.loginEpoch != f.epoch
	a.mu.Unlock()
	if stopped {
		return ErrZaloNotStarted
	}
	if epochChanged {
		return errZALoginSuperseded
	}

	rec = &zaSessionRecord{AccountID: a.id, UID: conn.UID(), Credentials: data}
	if err := a.mgr.store.Upsert(ctx, *rec); err != nil {
		if errors.Is(err, errZAGStoreUnopened) || errors.Is(err, errZAGStoreClosed) {
			return ErrZaloNotStarted
		}
		return zaSafeWrap("zalo 会话凭证写库失败", err)
	}
	if !a.installConnLocked(conn) {
		return ErrZaloNotStarted
	}
	return nil
}

// installConnLocked 换上新 conn 并启动 listener。调用方必须持 opMu。
//
// 返回 false 表示账号已停止（或 manager 正在停机），conn 未被安装。旧 conn 的
// listener 在释放 mu 之后、启动新 listener 之前停掉，避免两条长连接并存。
func (a *zaAccount) installConnLocked(conn zaConn) bool {
	if conn == nil {
		return false
	}
	a.mu.Lock()
	if a.stopped || a.mgr.stopping.Load() {
		a.mu.Unlock()
		return false
	}
	old := a.conn
	a.conn = conn
	a.uid = conn.UID()
	a.mu.Unlock()

	if old != nil && old != conn {
		old.StopListener()
	}
	a.startListener(conn)
	return true
}

// startListener 用 manager root（绝不能套 requestTimeout）启动 listener。
// 启动失败只记 Warning，不阻断登录：入站消息是增强能力，不是登录前提。
func (a *zaAccount) startListener(conn zaConn) {
	if conn == nil || !a.mgr.limits.listener {
		return
	}
	lctx, cancel := context.WithCancel(a.mgr.root)

	a.mu.Lock()
	if a.stopped || a.mgr.stopping.Load() || a.conn != conn {
		a.mu.Unlock()
		cancel()
		return
	}
	prev := a.listenerCancel
	a.listenerCancel = cancel
	a.mgr.wg.Add(1)
	a.mu.Unlock()

	if prev != nil {
		prev()
	}

	go func() {
		defer a.mgr.wg.Done()
		err := conn.StartListener(lctx,
			func(msg InboundMessage) {
				msg.AccountID = a.id
				util.DefaultCallFunc.Do(util.CallZaloMsg+"Inbound", &msg)
			},
			func(code int, reason string) {
				// reason 来自上游关闭帧，可能含服务端文本；只记关闭码。
				vars.Warning("Zalo listener 终止 account=%s code=%d", a.id, code)
				// 3003=Kick、3000=Duplicate：会话已失效，清 conn 让下次收发重新登录；
				// 不删库行。其它关闭码交给 zcago 的重连逻辑。
				if code == 3003 || code == 3000 {
					a.invalidateConn(conn)
				}
			},
		)
		if err != nil {
			vars.Warning("Zalo listener 启动失败 account=%s: %v", a.id, err)
		}
	}()
}

// invalidateConn 在 conn 仍是当前连接时清掉它（被踢/重复连接）。
//
// 只 cancel listener ctx，不调 StopListener：onTerminal 正在 listener 的消费
// 协程里执行，StopListener 会等这个协程自己退出而死锁。
func (a *zaAccount) invalidateConn(conn zaConn) {
	if conn == nil {
		return
	}
	a.mu.Lock()
	if a.conn != conn {
		a.mu.Unlock()
		return
	}
	a.conn = nil
	a.uid = ""
	cancel := a.listenerCancel
	a.listenerCancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// stopAccount 停一个账号：置 stopped、取消登录与 listener，尽力回写最新凭证，
// 最后停 listener。Stop 不持 opMu 调用它，长登录不会拖住停机预算。
// ctx 是 manager 的共享停机预算：快照写库用它，超时即放弃，避免多账号叠加。
func (a *zaAccount) stopAccount(ctx context.Context) {
	if a == nil || a.mgr == nil {
		return
	}
	a.mu.Lock()
	a.stopped = true
	conn := a.conn
	login := a.login
	listenerCancel := a.listenerCancel
	a.conn = nil
	a.uid = ""
	a.listenerCancel = nil
	a.mu.Unlock()

	if login != nil {
		login.cancel()
	}
	if listenerCancel != nil {
		listenerCancel()
	}
	if conn == nil {
		return
	}

	// best-effort：Stop 之后进程不再持有活会话，cookie 可能已被服务端刷新。
	// ctx 已带上共享停机预算；这里再加单账号上限，取两者中先到的。
	saveCtx, cancel := context.WithTimeout(ctx, zaMgrStopSnapshotBudget)
	defer cancel()
	if saveCtx.Err() == nil {
		if snap, err := conn.SnapshotCredentials(); err != nil {
			vars.Warning("Zalo 停机快照凭证失败 account=%s: %v", a.id, err)
		} else if data, err := json.Marshal(snap); err != nil {
			vars.Warning("Zalo 停机序列化凭证失败 account=%s: %v", a.id, err)
		} else if a.mgr.store != nil {
			rec := zaSessionRecord{AccountID: a.id, UID: conn.UID(), Credentials: data}
			if err := a.mgr.store.Upsert(saveCtx, rec); err != nil {
				vars.Warning("Zalo 停机保存凭证失败 account=%s: %v", a.id, err)
			}
		}
	}
	conn.StopListener()
}

// info 返回账号的可公开事实；UpdatedAt 留零（ListAccounts 用库时间）。
func (a *zaAccount) info() AccountInfo {
	conn, uid := a.online()
	return AccountInfo{ID: a.id, UID: uid, LoggedIn: conn != nil}
}

// currentConn 返回当前 conn；没有返回 nil。
func (a *zaAccount) currentConn() zaConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

// online 一次性读出 conn/uid，保证 LoggedIn 与 UID 来自同一时刻。
func (a *zaAccount) online() (zaConn, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn, a.uid
}

// detachConn 摘掉当前 conn 与 listener cancel；调用方在锁外停 listener。
func (a *zaAccount) detachConn() (zaConn, context.CancelFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	conn := a.conn
	cancel := a.listenerCancel
	a.conn = nil
	a.uid = ""
	a.listenerCancel = nil
	return conn, cancel
}

// canLogin 判断当前是否允许发起/安装登录（未停止且 manager 未停机）。
func (a *zaAccount) canLogin() bool {
	if a.mgr == nil || a.mgr.stopping.Load() {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.stopped
}

// stoppedOrStopping 是 canLogin 的读法别名，语义相同。
func (a *zaAccount) stoppedOrStopping() bool { return !a.canLogin() }

// listFriends 返回好友列表：refresh=false 走 TTL 缓存，refresh=true 强制刷新
// 但仍受 5 分钟闸（ErrZaloFetchTooSoon）。
func (a *zaAccount) listFriends(ctx context.Context, refresh bool) ([]UserInfo, error) {
	conn, err := a.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	fetch := func(fctx context.Context) ([]UserInfo, error) {
		rctx, cancel := context.WithTimeout(fctx, a.mgr.limits.requestTimeout)
		defer cancel()
		users, err := conn.GetAllFriends(rctx)
		if err != nil {
			return nil, zaSafeWrap("zalo 拉取好友列表失败", err)
		}
		return users, nil
	}
	ttl := a.mgr.limits.friendsTTL
	if refresh {
		return a.friends.Refresh(ctx, ttl, ttl, a.mgr.clock, fetch, cloneUserInfos)
	}
	return a.friends.List(ctx, ttl, ttl, a.mgr.clock, fetch, cloneUserInfos)
}

// listGroups 返回群列表：refresh=false 走 TTL 缓存，refresh=true 强制刷新。
// 刷新逻辑见 fetchGroups（版本差分 + 资料缓存）。
func (a *zaAccount) listGroups(ctx context.Context, refresh bool) ([]GroupInfo, error) {
	if _, err := a.ensureConn(ctx); err != nil {
		return nil, err
	}
	ttl := a.mgr.limits.groupsTTL
	if refresh {
		return a.groups.Refresh(ctx, ttl, ttl, a.mgr.clock, a.fetchGroups, cloneGroupInfos)
	}
	return a.groups.List(ctx, ttl, ttl, a.mgr.clock, a.fetchGroups, cloneGroupInfos)
}

// userInfo 返回用户资料（按 ID 缓存，命中不睡眠；缺失才走 1.5s 闸 + 批量请求）。
func (a *zaAccount) userInfo(ctx context.Context, ids []string) (map[string]UserInfo, error) {
	conn, err := a.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	fetch := func(fctx context.Context, batch []string) ([]UserInfo, error) {
		rctx, cancel := context.WithTimeout(fctx, a.mgr.limits.requestTimeout)
		defer cancel()
		users, err := conn.GetUserInfo(rctx, batch...)
		if err != nil {
			return nil, zaSafeWrap("zalo 拉取用户资料失败", err)
		}
		return users, nil
	}
	return a.infoCache.Users(ctx, ids, a.mgr.limits.infoTTL, a.mgr.limits.infoMin, a.mgr.clock, fetch)
}

// groupInfo 返回群资料（与 userInfo 共用缓存与 1.5s 闸）。
func (a *zaAccount) groupInfo(ctx context.Context, ids []string) (map[string]GroupInfo, error) {
	conn, err := a.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	fetch := func(fctx context.Context, batch []string) ([]GroupInfo, error) {
		rctx, cancel := context.WithTimeout(fctx, a.mgr.limits.requestTimeout)
		defer cancel()
		groups, err := conn.GetGroupInfo(rctx, batch...)
		if err != nil {
			return nil, zaSafeWrap("zalo 拉取群资料失败", err)
		}
		return groups, nil
	}
	return a.infoCache.Groups(ctx, ids, a.mgr.limits.infoTTL, a.mgr.limits.infoMin, a.mgr.clock, fetch)
}

// fetchGroups 是群列表的拉取体：先用 GetAllGroups 拿 groupID→version，只对
// 新增/版本变化/资料过期的群调 GetGroupInfo；全部成功后才更新 groupVer。
// 缺失任何一条资料即返回错误，绝不交半份列表。
func (a *zaAccount) fetchGroups(ctx context.Context) ([]GroupInfo, error) {
	conn, err := a.ensureConn(ctx)
	if err != nil {
		return nil, err
	}

	rctx, cancel := context.WithTimeout(ctx, a.mgr.limits.requestTimeout)
	vers, err := conn.GetAllGroups(rctx)
	cancel()
	if err != nil {
		return nil, zaSafeWrap("zalo 拉取群列表失败", err)
	}

	ttl := a.mgr.limits.infoTTL
	now := a.mgr.clock.Now()

	a.groupVerMu.Lock()
	prev := make(map[string]string, len(a.groupVer))
	for id, ver := range a.groupVer {
		prev[id] = ver
	}
	a.groupVerMu.Unlock()

	needed := make([]string, 0, len(vers))
	changed := make([]string, 0)
	for id, ver := range vers {
		if id == "" {
			continue
		}
		if old, ok := prev[id]; ok {
			if old != ver {
				// 版本变了必须让资料缓存失效：infoCache.Groups 只请求缺失条目，
				// 不先 drop 就会拿旧资料配新版本号。
				changed = append(changed, id)
				needed = append(needed, id)
				continue
			}
			if _, ok := a.infoCache.CachedGroup(id, ttl, now); !ok {
				needed = append(needed, id)
			}
			continue
		}
		needed = append(needed, id)
	}

	removed := make([]string, 0)
	for id := range prev {
		if _, ok := vers[id]; !ok {
			removed = append(removed, id)
		}
	}
	drops := make([]string, 0, len(removed)+len(changed))
	drops = append(drops, removed...)
	drops = append(drops, changed...)
	if len(drops) > 0 {
		a.infoCache.DropGroups(drops)
	}

	if len(needed) > 0 {
		sort.Strings(needed) // 稳定的分批顺序，便于对拍与排障
		batchFetch := func(fctx context.Context, batch []string) ([]GroupInfo, error) {
			rctx, cancel := context.WithTimeout(fctx, a.mgr.limits.requestTimeout)
			defer cancel()
			groups, err := conn.GetGroupInfo(rctx, batch...)
			if err != nil {
				return nil, zaSafeWrap("zalo 拉取群资料失败", err)
			}
			return groups, nil
		}
		if _, err := a.infoCache.Groups(ctx, needed, ttl, a.mgr.limits.infoMin, a.mgr.clock, batchFetch); err != nil {
			return nil, err
		}
	}

	out := make([]GroupInfo, 0, len(vers))
	now = a.mgr.clock.Now()
	for id, ver := range vers {
		if id == "" {
			continue
		}
		g, ok := a.infoCache.CachedGroup(id, ttl, now)
		if !ok {
			// 服务端没给这条群的资料：不能拿零值补造，也不能交半份列表。
			return nil, fmt.Errorf("zalo 群资料缺失（group_id=%s）：本轮未拿到完整群列表", id)
		}
		g.Version = ver
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].GroupID < out[j].GroupID
	})

	fresh := make(map[string]string, len(vers))
	for id, ver := range vers {
		if id == "" {
			continue
		}
		fresh[id] = ver
	}
	a.groupVerMu.Lock()
	a.groupVer = fresh
	a.groupVerMu.Unlock()
	return out, nil
}
