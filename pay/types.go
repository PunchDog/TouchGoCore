// Package pay 是资金通道（提现）的纯契约层：订单与回执的形状、收款方与账户的
// 校验口径，以及一张「哪条通道已经起来了」的分派表。
//
// 分层口径：本包只认「一单资金请求长什么样、该交给谁」，不碰 SQL、不碰业务积分、
// 不定义配置结构体（它们与 Cfg 同住 config 包），也没有任何传输与装配实现——
// HTTP 客户端已下沉到 internal/httpx，供应商与链上的装配归各通道包。
// 落库、额度校验、状态机跃迁都由下游业务工程负责。
// 「不引 config、不引任何通道包」是包级边界：这条边界一破，契约层就会重新
// 长出「按配置构造实现」的能力，分派与装配的先后顺序也就无从保证。
package pay

import (
	"errors"
	"fmt"
	"strings"
)

// 币种标识。代码内统一用这几个大写名字，与目录名 usdt 一致。
//
// CurrencyTRX 不是本仓的出款币种，它是手续费侧的事实：TRON 上转 USDT，
// gas 是以 TRX 收的，与转账币种不同一种。
// CurrencyBNB / CurrencySOL 是另两条链的原生币：BNB 是 BSC（EVM 形态）的 gas 币，
// SOL 是 Solana 的 gas 币（lamports，9 位最小单位）。
const (
	CurrencyUSDT = "USDT"
	CurrencyTON  = "TON"
	CurrencyTRX  = "TRX"
	CurrencyBNB  = "BNB"
	CurrencySOL  = "SOL"
)

// 公链网络标识。主网与测试网的区别必须是配置里看得出来的一个字段值，
// 不能靠基址猜：同一家供应商的测试网网关往往连着同一套报文格式，
// 把主网单发进测试网关会「调用成功、钱没动」，比报错更难查。
const (
	NetworkMainnet = "mainnet"
	// NetworkTestnet 是通用测试网名，TON 测试网用它。
	NetworkTestnet = "testnet"
	// NetworkShasta 与 NetworkNile 是 TRON 的两条测试网。
	NetworkShasta = "shasta"
	NetworkNile   = "nile"
	// NetworkChapel 是 BSC 的测试网（旧名 Chapel、现名 BSC Testnet 同源），
	// 沿「链自带专名就单独登记」的 TRON 口径，不用通用的 testnet 顶替。
	NetworkChapel = "chapel"
	// NetworkDevnet 是 Solana 的测试网名——它不叫 testnet，
	// 供应商按各自的链生态登记，混填会让测试单进了主网网关。
	NetworkDevnet = "devnet"
)

// Deprecated: NetworkTRC20 把代币标准当成了网络。TRC20 是「USDT 这张合约跑在
// TRON 主网上」的意思，网络取值应该是 mainnet；测试网则是 shasta 或 nile。
// 合约地址另有 usdt.contract 一列承载。网络和合约是两个正交维度，混进一个字段
// 就等于「换测试网」和「换代币」没法分别表达。
const NetworkTRC20 = "trc20"

// IsTestnet 该网络标识是否算测试网。
//
// 认不出的值按主网处理，这个方向是安全的：主网口径会把带测试网标记的地址判成
// 「与网络不符」而拒单，反之把没看懂的值当成测试网，才会让测试网地址混进主网报文。
func IsTestnet(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case NetworkTestnet, NetworkShasta, NetworkNile, NetworkChapel, NetworkDevnet:
		return true
	default:
		return false
	}
}

// 资金单状态。
//
// 四态里只有 SUCCESS/FAILED 是终态。PENDING 是「供应商已受理、结果未出」，
// UNKNOWN 是「我们没能确定供应商到底收没收到这一单」——两者的处置差别在于：
// PENDING 可以去查，UNKNOWN 既不能自动重下单也不能判定失败，必须留给对账。
const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusUnknown = "unknown"
)

// IsFinal 该状态是否已是终态（PENDING/UNKNOWN 都不是）。
func IsFinal(status string) bool {
	return status == StatusSuccess || status == StatusFailed
}

// 端点逻辑名。供应商路径写在配置的 endpoints 里，键就是这几个名字：
// 代码里固定逻辑名、配置里填实际路径，换供应商只改配置不改代码。
//
// 登录/好友列表类端点（send_code / login / friend_list）已随 facebook、instagram
// 两条 login 链路整包删除；出款全部上链后 withdraw 端点也不再由契约层定义。
// 留在契约层的只剩两条通道今天仍在用的：query 与 account。
const (
	EndpointQuery = "query"
	// EndpointAccount 是查商户账户的逻辑名。它排在下单之前是有原因的：
	// 提现前先看一眼可用余额与账户状态，比把单发出去再等供应商拒要省一次资金动作。
	EndpointAccount = "account"
)

// 商户账户状态。
//
// 同样只有两种确定态加一种兜底：ACTIVE 之外一律不许发起出款，
// 读不懂的状态词算 UNKNOWN 而不是 ACTIVE——把「没看懂」当成「账户正常」，
// 等于给一个被冻结的商户号继续送单。
const (
	AcctActive  = "active"
	AcctFrozen  = "frozen"
	AcctClosed  = "closed"
	AcctUnknown = "unknown"
)

// NormalizeAccountStatus 把供应商账户状态词映射到上面四态，映射不到给 UNKNOWN。
func NormalizeAccountStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "ok", "active", "normal", "enabled", "online", "valid":
		return AcctActive
	case "2", "frozen", "freeze", "locked", "suspend", "suspended", "disabled", "offline":
		return AcctFrozen
	case "3", "closed", "cancel", "cancelled", "deleted", "quit":
		return AcctClosed
	case "0", "pending", "processing", "waiting":
		return AcctUnknown
	default:
		return AcctUnknown
	}
}

// IsActive 账户是否处于「可以发起资金动作」的状态。
func IsActive(status string) bool { return status == AcctActive }

// PayOrder 是一次提现请求的全部输入。
//
// 收付两端结构体化（见 party.go）：Address / Network / Memo 三个平铺字段已整条删除，
// 改由 To（必填）与 From（可空＝用通道配置的默认出款方）承载，不保留回落。
//
// Amount 是币种最小单位的整数值，全链路禁用浮点：一旦中途出现 float64，大额出款
// 就会在元/分转换上丢精度。最小单位是几位**由那一单实际走的合约决定**，不由包名决定：
// USDT-TRC20 是 6 位，原生 TON 是 9 位 nanoton，而 jetton 的小数位数写在该代币自己的
// 元数据里（TEP-64）——它可以是 6、8，也可以是别的。位数取错的后果是 10^n 倍的
// 不可逆多付，所以本包只按 int64 最小单位原样透传，指数的解释权留给填
// contract / jetton 的那一方（以及它对着的那份代币文档）。
type PayOrder struct {
	// OrderNo 是业务侧订单号，同时是透传给供应商的幂等键：
	// 同一单二次提交不得产生第二笔资金动作。一旦生成不得变更。
	OrderNo  string `json:"order_no"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency,omitempty"`
	// To 是收款方，必填（见 Check）。Kind 留空时按 Bank / Address / Account 推断，
	// 推断规则与各性质的必填项见 party.go。
	To *Party `json:"to"`
	// From 是出款方。留空表示「用本通道配置的默认出款方」——链上通道是热钱包地址，
	// 供应商账本通道是付款商户号。填了就必须与配置一致：链上指定非热钱包地址即拒单，
	// 因为本仓没有第二个钱包的私钥，那样的单只会在签名那一步失败。
	//
	// 例外是按单携带完整出款参数的链上通道：From 可以带 Address + PrivateKey +
	// Contract（+ Network），通道用这一单的私钥签名并逐字校验派生地址。当前由
	// tron 的 USDT 通道消费；其他通道仍按「只认配置热钱包」处理。
	From  *Party `json:"from,omitempty"`
	Phone string `json:"phone,omitempty"`
	// Decimals 是 Amount 这个整数对应的小数位数，0 表示「不声明」。
	//
	// 它不是从 Currency 推出来的：同一个 USDT 在 TRON 是 6 位、在 BSC 是 18 位、
	// 在 SOL 是 6 位，币种名相同而最小单位不同。填了就必须与通道配置的 decimals
	// 逐字相等，否则拒单——位数取错的后果是 10^n 倍的不可逆多付。
	Decimals int `json:"decimals,omitempty"`
	// Code 是登录验证码明文。它不得进入日志、错误文案或 PayResult.RawNote。
	Code string `json:"code,omitempty"`
	// Extra 放通道特有字段（如 remark、供应商自定义的回调键名）。值同样不得含凭证。
	//
	// 契约层不消费它：拼报文与算签名是各通道自己的事（见 tron/account.go 的
	// mergeExtraFields）。这里只负责把「这一单带哪些额外键值」作为订单的一部分
	// 透传下去，无特有字段时不出现在报文里，避免供应商把 null 当成显式清空指令。
	Extra map[string]string `json:"extra,omitempty"`
}

// PayResult 是供应商回执的归一形态。
type PayResult struct {
	OrderNo string `json:"order_no"`
	// TradeNo 是供应商侧流水号，落库用于对账；空串按「供应商未给出」处理。
	TradeNo string `json:"trade_no,omitempty"`
	// Status 取值见上方四个状态常量。
	Status string `json:"status"`
	Amount int64  `json:"amount"`
	// TxHash 是链上交易哈希。供应商代我们广播时它才是「钱真的动了」的那条证据；
	// 账本划转类通道（whatsapp）留空。空哈希不表示失败，表示这家供应商没回。
	TxHash string `json:"tx_hash,omitempty"`
	// Fee 是本单实际扣的手续费，币种最小单位整数。
	//
	// 它与 AccountInfo.FeeRateBps 是两件不同的事，不能互相换算：费率是万分比、
	// 按金额比例收，而链上 gas 是**固定一笔、以那条链的原生币收**——
	// 转 USDT 收的是 TRX，转 jetton 收的是 TON，且与转账金额无关
	// （同一个 TRC20 transfer，收款地址是否活跃能让 gas 差一倍）。
	Fee int64 `json:"fee,omitempty"`
	// FeeCurrency 是 Fee 的计价币种，可能不同于本单币种（见上）。
	// 供应商给了 Fee 却没给币种时按本单币种兜底，兜底理由见 Provider.Result。
	FeeCurrency string `json:"fee_currency,omitempty"`
	// Confirmations 是链上确认数。nil 表示供应商没回这个字段，
	// 与「回了一个 0」必须区分开：0 是「交易已进块但还没确认」，
	// 上游据此该继续查；没回则是「这家供应商不提供」，据此查也查不出东西。
	Confirmations *int64 `json:"confirmations,omitempty"`
	// RawNote 是可公开给日志的响应摘要。凭证、验证码、私钥一律不得出现。
	RawNote string `json:"raw_note,omitempty"`
}

// IsFinal 回执是否已是终态。
func (r *PayResult) IsFinal() bool {
	return r != nil && IsFinal(r.Status)
}

// AccountQuery 是查商户账户的输入。这里的「商户账户」指我方在供应商那边的账户，
// 不是会员账户——余额、可用额度、授信、费率都是供应商侧的事实，本仓只负责读回来。
//
// 字段留空即「不附加该过滤条件」：MerchantID 空用通道配置里的默认商户号，
// Currency 空按供应商自己的口径返回（可能是全部账户汇总，文档到位后在此写死语义）。
type AccountQuery struct {
	MerchantID string `json:"merchant_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
}

// AccountInfo 是商户账户的归一快照。
//
// 金额一律 int64 最小单位、费率万分比整数：可用余额将直接参与「能不能提这么多」
// 的判断，中途出现 float64 就是给资金判断引入误差。
type AccountInfo struct {
	MerchantID string `json:"merchant_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
	// Balance 账户总额，Available 可动用（提现）额，Frozen 冻结额。
	Balance   int64 `json:"balance"`
	Available int64 `json:"available"`
	Frozen    int64 `json:"frozen"`
	// Credit 是供应商给的我方授信额度；没有授信概念的供应商返回 0。
	Credit int64 `json:"credit"`
	// FeeRateBps 手续费率，万分比整数（30 = 0.30%）。
	//
	// 它只对「按比例抽佣」的账本划转有意义。链上那一类通道别拿它预估成本：
	// gas 是固定一笔、以链原生币收的（TRC20 收 TRX、jetton 收 TON），
	// 单笔实际扣多少只有回执里的 PayResult.Fee 知道。
	FeeRateBps int64 `json:"fee_rate_bps"`
	// Status 取值见 Acct* 常量；读不懂一律 UNKNOWN，不当成正常。
	Status string `json:"status"`
	// RawNote 是可公开给日志的摘要，不含凭证。
	RawNote string `json:"raw_note,omitempty"`
}

// CanWithdraw 判断该账户当前能否发起一笔 amount 的出款。
//
// 只作为「发出去之前少挨一次拒」的前置判断，不作为额度账本：真正的额度
// 与幂等在下游业务侧落库。账户状态非 ACTIVE 时直接否，UNKNOWN 也否——
// 没看清的账户不该往外送钱。
//
// 它看的是「这一单的币种够不够」，看不到链上 gas：TRON 与 TON 的 gas 收在原生币
// （TRX / TON）上，是另一个账户。USDT 余额充足而 TRX 为 0 时本方法照样点头，
// 供应商会在广播那一步报 OUT_OF_ENERGY——而且 TRX 已经烧掉了。
func (a *AccountInfo) CanWithdraw(amount int64) bool {
	if a == nil || amount <= 0 || !IsActive(a.Status) {
		return false
	}
	return a.Available >= amount
}

// ProviderError 携带供应商错误码，把「我们这侧拒了」与「供应商拒了」分开看。
//
// Error() 的文案只含通道名、错误码与供应商消息，绝不含 AppID/SecretKey/token；
// 这三项一旦进 error 就会顺着日志和上层包装扩散到不可控的出口。
type ProviderError struct {
	// Channel 是通道名（whatsapp/usdt/ton），只用于定位，不参与判定。
	Channel string
	// Code 是供应商错误码，逐字保留，不做映射。
	Code string
	Msg  string
	// HTTPStatus 是传输层状态码；业务码在包络里时这里为 0。
	HTTPStatus int
	// Retryable 标记本次失败可否原单重发。由调用方（各通道的 Parse）判定：
	// 「余额不足」这类确定性业务失败必须为 false，否则重试只是重复挨拒。
	Retryable bool
}

func (e *ProviderError) Error() string {
	if e.Channel != "" {
		return fmt.Sprintf("%s 通道返回失败 code=%s: %s", e.Channel, e.Code, e.Msg)
	}
	return fmt.Sprintf("通道返回失败 code=%s: %s", e.Code, e.Msg)
}

// IsProviderRetryable 错误是否为「标记过可重试」的供应商错误。
func IsProviderRetryable(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// ProviderErrorOf 取出错误链上的供应商错误；没有则返回 nil, false。
func ProviderErrorOf(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
