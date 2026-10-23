package ton

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// 本文件是 TON 侧的装配与门面。
//
// 出款不经供应商：按 ton.native 的内联字段装配一条链上通道（onchain.go），
// 提现单在本地签名后直接广播，结论从链上读回。供应商侧账户查询已删除；
// 账户门面现在读的是默认热钱包的链上余额（QueryOnchainAccount 可指定地址）。

// tonNativeRoute 是原生 TON 门面实例：装配读 ton.native，回执走 CallTonMsg，
// 装配成功即登记到 pay 的 ton.native 种类。
var tonNativeRoute = route{
	label:     "TON",
	channel:   "native",
	callback:  util.CallTonMsg,
	currency:  pay.CurrencyTON,
	newClient: newTONNativeClient,
	kind:      pay.KindTON,
}

// isKnownTONNetwork 是配置里允许的 TON 网络名。
//
// 只认 pay 的规范名（mainnet / testnet，留空按主网），刻意不接受 TonAPI 自己回显的
// "testnet-v4" 这类写法：pay.IsTestnet 不认识它，收下来就等于配置说测试网、
// 校验按主网走。也别把 shasta/nile 或 ton/jetton 当网络收进来。
func isKnownTONNetwork(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "", pay.NetworkMainnet, pay.NetworkTestnet:
		return true
	}
	return false
}

// TonStart 按 ton.native 装配 TON 资金链路。
//
// 配置缺失、通道没开、密钥未注入，都只记日志并返回：资金通道没配好不该阻断
// 整机启动，但必须留下可查的痕迹。
func TonStart(ctx context.Context) { tonNativeRoute.start(ctx) }

// TonStop 摘掉 TON 资金通道并释放接入点。可重复调用。
func TonStop(ctx context.Context) { tonNativeRoute.stop() }

// inlineTONKey 是密钥注入钩子键名；键名直接取分派表里的种类（等价于配置段名 ton.native）。
const inlineTONKey = string(pay.KindTON)

// newTONNativeClient 按 ton.native 的内联字段构造 channelOptions 与 chainConfig。
//
// 报告口径：密钥必须在构造前注入、驱动名必须是本包登记的那一个、热钱包地址必须
// 能从配置里读出来。任何一条不成立都返回 ok=false——出款链路宁可不起。
func newTONNativeClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Ton == nil || cfg.Ton.Native == nil {
		vars.Info("TON 通道不启动: 缺 ton.native 段")
		return nil, false
	}
	n := cfg.Ton.Native
	network, ok := tonNetworkOf(n)
	if !ok {
		return nil, false
	}
	if !n.Enabled() {
		vars.Info("TON 通道不启动: ton.native.enable 不是 on")
		return nil, false
	}
	driver := strings.TrimSpace(n.Driver)
	if driver != DriverTonAPI {
		// 驱动名必须自己把住：写错驱动名在这里放行，就等于「配置写着换实现、
		// 实际还是这一套」静默通过。
		vars.Info("TON 通道不启动: ton.native.driver=%q 不是本包登记的链上驱动 %s", driver, DriverTonAPI)
		return nil, false
	}
	// TON 不支持按单私钥：配置私钥是唯一来源，必须在构造前注入。
	if n.Chain != nil {
		injectKey(util.CallPayChainMsg, inlineTONKey, &n.Chain.PrivateKey)
	}
	ch, err := newTonOnchainChannel(channelOptions{
		Name:   "ton",
		Driver: driver,
		Chain:  tonChainOptions(n.Chain, network),
	})
	if err != nil {
		vars.Info("TON 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("TON 通道已就绪: driver=%s 网络=%s 热钱包=%s", driver, network, strings.TrimSpace(n.Chain.WalletAddress))
	return &client{ch: ch, network: network}, true
}

// tonNetworkOf 校验并给出这条通道发到哪张 TON 网络。
func tonNetworkOf(n *config.TonNativeConfig) (string, bool) {
	network := strings.TrimSpace(n.Network)
	if !isKnownTONNetwork(network) {
		vars.Info("TON 通道不启动: ton.native.network=%s 不是 TON 的网络取值（mainnet / testnet，留空按主网）", network)
		return "", false
	}
	if jetton := strings.TrimSpace(n.Jetton); jetton != "" {
		// 硬拒而不是忽略：配了 jetton 的人想发的是代币，静默按原生币发出去就是把钱
		// 发成了另一种资产。jetton 的小数位与账户模型都不同于原生币，而本包的 TON 出款
		// 是链上直发原生币，没有 jetton 的组装与查单实现，所以宁可不开通道。
		vars.Info("TON 通道不启动: ton.native.jetton=%s 指向 jetton 合约，本仓的 TON 通道只发原生 TON", jetton)
		return "", false
	}
	return network, true
}

// tonChainOptions 把 ton.native.chain 与通道段声明的网络合成链上接入点。
func tonChainOptions(c *config.TonChainConfig, network string) *chainConfig {
	if c == nil {
		return nil
	}
	return &chainConfig{
		HTTPBase:      strings.TrimSpace(c.HTTPBase),
		APIKey:        strings.TrimSpace(c.APIKey),
		WalletAddress: strings.TrimSpace(c.WalletAddress),
		WalletVersion: strings.TrimSpace(c.WalletVersion),
		SubWalletID:   c.SubWalletID,
		PrivateKey:    strings.TrimSpace(c.PrivateKey),
		Network:       network,
		FeeLimitSun:   c.FeeLimit,
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
func injectKey(prefix, key string, field *string) {
	if field == nil || key == "" {
		return
	}
	util.DefaultCallFunc.Do(prefix+key, field)
}

// TonWithdraw 直接上链出款：本地组外部消息、用热钱包私钥签名、经接入点广播。
//
// order.OrderNo 是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔，广播结果说不清时按外部消息 hash 查证并回 UNKNOWN（带 TxHash 供对账）。
func TonWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := tonNativeRoute.currentClient()
	if err != nil {
		return nil, err
	}
	return tonNativeRoute.orderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.Withdraw(ctx, tonNativeRoute.order(c, order))
	})
}

// TonQuery 按订单号查链上处置结果（台账里的消息 hash → 交易 → 执行阶段）。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func TonQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("TON Query 失败: 订单号（幂等键）为空")
	}
	c, err := tonNativeRoute.currentClient()
	if err != nil {
		return nil, err
	}
	return tonNativeRoute.orderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// TonAccount 查默认热钱包的链上余额（余额/可动用/状态）。
//
// TON 已删除供应商侧账户查询：本方法读的是公链上那个地址还剩多少 TON，
// 与 tron 的供应商额度口径不同，与 bsc/sol 的链上余额口径一致。
// 指定其他地址请走 pay.QueryOnchainAccountTo(pay.KindTON, q)。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次链上快照。
func TonAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	c, err := tonNativeRoute.currentClient()
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if strings.TrimSpace(q.Currency) == "" {
		cp := *q
		cp.Currency = pay.CurrencyTON
		q = &cp
	}
	if ctx == nil {
		ctx = tonNativeRoute.runCtx()
	}
	info, err := c.ch.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("TON 账户查询失败: %w", err)
	}
	return info, nil
}
