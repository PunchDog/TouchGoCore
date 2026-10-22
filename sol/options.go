package sol

import (
	"strings"
	"time"
)

// 本文件是 Solana 侧的两个装配输入结构。
//
// 与 tron/options.go、telegram/options.go、bsc/options.go 同一个理由：每条链只持有
// 自己真正读的字段。Solana 需要的是 RPC、mint、确认级别、优先费与热钱包；
// 供应商账户那套字段一个都不要——本通道的余额直接读链上 token account。
//
// 与 BSC 的差别只有一处，但很要紧：EVM 用 nonce 给交易排序，Solana 用 blockhash
// 的有效期（约 150 个区块）给交易定生死。因此这里没有 gas_limit / gas_price，
// 换成了 commitment 与 priority_fee_microlamports。

// channelOptions 是构造一条 Solana 资金通道所需的全部输入。
type channelOptions struct {
	// Name 是通道名（usdt），进错误文案与日志。
	Name string
	// Driver 是驱动标记，只用于日志与错误定位，不参与判定。
	Driver string
	// Network 是本通道发到哪张网（mainnet-beta / devnet），参与订单网络校验。
	Network string
	// Mint 是 SPL 代币的 mint 地址（Base58 32 字节）：出款、余额、查单都认它。
	Mint string
	// Decimals 是 mint 小数位。PayOrder.Decimals 填了就必须等于它。
	Decimals int
	// Chain 是 JSON-RPC 接入点与热钱包。
	Chain *chainConfig
}

// chainConfig 是「直接动 Solana」所需的接入点与出款主体。
type chainConfig struct {
	// RPC 是节点 JSON-RPC 基址：广播、查回执、余额、签名反查都走它。
	RPC string
	// APIKey 是服务商的访问令牌（Helius/QuickNode），公开节点留空。
	// 不是资金密钥，但同样禁止进日志与 error 文案。
	APIKey string
	// WalletAddress 是热钱包地址，必须与私钥派生地址逐字相等。
	WalletAddress string
	// PrivateKey 是热钱包私钥（base58 或 hex）。配置里恒为空，
	// 由 util.CallPayChainMsg+段名 注入。
	PrivateKey string
	// Commitment 是查单/查余额用的确认级别（processed/confirmed/finalized）。
	// 留空按 confirmed：finalized 在出款回执上往往要等很久。
	Commitment string
	// PriorityFeeMicroLamports 是 ComputeBudget 的优先费（micro-lamports/CU）。
	// <=0 不附 ComputeBudget 指令——没有优先费就不该凭空多花这笔钱。
	PriorityFeeMicroLamports uint64
	// Timeout 是单次 RPC 调用的超时。
	Timeout time.Duration
	// MaxRetry 是读操作的重试次数；写侧（广播）从不看它。
	MaxRetry int
}

// secondsOf 把配置里的秒数换算成超时；<=0 交给驱动用缺省值。
func secondsOf(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// solCommitment 归一配置里的确认级别。
//
// 认不出的值按 confirmed 处理而不是报错不启动：确认级别只影响「查得多严」，
// 不影响「钱发给谁」；为一个拼错的词让整条通道停摆，代价比收益大。
func solCommitment(s string) string {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "processed", "confirmed", "finalized":
		return v
	default:
		return "confirmed"
	}
}
