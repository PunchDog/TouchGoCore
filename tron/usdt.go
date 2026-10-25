package tron

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

// 本文件是 USDT-TRC20 侧的装配与门面。住在本包而不是 usdt 包的原因：
// 收款端校验与地址算法只有 TRON 一份（address.go），代币通道复用同一条链的规则，
// 登记点分两处就会在下游同时 blank-import 两个包时重名 panic（见 chainrule.go）。
// touchgocore/usdt 这个 import 路径已被下游写死，所以它退成薄转发留在原地。
//
// 门面实例 usdtRoute 与启停入口在 route.go；本文件只留装配入口 newUSDTClient、
// 三扇转发门面与它们的私有 helper。

// inlineUSDTKey 是密钥注入钩子键名。
//
// 键名直接取分派表里的种类（等价于配置段名 tron.usdt）：供应商字段已经在段内，
// 再另找一个名字当键名只会多一层对不上的可能；与 Kind 同源则连“两边各写一遍、
// 改一处忘一处”的路都不留。
const inlineUSDTKey = string(pay.KindTronUSDT)

// newUSDTClient 按 tron.usdt 的内联字段构造本包的 channelOptions 与 chainConfig。
//
// 报告口径与旧装配层逐条对齐：密钥必须在构造前注入、商户账户必须点得出来、
// 驱动名必须是本包登记的那一个。任何一条不成立都返回 ok=false——USDT 出款链路宁可不起，
// 也不能带着「不知道该从哪个商户号出钱」的快照上线。
func newUSDTClient(cfg *config.Cfg) (*chainroute.Snapshot, bool) {
	if cfg == nil || cfg.Tron == nil || cfg.Tron.Usdt == nil {
		vars.Info("USDT 通道不启动: 缺 tron.usdt 段")
		return nil, false
	}
	u := cfg.Tron.Usdt
	if !u.Enabled() {
		vars.Info("USDT 通道不启动: tron.usdt.enable 不是 on")
		return nil, false
	}
	contract, network, ok := usdtAsset(u)
	if !ok {
		return nil, false
	}
	s := &u.ChannelSupplierConfig
	driver := strings.TrimSpace(s.Driver)
	if driver != DriverTronGrid {
		// 驱动名必须自己把住：写错驱动名在这里放行，就等于“配置写着换实现、
		// 实际还是这一套”静默通过。
		vars.Info("USDT 通道不启动: tron.usdt.driver=%q 不是本包登记的链上驱动 %s", driver, DriverTronGrid)
		return nil, false
	}
	// 两把秘密分开注入（两个钩子前缀）：一次 Do 只交出一个 *string，
	// 共用键名会把密钥和私钥写进同一格。
	//
	// P6 起不再回落旧段名（supplier_a）：双读期为「部署脚本与二进制分开发版」留了一条活路，
	// 而保留它意味着同一个段有两个名字都能把密钥塞进来——密钥从哪来变成了猜的事。
	injectKey(util.CallPaySDKMsg, inlineUSDTKey, &s.SecretKey)
	if u.Chain != nil {
		injectKey(util.CallPayChainMsg, inlineUSDTKey, &u.Chain.PrivateKey)
	}
	// 商户账户是供应商侧查询的可选项：纯链上部署（余额走 QueryOnchainAccount）
	// 没有供应商账户，不能因为 accounts 为空就把整条通道拒掉。配了账户才要求它可解析。
	merchantID := ""
	baseURL := strings.TrimSpace(s.BaseURL)
	if len(s.Accounts) > 0 {
		acc, err := s.Account(strings.TrimSpace(s.AccountID))
		if err != nil {
			vars.Info("USDT 通道不启动: tron.usdt 的商户账户不可用: %v", err)
			return nil, false
		}
		merchantID = acc.MerchantID
	} else {
		// 没有商户账户就没有「查供应商额度」这条链路。即使误填了 base_url 也不装配
		// 供应商客户端：空商户号发出去的查询只会换来供应商侧的一串拒签日志。
		baseURL = ""
	}
	opt := channelOptions{
		Name:       "usdt",
		Driver:     driver,
		MerchantID: merchantID,
		BaseURL:    baseURL,
		Timeout:    secondsOf(s.TimeoutSec),
		MaxRetry:   s.MaxRetries,
		AppID:      strings.TrimSpace(s.AppID),
		SecretKey:  strings.TrimSpace(s.SecretKey),
		AuthHeader: strings.TrimSpace(s.AuthHeader),
		Endpoint:   s.Endpoint,
		Extras:     usdtExtras(contract),
		Chain:      inlineChainOptions(u.Chain, contract, network),
	}
	ch, err := newOnchainChannel(opt)
	if err != nil {
		vars.Info("USDT 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("USDT 通道已就绪: driver=%s 商户号=%s 合约=%s 网络=%s", driver, merchantID, contract, network)
	return &chainroute.Snapshot{Ch: ch, Network: network}, true
}

// usdtAsset 校验并给出这条通道发的是哪张合约、哪张网。
//
// 合约地址在这里就要过一遍格式：它是广播时的收端，写错等于把这一通道的每一单
// 都送进黑洞，而报错要等到第一笔出款之后。
func usdtAsset(u *config.TronUsdtConfig) (contract, network string, ok bool) {
	contract = strings.TrimSpace(u.Contract)
	if contract != "" && !IsValidTRONAddress(contract) {
		vars.Info("USDT 通道不启动: 配置的 TRC20 合约地址 %s 不合法（长度、版本字节或末尾 4 字节校验和对不上）", contract)
		return "", "", false
	}
	network = strings.TrimSpace(u.Network)
	if isTokenStandard(network) {
		// trc20 是「这张合约跑在 TRON 上」的标准名，不是网络名。混填进 network 的
		// 后果是换测试网时无从表达，而且供应商侧多半按主网口径处理。
		vars.Info("USDT 通道不启动: tron.usdt.network=%s 是代币标准而不是公链网络，网络取值用 mainnet / shasta / nile，合约在 tron.usdt.contract", network)
		return "", "", false
	}
	return contract, network, true
}

// usdtExtras 是 USDT 通道特有字段：它并进报文并追加到签名域末尾，
// 所以登记顺序由这里（唯一知道报文形态的一方）决定。
func usdtExtras(contract string) []extraField {
	if contract == "" {
		return nil
	}
	return []extraField{{Name: "contract", Value: contract}}
}

// inlineChainOptions 把 tron.usdt.chain 与通道段声明的合约/网络合成链上接入点。
func inlineChainOptions(c *config.TronChainConfig, contract, network string) *chainConfig {
	if c == nil {
		return nil
	}
	return &chainConfig{
		Node: strings.TrimSpace(c.Node),
		// TLS 只有写成 "off" 才降级为明文：grpc-go 拒绝「没声明传输安全」的拨号，
		// 这一项不能留给驱动去猜缺省。
		PlaintextNode: strings.EqualFold(strings.TrimSpace(c.TLS), "off"),
		HTTPBase:      strings.TrimSpace(c.HTTPBase),
		APIKey:        strings.TrimSpace(c.APIKey),
		WalletAddress: strings.TrimSpace(c.WalletAddress),
		PrivateKey:    strings.TrimSpace(c.PrivateKey),
		Contract:      contract,
		Network:       network,
		FeeLimitSun:   c.FeeLimit,
		Timeout:       secondsOf(c.TimeoutSec),
		MaxRetry:      c.MaxRetries,
	}
}

// secondsOf 把配置里的秒数换算成超时；<=0 交给驱动用缺省值。
// 不复用上层 timeout_sec 的语义差异：供应商 HTTP 与链上操作量级不同。
func secondsOf(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// injectKey 执行一次密钥注入：只认段名这一个键。
//
// 注入钩子是密钥进进程的唯一通道（配置里恒留空）。只认一个键名时，「密钥没注入」
// 与「注入到了别的键」不再混为一谈：前者通道不启动，后者同样不启动，日志里都指向同一个键。
func injectKey(prefix, key string, field *string) {
	if field == nil || key == "" {
		return
	}
	util.DefaultCallFunc.Do(prefix+key, field)
}

// isTokenStandard 判断 network 里填的是不是代币标准名。
//
// 只列这几个已被误用过的写法，不做「不在已知网络表里就拒」：供应商自己的网络叫法
// 我们没资格断言，硬拦会把能用的配置拦死。
func isTokenStandard(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "trc20", "trc-20", "trc10", "erc20", "bep20", "bep-20":
		return true
	}
	return false
}

// UsdtWithdraw 直接上链出款：本地组 TRC20 transfer、用热钱包私钥签名、经 gRPC 广播。
//
// order.OrderNo 仍然是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔，广播结果说不清时按 txid 查证并回 UNKNOWN（带 TxHash 供对账）。
// 收款地址与备注走 pay 的 ChainRule 唯一那一份校验，网络与合约不符本通道的单一律拒。
// 补单、回执广播与未启动拒绝的机器见 chainroute.Route.Withdraw。
func UsdtWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	return usdtRoute.Withdraw(ctx, order)
}

// UsdtQuery 按订单号查链上处置结果（台账里的 txid → 区块高度 → 确认数）。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func UsdtQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	return usdtRoute.Query(ctx, orderNo)
}

// UsdtAccount 查我方在该供应商名下的 USDT 商户账户（余额/可用/授信/费率/状态）。
//
// 注意这与链上热钱包余额是两件事：本方法读的是供应商侧的额度事实，
// 链上那个账户还有多少 USDT/TRX 只有出款前置闸会去看（见 onchain.go 的 precheckFunds）。
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次供应商侧的快照。
func UsdtAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	return usdtRoute.Account(ctx, q)
}
