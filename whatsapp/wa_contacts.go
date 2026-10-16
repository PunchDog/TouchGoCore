package whatsapp

// 本文件是「联系人 app state 同步」的落点：门面 WhatsappSyncContacts、同账号单飞与
// 节流、以及登录等待失败的错误折算 waLoginErr。
//
// 为什么必须有这一支：whatsmeow 的联系人只来自 critical_unblock_low 这一份 app state
// 快照（WM/appstate/keys.go:139-143），批量落库发生在 FetchAppState 走全量分支时的
// Store.Contacts.PutAllContactNames（WM/appstate.go:182-191），**库不会在连接建立时
// 自动拉**。所以新配对的账号（扫码或配对码都一样）通讯录始终是空的：下游群发把目标集
// 算成 0 条，表现为「同一个接口，扫码的号能群发、配对码的号发不出去」。
//
// 与常驻协程的关系（口径不变）：本包的连接/重连仍由 wa_manager.go 里每账号一条常驻协程
// 守着；同步不是常驻任务，而是**按需一次性动作**——只有门面真需要联系人数据时才拉，
// 同账号并发由 stateSync 单飞收住（后进者不重复拉、直接读本地结果），
// 会擦版本的全量重拉另按 waContactSyncFullMinInterval 限流。
//
// 失败语义（这条是本文件的核心纪律）：同步失败一律报错，**绝不退化成「返回 0 条 + nil」**。
// 「看起来成功的 0」正是这次要治的病——下游把 0 目标当已受理，账面全绿而用户没收到东西。
// 唯一合法的「0 条且无错」是同步成功而服务器快照本就为空，那种情况额外记一条日志供分诊。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"

	"touchgocore/vars"
)

// 同步节奏的两个注入点（本轮不开配置键，测试整体替换、用完还原）：
//   - waContactSyncFullMinInterval：同一账号两次「全量重拉」的最小间隔。full=true 会先
//     DeleteAppStateVersion（WM/appstate.go:50）把已同步过的账号擦成 v0，代价是重付一次
//     全量快照；所以只允许「显式 WhatsappSyncContacts(full=true)」或「快路拉过但本地仍是
//     0 条」这两种升级，且都得受这个窗口约束。
//   - waContactSyncTimeout：单次同步的等待上限。预算账：连接 15s + 同步 20s < 下游
//     axios 的 40s，写在 WhatsappSyncContacts/WhatsappListContacts 的注释里供调用方核对。
var (
	waContactSyncFullMinInterval = 10 * time.Minute
	waContactSyncTimeout         = 20 * time.Second
	// waAppStateKeyWait 是「解码缺 app state 密钥」之后继续等手机回钥的上限。
	// 库在 ErrKeyNotFound 时已经异步发出密钥请求，但 FetchAppState 当场就返回；
	// 不等这一小段，群发会在密钥落地前被判失败。0 表示不等、立刻按失败返回（测试用）。
	waAppStateKeyWait = 8 * time.Second
)

// waLoginErr 把「登录/取会话阶段的失败」折成门面错误。
//
// 比照 waSendErr（wa_client.go）的写法，但文案是「本条未执行」而不是「本条未发出」：
// 这一层的失败发生在还没碰会话之前，一条都没出去过，不能与「发出去了但服务端报错」混读。
//
// 专治的误诊有两类，两类文案互斥，值班据此分诊：
//   - 调用方 ctx 取消/超时 → 「调用方上下文已结束（请求超时或客户端断开），本条未执行」；
//     原始错误仍用 %w 留在链上（waitConnected 那句「等待连上时被取消」也在里面）。
//   - 本机 EffectiveConnectTimeout 到点没连上 → 原样带出「… 内未能连上」，
//     这条不含「上下文已结束」，与上一条不会读成同一件事。
func waLoginErr(label string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("whatsapp %s失败: 调用方上下文已结束（请求超时或客户端断开），本条未执行: %w", label, err)
	}
	return fmt.Errorf("whatsapp %s失败: %w", label, err)
}

// waContactSync 是单个账号的同步单飞锁与全量重拉节流账。
type waContactSync struct {
	mu sync.Mutex
	// lastFullAt 最近一次以 full=true 真拉过的时刻（full=true 会先擦 app state 版本）。
	// 零值表示这个账号在本进程里从没全量重拉过，窗口对它不设限。
	lastFullAt time.Time
}

// stateSyncLock 取（并惰性建）某个账号的同步锁，照 restartLock 的范式。
func (m *waManager) stateSyncLock(key string) *waContactSync {
	if v, ok := m.stateSync.Load(key); ok {
		return v.(*waContactSync)
	}
	s := &waContactSync{}
	v, _ := m.stateSync.LoadOrStore(key, s)
	return v.(*waContactSync)
}

// readWaContacts 读本地联系人区（同步产物），失败折算成门面的「获取好友列表」错误串。
func readWaContacts(ctx context.Context, w *waClientWrapper) (map[types.JID]types.ContactInfo, error) {
	contacts, err := w.sess.GetContacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp 获取好友列表失败: %w", err)
	}
	return contacts, nil
}

// syncContactAppState 同账号单飞地拉一次联系人 app state，返回同步后回读的本地联系人。
//
// wantFull / escalate 的实际取值组合（escalate 只会与 wantFull=true 同时出现，
// 「快路 + 升级」这个组合没有调用方）：
//   - (false, false)：快路。full=false 随时可试——已同步过的账号库内零网络返回
//     （WM/appstate.go:61-63），所以这里不额外设间隔；但**拿锁后先双检本地**，
//     前一个并发调用已经拉到数据时后进者不再拉、直接读本地结果。
//   - (true, false)：显式 WhatsappSyncContacts(full=true)。受 waContactSyncFullMinInterval
//     窗口约束，窗口内退化成快路（调用方要的就是「拉一次」，不该白擦版本）。
//   - (true, true)：调用方在快路拉过但本地仍 0 条后的升级。同样受窗口约束，
//     但窗口挡住时**不再补一次快路**（同一次调用里刚试过，重试没有信息增量），直接交回本地结果。
//
// 为什么必须压住 full=true：它会先 DeleteAppStateVersion（WM/appstate.go:50）把已同步过的
// 账号擦成 v0，代价是重付一次全量快照；且擦版本与库侧异步补钥竞争时可能拉到半截快照，
// 而 PutAllContactNames 是整表覆盖（WM/appstate.go:186），半截即丢数据。
// 库侧的 appStateSyncLock 只串行化、不去重：两个并发的 full 拉取会各自擦一次版本互踩，
// 所以单飞必须压在本包这一层。
func (m *waManager) syncContactAppState(ctx context.Context, w *waClientWrapper, wantFull, escalate bool) (map[types.JID]types.ContactInfo, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	key := w.jid.String()
	s := m.stateSyncLock(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	full := wantFull
	if full && !s.lastFullAt.IsZero() && time.Since(s.lastFullAt) < waContactSyncFullMinInterval {
		if escalate {
			vars.Debug("WhatsApp[%s] 全量重拉处在 %s 窗口内，跳过（不擦 app state 版本）", key, waContactSyncFullMinInterval)
			return readWaContacts(ctx, w)
		}
		full = false
	}
	if !wantFull && !escalate {
		// 单飞双检（只对快路调用方生效；显式 full=true 的请求即便被窗口退化成快路，
		// 也照常真发一次——命令是调用方下的，不能替他省成一次都不做）：
		// 等锁期间前一个并发调用可能已经把联系人拉下来了。
		if contacts, err := readWaContacts(ctx, w); err == nil && len(contacts) > 0 {
			return contacts, nil
		}
	}

	sctx, cancel := context.WithTimeout(ctx, waContactSyncTimeout)
	err := w.sess.SyncContactAppState(sctx, full)
	if err != nil && errors.Is(err, appstate.ErrKeyNotFound) {
		// 库已在后台向手机要缺失的 app state 密钥，并会在密钥到达后自己再拉一遍。
		// 这里守到联系人落库或等待上限，避免群发在密钥落地前被判失败。
		if contacts, ok := waitContactsAfterMissingKey(sctx, w); ok {
			cancel()
			return contacts, nil
		}
		err = w.sess.SyncContactAppState(sctx, false)
	}
	// 「谁先到的点」必须在 cancel 之前问：sctx 到点而调用方 ctx 还活着 ⇒ 是**本包**的同步
	// 预算不够，不是上游断连。两者混成一句「调用方上下文已结束」会把值班引向
	// 「客户端挂断」，而真凶是我们自己设的 waContactSyncTimeout。
	selfTimeout := errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	if err != nil {
		// 一律报错：这里退化成「返回空集 + nil」就是把「同步失败」伪装成「这账号没好友」，
		// 下游会把群发目标算成 0 条并当成功记账——本次改动要治的就是这个形态。
		return nil, waSyncErr(key, err, selfTimeout)
	}
	if full {
		s.lastFullAt = time.Now()
	}
	contacts, err := readWaContacts(ctx, w)
	if err != nil {
		return nil, err
	}
	if len(contacts) == 0 {
		// 唯一合法的「0 条且无错」：同步成功而服务器快照本就为空。留这条日志供分诊，
		// 与「快照非 0 但都是没有号码的条目」区分开。
		vars.Info("WhatsApp[%s] contacts 快照 0 条（app state 同步成功，服务器侧通讯录为空）", key)
	}
	return contacts, nil
}

// waitContactsAfterMissingKey 在密钥请求已经发出之后轮询本地联系人，并间或再拉一次快路。
// 等到至少 1 条就返回 true。等待上限为 0、上下文结束、或到点仍是空集时返回 false。
func waitContactsAfterMissingKey(ctx context.Context, w *waClientWrapper) (map[types.JID]types.ContactInfo, bool) {
	if waAppStateKeyWait <= 0 {
		return nil, false
	}
	deadline := time.Now().Add(waAppStateKeyWait)
	for {
		contacts, err := readWaContacts(ctx, w)
		if err == nil && len(contacts) > 0 {
			return contacts, true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, false
		}
		if rerr := w.sess.SyncContactAppState(ctx, false); rerr == nil {
			contacts, err = readWaContacts(ctx, w)
			if err == nil && len(contacts) > 0 {
				return contacts, true
			}
			if err == nil {
				return contacts, true
			}
		} else if !errors.Is(rerr, appstate.ErrKeyNotFound) {
			return nil, false
		}
		wait := 400 * time.Millisecond
		if d := time.Until(deadline); d < wait {
			wait = d
		}
		if wait <= 0 {
			return nil, false
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		case <-timer.C:
		}
	}
}

// waSyncErr 折算同步阶段的失败。
//
// 库里缺 app state 主密钥时是**另一件事**：不是网络问题、也不是本包能代劳的，
// 要用户去手机端「已关联设备」里确认那台设备已同步对话与联系人，所以单独点名并把
// 「密钥」写进文案（配对码绑的号最常见的就是这个形态，与「通讯录真为空」必须分得开）。
//
// selfTimeout 是本包同步预算（waContactSyncTimeout）自己到点：它和「调用方 ctx 结束」
// 在错误里长一个样（都是 context.DeadlineExceeded），但归因完全相反——一条要人去查
// 我们的超时配置，一条要人去查上游请求为什么挂断，所以必须分成两句文案。
// 其余失败走 waLoginErr 的两级折算（调用方上下文已结束 / 原样带出），%w 全程保留。
func waSyncErr(key string, err error, selfTimeout bool) error {
	if errors.Is(err, appstate.ErrKeyNotFound) {
		return fmt.Errorf("whatsapp 同步联系人失败: 账号 %s 的 app state 配对密钥未共享，"+
			"请在手机端确认已关联设备（设置 > 已关联设备）: %w", key, err)
	}
	if selfTimeout {
		return fmt.Errorf("whatsapp 同步联系人失败: 本包同步预算 %s 到点，账号 %s 本轮未同步完成"+
			"（是本包超时，不是调用方断连）: %w", waContactSyncTimeout, key, err)
	}
	return waLoginErr("同步联系人", err)
}

// WhatsappSyncContacts 主动把账号手机上的通讯录拉一次（critical_unblock_low app state），
// 成功时返回同步后的**好友条数**——与 WhatsappListContacts 同一口径，只数存过通讯录的
// （本人、仅聊过的 pushName 行、企业号展示名行都不计）。
//
// 为什么要单独开这个出口：whatsmeow 的联系人只来自这份 app state 快照
// （WM/appstate/keys.go:139-143），批量落库只发生在显式 FetchAppState 的全量分支里
// （WM/appstate.go:182-191），**库不会在连接建立时自动拉**。少了这一步，新配对账号
// （扫码或配对码都一样）的通讯录永远是空的，下游群发把目标集算成 0 条。
//
// 账号不在跑时本门面会自动启动并等到连上（ensureLoggedIn）；库里没有设备行时报错文案
// 仍含「没有在跑」并带出「未配对」，处置是先去扫码/配对。
//
// full 的取舍：false 走快路（该账号已同步过时库内零网络往返）；true 会先擦掉 app state
// 版本再重付一次全量快照，同账号受 waContactSyncFullMinInterval（10 分钟）窗口约束，
// 窗口内自动退化为快路。首次同步不需要传 true——version==0 时库内部自动按全量走
// （WM/appstate.go:59-60）。
//
// 耗时预算：连接 EffectiveConnectTimeout（缺省 15 秒）+ 同步 waContactSyncTimeout（20 秒）
// < 下游 axios 的 40 秒。失败一律报错，不会返回「0 条 + nil」。
func WhatsappSyncContacts(ctx context.Context, fromJID string, full bool) (int, error) {
	m, err := currentWA()
	if err != nil {
		return 0, err
	}
	if ctx == nil {
		ctx = runCtx()
	}
	w, err := m.ensureLoggedIn(ctx, fromJID)
	if err != nil {
		return 0, waLoginErr("同步联系人", err)
	}
	contacts, err := m.syncContactAppState(ctx, w, full, false)
	if err != nil {
		return 0, err
	}
	return len(waContactList(contacts, w.jid.User)), nil
}
