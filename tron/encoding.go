package tron

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"
)

// 本文件是「一条 TRC20 转账在线格式上长什么样」的归属地。
//
// 写在这里而不是 pay：pay 只管「把一单资金请求送出去、把回执读回来」，
// 而选择子、32 字节左对齐、txID=sha256(raw_data) 都是 TRON 这条链的知识。

// trc20TransferSelector 是 TRC20 transfer(address,uint256) 的 4 字节调用前缀。
//
// 它逐字钉死而不是每次运行时对合约签名求哈希：这一串错了，交易照样能广播出去、
// 照样烧掉能量，只是没人收款。能钉成常量的东西不该依赖一次哈希。
const trc20TransferSelector = "a9059cbb"

// tvmWordLen 是 TRON 虚拟机（TVM）的单个字长，地址与金额都要左对齐补到这里。
//
// TVM 与 EVM 同构（同样的 32 字节字、同样的 keccak 选择子），但这条链上的代币标准
// 叫 TRC20，命名一律按 TRC20/TVM 走，不借以太坊的名字——借用会让读代码的人以为
// 这里对接的是 EVM 节点，而实际广播对象是 TRON 全节点。
const tvmWordLen = 32

// trc20CalldataLen 是 transfer 调用的数据长度：4 字节选择子 + 一个地址字 + 一个金额字。
const trc20CalldataLen = 4 + tvmWordLen*2

// trc20TransferCalldata 组一笔 USDT 转账的调用数据。
//
// amount 是合约最小单位的整数（USDT-TRC20 为 6 位），原样透传不做任何倍乘：
// 倍乘一次就把「小数位」变成了第二个真相，配错的后果是 10^n 倍的不可逆多付。
func trc20TransferCalldata(to string, amount int64) ([]byte, error) {
	if amount < 0 {
		return nil, fmt.Errorf("TRC20 转账金额不得为负，实得 %d", amount)
	}
	raw, err := decodeTRONAddress(to)
	if err != nil {
		return nil, fmt.Errorf("TRC20 收款地址不可用: %w", err)
	}
	selector, err := hex.DecodeString(trc20TransferSelector)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, trc20CalldataLen)
	data = append(data, selector...)
	data = append(data, padLeft(raw[1:21], tvmWordLen)...) // 去掉 0x41 版本字节，取 20 字节账户哈希
	data = append(data, padLeft(big.NewInt(amount).Bytes(), tvmWordLen)...)
	return data, nil
}

// padLeft 在左边补零到 n 字节；已经够长则原样返回。
func padLeft(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// decodeTRONAddress 解码 Base58 地址并复核校验和，返回 21 字节（含 0x41 版本字节）。
//
// 复用本包既有的三段式校验（长度、版本字节、双 SHA256），不另起一套解码：
// 「地址合不合法」在全仓只能有一个答案，否则 ChainRule 放行的地址可能在组包时被拒，
// 或更糟——被拒的地址在这里组出了一笔能广播的交易。
func decodeTRONAddress(s string) ([]byte, error) {
	raw, err := base58Decode(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(raw) != 25 || raw[0] != tronAddressVersion {
		return nil, fmt.Errorf("地址长度或版本字节不符（期望 25 字节且首字节 0x41，实得 %d 字节）", len(raw))
	}
	if !bytes.Equal(tronChecksum(raw[:21]), raw[21:]) {
		return nil, errors.New("末尾 4 字节校验对不上")
	}
	return raw, nil
}

// txIDOf 按 sha256(raw_data) 算交易号（小写十六进制）。
//
// 为什么本地算而不是只信节点回的 txid：广播结果不确定的那一刻，唯一能拿去查证的
// 标识必须在我们自己手里，而且它在签名之前就已确定——这正是「同一单永不签第二笔」
// 能成立的前提。gotron-sdk 自己算了一份（txcore.TransactionID），测试里两份逐字对照。
func txIDOf(tx *core.Transaction) (string, error) {
	raw := tx.GetRawData()
	if raw == nil {
		return "", errors.New("交易没有 raw_data，算不出交易号")
	}
	b, err := proto.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("raw_data 序列化失败: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// receiptVerdict 把链上回执读成「成功 / 失败 / 判定不明」三态。
//
// 只认交易级结果码与合约执行结果码，不认 contractResult 的字节内容：
// USDT-TRC20 是非标准 TRC20（Tether 的 transfer 不返回 bool），真机上那笔成功转账的
// contractResult 是 32 字节全零（2026-10-16 实测 txid ae07c52c…bba9），
// 按字节判会把每一笔真成功的出款读成失败。
//
// 判定不明（known=false）是刻意保留的第三态：回执缺字段时既不能说成功也不能说失败，
// 说失败的代价是上游重下一单，而链上那一笔可能已经转出去了。
func receiptVerdict(txResult core.TransactionInfoCode, contractResult core.Transaction_ResultContractResult, hasReceipt bool) (success, known bool) {
	if txResult != core.TransactionInfo_SUCESS {
		return false, true // 交易级已判失败（扣费/执行层面）
	}
	if !hasReceipt {
		return false, false
	}
	switch contractResult {
	case core.Transaction_Result_SUCCESS:
		return true, true
	case core.Transaction_Result_DEFAULT:
		// 节点没回填合约执行结果：读不懂就不下结论。
		return false, false
	default:
		return false, true // REVERT / OUT_OF_ENERGY 一类，明确失败且 gas 已烧
	}
}

// bigToInt64 把链上读回的大整数收进 int64，超界报错而不是截断。
//
// 截断的表现是「余额看着够用」，随后按错误的余额放行一笔出款。
func bigToInt64(v *big.Int) (int64, error) {
	if v == nil {
		return 0, errors.New("链上数值为空")
	}
	if !v.IsInt64() || v.Sign() < 0 || v.Int64() > math.MaxInt64 {
		return 0, fmt.Errorf("链上数值 %s 超出 int64 最小单位范围", v.String())
	}
	return v.Int64(), nil
}
