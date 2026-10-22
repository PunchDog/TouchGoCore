package pay

import (
	"errors"
	"fmt"
	"strings"
)

// 本文件是资金契约里「这笔钱从谁到谁」的那一半。
//
// 断代背景（2026-10）：PayOrder 曾把收款端拆成 Address / Network / Memo 三个平铺字段，
// 那是「只有链上钱包一种收款方」时代的形态。银行代付与供应商账本内划转接进来之后，
// 同一个 Address 位在不同通道里要填的是卡号、商户号或链上地址，平铺字段再也表达不了
// 「这一单发给谁、靠什么认」。于是收端收成 Party，付款端同样结构体化（From 可空＝
// 用通道配置的默认出款方），三个旧字段一并删除、不保留回落：
// 留着旧字段就等于留一个「一半调用方填旧字段、一半填新字段」的二义期，
// 而二义期里漏填的那个空位不会被任何一侧报出来——它只会变成一笔发给空地址的钱。

// PartyKind 是一笔资金的收/付方性质，取值就是这三个字面量。
type PartyKind string

const (
	// PartyWallet 链上钱包地址：钱上链，认地址。
	PartyWallet PartyKind = "wallet"
	// PartyBank 银行卡：钱走供应商的代付通道，认卡号与开户人。
	PartyBank PartyKind = "bank"
	// PartyMerchant 供应商侧商户号：钱不出供应商账本，认商户号。
	PartyMerchant PartyKind = "merchant"
)

// PartyKinds 返回已登记的性质，供错误文案与启动日志列出实况。
func PartyKinds() []string {
	return []string{string(PartyWallet), string(PartyBank), string(PartyMerchant)}
}

// BankAccount 是银行卡形态的收款方。
//
// 四个字段都不做格式校验：各家代付通道的卡号长度、开户行写法互不相同，
// 本包没有那份名录，凭空校验只会把合法的卡判成非法。必填项由通道侧按自己的文档把门。
type BankAccount struct {
	// AccountNo 卡号。明文可以进报文，但不得进日志与 RawNote。
	AccountNo string `json:"account_no,omitempty"`
	// Holder 开户人姓名。多数代付通道要求它与卡号的实名一致，不符即被供应商拒。
	Holder string `json:"holder,omitempty"`
	// BankName 开户行。
	BankName string `json:"bank_name,omitempty"`
	// Branch 开户支行。
	Branch string `json:"branch,omitempty"`
}

// Party 是一笔资金的收方或付方。
//
// 字段按 Kind 取用，其余留空：
//
//	wallet   → Address（链上地址）/ Network / Memo
//	merchant → Account（商户号）
//	bank     → Bank
//
// Memo 只对「链上认款靠备注」的网络有意义（TON 的 comment）：往交易所归集账户打钱时
// 收款方是所有人共用的一个热钱包，认款全靠这一串——漏填的后果是链上确认成功、
// 对方账上却认不出是谁的，只能走人工找回。反过来说，TRON / BSC / SOL 根本没有这个概念，
// 给它们填了 Memo 会被那条链的规则直接拒掉（见各包 checkOrderAddress）。
type Party struct {
	Kind PartyKind `json:"kind,omitempty"`
	// Address 链上地址。
	//
	// 它同时是供应商账本通道的「收款账号」位：Wallet / Merchant 两种性质在报文里
	// 落到同一个字段，因此 parse 之后 Merchant 的 Address 会由 Account 补上（见 Normalize）。
	Address string `json:"address,omitempty"`
	// Network 这一端所在的网络。
	//
	// 收款端填的 network 是「我认为这笔该落到哪张网」，与通道配置对不上即拒单：
	// 换网就是换币，而换错的链上转账没有任何一侧能自动挽回。
	Network string `json:"network,omitempty"`
	// Memo 附在转账上的备注（TON comment）。明文可以进报文，但不得进日志与 RawNote。
	Memo string `json:"memo,omitempty"`
	// Account 供应商侧商户号（kind=merchant）。
	Account string `json:"account,omitempty"`
	// Bank 银行卡（kind=bank）。指针为空与「填了一张全空白的卡」是两件事，见 Check。
	Bank *BankAccount `json:"bank,omitempty"`
	// Name 收/付方名称，只用于留痕与人工核对，不参与任何格式校验。
	Name string `json:"name,omitempty"`
}

// KindOf 返回这一端实际生效的性质：Kind 已填就用它，没填则按字段推断。
//
// 推断与归一分开，是因为 Check 必须是纯函数：「读一单没归一过的输入」是最常见的
// 调用形态，不该要求调用方先跑一遍变更才有资格校验。
func (p *Party) KindOf() PartyKind {
	if p == nil {
		return ""
	}
	if k := PartyKind(strings.ToLower(strings.TrimSpace(string(p.Kind)))); k != "" {
		return k
	}
	switch {
	case p.Bank != nil:
		return PartyBank
	case strings.TrimSpace(p.Address) != "":
		return PartyWallet
	case strings.TrimSpace(p.Account) != "":
		return PartyMerchant
	default:
		return ""
	}
}

// Normalize 补齐能推断出来的空位，幂等：跑两遍与跑一遍结果相同。
//
// 只补空位、不改已有值：
//
//	Kind 空               → 按 Bank / Address / Account 推断（顺序见 KindOf）
//	merchant 的 Address 空 → 取 Account
//
// 网络与备注不在这里补：它们没有「能从别处推断出来」的来源，缺省值是装配期的事实，
// 由各通道的 order() 按自己的配置补。
func (p *Party) Normalize() {
	if p == nil {
		return
	}
	if p.Kind = p.KindOf(); p.Kind == PartyMerchant && strings.TrimSpace(p.Address) == "" {
		p.Address = strings.TrimSpace(p.Account)
	}
}

// Check 按实际生效的性质校验这一端填得够不够。
//
// 判据只到「这一端能不能被这条链/这家供应商认出来」为止：
// 地址格式、卡号长度这类属于那条链或那家供应商的知识，由通道侧的规则把门。
func (p *Party) Check() error {
	if p == nil {
		return errors.New("这一端为空")
	}
	switch k := p.KindOf(); k {
	case PartyWallet:
		if strings.TrimSpace(p.Address) == "" {
			return errors.New("收款地址为空")
		}
	case PartyMerchant:
		if strings.TrimSpace(p.Account) == "" && strings.TrimSpace(p.Address) == "" {
			return errors.New("商户号为空")
		}
	case PartyBank:
		if p.Bank == nil {
			return errors.New("标记为银行转账（kind=bank），却没有 bank 段")
		}
		if strings.TrimSpace(p.Bank.AccountNo) == "" {
			return errors.New("银行卡号为空")
		}
	case "":
		return errors.New("既没有地址、没有商户号，也没有银行账户：无法判断这笔钱发给谁")
	default:
		return fmt.Errorf("性质 %q 未登记（可用 %s）", string(p.Kind), strings.Join(PartyKinds(), " / "))
	}
	return nil
}

// WithdrawTarget 声明一条通道能发到哪几类收款方。
//
// 显式列白而不是「除 X 外都放行」：将来新增一类收款方时，默认结果是拒单而不是
// 被静默接受——后者意味着一条根本发不出去的单被送进了组包与签名流程。
type WithdrawTarget struct {
	Wallet   bool
	Merchant bool
	Bank     bool
}

// CheckTarget 按收款方的实际性质判定本通道能不能发这一单。
//
// 链上通道只置 Wallet；供应商账本通道置 Wallet + Merchant（账本内划转不需要上链）。
// 银行代付本仓尚无实现，任何通道都会在这里给出明确错误，而不是落到地址解析里
// 报一句「TRON 收款地址 6222… 不合法」——那会让人以为填错了地址，而不是走错了通道。
func CheckTarget(o *PayOrder, t WithdrawTarget) error {
	if o == nil || o.To == nil {
		return errors.New("收款方（to）为空")
	}
	switch k := o.To.KindOf(); k {
	case PartyWallet:
		if t.Wallet {
			return nil
		}
		return errors.New("本通道不支持链上钱包收款（to.kind=wallet）")
	case PartyMerchant:
		if t.Merchant {
			return nil
		}
		return errors.New("本通道不支持供应商商户号收款（to.kind=merchant）")
	case PartyBank:
		if t.Bank {
			return nil
		}
		return errors.New("本通道不支持银行转账（to.kind=bank）")
	default:
		return fmt.Errorf("收款方性质 %q 未登记（可用 %s）", string(k), strings.Join(PartyKinds(), " / "))
	}
}

// Normalize 补齐订单里能推断出来的空位，幂等。
//
// From 的缺省值不在这里填：它的「本通道的热钱包 / 付款商户号」是装配期的事实，
// 由各通道的 order() 或通道自己按配置解析——pay 不引 config，也就不知道配置里写的是什么。
func (o *PayOrder) Normalize() {
	if o == nil {
		return
	}
	o.To.Normalize()
	o.From.Normalize()
}

// Check 是下单前置校验：幂等键必须有，金额必须是正的最小单位整数，收付两端必须可解析。
//
// 收款端必填这一条来自「充值删除后只剩提现一条下单路径」：空地址的一单出去，
// 钱就找不到主人了，而链上没有任何回滚手段。
func (o *PayOrder) Check() error {
	switch {
	case o == nil:
		return errors.New("订单为 nil")
	case strings.TrimSpace(o.OrderNo) == "":
		return errors.New("订单号（幂等键）为空")
	case o.Amount <= 0:
		return errors.New("金额必须为正整数最小单位")
	case o.To == nil:
		return errors.New("收款方（to）为空")
	}
	if err := o.To.Check(); err != nil {
		return fmt.Errorf("收款方（to）: %w", err)
	}
	if o.From != nil {
		if err := o.From.Check(); err != nil {
			return fmt.Errorf("出款方（from）: %w", err)
		}
	}
	return nil
}

// CheckOrder 是 Check 的导出入口，保留旧名供各通道与链上驱动共用同一份前置。
//
// 必须全仓只有一份实现：「哪些字段算必填」两处各写一遍，迟早漂移，
// 而漂移的表现是「一半通道收了一条根本没写收款地址的单」。
func CheckOrder(o *PayOrder) error { return o.Check() }
