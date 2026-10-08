// Package nft 是 NFT 资产通道（查询持有/单币详情/铸造/转账/对账）的共用契约与出站客户端。
//
// 分层口径同 pay：不碰 SQL、不碰积分、不定义供 config 引用的配置结构体，落库归下游业务。
package nft

import (
	"errors"
	"fmt"
	"strings"
)

// 链标识。与目录名、供应商口径一致的规范小写名。
//
// 链与网络是两个正交维度：链决定「资产跑在哪条公链」，网络决定「主网还是测试网」。
// 混进一个字段就等于「换链」和「换网络」没法分别表达（pay.NetworkTRC20 的教训不继承）。
const (
	ChainETH  = "eth"
	ChainBSC  = "bsc"
	ChainTRON = "tron"
	ChainSOL  = "sol"
	ChainTON  = "ton"
)

// 公链网络标识。主网与测试网的区别必须是配置里看得出来的一个字段值，不能靠基址猜：
// 同一家供应商的测试网网关往往连着同一套报文格式，把主网单发进测试网关会
// 「调用成功、资产没动」，比报错更难查。
const (
	NetworkMainnet = "mainnet"
	// NetworkTestnet 是通用测试网名，TON 测试网用它。
	NetworkTestnet = "testnet"
	// NetworkChapel 是 BSC 的测试网（旧名 Chapel、现名 BSC Testnet 同源），
	// 沿「链自带专名就单独登记」的口径，不用通用的 testnet 顶替。
	NetworkChapel = "chapel"
	// NetworkDevnet 是 Solana 的测试网名——它不叫 testnet，
	// 供应商按各自的链生态登记，混填会让测试单进了主网网关。
	NetworkDevnet = "devnet"
)

// IsTestnet 该网络标识是否算测试网。
//
// 认不出的值按主网处理，这个方向是安全的：主网口径会把带测试网标记的地址判成
// 「与网络不符」而拒单，反之把没看懂的值当成测试网，才会让测试网地址混进主网报文。
func IsTestnet(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case NetworkTestnet, NetworkChapel, NetworkDevnet:
		return true
	default:
		return false
	}
}

// 代币标准标识。标准与链/网络是第三个正交维度（ERC721/ERC1155/TRC721）。
const (
	StandardERC721  = "erc721"
	StandardERC1155 = "erc1155"
	StandardTRC721  = "trc721"
)

// 资产单状态。
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
const (
	EndpointHoldings = "holdings" // 查询持有
	EndpointToken    = "token"    // 查询单币详情
	EndpointMint     = "mint"     // 铸造提交
	EndpointTransfer = "transfer" // 转账提交
	EndpointQuery    = "query"    // 查单/对账
)

// NftOrder 是一次铸造或转账提交的完整输入（对位 pay.PayOrder）。
//
// 数量禁用浮点：Quantity 是 int64 件数；TokenID 是十进制字符串——EVM 的 tokenId
// 是 uint256，int64 装不下，转 float64 更是精度事故（硬约束 5）。
type NftOrder struct {
	// OrderNo 是业务侧订单号，同时是透传给供应商的幂等键：
	// 同一单二次提交不得产生第二笔资产动作。一旦生成不得变更（本地闸见 §3.4）。
	OrderNo string `json:"order_no"`
	Chain   string `json:"chain,omitempty"`   // 链标识，取值见 Chain* 常量
	Network string `json:"network,omitempty"` // 网络标识，取值见 Network* 常量；留空由装配层补默认
	// Contract 是代币合约地址。Mint：留空=由供应商部署新合约，非空=铸到既有合约；
	// Transfer：必填——转的是已铸成的币，没有合约无从定位资产域。
	Contract string `json:"contract,omitempty"`
	Standard string `json:"standard,omitempty"` // 取值见 Standard* 常量；供应商可按自家口径回填
	// TokenID 是十进制大整数字符串。Mint 留空（铸出后由回执回填）；Transfer 必填。
	TokenID string `json:"token_id,omitempty"`
	// FromAddress 是签发/转出地址（运营方托管地址）。Mint 可留空由供应商侧账户出；
	// Transfer 留空按装配层快照的默认主体，不允许「不知道从谁名下转」。
	FromAddress string `json:"from_address,omitempty"`
	// ToAddress 是接收地址。Mint/Transfer 都必须非空——钱进错地址还能追回，
	// NFT 进了无人持有的地址只剩人工找回。
	ToAddress string `json:"to_address,omitempty"`
	// Quantity 是件数：ERC721 恒为 1，ERC1155 类可批量。int64，禁浮点。
	Quantity int64 `json:"quantity"`
	// MetadataURI 是资产元数据链接（铸造侧的资产描述唯一书面输入）。
	// 明文可以进报文，但不得进日志与 RawNote（可能含未公开资产信息）。
	MetadataURI string `json:"metadata_uri,omitempty"`
	// Memo 是链上备注。不得进日志与 RawNote（口径对 pay.PayOrder Memo）。
	Memo string `json:"memo,omitempty"`
	// Extra 放通道特有字段。值不得含凭证。键值并进报文同一层，并按 key 字典序
	// 追加到签名域末尾（mergeExtras 同一次遍历产出，对 pay/provider.go:265-365 同构）。
	// 键名不得与已签名量同字（app_id/merchant_id/order_no/chain/network/contract/
	// standard/token_id/from_address/to_address/quantity/metadata_uri/memo/notify_url），
	// 命中即拒单——能覆盖已签名字段的逃生口等于给签名域开后门。
	Extra map[string]string `json:"extra,omitempty"`
}

// NftResult 是供应商回执的归一形态（对位 pay.PayResult）。
type NftResult struct {
	OrderNo string `json:"order_no"`
	// TradeNo 是供应商侧流水号，落库用于对账；空串按「供应商未给出」处理。
	TradeNo string `json:"trade_no,omitempty"`
	// Status 取值见 Status* 四态常量。
	Status string `json:"status"`
	// TokenID 是铸造成功回执里铸出的代币号（十进制字符串）；转账/查单回执原样带回。
	TokenID string `json:"token_id,omitempty"`
	// TxHash 是链上交易哈希——资产「真的动了」的那条证据。空不表示失败，表示没回。
	TxHash string `json:"tx_hash,omitempty"`
	// Quantity 是实际处置件数，与请求对账用（§3.3）。
	Quantity int64 `json:"quantity,omitempty"`
	// GasFee 是本次实际扣的手续费，int64 链上原生币最小单位。
	GasFee int64 `json:"gas_fee,omitempty"`
	// GasFeeToken 是 GasFee 的计价代币标识（"ETH"/"BNB"/"TRX"/"SOL"…）。
	// 它是字符串值不是 import——与 gas 侧「手续费币种不同于动作资产」的事实一致，
	// 口径对 pay.PayResult.FeeCurrency。
	GasFeeToken string `json:"gas_fee_token,omitempty"`
	// Confirmations 是链上确认数。nil=供应商没回，0=已进块未确认，必须区分
	// （对 pay.PayResult.Confirmations 同构）。
	Confirmations *int64 `json:"confirmations,omitempty"`
	// RawNote 是可公开给日志的响应摘要（超 64 rune 由 clip 截断）。
	// 凭证、助记词、私钥、会话 token、MetadataURI 一律不得出现（脱敏面比 pay 多
	// 后两项：NFT 供应商常见「助记词代管」形态，泄露即资产归零）。
	RawNote string `json:"raw_note,omitempty"`
}

// IsFinal 回执是否已是终态。
func (r *NftResult) IsFinal() bool { return r != nil && IsFinal(r.Status) }

// HoldingsQuery 是按地址查名下 NFT 的输入。留空=不加该过滤条件。
type HoldingsQuery struct {
	// Address 是持有者地址。必填——不接受「查全部」，那会把别的会员的资产
	// 清单从错误分支带进日志。
	Address string `json:"address"`
	Chain   string `json:"chain,omitempty"`
	Network string `json:"network,omitempty"`
	// Contract 非空时只查这一个合约（nft 侧「币种」过滤）。
	Contract string `json:"contract,omitempty"`
	PageNo   int64  `json:"page_no,omitempty"`   // 1 起；0 按 1
	PageSize int64  `json:"page_size,omitempty"` // 0 按供应商默认；上限由驱动 clamp
}

// HoldingItem 是名下一笔持有。
type HoldingItem struct {
	TokenID  string `json:"token_id"` // 十进制大整数字符串
	Contract string `json:"contract,omitempty"`
	Chain    string `json:"chain,omitempty"`
	Network  string `json:"network,omitempty"`
	Standard string `json:"standard,omitempty"`
	Quantity int64  `json:"quantity"` // ERC721 恒 1；禁浮点
}

// HoldingsResult 是持有清单的归一快照。
type HoldingsResult struct {
	Address string        `json:"address"`
	Items   []HoldingItem `json:"items"`
	// Total 是供应商口径的总条数（分页前），没回则 0——0 与「恰好 0 条」
	// 由 len(Items) 区分，上游别拿 Total 判空。
	Total   int64  `json:"total,omitempty"`
	RawNote string `json:"raw_note,omitempty"` // 脱敏口径同 NftResult.RawNote
}

// TokenQuery 是按 (chain, network, contract, token_id) 定位单币的输入。
// 四元组缺一即拒（CheckTokenQuery 在 provider 侧，四元组是资产的唯一坐标，
// 少一维就是把 A 链的 tokenId 拿去问 B 链）。
type TokenQuery struct {
	Chain    string `json:"chain"`
	Network  string `json:"network,omitempty"` // 留空按装配层快照默认网络
	Contract string `json:"contract"`
	TokenID  string `json:"token_id"` // 十进制大整数字符串
}

// TokenInfo 是单币详情的归一快照。大整数字段一律字符串口径（TotalSupply/Balance）。
type TokenInfo struct {
	Chain       string `json:"chain,omitempty"`
	Network     string `json:"network,omitempty"`
	Contract    string `json:"contract,omitempty"`
	TokenID     string `json:"token_id"`
	Standard    string `json:"standard,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// ImageURI / MetadataURI 是公开资源链接，可进日志（不是凭证）；
	// 但供应商回退的内部临时签名 URL 可能被驱动改写为 "<redacted>"。
	ImageURI    string `json:"image_uri,omitempty"`
	MetadataURI string `json:"metadata_uri,omitempty"`
	// Owner 是 ERC721 持有权地址；ERC1155 无单一 owner，留空。
	Owner string `json:"owner,omitempty"`
	// TotalSupply 是十进制大整数字符串（uint256 口径，禁 float64/禁 int64——
	// 供应量上限可超 int64，字符串是唯一无损形态）。
	TotalSupply string `json:"total_supply,omitempty"`
	// RawNote 可公开摘要，不含凭证。
	RawNote string `json:"raw_note,omitempty"`
}

// ProviderError 携带供应商错误码，把「我们这侧拒了」与「供应商拒了」分开看。
//
// Error() 的文案只含通道名、错误码与供应商消息，绝不含 AppID/SecretKey/token/助记词；
// 这几项一旦进 error 就会顺着日志和上层包装扩散到不可控的出口。
type ProviderError struct {
	// Channel 是通道名（"nft"），只用于定位，不参与判定。
	Channel string
	// Code 是供应商错误码，逐字保留，不做映射。
	Code string
	Msg  string
	// HTTPStatus 是传输层状态码；业务码在包络里时这里为 0。
	HTTPStatus int
	// Retryable 标记本次失败可否原单重发。由 Parse 判定：
	// Mint/Transfer 这类不可逆资产动作默认可重试性为 false；「明确未受理」
	// （本地前置校验失败、签名失败、4xx 中确定拒绝的包络码）才可以原单重发；
	// 「可能已受理」（transport/read_body/502/504）一律 false 且归 UNKNOWN 语义进对账队列。
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
