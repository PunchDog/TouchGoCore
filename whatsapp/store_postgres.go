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

	"touchgocore/vars"
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

// waPinnedContainer 是设备行的「删除闸门」：把库侧对 *store.Device 的自动删行拦下来。
//
// 拦的是哪两条路（whatsmeow 自己动钥匙，全仓只有 Device.Delete 一个入口，查自
// WM/store/store.go:286-298 —— 它唯一的写动作就是 device.Container.DeleteDevice）：
//   - 收到 401 device_removed 流错误：WM/connectionevents.go:40-47；
//   - 收到登出型 connect failure：WM/connectionevents.go:126-132。
// 两处都紧跟着 `cli.Store.Delete(ctx)`，即「服务端一句话就把本机的配对凭据抹掉」。
//
// 为什么要拦：删行等于业务侧那个号从此无法再被认出来（重新扫码前 StartClient 必失败），
// 而吊销配对是数据完整性动作，必须由调用方显式发起。本包的契约口径是
// 「删设备行只经 WhatsappDeleteDevice 这一条路」（见 wa_delete_device.go 的编排与
// wa_client.go:279 的门面注释），库侧自删会让这条契约只对一半。
//
// 代价写在明面上：被拦下的那一刻会话确实已经死了（服务端不再认这套钥匙），
// 行留着只是留着密钥材料——密钥进不了日志/错误文案的纪律不变（见文件头），
// 清掉它的唯一时机是后台点「解绑」。
//
// PutDevice 原样转发：配对成功落库走的就是它（WM/store/sqlstore/container.go:208-224），
// 拦这条路等于不让扫码配对生效。
type waPinnedContainer struct {
	inner store.DeviceContainer
	jid   string
}

// 编译期钉住：包装仍是库要求的 DeviceContainer 两方法形态。
var _ store.DeviceContainer = waPinnedContainer{}

func (p waPinnedContainer) PutDevice(ctx context.Context, dev *store.Device) error {
	return p.inner.PutDevice(ctx, dev)
}

// DeleteDevice 拦下删行：只记日志并返回 nil。
//
// 返回 nil 而不是错误，是让库把自己那套「已删除」的内存收尾走完（置 ID=nil、
// 把各子存储换成 NoopStore，查自 WM/store/store.go:293-297）——死会话在本进程内
// 不该再写任何库表；真要恢复由调用方重新 GetDevice 拿到带 ID 的新对象。
// 返回错误反而会让库把失败往上抛进它自己的日志与重试分支。
func (p waPinnedContainer) DeleteDevice(_ context.Context, dev *store.Device) error {
	jid := p.jid
	if dev != nil && dev.ID != nil {
		jid = dev.ID.String()
	}
	vars.Warning("WhatsApp[%s] 拦下库侧的自动删行：设备行保留，账号需重新扫码配对；要连行一并吊销用 WhatsappDeleteDevice", jid)
	return nil
}

// pinWADevice 给一个从库里出来的设备套上删除闸门，幂等且对 nil 安全。
//
// 只碰 Container 字段：sqlstore 在 GetDevice/NewDevice 里把 device.Container 指回
// container 自身（查自 WM/store/sqlstore/container.go:257-263 的 initializeDevice 与
// :212-215 的 NewDevice 字面量），库后面只能经由这个字段删行，
// 所以换掉它就等价于掐断全部自删路。
// WAStore.DeleteDevice 走的是 s.container 上的方法、不看这个字段，显式吊销因此照旧生效。
func pinWADevice(dev *store.Device) *store.Device {
	if dev == nil || dev.Container == nil {
		return dev
	}
	if _, ok := dev.Container.(waPinnedContainer); ok {
		return dev
	}
	jid := ""
	if dev.ID != nil {
		jid = dev.ID.String()
	}
	dev.Container = waPinnedContainer{inner: dev.Container, jid: jid}
	return dev
}

// GetDevice 按 JID 取一个已配对设备；不存在返回 (nil, nil)。
// 「查无」不是错误是 sqlstore 的口径（查自 WM/store/sqlstore/container.go:182-188，
// sql.ErrNoRows 被归一成 nil, nil），消费侧要把 nil 设备与查询失败分开处理。
// 返回的设备已套删除闸门（waPinnedContainer）。
func (s *WAStore) GetDevice(ctx context.Context, jid types.JID) (*store.Device, error) {
	dev, err := s.container.GetDevice(ctx, jid)
	return pinWADevice(dev), err
}

// AllDevices 返回库里全部已配对设备。
// 真实方法名是 container.GetAllDevices(ctx) ([]*store.Device, error)
// （查自 WM/store/sqlstore/container.go:157）——带 ctx、带 error，与旧文档里的
// 无参形态不同；本门面改成 AllDevices 只为与本仓命名一致，签名照抄库侧。
// 每个设备都已套删除闸门。
func (s *WAStore) AllDevices(ctx context.Context) ([]*store.Device, error) {
	devs, err := s.container.GetAllDevices(ctx)
	for _, dev := range devs {
		pinWADevice(dev)
	}
	return devs, err
}

// NewDevice 在内存里造一个未配对设备（ID 为 nil）。
// 不写库：配对成功时库自动调 Save 落盘（查自 WM/store/sqlstore/container.go:208-224
// 的注释「the pairing process will automatically call Save after a successful pairing」），
// QR 配对流程据此不需要手动 PutDevice。
//
// 同样套删除闸门：库在配对失败的两条分支上也会调 Store.Delete
//（查自 WM/pair.go:234 与 :262），拦下来与上面同一套理由。
func (s *WAStore) NewDevice() *store.Device {
	return pinWADevice(s.container.NewDevice())
}

// DeleteDevice 从库里删除一个设备（等价于移除配对，之后必须重新扫码）。
// 参数是 *store.Device 而不是 JID——库侧真实签名
// container.DeleteDevice(ctx, *store.Device) error（查自 container.go:266），
// 它要用设备本体上的 jid 定位行；调用方先 GetDevice 再删。
//
// 这是本包唯一真正删行的出口：它直接打在 container 上，不经 waPinnedContainer，
// 所以设备对象带着闸门进来也照删（对照 wa_delete_device.go ⑤ 的重读守卫）。
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
