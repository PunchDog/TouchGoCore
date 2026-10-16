package whatsapp

// 本文件是合流批（Lead）为 MG 缺陷写的「先红后绿」闸。
//
// 缺陷：`syncContactAppState` 用 `context.WithTimeout(ctx, waContactSyncTimeout)` 造的是
// **本包自己的**同步预算（缺省 20 秒）。它到点时返回的错误里带 `context.DeadlineExceeded`，
// 而 `waLoginErr` 只看「是不是 DeadlineExceeded」就折成
// 「调用方上下文已结束（请求超时或客户端断开），本条未执行」——等于把自家超时误判成上游断连，
// 与本轮刚立的「取消与到点两支必须分开、值班不得读成同一件事」纪律自相矛盾
// （T10 报告观察 6、T11 观察 1 均登记未测，按纪律留给 Lead 连修带闸一起做）。
//
// 两条用例分别钉两个方向：内部超时不许说成调用方断连；调用方真断连时仍要说成调用方断连。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestSyncSelfTimeoutIsNotBlamedOnCaller(t *testing.T) {
	dev := fakeDevice(t, "447378152319:21@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncGate = make(chan struct{}) }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	s := get(dev.ID.String())
	// 前置非空跑：账号必须真连着，否则测的就不是同步阶段而是登录阶段。
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	old := waContactSyncTimeout
	waContactSyncTimeout = 60 * time.Millisecond
	t.Cleanup(func() { waContactSyncTimeout = old })

	n, err := WhatsappSyncContacts(context.Background(), dev.ID.String(), false)
	if n != 0 || err == nil {
		t.Fatalf("本包同步预算到点必须报错且不给条数: n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "同步预算") {
		t.Errorf("错误文案要点名是本包的同步预算到点: %v", err)
	}
	if strings.Contains(err.Error(), "上下文已结束") {
		t.Errorf("内部同步超时不得报成调用方断连: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("必须保留 %%w 让下游仍可判定超时: %v", err)
	}
	if c, _, _ := s.countSyncs(); c != 1 {
		t.Errorf("同步应恰好发起一次，实得 %d", c)
	}
}

func TestSyncCallerCancelStillBlamedOnCaller(t *testing.T) {
	dev := fakeDevice(t, "447378152319:22@s.whatsapp.net", "android", "P")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncGate = make(chan struct{}) }, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	s := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	// 同步门闩永不打开：调用方先结束，本包预算（缺省 20s）远没到点。
	n, err := WhatsappSyncContacts(ctx, dev.ID.String(), false)
	if n != 0 || err == nil {
		t.Fatalf("调用方取消必须报错: n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "上下文已结束") {
		t.Errorf("调用方断连仍要如实报成调用方断连: %v", err)
	}
	if strings.Contains(err.Error(), "同步预算") {
		t.Errorf("这条不是本包超时，不许出现同步预算文案: %v", err)
	}
}

// TestStExplicitFullConcurrentPaysOneFullSync 是为 MD 变异补的闸（Lead 合流批）。
//
// 为什么必须单独有这条：T10/T11 的「并发只拉一次」用例其实测不到 `s.mu`——那条走的是
// 快路，压住重复拉的是「拿锁后双检本地」，把锁换成 `TryLock()` 它照样绿（MD 首打 SURVIVED
// 就是这么暴露的）。真正只有锁能保护的是 **full=true 的账**：`lastFullAt` 由第一个真全量
// 写下，后进者必须在锁内看到它并退化成快路；没有锁，8 个并发各自都看到零值，
// 于是 8 次 `DeleteAppStateVersion` 互踩版本、8 份全量快照重复付。
func TestStExplicitFullConcurrentPaysOneFullSync(t *testing.T) {
	dev := fakeDevice(t, "447378152319:23@s.whatsapp.net", "android", "P")
	jid := types.JID{User: "8613900000023", Server: types.DefaultUserServer}
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: {FirstName: "st"}}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	waitUntil(t, 2*time.Second, "首次连上", get(dev.ID.String()).LoggedIn)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil {
				t.Errorf("并发显式全量同步不该报错: %v", err)
			}
		}()
	}
	wg.Wait()

	_, full, _ := get(dev.ID.String()).countSyncs()
	if full != 1 {
		t.Errorf("8 个并发显式 full=true 必须被单飞压成 1 次全量（其余退快路），实得 %d 次", full)
	}
}
