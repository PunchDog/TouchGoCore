package pay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// 本文件是「按通道种类分派资金动作」的那张表。
//
// 它取代了原来住在 pay 里的驱动注册表：那张表以「驱动名」为键、以「能构造出通道」
// 为值，装配逻辑因此必须留在契约层——pay 于是知道了 config、知道了 HTTP 报文、
// 知道了每一家供应商。现在装配归各通道包，pay 只留一张「谁已经起来了」的表：
// 键是通道种类（Kind），值是那一条已经装配好的出款实现。
//
// 收益不是少了几行代码，而是方向的确定性：契约层不再可能悄悄持有传输实现。
// 代价是启动顺序敏感——某条通道没起来，TransferTo 就查不到它。这一点是刻意的：
// 「查不到」必须在分派那一刻被说出来，而不是像注册表时代那样，谁都能在任何时候
// 重新构造一条通道，从而绕开装配期已经做过的配置校验。

// Kind 是资金通道的种类标识，取值就是「配置里那一段的路径」形式。
//
// 用点号路径而不是 "tron" / "ton" 这类裸链名：同一条链上将来可能有多张合约
// 各开一条通道（USDT 与另一种代币的 gas 与收款策略都不同），可扩展性留在名字里，
// 比事后加一层 map[string]map[string] 便宜得多。
type Kind string

const (
	// KindTronUSDT 是 TRON 上的 USDT-TRC20，由 tron 包装配与持有。
	KindTronUSDT Kind = "tron.usdt"
	// KindTON 是 TON 链，由 telegram 包装配与持有。
	KindTON Kind = "telegram.ton"
	// KindBscUSDT 是 BSC 上的 USDT-BEP20，由 bsc 包装配与持有。
	KindBscUSDT Kind = "bsc.usdt"
	// KindSolUSDT 是 Solana 上的 USDT-SPL，由 sol 包装配与持有。
	KindSolUSDT Kind = "sol.usdt"
)

// Kinds 返回全部已登记的种类，供错误文案与启动日志列出实况。
func Kinds() []Kind {
	return []Kind{KindTronUSDT, KindTON, KindBscUSDT, KindSolUSDT}
}

// Transfer 是一条已经装配好的资金通道能对外承诺的全部动作。
//
// 只有 Withdraw 一个方法，这不是残缺：查单与查账户的入参各不相同
// （供应商按订单号查、链上按 txid 查、账户查询在有些通道上根本不存在），
// 硬塞进一个接口只会催生一个个「这条通道不支持，返回 not implemented」的空实现。
// 分派只需要一个共同动作，那就是把钱发出去。
type Transfer interface {
	Withdraw(ctx context.Context, o *PayOrder) (*PayResult, error)
}

// bindings 是「已装配通道」表。键是种类，值是那一条实现。
var (
	bindingsMu sync.RWMutex
	bindings   = map[Kind]Transfer{}
)

// Bind 登记一条已装配好的通道。
//
// 重复登记一律报错，不覆盖：覆盖看起来更宽容，实际是把「两条通道抢同一个种类」
// 变成静默的一半失效——先后三次启动、最后一次赢，中间那次留下的 goroutine 与
// 连接仍然在跑，而分派已经指向别人。要换实现请先 Unbind，让这次替换是显式的。
//
// 空种类与 nil 实现在这里就拒：它们只可能来自装配代码写错，而写错的后果是
// 分派时「查到了却调用不了」或者「绑进了一个谁也不是的键」。
func Bind(k Kind, t Transfer) error {
	if strings.TrimSpace(string(k)) == "" {
		return errors.New("绑定资金通道失败：种类为空")
	}
	if t == nil {
		return fmt.Errorf("绑定资金通道 %s 失败：实现为 nil", k)
	}
	bindingsMu.Lock()
	defer bindingsMu.Unlock()
	if _, exists := bindings[k]; exists {
		return fmt.Errorf("绑定资金通道 %s 失败：该种类已绑定，需先 Unbind", k)
	}
	bindings[k] = t
	return nil
}

// Unbind 摘掉一条通道的登记，返回它此前是否存在。
//
// 与 Bind 成对存在是必须的：Start/Stop 可以反复调用，没有 Unbind 的话
// 「停掉再起」会在第二次 Bind 上撞死，而为了绕过它放宽 Bind 的重复检查，
// 就等于放弃了上面那条「不静默覆盖」的判据。
func Unbind(k Kind) bool {
	bindingsMu.Lock()
	defer bindingsMu.Unlock()
	if _, exists := bindings[k]; !exists {
		return false
	}
	delete(bindings, k)
	return true
}

// TransferFor 取出某个种类的通道实现。未装配时返回明确的错误，
// 而不是返回一个「什么也不做」的空实现——后者会让一笔本该报错的出款安静地消失。
func TransferFor(k Kind) (Transfer, error) {
	bindingsMu.RLock()
	t, ok := bindings[k]
	bindingsMu.RUnlock()
	if ok {
		return t, nil
	}
	bound := BoundKinds()
	if len(bound) == 0 {
		return nil, fmt.Errorf("资金通道 %s 未装配：当前没有任何通道启动成功", k)
	}
	return nil, fmt.Errorf("资金通道 %s 未装配（已装配: %s）", k, strings.Join(bound, " / "))
}

// TransferTo 把一单出款分派给对应种类。
//
// 它是上游唯一需要的入口：调用方说「走 tron.usdt」，钱就从 tron 那条链出去，
// 不需要知道是哪个包在实现、也不需要 import 它。前置校验不在这一层做第二次——
// 各通道自己会调 CheckOrder，重复一遍只会让两处判据迟早漂移。
func TransferTo(ctx context.Context, k Kind, o *PayOrder) (*PayResult, error) {
	t, err := TransferFor(k)
	if err != nil {
		return nil, err
	}
	return t.Withdraw(ctx, o)
}

// BoundKinds 返回当前已装配的种类（字典序），供启动日志与错误文案使用。
func BoundKinds() []string {
	bindingsMu.RLock()
	out := make([]string, 0, len(bindings))
	for k := range bindings {
		out = append(out, string(k))
	}
	bindingsMu.RUnlock()
	sort.Strings(out)
	return out
}
