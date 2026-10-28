package zalo

// 本文件是 Zalo 多账号管理器的活闸：单飞登录、conn 生命周期、Stop、账号列表/
// 删除/登出、cookie 导入、缓存封装与群版本差分。
//
// 全部使用 fake store/SDK/conn 与假时钟，不连网、不连库；listener 用 fake conn
// 保存的 handler 手动 emit，不启动真实 WebSocket。

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrakk/zcago"
	"touchgocore/config"
	"touchgocore/util"
)

// errMgrBoom 是被注入的通用失败，用于断言 errors.Is 能穿过安全包装。
var errMgrBoom = errors.New("mgr test boom")

// fakeZAStore 是 zaStore 的记账假件：map 存储、调用计数、可注入错误。
type fakeZAStore struct {
	mu   sync.Mutex
	recs map[string]zaSessionRecord

	getCalls, listCalls, upsertCalls, deleteCalls, closeCalls int
	getErr, listErr, upsertErr, deleteErr, closeErr           error
	lastUpsert                                                zaSessionRecord
	hasUpsert                                                 bool
}

// newFakeZAStore 构造空库。
func newFakeZAStore() *fakeZAStore {
	return &fakeZAStore{recs: make(map[string]zaSessionRecord)}
}

// seed 写入一行会话（固定更新时间）。
func (s *fakeZAStore) seed(id, uid string, cred []byte) {
	s.seedAt(id, uid, cred, time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
}

// seedAt 写入一行带指定更新时间的会话。
func (s *fakeZAStore) seedAt(id, uid string, cred []byte, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[id] = zaSessionRecord{
		AccountID:   id,
		UID:         uid,
		Credentials: append([]byte(nil), cred...),
		UpdatedAt:   at,
	}
}

// Get 返回记录副本；查无返回 (nil,nil)。
func (s *fakeZAStore) Get(_ context.Context, accountID string) (*zaSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	rec, ok := s.recs[accountID]
	if !ok {
		return nil, nil
	}
	cp := rec
	cp.Credentials = append([]byte(nil), rec.Credentials...)
	return &cp, nil
}

// List 返回全部元数据（故意按 map 顺序，调用方必须自行排序）。
func (s *fakeZAStore) List(_ context.Context) ([]zaSessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]zaSessionMeta, 0, len(s.recs))
	for _, rec := range s.recs {
		out = append(out, zaSessionMeta{AccountID: rec.AccountID, UID: rec.UID, UpdatedAt: rec.UpdatedAt})
	}
	return out, nil
}

// Upsert 覆盖写入并记账最后一次载荷。
func (s *fakeZAStore) Upsert(_ context.Context, rec zaSessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertCalls++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	cp := rec
	cp.Credentials = append([]byte(nil), rec.Credentials...)
	// 与真库同口径：Upsert 忽略调用方 UpdatedAt，写路径统一取 now()。
	cp.UpdatedAt = time.Now().UTC()
	s.recs[rec.AccountID] = cp
	s.lastUpsert = cp
	s.hasUpsert = true
	return nil
}

// Delete 删除行；未知 ID 也算成功（幂等）。
func (s *fakeZAStore) Delete(_ context.Context, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.recs, accountID)
	return nil
}

// Close 只记账。
func (s *fakeZAStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return s.closeErr
}

// getCount / upsertCount / deleteCount / closeCount 返回对应调用计数。
func (s *fakeZAStore) getCount() int    { s.mu.Lock(); defer s.mu.Unlock(); return s.getCalls }
func (s *fakeZAStore) upsertCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.upsertCalls }
func (s *fakeZAStore) deleteCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.deleteCalls }
func (s *fakeZAStore) closeCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.closeCalls }

// record 返回某账号的当前行。
func (s *fakeZAStore) record(id string) (zaSessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[id]
	return rec, ok
}

// lastUpsertRec 返回最后一次 Upsert 的载荷。
func (s *fakeZAStore) lastUpsertRec() (zaSessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUpsert, s.hasUpsert
}

// fakeZASDK 是登录 seam 的假件。
//
// gate 非 nil 时 Login 会一直等到 gate 关闭且**忽略 ctx**：这样「停机期间登录
// 成功」的路径才能被真实覆盖（否则 ctx 取消会让 Login 直接报错）。
type fakeZASDK struct {
	mu         sync.Mutex
	loginCalls int
	qrCalls    int
	loginErr   error
	gate       chan struct{}
	entered    chan struct{}
	factory    func(zcago.Credentials) *fakeZAConn
}

// newFakeZASDK 构造默认假件（Login 立即返回带默认 UID 的假 conn）。
func newFakeZASDK() *fakeZASDK { return &fakeZASDK{} }

// Login 记录调用并按 gate/factory 行为返回。
func (s *fakeZASDK) Login(_ context.Context, cred zcago.Credentials) (zaConn, error) {
	s.mu.Lock()
	s.loginCalls++
	gate := s.gate
	entered := s.entered
	err := s.loginErr
	factory := s.factory
	s.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return &fakeZAConn{uid: "uid-default"}, nil
	}
	return factory(cred), nil
}

// LoginQR 不在本文件使用。
func (s *fakeZASDK) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zaConn, error) {
	s.mu.Lock()
	s.qrCalls++
	s.mu.Unlock()
	return nil, errMgrBoom
}

// loginCount / qrCount 返回调用计数。
func (s *fakeZASDK) loginCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.loginCalls }

// fakeZAConn 是 zaConn 的假件：可注入数据/错误，保存 listener handler 供手动 emit。
type fakeZAConn struct {
	uid     string
	snap    zcago.Credentials
	snapErr error

	mu sync.Mutex

	startCalls, stopCalls int
	startErr              error
	onMessage             func(InboundMessage)
	onTerminal            func(int, string)

	friends      []UserInfo
	friendsErr   error
	friendsCalls int

	groups      map[string]string
	groupsErr   error
	groupsCalls int

	groupInfo      map[string]GroupInfo
	groupInfoErr   error
	groupInfoCalls [][]string

	users         map[string]UserInfo
	userInfoErr   error
	userInfoCalls [][]string

	sendErr   error
	sendMsgID string

	deleteErr   error
	deleteCalls []fakeZADeleteCall
}

// fakeZADeleteCall 是一次到达 conn.DeleteChat 的调用记录。
type fakeZADeleteCall struct {
	threadID string
	group    bool
	boundary ChatBoundary
}

// UID 返回固定 UID。
func (c *fakeZAConn) UID() string { return c.uid }

// SnapshotCredentials 返回固定快照或注入错误。
func (c *fakeZAConn) SnapshotCredentials() (zcago.Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapErr != nil {
		return zcago.Credentials{}, c.snapErr
	}
	return c.snap, nil
}

// SendText 记录并返回注入的 msgID（本文件不测发送）。
func (c *fakeZAConn) SendText(context.Context, string, bool, string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendMsgID, c.sendErr
}

// DeleteChat 记录边界参数并返回注入错误。
func (c *fakeZAConn) DeleteChat(_ context.Context, threadID string, group bool, boundary ChatBoundary) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteCalls = append(c.deleteCalls, fakeZADeleteCall{threadID: threadID, group: group, boundary: boundary})
	return c.deleteErr
}

// recordedDeletes 返回删除调用记录副本。
func (c *fakeZAConn) recordedDeletes() []fakeZADeleteCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeZADeleteCall(nil), c.deleteCalls...)
}

// GetAllFriends 返回好友副本。
func (c *fakeZAConn) GetAllFriends(ctx context.Context) ([]UserInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friendsCalls++
	if c.friendsErr != nil {
		return nil, c.friendsErr
	}
	out := make([]UserInfo, len(c.friends))
	copy(out, c.friends)
	return out, nil
}

// GetAllGroups 返回版本 map 副本。
func (c *fakeZAConn) GetAllGroups(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupsCalls++
	if c.groupsErr != nil {
		return nil, c.groupsErr
	}
	out := make(map[string]string, len(c.groups))
	for id, ver := range c.groups {
		out[id] = ver
	}
	return out, nil
}

// GetGroupInfo 返回请求 ID 中真实存在的资料并记录每批 ID。
func (c *fakeZAConn) GetGroupInfo(ctx context.Context, ids ...string) ([]GroupInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupInfoCalls = append(c.groupInfoCalls, append([]string(nil), ids...))
	if c.groupInfoErr != nil {
		return nil, c.groupInfoErr
	}
	out := make([]GroupInfo, 0, len(ids))
	for _, id := range ids {
		if g, ok := c.groupInfo[id]; ok {
			if g.GroupID == "" {
				g.GroupID = id
			}
			out = append(out, g)
		}
	}
	return out, nil
}

// GetUserInfo 返回请求 ID 中真实存在的资料并记录每批 ID。
func (c *fakeZAConn) GetUserInfo(ctx context.Context, ids ...string) ([]UserInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userInfoCalls = append(c.userInfoCalls, append([]string(nil), ids...))
	if c.userInfoErr != nil {
		return nil, c.userInfoErr
	}
	out := make([]UserInfo, 0, len(ids))
	for _, id := range ids {
		if u, ok := c.users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// StartListener 保存 handler；emit 由测试驱动。
func (c *fakeZAConn) StartListener(_ context.Context, onMessage func(InboundMessage), onTerminal func(int, string)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startCalls++
	c.onMessage = onMessage
	c.onTerminal = onTerminal
	return c.startErr
}

// StopListener 只计数。
func (c *fakeZAConn) StopListener() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopCalls++
}

// emitMessage 手动投递一条入站消息。
func (c *fakeZAConn) emitMessage(msg InboundMessage) {
	c.mu.Lock()
	h := c.onMessage
	c.mu.Unlock()
	if h != nil {
		h(msg)
	}
}

// emitTerminal 手动投递一条 listener 关闭事件。
func (c *fakeZAConn) emitTerminal(code int, reason string) {
	c.mu.Lock()
	h := c.onTerminal
	c.mu.Unlock()
	if h != nil {
		h(code, reason)
	}
}

// setFriends / setFriendsErr 设置好友数据。
func (c *fakeZAConn) setFriends(list []UserInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friends = list
}

func (c *fakeZAConn) setFriendsErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friendsErr = err
}

// setGroups / setGroupInfo 设置群版本与资料。
func (c *fakeZAConn) setGroups(vers map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groups = vers
}

func (c *fakeZAConn) setGroupInfo(infos map[string]GroupInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupInfo = infos
}

// startCount / stopCount / friendsCallCount / groupsCallCount / groupInfoCallCount
// 返回计数器与请求批次快照。
func (c *fakeZAConn) startCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.startCalls }
func (c *fakeZAConn) stopCount() int  { c.mu.Lock(); defer c.mu.Unlock(); return c.stopCalls }

func (c *fakeZAConn) friendsCallCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.friendsCalls }
func (c *fakeZAConn) groupsCallCount() int  { c.mu.Lock(); defer c.mu.Unlock(); return c.groupsCalls }

func (c *fakeZAConn) groupInfoRequests() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]string, len(c.groupInfoCalls))
	for i, ids := range c.groupInfoCalls {
		out[i] = append([]string(nil), ids...)
	}
	return out
}

// mgrTestCredJSON 是一份最小的可解码凭证（假 SDK 不校验 cookie）。
const mgrTestCredJSON = `{"imei":"imei-seed","userAgent":"ua-seed","language":"vi"}`

// mgrTestLimits 是测试用限频：TTL/间隔取生产下限，超时缩短。
func mgrTestLimits() zaLimits {
	return zaLimits{
		qrTTL:          5 * time.Minute,
		requestTimeout: 5 * time.Second,
		listener:       true,
		friendsTTL:     5 * time.Minute,
		groupsTTL:      5 * time.Minute,
		infoMin:        1500 * time.Millisecond,
		infoTTL:        10 * time.Minute,
		sendMin:        3 * time.Second,
		sendMax:        5 * time.Second,
		sendMaxPerHour: 30,
		userAgent:      "test-ua",
		language:       "vi",
	}
}

// mgrNewTestManager 构造 manager 并登记停机清理。
func mgrNewTestManager(t *testing.T, store zaStore, sdk zaSDK, clock zaClock) *zaManager {
	t.Helper()
	if clock == nil {
		clock = newCacheClock()
	}
	m := newZAManager(context.Background(), mgrTestLimits(), store, sdk, clock, nil)
	t.Cleanup(m.Stop)
	return m
}

// mgrWaitTrue 用 Gosched 自旋等待条件成立，避免用睡眠猜调度。
func mgrWaitTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时", what)
		}
		runtime.Gosched()
	}
}

// TestZAManagerEnsureConnSingleFlight 断言 100 个并发 ensureConn 只读一次库、
// 只登录一次，且拿到同一个 conn。
func TestZAManagerEnsureConnSingleFlight(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))

	sdk := newFakeZASDK()
	sdk.gate = make(chan struct{})
	sdk.entered = make(chan struct{}, 1)
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}

	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	const workers = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	conns := make([]zaConn, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			conns[i], errs[i] = acc.ensureConn(context.Background())
		}(i)
	}
	close(start)
	select {
	case <-sdk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("登录未被调用")
	}
	close(sdk.gate)
	wg.Wait()

	if got := store.getCount(); got != 1 {
		t.Fatalf("store.Get 调用 %d 次，期望 1", got)
	}
	if got := sdk.loginCount(); got != 1 {
		t.Fatalf("sdk.Login 调用 %d 次，期望 1", got)
	}
	if got := store.upsertCount(); got != 1 {
		t.Fatalf("store.Upsert 调用 %d 次，期望 1", got)
	}
	first := conns[0]
	if first == nil {
		t.Fatal("第一个等待者拿到 nil conn")
	}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d 失败: %v", i, errs[i])
		}
		if conns[i] != first {
			t.Fatalf("worker %d 拿到不同 conn", i)
		}
	}
	if rec, ok := store.lastUpsertRec(); !ok || rec.UID != "uid-live" {
		t.Fatalf("落库 UID=%q ok=%v，期望 uid-live", rec.UID, ok)
	}
}

// TestZAManagerEnsureConnStoreErrors 覆盖库缺失/库句柄哨兵/库故障/登录失败：
// 一律不安装 conn、不写库。
func TestZAManagerEnsureConnStoreErrors(t *testing.T) {
	cases := []struct {
		name     string
		seed     bool
		getErr   error
		loginErr error
		want     error
	}{
		{name: "missing_row", want: ErrZaloNotLoggedIn},
		{name: "store_unopened", getErr: errZAGStoreUnopened, want: ErrZaloNotStarted},
		{name: "store_closed", getErr: errZAGStoreClosed, want: ErrZaloNotStarted},
		{name: "store_failed", getErr: errMgrBoom, want: errMgrBoom},
		{name: "login_failed", seed: true, loginErr: errMgrBoom, want: errMgrBoom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeZAStore()
			if tc.seed {
				store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
			}
			store.getErr = tc.getErr
			sdk := newFakeZASDK()
			sdk.loginErr = tc.loginErr
			m := mgrNewTestManager(t, store, sdk, nil)

			acc, err := m.accountOrCreate("acct")
			if err != nil {
				t.Fatalf("accountOrCreate: %v", err)
			}
			conn, err := acc.ensureConn(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("ensureConn err=%v，期望 errors.Is(%v)", err, tc.want)
			}
			if conn != nil {
				t.Fatal("失败路径安装了 conn")
			}
			if acc.currentConn() != nil {
				t.Fatal("失败路径账号上有 conn")
			}
			if store.upsertCount() != 0 {
				t.Fatal("失败路径写了库")
			}
			if tc.loginErr == nil && sdk.loginCount() != 0 {
				t.Fatal("读库失败/查无时不应调登录")
			}
		})
	}
}

// TestZAManagerEnsureConnWaiterCancel 断言等待者 ctx 取消只影响自己，leader
// 继续完成登录。
func TestZAManagerEnsureConnWaiterCancel(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.gate = make(chan struct{})
	sdk.entered = make(chan struct{}, 1)
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	leaderDone := make(chan struct{})
	var leaderConn zaConn
	var leaderErr error
	go func() {
		defer close(leaderDone)
		leaderConn, leaderErr = acc.ensureConn(context.Background())
	}()
	<-sdk.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acc.ensureConn(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待者 err=%v，期望 context.Canceled", err)
	}

	close(sdk.gate)
	<-leaderDone
	if leaderErr != nil {
		t.Fatalf("leader 失败: %v", leaderErr)
	}
	if leaderConn == nil {
		t.Fatal("leader 没有拿到 conn")
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("store.Get 调用 %d 次，期望 1", got)
	}
}

// TestZAManagerStopDuringLogin 断言 Stop 开始后即使登录成功也不写库、不安装。
func TestZAManagerStopDuringLogin(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.gate = make(chan struct{})
	sdk.entered = make(chan struct{}, 1)
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	leaderErr := make(chan error, 1)
	go func() {
		_, err := acc.ensureConn(context.Background())
		leaderErr <- err
	}()
	<-sdk.entered

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	mgrWaitTrue(t, "manager 进入 stopping", func() bool { return m.stopping.Load() })
	close(sdk.gate)

	if err := <-leaderErr; !errors.Is(err, ErrZaloNotStarted) {
		t.Fatalf("停机中登录 err=%v，期望 ErrZaloNotStarted", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 未在预算内返回")
	}
	if got := store.upsertCount(); got != 0 {
		t.Fatalf("停机期间写了 %d 次库，期望 0", got)
	}
	if acc.currentConn() != nil {
		t.Fatal("停机期间安装出了 conn")
	}
	if got := store.closeCount(); got != 1 {
		t.Fatalf("store.Close 调用 %d 次，期望 1", got)
	}
}

// TestZAManagerListAccountsMergesOnline 断言查库元数据与内存在线状态合并、
// 按 ID 排序；查库失败返回错误而不是半份列表。
func TestZAManagerListAccountsMergesOnline(t *testing.T) {
	store := newFakeZAStore()
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	store.seedAt("b", "uid-b-db", []byte(mgrTestCredJSON), at)
	store.seedAt("a", "uid-a-db", []byte(mgrTestCredJSON), at)

	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-a-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	if _, err := m.StartClient(context.Background(), "a"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}

	list, err := m.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("账号数 %d，期望 2", len(list))
	}
	if list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("排序错误: %+v", list)
	}
	if !list[0].LoggedIn || list[0].UID != "uid-a-live" {
		t.Fatalf("在线账号状态错误: %+v", list[0])
	}
	if list[0].UpdatedAt.IsZero() {
		t.Fatal("在线账号的 UpdatedAt 应来自库时间")
	}
	if list[1].LoggedIn || list[1].UID != "uid-b-db" {
		t.Fatalf("离线账号状态错误: %+v", list[1])
	}
	if list[1].UpdatedAt != at {
		t.Fatalf("离线账号 UpdatedAt=%v，期望库时间 %v", list[1].UpdatedAt, at)
	}

	store.mu.Lock()
	store.listErr = errMgrBoom
	store.mu.Unlock()
	got, err := m.ListAccounts(context.Background())
	if !errors.Is(err, errMgrBoom) {
		t.Fatalf("查库失败 err=%v", err)
	}
	if got != nil {
		t.Fatalf("查库失败返回了半份列表: %+v", got)
	}
}

// TestZAManagerLogoutAndDeleteIdempotent 覆盖登出保留库行、删除移除库行/内存，
// 以及两者的幂等与未知账号语义。
func TestZAManagerLogoutAndDeleteIdempotent(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)
	ctx := context.Background()

	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("StartClient 后账号不在内存")
	}
	conn, _ := acc.online()
	if conn == nil {
		t.Fatal("StartClient 后没有 conn")
	}
	live := conn.(*fakeZAConn)

	if err := m.Logout(ctx, "acct"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if acc.currentConn() != nil {
		t.Fatal("Logout 后 conn 未清")
	}
	if live.stopCount() == 0 {
		t.Fatal("Logout 未停 listener")
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("Logout 不应删除库行")
	}
	if err := m.Logout(ctx, "acct"); err != nil {
		t.Fatalf("重复 Logout: %v", err)
	}
	if err := m.Logout(ctx, "unknown"); err != nil {
		t.Fatalf("未知账号 Logout 返回错误: %v", err)
	}

	// 登出后可以再次按需登录。
	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("登出后 StartClient: %v", err)
	}
	if got := sdk.loginCount(); got != 2 {
		t.Fatalf("登录次数 %d，期望 2", got)
	}

	if err := m.DeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("DeleteAccount 后账号仍在内存")
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("DeleteAccount 未删除库行")
	}
	if err := m.DeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("重复 DeleteAccount: %v", err)
	}
	if err := m.DeleteAccount(ctx, ".."); !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("非法 ID DeleteAccount err=%v", err)
	}
}

// TestZAManagerDeleteAccountStoreError 断言删库失败返回错误且账号保留（已停），
// 清掉故障后重试收敛。
func TestZAManagerDeleteAccountStoreError(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	m := mgrNewTestManager(t, store, newFakeZASDK(), nil)
	ctx := context.Background()

	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}
	store.mu.Lock()
	store.deleteErr = errMgrBoom
	store.mu.Unlock()

	if err := m.DeleteAccount(ctx, "acct"); !errors.Is(err, errMgrBoom) {
		t.Fatalf("DeleteAccount err=%v，期望注入错误", err)
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("删库失败后账号被移出内存")
	}
	if acc.currentConn() != nil {
		t.Fatal("删库失败后仍留着 conn")
	}

	store.mu.Lock()
	store.deleteErr = nil
	store.mu.Unlock()
	if err := m.DeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("重试 DeleteAccount: %v", err)
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("重试后库行仍在")
	}
}

// TestZAManagerLoginWithCookie 断言 cookie 导入：写库成功才安装 conn，
// 返回在线信息，并触发 QR 取消钩子。
func TestZAManagerLoginWithCookie(t *testing.T) {
	store := newFakeZAStore()
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-cookie", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	var qrCanceled []string
	m.qrCancel = func(id string) { qrCanceled = append(qrCanceled, id) }

	raw := []byte(`[{"name":"zpw_sek","value":"not-a-real-token","domain":"zalo.me"}]`)
	info, err := m.LoginWithCookie(context.Background(), "acct", raw)
	if err != nil {
		t.Fatalf("LoginWithCookie: %v", err)
	}
	if info == nil || !info.LoggedIn || info.ID != "acct" || info.UID != "uid-cookie" {
		t.Fatalf("返回信息不符: %+v", info)
	}
	rec, ok := store.record("acct")
	if !ok {
		t.Fatal("导入后库中没有会话行")
	}
	if rec.UID != "uid-cookie" {
		t.Fatalf("落库 UID=%q", rec.UID)
	}
	var parsed zcago.Credentials
	if err := json.Unmarshal(rec.Credentials, &parsed); err != nil {
		t.Fatalf("落库凭证不是 Credentials JSON: %v", err)
	}
	if parsed.IMEI != "snap-imei" {
		t.Fatalf("落库凭证应取会话快照的 IMEI，实得 %q", parsed.IMEI)
	}
	acc, ok := m.account("acct")
	if !ok || acc.currentConn() == nil {
		t.Fatal("导入后没有安装 conn")
	}
	if len(qrCanceled) != 1 || qrCanceled[0] != "acct" {
		t.Fatalf("QR 取消钩子调用=%v", qrCanceled)
	}
}

// TestZAManagerLoginWithCookieBadInput 断言解析失败不打接口、不创建账号。
func TestZAManagerLoginWithCookieBadInput(t *testing.T) {
	store := newFakeZAStore()
	sdk := newFakeZASDK()
	m := mgrNewTestManager(t, store, sdk, nil)
	ctx := context.Background()

	if _, err := m.LoginWithCookie(ctx, "acct", []byte(`{"nope":true}`)); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("坏凭证 err=%v，期望 ErrZaloBadCredentials", err)
	}
	if sdk.loginCount() != 0 {
		t.Fatal("解析失败仍调了登录")
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("解析失败仍创建了账号")
	}

	valid := []byte(`[{"name":"zpw_sek","value":"x","domain":"zalo.me"}]`)
	if _, err := m.LoginWithCookie(ctx, "..", valid); !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("非法 ID err=%v，期望 ErrZaloBadAccountID", err)
	}
	if sdk.loginCount() != 0 {
		t.Fatal("非法 ID 仍调了登录")
	}
}

// TestZAManagerInboundBroadcast 断言 listener 回调把 AccountID 填成本账号并广播
// 到 CallZaloMsg+"Inbound"。
func TestZAManagerInboundBroadcast(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	conn, ok := acc.currentConn().(*fakeZAConn)
	if !ok {
		t.Fatal("当前 conn 不是 fake")
	}
	mgrWaitTrue(t, "listener 启动", func() bool { return conn.startCount() == 1 })

	key := util.CallZaloMsg + "Inbound"
	got := make(chan InboundMessage, 4)
	id := util.DefaultCallFunc.Register(key, func(msg *InboundMessage) { got <- *msg })
	defer util.DefaultCallFunc.Unregister(key, id)

	conn.emitMessage(InboundMessage{ThreadID: "peer-1", ThreadType: "user", Text: "hello"})
	select {
	case msg := <-got:
		if msg.AccountID != "acct" {
			t.Fatalf("AccountID=%q，期望 acct", msg.AccountID)
		}
		if msg.ThreadID != "peer-1" || msg.Text != "hello" {
			t.Fatalf("广播载荷被改写: %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Inbound 回调未收到消息")
	}
}

// TestZAManagerListenerTerminalInvalidatesConn 断言 3003/3000 清 conn 让下次
// 收发重新登录，普通关闭码不清 conn。
func TestZAManagerListenerTerminalInvalidatesConn(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	first := acc.currentConn().(*fakeZAConn)
	mgrWaitTrue(t, "listener 启动", func() bool { return first.startCount() == 1 })

	first.emitTerminal(3003, "kicked")
	if acc.currentConn() != nil {
		t.Fatal("被踢后 conn 未清")
	}

	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("被踢后重登: %v", err)
	}
	second := acc.currentConn().(*fakeZAConn)
	if second == first {
		t.Fatal("重登应换新 conn")
	}
	mgrWaitTrue(t, "第二个 listener 启动", func() bool { return second.startCount() == 1 })

	second.emitTerminal(1000, "normal-close")
	if acc.currentConn() != second {
		t.Fatal("普通关闭码不应清 conn")
	}
}

// TestZAManagerGroupVersionDiff 覆盖：版本不变不调 GetGroupInfo、变化/新增才调、
// 移除项丢弃、缺资料返回错误且不更新版本表。
func TestZAManagerGroupVersionDiff(t *testing.T) {
	clock := newCacheClock()
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, clock)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	conn := acc.currentConn().(*fakeZAConn)
	ctx := context.Background()

	conn.setGroups(map[string]string{"g1": "v1", "g2": "v1"})
	conn.setGroupInfo(map[string]GroupInfo{
		"g1": {GroupID: "g1", Name: "Beta"},
		"g2": {GroupID: "g2", Name: "Alpha"},
	})

	got, err := acc.listGroups(ctx, false)
	if err != nil {
		t.Fatalf("首次 listGroups: %v", err)
	}
	if len(got) != 2 || got[0].GroupID != "g2" || got[1].GroupID != "g1" {
		t.Fatalf("首次结果/排序不符: %+v", got)
	}
	if got[0].Version != "v1" || got[1].Version != "v1" {
		t.Fatalf("Version 未填: %+v", got)
	}
	if reqs := conn.groupInfoRequests(); len(reqs) != 1 || len(reqs[0]) != 2 {
		t.Fatalf("首次应一次批量请求两个群: %v", reqs)
	}

	// TTL 内命中：不再打任何接口。
	if _, err := acc.listGroups(ctx, false); err != nil {
		t.Fatalf("二次 listGroups: %v", err)
	}
	if conn.groupsCallCount() != 1 || len(conn.groupInfoRequests()) != 1 {
		t.Fatalf("TTL 内又打了接口: groups=%d info=%v", conn.groupsCallCount(), conn.groupInfoRequests())
	}

	// TTL 过期但版本未变：只刷新版本表，不调 GetGroupInfo（资料缓存仍新鲜）。
	clock.Advance(5 * time.Minute)
	if _, err := acc.listGroups(ctx, false); err != nil {
		t.Fatalf("版本不变刷新: %v", err)
	}
	if conn.groupsCallCount() != 2 {
		t.Fatalf("GetAllGroups 调用 %d 次，期望 2", conn.groupsCallCount())
	}
	if len(conn.groupInfoRequests()) != 1 {
		t.Fatalf("版本不变却调了 GetGroupInfo: %v", conn.groupInfoRequests())
	}

	// 版本变化 + 新增 + 移除：只请变化的 g1 与新 g3，g2 从资料缓存丢弃。
	conn.setGroups(map[string]string{"g1": "v2", "g3": "v1"})
	conn.setGroupInfo(map[string]GroupInfo{
		"g1": {GroupID: "g1", Name: "Beta2"},
		"g3": {GroupID: "g3", Name: "Gamma"},
	})
	clock.Advance(5 * time.Minute)
	got, err = acc.listGroups(ctx, true)
	if err != nil {
		t.Fatalf("版本变化刷新: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("版本变化后群数=%d，期望 2: %+v", len(got), got)
	}
	for _, g := range got {
		if g.GroupID == "g2" {
			t.Fatalf("已移除的群仍在结果里: %+v", g)
		}
		if g.GroupID == "g1" && g.Version != "v2" {
			t.Fatalf("g1 Version=%q，期望 v2", g.Version)
		}
	}
	reqs := conn.groupInfoRequests()
	if len(reqs) != 2 {
		t.Fatalf("GetGroupInfo 调用 %d 次，期望 2", len(reqs))
	}
	if !reflect.DeepEqual(reqs[1], []string{"g1", "g3"}) {
		t.Fatalf("第二轮请求 ID=%v，期望 [g1 g3]", reqs[1])
	}
	if _, ok := acc.infoCache.CachedGroup("g2", m.limits.infoTTL, clock.Now()); ok {
		t.Fatal("已移除群的资料仍在缓存")
	}

	// 缺资料：返回错误、不交半份列表、不更新 groupVer。
	conn.setGroups(map[string]string{"g1": "v3"})
	conn.setGroupInfo(map[string]GroupInfo{})
	clock.Advance(5 * time.Minute)
	got, err = acc.listGroups(ctx, true)
	if err == nil {
		t.Fatal("缺资料应返回错误")
	}
	if got != nil {
		t.Fatalf("缺资料仍返回了半份列表: %+v", got)
	}
	if !strings.Contains(err.Error(), "群资料缺失") {
		t.Fatalf("缺资料错误文案=%q，期望包含「群资料缺失」", err.Error())
	}
	acc.groupVerMu.Lock()
	ver := make(map[string]string, len(acc.groupVer))
	for id, v := range acc.groupVer {
		ver[id] = v
	}
	acc.groupVerMu.Unlock()
	if !reflect.DeepEqual(ver, map[string]string{"g1": "v2", "g3": "v1"}) {
		t.Fatalf("失败刷新污染了 groupVer: %v", ver)
	}
}

// TestZAManagerListFriendsCacheWindow 断言好友列表 5 分钟内第二次不拉取，
// 强刷也受 5 分钟闸；刷新失败返回错误而不是空列表。
func TestZAManagerListFriendsCacheWindow(t *testing.T) {
	clock := newCacheClock()
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, clock)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	conn := acc.currentConn().(*fakeZAConn)
	conn.setFriends([]UserInfo{{UserID: "u1", DisplayName: "Alice"}})
	ctx := context.Background()

	first, err := acc.listFriends(ctx, false)
	if err != nil {
		t.Fatalf("首次 listFriends: %v", err)
	}
	if len(first) != 1 || first[0].UserID != "u1" {
		t.Fatalf("首次结果不符: %+v", first)
	}
	if _, err := acc.listFriends(ctx, false); err != nil {
		t.Fatalf("二次 listFriends: %v", err)
	}
	if got := conn.friendsCallCount(); got != 1 {
		t.Fatalf("5 分钟内第二次仍拉取，调用 %d 次", got)
	}

	clock.Advance(5 * time.Minute)
	if _, err := acc.listFriends(ctx, true); err != nil {
		t.Fatalf("过闸后强刷: %v", err)
	}
	if got := conn.friendsCallCount(); got != 2 {
		t.Fatalf("过闸后强刷未拉取，调用 %d 次", got)
	}
	if _, err := acc.listFriends(ctx, true); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("5 分钟内重复强刷 err=%v，期望 ErrZaloFetchTooSoon", err)
	}
	if got := conn.friendsCallCount(); got != 2 {
		t.Fatalf("被闸拒绝仍拉取，调用 %d 次", got)
	}

	conn.setFriendsErr(errMgrBoom)
	clock.Advance(5 * time.Minute)
	got, err := acc.listFriends(ctx, true)
	if !errors.Is(err, errMgrBoom) {
		t.Fatalf("刷新失败 err=%v", err)
	}
	if got != nil {
		t.Fatalf("刷新失败返回了列表: %+v", got)
	}
}

// TestZALimitsFromConfig 表驱动覆盖缺省值与下限 clamp。
func TestZALimitsFromConfig(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name string
		cfg  *config.ZaloSessionsConfig
		want zaLimits
	}{
		{
			name: "nil_config",
			cfg:  nil,
			want: zaLimits{
				qrTTL: 5 * time.Minute, requestTimeout: 30 * time.Second, listener: false,
				friendsTTL: 1 * time.Minute, groupsTTL: 1 * time.Minute,
				infoMin: 1500 * time.Millisecond, infoTTL: 10 * time.Minute,
				sendMin: 3 * time.Second, sendMax: 5 * time.Second, sendMaxPerHour: 30,
			},
		},
		{
			name: "below_floors",
			cfg: &config.ZaloSessionsConfig{
				QRTTLSec: 10, RequestTimeoutSec: 1, ListenerEnabled: boolPtr(false),
				FriendsCacheTTLSec: 60, GroupsCacheTTLSec: 60,
				InfoMinIntervalMs: 100, InfoCacheTTLSec: 60,
				SendMinDelayMs: 100, SendMaxDelayMs: 200, SendMaxPerHour: -1,
				UserAgent: "  ua-x  ", Language: " vi ",
			},
			want: zaLimits{
				qrTTL: 2 * time.Minute, requestTimeout: 5 * time.Second, listener: false,
				friendsTTL: 1 * time.Minute, groupsTTL: 1 * time.Minute,
				infoMin: 1500 * time.Millisecond, infoTTL: 10 * time.Minute,
				sendMin: 1 * time.Second, sendMax: 1 * time.Second, sendMaxPerHour: 30,
				userAgent: "ua-x", language: "vi",
			},
		},
		{
			name: "explicit_values",
			cfg: &config.ZaloSessionsConfig{
				QRTTLSec: 600, RequestTimeoutSec: 60, ListenerEnabled: boolPtr(true),
				FriendsCacheTTLSec: 900, GroupsCacheTTLSec: 1200,
				InfoMinIntervalMs: 2000, InfoCacheTTLSec: 1800,
				SendMinDelayMs: 4000, SendMaxDelayMs: 7000, SendMaxPerHour: 50,
				UserAgent: "ua-y", Language: "en",
			},
			want: zaLimits{
				qrTTL: 10 * time.Minute, requestTimeout: time.Minute, listener: true,
				friendsTTL: 15 * time.Minute, groupsTTL: 20 * time.Minute,
				infoMin: 2 * time.Second, infoTTL: 30 * time.Minute,
				sendMin: 4 * time.Second, sendMax: 7 * time.Second, sendMaxPerHour: 50,
				userAgent: "ua-y", language: "en",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := zaLimitsFromConfig(tc.cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("zaLimitsFromConfig=%+v，期望 %+v", got, tc.want)
			}
		})
	}
}

// TestZARandomDelay 断言 max<=min 时返回 min，其余落在闭区间内。
func TestZARandomDelay(t *testing.T) {
	if got := zaRandomDelay(2*time.Second, time.Second); got != 2*time.Second {
		t.Fatalf("max<min 返回 %v，期望 min", got)
	}
	if got := zaRandomDelay(time.Second, time.Second); got != time.Second {
		t.Fatalf("max==min 返回 %v，期望 min", got)
	}
	for i := 0; i < 500; i++ {
		d := zaRandomDelay(3*time.Second, 5*time.Second)
		if d < 3*time.Second || d > 5*time.Second {
			t.Fatalf("随机间隔越界: %v", d)
		}
	}
}

// TestZACurrentManager 断言未启动返回哨兵、存入后可读，且构造器补全 nil 依赖。
func TestZACurrentManager(t *testing.T) {
	old := zaMgr.Load()
	zaMgr.Store(nil)
	defer zaMgr.Store(old)

	if _, err := currentManager(); !errors.Is(err, ErrZaloNotStarted) {
		t.Fatalf("未启动 currentManager err=%v", err)
	}

	store := newFakeZAStore()
	m := newZAManager(nil, mgrTestLimits(), store, newFakeZASDK(), nil, nil)
	defer m.Stop()
	if m.root == nil || m.clock == nil || m.randDelay == nil || m.cancel == nil {
		t.Fatal("newZAManager 未补全 nil 依赖")
	}

	zaMgr.Store(m)
	got, err := currentManager()
	if err != nil || got != m {
		t.Fatalf("currentManager=%v err=%v，期望存入的 manager", got, err)
	}
}
