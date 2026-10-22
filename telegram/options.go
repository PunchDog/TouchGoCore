package telegram

import "time"

// 本文件是 TON 侧的两个装配输入结构。
//
// 它们曾经是 pay.ProviderOptions 与 pay.ChainOptions——被三家通道共用的同一副形状。
// 共用把 TON 真正需要的两样东西挤掉了：wallet_version 与 sub_wallet_id（v3/v4 系钱包
// 的子账户号，0 是合法取值）。它们在共用结构里只占一行「这条通道不读它」的注释，
// 而 TON 这边得靠指针类型才敢表达「没配」与「配了 0」的区别。
//
// 现在各包各有自己的一份：字段少而准。与 tron 侧刻意重复，不抽公共包——
// 两条链的供应商不一定是同一家，抽一层「通用供应商客户端」只会把两家差异
// 挤进同一个袋子里，而袋子越长，改一家就得担心另一家。

// channelOptions 是构造一条 TON 资金通道所需的全部输入。
type channelOptions struct {
	// Name 是通道名（ton），进错误文案与日志。
	Name   string
	Driver string
	// MerchantID 是我方在供应商侧的商户号，进报文并参与签名域。
	MerchantID string
	// BaseURL 是供应商侧的基址；账户查询走它。留空即没有供应商账户查询链路。
	BaseURL   string
	Timeout   time.Duration
	MaxRetry  int
	AppID     string
	SecretKey string
	// AuthHeader 是签名头的名字，留空取缺省值。
	AuthHeader string
	NotifyURL  string
	// Endpoint 是按逻辑端点名取路径的取值函数。
	Endpoint func(name string) (string, bool)
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
