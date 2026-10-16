package whatsapp

// 本文件是 T11 的负例与竞态补强闸：把 T10 的 15 条没钉到的残余形态钉成活闸。
// 被测面只有三处（全部只读，本文件不改生产代码）：
//   - wa_contacts.go 的 syncContactAppState 退化分支（:117-123）：窗口内的「显式 full=true」
//     要降档但**照常真发一次**；窗口内的「快路后升级（escalate=true）」要**一次都不发**、直接回读本地。
//   - wa_manager.go 的 ensureLoggedIn（:326-345）/ StartClient 的 MaxConcurrent 闸（:166-170）/
//     liveWrapper 的僵尸复活（:370-403）；
//   - wa_client.go 的 WhatsappListContacts 两段式（:703-740）。
//
// 口径与纪律：
//   - 全部走既有 seam 假件（fakeSession/fakeStore/startFakeWA），不联网、不连真 Postgres；
//   - 手搓 t.Fatalf/t.Errorf，本包不用 testify；
//   - helper 一律 ch 前缀（同包命名空间：pc/pch/pcs/al 已被占用，重名在 Go 里是编译错误）；
//     复用既有 helper（alCountSyncs/alWantSeq/alContact/waitUntil/startFakeWA/fakeDevice）不新增同义反复；
//   - 零 t.Skip：跑不出去的直接红，不许把形态藏起来；
//   - 竞态时序一律避开「setDown() 之后立刻等登录」这一档（wa_manager.go:540-560 的 waitLoggedIn
//     不听 kick，是 Lead 已登记的已知抖动面），改用既有确定性形态：先等到连上，或用 LoggedOut
//     让常驻协程自己收手，或让首轮 Login 直接失败停在 sleepBackoff（它认 kick）。
//
// 已知缺陷**不在本文件写断言**（Lead 在合流批连修带测试一起做）：wa_contacts.go:133-139 的
// 内部同步超时 waContactSyncTimeout 会被 waLoginErr（:56-61）折成「调用方上下文已结束」，
// 把本包自己的 20 秒超时误判成调用方断连。本文件因此从不注入慢同步、也不缩短
// waContactSyncTimeout，绝不碰那条折算路径。

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"touchgocore/config"
)

// chContactsErr 是注入给 fakeSession.contactsErr 的哨兵错。
// 用包级哨兵而不是每次现造：断言要能用 errors.Is 钉住门面的 %w 链没被折成 %v。
var chContactsErr = errors.New("假件：好友表读取炸了")

// chSetSyncWindow 注入「同一账号两次全量重拉的最小间隔」，t.Cleanup 还原。
// 与 alSetSyncWindow 同形，但本文件的用例要在**一条用例中途**翻窗口（0 → 生效），
// 中途那次直接赋值、由这里登记的 cleanup 统一还原，避免嵌套 cleanup 把窗口留在 0。
func chSetSyncWindow(t *testing.T, d time.Duration) {
	t.Helper()
	old := waContactSyncFullMinInterval
	waContactSyncFullMinInterval = d
	t.Cleanup(func() { waContactSyncFullMinInterval = old })
}

// chStateSyncEntry 只读地取某账号在 m.stateSync 里的单飞条目，不在册返回 nil。
// 绝不能用 m.stateSyncLock(key)：那个函数会惰性建条目，用它当判据会把「没建」测成「建了」。
func chStateSyncEntry(m *waManager, key string) *waContactSync {
	v, ok := m.stateSync.Load(key)
	if !ok {
		return nil
	}
	return v.(*waContactSync)
}

// chWrapper 只读地取 clients 表里某个账号的 wrapper，不在册返回 nil。
func chWrapper(m *waManager, key string) *waClientWrapper {
	v, ok := m.clients.Load(key)
	if !ok {
		return nil
	}
	return v.(*waClientWrapper)
}

// chStateSyncCount 数管理器单飞账里的条目总数（不限量增长的判据）。
func chStateSyncCount(m *waManager) int {
	n := 0
	m.stateSync.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// chStacks 抓全进程 goroutine 栈（缓冲自增长直到一次装得下）。
// 协程泄漏计数一律用 runtime.Stack，不引 goleak 之类要改 go.mod 的依赖。
func chStacks(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8192)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		if len(buf) >= 1<<24 {
			t.Fatalf("goroutine 栈抓不完（缓冲已 %d 字节）", len(buf))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// chGoroutinesIn 按「协程块」数出仍停在某个源文件里的协程条数
// （一条协程在同一文件可能有多个栈帧，故按块计数而不是按出现次数）。
func chGoroutinesIn(t *testing.T, file string) int {
	t.Helper()
	n := 0
	for _, block := range strings.Split(chStacks(t), "\n\ngoroutine ") {
		if strings.Contains(block, file) {
			n++
		}
	}
	return n
}

// chWantNoSyncCall 断言错误串里点名的是「取好友列表」这一步，
// 而不是被掩盖成登录/上下文问题——这几条串互斥，值班才分得开诊。
func chWantReadContactsError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("取好友列表失败必须报错，实得 nil（静默退化就是把失败伪装成「这账号没好友」）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "获取好友列表失败") {
		t.Fatalf("错误串要点名「获取好友列表失败」，实得 %q", msg)
	}
	if !errors.Is(err, chContactsErr) {
		t.Fatalf("必须保留 %%w 让下游可判定，实得 %q", msg)
	}
	for _, bad := range []string{"没有在跑", "未配对", "未能连上", "上下文已结束", "同步联系人"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("本地读取失败被掩盖成 %q 那一类问题了: %q", bad, msg)
		}
	}
}

// TestChExplicitFullInWindowStillSendsOnce 钉 wa_contacts.go:117-123 的两半：
//
//	① 窗口内的**显式** full=true（escalate=false）：把 full 降成 false，但**照常真发一次**
//	   （:122 只改档位不 return；:124 的单飞双检又明确只对 !wantFull && !escalate 生效）。
//	   这条与 T10 的 TestSyncContactsFacadeCountsAndThrottles 的区别在判据：那条只看
//	   full 计数没涨（「一次都不发」也能绿灯），这里钉的是 calls 必须逐次 +1 且末位为 false，
//	   正治「窗口内把显式请求整段跳过」这种看起来更省的做法。
//	② 窗口内的**升级**（escalate=true）：反过来**一次都不发**，直接回读本地
//	   （:118-121——同一次调用里刚试过快路，重试没有信息增量，更不该白擦版本）。
//
// 改坏哪行会红：
//   - 把 :122 的 `full = false` 换成 `return readWaContacts(ctx, w)`（窗口内跳过显式请求）⇒
//     子用例①的 calls 停在 1，红。
//   - 删掉 :118-120 的 escalate 早退分支 ⇒ 子用例②的第三次同步照样发出去，calls 涨到 3、
//     序列多出末位 false，红。
//   - 把 :124 的双检条件写成 `!escalate`（漏掉 !wantFull）⇒ 子用例①的末位真值断言红
//     （显式请求被本地缓存短路，一次都不发）。
func TestChExplicitFullInWindowStillSendsOnce(t *testing.T) {
	t.Run("窗口内显式full=true仍真发一次", func(t *testing.T) {
		chSetSyncWindow(t, time.Hour)
		dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
		a, ia := alContact(t, "442071234567@s.whatsapp.net", "Alice")
		b, ib := alContact(t, "442071234568@s.whatsapp.net", "Beno")
		_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
			s.syncAdds = map[types.JID]types.ContactInfo{a: ia, b: ib}
		}, dev)
		if err := WhatsappStartClient(dev.ID.String()); err != nil {
			t.Fatal(err)
		}
		s := get(dev.ID.String())
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		// 第一次：零值 lastFullAt 对窗口不设限 ⇒ 显式 full=true 真发，并把窗口账记上。
		if n, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil || n != 2 {
			t.Fatalf("首次显式全量同步: n=%d err=%v，期望 2 条", n, err)
		}
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != 1 || fullCalls != 1 {
			t.Fatalf("首次显式同步的账 calls=%d full=%d 序列=[%s]，期望 1/1", calls, fullCalls, seq)
		}

		// 第二、第三次：都落在窗口内 —— 档位该降成 false，但请求**一次都不能少发**。
		for _, want := range []int{2, 3} {
			if n, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil || n != 2 {
				t.Fatalf("窗口内第 %d 次显式同步: n=%d err=%v，期望仍返回 2 条", want, n, err)
			}
			calls, fullCalls, raw := s.countSyncs()
			if calls != want {
				t.Fatalf("窗口内第 %d 次显式同步没真发：calls=%d（序列=%v），期望 %d", want, calls, raw, want)
			}
			if fullCalls != 1 {
				t.Fatalf("窗口内又擦了一次版本：full=%d，期望恒为 1", fullCalls)
			}
			if raw[len(raw)-1] {
				t.Fatalf("窗口内的显式请求没降档成快路：末次 full=true（序列=%v）", raw)
			}
		}
		alWantSeq(t, s, "true", "false", "false")
	})

	t.Run("窗口内升级一次都不发", func(t *testing.T) {
		chSetSyncWindow(t, time.Hour)
		dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
		// 同步成功但一条联系人也补不进来：仿真服务器快照本就为空，逼出第二段的升级分支。
		_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
		if err := WhatsappStartClient(dev.ID.String()); err != nil {
			t.Fatal(err)
		}
		s := get(dev.ID.String())
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		// 先用显式全量把 lastFullAt 写进窗口（这次该真发）。
		if n, err := WhatsappSyncContacts(nil, dev.ID.String(), true); err != nil || n != 0 {
			t.Fatalf("空快照的显式同步不该报错: n=%d err=%v", n, err)
		}
		alWantSeq(t, s, "true")

		// 列好友：快路（false）照常真发一次，升级（true,escalate）落在窗口内 ⇒ 一次都不发。
		list, err := WhatsappListContacts(nil, dev.ID.String())
		if err != nil || len(list) != 0 {
			t.Fatalf("空快照应交回空列表且无错: list=%+v err=%v", list, err)
		}
		alWantSeq(t, s, "true", "false")
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != 2 || fullCalls != 1 {
			t.Fatalf("升级分支被窗口挡住时该一次都不发：calls=%d full=%d 序列=[%s]，期望 2/1", calls, fullCalls, seq)
		}
	})
}

// TestContactChConcurrentListPullsOnce 钉两段式门面这一侧的单飞短路（wa_contacts.go:124-131 +
// wa_client.go:712-738）：同账号并发 N 条列好友，只有第一个把快照拉下来；
// 后进者要么在①段就看见本地 >0（一次同步都不做），要么拿锁后双检命中本地 >0 而**不再产生第二次同步**。
//
// 与 T10 的 TestContactSyncSingleFlight 的差别：那条打的是 WhatsappSyncContacts（①段不存在），
// 这条打的是真实热路径 WhatsappListContacts——它同时经过两段，
// 只有这条能证明「群发前的每次列好友不会把同账号打爆」。
//
// 改坏哪行会红：
//   - 删掉 wa_contacts.go:124-131 的单飞双检 ⇒ 后进者各自再拉一次，calls 涨到 >1，红；
//   - 删掉 wa_contacts.go:113-114 的 s.mu.Lock（单飞锁本体）⇒ 同上，且 -race 还会报
//     fakeSession.contacts 的并发读写；
//   - 把 wa_client.go:718 的 `len(contacts) > 0` 判据去掉（①段永远放行到②段）⇒
//     这条的 calls 仍为 1（靠双检兜住），但 TestListContactsSkipsSyncWhenLocalHasData 会红，
//     所以本用例的红信息专门描述「同步次数」，两条合起来才把两段都钉死。
func TestContactChConcurrentListPullsOnce(t *testing.T) {
	const concurrency = 8
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		// 第一个调用拉完就塞入 1 条联系人：后进者的双检/①段本地读因此有真数据可命中。
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(dev.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	if s == nil {
		t.Fatal("假会话未登记（waSessionFactory 没被走到）")
	}
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	counts := make([]int, concurrency)
	lists := make([][]WaContact, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让 N 条同时挤在单飞锁上
			list, err := WhatsappListContacts(nil, dev.ID.String())
			lists[i], errs[i], counts[i] = list, err, len(list)
		}(i)
	}
	close(start)
	wg.Wait()

	calls, fullCalls, seq := alCountSyncs(t, s)
	if calls != 1 {
		t.Fatalf("%d 并发列好友把同账号同步了 %d 次（full=%d，序列=[%s]），期望单飞 1 次",
			concurrency, calls, fullCalls, seq)
	}
	if fullCalls != 0 {
		t.Fatalf("并发热路径里出现了全量重拉（full=%d），期望 0", fullCalls)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("并发结果[%d] 报错: %v", i, errs[i])
		}
		if counts[i] != 1 || len(lists[i]) != 1 || lists[i][0].JID != jid.String() {
			t.Fatalf("并发结果[%d] 不符: n=%d %+v", i, counts[i], lists[i])
		}
	}
}

// TestChEnsureLoggedInMaxConcurrentGate 自动登录撞上 MaxConcurrent 闸：
// 错误串必须**同时**含「没有在跑」（wa_manager.go:334 的前缀，六条既有门面断言钉着这个关键词）
// 与上限文案（:169 的「在跑会话数已达上限 …/先用 WhatsappStopClient 停一个」），
// 让值班知道「不是没配对、也不是网络问题，是额度满了、要自己腾一个」；
// 且失败路径**不得留下半个 clients 条目**——否则下一条请求会把这个半截对象当成在跑会话端出去。
//
// 改坏哪行会红：
//   - 删掉 wa_manager.go:334 的包装前缀（只把 StartClient 的错原样递出去）⇒
//     「没有在跑」关键词消失，本用例与 wa_client_test.go 那六条一起红；
//   - 把 :166-170 的上限判断挪到 LoadOrStore 之后（先建对象再拒）⇒
//     下面的 clients/list/session/单飞账四条断言全红；
//   - 把上限文案改软（去掉「先用 WhatsappStopClient 停一个」）⇒ 处置关键词红。
func TestChEnsureLoggedInMaxConcurrentGate(t *testing.T) {
	running := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "A")
	blocked := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "B")
	cfg := &config.WhatsappSessionsConfig{MaxConcurrent: 1, AntibanEnabled: waBool(false)}
	m, _, get := startFakeWA(t, cfg, nil, running, blocked)
	if err := WhatsappStartClient(running.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(running.ID.String())
	waitUntil(t, 2*time.Second, "在跑账号连上", s.LoggedIn)

	// 不在册的第二个账号：设备行真的在库里，所以失败原因只可能是额度闸。
	list, err := WhatsappListContacts(nil, blocked.ID.String())
	if err == nil {
		t.Fatalf("额度已满时列好友不该成功，实得 %+v", list)
	}
	if len(list) != 0 {
		t.Fatalf("被额度闸挡下却交回了 %d 条好友数据: %+v", len(list), list)
	}
	msg := err.Error()
	for _, want := range []string{"没有在跑", "自动启动失败", "在跑会话数已达上限 1", "max_concurrent", "先用 WhatsappStopClient 停一个"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误串缺少 %q，实得 %q", want, msg)
		}
	}
	// 额度闸与「从未配对」是两件事，文案不许混读（处置完全不同）。
	if strings.Contains(msg, "未配对") {
		t.Errorf("库里明明有设备行，不许报成未配对: %q", msg)
	}
	if strings.Contains(msg, "上下文已结束") {
		t.Errorf("额度不足不是调用方断连，不许折成上下文已结束: %q", msg)
	}

	// 失败路径不留半截状态：clients 表仍只有那一个在跑账号，被挡的账号压根没建会话。
	if n := m.countClients(); n != 1 {
		t.Fatalf("额度闸挡下后在跑会话数=%d，期望仍为 1（没留下半个条目）", n)
	}
	if _, ok := m.clients.Load(blocked.ID.String()); ok {
		t.Fatal("被额度闸拒绝的账号竟在册（半截 clients 条目）")
	}
	if got := get(blocked.ID.String()); got != nil {
		t.Fatal("被额度闸拒绝的账号竟建出了假会话（waSessionFactory 被白走了一次）")
	}
	infos := WhatsappListClients()
	if len(infos) != 1 || infos[0].JID != running.ID.String() {
		t.Fatalf("在跑会话列表不符: %+v", infos)
	}
	// 同步一次都没发生：单飞账里既没有被挡账号的条目，在跑账号也没被拉过快照。
	if chStateSyncEntry(m, blocked.ID.String()) != nil {
		t.Fatal("被挡账号竟建出了同步单飞条目（说明请求掉进了同步阶段）")
	}
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 0 || fullCalls != 0 || seq != "" {
		t.Fatalf("额度闸挡下后仍发生了同步：calls=%d full=%d 序列=[%s]，期望 0/0/[]", calls, fullCalls, seq)
	}
	if n := chStateSyncCount(m); n != 0 {
		t.Fatalf("单飞账条目数=%d，期望 0（一次同步都没试，不该建锁）", n)
	}

	// 同一个额度闸对显式同步门面同样成立，且串口径一致。
	if n, serr := WhatsappSyncContacts(nil, blocked.ID.String(), true); n != 0 || serr == nil ||
		!strings.Contains(serr.Error(), "没有在跑") || !strings.Contains(serr.Error(), "在跑会话数已达上限") {
		t.Fatalf("显式同步门面撞额度闸时的表现不符: n=%d err=%v", n, serr)
	}
	if n := m.countClients(); n != 1 {
		t.Fatalf("两次被挡后在跑会话数=%d，期望 1", n)
	}
}

// TestChZombieRevivesThenAwaitsLogin 僵尸（在册但常驻协程已收手，done 已关）叠加自动登录：
// 门面必须先把这具死对象摘掉重建，再在新会话上等到连上，最后才同步取数。
//
// 造僵尸走既有确定性形态（fire LoggedOut + waitWrapperExited），**不碰 setDown()**：
// 后者会让常驻协程停在听不见 kick 的 waitLoggedIn（wa_manager.go:540-560），
// 正是 Lead 登记的三条已知抖动闸的同形根因，本文件绝不去挤那条时序。
//
// 改坏哪行会红：
//   - 去掉 liveWrapper 的 `if !w.retired()`（:376-378，死对象原样端出）⇒
//     reconnectAndAwait 在 waitConnected 里撞 `<-w.done` 报「常驻协程已退出」，
//     本用例第一步 err 即红；
//   - 去掉 ensureLoggedIn 的 reconnectAndAwait（:341-343，复活完就直接取数）⇒
//     新会话的协程还没跑完 Login，同步账落在没连上的会话上，
//     fresh.LoggedIn()/old.syncCalls==0 与「取到 1 条」三条里至少一条红；
//   - 把 liveWrapper 的重建改成「摘掉但不重起」（去掉 :395 的 StartClient）⇒
//     liveWrapper 报「重登后会话不在册」，红。
func TestChZombieRevivesThenAwaitsLogin(t *testing.T) {
	dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
	jid, info := alContact(t, "8613900000000@s.whatsapp.net", "Bob")
	key := dev.ID.String()
	m, _, get := startFakeWA(t, nil, func(s *fakeSession) {
		s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
	}, dev)
	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	old := get(key)
	waitUntil(t, 2*time.Second, "首次连上", old.LoggedIn)
	beforeLogins := old.countLogins()

	// 库派发吊销：dispatch 当场落 loggedOut 标记，常驻协程随即收手，但 key 仍在册。
	old.fire(events.LoggedOut{OnConnect: false})
	waitWrapperExited(t, m, key)
	if zombie := chWrapper(m, key); zombie == nil || !zombie.retired() {
		t.Fatal("夹具没能造出僵尸形态（在册、done 已关、retired() 为真）")
	}

	start := time.Now()
	list, err := WhatsappListContacts(nil, key)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("僵尸账号列好友应被复活后成功: %v", err)
	}
	if len(list) != 1 || list[0].JID != jid.String() {
		t.Fatalf("复活后取到的好友列表不符: %+v", list)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("复活+等连上耗时 %s（没被自愈接住，会烧满连接预算）", elapsed)
	}

	fresh := get(key)
	if fresh == nil || fresh == old {
		t.Fatal("死对象没被换成新会话（复活没发生）")
	}
	if got := fresh.countLogins(); got < 1 {
		t.Fatalf("复活出的新会话 Login 调用数=%d，期望 ≥1（重建后没重连）", got)
	}
	if !fresh.LoggedIn() {
		t.Fatal("门面返回时新会话仍未连上（没等到登录就取了数）")
	}
	// 死对象保持原样：既没被重连回去，也没被拿去同步。
	if got := old.countLogins(); got != beforeLogins {
		t.Fatalf("死会话的 Login 调用数 %d → %d，被复用的竟是死对象", beforeLogins, got)
	}
	if old.LoggedIn() {
		t.Fatal("已退出的旧会话竟又回到登录态")
	}
	if calls, fullCalls, seq := alCountSyncs(t, old); calls != 0 || fullCalls != 0 || seq != "" {
		t.Fatalf("同步跑在了死会话上（calls=%d full=%d 序列=[%s]），期望 0/0/[]", calls, fullCalls, seq)
	}
	if calls, fullCalls, seq := alCountSyncs(t, fresh); calls != 1 || fullCalls != 0 || seq != "false" {
		t.Fatalf("新会话的同步账 calls=%d full=%d 序列=[%s]，期望 1/0/[false]", calls, fullCalls, seq)
	}
	// clients 表仍只有该 key 的一条，且指向复活后的那份。
	if n := m.countClients(); n != 1 {
		t.Fatalf("复活后在跑会话数=%d，期望 1（死对象没摘干净或多起了一条）", n)
	}
	v, ok := m.clients.Load(key)
	if !ok || v.(*waClientWrapper).sess != fresh {
		t.Fatalf("clients 表里该 key 的不是复活后的会话（ok=%v）", ok)
	}
	if infos := WhatsappListClients(); len(infos) != 1 || infos[0].JID != key {
		t.Fatalf("在跑会话列表不符: %+v", infos)
	}
}

// TestChUnpairedIsNotSwallowed 库里没有设备行（fakeStore.GetDevice 查无返 nil,nil，
// 与 sqlstore 同口径）时，自动登录不许把 liveWrapper 的「未配对」吞成一句「没有在跑」：
// 两条串必须在同一条错误里——前者是唯一处置（去扫码/配对），后者是六条既有门面断言的关键词。
// 同时这条请求必须**一次同步都不发**：账号压根不存在，跑同步就是把失败伪装成「这账号没好友」。
//
// 与 T10 的 TestEnsureLoggedInUnpairedKeepsNotRunningPhrase 的差别：那条只钉文案，
// 这里补的是「不发生任何同步调用」这一侧（拿一个真的在跑的账号当对照物，
// 用它自己的 syncCalls==0 与单飞账条目数 0 正面证明）。
//
// 改坏哪行会红：
//   - wa_manager.go:333-335 改成「StartClient 失败就自己起一个空会话继续走」⇒
//     在跑账号的同步账变成 >0，红；
//   - wa_client.go:724-727 把 ensureLoggedIn 的错误吞掉（继续往下同步）⇒ 同上；
//   - 去掉 :334 前缀 ⇒ 「没有在跑」消失，红（与那六条一起）。
func TestChUnpairedIsNotSwallowed(t *testing.T) {
	running := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, running)
	if err := WhatsappStartClient(running.ID.String()); err != nil {
		t.Fatal(err)
	}
	s := get(running.ID.String())
	waitUntil(t, 2*time.Second, "对照账号连上", s.LoggedIn)

	// 447378152319 就是下游现场那个「配对码绑的号」，这里库里连设备行都没有。
	const unpaired = "447378152319"
	list, err := WhatsappListContacts(nil, unpaired)
	if err == nil {
		t.Fatalf("未配对账号列好友不该成功，实得 %+v", list)
	}
	if len(list) != 0 {
		t.Fatalf("未配对账号却交回 %d 条好友: %+v", len(list), list)
	}
	msg := err.Error()
	if !strings.Contains(msg, "未配对") {
		t.Errorf("错误串必须点名「未配对」（唯一处置是去扫码）: %q", msg)
	}
	if !strings.Contains(msg, "没有在跑") {
		t.Errorf("错误串必须留住「没有在跑」子串: %q", msg)
	}
	if !strings.Contains(msg, "扫码") {
		t.Errorf("错误串要给出处置（扫码/配对），实得 %q", msg)
	}
	if strings.Contains(msg, "上下文已结束") || strings.Contains(msg, "密钥") {
		t.Errorf("从未配对不许被读成断连或密钥缺失: %q", msg)
	}

	// 一次同步都没发生：进程里唯一存在的会话没有被拉过快照，单飞账也没建条目。
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 0 || fullCalls != 0 || seq != "" {
		t.Fatalf("未配对请求触发了同步：calls=%d full=%d 序列=[%s]，期望 0/0/[]", calls, fullCalls, seq)
	}
	if n := chStateSyncCount(m); n != 0 {
		t.Fatalf("单飞账条目数=%d，期望 0（没试同步就不该建锁）", n)
	}
	if n := m.countClients(); n != 1 {
		t.Fatalf("在跑会话数=%d，期望 1（未配对请求不该动 clients 表）", n)
	}
	if infos := WhatsappListClients(); len(infos) != 1 || infos[0].JID != running.ID.String() {
		t.Fatalf("在跑会话列表被改动: %+v", infos)
	}

	// 显式同步门面同一口径。
	if n, serr := WhatsappSyncContacts(nil, unpaired, false); n != 0 || serr == nil ||
		!strings.Contains(serr.Error(), "未配对") || !strings.Contains(serr.Error(), "没有在跑") {
		t.Fatalf("显式同步门面对未配对账号的表现不符: n=%d err=%v", n, serr)
	}
	if calls, _, seq := alCountSyncs(t, s); calls != 0 || seq != "" {
		t.Fatalf("显式同步门面也未配对时跑了同步（calls=%d），期望 0", calls)
	}
}

// TestChEmptySnapshotIsNotFailure 「同步成功但服务器快照本就 0 条」是本包唯一合法的
// 「0 条且无错」（wa_contacts.go:148-152 只记日志、不改返回值）：
// 列好友的第二段会因此升级一次 full=true（wa_client.go:732-738），最终仍交回空列表 + nil error，
// 而不是把「没好友」冒充成失败，也不许把升级烧成无穷次。
//
// 本用例刻意**不动窗口注入点**（用生产缺省 10 分钟）：零值 lastFullAt 对窗口不设限，
// 所以首次升级本就该执行——这条同时钉住「首次同步不需要显式 full=true、但快路拉空后必须升级一次」。
//
// 改坏哪行会红：
//   - 把 wa_client.go:732 的 `len(contacts) == 0` 判据删掉（永不升级）⇒
//     序列只剩 [false]、fullCalls=0，alWantSeq 红；
//   - 把 wa_contacts.go:148-151 的空快照分支改成 return error（把合法的 0 当失败）⇒
//     第一条 err 断言红，下游又会回到「看起来成功的 0」的反面：一个都发不出去；
//   - 去掉 :141-143 的 `s.lastFullAt = time.Now()` 记账 ⇒ 本用例仍绿，但下一条
//     （TestContactChFullEscalationTwiceThenWindowBack）的窗口挡不住，full 涨到 3 而红。
func TestChEmptySnapshotIsNotFailure(t *testing.T) {
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	key := dev.ID.String()
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev) // 同步成功但一条也补不进来
	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	s := get(key)
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	list, err := WhatsappListContacts(nil, key)
	if err != nil {
		t.Fatalf("快照本就为空不是失败，不该报错: %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("空快照应交回非 nil 的空列表，实得 %+v", list)
	}
	alWantSeq(t, s, "false", "true")
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 2 || fullCalls != 1 {
		t.Fatalf("空快照的同步账 calls=%d full=%d 序列=[%s]，期望 2/1（快路一次 + 升级一次）", calls, fullCalls, seq)
	}

	// 显式同步门面对同一形态也是「0 条 + nil」：下游要能把它和「同步失败」分开读。
	if n, serr := WhatsappSyncContacts(nil, key, false); n != 0 || serr != nil {
		t.Fatalf("空快照的显式同步应返回 0 条且无错: n=%d err=%v", n, serr)
	}
	alWantSeq(t, s, "false", "true", "false")
	if calls, fullCalls, _ := alCountSyncs(t, s); calls != 3 || fullCalls != 1 {
		t.Fatalf("第三次同步的账 calls=%d full=%d，期望 3/1（快路零网络，不该再擦版本）", calls, fullCalls)
	}
}

// TestChReadContactsFailureIsNotSilent 钉 wa_contacts.go:82-88 与 :144-147：
// 取好友列表（GetContacts）失败时必须**报错**，一条一条形态钉死，不许静默成「0 条 + nil」，
// 也不许被掩盖成登录/上下文问题（那会把值班引去查网络，而真因是本地好友表读不出）。
//
// 三段的返回逐字钉住：
//   - ①段（ensureAlive 命中本地读，wa_client.go:713-717）：直接「获取好友列表失败」并返回，
//     **不得掉进②段**——用 syncCalls==0 与「单飞账没建条目」正面证明它没往下走；
//   - ②段同步后的回读（wa_contacts.go:144-147）：同步已经成功，回读失败仍要报错，
//     错误串里不许出现「同步联系人」（同步没失败）；
//   - 显式同步门面：同一串，且条数为 0。
//
// 改坏哪行会红：
//   - 把 wa_client.go:715-717 的 `if err != nil { return }` 改成「忽略错误继续走②段」⇒
//     ①子用例的 syncCalls 变成 >1、错误串变成带「同步联系人」或登录问题，红；
//   - 把 wa_contacts.go:145-147 的回读失败改成 `return contacts, nil`（静默交空集）⇒
//     ②子用例拿到 nil error，红（正是本次要治的「看起来成功的 0」）；
//   - 把 :85 的 %w 换成 %v ⇒ errors.Is(err, chContactsErr) 断言红。
func TestChReadContactsFailureIsNotSilent(t *testing.T) {
	t.Run("第一段本地读失败即返回", func(t *testing.T) {
		dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
		key := dev.ID.String()
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.contactsErr = chContactsErr }, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		s := get(key)
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		list, err := WhatsappListContacts(nil, key)
		chWantReadContactsError(t, err)
		if len(list) != 0 {
			t.Fatalf("本地读失败却交回 %d 条: %+v", len(list), list)
		}
		// 没掉进第二段：一次同步都没发，单飞锁条目也没建。
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != 0 || fullCalls != 0 || seq != "" {
			t.Fatalf("第一段失败后竟掉进第二段同步了：calls=%d full=%d 序列=[%s]，期望 0/0/[]", calls, fullCalls, seq)
		}
		if chStateSyncEntry(m, key) != nil {
			t.Fatal("第一段失败不该建出同步单飞条目")
		}
	})

	t.Run("第二段同步后回读失败", func(t *testing.T) {
		dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
		key := dev.ID.String()
		// 压根不在册：①段 ensureAlive 直接失败，请求走进②段（自动登录 + 同步 + 回读）。
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.contactsErr = chContactsErr }, dev)

		list, err := WhatsappListContacts(nil, key)
		chWantReadContactsError(t, err)
		if strings.Contains(err.Error(), "同步联系人") {
			t.Fatalf("同步本身是成功的，不许把回读失败报成同步失败: %q", err.Error())
		}
		if len(list) != 0 {
			t.Fatalf("回读失败却交回 %d 条: %+v", len(list), list)
		}
		s := get(key)
		if s == nil {
			t.Fatal("②段的按需启动没建出会话")
		}
		alWantSeq(t, s, "false")
		if chStateSyncEntry(m, key) == nil {
			t.Fatal("②段没经过同步单飞锁")
		}
	})

	t.Run("显式同步门面同样报错不静默", func(t *testing.T) {
		dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
		key := dev.ID.String()
		_, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.contactsErr = chContactsErr }, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		s := get(key)
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		n, err := WhatsappSyncContacts(nil, key, false)
		chWantReadContactsError(t, err)
		if n != 0 {
			t.Fatalf("回读失败时条数必须是 0，实得 %d", n)
		}
		alWantSeq(t, s, "false")
	})
}

// TestChNoStateOrGoroutineLeak 三态（成功 / 同步失败 / 未配对）各重复跑若干次之后：
//   - 同账号在 m.stateSync 里的条目数恒为 1，且始终是**同一把锁对象**（复用而非重建），
//     失败路径也不许攒条目；压根没同步过的账号（未配对）条目数为 0；
//   - 没有任何协程残留在 wa_contacts.go 里阻塞（runtime.Stack 按协程块计数自证，
//     不引 goleak 之类要改 go.mod 的依赖）。
//
// 改坏哪行会红：
//   - 把 wa_manager.go:72-79 的 stateSyncLock 改成「每次新建一把锁、LoadOrStore 换成 Store」⇒
//     条目数随调用次数增长（且单飞语义失效），成功/失败两档子用例红；
//   - 把 wa_contacts.go:113-114 的 s.mu.Lock 换成没有 Unlock 的早退（或 defer 丢掉）⇒
//     第二次调用永久阻塞在锁上，本用例的协程计数与超时同时红；
//   - 让同步路径在失败时不返回（协程挂在 sctx 上）⇒ 协程计数红。
func TestChNoStateOrGoroutineLeak(t *testing.T) {
	t.Run("成功态复用同一把锁", func(t *testing.T) {
		dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
		key := dev.ID.String()
		jid, info := alContact(t, "442071234567@s.whatsapp.net", "Alice")
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) {
			s.syncAdds = map[types.JID]types.ContactInfo{jid: info}
		}, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		s := get(key)
		waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

		// 条目是惰性建的（wa_manager.go:72-79），第一次同步之前不该存在；
		// 所以「复用同一把锁」的基准必须从第一次调用之后取。
		var first *waContactSync
		for i := 0; i < 5; i++ {
			if n, err := WhatsappSyncContacts(nil, key, false); err != nil || n != 1 {
				t.Fatalf("第 %d 次同步门面: n=%d err=%v", i+1, n, err)
			}
			if got := chStateSyncCount(m); got != 1 {
				t.Fatalf("第 %d 次调用后单飞账条目数=%d，期望恒为 1", i+1, got)
			}
			e := chStateSyncEntry(m, key)
			if e == nil {
				t.Fatalf("第 %d 次调用后没有单飞条目（同步没经过 stateSyncLock）", i+1)
			}
			if first == nil {
				first = e
			} else if e != first {
				t.Fatalf("第 %d 次调用换了一把锁（单飞账被重建过）", i+1)
			}
		}
		if calls, _, _ := alCountSyncs(t, s); calls != 1 {
			t.Fatalf("重复调用把同账号同步了 %d 次，期望 1（本地已有数据后双检命中）", calls)
		}
		if n := chGoroutinesIn(t, "wa_contacts.go"); n != 0 {
			t.Fatalf("残留 %d 条协程停在 wa_contacts.go：\n%s", n, chStacks(t))
		}
	})

	t.Run("同步失败态不攒条目", func(t *testing.T) {
		dev := fakeDevice(t, "8618608008318:9@s.whatsapp.net", "android", "C")
		key := dev.ID.String()
		m, _, get := startFakeWA(t, nil, func(s *fakeSession) { s.syncErr = errBoom }, dev)
		if err := WhatsappStartClient(key); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 2*time.Second, "首次连上", get(key).LoggedIn)

		for i := 0; i < 4; i++ {
			if n, err := WhatsappSyncContacts(nil, key, false); n != 0 || err == nil {
				t.Fatalf("第 %d 次同步失败应报 0 条 + 错误: n=%d err=%v", i+1, n, err)
			}
			if _, err := WhatsappListContacts(nil, key); err == nil {
				t.Fatalf("第 %d 次列好友应把同步失败报出去", i+1)
			}
			if got := chStateSyncCount(m); got != 1 {
				t.Fatalf("第 %d 轮失败后单飞账条目数=%d，期望恒为 1", i+1, got)
			}
		}
		if n := chGoroutinesIn(t, "wa_contacts.go"); n != 0 {
			t.Fatalf("失败路径残留 %d 条协程停在 wa_contacts.go：\n%s", n, chStacks(t))
		}
	})

	t.Run("未配对态零条目零协程", func(t *testing.T) {
		m, _, _ := startFakeWA(t, nil, nil) // 空设备库
		const unpaired = "447378152319"
		for i := 0; i < 4; i++ {
			if _, err := WhatsappListContacts(nil, unpaired); err == nil ||
				!strings.Contains(err.Error(), "未配对") {
				t.Fatalf("第 %d 次未配对请求的错误不符: %v", i+1, err)
			}
		}
		if got := chStateSyncCount(m); got != 0 {
			t.Fatalf("未配对请求建出了 %d 条单飞账条目，期望 0", got)
		}
		if n := m.countClients(); n != 0 {
			t.Fatalf("未配对请求建出了 %d 条会话，期望 0", n)
		}
		if n := chGoroutinesIn(t, "wa_contacts.go"); n != 0 {
			t.Fatalf("未配对路径残留 %d 条协程停在 wa_contacts.go：\n%s", n, chStacks(t))
		}
	})
}

// TestContactChFullEscalationTwiceThenWindowBack 钉「full 升级每轮只发生一次、且第二次落在
// 窗口内就不再发生」：窗口放开时连跑两次列好友 ⇒ fullCalls 恰为 2、序列 [false,true,false,true]；
// 把窗口调回生效 ⇒ 第三次只有快路那一次真发，升级被挡住后**不再补发**（序列末位多一个 false、
// full 仍为 2）。这条与 T10 的两条窗口用例的分工：那条钉「第一次会升级 / 第二次被挡」，
// 这条钉「连续两轮都各升级一次」+「中途把窗口调回生效立刻开始挡」这个翻转。
//
// 改坏哪行会红：
//   - wa_contacts.go:141-143 不记 lastFullAt ⇒ 窗口永远挡不住，第三轮的 full 涨到 3，红；
//   - wa_contacts.go:117 的窗口判据写成 `<=`/取反 ⇒ 第二轮就被挡住（full=1），红；
//   - wa_client.go:735 的升级实参从 true 改成 false ⇒ 序列里出现不了 true，红；
//   - wa_contacts.go:118-121 的 escalate 早退被删 ⇒ 第三轮多真发一次（calls 涨到 6），红。
func TestContactChFullEscalationTwiceThenWindowBack(t *testing.T) {
	chSetSyncWindow(t, 0) // 窗口放开：升级一律允许，先证明「每轮各一次」不是假绿
	dev := fakeDevice(t, "447378152319:1@s.whatsapp.net", "android", "P")
	key := dev.ID.String()
	_, _, get := startFakeWA(t, nil, func(s *fakeSession) {}, dev)
	if err := WhatsappStartClient(key); err != nil {
		t.Fatal(err)
	}
	s := get(key)
	waitUntil(t, 2*time.Second, "首次连上", s.LoggedIn)

	for round, want := range []struct {
		seq   []string
		calls int
	}{
		{[]string{"false", "true"}, 2},
		{[]string{"false", "true", "false", "true"}, 4},
	} {
		if list, err := WhatsappListContacts(nil, key); err != nil || len(list) != 0 {
			t.Fatalf("第 %d 轮列好友: list=%+v err=%v", round+1, list, err)
		}
		alWantSeq(t, s, want.seq...)
		if calls, fullCalls, seq := alCountSyncs(t, s); calls != want.calls || fullCalls != round+1 {
			t.Fatalf("第 %d 轮的同步账 calls=%d full=%d 序列=[%s]，期望 %d/%d",
				round+1, calls, fullCalls, seq, want.calls, round+1)
		}
	}

	// 窗口调回生效（t.Cleanup 登记的还原仍会把生产缺省值放回去）。
	waContactSyncFullMinInterval = time.Hour
	if list, err := WhatsappListContacts(nil, key); err != nil || len(list) != 0 {
		t.Fatalf("窗口生效后列好友: list=%+v err=%v", list, err)
	}
	alWantSeq(t, s, "false", "true", "false", "true", "false")
	if calls, fullCalls, seq := alCountSyncs(t, s); calls != 5 || fullCalls != 2 {
		t.Fatalf("窗口生效后升级该被挡住：calls=%d full=%d 序列=[%s]，期望 5/2", calls, fullCalls, seq)
	}
	if n := chGoroutinesIn(t, "wa_contacts.go"); n != 0 {
		t.Fatalf("三轮之后残留 %d 条协程停在 wa_contacts.go：\n%s", n, chStacks(t))
	}
}
