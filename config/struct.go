package config

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
	Host     string `json:"redis_host"`     //连接地址
	Password string `json:"redis_password"` //用户密码
	Db       int    `json:"redis_db"`       //库编号
}

// TLSConfig 通用服务端 TLS（Gin / WebSocket 直连场景；前置反代可保持 enable=false）
type TLSConfig struct {
	Enable   bool   `json:"enable"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type WebConfig struct {
	HTTPPort     int      `json:"httpport"`      //监听端口
	Static       *string  `json:"static"`        //文件服
	AllowOrigins []string `json:"allow_origins"` // CORS 允许的来源，空则不允许跨域（生产默认）
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
	// 内网连接是否跳过 Origin 验证（仅当 RemoteAddr 为内网且未伪造头时）
	SkipOriginForIntranet bool `json:"skip_origin_for_intranet"`
	// 信任的反向代理，仅这些 RemoteAddr 才读取 X-Forwarded-For / X-Real-IP
	TrustedProxies []string `json:"trusted_proxies"`
	// Worker Pool 配置
	WorkerPoolSize int  `json:"worker_pool_size"` // Worker 数量，0 表示串行模式（默认），>0 启用并行
	ShardByKey     bool `json:"shard_by_key"`     // 是否按 UID 分片（保证同 UID 消息顺序性）
	// 认证配置
	AuthTokenHeader  string `json:"auth_token_header"`  // 认证 Token 的 HTTP 头名称（默认 "X-Auth-Token"），为空则不验证
	AuthTokenQuery   string `json:"auth_token_query"`   // 认证 Token 的 URL 查询参数名（默认 "token"），为空则不从 query 读取
	AuthIntranetSkip bool   `json:"auth_intranet_skip"` // 内网连接是否跳过认证
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
	// 鉴权，缺省为 none（仅校验 client-name 存在）
	Auth *RpcAuthConfig `json:"auth"`
}

// telegram配置
type TelegramConfig struct {
	BotToken        string            `json:"bot_token"`
	GameUrl         string            `json:"game_url"`
	GameBannerUrl   string            `json:"game_banner_url"`
	GameDescription string            `json:"game_description"`
	GameToShort     map[string]string `json:"game_to_short"`
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
	WriteBuffer  int    `json:"write_buffer"` // 写缓冲大小
	ReadBuffer   int    `json:"read_buffer"`  // 读缓冲大小
	Backpressure bool   `json:"backpressure"` // 是否启用背压
}

// Metrics 监控配置
type MetricsConfig struct {
	Enabled bool `json:"enabled"` // 是否启用 Prometheus 监控
	Port    int  `json:"port"`    // metrics HTTP 端口（默认 9090）
}
