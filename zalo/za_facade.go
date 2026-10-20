package zalo

// 本文件是 Zalo 通道的公开门面与每账号发送闸。
//
// 门面纪律：所有入口先取 currentManager（未启动 → ErrZaloNotStarted），再
// accountOrCreate（ID 校验 → ErrZaloBadAccountID），最后才碰 conn/缓存；门面不
// 改写 manager 的错误，也不直接操作 zaAccount 的 conn 字段。
//
// 发送闸（sendText）：每账号一把 sendMu，整段覆盖「滚动 1 小时配额 → 3–5 秒随机
// 间隔 → 真发 → 记账」。配额先判后睡：超限立即 ErrZaloSendLimit，不让调用方白等
// 3–5 秒；失败发送也占配额（请求已经打到服务端）；等待被 ctx 取消则不占配额、不
// 推进 lastSend。错误文案只描述规则，绝不回显正文（消息可能敏感）。
//
// ZaloLogout 在文件末尾（停 listener、清 conn、保留 Postgres 会话行）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 门面层的两个参数错误：正文/收件人为空。
// 文案只描述规则，不回显正文；正文可能承载验证码、订单号等敏感信息。
var (
	errZAFacadeEmptyText   = errors.New("zalo 发送失败: 正文为空")
	errZAFacadeEmptyThread = errors.New("zalo 发送失败: 收件人为空")
)

// zaFacadeAccount 是账号类门面的公共前缀：先确认通道已启动，再校验/创建账号。
// 返回 manager 供 LoginWithCookie/StartClient 这类仍要委托 manager 的入口使用。
func zaFacadeAccount(accountID string) (*zaManager, *zaAccount, error) {
	m, err := currentManager()
	if err != nil {
		return nil, nil, err
	}
	a, err := m.accountOrCreate(accountID)
	if err != nil {
		return nil, nil, err
	}
	return m, a, nil
}

// ZaloLoginWithCookie 用导入的 cookie 凭证登录并落库。
// raw 支持完整 Credentials / 裸 cookie 数组 / J2Cookie / 信封四种形态（解析在
// manager 内完成）；形态不合法返回 ErrZaloBadCredentials。
func ZaloLoginWithCookie(ctx context.Context, accountID string, raw []byte) (*AccountInfo, error) {
	m, _, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return m.LoginWithCookie(ctx, accountID, raw)
}

// ZaloStartClient 显式拉起账号：按需从会话库读凭证登录，成功后返回账号信息。
func ZaloStartClient(ctx context.Context, accountID string) (*AccountInfo, error) {
	m, _, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return m.StartClient(ctx, accountID)
}

// ZaloDeleteAccount 删除账号：停 listener、清 conn、删库行、移出内存；幂等。
func ZaloDeleteAccount(ctx context.Context, accountID string) error {
	m, _, err := zaFacadeAccount(accountID)
	if err != nil {
		return err
	}
	return m.DeleteAccount(ctx, accountID)
}

// ZaloListAccounts 返回库里的账号元数据并合并内存在线状态，按账号 ID 升序。
func ZaloListAccounts(ctx context.Context) ([]AccountInfo, error) {
	m, err := currentManager()
	if err != nil {
		return nil, err
	}
	return m.ListAccounts(ctx)
}

// ZaloSendMessage 向单聊线程 to 发送一条纯文本消息，返回服务端受理的 msgID。
func ZaloSendMessage(ctx context.Context, accountID, to, text string) (string, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return "", err
	}
	conn, err := a.ensureConn(ctx)
	if err != nil {
		return "", err
	}
	return a.sendText(ctx, conn, false, to, text)
}

// ZaloSendGroupMessage 向群 groupID 发送一条纯文本消息，返回服务端受理的 msgID。
func ZaloSendGroupMessage(ctx context.Context, accountID, groupID, text string) (string, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return "", err
	}
	conn, err := a.ensureConn(ctx)
	if err != nil {
		return "", err
	}
	return a.sendText(ctx, conn, true, groupID, text)
}

// ZaloListFriends 返回好友列表：缓存新鲜直接返回，过期自动刷新（5 分钟闸）。
func ZaloListFriends(ctx context.Context, accountID string) ([]UserInfo, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return a.listFriends(ctx, false)
}

// ZaloSyncFriends 强制刷新好友列表，但受 5 分钟最小间隔约束
// （间隔内返回 ErrZaloFetchTooSoon）。
func ZaloSyncFriends(ctx context.Context, accountID string) ([]UserInfo, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return a.listFriends(ctx, true)
}

// ZaloListGroups 返回群列表：缓存新鲜直接返回，过期自动刷新（版本差分）。
func ZaloListGroups(ctx context.Context, accountID string) ([]GroupInfo, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return a.listGroups(ctx, false)
}

// ZaloSyncGroups 强制刷新群列表，同样受 5 分钟最小间隔约束。
func ZaloSyncGroups(ctx context.Context, accountID string) ([]GroupInfo, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return a.listGroups(ctx, true)
}

// ZaloGetUserInfo 按 ID 查询用户资料：缓存命中不等待，缺失才走 1.5 秒共用闸。
func ZaloGetUserInfo(ctx context.Context, accountID string, userIDs ...string) (map[string]UserInfo, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return a.userInfo(ctx, userIDs)
}

// ZaloGetGroupInfo 按 ID 查询群资料，与用户资料共用缓存与 1.5 秒闸。
func ZaloGetGroupInfo(ctx context.Context, accountID string, groupIDs ...string) (map[string]GroupInfo, error) {
	_, a, err := zaFacadeAccount(accountID)
	if err != nil {
		return nil, err
	}
	return a.groupInfo(ctx, groupIDs)
}

// sendText 是每账号唯一的发送闸。
//
// 顺序固定为「参数检查 → 小时配额 → 随机间隔 → 真发 → 记账」，整段由 sendMu
// 串行化，同一账号的发送严格顺序执行，不同账号之间互不阻塞。
//
// 记账口径：
//   - 配额在睡眠之前判定：已超限直接 ErrZaloSendLimit，不让调用方等到睡眠结束才被拒；
//   - 睡眠成功后才 append 小时窗口，随后无论 conn.SendText 成败都占槽（请求已经
//     打到服务端，失败重试同样计入供应商侧频控）；
//   - 睡眠被 ctx 取消时立即返回：不 append 窗口、不推进 lastSend，下一次发送仍按
//     「距上次成功收尾」的间隔计算；
//   - conn == nil 表示账号没有可用会话，返回 ErrZaloNotLoggedIn；
//   - 空 msgID（含 err == nil 的空回执）折算 ErrZaloEmptyMessageID：没拿到 id 等于
//     没受理，不能装成成功。
func (a *zaAccount) sendText(ctx context.Context, conn zaConn, group bool, threadID, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errZAFacadeEmptyText
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", errZAFacadeEmptyThread
	}
	if conn == nil {
		return "", ErrZaloNotLoggedIn
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("zalo 发送前请求已取消: %w", err)
	}

	m := a.mgr

	a.sendMu.Lock()
	defer a.sendMu.Unlock()

	// 滚动 1 小时窗口：先按 now 剪掉到期的记录，再判配额。
	now := m.clock.Now()
	window := a.sendWindow[:0]
	for _, ts := range a.sendWindow {
		if now.Sub(ts) < time.Hour {
			window = append(window, ts)
		}
	}
	a.sendWindow = window
	if max := m.limits.sendMaxPerHour; max > 0 && len(a.sendWindow) >= max {
		return "", fmt.Errorf("%w: 滚动 1 小时已发送 %d 条，上限 %d 条", ErrZaloSendLimit, len(a.sendWindow), max)
	}

	// 首条立即发；之后距上次收尾随机 [sendMin,sendMax]，等待可被 ctx 取消。
	if !a.lastSend.IsZero() {
		delay := m.randDelay(m.limits.sendMin, m.limits.sendMax)
		if err := m.clock.Sleep(ctx, delay); err != nil {
			return "", fmt.Errorf("zalo 发送间隔等待被取消: %w", err)
		}
	}

	// 睡眠期间账号可能被登出/删除/被踢：发送前复核 stopped 与 conn 同一性，
	// 避免「DeleteAccount 返回后仍发出一条」。不占配额、不推进 lastSend。
	if a.stoppedOrStopping() {
		return "", ErrZaloNotStarted
	}
	if cur := a.currentConn(); cur == nil || cur != conn {
		return "", ErrZaloNotLoggedIn
	}

	// 睡眠成功后重新取时：窗口记录的是真实起发时刻。
	now = m.clock.Now()
	a.sendWindow = append(a.sendWindow, now)
	// defer 在 sendMu 释放前执行，保证并发调用看到一致的 lastSend。
	defer func() { a.lastSend = m.clock.Now() }()

	sendCtx, cancel := context.WithTimeout(ctx, m.limits.requestTimeout)
	defer cancel()
	msgID, err := conn.SendText(sendCtx, threadID, group, text)
	if err != nil {
		// 上游 error 可能带响应体；Error() 只给固定文案，Unwrap 保留原错误供排障。
		return "", zaSafeWrap("zalo 发送失败", err)
	}
	if strings.TrimSpace(msgID) == "" {
		return "", ErrZaloEmptyMessageID
	}
	return msgID, nil
}

// ZaloLogout 登出账号：停 listener、清内存里的 conn，但保留会话库行——下次发送/查询
// 会按需重新登录。账号从未上线或未知账号都是幂等 no-op；通道未启动返回
// ErrZaloNotStarted，账号 id 非法返回 ErrZaloBadAccountID。
func ZaloLogout(ctx context.Context, accountID string) error {
	m, err := currentManager()
	if err != nil {
		return err
	}
	if _, err := m.accountOrCreate(accountID); err != nil {
		return err
	}
	return m.Logout(ctx, accountID)
}
