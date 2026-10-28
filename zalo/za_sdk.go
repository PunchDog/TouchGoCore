package zalo

// 本文件是 zcago v0.3.0 的唯一接触面：窄 seam（zaSDK/zaConn）与真实现。
//
// 分层口径：manager/facade/pairing 只依赖 zaSDK/zaConn 与本包类型（za_types.go），
// 不 import zcago；zcago 的类型只允许出现在本文件里，并在边界处映射为 UserInfo/
// GroupInfo。这样上游升级（v0.4 的附件实现、字段改名）只需改这一个文件。
//
// 约束（已读 v0.3.0 源码）：
//   - api.New 在登录成功时已构造好 listener，这里不再另建；
//   - SendMessage 的附件分支在 v0.3.0 直接 panic("not implemented")，所以只封装文本；
//   - GetAllGroups 只返回 groupID→version，群名/人数要再调 GetGroupInfo；
//   - GetAllFriends 单页上限 20000，本包只取第一页。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strings"
	"sync"

	"github.com/amrakk/zcago"
	zcagoapi "github.com/amrakk/zcago/api"
	"github.com/amrakk/zcago/model"
	"github.com/amrakk/zcago/session"
)

// zaFriendPageSize 是好友列表的单页大小：zcago 默认也是 20000，这里显式写死，
// 避免上游改默认值时本包悄悄少拿好友。
const zaFriendPageSize = 20000

var (
	// errZANilAPI 表示 zcago 没有返回可用的 API 句柄。
	errZANilAPI = errors.New("zalo API 不可用")
	// errZANilContext 表示 zcago 会话上下文为空或读取失败。
	errZANilContext = errors.New("zalo 会话上下文不可用")
	// errZANilResponse 表示 zcago 接口返回了 nil 响应（没有错误但没有数据）。
	errZANilResponse = errors.New("zalo 接口返回空响应")
)

// zaSDK 是登录能力的窄 seam。
//
// 两个方法都返回已经带会话上下文的 zaConn：manager 拿到后即可 UID/快照/收发，
// 不需要再接触 zcago 的 API 接口。fake 实现见测试。
type zaSDK interface {
	Login(ctx context.Context, cred zcago.Credentials) (zaConn, error)
	LoginQR(ctx context.Context, opt *zcago.LoginQROption, cb zcago.LoginQRCallback) (zaConn, error)
}

// zaConn 是一个已登录 Zalo 会话的全部能力面。
//
// 所有方法都允许并发调用（zcago 内部对 HTTP 与 listener 通道有各自的同步）；
// StartListener/StopListener 的并发语义见 za_listener.go。
type zaConn interface {
	UID() string
	SnapshotCredentials() (zcago.Credentials, error)
	SendText(ctx context.Context, threadID string, group bool, text string) (string, error)
	DeleteChat(ctx context.Context, threadID string, group bool, boundary ChatBoundary) error
	GetAllFriends(ctx context.Context) ([]UserInfo, error)
	GetAllGroups(ctx context.Context) (map[string]string, error) // groupID -> version
	GetGroupInfo(ctx context.Context, ids ...string) ([]GroupInfo, error)
	GetUserInfo(ctx context.Context, ids ...string) ([]UserInfo, error)
	StartListener(ctx context.Context, onMessage func(InboundMessage), onTerminal func(code int, reason string)) error
	StopListener()
}

// zaRealSDK 是 zaSDK 的 zcago 实现。
//
// **每次 Login/LoginQR 都新建一个 zcago 句柄与私有 http.Client**：zcago 的
// defaultOptions 把 client 设成 http.DefaultClient（session/options.go:70），
// newContextImpl 在 client.Jar 为空时新建 jar 并写回 cfg.client.Jar
// （session/context_impl.go:48-53）。若共用默认 client，第二个账号会复用第一个
// 账号的 jar，cookie 互相覆盖（P0 跨账号凭据串线）。私有 client+独立 jar 是
// 多账号隔离的必要条件。
type zaRealSDK struct{}

var _ zaSDK = (*zaRealSDK)(nil)

// newZASDK 构造生产用 SDK（无状态工厂：真正的 zcago 句柄在每次登录时创建）。
func newZASDK() zaSDK {
	return &zaRealSDK{}
}

// newZAHTTPClient 返回一个带独立 cookie jar 的私有 HTTP client。
// 不设 Timeout：listener 是长连接，请求超时由 manager 在 context 上控制。
//
// Transport 包了 zaFallbackTransport：传输层失败不返回 (nil, err)，而是折成 599
// 合成响应。原因是上游 zcago v0.3.0 的登录链会对 nil response 取 .Status 而 panic，
// 且该 panic 在上游自己的 goroutine 里、调用方 recover 接不住（见 za_http.go 头注）。
func newZAHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:       jar,
		Transport: zaFallbackTransport{base: http.DefaultTransport},
	}
}

// zaNewZaloHandle 构造一次登录用的 zcago 句柄。
//
// 关掉 zcago 的日志与版本检查：前者会往 stdout 打请求噪音（含会话参数），
// 后者每次登录多发一次无关请求。
//
// 用包级 var 而不是方法：单测要把它换成「一登录就 panic」的假句柄，验证
// zaRecoverLoginPanic 是活闸（见 za_login_panic_test.go）。
var zaNewZaloHandle = func() zcago.Zalo {
	return zcago.NewZalo(
		zcago.WithLogging(false),
		zcago.WithCheckUpdate(false),
		zcago.WithHTTPClient(newZAHTTPClient()),
	)
}

// errZALoginUpstreamPanic 表示上游登录链在 HTTP 请求阶段 panic（拿不到 response）。
//
// 触发面：zcago v0.3.0 的 auth.login 在 httpx.Request 失败（网络不可达、超时、
// ctx 取消）时会对 nil response 取 .Status（session/auth/login.go:62/74/167/176），
// 必然 panic。登录失败是可恢复错误，不能让它带走整个进程。
var errZALoginUpstreamPanic = errors.New(
	"zalo 登录失败：上游 SDK 请求无响应（getServerInfo/getLoginInfo 连接失败或超时；检查到 wpa.chat.zalo.me 的网络/代理，必要时调大 zalo.sessions.request_timeout_sec）")

// zaRecoverLoginPanic 把上游登录链的 panic 折算成可配置可判的错误。
// 只用于登录入口：登录是懒登录/重登，任何一次网络抖动都不该升级成进程崩溃。
func zaRecoverLoginPanic(errp *error) {
	if r := recover(); r != nil {
		*errp = fmt.Errorf("%w: %v", errZALoginUpstreamPanic, r)
	}
}

// Login 用已保存的凭证恢复会话。
func (s *zaRealSDK) Login(ctx context.Context, cred zcago.Credentials) (conn zaConn, err error) {
	defer zaRecoverLoginPanic(&err)
	api, err := zaNewZaloHandle().Login(ctx, cred)
	if err != nil {
		return nil, err
	}
	return newZARealConn(api)
}

// LoginQR 走扫码登录，cb 负责把二维码/扫描/过期事件交给 pairing 层。
func (s *zaRealSDK) LoginQR(ctx context.Context, opt *zcago.LoginQROption, cb zcago.LoginQRCallback) (conn zaConn, err error) {
	defer zaRecoverLoginPanic(&err)
	api, err := zaNewZaloHandle().LoginQR(ctx, opt, cb)
	if err != nil {
		return nil, err
	}
	return newZARealConn(api)
}

// zaRealConn 是 zaConn 的 zcago 实现。
//
// sc 是登录后的会话上下文（UID/IMEI/UA/Language/CookieJar 唯一权威来源），
// 由 api.GetContext() 取一次并长期持有；快照凭证时直接读它，保证拿到的是
// 服务端刷新后的 cookie，而不是导入时的旧值。
type zaRealConn struct {
	api zcago.API
	sc  session.Context

	mu              sync.Mutex
	listenerStarted bool
	listenerCancel  context.CancelFunc
	listenerWG      sync.WaitGroup
}

var _ zaConn = (*zaRealConn)(nil)

// newZARealConn 把 zcago.API 包成 zaConn。
func newZARealConn(api zcago.API) (zaConn, error) {
	if api == nil {
		return nil, errZANilAPI
	}
	sc, err := api.GetContext()
	if err != nil {
		return nil, errors.Join(errZANilContext, err)
	}
	if sc == nil {
		return nil, errZANilContext
	}
	return &zaRealConn{api: api, sc: sc}, nil
}

// UID 返回当前登录账号的 Zalo UID。
func (c *zaRealConn) UID() string { return c.sc.UID() }

// SnapshotCredentials 把当前活会话快照成可落库的凭证。
func (c *zaRealConn) SnapshotCredentials() (zcago.Credentials, error) {
	return snapshotZACredentials(c.sc)
}

// SendText 发送一条纯文本消息，返回服务端受理后的 msgID。
//
// 空 msgID 不是错误（resp 或 resp.Message 为 nil 时返回 ""、nil），由 facade
// 折算 ErrZaloEmptyMessageID：供应商没给 id 等于没受理，不能装成成功。
func (c *zaRealConn) SendText(ctx context.Context, threadID string, group bool, text string) (string, error) {
	tt := model.ThreadTypeUser
	if group {
		tt = model.ThreadTypeGroup
	}
	resp, err := c.api.SendMessage(ctx, threadID, tt, zcagoapi.MessageContent{Msg: text})
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Message == nil {
		return "", nil
	}
	return resp.Message.MsgID, nil
}

// DeleteChat 清空本端（onlyMe）threadID 会话的消息。
//
// 上游 deleteconver 只回 status，非 0 状态在 Resolve 里已折算成错误；空响应同样
// 不能装成成功。boundary 经 zaNormalizeBoundary 归一（OwnerID 留空 ⇒ 取当前登录
// UID；CliMsgID 留空 ⇒ 用 GlobalMsgID 顶替，理由见 ChatBoundary 注释）。
func (c *zaRealConn) DeleteChat(ctx context.Context, threadID string, group bool, boundary ChatBoundary) error {
	tt := model.ThreadTypeUser
	if group {
		tt = model.ThreadTypeGroup
	}
	boundary = zaNormalizeBoundary(boundary, c.UID())
	resp, err := c.api.DeleteChat(ctx, threadID, tt, zcagoapi.DeleteChatLastMessage{
		OwnerID:     boundary.OwnerID,
		CliMsgID:    boundary.CliMsgID,
		GlobalMsgID: boundary.GlobalMsgID,
	})
	if err != nil {
		return err
	}
	if resp == nil {
		return errZANilResponse
	}
	return nil
}

// zaNormalizeBoundary 归一删除会话的边界参数。
//
// OwnerID 留空 ⇒ 取当前登录账号的 UID（发送后清理的边界就是本端刚发出去的那条，
// owner 恒为自己）；空 owner 会被上游拒成 114。
// CliMsgID 留空 ⇒ 用 GlobalMsgID 顶替：空值会被上游拒成 112，整次删除不生效
// （2026-10-27 对真实接口核实）。
func zaNormalizeBoundary(boundary ChatBoundary, selfUID string) ChatBoundary {
	if strings.TrimSpace(boundary.OwnerID) == "" {
		boundary.OwnerID = strings.TrimSpace(selfUID)
	}
	if strings.TrimSpace(boundary.CliMsgID) == "" {
		boundary.CliMsgID = boundary.GlobalMsgID
	}
	return boundary
}

// GetAllFriends 拉取全量好友（单页 20000），返回按 UserID 升序的切片。
func (c *zaRealConn) GetAllFriends(ctx context.Context) ([]UserInfo, error) {
	resp, err := c.api.GetAllFriends(ctx, model.OffsetPaginationOptions{
		Count: zaFriendPageSize,
		Page:  1,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errZANilResponse
	}
	return mapZAUsers([]model.User(*resp)), nil
}

// GetAllGroups 返回 groupID → version 的副本。
// 不把 zcago 的 map 直接交出去：调用方（缓存/差分）若原地增删，会污染下一次
// 由同一响应持有的视图。
func (c *zaRealConn) GetAllGroups(ctx context.Context) (map[string]string, error) {
	resp, err := c.api.GetAllGroups(ctx)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errZANilResponse
	}
	out := make(map[string]string, len(resp.GridVerMap))
	for id, ver := range resp.GridVerMap {
		out[id] = ver
	}
	return out, nil
}

// GetGroupInfo 批量查询群资料，返回按 GroupID 升序的切片。
// zcago 的 GridInfoMap 偶有 GroupID 为空而键有值，mapZAGroups 用键补齐。
func (c *zaRealConn) GetGroupInfo(ctx context.Context, ids ...string) ([]GroupInfo, error) {
	resp, err := c.api.GetGroupInfo(ctx, ids...)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errZANilResponse
	}
	return mapZAGroups(resp.GridInfoMap), nil
}

// GetUserInfo 批量查询用户资料，返回按 UserID 升序的切片。
// 只返回 ChangedProfiles 里真实存在的条目，不替缺失的 id 补空壳。
func (c *zaRealConn) GetUserInfo(ctx context.Context, ids ...string) ([]UserInfo, error) {
	resp, err := c.api.GetUserInfo(ctx, ids...)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errZANilResponse
	}
	users := make([]model.User, 0, len(resp.ChangedProfiles))
	for _, u := range resp.ChangedProfiles {
		users = append(users, u)
	}
	return mapZAUsers(users), nil
}

// mapZAUser 把 zcago model.User 映射成本包 UserInfo。
// 只做字段搬运：供应商没给的列保持零值，不补造。
func mapZAUser(u model.User) UserInfo {
	return UserInfo{
		UserID:      u.UserId,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		ZaloName:    u.ZaloName,
		Phone:       u.PhoneNumber,
		Avatar:      u.Avatar,
		Status:      u.Status,
	}
}

// mapZAUsers 映射并排序。先复制到新切片再排，绝不原地重排 API 返回的底层数组。
func mapZAUsers(users []model.User) []UserInfo {
	out := make([]UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, mapZAUser(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}

// mapZAGroup 把 zcago api.GroupInfo 映射成本包 GroupInfo。
// AdminIDs 深拷贝：GroupInfo 会进缓存并被多个调用方读取，不能让一方修改
// 输入切片（或缓存）时串改另一方的视图。
func mapZAGroup(g zcagoapi.GroupInfo) GroupInfo {
	var admins []string
	if g.AdminIDs != nil {
		admins = append([]string(nil), g.AdminIDs...)
	}
	return GroupInfo{
		GroupID:     g.GroupID,
		Name:        g.Name,
		Description: g.Description,
		CreatorID:   g.CreatorID,
		Avatar:      g.Avatar,
		Version:     g.Version,
		TotalMember: g.TotalMember,
		MaxMember:   g.MaxMember,
		Type:        int(g.Type),
		AdminIDs:    admins,
		LockSendMsg: g.Setting.LockSendMsg != 0,
	}
}

// mapZAGroups 映射 GridInfoMap 并排序。map 键是查询时用的 groupID，
// 当内嵌 GroupID 为空时用它补齐；按 GroupID 升序让结果稳定可对拍。
func mapZAGroups(m map[string]zcagoapi.GroupInfo) []GroupInfo {
	out := make([]GroupInfo, 0, len(m))
	for id, g := range m {
		if g.GroupID == "" {
			g.GroupID = id
		}
		out = append(out, mapZAGroup(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out
}
