package zalo

// 本文件是 Zalo 会话库的窄 seam 与两条公共纪律：账号 id 校验、DSN 脱敏。
//
// 为什么不复用 whatsapp 的同名逻辑：maskDSN 在 whatsapp/store_postgres.go:203 是未导出
// 标识符，Go 的包边界让 zalo 无法引用；把 whatsapp 包反过来导出它会让「WhatsApp 的存储
// 实现」变成「Zalo 的公共依赖」，耦合方向错误。所以这里按同一口径重写三层脱敏
// （整串替换 / keyword 的 password= / URL 的 :pass@），两处注释互指，改一处时另一处要跟。
//
// seam 的用途：manager 与 pairing 只依赖 zaStore 接口，测试用记账假件；
// 真实现 zaPGStore 在 store_postgres.go，集成测试用一次性表名构造。
//
// 并发口径：*sql.DB 并发安全，同账号的登录由 manager 的 opMu/单飞串行化；多进程共用
// 同一库时是 last-write-wins，不提供分布式锁——约定同一账号只由一个进程管理。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// zaSessionsTable 是生产表名：与 WhatsApp 的 whatsmeow_* 表同库不同表，
// 不共享任何密钥材料。
const zaSessionsTable = "zalo_sessions"

// errZAGStoreUnopened / errZAGStoreClosed 是存储句柄的生命周期哨兵。
//
// 它们必须与「查询失败」分开：manager 关停时可能还有一次迟到的 ensureConn，
// 拿到「库已关闭」应直接拒绝而不是当成可重试的库故障。manager 用 errors.Is 判定。
var (
	errZAGStoreUnopened = errors.New("zalo 会话库未打开")
	errZAGStoreClosed   = errors.New("zalo 会话库已关闭")
)

// zaStore 是会话持久化的全部能力面。
//
// Get 的「查无」是 (nil, nil) 而不是错误：管理器的 ensureConn 要把「库里没有凭证」
// （走重新扫码/导入）与「库查询失败」（通道故障，不能假装没登录）分开处理。
// List 只读元数据：列账号时不需要把凭据拉进内存，少一次敏感数据搬运。
type zaStore interface {
	Upsert(ctx context.Context, rec zaSessionRecord) error
	Get(ctx context.Context, accountID string) (*zaSessionRecord, error) // 查无返回 (nil,nil)
	List(ctx context.Context) ([]zaSessionMeta, error)                   // 只读元数据，不读 credentials
	Delete(ctx context.Context, accountID string) error
	Close() error
}

// zaSessionRecord 是一行完整会话：Credentials 是 zcago.Credentials 的 JSON 字节，
// 由 za_credentials.go 归一化后序列化而来；本层不解释其内容。
//
// UpdatedAt 是输出字段：Get/List 返回库里的时间；Upsert 忽略它，写路径统一用 now()，
// 避免调用方时钟把「最后刷新时间」写回过去。
type zaSessionRecord struct {
	AccountID   string
	UID         string
	Credentials []byte // zcago.Credentials 的 JSON
	UpdatedAt   time.Time
}

// zaSessionMeta 是 List 的返回项：只有可公开的元数据，没有凭据。
type zaSessionMeta struct {
	AccountID string
	UID       string
	UpdatedAt time.Time
}

// zaAccountIDPattern 是账号 id 的唯一规则：字母或数字开头，随后只允许
// 字母/数字/点/下划线/连字符，总长 1-64。
//
// 这条规则同时服务三个目的：DB 主键（TEXT）、日志里的账号标识、以及将来可能把
// id 拼进文件名/路径的调用方。因为首字符必须是字母或数字，"." 与 ".." 天然被拒，
// 空白、"/"、"\"、":" 等分隔符也不在字符集内。
var zaAccountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateZAAccountID 校验账号 id，错误文案只描述规则，不回显输入全文——
// id 可能来自外部请求，把原文拼进 error 会让日志被不可信内容污染。
func validateZAAccountID(id string) error {
	if zaAccountIDPattern.MatchString(id) {
		return nil
	}
	return fmt.Errorf("%w: 需匹配 ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$（字母或数字开头，只含字母/数字/点/下划线/连字符，长度 1-64，不得为 . 或 ..）", ErrZaloBadAccountID)
}

// dsnPasswordKV 命中 keyword/value 形态 DSN 里的 password=... 段。
// 引号值（password='a b' / password="a b"）必须整段吃掉：旧的 `[^ '"]*` 会在
// 空格处停下，把后半截留进日志。
var dsnPasswordKV = regexp.MustCompile(`(?i)password=(?:'[^']*'|"[^"]*"|[^ '\"]*)`)

// dsnPasswordURL 命中 URL 形态 DSN（postgres://user:pass@host）里的口令段。
// 与 whatsapp/store_postgres.go:196 同口径。
var dsnPasswordURL = regexp.MustCompile(`(://[^/@\s]+:)([^@\s]+)(@)`)

// maskDSN 把错误文案里可能出现的 DSN 与口令抹掉。
//
// 三层递进：整串替换（错误原样回显 DSN 时）、keyword/value 的 password= 值、
// URL 形态的 :pass@ 段。宁可多抹一层观测信息，也不让口令顺着 error 进日志。
// 口径与 whatsapp/store_postgres.go:203-210 完全一致，两处必须同改。
func maskDSN(msg, dsn string) string {
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "***")
	}
	msg = dsnPasswordKV.ReplaceAllString(msg, "password=***")
	msg = dsnPasswordURL.ReplaceAllString(msg, "$1***$3")
	return msg
}

// zaTableNamePattern 是表名白名单：小写字母开头，只含小写字母/数字/下划线。
// 表名会被拼进 SQL（标识符不能用占位符），所以必须只允许这个字符集。
var zaTableNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validZATableName 判断表名是否可安全拼进 SQL。
//
// 额外限制 63 字节：PostgreSQL 标识符上限是 NAMEDATALEN-1=63，超长会被服务端
// 静默截断，导致建表与读写的实际表名不一致——那是一种「看起来成功」的错位。
func validZATableName(name string) bool {
	return len(name) <= 63 && zaTableNamePattern.MatchString(name)
}

// zaTableIdent 把已校验的表名包成 SQL 标识符。
//
// 表名白名单只保证字符集安全；包一层双引号还能挡住保留字（例如 user）导致的语法错。
// 白名单已排除双引号，所以这里不需要再转义。
func zaTableIdent(table string) string {
	return `"` + table + `"`
}
