package pay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Channel 是一条资金通道对外提供的四件事：查商户账户、充值、提现、查单。
//
// 有了这个接口，上游就不必先知道「配置里写的是哪家 SDK」再写调用代码；
// SDK 差异收在实现里，不散落成一层 switch。各通道的门面函数（WhatsappRecharge
// 等）保留下来做包级留痕与回执广播，实际出资动作都转给本接口。
type Channel interface {
	// Driver 返回驱动标记，MerchantID 返回我方在供应商侧的默认商户号；两者只用于定位。
	Driver() string
	MerchantID() string
	Recharge(ctx context.Context, o *PayOrder) (*PayResult, error)
	Withdraw(ctx context.Context, o *PayOrder) (*PayResult, error)
	QueryOrder(ctx context.Context, orderNo string) (*PayResult, error)
	QueryAccount(ctx context.Context, q *AccountQuery) (*AccountInfo, error)
}

// Provider 是本包自带的通用实现：MD5 签名 + 统一包络。
var _ Channel = (*Provider)(nil)

// DriverGeneric 是内置通用驱动的登记名：HTTP + 签名域拼接 MD5 + code/msg/data 包络。
//
// 真实供应商若形态不同（RSA 签名、异步回调对账、链上广播），由那个 SDK 的包
// 在自己 init 里 Register 一个名字，配置里把 sdk 改成它即可，通道包与上游都不动。
const DriverGeneric = "generic_md5"

// Factory 把一份已解析好的配置变成可用通道。
//
// 失败必须返回 error 而不是 nil：凭证缺失、端点表不全这类问题要在启动时就报出来，
// 半成品的 Channel 只会在第一笔出款时炸。
type Factory func(opt ProviderOptions) (Channel, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register 登记一个供应商驱动。
//
// 重名直接拒绝：两处实现抢同一个名字，生效的是初始化顺序而非配置意图，
// 出款走错驱动是资损级事故。
func Register(driver string, f Factory) error {
	name := strings.TrimSpace(driver)
	if name == "" {
		return errors.New("pay: 驱动名不得为空")
	}
	if f == nil {
		return fmt.Errorf("pay: 驱动[%s] 的构造函数为 nil", name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[name]; ok {
		return fmt.Errorf("pay: 驱动[%s] 已登记，不得覆盖", name)
	}
	registry[name] = f
	return nil
}

// Open 按驱动名开一条通道。名没登记时把已登记的名单打在错误里，
// 免得配置写错一个字母就要翻代码找可用的名字。
func Open(driver string, opt ProviderOptions) (Channel, error) {
	name := strings.TrimSpace(driver)
	if name == "" {
		return nil, errors.New("pay: 未指定驱动名")
	}
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("pay: 驱动[%s] 未登记，可用驱动: %s", name, strings.Join(Drivers(), ", "))
	}
	if strings.TrimSpace(opt.Driver) == "" {
		opt.Driver = name
	}
	ch, err := f(opt)
	if err != nil {
		return nil, fmt.Errorf("pay: 打开驱动[%s] 失败: %w", name, err)
	}
	if ch == nil {
		return nil, fmt.Errorf("pay: 驱动[%s] 返回了空通道", name)
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
	// 让它炸在启动期，比留一个「配置了 SDK 却找不到驱动」的运行时错误有用。
	if err := Register(DriverGeneric, func(opt ProviderOptions) (Channel, error) {
		return NewProvider(opt)
	}); err != nil {
		panic(err)
	}
}
