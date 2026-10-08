package nft

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"touchgocore/corectx"
	"touchgocore/util"
	"touchgocore/vars"
)

var (
	// 只读快照：NftStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	nftRunCtx atomic.Pointer[context.Context]
	// active 为 nil 表示链路未启动，对应操作会就地拒绝而不是发出请求。
	active atomic.Pointer[client]
)

func init() {
	setRunCtx(context.Background())
	// provider.go 是契约文件、按 §1 表零本仓 import，告警走这里注入（见 warnf 注释）。
	warnf = vars.Error
}

func setRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := ctx
	nftRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := nftRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// NftStart 按配置装配 NFT 资产链路。
//
// 配置段缺失、SDK 段没开、密钥未注入，都只记日志并返回：资产通道没配好不该阻断整机启动，
// 但必须留下可查的痕迹（逐字沿用 BscStart 注释口径）。
func NftStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	if c, ok := newClient(corectx.CfgFrom(ctx)); ok {
		active.Store(c)
		return
	}
	vars.Info("不启动NFT")
}

// NftStop 摘掉供应商客户端。本包无常驻协程（对账节奏归下游定时器，§3.5），
// 清空指针即可；可重复调用。签名收 ctx 与 BscStop 对齐，便于将来挂清理钩子不破签名。
func NftStop(ctx context.Context) {
	active.Store(nil)
}

// currentClient 取资产链路；未启动时返回明确错误而不是 panic。
//
// 未启动错误全文就是下面这一句（T7 断言按逐字匹配写，§8 精确口径对 bsc/fund.go:18-21）。
func currentClient() (*client, error) {
	if c := active.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("NFT 通道未启动（检查 nft.provider 是否引用了已开启的 nft_sdks 段与签发账户）")
}

// publish 把归一后的资产回执广播给下游，键为 util.CallNftMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *nft.NftResult，回调签名据此写。只读动作 Holdings/Token 不广播（ADR-9）。
func publish(kind string, res *NftResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallNftMsg+kind, res); !ok {
		vars.Debug("NFT 回执无人订阅: kind=%s order_no=%s", kind, res.OrderNo)
	}
}

// orderCall 是三个会广播的门面（Mint/Transfer/Query）共用的收尾：补通道前缀 → 广播回执。
//
// ctx 为 nil 时回落到包级运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验、幂等闸与状态归一都在 provider 侧做，这里只负责本包的留痕与广播口径。
func orderCall(ctx context.Context, kind string, call func(context.Context) (*NftResult, error)) (*NftResult, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		// 回执必须跟着 error 一起带回（NF-F5③）：已外发但结论读不出时 provider 给的是
		// (UNKNOWN 回执, err)。门面把回执抹成 nil，上游就只能按「没发过」分支另起新单号
		// 重下——那是二次铸造。错误路径不广播：回执不可信的回据不落下游（§3.3）。
		return res, fmt.Errorf("NFT %s 失败: %w", kind, err)
	}
	publish(kind, res)
	return res, nil
}

// NftMint 向供应商下铸造单。
//
// order.OrderNo 是幂等键：同一单重复调用只是重发同一张单，不会产生第二笔资产动作。
// 返回 UNKNOWN 状态时不要直接重下单，先 NftQuery 核对供应商到底收没收到（§3.5）。
func NftMint(ctx context.Context, order *NftOrder) (*NftResult, error) {
	c, err := currentClient()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Mint", func(ctx context.Context) (*NftResult, error) {
		return c.ch.Mint(ctx, c.fillOrder(order))
	})
}

// NftTransfer 向供应商下转账单（转已铸成的币，不可逆资产动作）。
func NftTransfer(ctx context.Context, order *NftOrder) (*NftResult, error) {
	c, err := currentClient()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Transfer", func(ctx context.Context) (*NftResult, error) {
		return c.ch.Transfer(ctx, c.fillOrder(order))
	})
}

// NftQuery 按订单号查供应商侧的处置结果（会广播 Query 回执，供下游推进 pending/UNKNOWN）。
func NftQuery(ctx context.Context, orderNo string) (*NftResult, error) {
	c, err := currentClient()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Query", func(ctx context.Context) (*NftResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// NftHoldings 按地址查名下 NFT。这是只读动作，不广播回执：下游要的是当场判断，
// 而清单/账本在业务侧（口径照 BscAccount 先例，ADR-9）。
func NftHoldings(ctx context.Context, q *HoldingsQuery) (*HoldingsResult, error) {
	c, err := currentClient()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := c.ch.QueryHoldings(ctx, c.fillHoldingsQuery(q))
	if err != nil {
		return nil, fmt.Errorf("NFT 持有查询失败: %w", err)
	}
	return res, nil
}

// NftToken 按四元组查单币详情。同 NftHoldings，只读不广播。
func NftToken(ctx context.Context, q *TokenQuery) (*TokenInfo, error) {
	c, err := currentClient()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = runCtx()
	}
	info, err := c.ch.QueryToken(ctx, c.fillTokenQuery(q))
	if err != nil {
		return nil, fmt.Errorf("NFT 单币查询失败: %w", err)
	}
	return info, nil
}

// fillHoldingsQuery 用快照默认补 chain/network（复制不改调用方对象）；q 为 nil 时给空查询。
func (c *client) fillHoldingsQuery(q *HoldingsQuery) *HoldingsQuery {
	if q == nil {
		q = &HoldingsQuery{}
	}
	chain := strings.TrimSpace(q.Chain)
	if chain == "" {
		chain = c.chain
	}
	network := strings.TrimSpace(q.Network)
	if network == "" {
		network = c.network
	}
	if chain == q.Chain && network == q.Network {
		return q
	}
	cp := *q
	cp.Chain, cp.Network = chain, network
	return &cp
}

// fillTokenQuery 用快照默认补 chain/network（四元组里 network 常留空按默认，§2.3）。
func (c *client) fillTokenQuery(q *TokenQuery) *TokenQuery {
	if q == nil {
		q = &TokenQuery{}
	}
	chain := strings.TrimSpace(q.Chain)
	if chain == "" {
		chain = c.chain
	}
	network := strings.TrimSpace(q.Network)
	if network == "" {
		network = c.network
	}
	if chain == q.Chain && network == q.Network {
		return q
	}
	cp := *q
	cp.Chain, cp.Network = chain, network
	return &cp
}
