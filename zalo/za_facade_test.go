package zalo

// 本文件是 za_facade.go 的活闸：门面前缀（未启动/非法账号 id）、发送路由与参数、
// 发送闸（首条不等、随机间隔、小时配额、失败占槽、等待取消不占槽）、好友/群/资料
// 缓存经门面的行为，以及账号类门面的委托。
//
// 全部用 fake store/SDK/conn 与假时钟（cacheClock / facBlockClock），不连网、不读
// 真实时间；fake store/SDK/conn 复用 za_manager_test.go 的定义，不修改该文件。
//
// ZaloLogout 的委托用例在 zalo_test.go（TestZaloLogoutClearsConnKeepsStore）：
// 旧供应商层删除后新签名已落地，本文件只覆盖其余门面与发送闸。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrakk/zcago"
)

// errFacBoom 是门面测试注入的通用失败，用于断言 errors.Is 能穿过包装。
var errFacBoom = errors.New("facade test boom")

// facRecordedSend 是一次到达 conn.SendText 的发送记录。
type facRecordedSend struct {
	group    bool
	threadID string
	text     string
}

// facConn 在 fakeZAConn 之上覆盖 SendText：记录 group/threadID/text，返回可注入的
// msgID/error。其余方法（UID/快照/好友/群/listener）由嵌入的 fakeZAConn 提供。
type facConn struct {
	*fakeZAConn

	recMu   sync.Mutex
	sends   []facRecordedSend
	msgID   string
	sendErr error
}

var _ zaConn = (*facConn)(nil)

// SendText 记录本次调用的全部参数并返回注入结果。
func (c *facConn) SendText(_ context.Context, threadID string, group bool, text string) (string, error) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.sends = append(c.sends, facRecordedSend{group: group, threadID: threadID, text: text})
	if c.sendErr != nil {
		return "", c.sendErr
	}
	return c.msgID, nil
}

// recorded 返回发送记录副本。
func (c *facConn) recorded() []facRecordedSend {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	return append([]facRecordedSend(nil), c.sends...)
}

// setSendMsgID 设置 SendText 返回的 msgID。
func (c *facConn) setSendMsgID(id string) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.msgID = id
}

// setSendErr 设置 SendText 返回的错误（nil 表示恢复正常）。
func (c *facConn) setSendErr(err error) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.sendErr = err
}

// setUsers 写入 GetUserInfo 的返回数据；fakeZAConn 只提供 GetUserInfo 读取器，
// 没有对应 setter，这里在 fake 的锁内补齐。
func (c *facConn) setUsers(users map[string]UserInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users = users
}

// userInfoCallCount 返回 GetUserInfo 的请求批次数。
func (c *facConn) userInfoCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.userInfoCalls)
}

// facSDK 是 zaSDK 的假件：每次 Login 返回一个新的 *facConn，可注入登录错误与
// conn 初始化回调（用于预先塞好友/群/资料数据）。
type facSDK struct {
	mu         sync.Mutex
	loginCalls int
	conns      []*facConn
	loginErr   error
	configure  func(*facConn)
}

var _ zaSDK = (*facSDK)(nil)

// Login 记录调用并按注入行为返回新 conn。
func (s *facSDK) Login(_ context.Context, _ zcago.Credentials) (zaConn, error) {
	s.mu.Lock()
	s.loginCalls++
	err := s.loginErr
	configure := s.configure
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c := &facConn{
		fakeZAConn: &fakeZAConn{
			uid:  "uid-fac",
			snap: zcago.Credentials{IMEI: "imei-fac", UserAgent: "ua-fac"},
		},
		msgID: "msg-fac",
	}
	if configure != nil {
		configure(c)
	}
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
	return c, nil
}

// LoginQR 不在本文件使用。
func (s *facSDK) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zaConn, error) {
	return nil, errors.New("facade test: LoginQR 未实现")
}

// loginCount 返回 Login 调用次数。
func (s *facSDK) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginCalls
}

// lastConn 返回最近一次登录得到的 conn；未登录返回 nil。
func (s *facSDK) lastConn() *facConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return nil
	}
	return s.conns[len(s.conns)-1]
}

// facBlockClock 是可在 Sleep 中阻塞的假时钟：block 为 true 时 Sleep 等到 ctx 取消
// 或测试显式 unblock；Now/Advance/Sleeps 复用 cacheClock。
type facBlockClock struct {
	*cacheClock

	mu      sync.Mutex
	block   bool
	entered chan struct{}
}

// newFacBlockClock 构造会阻塞 Sleep 的假时钟。
func newFacBlockClock() *facBlockClock {
	return &facBlockClock{cacheClock: newCacheClock(), block: true, entered: make(chan struct{}, 4)}
}

// Sleep 在 block 为 true 时阻塞，否则退化为 cacheClock 的记录语义。
func (c *facBlockClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	block := c.block
	c.mu.Unlock()
	if !block {
		return c.cacheClock.Sleep(ctx, d)
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("facade test: 等待取消超时")
	}
}

// unblock 让后续 Sleep 立即返回。
func (c *facBlockClock) unblock() {
	c.mu.Lock()
	c.block = false
	c.mu.Unlock()
}

// facNewManager 构造 manager 并登记停机清理；clock 为 nil 时用可控假时钟。
func facNewManager(t *testing.T, limits zaLimits, clock zaClock) (*zaManager, *fakeZAStore, *facSDK) {
	t.Helper()
	if clock == nil {
		clock = newCacheClock()
	}
	store := newFakeZAStore()
	sdk := &facSDK{}
	m := newZAManager(context.Background(), limits, store, sdk, clock, nil)
	t.Cleanup(m.Stop)
	return m, store, sdk
}

// facInstall 把 manager 装进进程级快照，并在测试结束时清空（先于 Stop 执行）。
func facInstall(t *testing.T, m *zaManager) {
	t.Helper()
	zaMgr.Store(m)
	t.Cleanup(func() { zaMgr.Store(nil) })
}

// facNewSendFixture 构造可发送的账号：库里有一行凭证、随机间隔固定为 0，
// maxPerHour>0 时覆盖小时上限。
func facNewSendFixture(t *testing.T, maxPerHour int) (*zaManager, *facSDK, *cacheClock) {
	t.Helper()
	limits := mgrTestLimits()
	if maxPerHour > 0 {
		limits.sendMaxPerHour = maxPerHour
	}
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	m.randDelay = func(time.Duration, time.Duration) time.Duration { return 0 }
	facInstall(t, m)
	return m, sdk, clock
}

// TestZAFacadeNotStarted 断言通道未启动时全部公开入口返回 ErrZaloNotStarted。
func TestZAFacadeNotStarted(t *testing.T) {
	old := zaMgr.Load()
	zaMgr.Store(nil)
	t.Cleanup(func() { zaMgr.Store(old) })

	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{name: "login_with_cookie", call: func() error {
			_, err := ZaloLoginWithCookie(ctx, "acct", []byte("[]"))
			return err
		}},
		{name: "start_client", call: func() error {
			_, err := ZaloStartClient(ctx, "acct")
			return err
		}},
		{name: "delete_account", call: func() error { return ZaloDeleteAccount(ctx, "acct") }},
		{name: "list_accounts", call: func() error {
			_, err := ZaloListAccounts(ctx)
			return err
		}},
		{name: "send_message", call: func() error {
			_, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello")
			return err
		}},
		{name: "send_group_message", call: func() error {
			_, err := ZaloSendGroupMessage(ctx, "acct", "group-1", "hello")
			return err
		}},
		{name: "delete_chat", call: func() error {
			return ZaloDeleteChat(ctx, "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m1"})
		}},
		{name: "list_friends", call: func() error {
			_, err := ZaloListFriends(ctx, "acct")
			return err
		}},
		{name: "sync_friends", call: func() error {
			_, err := ZaloSyncFriends(ctx, "acct")
			return err
		}},
		{name: "list_groups", call: func() error {
			_, err := ZaloListGroups(ctx, "acct")
			return err
		}},
		{name: "sync_groups", call: func() error {
			_, err := ZaloSyncGroups(ctx, "acct")
			return err
		}},
		{name: "get_user_info", call: func() error {
			_, err := ZaloGetUserInfo(ctx, "acct", "u1")
			return err
		}},
		{name: "get_group_info", call: func() error {
			_, err := ZaloGetGroupInfo(ctx, "acct", "g1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrZaloNotStarted) {
				t.Fatalf("未启动 err=%v，期望 ErrZaloNotStarted", err)
			}
		})
	}
}

// TestZAFacadeBadAccountID 断言通道已启动时非法 accountID 一律返回
// ErrZaloBadAccountID，且不触发任何登录/发送。
func TestZAFacadeBadAccountID(t *testing.T) {
	m, store, sdk := facNewManager(t, mgrTestLimits(), nil)
	facInstall(t, m)

	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{name: "login_with_cookie", call: func() error {
			_, err := ZaloLoginWithCookie(ctx, "..", []byte(`[{"name":"a","value":"b","domain":"zalo.me"}]`))
			return err
		}},
		{name: "start_client", call: func() error {
			_, err := ZaloStartClient(ctx, "..")
			return err
		}},
		{name: "delete_account", call: func() error { return ZaloDeleteAccount(ctx, "..") }},
		{name: "send_message", call: func() error {
			_, err := ZaloSendMessage(ctx, "..", "peer-1", "hello")
			return err
		}},
		{name: "send_group_message", call: func() error {
			_, err := ZaloSendGroupMessage(ctx, "..", "group-1", "hello")
			return err
		}},
		{name: "delete_chat", call: func() error {
			return ZaloDeleteChat(ctx, "..", "peer-1", false, ChatBoundary{GlobalMsgID: "m1"})
		}},
		{name: "list_friends", call: func() error {
			_, err := ZaloListFriends(ctx, "..")
			return err
		}},
		{name: "sync_friends", call: func() error {
			_, err := ZaloSyncFriends(ctx, "..")
			return err
		}},
		{name: "list_groups", call: func() error {
			_, err := ZaloListGroups(ctx, "..")
			return err
		}},
		{name: "sync_groups", call: func() error {
			_, err := ZaloSyncGroups(ctx, "..")
			return err
		}},
		{name: "get_user_info", call: func() error {
			_, err := ZaloGetUserInfo(ctx, "..", "u1")
			return err
		}},
		{name: "get_group_info", call: func() error {
			_, err := ZaloGetGroupInfo(ctx, "..", "g1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrZaloBadAccountID) {
				t.Fatalf("非法账号 id err=%v，期望 ErrZaloBadAccountID", err)
			}
		})
	}
	if got := sdk.loginCount(); got != 0 {
		t.Fatalf("非法 id 触发了 %d 次登录", got)
	}
	if got := store.upsertCount(); got != 0 {
		t.Fatalf("非法 id 写了 %d 次库", got)
	}
}

// TestZAFacadeSendRoutesThread 断言单聊/群聊把 group 与 threadID 正确传给
// conn.SendText，并原样返回 msgID。
func TestZAFacadeSendRoutesThread(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	ctx := context.Background()

	id, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello")
	if err != nil {
		t.Fatalf("ZaloSendMessage: %v", err)
	}
	if id != "msg-fac" {
		t.Fatalf("msgID=%q，期望 msg-fac", id)
	}
	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("发送后没有 conn")
	}

	id, err = ZaloSendGroupMessage(ctx, "acct", "group-1", "hi group")
	if err != nil {
		t.Fatalf("ZaloSendGroupMessage: %v", err)
	}
	if id != "msg-fac" {
		t.Fatalf("群聊 msgID=%q，期望 msg-fac", id)
	}

	sends := conn.recorded()
	if len(sends) != 2 {
		t.Fatalf("SendText 调用 %d 次，期望 2", len(sends))
	}
	if sends[0].group {
		t.Fatalf("单聊被标成群聊: %+v", sends[0])
	}
	if sends[0].threadID != "peer-1" || sends[0].text != "hello" {
		t.Fatalf("单聊参数不符: %+v", sends[0])
	}
	if !sends[1].group {
		t.Fatalf("群聊没有标 group: %+v", sends[1])
	}
	if sends[1].threadID != "group-1" || sends[1].text != "hi group" {
		t.Fatalf("群聊参数不符: %+v", sends[1])
	}
}

// TestZAFacadeSendRejectsBadInput 覆盖空正文、空 threadID、空 msgID、conn 错误，
// 并断言错误文案不回显正文。
func TestZAFacadeSendRejectsBadInput(t *testing.T) {
	t.Run("empty_text", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		_, err := ZaloSendMessage(context.Background(), "acct", "peer-1", "  \n\t ")
		if !errors.Is(err, errZAFacadeEmptyText) {
			t.Fatalf("空正文 err=%v，期望 errZAFacadeEmptyText", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recorded()) != 0 {
			t.Fatalf("空正文仍调了 SendText: %+v", conn.recorded())
		}
	})

	t.Run("empty_thread", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		_, err := ZaloSendGroupMessage(context.Background(), "acct", "   ", "hello")
		if !errors.Is(err, errZAFacadeEmptyThread) {
			t.Fatalf("空 threadID err=%v，期望 errZAFacadeEmptyThread", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recorded()) != 0 {
			t.Fatalf("空 threadID 仍调了 SendText: %+v", conn.recorded())
		}
	})

	t.Run("empty_msg_id", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.setSendMsgID("  ") }
		_, err := ZaloSendMessage(context.Background(), "acct", "peer-1", "hello")
		if !errors.Is(err, ErrZaloEmptyMessageID) {
			t.Fatalf("空 msgID err=%v，期望 ErrZaloEmptyMessageID", err)
		}
	})

	t.Run("conn_error_not_echo_body", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.setSendErr(errFacBoom) }
		const body = "S3CRET-BODY-9182"
		_, err := ZaloSendMessage(context.Background(), "acct", "peer-1", body)
		if !errors.Is(err, errFacBoom) {
			t.Fatalf("conn 错误 err=%v，期望可 errors.Is(errFacBoom)", err)
		}
		if strings.Contains(err.Error(), body) {
			t.Fatalf("错误文案回显了正文: %q", err.Error())
		}
	})
}

// TestZAFacadeSendDelay 断言首条不等待、第二条睡眠在 [sendMin,sendMax] 内，
// 且随机间隔参数取自 limits。
func TestZAFacadeSendDelay(t *testing.T) {
	limits := mgrTestLimits()
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	var (
		delayCalls int
		gotMin     time.Duration
		gotMax     time.Duration
	)
	m.randDelay = func(min, max time.Duration) time.Duration {
		delayCalls++
		gotMin, gotMax = min, max
		return 4 * time.Second
	}
	facInstall(t, m)
	ctx := context.Background()

	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "first"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}
	if got := clock.Sleeps(); len(got) != 0 {
		t.Fatalf("首条不应等待，实际睡眠 %v", got)
	}
	if delayCalls != 0 {
		t.Fatalf("首条不应计算随机间隔，实际调用 %d 次", delayCalls)
	}

	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "second"); err != nil {
		t.Fatalf("第二条发送: %v", err)
	}
	if delayCalls != 1 {
		t.Fatalf("第二条应计算一次随机间隔，实际 %d 次", delayCalls)
	}
	if gotMin != limits.sendMin || gotMax != limits.sendMax {
		t.Fatalf("间隔参数=(%v,%v)，期望 (%v,%v)", gotMin, gotMax, limits.sendMin, limits.sendMax)
	}
	sleeps := clock.Sleeps()
	if len(sleeps) != 1 || sleeps[0] != 4*time.Second {
		t.Fatalf("睡眠记录=%v，期望 [4s]", sleeps)
	}
	if conn := sdk.lastConn(); len(conn.recorded()) != 2 {
		t.Fatalf("SendText 调用 %d 次，期望 2", len(conn.recorded()))
	}
}

// TestZAFacadeSendHourlyLimit 覆盖滚动小时配额：第 N+1 条立即被拒、失败发送也占槽、
// 恰好满一小时的时间戳被剪掉。
func TestZAFacadeSendHourlyLimit(t *testing.T) {
	t.Run("third_rejected", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 2)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "ok"); err != nil {
				t.Fatalf("第 %d 条发送: %v", i+1, err)
			}
		}
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "over"); !errors.Is(err, ErrZaloSendLimit) {
			t.Fatalf("第 3 条 err=%v，期望 ErrZaloSendLimit", err)
		}
		if got := len(sdk.lastConn().recorded()); got != 2 {
			t.Fatalf("被拒后 SendText 调用 %d 次，期望 2", got)
		}
	})

	t.Run("failed_send_uses_slot", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 2)
		sdk.configure = func(c *facConn) { c.setSendErr(errFacBoom) }
		ctx := context.Background()

		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "boom"); !errors.Is(err, errFacBoom) {
			t.Fatalf("首次失败 err=%v，期望注入错误", err)
		}
		conn := sdk.lastConn()
		conn.setSendErr(nil)
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "ok"); err != nil {
			t.Fatalf("第二次发送: %v", err)
		}
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "over"); !errors.Is(err, ErrZaloSendLimit) {
			t.Fatalf("失败发送未占槽，第 3 条 err=%v", err)
		}
	})

	t.Run("window_prunes_at_boundary", func(t *testing.T) {
		_, _, clock := facNewSendFixture(t, 2)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "ok"); err != nil {
				t.Fatalf("第 %d 条发送: %v", i+1, err)
			}
		}
		clock.Advance(time.Hour)
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "after-hour"); err != nil {
			t.Fatalf("满一小时后发送: %v", err)
		}
	})
}

// TestZAFacadeSendCancelDuringDelay 断言随机间隔睡眠被 ctx 取消时不占小时槽、
// 不推进 lastSend，且闸在取消后仍可继续工作。
func TestZAFacadeSendCancelDuringDelay(t *testing.T) {
	limits := mgrTestLimits()
	clock := newFacBlockClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	m.randDelay = func(time.Duration, time.Duration) time.Duration { return 4 * time.Second }
	facInstall(t, m)

	start := clock.Now()
	if _, err := ZaloSendMessage(context.Background(), "acct", "peer-1", "first"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}
	clock.Advance(7 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ZaloSendMessage(ctx, "acct", "peer-2", "second")
		done <- err
	}()
	select {
	case <-clock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第二条没有进入随机间隔等待")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("等待取消 err=%v，期望 context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后发送未返回")
	}

	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("账号不在内存")
	}
	acc.sendMu.Lock()
	windowLen := len(acc.sendWindow)
	lastSend := acc.lastSend
	acc.sendMu.Unlock()
	if windowLen != 1 {
		t.Fatalf("取消的发送占了小时槽：窗口 %d 条，期望 1", windowLen)
	}
	if !lastSend.Equal(start) {
		t.Fatalf("取消的发送推进了 lastSend=%v，期望仍为 %v", lastSend, start)
	}
	if got := len(sdk.lastConn().recorded()); got != 1 {
		t.Fatalf("取消的发送打到了 SendText：调用 %d 次，期望 1", got)
	}

	clock.unblock()
	if _, err := ZaloSendMessage(context.Background(), "acct", "peer-3", "third"); err != nil {
		t.Fatalf("取消后继续发送: %v", err)
	}
	acc.sendMu.Lock()
	windowLen = len(acc.sendWindow)
	acc.sendMu.Unlock()
	if windowLen != 2 {
		t.Fatalf("恢复发送后窗口 %d 条，期望 2", windowLen)
	}
}

// TestZAFacadeFriendsCache 断言好友列表 5 分钟缓存经门面生效：两次 List 只打一次
// 接口，间隔内的 Sync 返回 ErrZaloFetchTooSoon，过闸后可强刷。
func TestZAFacadeFriendsCache(t *testing.T) {
	limits := mgrTestLimits()
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk.configure = func(c *facConn) {
		c.setFriends([]UserInfo{{UserID: "u1", DisplayName: "Alice"}})
	}
	facInstall(t, m)
	ctx := context.Background()

	first, err := ZaloListFriends(ctx, "acct")
	if err != nil {
		t.Fatalf("首次 ListFriends: %v", err)
	}
	if len(first) != 1 || first[0].UserID != "u1" {
		t.Fatalf("首次好友列表不符: %+v", first)
	}
	second, err := ZaloListFriends(ctx, "acct")
	if err != nil {
		t.Fatalf("二次 ListFriends: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("二次好友列表不符: %+v", second)
	}
	conn := sdk.lastConn()
	if got := conn.friendsCallCount(); got != 1 {
		t.Fatalf("5 分钟内 List 两次打了 %d 次接口，期望 1", got)
	}
	if _, err := ZaloSyncFriends(ctx, "acct"); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("间隔内 SyncFriends err=%v，期望 ErrZaloFetchTooSoon", err)
	}
	if got := conn.friendsCallCount(); got != 1 {
		t.Fatalf("被闸拒绝仍拉取：%d 次", got)
	}

	clock.Advance(limits.friendsTTL)
	synced, err := ZaloSyncFriends(ctx, "acct")
	if err != nil {
		t.Fatalf("过闸后 SyncFriends: %v", err)
	}
	if len(synced) != 1 {
		t.Fatalf("过闸后好友列表不符: %+v", synced)
	}
	if got := conn.friendsCallCount(); got != 2 {
		t.Fatalf("过闸后 SyncFriends 未拉取：%d 次，期望 2", got)
	}
}

// TestZAFacadeGroupsAndInfo 断言群列表缓存/同步闸与用户、群资料的批量查询经门面
// 正确透传，资料缓存命中不再打接口。
func TestZAFacadeGroupsAndInfo(t *testing.T) {
	limits := mgrTestLimits()
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk.configure = func(c *facConn) {
		c.setGroups(map[string]string{"g1": "v1", "g2": "v1"})
		c.setGroupInfo(map[string]GroupInfo{
			"g1": {GroupID: "g1", Name: "Alpha"},
			"g2": {GroupID: "g2", Name: "Beta"},
		})
		c.setUsers(map[string]UserInfo{"u1": {UserID: "u1", DisplayName: "Alice"}})
	}
	facInstall(t, m)
	ctx := context.Background()

	groups, err := ZaloListGroups(ctx, "acct")
	if err != nil {
		t.Fatalf("ZaloListGroups: %v", err)
	}
	if len(groups) != 2 || groups[0].Name != "Alpha" || groups[1].Name != "Beta" {
		t.Fatalf("群列表不符: %+v", groups)
	}
	if groups[0].Version != "v1" {
		t.Fatalf("群版本未填: %+v", groups[0])
	}
	conn := sdk.lastConn()
	if got := conn.groupsCallCount(); got != 1 {
		t.Fatalf("首次 ListGroups 调用 GetAllGroups %d 次，期望 1", got)
	}
	if _, err := ZaloListGroups(ctx, "acct"); err != nil {
		t.Fatalf("二次 ZaloListGroups: %v", err)
	}
	if got := conn.groupsCallCount(); got != 1 {
		t.Fatalf("TTL 内二次 ListGroups 又拉取：%d 次", got)
	}
	if _, err := ZaloSyncGroups(ctx, "acct"); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("间隔内 SyncGroups err=%v，期望 ErrZaloFetchTooSoon", err)
	}

	users, err := ZaloGetUserInfo(ctx, "acct", "u1")
	if err != nil {
		t.Fatalf("ZaloGetUserInfo: %v", err)
	}
	if len(users) != 1 || users["u1"].DisplayName != "Alice" {
		t.Fatalf("用户资料不符: %+v", users)
	}
	if _, err := ZaloGetUserInfo(ctx, "acct", "u1"); err != nil {
		t.Fatalf("二次 ZaloGetUserInfo: %v", err)
	}
	if got := conn.userInfoCallCount(); got != 1 {
		t.Fatalf("资料缓存命中仍请求：%d 批，期望 1", got)
	}

	infos, err := ZaloGetGroupInfo(ctx, "acct", "g1")
	if err != nil {
		t.Fatalf("ZaloGetGroupInfo: %v", err)
	}
	if len(infos) != 1 || infos["g1"].Name != "Alpha" {
		t.Fatalf("群资料不符: %+v", infos)
	}
	if got := len(conn.groupInfoRequests()); got != 1 {
		t.Fatalf("群资料缓存命中仍请求：%d 批，期望 1", got)
	}
}

// TestZAFacadeAccountDelegation 覆盖账号类门面的委托：cookie 导入、显式拉起、
// 删除、列表合并，以及坏凭证/无凭证/查库失败的错误透传。
func TestZAFacadeAccountDelegation(t *testing.T) {
	m, store, sdk := facNewManager(t, mgrTestLimits(), nil)
	facInstall(t, m)
	ctx := context.Background()

	raw := []byte(`[{"name":"zpw_sek","value":"not-a-real-token","domain":"zalo.me"}]`)
	info, err := ZaloLoginWithCookie(ctx, "acct", raw)
	if err != nil {
		t.Fatalf("ZaloLoginWithCookie: %v", err)
	}
	if info == nil || info.ID != "acct" || info.UID != "uid-fac" || !info.LoggedIn {
		t.Fatalf("导入返回信息不符: %+v", info)
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("导入后库里没有会话行")
	}
	if got := sdk.loginCount(); got != 1 {
		t.Fatalf("导入触发登录 %d 次，期望 1", got)
	}

	list, err := ZaloListAccounts(ctx)
	if err != nil {
		t.Fatalf("ZaloListAccounts: %v", err)
	}
	if len(list) != 1 || list[0].ID != "acct" || !list[0].LoggedIn {
		t.Fatalf("列表不符: %+v", list)
	}

	store.seed("seeded", "uid-seeded", []byte(mgrTestCredJSON))
	seeded, err := ZaloStartClient(ctx, "seeded")
	if err != nil {
		t.Fatalf("ZaloStartClient: %v", err)
	}
	if seeded == nil || seeded.ID != "seeded" || !seeded.LoggedIn {
		t.Fatalf("StartClient 返回不符: %+v", seeded)
	}

	list, err = ZaloListAccounts(ctx)
	if err != nil {
		t.Fatalf("二次 ZaloListAccounts: %v", err)
	}
	if len(list) != 2 || list[0].ID != "acct" || list[1].ID != "seeded" {
		t.Fatalf("列表排序/内容不符: %+v", list)
	}

	if err := ZaloDeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("ZaloDeleteAccount: %v", err)
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("删除后库行仍在")
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("删除后账号仍在内存")
	}

	if _, err := ZaloStartClient(ctx, "ghost"); !errors.Is(err, ErrZaloNotLoggedIn) {
		t.Fatalf("无凭证 StartClient err=%v，期望 ErrZaloNotLoggedIn", err)
	}
	if _, err := ZaloLoginWithCookie(ctx, "bad", []byte(`{"nope":true}`)); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("坏凭证 err=%v，期望 ErrZaloBadCredentials", err)
	}

	store.mu.Lock()
	store.listErr = errFacBoom
	store.mu.Unlock()
	if got, err := ZaloListAccounts(ctx); !errors.Is(err, errFacBoom) {
		t.Fatalf("查库失败 err=%v，期望注入错误", err)
	} else if got != nil {
		t.Fatalf("查库失败返回了半份列表: %+v", got)
	}
}
