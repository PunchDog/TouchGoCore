// Package sol 是 Solana 原生币（SOL）通道的充值与提现接入点。
//
// 目录、包名与币种常量统一为 sol/SOL，代码内币种一律用 pay.CurrencySOL，
// 金额按 lamports（9 位最小单位）原样透传——指数的解释权在填单的一方。
// 网络取值走 pay.NetworkMainnet / devnet——Solana 地址校验（32 字节 ed25519
// 公钥的 Base58 形态）在本包 address.go，SPL 代币与原生 SOL 共用同一地址形态。
//
// 本包只负责把请求送到供应商、把回执读成 pay.PayResult，不碰 SQL、不碰积分与额度；
// 下游业务按 util.CallSolMsg+"Xxx" 注册回调接手结果。
// 与 whatsapp 通道相比这里没有登录链路：SOL 的收付靠签名凭证即可，没有会话态。
//
// 供应商接入本身不在这个包里配置：sol.provider 只是对 pay_sdks 一段的引用，
// 凭证、端点表与商户账户都集中在 pay_sdks，装配动作在 paysdk。
// 本包是「接入点」而非「已完成对接」：真实接口文档到位后只需要改配置里的 endpoints
// 路径与 pay_sdks.<段>.driver 标记，生命周期与门面函数签名都不动。
package sol

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

var (
	// 只读快照：SolStart 写入，各入口经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求存入的具体类型一致，
	// 而 context.Context 的实现类型随调用方变化，存第二个即 panic。
	solRunCtx atomic.Pointer[context.Context]
	// active 为 nil 表示链路未启动，对应操作会就地拒绝而不是发出请求。
	active atomic.Pointer[client]
)

func init() {
	setRunCtx(context.Background())
}

func setRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := ctx
	solRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := solRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// SolStart 按配置装配 SOL 资金链路。
//
// 配置段缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
func SolStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	if c, ok := newClient(corectx.CfgFrom(ctx)); ok {
		active.Store(c)
		return
	}
	vars.Info("不启动SOL")
}

// SolStop 摘掉供应商客户端。本包无常驻协程，清空指针即可；可重复调用。
func SolStop(ctx context.Context) {
	active.Store(nil)
}

// currentClient 取资金链路；未启动时返回明确错误而不是 panic。
func currentClient() (*client, error) {
	if c := active.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("SOL 通道未启动（检查 sol.provider 是否引用了已开启的 pay_sdks 段与商户账户）")
}

// publish 把归一后的资金回执广播给下游，键为 util.CallSolMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func publish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallSolMsg+kind, res); !ok {
		vars.Debug("SOL 回执无人订阅: kind=%s order_no=%s", kind, res.OrderNo)
	}
}

// orderCall 是三个下单/查单门面共用的收尾：补通道前缀 → 广播回执。
//
// ctx 为 nil 时回落到包级运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本包的留痕口径。
func orderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("SOL %s 失败: %w", kind, err)
	}
	publish(kind, res)
	return res, nil
}
