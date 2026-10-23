package ton

import "time"

// 本文件是 TON 侧的两个装配输入结构。
//
// 与 tron 侧刻意重复，不抽公共包：两条链的接入点、签名库与失败路径都不同，
// 抽一层「通用链接入」只会把两家差异挤进同一个袋子里。TON 的供应商账户查询
// 已删除，所以这里不再保留 merchant/base_url/secret_key 等字段——通道只认链。

// channelOptions 是构造一条 TON 资金通道所需的全部输入。
type channelOptions struct {
	// Name 是通道名（ton），进错误文案与日志。
	Name   string
	Driver string
	// Chain 是链上接入点与热钱包。nil 即「这一段不是链上通道」。
	Chain *chainConfig
}

// chainConfig 是「直接动公链」所需的接入点与出款主体。
type chainConfig struct {
	// HTTPBase 是 TonAPI v2 的基址；留空按 network 取公开基址。
	HTTPBase string
	// APIKey 是 TonAPI 的访问令牌（Authorization: Bearer）。
	APIKey        string
	WalletAddress string
	// WalletVersion 是热钱包合约版本名；留空由装配期探测。
	WalletVersion string
	// SubWalletID 是 v3/v4 系钱包的子账户号。指针：0 是合法取值，
	// 用 0 表达「没配」会把显式配 0 的人推到默认值上，那派生出来的是另一个账户。
	SubWalletID *uint32
	PrivateKey  string
	Network     string
	// FeeLimitSun 是本单之外必须仍留在热钱包里的余量，单位纳吨（nanoton）。
	// 字段名沿用 sun 这个「最小单位」的叫法，与 tron 侧同口径，避免两处各写一个名字。
	FeeLimitSun int64
	Timeout     time.Duration
	MaxRetry    int
}
