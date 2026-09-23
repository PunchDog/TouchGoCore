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
	// Ton 是 TON 币充值/提现对本 SDK 的引用。代码归在 telegram 包（TON 的
	// 收付入口在 Telegram 侧），但资金接口的凭证域与 Bot 长连接完全不同，
	// 所以凭证集中在 pay_sdks，这里只留「用哪个 SDK、用哪个商户账户」。
	Ton *PaySDKRef `json:"ton"`
	// TonNetwork 是 TON 链网络标识（mainnet/testnet），留空按供应商默认。
	TonNetwork string `json:"ton_network"`
	// Jetton 是 TON 侧代币合约地址；原生 TON 留空。
	Jetton string `json:"jetton"`
}

// PaySDKConfig 是一段资金 SDK 配置：一套供应商接入的全部事实——驱动标记、
// 基址、凭证、端点表，以及我方在该供应商名下的商户账户表。
//
// 集中登记的用意：同一个供应商往往同时供着充值、提现、验证码下发三条链路，
// 凭证与端点抄三份之后必然改漏一处，出款那侧还留着旧密钥。SDK 段写一次，
// 各通道只留「用哪个 SDK、用哪个商户账户」两个名字。
//
// 配置结构体刻意留在 config 包而不是 pay 包：本包的 Cfg 要引用它，而 pay 只认
// 与配置无关的 ProviderOptions（端点表也是以取值函数的形式交进去）。若把结构体
// 挪到 pay，config→pay 与 pay→config 两头引用就成环了。
// 各通道包（whatsapp/usdt/telegram）负责把本结构逐字段转成 pay.ProviderOptions。
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
	NotifyURL   string `json:"notify_url"`   // 本侧回执接收地址，下单时随报文交出去
	// Endpoints 是「接口文档到位后只改这里」的注入点之一：
	// 键是本仓代码里固定的逻辑名（account/recharge/query/withdraw/login/send_code），
	// 值是供应商的实际路径。缺某个键 ⇒ 对应操作返回明确错误，不发请求。
	Endpoints map[string]string `json:"endpoints"`
	// Accounts 是我方在该供应商名下的商户账户表，键是本地别名（default/reserve…），
	// 通道段用别名引用。商户号是路由信息不是凭证，可以进日志。
	Accounts map[string]*PayMerchantAccount `json:"accounts"`
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
// 通道段（whatsapp.login / whatsapp.provider / usdt.provider / telegram.ton）
// 各自的开关就是这里填不填名字：留空即该通道不启用。之所以不再单独放一个
// enable，是为了让「开不开」只有一处真相——SDK 段的 enable 决定凭证能不能用，
// 这里的 sdk 决定这条通道用不用它；两处都开才是开。
type PaySDKRef struct {
	SDK string `json:"sdk"` // pay_sdks 的键名
	// Account 是 SDK 段 accounts 里的键名。该 SDK 只有一个账户时留空即取那一个；
	// 多于一个账户时必须点名，否则「这一单从哪个商户号出」就成了初始化顺序的函数。
	Account string `json:"account"`
}

// Empty 该通道是否未绑定任何 SDK（未绑定即不启用）。
func (r *PaySDKRef) Empty() bool { return r == nil || strings.TrimSpace(r.SDK) == "" }

// Resolve 在全表里取被引用的 SDK 段与商户账户。资金通道（充值/提现）用它：
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

// WhatsappConfig 是 WhatsApp 通道配置。登录与资金走两家供应商是常见形态
// （验证码走消息网关、充值提现走支付网关），所以两段各自引用一个 SDK。
type WhatsappConfig struct {
	Login    *PaySDKRef `json:"login"`    // 登录/验证码下发通道引用的 SDK
	Provider *PaySDKRef `json:"provider"` // 充值/提现通道引用的 SDK
	// Templates 是消息模板名表，键为逻辑名（auth_verify/bind_notice）。
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
