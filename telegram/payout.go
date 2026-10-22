package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// 本文件是 TON 侧的装配与门面。实现住在 telegram 包而不是新建一个 ton 包：
// 收付入口在 Telegram 这一侧，配置也是 telegram.ton 一段；与 Bot 长连接的区别只在
// 凭证与失败路径，不在归属（口径与被删前的 telegram/ton.go 一致）。
//
// 出款不经供应商：本包按 telegram.ton 内联字段装配一条链上通道（telegram/ton_onchain.go），
// 提现单在本地签名后直接广播，结论从链上读回。
// 商户账户查询仍走同段的 HTTP 供应商（没配就没有这一项）。

var (
	// tonActive 为 nil 表示资金链路未启动，对应操作会就地拒绝而不是发出请求。
	// 复用 telegramRunCtx 作为运行上下文，不新建第二个包级 ctx 快照：
	// 两份快照会在 TelegramStart 与 TonStart 先后写入时互相漂移。
	tonActive atomic.Pointer[tonClient]
)

// tonClient 把资金通道与「哪张 TON 网络」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换网络，会出现用新网关发旧链的单子。
type tonClient struct {
	ch      tonChainChannel
	network string
}

// tonChainChannel 是本包对「一条链上资金通道」的全部要求。
//
// 它曾经是 pay.Channel——住在契约层、被三家通道共用。那张接口的 Driver/MerchantID
// 两个方法只服务于驱动注册表：表没了，它们就只剩「接口上多两个没人调的方法」。
// 现在接口收在这里，宽度正好是本包门面真正要调的四件事。
type tonChainChannel interface {
	Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error)
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
	QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error)
	Close() error
}

// tonNetworkLabel 是配置里允许的 TON 网络名。
//
// 只认 pay 的规范名（mainnet / testnet，留空按主网），刻意不接受 TonAPI 自己回显的
// "testnet-v4" 这类写法：pay.IsTestnet 不认识它，收下来就等于配置说测试网、
// 校验按主网走——测试网地址会被判成「与 network 不符」，而这条链的闸正是防这个的。
// 也别把 shasta/nile（TRON 的测试网）或 ton/jetton（链名与代币标准）当网络收进来：
// 填错的人在配置里看不出问题，等第一笔出款才看得见。
func isKnownTONNetwork(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "", pay.NetworkMainnet, pay.NetworkTestnet:
		return true
	}
	return false
}

// TonStart 按 telegram.ton 的引用装配 TON 资金链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// Bot 能不能起来与钱包通道配没配好互不牵制，两边都不该阻断整机启动。
func TonStart(ctx context.Context) {
	if ctx == nil {
		ctx = runCtx()
	}
	setRunCtx(ctx)
	if c, ok := newTonClient(corectx.CfgFrom(ctx)); ok {
		tonActive.Store(c)
		bindTransfer(c)
		return
	}
	vars.Info("不启动TON")
}

// bindTransfer 把这条已装配的通道登记进 pay 的分派表（telegram.ton）。
//
// 先 Unbind 再 Bind：Start 可被调用两次（管线重启路径、测试里的交叉用例），
// 而 tonActive 已被新快照换掉。分派表若还指着上一条，出款会走一条已经 Close 的通道。
func bindTransfer(c *tonClient) {
	if c == nil {
		return
	}
	pay.Unbind(pay.KindTON)
	if err := pay.Bind(pay.KindTON, c.ch); err != nil {
		vars.Info("TON 通道登记分派失败: %v", err)
	}
}

// TonStop 摘掉 TON 资金通道并释放接入点。可重复调用。
func TonStop(ctx context.Context) {
	// 先摘分派再关连接：反过来的话，正在分派的那一单会拿到一条正在关的通道，
	// 报出来的是「连接已关闭」而不是「通道未启动」。
	pay.Unbind(pay.KindTON)
	c := tonActive.Swap(nil)
	if c == nil {
		return
	}
	// 链上通道背后是带超时的 HTTP 客户端与连接池：只清空指针的话，
	// 第二次 start 就多一条不会关的连接，停启几次就把进程挂在泄漏的 socket 上。
	// Close 已经收进 tonChainChannel，所以这里直接调。
	if err := c.ch.Close(); err != nil {
		vars.Info("TON 通道停服时释放连接失败: %v", err)
	}
}

// currentTon 取资金链路；未启动时返回明确错误而不是 panic。
func currentTon() (*tonClient, error) {
	if c := tonActive.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("TON 通道未启动（检查 telegram.ton.enable 是否为 on、chain 与商户账户是否齐备）")
}

// inlineTONKey 是密钥注入钩子键名；键名直接取分派表里的种类（等价于配置段名
// telegram.ton）——供应商字段已经在段内，再另找一个名字当键名只会多一层对不上的可能。
const inlineTONKey = string(pay.KindTON)

// newTonClient 按 telegram.ton 的内联字段构造本包的 channelOptions 与 chainConfig。
//
// 报告口径与旧装配层逐条对齐：密钥必须在构造前注入、驱动名必须是本包登记的那一个、
// 商户账户必须点得出来。任何一条不成立都返回 ok=false——出款链路宁可不起，
// 也不能带着「不知道该从哪个商户号出钱」的快照上线。
func newTonClient(cfg *config.Cfg) (*tonClient, bool) {
	if cfg == nil || cfg.Telegram == nil || cfg.Telegram.Ton == nil {
		vars.Info("TON 通道不启动: 缺 telegram.ton 段")
		return nil, false
	}
	tg := cfg.Telegram.Ton
	network, ok := tonNetworkOf(tg)
	if !ok {
		return nil, false
	}
	if !tg.Enabled() {
		vars.Info("TON 通道不启动: telegram.ton.enable 不是 on")
		return nil, false
	}
	s := &tg.ChannelSupplierConfig
	driver := strings.TrimSpace(s.Driver)
	if driver != DriverTonAPI {
		// 驱动名必须自己把住：写错驱动名在这里放行，就等于“配置写着换实现、
		// 实际还是这一套”静默通过。
		vars.Info("TON 通道不启动: telegram.ton.driver=%q 不是本包登记的链上驱动 %s", driver, DriverTonAPI)
		return nil, false
	}
	// 两把秘密分开注入（两个钩子前缀）：一次 Do 只交出一个 *string。
	// P6 起不再回落旧段名（supplier_ton）：同一个段有两个名字都能把密钥塞进来，
	// 密钥从哪来就成了猜的事。
	injectKey(util.CallPaySDKMsg, inlineTONKey, &s.SecretKey)
	if tg.Chain != nil {
		injectKey(util.CallPayChainMsg, inlineTONKey, &tg.Chain.PrivateKey)
	}
	acc, err := s.Account(strings.TrimSpace(s.AccountID))
	if err != nil {
		vars.Info("TON 通道不启动: telegram.ton 的商户账户不可用: %v", err)
		return nil, false
	}
	opt := channelOptions{
		Name:       "ton",
		Driver:     driver,
		MerchantID: acc.MerchantID,
		BaseURL:    strings.TrimSpace(s.BaseURL),
		Timeout:    secondsOf(s.TimeoutSec),
		MaxRetry:   s.MaxRetries,
		AppID:      strings.TrimSpace(s.AppID),
		SecretKey:  strings.TrimSpace(s.SecretKey),
		AuthHeader: strings.TrimSpace(s.AuthHeader),
		Endpoint:   s.Endpoint,
		// 本通道只发原生 TON，没有通道特有字段（jetton 已在 tonNetworkOf 里硬拒）。
		Chain: tonChainOptions(tg.Chain, network),
	}
	ch, err := newTonOnchainChannel(opt)
	if err != nil {
		vars.Info("TON 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("TON 通道已就绪: driver=%s 商户号=%s 网络=%s", driver, acc.MerchantID, network)
	return &tonClient{ch: ch, network: network}, true
}

// tonNetworkOf 校验并给出这条通道发到哪张 TON 网络。
func tonNetworkOf(tg *config.TonChannelConfig) (string, bool) {
	network := strings.TrimSpace(tg.Network)
	if !isKnownTONNetwork(network) {
		vars.Info("TON 通道不启动: telegram.ton.network=%s 不是 TON 的网络取值（mainnet / testnet，留空按主网）", network)
		return "", false
	}
	if jetton := strings.TrimSpace(tg.Jetton); jetton != "" {
		// 硬拒而不是忽略：配了 jetton 的人想发的是代币，静默按原生币发出去就是把钱
		// 发成了另一种资产。jetton 的小数位与账户模型都不同于原生币，而本包的 TON 出款
		// 是链上直发原生币，没有 jetton 的组装与查单实现，所以宁可不开通道。
		vars.Info("TON 通道不启动: telegram.ton.jetton=%s 指向 jetton 合约，本仓的 TON 通道只发原生 TON", jetton)
		return "", false
	}
	return network, true
}

// tonChainOptions 把 telegram.ton.chain 与通道段声明的网络合成链上接入点。
//
// 合成口径逐字段对着 config.TonChainConfig：字段名两处能对上，读配置的人
// 不必在配置与装配之间做一次翻译。
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
//
// 注入钩子是密钥进进程的唯一通道（配置里恒留空）。只认一个键名时，「密钥没注入」
// 与「注入到了别的键」不再混为一谈：前者通道不启动，后者同样不启动，日志里都指向同一个键。
func injectKey(prefix, key string, field *string) {
	if field == nil || key == "" {
		return
	}
	util.DefaultCallFunc.Do(prefix+key, field)
}

// tonPublish 把归一后的资金回执广播给下游，键为 util.CallTonMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func tonPublish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallTonMsg+kind, res); !ok {
		vars.Debug("TON 回执无人订阅: kind=%s order_no=%s", kind, res.OrderNo)
	}
}

// tonOrderCall 是下单/查单门面共用的收尾：调用→补通道前缀→广播回执。
//
// ctx 为 nil 时回落到包级运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本包的留痕口径。
func tonOrderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("TON %s 失败: %w", kind, err)
	}
	tonPublish(kind, res)
	return res, nil
}

// tonOrder 把链路配置补进订单里未指定的字段。
//
// 本通道只管 TON：调用方漏填币种时补成 TON，而不是发出一张「币种为空」的单。
// 网络补在 To.Network 上而不是订单顶层：网络是收款端那一侧的属性。
// 复制（含 To 深拷贝）而不是就地改：调用方交来的 PayOrder 可能还要用于重试与落库，
// 门面把它改了就等于替上游决定了「这一单实际发出去的是什么」。
func tonOrder(c *tonClient, o *pay.PayOrder) *pay.PayOrder {
	if o == nil {
		return nil
	}
	network := ""
	if o.To != nil {
		network = strings.TrimSpace(o.To.Network)
	}
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = pay.CurrencyTON
	}
	if o.To == nil {
		if currency == o.Currency {
			return o
		}
		cp := *o
		cp.Currency = currency
		return &cp
	}
	if network == o.To.Network && currency == o.Currency {
		return o
	}
	cp := *o
	cp.Currency = currency
	to := *o.To
	to.Network = network
	cp.To = &to
	return &cp
}

// TonWithdraw 直接上链出款：本地组外部消息、用热钱包私钥签名、经接入点广播。
//
// order.OrderNo 是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔，广播结果说不清时按外部消息 hash 查证并回 UNKNOWN（带 TxHash 供对账）。
// 收款地址与备注走本包的 checkOrderAddress 唯一那一份校验，网络与币种不符本通道的单一律拒。
func TonWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.Withdraw(ctx, tonOrder(c, order))
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
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// TonAccount 查我方在该供应商名下的 TON 商户账户（余额/可用/授信/费率/状态）。
//
// 注意这与链上热钱包余额是两件事：本方法读的是供应商侧的额度事实，
// 链上那个账户还有多少 TON 只有出款前置闸会去看（见 ton_onchain.go 的 precheckFunds）。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次供应商侧的快照。
func TonAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	c, err := currentTon()
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
		ctx = runCtx()
	}
	info, err := c.ch.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("TON 账户查询失败: %w", err)
	}
	return info, nil
}
