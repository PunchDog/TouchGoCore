package sol

import (
	"context"
	"math/big"
	"strings"

	"touchgocore/chainroute"
	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// 本文件是 USDT-SPL 侧的装配与门面。
//
// 装配读 sol.usdt 一段：mint、网络、小数位与 chain（RPC + 热钱包）都在同一段里，
// 不必再去另一张表对一遍。余额查询直接读链上 token account，所以这段没有供应商
// HTTP 字段——也正因如此，本通道的「账户」含义与 tron 那侧不同（见 QueryAccount）。

// driverSolRPC 是本包登记的驱动名。
//
// 驱动名只用于日志与错误定位，但装配期仍然要求它逐字相等：写错驱动名却照常启动，
// 等于「配置写着换实现、实际还是这一套」静默通过。
const driverSolRPC = "sol_rpc"

// solUSDTKey 是密钥注入钩子键名。
//
// 键名直接取分派表里的种类（等价于配置段名 sol.usdt）：与 Kind 同源则连
// 「两边各写一遍、改一处忘一处」的路都不留。
const solUSDTKey = string(pay.KindSolUSDT)

// maxInt64 是 int64 的上界，用于算「单笔最大可表达金额」的提示值。
const maxInt64 = int64(^uint64(0) >> 1)

// newSolUSDTClient 按 sol.usdt 的内联字段构造本包的 channelOptions 与 chainConfig。
//
// 校验口径与 tron / bsc 两侧逐条对齐：驱动名必须是本包登记的那一个、mint 必须过
// Base58 32 字节、网络必须显式、私钥必须在构造前注入、chain 必须在。任何一条不成立
// 都返回 ok=false——本通道宁可不起，也不能带着「不知道该发哪个 mint」的快照上线。
func newSolUSDTClient(cfg *config.Cfg) (*chainroute.Snapshot, bool) {
	if cfg == nil || cfg.Sol == nil || cfg.Sol.Usdt == nil {
		vars.Info("SOL-USDT 通道不启动: 缺 sol.usdt 段")
		return nil, false
	}
	u := cfg.Sol.Usdt
	if !u.Enabled() {
		vars.Info("SOL-USDT 通道不启动: sol.usdt.enable 不是 on")
		return nil, false
	}
	driver := strings.TrimSpace(u.Driver)
	if driver != driverSolRPC {
		vars.Info("SOL-USDT 通道不启动: sol.usdt.driver=%q 不是本包登记的链上驱动 %s", driver, driverSolRPC)
		return nil, false
	}
	mint := strings.TrimSpace(u.Contract)
	if mint == "" {
		vars.Info("SOL-USDT 通道不启动: sol.usdt.contract 为空，USDT 通道必须点明 mint 地址")
		return nil, false
	}
	if !IsValidSolanaAddress(mint) {
		vars.Info("SOL-USDT 通道不启动: 配置的 SPL mint %s 不合法（应为 Base58 编码且解出恰好 32 字节）", mint)
		return nil, false
	}
	network := strings.TrimSpace(u.Network)
	if network == "" {
		// 网络不是可选项：它决定订单能否跨链复用，也让「这张 mint 在哪张网」有地方写。
		vars.Info("SOL-USDT 通道不启动: sol.usdt.network 为空（mainnet-beta / devnet）")
		return nil, false
	}
	if u.Chain == nil {
		vars.Info("SOL-USDT 通道不启动: sol.usdt.chain 为空，链上出款必须要 RPC 与热钱包")
		return nil, false
	}
	// 私钥只从钩子注入：配置里恒留空。与 tron / bsc 同一把钥匙、同一个键名规则。
	injectKey(util.CallPayChainMsg, solUSDTKey, &u.Chain.PrivateKey)

	decimals := u.Decimals
	if decimals <= 0 {
		decimals = defaultSOLDecimals
	}
	opt := channelOptions{
		Name:     "usdt",
		Driver:   driver,
		Network:  network,
		Mint:     mint,
		Decimals: decimals,
		Chain:    inlineSolChainOptions(u.Chain),
	}
	ch, err := newSolOnchainChannel(opt)
	if err != nil {
		vars.Info("SOL-USDT 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("SOL-USDT 通道已就绪: driver=%s mint=%s 网络=%s 小数位=%d 热钱包=%s",
		driver, mint, network, decimals, strings.TrimSpace(u.Chain.WalletAddress))
	vars.Info("SOL-USDT 出款上限提示: PayOrder.Amount 为 int64 最小单位，%d 位小数下单笔最大约 %s 个 USDT",
		decimals, maxOrderAmountHint(decimals))
	return &chainroute.Snapshot{Ch: ch, Network: network}, true
}

// inlineSolChainOptions 把 sol.usdt.chain 合成本包的接入点结构。
func inlineSolChainOptions(c *config.SolChainConfig) *chainConfig {
	if c == nil {
		return nil
	}
	return &chainConfig{
		RPC:                      strings.TrimSpace(c.RPC),
		APIKey:                   strings.TrimSpace(c.APIKey),
		WalletAddress:            strings.TrimSpace(c.WalletAddress),
		PrivateKey:               strings.TrimSpace(c.PrivateKey),
		Commitment:               c.Commitment,
		PriorityFeeMicroLamports: c.PriorityFeeMicrolamports,
		Timeout:                  secondsOf(c.TimeoutSec),
		MaxRetry:                 c.MaxRetries,
	}
}

// injectKey 执行一次密钥注入：只认段名这一个键。
//
// 注入钩子是密钥进进程的唯一通道（配置里恒留空）。只认一个键名时，「密钥没注入」
// 与「注入到了别的键」不再混为一谈：两者都会让通道不启动，日志里都指向同一个键。
func injectKey(prefix, key string, field *string) {
	if field == nil || key == "" {
		return
	}
	util.DefaultCallFunc.Do(prefix+key, field)
}

// maxOrderAmountHint 算出「int64 的最小单位在 N 位小数下最多是几个币」。
//
// 只为启动日志服务：6 位小数下这个上限约 9.22e12，真实出款远够用；把它写进日志，
// 比让运维在第一笔超大额时收到一句「金额非法」要好。
func maxOrderAmountHint(decimals int) string {
	if decimals < 0 {
		decimals = 0
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Rat).SetFrac(big.NewInt(maxInt64), den).FloatString(2)
}

// SolUsdtWithdraw 直接上链出款：本地组 transferChecked、用热钱包私钥签名、经 JSON-RPC 广播。
//
// order.OrderNo 仍然是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔（唯一例外是「签好了、确定没交给节点、且 blockhash 已失效」），
// 广播结果说不清时按签名查证并回 UNKNOWN（带 TxHash 供对账）。
// 补单、回执广播与未启动拒绝的机器见 chainroute.Route.Withdraw。
func SolUsdtWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	return solRoute.Withdraw(ctx, order)
}

// SolUsdtQuery 按订单号查链上处置结果（台账里的签名 → 交易 meta → 确认数）。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func SolUsdtQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	return solRoute.Query(ctx, orderNo)
}

// SolUsdtAccount 读热钱包在 USDT-SPL 上的链上余额。
//
// 与 tron 那侧的 UsdtAccount 不是同一个口径：那边读供应商账本里的额度
// （可用 / 冻结 / 授信），这边只读得到一个数——热钱包 ATA 的链上余额。
// 链上没有「可用 / 冻结」之说，所以 Available 与 Balance 同值、其余留 0，
// RawNote 里附上原生 SOL 余额（手续费收在它上面）。这是读操作，不广播回执：
// 下游要的是当场判断（能不能发这么多），而真正的资金闸在出款前置里（见 precheckFunds）。
func SolUsdtAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	return solRoute.Account(ctx, q)
}
