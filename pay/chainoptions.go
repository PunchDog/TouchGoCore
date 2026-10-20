package pay

import "time"

// ChainOptions 是「直接动公链」那一类通道驱动所需的输入。
//
// 为什么单独成一个结构而不塞进 Extras：Extras 的语义是「并入出站报文并追加进签名域」
// （见 mergeExtras），把私钥写进去等于给资金密钥开了一条通往请求体、签名串与日志的出口。
// 本结构的字段只在驱动内部用，永远不进报文、不进签名域、不进 RawNote。
//
// 谁填哪一半是刻意的分工，两个维度各只有一处真相：
//   - Contract / Network / FeeLimitSun 由通道包给出——合约与网络是「在哪条链发哪个币」的
//     属性，权威值已在通道配置段（usdt.contract / usdt.network）校验过一遍，不能再由
//     SDK 段抄一份，否则「校验过的合约」与「广播用的合约」可以不是同一个。
//   - Node / HTTPBase / APIKey / WalletAddress / WalletVersion / SubWalletID / PrivateKey
//     由装配层（assemble.go）从 pay_sdks.<段>.chain 填入——那是接入点、凭证与「我的热钱包
//     是什么形状」，与币种无关，同一节点服务多条链。
//
// 同一份配置被不同链的驱动各取所需：TRON 的出款只认 Node（gRPC），TON 的出款只认
// HTTPBase（TonAPI 的 HTTP 接入点）——Node 对后者留空是合法取值，见 telegram/ton_gateway_tonapi.go。
//
// 这里没有 Decimals：PayOrder.Amount 已经是该合约的最小单位整数（见 PayOrder 的注释），
// 链上驱动只做原样透传。若允许配置一个小数位，就把「10^n 倍多付」做成了一个可配错项。
type ChainOptions struct {
	// Node 是链节点的接入点（TRON 为 gRPC 的 host:port），出款广播只走它。
	// 走 HTTP 接入点的驱动（TON）不读这一项。
	Node string
	// HTTPBase 是同链的 REST 基址，只服务读侧（查单/余额/反查），不用于广播：
	// 少一条会动钱的未验证代码路径。留空即读侧只能用 gRPC。
	HTTPBase string
	// PlaintextNode 显式允许 Node 走明文传输。
	//
	// 缺省是 TLS：公共接入点要求 TLS，而 grpc-go 现在对「没声明传输安全」的拨号直接拒绝，
	// 让实现去猜这一项，表现是第一笔出款才报出一条看不出原因的错。自建节点才需要置真。
	PlaintextNode bool
	// APIKey 是节点服务方要求的访问凭证（TRON 侧为 TRON-PRO-API-KEY）。
	// 它不是资金密钥，但同样禁止进日志与 error 文案。
	APIKey string
	// WalletAddress 是我方出款账户的链上地址，用于日志定位与「私钥派生地址是否等于它」的核对。
	WalletAddress string
	// WalletVersion 是热钱包合约的版本名（TON 用 v5R1/v4R2/v3R2 这类写法，TRON 不读它）。
	//
	// 它和 WalletAddress 是同一族事实——「我的热钱包长什么样」，所以放在 chain 段而不是
	// 通道段：通道段只声明发的是哪个资产、哪张网。留空由驱动自行探测（探测不出来就拒装配）。
	WalletVersion string
	// SubWalletID 是钱包合约的子账户号（TON 的 v3/v4 系用它区分同一把公钥下的不同账户）。
	//
	// 指针而不是零值：0 是一个合法且常见的子账户号，用「是不是 0」表达「没配」会把
	// 显式配 0 的人推到默认值上，那派生出来的是另一个账户。nil 才是「没配，用该版本默认」。
	SubWalletID *uint32
	// PrivateKey 是热钱包私钥（hex）。配置里恒为空，由下游经注入钩子在启动前填入，
	// 口径与 SecretKey 一致：不进版本库、不进日志、不进任何摘要字段。
	PrivateKey string
	// Contract 是代币合约地址（TRC20 为 Base58）；原生币通道留空。
	Contract string
	// Network 是公链网络标识（mainnet/shasta/nile），驱动据此拒绝跨网发的单。
	Network string
	// FeeLimitSun 是这一类通道允许烧掉的手续费上限（原生币最小单位，TRON 为 sun、TON 为纳吨）。
	// <=0 由驱动用各自链的缺省值，本包不替链侧定默认。
	FeeLimitSun int64
	// Timeout 是单次链上操作的超时；<=0 由驱动用缺省值。
	Timeout time.Duration
	// MaxRetry 是读侧（查单/余额/反查）的重试次数；<0 由驱动用缺省值。
	// 写侧不看它：广播一旦发出就进「结果不确定」分支，重试次数救不了它（见链上驱动的台账）。
	MaxRetry int
}
