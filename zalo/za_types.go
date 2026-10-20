package zalo

import (
	"errors"
	"time"
)

// 本文件是 Zalo 通道的公开类型与错误哨兵：store / manager / facade / pairing /
// listener 各文件只引用这里的名字，不改名、不增删字段。新增能力请另开文件用嵌入
// 或新类型，不要改这里的既有形状。

// AccountInfo 是一个 Zalo 账号的可公开事实。
// 字段表即纪律：只有账号标识与状态，没有任何 cookies/IMEI/UA 等会话材料。
type AccountInfo struct {
	ID        string    `json:"id"`
	UID       string    `json:"uid"`
	LoggedIn  bool      `json:"logged_in"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UserInfo 是好友列表/用户资料查询的返回项。
// 字段映射自 zcago model.User；供应商没给的列就是空串，不做补造。
type UserInfo struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	ZaloName    string `json:"zalo_name"`
	Phone       string `json:"phone"`
	Avatar      string `json:"avatar"`
	Status      string `json:"status"`
}

// GroupInfo 是群列表/群资料查询的返回项。
// Version 来自 GetAllGroups 的 GridVerMap；LockSendMsg 取自群设置，true 表示
// 只有管理员能发言（本包不替业务判定能不能发，原样交出去）。
type GroupInfo struct {
	GroupID     string   `json:"group_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	CreatorID   string   `json:"creator_id"`
	Avatar      string   `json:"avatar"`
	Version     string   `json:"version"`
	TotalMember int      `json:"total_member"`
	MaxMember   int      `json:"max_member"`
	Type        int      `json:"type"`
	AdminIDs    []string `json:"admin_ids"`
	LockSendMsg bool     `json:"lock_send_msg"`
}

// InboundMessage 是 listener 收到的一条文本消息（非文本不广播）。
// TimestampMS 是 zcago 报文里的 ts 按毫秒整数解析的结果，解析失败为 0。
type InboundMessage struct {
	AccountID   string `json:"account_id"`
	ThreadID    string `json:"thread_id"`
	ThreadType  string `json:"thread_type"` // user / group
	IsSelf      bool   `json:"is_self"`
	MsgID       string `json:"msg_id"`
	FromID      string `json:"from_id"`
	ToID        string `json:"to_id"`
	SenderName  string `json:"sender_name"`
	Text        string `json:"text"`
	TimestampMS int64  `json:"timestamp_ms"`
}

// QREvent 是扫码会话推给 WS / CallZaloMsg+"QR" 的事件。
// Event 取值：qr / scanned / expired / declined / success / error / timeout。
// ImageBase64 是 PNG 原始字节的 base64（不带 data: 前缀）；二维码等同登录凭据，
// 不得写入日志。
type QREvent struct {
	SessionID   string `json:"session_id"`
	AccountID   string `json:"account_id"`
	Event       string `json:"event"`
	ImageBase64 string `json:"image_base64,omitempty"`
	QRCode      string `json:"qr_code,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	UID         string `json:"uid,omitempty"`
	Error       string `json:"error,omitempty"`
	TimeoutMS   int64  `json:"timeout_ms,omitempty"`
}

// 错误哨兵：调用方用 errors.Is 区分「通道没启动/账号没登录/配置错」与
// 「供应商拒了这一条」。所有包装一律 %w，错误文案不得带 cookies/IMEI/DSN。
var (
	// ErrZaloNotStarted Zalo 通道未启动（enable 不为 on、sessions 未配、DSN 空或库打不开）。
	ErrZaloNotStarted = errors.New("zalo 通道未启动")
	// ErrZaloNotLoggedIn 账号没有可用会话：库里无凭证，或凭证已过期需重新扫码/导入。
	ErrZaloNotLoggedIn = errors.New("zalo 账号未登录（先扫码或导入 cookie）")
	// ErrZaloBadCredentials 导入的凭证形态不合法（无 cookie、外域 cookie、IMEI/UA 缺失）。
	ErrZaloBadCredentials = errors.New("zalo 凭证不可用")
	// ErrZaloAccountBusy 账号已在线，不能同时发起扫码配对。
	ErrZaloAccountBusy = errors.New("zalo 账号已在线")
	// ErrZaloFetchTooSoon 距上次拉取不足最小间隔（好友/群组 5 分钟）。
	ErrZaloFetchTooSoon = errors.New("zalo 距上次拉取不足最小间隔")
	// ErrZaloSendLimit 单账号滚动一小时发送量已达上限。
	ErrZaloSendLimit = errors.New("zalo 单账号小时发送量已达上限")
	// ErrZaloEmptyMessageID 发送回执没有消息 id：等于没受理，不能当成功。
	ErrZaloEmptyMessageID = errors.New("zalo 发送回执没有消息 id")
	// ErrZaloBadAccountID 账号 id 不合法（空串、空白、分隔符或超长）。
	ErrZaloBadAccountID = errors.New("zalo 账号 id 非法")
)
