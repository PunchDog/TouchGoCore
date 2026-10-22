package bsc

import "time"

// 本文件是 BSC 侧的两个装配输入结构。
//
// 与 tron/options.go、telegram/options.go 同一个理由：每条链只持有自己真正读的字段。
// BSC 是 EVM 形态，需要的是 RPC、chain_id、gas 与热钱包；供应商账户那套字段一个都不要
// （本通道的余额直接读链上 balanceOf）。合成一张大表的结果是每个字段都带一句
// 「这条通道不读它」，而读代码的人得先分清哪半属于自己。

// channelOptions 是构造一条 BSC 资金通道所需的全部输入。
type channelOptions struct {
	// Name 是通道名（usdt），进错误文案与日志。
	Name string
	// Driver 是驱动标记，只用于日志与错误定位，不参与判定。
	Driver string
	// Network 是本通道发到哪张网（mainnet / chapel），参与订单网络校验。
	Network string
	// Contract 是 BEP20 合约地址（EIP-55），出款与余额都认它。
	Contract string
	// Decimals 是合约小数位。PayOrder.Decimals 填了就必须等于它。
	Decimals int
	// Chain 是 JSON-RPC 接入点与热钱包。
	Chain *chainConfig
}

// chainConfig 是「直接动 BSC」所需的接入点与出款主体。
type chainConfig struct {
	// RPC 是节点 JSON-RPC 基址：广播、回执、balanceOf、日志查询都走它。
	RPC string
	// ChainID 是 EIP-155 链 ID（主网 56 / Chapel 97）。
	ChainID int64
	// WalletAddress 是热钱包地址，必须与私钥派生地址逐字相等。
	WalletAddress string
	// PrivateKey 是热钱包私钥（hex）。配置里恒为空，由 CallPayChainMsg+段名 注入。
	PrivateKey string
	// GasLimit 是 transfer 的 gas 上限，<=0 由驱动按估算值定。
	GasLimit uint64
	// GasPriceGwei 是 gas 价下限（gwei）。节点报价超过它的 2 倍时拒单。
	GasPriceGwei float64
	Timeout      time.Duration
	MaxRetry     int
}
