package pay

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ChainRule 是某条链对「收款端长什么样」的前置约束：地址格式与备注。
//
// 本接口存在的原因是分层：地址算法（TRON 的 base58check、TON 的 CRC16）属于那条链
// 的知识，不属于资金契约；而 pay 是零内部依赖的叶子包，把算法写进来就等于让契约层
// 反过来知道 TRC20 与 jetton 的细节。于是契约只留这两个方法，实现由各通道包在自己
// 的 init 里按币种登记——与驱动注册表完全同构。
//
// 两个方法都收整单而不是单个字符串：判断地址合法往往要连着看 network（测试网位）
// 和 currency（原生币与代币的收款端不是同一个地址形态）。
type ChainRule interface {
	// CheckAddress 校验收款地址。提现侧在 CheckOrder 之后调用，充值侧地址通常为空。
	CheckAddress(o *PayOrder) error
	// CheckMemo 校验备注。链上根本没有这一概念的（TRC20）应当直接拒绝非空值——
	// 上游给不许带 memo 的链填了 memo，说明它把两条链的规则搞混了。
	CheckMemo(o *PayOrder) error
}

var (
	chainRuleMu sync.RWMutex
	chainRules  = map[string]ChainRule{}
)

// RegisterChainRule 按币种登记一条链的校验规则。
//
// 重名拒绝而不是覆盖：两处实现抢同一个币种，生效的是包初始化顺序而非配置意图，
// 后果是「以为校验过了」的假安全感——比没校验更糟。
func RegisterChainRule(currency string, rule ChainRule) error {
	name := strings.ToUpper(strings.TrimSpace(currency))
	if name == "" {
		return errors.New("pay: 币种不得为空")
	}
	if rule == nil {
		return fmt.Errorf("pay: 币种[%s] 的校验规则为 nil", name)
	}
	chainRuleMu.Lock()
	defer chainRuleMu.Unlock()
	if _, ok := chainRules[name]; ok {
		return fmt.Errorf("pay: 币种[%s] 的校验规则已登记，不得覆盖", name)
	}
	chainRules[name] = rule
	return nil
}

// ChainRuleFor 取出该币种的校验规则；未登记时 ok 为 false。
func ChainRuleFor(currency string) (ChainRule, bool) {
	name := strings.ToUpper(strings.TrimSpace(currency))
	if name == "" {
		return nil, false
	}
	chainRuleMu.RLock()
	defer chainRuleMu.RUnlock()
	rule, ok := chainRules[name]
	return rule, ok
}

// CurrenciesWithRules 返回已登记校验规则的币种，按字典序，供启动日志列出实况。
func CurrenciesWithRules() []string {
	chainRuleMu.RLock()
	defer chainRuleMu.RUnlock()
	names := make([]string, 0, len(chainRules))
	for n := range chainRules {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// checkChain 在下单前跑该币种的规则；没登记规则就只做现有的非空校验。
//
// 「未登记即放行格式」不是漏掉，而是本仓的既有口径：规则由各通道包的 init 提供，
// 宿主工程没链接那个包就没有那条链的知识，pay 无从凭空校验。启动日志里把已登记
// 币种打出来（见 CurrenciesWithRules），配了通道却没见到规则就是包没被链进来。
func checkChain(o *PayOrder) error {
	rule, ok := ChainRuleFor(o.Currency)
	if !ok {
		return nil
	}
	if err := rule.CheckAddress(o); err != nil {
		return err
	}
	return rule.CheckMemo(o)
}
