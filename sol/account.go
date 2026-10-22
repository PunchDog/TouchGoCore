package sol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// 本文件是「热钱包在 USDT-SPL 上到底有多少钱」的那一半。
//
// 与 tron / telegram 那两侧的账户查询不是同一个口径：那边读的是供应商账本里的
// 额度（可用 / 冻结 / 授信），这边读的是链上事实——一个 token account 的余额。
// 链上没有「冻结」与「授信」之分，因此 Available 与 Balance 同值、其余留 0；
// 把它们编出一个数来，只会让下游把链上余额当成供应商授信。
//
// SPL 的余额挂在 associated token account（ATA）上，而不是钱包地址本身：
// 钱包地址下没有直接可读的 USDT 余额，必须先按 (wallet, mint) 派生出那个
// 确定性的 PDA 再查。少这一步，读出来的永远是「0」。

// tokenAccountMissing 是节点对「这个 token account 不存在」的固定回答。
//
// getTokenAccountBalance 不像 getAccountInfo 那样把「不存在」映射成 rpc.ErrNotFound，
// 而是把节点错误原样抛回来：Agave 的文案是 "Invalid param: could not find account"。
// 只有这一种错误被当成余额 0——其他的读失败必须往上报，否则一次 RPC 抖动就会
// 变成「热钱包没钱」，进而让一整批本来能发的单被拒。
const tokenAccountMissing = "could not find account"

// hotWalletATA 返回热钱包在 USDT mint 上的 ATA。
//
// 派生是纯本地的（PDA 由 wallet + token program + mint 唯一确定），不需要问节点；
// 因此它不会失败到需要重试，唯一的错误来源是 mint 地址本身不可解析。
func (c *onchainChannel) hotWalletATA() (solana.PublicKey, error) {
	ata, _, err := solana.FindAssociatedTokenAddress(c.sign.Address(), c.mint)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("派生热钱包的 ATA 失败: %w", err)
	}
	return ata, nil
}

// tokenBalance 读一个 token account 的原始余额（最小单位）。
//
// 账户不存在按 0 处理：新钱包的 ATA 要等第一次收款才会被创建，而「没有账户」
// 与「余额为 0」在出款判断上是同一件事——都不够发。
func (c *onchainChannel) tokenBalance(ctx context.Context, ata solana.PublicKey) (uint64, error) {
	bal, err := c.rpc.TokenAccountBalance(ctx, ata)
	if err != nil {
		if errors.Is(err, rpc.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), tokenAccountMissing) {
			return 0, nil
		}
		return 0, err
	}
	if bal == nil {
		return 0, nil
	}
	if bal.Decimals != uint8(c.opt.Decimals) {
		// 运行时再核一次小数位：装配期核的是 mint 账户里的 decimals，
		// 这里是 token account 的视图。两者不一致说明配置换了 mint 却没过装配，
		// 此时按配置解释金额就是 10^n 倍的错。
		return 0, fmt.Errorf("token account %s 的 decimals=%d 与通道配置的 %d 不一致",
			ata, bal.Decimals, c.opt.Decimals)
	}
	v, err := parseTokenAmount(bal.Amount)
	if err != nil {
		return 0, fmt.Errorf("token account %s 的余额字段不可解析: %w", ata, err)
	}
	return v, nil
}

// parseTokenAmount 把 UiTokenAmount.Amount 的十进制字符串解成 uint64。
//
// SPL 的金额是 u64。用字符串解析而不是 float：链上余额可以到 1e15 量级，
// float64 在这个量级已经丢精度，而「够不够」的判断不能靠一个近似值。
func parseTokenAmount(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("余额为空串")
	}
	return strconv.ParseUint(s, 10, 64)
}

// saturateUint64 把 u64 余额收窄到 int64。
//
// 超出即饱和而不是报错：本仓任何一单的 Amount 都是 int64，因此「饱和」在
// 「够不够」这件事上恒等于真实的「够」。真正的资金闸在 precheckFunds，
// 那里比的是 uint64。
func saturateUint64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
