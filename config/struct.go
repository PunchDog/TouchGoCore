package config

import (
	"errors"
	"fmt"
	"strings"
)

/*
数据库配置结构体
*/
type MySqlDBConfig struct {
	Host         string `json:"db_host"`           //连接地址
	Username     string `json:"db_username"`       //用户名
	Password     string `json:"db_password"`       //用户密码
	DBName       string `json:"db_name"`           //数据库名
	MaxIdleConns int    `json:"db_max_idle_conns"` //连接池最大空闲连接数
	MaxOpenConns int    `json:"db_max_open_conns"` //连接池最大连接数
	Loc          string `json:"db_loc"`            //连接时区（如 "UTC"、"Local"、"+08:00"）；为空时沿用默认 loc=Local
}

// PostgreDBConfig 是 PostgreSQL 连接配置。与 MySqlDBConfig 同构，差异在于
// PG 用 host+port 而非 host:port 拼地址，且带 sslmode/时区两个 PG 特有维度。
// <=0 的池字段与 ConnMaxLifetime 在装配时回落 db/postgres 框架默认值。
type PostgreDBConfig struct {
	Host            string `json:"pg_host"`              //连接地址（不含端口）
	Port            int    `json:"pg_port"`              //端口，<=0 时用默认 5432
	Username        string `json:"pg_username"`          //用户名
	Password        string `json:"pg_password"`          //用户密码
	DBName          string `json:"pg_dbname"`            //数据库名
	SSLMode         string `json:"pg_sslmode"`           //disable/require/verify-ca/verify-full，为空时默认 disable
	TimeZone        string `json:"pg_timezone"`          //连接时区，为空时默认 UTC
	MaxIdleConns    int    `json:"pg_max_idle_conns"`    //连接池最大空闲连接数
	MaxOpenConns    int    `json:"pg_max_open_conns"`    //连接池最大连接数
	ConnMaxLifetime int    `json:"pg_conn_max_lifetime"` //连接最大存活秒数，<=0 时用默认 24h
	Schema          string `json:"pg_schema"`            //PG schema 名，为空时默认 public
}

type MongoTableIndex struct {
	TableName string   `json:"table"` //数据表名
	Index     []string `json:"index"` //哪些关键字设置查询索引
}
type MongoDBConfig struct {
	Host             string             `json:"db_host"`             //连接地址
	Username         string             `json:"db_username"`         //用户名
	Password         string             `json:"db_password"`         //用户密码
	DBName           string             `json:"db_name"`             //数据库名
	MongoUpUrl       string             `json:"mongo_up_url"`        //连接格式化信息
	MongoUrl         string             `json:"mongo_url"`           //连接格式化信息
	ReplicaSetName   string             `json:"db_replica_set_name"` //集群名（设置集群模式需要）
	InitDBTableIndex []*MongoTableIndex `json:"init_dbtable_index"`  //初始化时创建查询索引
}

type RedisConfig struct {
	Host     string `json:"redis_host"`      //连接地址
	Password string `json:"redis_password"`  //用户密码
	Db       int    `json:"redis_db"`        //库编号
	PoolSize int    `json:"redis_pool_size"` // 连接池大小；<=0 时用默认 512
}

// CacheConfig 两级缓存（Redis 一级 + MySQL/Mongo 回源）框架级配置。
// nil = 不启用缓存层。时长统一 _ms 整数约定（JSON 不反序列化时长串）；
// <=0 的字段在装配时回落 db/cache 框架默认值。
type CacheConfig struct {
	Enabled   bool   `json:"enabled"`    // 总开关；false 时 Write 降级 write-through
	KeyPrefix string `json:"key_prefix"` // Redis 键前缀，默认 "tg"

	TTLMS         int `json:"ttl_ms"`          // 物理 TTL 毫秒，默认 300000（5min）
	LogicalPct    int `json:"logical_pct"`     // 逻辑过期占物理 TTL 百分比（1..99），默认 80
	JitterPct     int `json:"jitter_pct"`      // TTL 抖动百分比（0..50），默认 10
	NegativeTTLMS int `json:"negative_ttl_ms"` // 空值缓存毫秒，默认 30000；负值显式关闭空值缓存

	ReadTimeoutMS  int `json:"read_timeout_ms"`  // 回源超时，默认 300
	WriteTimeoutMS int `json:"write_timeout_ms"` // 写 Redis 超时，默认 300
	FailBackoffMS  int `json:"fail_backoff_ms"`  // 回源失败退避，默认 2000

	FlushIntervalMS int `json:"flush_interval_ms"` // 写线定时器间隔，默认 2000
	BatchSize       int `json:"batch_size"`        // 批量落库上限，默认 200
	MaxDirtyKeys    int `json:"max_dirty_keys"`    // 写缓冲脏键上限，默认 100000
	MaxDirtyAgeMS   int `json:"max_dirty_age_ms"`  // 脏键超龄告警阈值，默认 300000
	Shards          int `json:"shards"`            // 写缓冲分片数（2 的幂），默认 16
	MaxRetry        int `json:"max_retry"`         // 落库最大重试轮数，默认 3
	SaveConcurrency int `json:"save_concurrency"`  // 单条落库并发度，默认 4

	// StalePolicy "async"(默认，逻辑过期先返旧值+异步预热) / "wait"(同步回源)
	StalePolicy string `json:"stale_policy"`
	// Overflow "block"(默认，缓冲超限内联 flush 背压) / "drop"(仅告警)
	Overflow string `json:"overflow"`
	// RequireRedis 指针区分「未配置(默认 true)」与「显式 false 降级直返源值」
	RequireRedis *bool `json:"require_redis"`
	// Journal 脏账本开关：同步写 Redis 时把「待落库」记入 Redis ZSET，
	// 重启扫账恢复，防非优雅退出丢写缓冲。指针区分未配置(默认 true)与显式 false。
	// 仅当一级缓存支持账本（内置 Redis store）才生效。
	Journal *bool `json:"journal"`

	// LockTTLMS 分布式锁（SETNX）持有时长毫秒，默认 5000；<=0 禁用跨进程锁。
	LockTTLMS int `json:"lock_ttl_ms"`
	// LockWaitMS 未抢到分布式锁的最大等待毫秒，默认 2000；超时返回 ErrLockTimeout。
	LockWaitMS int `json:"lock_wait_ms"`
	// LockPollIntervalMS 未抢到锁时的轮询间隔毫秒，默认 20。
	LockPollIntervalMS int `json:"lock_poll_interval_ms"`
}

// TLSConfig 通用服务端 TLS（Gin / WebSocket 直连场景；前置反代可保持 enable=false）
type TLSConfig struct {
	Enable   bool   `json:"enable"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type WebConfig struct {
	HTTPPort     int        `json:"httpport"`      //监听端口
	Static       *string    `json:"static"`        //文件服
	AllowOrigins []string   `json:"allow_origins"` // CORS 允许的来源，空则不允许跨域（生产默认）
	TLS          *TLSConfig `json:"tls"`
}

type WebsocketPort struct {
	//连接的端口
	Port int `json:"port"`
	//端口对应的回调类名
	CallbackClassName string `json:"callbackclassname"`
}
type WebsocketConfig struct {
	//监听配置,多个端口用|分割
	Port []*WebsocketPort `json:"port"`
	//外网地址
	URL string `json:"url"`
	//内网地址
	InURL string `json:"inurl"`
	// 允许的 Origin 白名单；启用 CheckOrigin 时为空则拒绝。显式 "*" 表示允许所有
	AllowedOrigins []string `json:"allowed_origins"`
	// 是否启用 Origin 验证（建议生产环境为 true）
	CheckOrigin bool `json:"check_origin"`
	// 内网连接在「请求不带 Origin」时直接放行（原生客户端/服务间调用）；
	// 只要对端报了 Origin 就照常比对白名单，内网不是跨站请求的豁免区
	SkipOriginForIntranet bool `json:"skip_origin_for_intranet"`
	// Origin 头缺失时是否放行。默认 false：原生客户端确需握手时再显式打开
	AllowEmptyOrigin bool `json:"allow_empty_origin"`
	// 握手期读写缓冲（字节），<=0 时用框架默认 4KiB。这是 TCP 暂存缓冲，不是消息上限
	UpgraderReadBuffer  int `json:"upgrader_read_buffer"`
	UpgraderWriteBuffer int `json:"upgrader_write_buffer"`
	// 单条消息大小上限（字节），<=0 时回落到 server.max_msg_size
	MaxMessageSize int `json:"max_message_size"`
	// 心跳与超时（毫秒），<=0 时用框架默认值
	PingIntervalMS int `json:"ping_interval_ms"` // 服务端发 ping 的间隔，默认 30000
	ReadTimeoutMS  int `json:"read_timeout_ms"`  // 读超时（应 ≥ 2×ping），默认 90000
	WriteTimeoutMS int `json:"write_timeout_ms"` // 单次写超时，默认 5000
	// 信任的反向代理，仅这些 RemoteAddr 才读取 X-Forwarded-For / X-Real-IP
	TrustedProxies []string `json:"trusted_proxies"`
	// Worker Pool 配置
	WorkerPoolSize int  `json:"worker_pool_size"` // Worker 数量，0 表示串行模式（默认），>0 启用并行
	ShardByKey     bool `json:"shard_by_key"`     // 是否按 UID 分片（保证同 UID 消息顺序性）
	// 认证配置
	AuthTokenHeader  string     `json:"auth_token_header"`  // 认证 Token 的 HTTP 头名称（默认 "X-Auth-Token"），为空则不验证
	AuthTokenQuery   string     `json:"auth_token_query"`   // 认证 Token 的 URL 查询参数名（默认 "token"），为空则不从 query 读取
	AuthIntranetSkip bool       `json:"auth_intranet_skip"` // 内网连接是否跳过认证
	TLS              *TLSConfig `json:"tls"`
}

type RpcAddr struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Port   int    `json:"port"`
	UseTLS bool   `json:"use_tls"` // 是否使用 TLS，为 false 时表示内网连接跳过 TLS
	// GinPath 仅客户端侧使用：声明本进程可通过反向 gRPC 代理到网关 gin 的 HTTP 路由。
	// 每项形如 "urlpath|METHOD"（METHOD 省略=通配所有方法），urlpath 可含 ":name" 参数段。
	// 连接建立后客户端据此向网关注册路由；服务端条目忽略该项。
	GinPath []string `json:"ginpath"`
}

type RpcTLSConfig struct {
	CertFile        string `json:"cert_file"`         // 证书文件路径
	KeyFile         string `json:"key_file"`          // 私钥文件路径
	Enable          bool   `json:"enable"`            // 是否启用 TLS
	SkipForIntranet bool   `json:"skip_for_intranet"` // 内网连接是否跳过 TLS
}

// RpcAuthConfig gRPC 鉴权。mode: none / allowlist / token / mtls
type RpcAuthConfig struct {
	Mode           string   `json:"mode"`
	AllowList      []string `json:"allowlist"`
	Token          string   `json:"token"`
	CAFile         string   `json:"ca_file"`
	ClientCertFile string   `json:"client_cert_file"`
	ClientKeyFile  string   `json:"client_key_file"`
}

type RpcConfig struct {
	//监听配置,多个端口用|分割
	Server []*RpcAddr `json:"server"`
	//客户端配置
	Client []*RpcAddr `json:"client"`
	// TLS 配置
	TLS *RpcTLSConfig `json:"tls"`
	// 鉴权，缺省为 token（配置缺失即拒绝所有连接）；显式 mode=none 才放行
	Auth *RpcAuthConfig `json:"auth"`
}

// telegram配置
type TelegramConfig struct {
	BotToken        string            `json:"bot_token"`
	GameUrl         string            `json:"game_url"`
	GameBannerUrl   string            `json:"game_banner_url"`
	GameDescription string            `json:"game_description"`
	GameToShort     map[string]string `json:"game_to_short"`
	// Ton 是 TON 资金链路。它与 Bot 长连接共用一段配置但互不牵制：
	// Bot 起不起来与钱包通道配没配好是两件事，各自的开关各自判（见 TonChannelConfig）。
	Ton *TonChannelConfig `json:"ton"`
}

// TonChannelConfig 是 telegram.ton 段：这条 TON 链路引用哪个 SDK 段、面向哪张网。
//
// 凭证、接入点、热钱包地址与私钥都不在这里（那是 pay_sdks.<段>.chain 的事实，
// 同一个接入点服务多条链时不该复制两份）；这里只描述「这个 Bot 发的钱走哪条路」。
type TonChannelConfig struct {
	// Provider 是对 pay_sdks 表的引用。留空即本通道不启用（口径与 usdt.provider 一致：
	// SDK 段的 enable 决定凭证能不能用，这里的 sdk 决定这条通道用不用它，两处都开才是开）。
	Provider *PaySDKRef `json:"provider"`
	// Network 是公链网络标识，取值 mainnet / testnet；留空按主网。
	// 它同时决定链上驱动连的是哪个接入点默认基址、以及接受哪种带测试网位的地址。
	Network string `json:"network"`
	// Jetton 是 jetton 合约（minter）地址，本仓当前**不使用**：出款只发原生 TON。
	// 填了不会静默忽略——装配期直接拒绝并说明原因，因为「配了 jetton、实际发的是原生币」
	// 是能把钱发错形状的一类错（见 telegram/ton_onchain.go 与 config/validate.go）。
	Jetton string `json:"jetton"`
}

// PaySDKConfig 是一段资金 SDK 配置：一套供应商接入的全部事实——驱动标记、
// 基址、凭证、端点表，以及我方在该供应商名下的商户账户表。
//
// 集中登记的用意：同一个供应商往往同时供着提现、查单、验证码下发几条链路，
// 凭证与端点抄几份之后必然改漏一处，出款那侧还留着旧密钥。SDK 段写一次，
// 各通道只留「用哪个 SDK、用哪个商户账户」两个名字。
//
// 配置结构体留在 config 包：pay 的核心文件（channel/client/provider/types）只认
// 与配置无关的 ProviderOptions（端点表也是以取值函数的形式交进去），只有装配层
// pay/assemble.go 按值引用这里的结构体——config 不反向引 pay，这个方向不成环。
// 各通道包（whatsapp/zalo/facebook/instagram/usdt/telegram）负责把本结构逐字段转成
// pay.ProviderOptions。
//
// Enable 必须显式为 "on" 才启动。出款接口的新配置段默认不启动，
// 比「写了 base_url 就以为开了」更安全：漏一个开关的代价是少一条通道，
// 多开一条通道的代价是真金出款。
type PaySDKConfig struct {
	Enable string `json:"enable"` // "on" 启动；空或 "off" 时引用它的通道一律不启动
	// Driver 是驱动标记，取值是 pay.Register 登记过的驱动名（内置为 pay.DriverGeneric）。
	// 换一家供应商=换这一个名字加一份凭证，通道代码不动。
	Driver  string `json:"driver"`
	BaseURL string `json:"base_url"` // 供应商接口基址，不含末尾斜杠
	AppID   string `json:"app_id"`   // 应用标识，参与签名域
	// SecretKey 是签名密钥。只允许经配置或环境变量注入，禁止写入日志、
	// error 文案与任何对外摘要字段。
	SecretKey   string `json:"secret_key"`
	AuthHeader  string `json:"auth_header"`  // 签名所在请求头名，如 X-Sign；留空按驱动默认
	TokenHeader string `json:"token_header"` // 登录态 token 所在请求头名，如 Authorization
	TimeoutSec  int    `json:"timeout_sec"`  // 单次请求超时（秒），<=0 用 pay.DefaultTimeout
	MaxRetries  int    `json:"max_retries"`  // 可重试错误的重试次数，<0 用 pay.DefaultMaxRetry
	// NotifyURL 只进 HTTP 供应商的下单报文（pay.Open 那条路径）；本仓 usdt 走链上、
	// 三条登录链路也不读它。
	NotifyURL string `json:"notify_url"` // 本侧回执接收地址，下单时随报文交出去
	// Endpoints 是「接口文档到位后只改这里」的注入点之一：
	// 键是本仓代码里固定的逻辑名（account/query/withdraw/login/send_code/friend_list），
	// 值是供应商的实际路径。缺某个键 ⇒ 对应操作返回明确错误，不发请求。
	// withdraw 只对纯 HTTP 供应商资金通道有效：本仓 usdt 的出款走链上驱动，示例段不登记它。
	Endpoints map[string]string `json:"endpoints"`
	// Accounts 是我方在该供应商名下的商户账户表，键是本地别名（default/reserve…），
	// 通道段用别名引用。商户号是路由信息不是凭证，可以进日志。
	Accounts map[string]*PayMerchantAccount `json:"accounts"`
	// Chain 是「直接动公链」那一类驱动的接入点与凭证，nil 即这一段只是 HTTP 供应商配置。
	//
	// 它挂在 SDK 段而不是通道段：节点、API Key、热钱包是「我用哪个接入点、以谁的身份出资」，
	// 与这一单发的是 USDT 还是 TRX 无关；同一接入点服务同链多币种时不该复制两份。
	// 合约地址与网络不在这里（那是通道段的 usdt.contract / usdt.network，权威值只有一处）。
	Chain *PayChainConfig `json:"chain"`
}

// PayChainConfig 是链上接入点配置（pay_sdks.<段>.chain）。
//
// 单位与币种名不进配置：fee_limit 明确写死是「该链原生币的最小单位」（TRON 为 sun），
// 因为链上 gas 收的是原生币而不是本单币种（见 pay.AccountInfo.CanWithdraw 的注释），
// 把它写成不带单位的 fee_limit 就会有人填 65 而不是 65000000。
type PayChainConfig struct {
	// Node 是链节点接入点（TRON 为 gRPC 的 host:port，如 grpc.trongrid.io:50051）。
	// 出款广播只走它；以 gRPC 节点广播的驱动（tron_trongrid）必填它。
	// 走 HTTP 接入点的驱动（ton_tonapi）不读这一项，它必填的是 http_base。
	Node string `json:"node"`
	// TLS 是接入点的传输安全开关："off" 才走明文，其余（含留空）一律按 TLS 建连。
	//
	// 反向缺省是刻意的：公共接入点必须 TLS，把「明文」写成需要显式声明的例外，
	// 才不会有人照抄一份自建节点的配置就把热钱包私钥签名链路挂到了明文中转上。
	TLS string `json:"tls"`
	// HTTPBase 是同链的 REST 基址（TRON 如 https://api.trongrid.io，TON 如 https://tonapi.io）。
	// TRON 侧只用于读侧兜底（查单、余额、近期转出反查），广播仍只走 node——多一条会动钱的
	// 未验证路径，出事时连该信哪条都不知道。
	// TON 侧它就是广播与查证的唯一入口（本仓不接 lite server，见 telegram/ton_gateway_tonapi.go），
	// 留空由驱动按 network 取该链接入点的公开基址。
	HTTPBase string `json:"http_base"`
	// APIKey 是接入点要求的访问凭证（TRON 侧为 TRON-PRO-API-KEY）。
	// 它不是资金密钥，但同样禁止写入日志、error 文案与摘要字段。
	APIKey string `json:"api_key"`
	// WalletAddress 是我方出款账户的链上地址。它是路由信息不是凭证，可以进日志，
	// 且必须与私钥派生出的地址逐字相等，否则「配了 A 的地址、实际用 B 的私钥出款」
	// 这种错要到对账时才看得见。
	WalletAddress string `json:"wallet_address"`
	// WalletVersion 是热钱包合约的版本名，只有需要它的链读它（TON 为 v5R1/v4R2/v3R2/v4R1/v3R1，
	// TRON 不填也一样能出款）。留空由驱动探测；探测不出来就拒装配，不猜第二个候选。
	WalletVersion string `json:"wallet_version"`
	// SubWalletID 是钱包合约的子账户号（TON 的 v3/v4 系用它区分同一把公钥下的不同账户）。
	// 指针：0 是合法取值，用 0 表达「没配」会把显式配 0 的人推到默认值上，那派生出来的是
	// 另一个账户——本包宁可让「没配」与「配 0」在结构体里就是两种形状。
	SubWalletID *uint32 `json:"sub_wallet_id"`
	// PrivateKey 是热钱包私钥（hex）。配置里恒为空，由下游在启动前经
	// util.CallPayChainMsg+段名 钩子注入；它不得进入日志、error 文案与任何摘要字段。
	PrivateKey string `json:"private_key"`
	// FeeLimit 是允许为本单烧掉的手续费上限，单位是该链原生币的最小单位（TRON 为 sun、TON 为纳吨）。
	// <=0 由驱动按该链缺省值定，配置不必知道缺省是多少。
	FeeLimit int64 `json:"fee_limit"`
	// TimeoutSec 是单次链上操作的超时（秒），<=0 由驱动用缺省值。
	//
	// 它不复用上层的 timeout_sec：那一个是「供应商 HTTP 接口响应要多久」，这一个管的是
	// 「链节点取一份 raw transaction / 广播 / 查回执要多久」，两者量级与失败形态都不同，
	// 共用一格会让调过 HTTP 超时的人顺手改坏链上超时。
	TimeoutSec int `json:"timeout_sec"`
	// MaxRetries 是链上读操作（查单、余额、近期转出反查）的重试次数，0 即不重试。
	//
	// 写侧永远不看它：广播不是「失败了再来一次」的操作，重试由链上驱动的台账判定决定
	// （见 pay.ChainOptions.MaxRetry 的注释）。
	MaxRetries int `json:"max_retries"`
}

// PayMerchantAccount 是「我方在供应商侧的一个商户账户」的配置。
//
// 余额、可用额度、授信、费率都是供应商侧的事实，由 pay 的账户查询接口读回来，
// 不写在这里：把读来的值抄进配置，就等于给对账留了一份会过期的假账。
type PayMerchantAccount struct {
	// Enable 必须显式为 "on"。停用某个商户号（怀疑泄露、额度冻结）时改这一处，
	// 不必删整段 SDK 配置、也不影响同供应商的其它通道。
	Enable string `json:"enable"`
	// MerchantID 是我方在供应商侧的商户号，进报文并参与签名域。
	MerchantID string `json:"merchant_id"`
}

// Enabled 该商户账户是否可用（口径与 SDK 段一致：显式 "on" 才算开）。
func (a *PayMerchantAccount) Enabled() bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.Enable), "on")
}

// PaySDKRef 是通道段对 pay_sdks 表的引用：只留两个名字，不留任何凭证。
//
// 通道段（usdt.provider / facebook.login / instagram.login）
// 各自的开关就是这里填不填名字：留空即该通道不启用。之所以不再单独放一个
// enable，是为了让「开不开」只有一处真相——SDK 段的 enable 决定凭证能不能用，
// 这里的 sdk 决定这条通道用不用它；两处都开才是开。
// Zalo 已改由 zcago 协议直连，不再引用这张表。
type PaySDKRef struct {
	SDK string `json:"sdk"` // pay_sdks 的键名
	// Account 是 SDK 段 accounts 里的键名。该 SDK 只有一个账户时留空即取那一个；
	// 多于一个账户时必须点名，否则「这一单从哪个商户号出」就成了初始化顺序的函数。
	Account string `json:"account"`
}

// Empty 该通道是否未绑定任何 SDK（未绑定即不启用）。
func (r *PaySDKRef) Empty() bool { return r == nil || strings.TrimSpace(r.SDK) == "" }

// Resolve 在全表里取被引用的 SDK 段与商户账户。资金通道（提现）用它：
// 出款必须有一个明确的商户主体，取不到账户就是配置错。
//
// 报错文案只说名字与缺项，绝不带出基址之外的敏感值；SDK 段里可能的密钥
// 一旦顺着 error 扩散，就会出现在日志和上层包装里。
func (r *PaySDKRef) Resolve(sdks map[string]*PaySDKConfig) (*PaySDKConfig, *PayMerchantAccount, error) {
	return r.resolve(sdks, true)
}

// ResolveService 取「只发消息、不动资金」的链路（验证码下发、登录换会话）。
//
// 这类 SDK 本就没有商户账户，要求它配一个只会逼人填个假商户号；但一旦配了账户，
// 歧义规则与资金链路完全一样：多个账户就必须点名。
func (r *PaySDKRef) ResolveService(sdks map[string]*PaySDKConfig) (*PaySDKConfig, *PayMerchantAccount, error) {
	return r.resolve(sdks, false)
}

func (r *PaySDKRef) resolve(sdks map[string]*PaySDKConfig, needAccount bool) (*PaySDKConfig, *PayMerchantAccount, error) {
	if r.Empty() {
		return nil, nil, errors.New("未配置 sdk 引用，该通道不启用")
	}
	name := strings.TrimSpace(r.SDK)
	cfg := sdks[name]
	if cfg == nil {
		return nil, nil, fmt.Errorf("pay_sdks 里没有名为 %s 的 SDK 段", name)
	}
	if !cfg.Enabled() {
		return nil, nil, fmt.Errorf("pay_sdks.%s 未显式开启（enable 需为 on）", name)
	}
	if !needAccount && len(cfg.Accounts) == 0 {
		return cfg, nil, nil
	}
	acc, err := cfg.Account(strings.TrimSpace(r.Account))
	if err != nil {
		return nil, nil, fmt.Errorf("pay_sdks.%s: %w", name, err)
	}
	return cfg, acc, nil
}

// Account 按别名取商户账户。别名留空且表中只有一个账户时取那一个；
// 多于一个则报错要求点名——静默挑一个商户号出钱是不可接受的歧义。
func (p *PaySDKConfig) Account(alias string) (*PayMerchantAccount, error) {
	if p == nil || len(p.Accounts) == 0 {
		return nil, errors.New("未配置 accounts 商户账户")
	}
	if alias == "" {
		if len(p.Accounts) > 1 {
			return nil, fmt.Errorf("accounts 里有 %d 个商户账户，通道段必须用 account 点名", len(p.Accounts))
		}
		for k := range p.Accounts {
			alias = k
		}
	}
	a, ok := p.Accounts[alias]
	if !ok {
		return nil, fmt.Errorf("accounts 里没有名为 %s 的商户账户", alias)
	}
	if !a.Enabled() {
		return nil, fmt.Errorf("accounts.%s 未显式开启（enable 需为 on）", alias)
	}
	if strings.TrimSpace(a.MerchantID) == "" {
		return nil, fmt.Errorf("accounts.%s 缺 merchant_id", alias)
	}
	return a, nil
}

// Enabled 该 SDK 段是否被显式开启（enable 大小写不敏感的 "on"）。
// nil 接收者可安全调用，三通道统一用它判「启不启动」，避免各写一份口径。
func (p *PaySDKConfig) Enabled() bool {
	return p != nil && strings.EqualFold(strings.TrimSpace(p.Enable), "on")
}

// Endpoint 按逻辑名取供应商路径。未登记时 ok=false——配置不全的操作
// 必须在发请求之前就拒掉，不能让会员看到一个点了必失败的入口。
func (p *PaySDKConfig) Endpoint(name string) (string, bool) {
	if p == nil {
		return "", false
	}
	v := strings.TrimSpace(p.Endpoints[name])
	if v == "" {
		return "", false
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	return v, true
}

// WhatsappConfig 是 WhatsApp 通道配置。
//
// 与其它三条社交通道不同，这里**没有 login/templates**：消息侧（QR 配对、发信、
// 删会话）已改由 go.mau.fi/whatsmeow 直连 WhatsApp 多设备协议，不再经 HTTP 消息网关，
// 会话本体（含 NoiseKey/IdentityKey 等密钥材料）持久化在本仓的 Postgres 会话库里。
// 资金侧（提现门面与充值接口）已随 2026-10 的资金面收口删除，本包只剩消息侧，所以只有 sessions 一段。
type WhatsappConfig struct {
	// Enable 是通道总闸：显式为 "on" 才装配本通道，闸的是 whatsmeow 消息侧（资金链路已删除，无资金侧可闸）。
	// 整段不写、键不写、写成 off 或任何别的值都读作「不开」——口径与 pay_sdks/nft_sdks
	// 段一致：默认不启动。whatsmeow 是非官方协议客户端，「没配即不连 WhatsApp 服务器」
	// 必须是缺省，而不是靠「记得别填 dsn」。
	Enable string `json:"enable"`
	// Sessions 是 whatsmeow 消息侧的会话库与连接参数。Enable 为 on 但整段留空
	//（或 dsn 为空）即消息侧不装配——本包已没有「只跑钱、不跑协议」这种形态可配。
	Sessions *WhatsappSessionsConfig `json:"sessions"`
}

// Enabled 该通道是否被显式开启（口径与 PaySDKConfig.Enabled 一致：enable 大小写
// 不敏感的 "on"，nil 接收者可安全调用）。段缺失即未开，调用侧不必先判 nil。
func (c *WhatsappConfig) Enabled() bool {
	return c != nil && strings.EqualFold(strings.TrimSpace(c.Enable), "on")
}

// WhatsappSessionsConfig 是 whatsmeow 消息侧的运行参数。
//
// 这里只放 DSN、不放任何凭证字段：账号密钥材料（NoiseKey/IdentityKey/SignedPreKey/
// AdvSecretKey）由 whatsmeow 的 sqlstore 直接管在数据库表里，配进启动配置等于把账号
// 本体抄进版本库。代价是 DSN 本身成了敏感值（它含数据库口令）：只经部署配置注入，
// 禁入日志与 error 文案（whatsapp 包侧对含 DSN 的错误做脱敏）。
// 配置约定：JSON 不反序列化 time.Duration，时长一律用秒，由 Effective* 折算。
type WhatsappSessionsConfig struct {
	// DSN 是 Postgres 连接串（whatsmeow sqlstore 经 database/sql 访问，
	// URL 形态 postgres://user:pass@host:5432/db 与 keyword/value 形态都收）。
	// 留空即消息侧不启动。
	DSN string `json:"dsn"`
	// MaxConcurrent 是在跑 Client 数的软上限，<=0 不限制（用户裁决：来一个配一个，
	// 不设硬上限；填了正数才生效，用于给内存/连接数增长设一道闸）。
	MaxConcurrent int `json:"max_concurrent"`
	// ConnectTimeoutSec 是一次连接（含握手）的超时，<=0 折算成 15 秒。
	ConnectTimeoutSec int `json:"connect_timeout_sec"`
	// ReconnectMinSec 是断线重连的退避下界，<=0 折算成 3 秒。
	ReconnectMinSec int `json:"reconnect_min_sec"`
	// ReconnectMaxSec 是退避上界，<=0 折算成 60 秒；下界大于上界时按上界取。
	ReconnectMaxSec int `json:"reconnect_max_sec"`
	// SendMinDelaySec 是异步批量发信两条消息之间的最小间隔，<=0 折算成 1 秒。
	SendMinDelaySec int `json:"send_min_delay_sec"`
	// SendMaxDelaySec 是批内消息间隔的上界（每条在 [min,max] 内随机取值），
	// <=0 折算成 5 秒；下界大于上界时按上界取。
	SendMaxDelaySec int `json:"send_max_delay_sec"`
	// SendMaxPerHour 是单账号滚动一小时内的发送闸（异步批量发信路径记账），
	// <=0 折算成 30 条。非官方客户端发得越密越容易被风控，这条是封号风险的软闸。
	SendMaxPerHour int `json:"send_max_per_hour"`
	// AntibanEnabled 给该通道的发送路径挂 whatsmeow-antiban 防封闸（11 道独立
	// 风控门的中间件）。指针区分「未配置(默认开)」与「显式 false 关」：开了之后
	// 同账号发送串成单行道，且闸可能直接拦截某条发送（错误串含「防封闸拦截」），
	// 不想要这个结果形态就显式配 false；整段 sessions 缺失读作关（消息侧本来不启动）。
	AntibanEnabled *bool `json:"antiban_enabled"`
	// AntibanPreset 是防封闸档位：relaxed / moderate / conservative / aggressive /
	// high-volume 等，留空或写错名一律折算成 moderate（库 Resolve 的兜底口径）。
	AntibanPreset string `json:"antiban_preset"`
	// AntibanStateDir 是防封闸的持久化状态目录（warm-up 进度、已知会话集等，
	// 每账号一个文件）。留空折算成相对工作目录的 antiban_state；重启丢档等于
	// 新配对的 warm-up 进度归零重来，生产要指到随部署卷持久化的路径。
	AntibanStateDir string `json:"antiban_state_dir"`
}

// ZaloConfig 是 Zalo 通道配置。
//
// 与 Facebook/Instagram 的供应商网关形态不同：Zalo 消息侧已改由 github.com/amrakk/zcago
// 直连 Zalo 个人号协议（非官方客户端），会话凭证（cookies/IMEI/UA）持久化在 Postgres 的
// zalo_sessions 表里，与 WhatsApp 一样按会话库管理。资金链路已在 2026-10 收口里删除，
// 本通道只剩消息/登录面，所以只有 enable + sessions 两段。
type ZaloConfig struct {
	// Enable 是通道总闸：显式为 "on" 才装配。zcago 是非官方协议客户端，
	// 缺省（整段不写、键不写、写任何非 on 值）都读作「不连 Zalo」。
	Enable string `json:"enable"`
	// Sessions 是 zcago 会话库与消息侧参数。Enable 为 on 但整段留空即不装配。
	Sessions *ZaloSessionsConfig `json:"sessions"`
	// Deprecated: Login 是旧 HTTP 供应商登录链路的 SDK 引用，已随 zcago 直连替换。
	// 保留字段只为让旧配置在 Validate 阶段得到明确报错，而不是被 JSON 静默丢弃后
	// 通道悄悄不工作。
	Login *PaySDKRef `json:"login,omitempty"`
	// Deprecated: Templates 是旧验证码模板名表，已废弃。保留理由同 Login。
	Templates map[string]string `json:"templates,omitempty"`
}

// Enabled 该通道是否被显式开启（口径与 WhatsappConfig/PaySDKConfig 一致：enable
// 大小写不敏感的 "on"，nil 接收者可安全调用）。段缺失即未开，调用侧不必先判 nil。
func (c *ZaloConfig) Enabled() bool {
	return c != nil && strings.EqualFold(strings.TrimSpace(c.Enable), "on")
}

// ZaloSessionsConfig 是 zcago 会话库与消息侧运行参数。
//
// DSN 指向 Postgres 的 zalo_sessions 表；留空时复用 whatsapp.sessions.dsn（两者共用
// 同一个 Postgres 实例，表各自独立）。DSN 本身含数据库口令，只经部署配置注入，
// 禁入日志与 error 文案（zalo 包侧对含 DSN 的错误做脱敏）。
// 配置约定：JSON 不反序列化 time.Duration，时长一律用秒/毫秒，由 Effective* 折算。
type ZaloSessionsConfig struct {
	// DSN 是 Postgres 连接串（database/sql + pgx stdlib，URL 与 keyword/value 形态都收）。
	// 留空则复用 whatsapp.sessions.dsn；两者都空时不装配 Zalo 消息侧。
	DSN string `json:"dsn"`
	// QRTTLSec 是一次扫码会话的存活上限（秒），<=0 折算 300 秒；低于 120 秒按 120 取
	// （zcago 单张二维码约 100 秒过期，至少要允许刷新一次）。
	QRTTLSec int `json:"qr_ttl_sec"`
	// RequestTimeoutSec 是单次登录/发送/拉取请求的超时（秒），<=0 折算 30 秒。
	// 只经 ctx 生效，不设 http.Client.Timeout——listener 是长连接。
	RequestTimeoutSec int `json:"request_timeout_sec"`
	// ListenerEnabled 是否在登录后建立入站消息长连接。指针区分「未配置(默认开)」
	// 与「显式 false 关」；listener 本身风险低，但不需要入站消息时可以关掉。
	ListenerEnabled *bool `json:"listener_enabled"`
	// FriendsCacheTTLSec 是好友列表缓存 TTL（秒），<=0 折算 300 秒，低于 300 按 300 取：
	// getAllFriends 被 zca-js 社区标为极高风险，必须缓存且两次拉取间隔 >=5 分钟。
	FriendsCacheTTLSec int `json:"friends_cache_ttl_sec"`
	// GroupsCacheTTLSec 是群列表缓存 TTL（秒），<=0 折算 300 秒，低于 300 按 300 取（同上）。
	GroupsCacheTTLSec int `json:"groups_cache_ttl_sec"`
	// InfoMinIntervalMs 是 getUserInfo/getGroupInfo 两次请求的最小间隔（毫秒），
	// <=0 折算 1500，低于 1500 按 1500 取。
	InfoMinIntervalMs int `json:"info_min_interval_ms"`
	// InfoCacheTTLSec 是用户/群资料缓存 TTL（秒），<=0 折算 600 秒，低于 600 按 600 取。
	InfoCacheTTLSec int `json:"info_cache_ttl_sec"`
	// SendMinDelayMs/SendMaxDelayMs 是同账号连续两条消息的随机间隔下/上界（毫秒），
	// 缺省 3000/5000，下界低于 3000 按 3000 取；下界大于上界时按上界取。
	SendMinDelayMs int `json:"send_min_delay_ms"`
	SendMaxDelayMs int `json:"send_max_delay_ms"`
	// SendMaxPerHour 是单账号滚动一小时内的发送闸，<=0 折算 30 条。
	// 非官方客户端发得越密越容易被风控，这条是封号风险的软闸。
	SendMaxPerHour int `json:"send_max_per_hour"`
	// UserAgent/Language 是新登录（扫码/cookie 缺省）使用的 UA 与语言。
	// 留空由 zcago 用其默认值；已有凭证恢复时以库里保存的值为准，不被这里覆盖。
	UserAgent string `json:"user_agent"`
	Language  string `json:"language"`
}

// FacebookConfig 是 Facebook 通道配置，与 ZaloConfig 同构，差异只在供应商口径。
//
// 真实世界 Facebook 走 OAuth 跳转授权，但本包登录的是供应商代持账号，登录链路仍然只有
// 「下发一次性凭证、拿凭证换会话」两步。那个 code 到底是短信验证码、OAuth 授权码还是
// 回调 ticket，由供应商在 pay_sdks 里的登记口径决定，本层不猜：签名形状保持与另两条通道
// 同构，换成 OAuth 形态改的是 SDK 段与供应商适配代码，不是这里的一个字段。
//
// 供应商压根没有「下发验证码」这一环时，就在该 SDK 段的 endpoints 里不登记 send_code：
// 缺键的操作就地拒绝、不发请求，比为了凑齐两段而配一个假的登录网关省事，也不会让会员
// 看到一个点了必失败的入口。
type FacebookConfig struct {
	Login *PaySDKRef `json:"login"` // 登录/验证码下发通道引用的 SDK
	// Templates 是消息模板名表，逻辑名口径与 zalo/facebook/instagram 三条通道一致。
	Templates map[string]string `json:"templates"`
}

// InstagramConfig 是 Instagram 通道配置，与 FacebookConfig 同构。
//
// 两家同属 Meta、多半复用同一段 SDK，仍然拆成两个配置段：两条通道的账号、商户账户
// 与开关各自独立，合并成一段就等于让「只关 Instagram」这件事没法表达。
// code 形态的口径同 FacebookConfig 所述，也不在这里区分。
//
// Instagram 没有「好友」概念，好友列表操作的逻辑名仍沿用 friend_list，读作「按账号
// 返回的关联账号列表」：三条社交通道各起一个逻辑名的话，端点表就散成三个名字，
// 而取路径那侧只按逻辑名查，多一个名字就是多一处配漏了静默失败的地方。
type InstagramConfig struct {
	Login *PaySDKRef `json:"login"` // 登录/验证码下发通道引用的 SDK
	// Templates 是消息模板名表，逻辑名口径与 zalo/facebook/instagram 三条通道一致。
	Templates map[string]string `json:"templates"`
}

// UsdtConfig 是 USDT（TRC20）通道配置。
type UsdtConfig struct {
	Provider *PaySDKRef `json:"provider"`
	// Network 是公链网络标识（mainnet/shasta/nile），留空按供应商默认。
	// 这里要填的是「在哪条链上发」，不是代币标准：trc20 是标准名，填进来会在
	// 装配时被拒——网络决定单号能否跨链复用，标准名只是合约的属性。
	Network string `json:"network"`
	// Contract 是 TRC20 合约地址（Base58Check，过不了校验和则通道不启动）；
	// 原生 TRX 留空。网络与合约是两个正交维度：同一网络下 USDT 与原生币
	// 靠合约区分，同一合约在不同网络下又是两个不同的币，都不能靠通道名区分。
	Contract string `json:"contract"`
}

// Lua 配置
type LuaConfig struct {
	ScriptPath     string `json:"script_path"`     // Lua 脚本路径
	Enable         string `json:"enable"`          // 是否启用 "on" 或 "off"
	UpdateInterval int64  `json:"update_interval"` // 更新间隔 (毫秒)
	GCTickCount    int64  `json:"gc_tick_count"`   // GC 触发周期 (tick数)
	MaxMemoryMB    int64  `json:"max_memory_mb"`   // 最大内存限制 (MB)
}

// 服务器全局配置
type ServerConfig struct {
	Debug        bool   `json:"debug"`        // 调试模式
	FPS          int    `json:"fps"`          // 帧率
	Version      string `json:"version"`      // 版本号
	MaxMsgSize   int    `json:"max_msg_size"` // 最大消息大小
	WriteBuffer  int    `json:"write_buffer"` // 写队列容量（消息条数，不是字节）
	ReadBuffer   int    `json:"read_buffer"`  // 读队列容量（消息条数，不是字节；旧配置的字节值需换算）
	Backpressure bool   `json:"backpressure"` // 是否启用背压
	MaxProcs     int    `json:"max_procs"`    // GOMAXPROCS，<=0 时保留运行时默认值
}

// Metrics 监控配置
type MetricsConfig struct {
	Enabled bool   `json:"enabled"` // 是否启用 Prometheus 监控
	Port    int    `json:"port"`    // metrics HTTP 端口（默认 9090）
	Token   string `json:"token"`   // 非空时 /metrics 需要 Bearer 或 ?token=
}

// 模型选择策略取值
const (
	// ModelStrategyDefault 使用默认提供方 + 其默认模型
	ModelStrategyDefault = "default"
	// ModelStrategyCheapest 自动选择预估消费最低的模型
	ModelStrategyCheapest = "cheapest"
)

// ModelInfoConfig 单个模型条目（名称 + 价格 + 能力），用于策略比价
type ModelInfoConfig struct {
	Name           string  `json:"name"`            // 模型名
	InputPrice     float64 `json:"input_price"`     // 输入价格（美元/百万 token），0 表示未配置（可由在线价格源补齐）
	OutputPrice    float64 `json:"output_price"`    // 输出价格（美元/百万 token），0 表示未配置（可由在线价格源补齐）
	Priority       int     `json:"priority"`        // 优先级，数字越小越优先；0 为最高层（未配置时全部同层）
	MaxConcurrency int     `json:"max_concurrency"` // 最大并发在途请求数，0=不限（未配置时回退提供方级）
	RPMLimit       int     `json:"rpm_limit"`       // 每分钟最大请求数，0=不限（未配置时回退提供方级）
	SupportsTools  bool    `json:"supports_tools"`  // 是否支持 Function Calling
}

// 在线价格源取值
const (
	// PriceSourceLiteLLM 社区价格库（LiteLLM model_prices JSON）
	PriceSourceLiteLLM = "litellm"
	// PriceSourceOpenRouter OpenRouter /api/v1/models 的 pricing 字段
	PriceSourceOpenRouter = "openrouter"
	// PriceSourceOff 关闭在线价格获取（仅使用本地配置价格）
	PriceSourceOff = "off"
)

// PriceSourceConfig 在线价格源配置（模型未配置本地价格时自动查询）
type PriceSourceConfig struct {
	Provider string `json:"provider"`  // "litellm" / "openrouter" / "off"（关闭）；未配置本段时不获取在线价格
	URL      string `json:"url"`       // 可选，覆盖默认地址（自建镜像或测试）
	CacheTTL int    `json:"cache_ttl"` // 价格缓存时间（秒），默认 86400（24 小时）
	Timeout  int    `json:"timeout"`   // 拉取超时（秒），默认 10
}

// EffectiveProvider 返回生效的价格源名；未配置或取值非法/off 时返回空串（关闭）。
func (p *PriceSourceConfig) EffectiveProvider() string {
	if p == nil {
		return ""
	}
	switch s := strings.ToLower(strings.TrimSpace(p.Provider)); s {
	case PriceSourceLiteLLM, PriceSourceOpenRouter:
		return s
	default:
		return ""
	}
}

// ModelProviderConfig 单个模型提供方配置（OpenAI 兼容接口）
type ModelProviderConfig struct {
	BaseURL        string             `json:"base_url"`        // API 地址，如 https://api.openai.com/v1
	APIKey         string             `json:"api_key"`         // API 密钥
	Model          string             `json:"model"`           // 默认模型名；models 非空时须为其一，为空时取 models 首个
	Models         []*ModelInfoConfig `json:"models"`          // 可选：该提供方的模型列表（多模型 + 价格 + 能力）
	Timeout        int                `json:"timeout"`         // 请求超时（秒），默认 60
	MaxRetries     int                `json:"max_retries"`     // 429/5xx 重试次数，默认 2
	MaxConcurrency int                `json:"max_concurrency"` // 提供方级默认最大并发在途请求数，0=不限（模型未配置时回退到此值）
	RPMLimit       int                `json:"rpm_limit"`       // 提供方级默认每分钟最大请求数，0=不限（模型未配置时回退到此值）
}

// DefaultModel 返回生效的默认模型名：优先 model，其次 models 中首个有效条目。
func (p *ModelProviderConfig) DefaultModel() string {
	if p == nil {
		return ""
	}
	if s := strings.TrimSpace(p.Model); s != "" {
		return s
	}
	for _, m := range p.Models {
		if m != nil && strings.TrimSpace(m.Name) != "" {
			return strings.TrimSpace(m.Name)
		}
	}
	return ""
}

// ModelAPIConfig 模型API接入配置（支持多提供方、多模型与选择策略）
type ModelAPIConfig struct {
	Enable      string                          `json:"enable"`       // 是否启用 "on" 或 "off"
	Default     string                          `json:"default"`      // 默认提供方名（多个提供方时必填）
	Strategy    string                          `json:"strategy"`     // 模型选择策略："default"（默认）/ "cheapest"
	Providers   map[string]*ModelProviderConfig `json:"providers"`    // 命名的提供方
	PriceSource *PriceSourceConfig              `json:"price_source"` // 可选：在线价格源（未配置时不获取在线价格）
}

// StrategyOf 返回生效的模型选择策略；未配置时按 default 处理，未知取值由 Validate 拦截。
func (c *ModelAPIConfig) StrategyOf() string {
	if c == nil || strings.TrimSpace(c.Strategy) == "" {
		return ModelStrategyDefault
	}
	return strings.ToLower(strings.TrimSpace(c.Strategy))
}

// StrategyValid 策略取值是否合法（空 / default / cheapest）。
func (c *ModelAPIConfig) StrategyValid() bool {
	switch c.StrategyOf() {
	case ModelStrategyDefault, ModelStrategyCheapest:
		return true
	default:
		return false
	}
}

// NftConfig 是 NFT 通道段。开不开有两处真相、必须都开：
// ref 留空即本通道不启用；nft_sdks 段 enable 不为 "on" 即凭证不可用（口径对 PaySDKRef 注释）。
//
// 配置结构体刻意留在 config 包而不是 nft 包：本包的 Cfg 要引用它，而 nft 只认与配置无关的
// ProviderOptions（端点表也是以取值函数的形式交进去）。若把结构体挪到 nft，
// config→nft 与 nft→config 两头引用就成环了（硬约束 4）。
type NftConfig struct {
	Provider *NftSDKRef `json:"provider"` // 五动作共同引用的 SDK 段
	// Chain 是默认链标识（eth/bsc/tron/sol/ton），单里未填时由装配层补进去。
	Chain string `json:"chain"`
	// Network 是默认网络标识（mainnet/testnet/chapel/devnet），留空按供应商默认。
	// 「在哪条链上铸」必须是配置里看得出来的字段值，不能靠基址猜（pay/types.go:28-44 论证同构）。
	Network string `json:"network"`
	// Contract 是默认代币合约地址；留空表示铸造走「供应商部署新合约」形态。
	Contract string `json:"contract"`
}

// NftSDKConfig 是一段 NFT SDK 配置：字段形态对 PaySDKConfig 同构复刻，类型独立。
// 与资金段的差别只在 driver 名空间与 endpoints 键集（§6.3）。
//
// Enable 必须显式为 "on" 才启动。资产通道的新配置段默认不启动，比「写了 base_url 就
// 以为开了」更安全：漏一个开关的代价是少一条通道，多开一条通道的代价是真资产动作。
type NftSDKConfig struct {
	Enable      string `json:"enable"`       // 显式 "on" 才启动
	Driver      string `json:"driver"`       // nft.Register 登记过的驱动名（内置 "nft_generic_md5"）
	BaseURL     string `json:"base_url"`     // 不含末尾斜杠
	AppID       string `json:"app_id"`       // 参与签名域
	SecretKey   string `json:"secret_key"`   // 只经配置或 CallNftSDKMsg 钩子注入；禁入日志/error
	AuthHeader  string `json:"auth_header"`  // 留空按驱动默认 X-Sign
	TokenHeader string `json:"token_header"` // 留空按驱动默认 Authorization
	TimeoutSec  int    `json:"timeout_sec"`  // <=0 用 nft.DefaultTimeout
	MaxRetries  int    `json:"max_retries"`  // <0 用 nft.DefaultMaxRetry
	NotifyURL   string `json:"notify_url"`
	// Endpoints 键是本包固定的五个逻辑名：holdings/token/mint/transfer/query。
	// 缺某个键 ⇒ 对应操作返回明确错误，不发请求。
	Endpoints map[string]string `json:"endpoints"`
	// Accounts 是我方在该供应商名下的签发主体表，键是本地别名。
	// 「这一单从哪个主体签发」在配置解析期由装配层定死，报文里没有二次改口余地。
	Accounts map[string]*NftMerchantAccount `json:"accounts"`
}

// Enabled 该 NFT SDK 段是否被显式开启（enable 大小写不敏感的 "on"）。
func (p *NftSDKConfig) Enabled() bool {
	return p != nil && strings.EqualFold(strings.TrimSpace(p.Enable), "on")
}

// Endpoint 按逻辑名取供应商路径。未登记时 ok=false——配置不全的操作必须在发请求之前就拒掉。
func (p *NftSDKConfig) Endpoint(name string) (string, bool) {
	if p == nil {
		return "", false
	}
	v := strings.TrimSpace(p.Endpoints[name])
	if v == "" {
		return "", false
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	return v, true
}

// Account 按别名取签发主体。别名留空且表中只有一个账户时取那一个；多于一个则报错要求点名
// ——静默挑一个主体铸币是不可接受的歧义。
func (p *NftSDKConfig) Account(alias string) (*NftMerchantAccount, error) {
	if p == nil || len(p.Accounts) == 0 {
		return nil, errors.New("未配置 accounts 签发主体")
	}
	if alias == "" {
		if len(p.Accounts) > 1 {
			return nil, fmt.Errorf("accounts 里有 %d 个签发主体，通道段必须用 account 点名", len(p.Accounts))
		}
		for k := range p.Accounts {
			alias = k
		}
	}
	a, ok := p.Accounts[alias]
	if !ok {
		return nil, fmt.Errorf("accounts 里没有名为 %s 的签发主体", alias)
	}
	if !a.Enabled() {
		return nil, fmt.Errorf("accounts.%s 未显式开启（enable 需为 on）", alias)
	}
	if strings.TrimSpace(a.MerchantID) == "" {
		return nil, fmt.Errorf("accounts.%s 缺 merchant_id", alias)
	}
	return a, nil
}

// NftMerchantAccount 是「我方在 NFT 供应商侧的一个签发主体」。
// 余额类事实不写在这里（由查询动作当场读回），抄进来就是会过期的假账（PayMerchantAccount 论证同构）。
type NftMerchantAccount struct {
	Enable     string `json:"enable"`      // 显式 "on"
	MerchantID string `json:"merchant_id"` // 进报文并参与签名域；是路由信息不是凭证，可进日志
}

// Enabled 该签发主体是否可用（口径与 SDK 段一致：显式 "on" 才算开）。
func (a *NftMerchantAccount) Enabled() bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.Enable), "on")
}

// NftSDKRef 是 nft.provider 段对 nft_sdks 的引用：只留两个名字，不留凭证。
type NftSDKRef struct {
	SDK     string `json:"sdk"`     // nft_sdks 的键名
	Account string `json:"account"` // 段内 accounts 的键名；单账户可留空，多账户必须点名
}

// Empty 该通道是否未绑定任何 SDK（未绑定即不启用）。
func (r *NftSDKRef) Empty() bool { return r == nil || strings.TrimSpace(r.SDK) == "" }

// Resolve 在全表里取被引用的 SDK 段与签发主体。五动作都会动资产（连只读侧也按主体口径走），
// nft 不需要 ResolveService 形态——这是与 pay 的显式差异（§6.2）。
//
// 报错文案只说名字与缺项，绝不带出基址之外的敏感值；SDK 段里可能的密钥一旦顺着 error
// 扩散，就会出现在日志和上层包装里。
func (r *NftSDKRef) Resolve(sdks map[string]*NftSDKConfig) (*NftSDKConfig, *NftMerchantAccount, error) {
	if r.Empty() {
		return nil, nil, errors.New("未配置 sdk 引用，该通道不启用")
	}
	name := strings.TrimSpace(r.SDK)
	cfg := sdks[name]
	if cfg == nil {
		return nil, nil, fmt.Errorf("nft_sdks 里没有名为 %s 的 SDK 段", name)
	}
	if !cfg.Enabled() {
		return nil, nil, fmt.Errorf("nft_sdks.%s 未显式开启（enable 需为 on）", name)
	}
	acc, err := cfg.Account(strings.TrimSpace(r.Account))
	if err != nil {
		return nil, nil, fmt.Errorf("nft_sdks.%s: %w", name, err)
	}
	return cfg, acc, nil
}
