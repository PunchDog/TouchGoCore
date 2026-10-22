package tron

import "time"

// 本文件是 TRON 侧的两个装配输入结构。
//
// 它们曾经是 pay.ProviderOptions 与 pay.ChainOptions——住在契约层、被三家通道共用。
// 共用听起来省事，实际是把「TRON 需要什么」和「TON 需要什么」挤进同一个形状里：
// TON 要 wallet_version 与 sub_wallet_id，TRON 一个都不要；TRON 要 tls 开关与
// fee_limit_sun，那是 TRON 的计费单位。合在一起的结果是每个字段都带着一句
// 「这条通道不读它」的注释，而读代码的人得先分清哪半属于自己。
//
// 现在各包各有自己的那一份：字段少而准，加一条链不必回来改别人。代价是装配时
// 不能直接传真身过去，得逐字段填一遍——那几行代码本来就该由最懂这条链的人写。

// channelOptions 是构造一条 TRON 资金通道所需的全部输入。
type channelOptions struct {
	// Name 是通道名（usdt），进错误文案与日志。
	Name string
	// Driver 是驱动标记，只用于日志与错误定位，不参与判定。
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
	// Endpoint 是按逻辑端点名取路径的取值函数：端点表住在 config 包，
	// 本包不引它，装配时转成一个纯函数传进来。
	Endpoint func(name string) (string, bool)
	// Extras 是通道特有字段（USDT 的 contract）。登记顺序就是签名域追加顺序：
	// 报文里有、签名域不跟着追加，供应商会按「报文有、签名没」全量拒签。
	Extras []extraField
	// Chain 是链上接入点与热钱包。nil 即「这一段不是链上通道」。
	Chain *chainConfig
}

// chainConfig 是「直接动公链」所需的接入点与出款主体。
type chainConfig struct {
	Node          string
	HTTPBase      string
	PlaintextNode bool
	APIKey        string
	WalletAddress string
	PrivateKey    string
	Contract      string
	Network       string
	FeeLimitSun   int64
	Timeout       time.Duration
	MaxRetry      int
}

// extraField 是一个通道特有字段的「JSON 键名 + 取值」。
type extraField struct {
	Name  string
	Value string
}
