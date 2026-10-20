package zalo

// 本文件是安全加固的活闸：
//   - 登出/删除期间的在飞登录不得在事后写库或安装 conn（登录代际）；
//   - 删库失败必须解除 stopped，账号可重新使用；
//   - 发送在 3-5 秒睡眠期间被登出/删除后，不得再真正发出。
//
// 复用 manager/facade 测试的假件，不连网、不连库。

import (
	"context"
	"errors"
	"testing"
	"time"
)

// secRelease 把一次性 close 包成可重复调用的释放函数，避免测试失败路径泄漏
// 一个永远等 gate 的登录协程。
func secRelease(gate chan struct{}) func() {
	var once = make(chan struct{})
	return func() {
		select {
		case <-once:
			return
		default:
			close(once)
			close(gate)
		}
	}
}

// TestZASecLogoutDuringLoginNoInstall 在飞登录被 Logout 取代后，即使 SDK 最终
// 返回了 conn，也不能安装（代际检查），库行仍保留（Logout 的语义）。
func TestZASecLogoutDuringLoginNoInstall(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sdk.gate = gate
	sdk.entered = entered
	release := secRelease(gate)
	defer release()

	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	loginDone := make(chan error, 1)
	go func() {
		_, err := acc.ensureConn(context.Background())
		loginDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("登录未进入 SDK")
	}

	logoutDone := make(chan error, 1)
	go func() { logoutDone <- m.Logout(context.Background(), "acct") }()
	mgrWaitTrue(t, "Logout 递增登录代际", func() bool {
		acc.mu.Lock()
		defer acc.mu.Unlock()
		return acc.loginEpoch >= 1
	})

	release()
	if err := <-loginDone; err == nil {
		t.Fatal("被 Logout 取代的登录应返回错误")
	}
	if err := <-logoutDone; err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if acc.currentConn() != nil {
		t.Fatal("Logout 后仍安装了在飞登录的 conn")
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("Logout 不应删除会话库行")
	}
}

// TestZASecDeleteDuringLoginNoResurrect 在飞登录被 Delete 取代后不得复活库行，
// 也不得安装 conn。
func TestZASecDeleteDuringLoginNoResurrect(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sdk.gate = gate
	sdk.entered = entered
	release := secRelease(gate)
	defer release()

	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	loginDone := make(chan error, 1)
	go func() {
		_, err := acc.ensureConn(context.Background())
		loginDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("登录未进入 SDK")
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- m.DeleteAccount(context.Background(), "acct") }()
	mgrWaitTrue(t, "Delete 标记 stopped", func() bool {
		acc.mu.Lock()
		defer acc.mu.Unlock()
		return acc.stopped
	})

	release()
	if err := <-loginDone; err == nil {
		t.Fatal("被 Delete 取代的登录应返回错误")
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("删除成功后账号仍在内存")
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("删除后库行被在飞登录复活")
	}
	if acc.currentConn() != nil {
		t.Fatal("删除后仍安装了在飞登录的 conn")
	}
}

// TestZASecDeleteFailureAllowsReuse 删库失败必须解除 stopped：账号保留、可重新
// 登录使用，而不是在本进程里永久不可用。
func TestZASecDeleteFailureAllowsReuse(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	m := mgrNewTestManager(t, store, sdk, nil)
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
	if acc.stopped {
		t.Fatal("删库失败后账号仍处于 stopped，无法恢复")
	}
	if acc.currentConn() != nil {
		t.Fatal("删库失败后仍留着旧 conn")
	}

	store.mu.Lock()
	store.deleteErr = nil
	store.mu.Unlock()
	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("删库失败后重新登录: %v", err)
	}
	if acc.currentConn() == nil {
		t.Fatal("重新登录后没有 conn")
	}
}

// secBlockClock 在 Sleep 中阻塞到测试释放；用于把发送卡在 3-5 秒间隔里。
type secBlockClock struct {
	*cacheClock
	entered chan struct{}
	release chan struct{}
}

func newSecBlockClock() *secBlockClock {
	return &secBlockClock{
		cacheClock: newCacheClock(),
		entered:    make(chan struct{}, 1),
		release:    make(chan struct{}),
	}
}

func (c *secBlockClock) Sleep(ctx context.Context, _ time.Duration) error {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return nil
	}
}

// TestZASecSendAfterLogoutRejected 发送卡在间隔睡眠时登出：醒来后必须复核 conn，
// 不得再用旧 conn 发出第二条。
func TestZASecSendAfterLogoutRejected(t *testing.T) {
	clock := newSecBlockClock()
	limits := mgrTestLimits()
	limits.sendMin = time.Millisecond
	limits.sendMax = time.Millisecond
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	facInstall(t, m)

	ctx := context.Background()
	if _, err := ZaloSendMessage(ctx, "acct", "peer", "one"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}

	sendDone := make(chan error, 1)
	go func() {
		_, err := ZaloSendMessage(ctx, "acct", "peer", "two")
		sendDone <- err
	}()
	select {
	case <-clock.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第二条发送未进入间隔睡眠")
	}

	if err := ZaloLogout(ctx, "acct"); err != nil {
		t.Fatalf("ZaloLogout: %v", err)
	}
	close(clock.release)

	err := <-sendDone
	if !errors.Is(err, ErrZaloNotLoggedIn) && !errors.Is(err, ErrZaloNotStarted) {
		t.Fatalf("登出后发送 err=%v，期望未登录/未启动", err)
	}
	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("没有记录到登录 conn")
	}
	if got := len(conn.recorded()); got != 1 {
		t.Fatalf("登出后仍发出了消息: sends=%d，期望 1", got)
	}
}
