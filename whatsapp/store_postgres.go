package whatsapp

// 本文件是 whatsmeow 会话存储的唯一出口：把 sqlstore 的 Postgres 形态包成本仓友好的
// 门面，并把「密钥材料绝不进日志/错误文案」的纪律收在这一层。
//
// 存储归本仓（用户裁决）：会话密钥材料（NoiseKey/IdentityKey/SignedPreKey/AdvSecretKey）
// 就是账号本体凭据，等价于把那只手机交出去——它们由 sqlstore 直接落在 Postgres 的
// whatsmeow_device 等表里，部署侧应加密存储或收紧库权限；本包任何日志、error、
// SessionInfo 都不携带这些材料。
//
// 方言口径（查证）：走 database/sql + pgx v5 的 stdlib 驱动（import 后注册驱动名 "pgx"），
// 不引 lib/pq——pgx/v5 已是本仓直接依赖（db/postgres 在用），少一个驱动就少一份
// 连接语义分叉。sqlstore 侧 dbutil.ParseDialect 认 "postgres" 前缀与 "pgx"
//（查自 go.mau.fi/util@v0.10.1/dbutil/database.go:39-50），这里传 "pgx"。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 database/sql 驱动名 "pgx"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// WAStore 封装 sqlstore 的 Postgres 容器。
//
// 并发安全口径跟 container 走：*sql.DB 本身并发安全，sqlstore 的读写都在其上，
// 多 Client 共用一个 WAStore 是库的设计形态（一个 Container 管全部设备）。
type WAStore struct {
	container *sqlstore.Container
}

// 编译期钉住：*WAStore 满足管理器的设备存储 seam（假实现据此同形）。
var _ waDeviceStore = (*WAStore)(nil)

// NewWAStore 打开（并按需升级表结构）Postgres 会话库。
//
// 不需要显式调 Upgrade：sqlstore.New 内部 sql.Open 之后紧跟着 container.Upgrade(ctx)
// （查自 WM/store/sqlstore/container.go:46-59）；只有 NewWithDB 那条路才要手动升级，
// 本仓不走它——自建 *sql.DB 再包一层只会多一份要保管的句柄。
// sql.Open 本身不拨号，DSN 指不通是在 Upgrade 的首个查询上暴露的，所以这里必须
// 给一个带超时的 ctx，否则「配错 DSN」会表现成整机启动卡死。
//
// 错误文案经 maskDSN 脱敏：pgx 的拨号错误可能回显连接串，口令跟着 error 扩散
// 就会出现在日志和上层包装里。
func NewWAStore(ctx context.Context, dsn string, log waLog.Logger) (*WAStore, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, errors.New("whatsapp 会话库 DSN 为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	container, err := sqlstore.New(ctx, "pgx", dsn, log)
	if err != nil {
		return nil, fmt.Errorf("打开 whatsapp 会话库失败: %s", maskDSN(err.Error(), dsn))
	}
	return &WAStore{container: container}, nil
}

// Close 释放底层数据库连接池。
func (s *WAStore) Close() error {
	if s == nil || s.container == nil {
		return nil
	}
	return s.container.Close()
}

// GetDevice 按 JID 取一个已配对设备；不存在返回 (nil, nil)。
// 「查无」不是错误是 sqlstore 的口径（查自 WM/store/sqlstore/container.go:182-188，
// sql.ErrNoRows 被归一成 nil, nil），消费侧要把 nil 设备与查询失败分开处理。
func (s *WAStore) GetDevice(ctx context.Context, jid types.JID) (*store.Device, error) {
	return s.container.GetDevice(ctx, jid)
}

// AllDevices 返回库里全部已配对设备。
// 真实方法名是 container.GetAllDevices(ctx) ([]*store.Device, error)
// （查自 WM/store/sqlstore/container.go:157）——带 ctx、带 error，与旧文档里的
// 无参形态不同；本门面改成 AllDevices 只为与本仓命名一致，签名照抄库侧。
func (s *WAStore) AllDevices(ctx context.Context) ([]*store.Device, error) {
	return s.container.GetAllDevices(ctx)
}

// NewDevice 在内存里造一个未配对设备（ID 为 nil）。
// 不写库：配对成功时库自动调 Save 落盘（查自 WM/store/sqlstore/container.go:208-224
// 的注释「the pairing process will automatically call Save after a successful pairing」），
// QR 配对流程据此不需要手动 PutDevice。
func (s *WAStore) NewDevice() *store.Device {
	return s.container.NewDevice()
}

// DeleteDevice 从库里删除一个设备（等价于移除配对，之后必须重新扫码）。
// 参数是 *store.Device 而不是 JID——库侧真实签名
// container.DeleteDevice(ctx, *store.Device) error（查自 container.go:266），
// 它要用设备本体上的 jid 定位行；调用方先 GetDevice 再删。
func (s *WAStore) DeleteDevice(ctx context.Context, dev *store.Device) error {
	if dev == nil {
		return errors.New("whatsapp 删除设备失败: 设备为空")
	}
	return s.container.DeleteDevice(ctx, dev)
}

// dsnPasswordKV 命中 keyword/value 形态 DSN 里的 password=... 段。
var dsnPasswordKV = regexp.MustCompile(`(?i)password=[^ '\"]*`)

// dsnPasswordURL 命中 URL 形态 DSN（postgres://user:pass@host）里的口令段。
var dsnPasswordURL = regexp.MustCompile(`(://[^/@\s]+:)([^@\s]+)(@)`)

// maskDSN 把错误文案里可能出现的 DSN 与口令抹掉。
//
// 三层递进：整串替换（错误原样回显 DSN 时）、keyword/value 的 password= 值、
// URL 形态的 :pass@ 段。宁可多抹一层观测信息，也不让口令顺着 error 进日志——
// 这条纪律在 pay 侧是「secret_key 禁入 error 文案」，这里是它的 DSN 版本。
func maskDSN(msg, dsn string) string {
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "***")
	}
	msg = dsnPasswordKV.ReplaceAllString(msg, "password=***")
	msg = dsnPasswordURL.ReplaceAllString(msg, "$1***$3")
	return msg
}
