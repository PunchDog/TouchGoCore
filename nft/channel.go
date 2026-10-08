package nft

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// NftChannel 是一条 NFT 资产通道的五件事：查询持有、查询单币详情、铸造提交、转账提交、查单。
//
// 有了这个接口，上游不必先知道「配置里写的是哪家 NFT 供应商」再写调用代码；
// 供应商差异收在驱动实现里（nft/channel.go 包注释，口径对 pay/channel.go:12-25 同构复刻）。
type NftChannel interface {
	// Driver 返回驱动标记，MerchantID 返回我方在供应商侧的默认签发主体号；两者只用于定位。
	Driver() string
	MerchantID() string
	QueryHoldings(ctx context.Context, q *HoldingsQuery) (*HoldingsResult, error)
	QueryToken(ctx context.Context, q *TokenQuery) (*TokenInfo, error)
	Mint(ctx context.Context, o *NftOrder) (*NftResult, error)
	Transfer(ctx context.Context, o *NftOrder) (*NftResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*NftResult, error)
}

// Provider 是本包自带的通用实现：MD5 签名 + 统一包络。
var _ NftChannel = (*Provider)(nil)

// DriverGeneric 是内置通用驱动的登记名：HTTP + 签名域拼接 MD5 + code/msg/data 包络。
//
// 取名规则：内置 generic_md5 的同构名，加 nft_ 前缀避免与 pay.DriverGeneric
// （值 "generic_md5"）在配置文件里被混填——配置写 driver:"generic_md5" 落在
// nft_sdks 段上应命中「未登记，可用驱动」报错，而不是「看起来配对了但拿的是资金驱动」。
const DriverGeneric = "nft_generic_md5"

// Factory 把一份已解析好的配置变成可用通道。
//
// 失败必须返回 error 而不是 nil：凭证缺失、端点表不全这类问题要在启动时就报出来，
// 半成品的通道只会在第一次铸造时炸（pay/channel.go:36-40 论证逐字沿用）。
type Factory func(opt ProviderOptions) (NftChannel, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register 登记一个供应商驱动。
//
// 重名直接拒绝（不 panic，给运行期第三方 SDK 留活路）：两处实现抢同名生效的是
// 初始化顺序而非配置意图，资产动作走错驱动是资损级事故。
func Register(driver string, f Factory) error {
	name := strings.TrimSpace(driver)
	if name == "" {
		return errors.New("nft: 驱动名不得为空")
	}
	if f == nil {
		return fmt.Errorf("nft: 驱动[%s] 的构造函数为 nil", name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[name]; ok {
		return fmt.Errorf("nft: 驱动[%s] 已登记，不得覆盖", name)
	}
	registry[name] = f
	return nil
}

// Open 按驱动名开一条通道。名没登记时把已登记的名单打在错误里，
// 免得配置写错一个字母就要翻代码找可用的名字。
func Open(driver string, opt ProviderOptions) (NftChannel, error) {
	name := strings.TrimSpace(driver)
	if name == "" {
		return nil, errors.New("nft: 未指定驱动名")
	}
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("nft: 驱动[%s] 未登记，可用驱动: %s", name, strings.Join(Drivers(), ", "))
	}
	if strings.TrimSpace(opt.Driver) == "" {
		opt.Driver = name
	}
	ch, err := f(opt)
	if err != nil {
		return nil, fmt.Errorf("nft: 打开驱动[%s] 失败: %w", name, err)
	}
	if ch == nil {
		return nil, fmt.Errorf("nft: 驱动[%s] 返回了空通道", name)
	}
	return ch, nil
}

// Drivers 返回已登记的驱动名，按字典序，供启动日志与报错列出可选值。
func Drivers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func init() {
	// 内置驱动用同一个 panic 口径：登记失败只可能是本包写错了名字，
	// 让它炸在启动期，比留一个「配置了 SDK 却找不到驱动」的运行时错误有用
	// （pay/channel.go:106-114 逐字同构）。
	if err := Register(DriverGeneric, func(opt ProviderOptions) (NftChannel, error) {
		return NewProvider(opt)
	}); err != nil {
		panic(err)
	}
}
