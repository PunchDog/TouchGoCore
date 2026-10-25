package bsc

import (
	"context"
	"strings"
	"time"

	"touchgocore/chainroute"
	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// 本文件是 USDT-BEP20 侧的装配与门面。
//
// 装配读 bsc.usdt 一段：合约、网络、小数位与 chain（RPC + 热钱包）都在同一段里，
// 不必再去另一张表对一遍。余额查询直接读链上 balanceOf，所以这段没有供应商 HTTP
// 字段——也正因如此，本通道的「账户」含义与 tron 那侧不同（见 QueryAccount）。

// driverBSCRPC 是本包登记的驱动名。
//
// 驱动名只用于日志与错误定位，但装配期仍然要求它逐字相等：写错驱动名却照常启动，
// 等于「配置写着换实现、实际还是这一套」静默通过。
const driverBSCRPC = "bsc_jsonrpc"

// bscUSDTKey 是密钥注入钩子键名。
//
// 键名直接取分派表里的种类（等价于配置段名 bsc.usdt）：与 Kind 同源则连
// 「两边各写一遍、改一处忘一处」的路都不留。
const bscUSDTKey = string(pay.KindBscUSDT)

// newBscUSDTClient 按 bsc.usdt 的内联字段构造本包的 channelOptions 与 chainConfig。
//
// 校验口径与 tron 侧逐条对齐：驱动名必须是本包登记的那一个、合约必须过 EIP-55、
// 网络必须显式、私钥必须在构造前注入、chain 必须在。任何一条不成立都返回 ok=false——
// 本通道宁可不起，也不能带着「不知道该发哪张合约」的快照上线。
func newBscUSDTClient(cfg *config.Cfg) (*chainroute.Snapshot, bool) {
	if cfg == nil || cfg.Bsc == nil || cfg.Bsc.Usdt == nil {
		vars.Info("BSC-USDT 通道不启动: 缺 bsc.usdt 段")
		return nil, false
	}
	u := cfg.Bsc.Usdt
	if !u.Enabled() {
		vars.Info("BSC-USDT 通道不启动: bsc.usdt.enable 不是 on")
		return nil, false
	}
	driver := strings.TrimSpace(u.Driver)
	if driver != driverBSCRPC {
		vars.Info("BSC-USDT 通道不启动: bsc.usdt.driver=%q 不是本包登记的链上驱动 %s", driver, driverBSCRPC)
		return nil, false
	}
	contract := strings.TrimSpace(u.Contract)
	if contract == "" {
		vars.Info("BSC-USDT 通道不启动: bsc.usdt.contract 为空，USDT 通道必须点明合约地址")
		return nil, false
	}
	if !IsValidEVMAddress(contract) {
		vars.Info("BSC-USDT 通道不启动: 配置的 BEP20 合约地址 %s 不合法（应为 0x 开头的 40 位十六进制，混合大小写须过 EIP-55）", contract)
		return nil, false
	}
	network := strings.TrimSpace(u.Network)
	if network == "" {
		// 网络不是可选项：它决定订单能否跨链复用，也让「这张合约在哪张网」有地方写。
		vars.Info("BSC-USDT 通道不启动: bsc.usdt.network 为空（mainnet / chapel）")
		return nil, false
	}
	if u.Chain == nil {
		vars.Info("BSC-USDT 通道不启动: bsc.usdt.chain 为空，链上出款必须要 RPC 与热钱包")
		return nil, false
	}
	// 私钥只从钩子注入：配置里恒留空。P6 起不再回落旧段名——
	// 同一个段有两个名字都能把密钥塞进来，密钥从哪来就变成了猜的事。
	injectKey(util.CallPayChainMsg, bscUSDTKey, &u.Chain.PrivateKey)

	decimals := u.Decimals
	if decimals <= 0 {
		decimals = defaultBSCDecimals
	}
	opt := channelOptions{
		Name:     "usdt",
		Driver:   driver,
		Network:  network,
		Contract: contract,
		Decimals: decimals,
		Chain:    inlineBscChainOptions(u.Chain),
	}
	ch, err := newBscOnchainChannel(opt)
	if err != nil {
		vars.Info("BSC-USDT 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("BSC-USDT 通道已就绪: driver=%s 合约=%s 网络=%s 小数位=%d 热钱包=%s",
		driver, contract, network, decimals, strings.TrimSpace(u.Chain.WalletAddress))
	vars.Info("BSC-USDT 金额口径: 18 位最小单位，超出 int64 的金额走 PayOrder.AmountUnits（十进制字符串）")
	return &chainroute.Snapshot{Ch: ch, Network: network}, true
}

// inlineBscChainOptions 把 bsc.usdt.chain 合成本包的接入点结构。
func inlineBscChainOptions(c *config.BscChainConfig) *chainConfig {
	if c == nil {
		return nil
	}
	return &chainConfig{
		RPC:           strings.TrimSpace(c.RPC),
		ChainID:       c.ChainID,
		WalletAddress: strings.TrimSpace(c.WalletAddress),
		PrivateKey:    strings.TrimSpace(c.PrivateKey),
		GasLimit:      c.GasLimit,
		GasPriceGwei:  c.GasPriceGwei,
		Timeout:       secondsOf(c.TimeoutSec),
		MaxRetry:      c.MaxRetries,
	}
}

// secondsOf 把配置里的秒数换算成超时；<=0 交给驱动用缺省值。
func secondsOf(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
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

// BscUsdtWithdraw 直接上链出款：本地组 BEP20 transfer、用热钱包私钥签名、经 JSON-RPC 广播。
//
// order.OrderNo 仍然是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔，广播结果说不清时按哈希查证并回 UNKNOWN（带 TxHash 供对账）。
// 补单、回执广播与未启动拒绝的机器见 chainroute.Route.Withdraw。
func BscUsdtWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	return bscRoute.Withdraw(ctx, order)
}

// BscUsdtQuery 按订单号查链上处置结果（台账里的哈希 → 回执 → 确认数）。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func BscUsdtQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	return bscRoute.Query(ctx, orderNo)
}

// BscUsdtAccount 读热钱包在 USDT 合约上的链上余额。
//
// 与 tron 那侧的 UsdtAccount 不是同一个口径：那边读供应商账本里的额度
// （可用 / 冻结 / 授信），这边只读得到一个数——链上余额。契约里没有「可用 / 冻结」
// 之说，所以 Available 与 Balance 同值、其余留 0，RawNote 里附上 gas 余额。
// 这是读操作，不广播回执：下游要的是当场判断（能不能发这么多），
// 而真正的资金闸在出款前置里（见 precheckFunds），那里比的是大整数。
func BscUsdtAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	return bscRoute.Account(ctx, q)
}
