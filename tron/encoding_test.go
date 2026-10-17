package tron

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/fbsobreira/gotron-sdk/pkg/standards/trc20enc"
	"github.com/fbsobreira/gotron-sdk/pkg/txcore"
	"golang.org/x/crypto/sha3"
	"google.golang.org/protobuf/types/known/anypb"
)

// 本文件全部离线。calldata、选择子、交易号与地址形态各有两到三份独立来源可比：
// 本包实现、gotron-sdk 的 TRC20 编码件（pkg/standards/trc20enc）、SDK 的 txID 计算，
// 以及从函数签名字面量现算的 keccak256。参照物一律取 TRON 侧的件，不借以太坊的 ABI
// 编码器：这条链上的标准叫 TRC20，拿 ERC20 的库当 oracle 等于把「同构」误当成「同源」，
// 而真出问题时该被怀疑的正是这层假设。编码错的代价是钱进了没人拥有的地址，
// 所以这里不留「看着像对」的余地。

// 夹具沿用本包 tron_test.go 里造好的向量：tronOfficialUSDT（主网合约）、
// tronRangeAcct（20 字节账户哈希造出的合法地址，用来证明编码没把地址写死）、
// tronBogusChecksum（长度与版本字节全对、只有校验和不过）。

func TestSelectorMatchesKeccakOfSignature(t *testing.T) {
	// 选择子的来历只能由函数签名字面量现算来验：写死的常量可能抄自一份错文档，
	// 而那要等到第一笔转账才会暴露。
	for _, cs := range []struct {
		signature string
		want      string
		sdk       string
	}{
		{"transfer(address,uint256)", trc20TransferSelector, trc20enc.SelectorTransfer},
		{"balanceOf(address)", trc20BalanceSelector, trc20enc.SelectorBalanceOf},
	} {
		h := sha3.NewLegacyKeccak256()
		h.Write([]byte(cs.signature))
		got := hex.EncodeToString(h.Sum(nil)[:4])
		if got != cs.want {
			t.Errorf("%s 算出的选择子是 %s，代码里写的是 %s", cs.signature, got, cs.want)
		}
		if got != cs.sdk {
			t.Errorf("%s 的选择子与 SDK 的 TRC20 常量不符: 本包 %s / trc20enc %s", cs.signature, got, cs.sdk)
		}
	}
}

func TestTRC20CalldataAgainstReferenceEncoder(t *testing.T) {
	for _, amount := range []int64{1, 1_000_000, 3_000_000_000, 9223372036854775807} {
		for _, to := range []string{tronOfficialUSDT, tronRangeAcct} {
			want, err := referenceTransferCalldata(t, to, amount)
			if err != nil {
				t.Fatal(err)
			}
			got, err := trc20TransferCalldata(to, amount)
			if err != nil {
				t.Fatalf("amount=%d: %v", amount, err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("to=%s amount=%d 的调用数据与参照编码器不一致\n got=%x\nwant=%x", to, amount, got, want)
			}
		}
	}
}

// referenceTransferCalldata 用 SDK 的 TRC20 编码件算同一笔转账，作为独立参照物。
func referenceTransferCalldata(t *testing.T, to string, amount int64) ([]byte, error) {
	t.Helper()
	raw, err := decodeTRONAddress(to)
	if err != nil {
		return nil, err
	}
	hexed, err := trc20enc.EncodeTransferCall(address.BytesToAddress(raw[:21]), big.NewInt(amount))
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(hexed, "0x"))
}

func TestTRC20CalldataShape(t *testing.T) {
	data, err := trc20TransferCalldata(tronOfficialUSDT, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != trc20CalldataLen {
		t.Fatalf("transfer 调用数据应为 %d 字节，实得 %d", trc20CalldataLen, len(data))
	}
	if hex.EncodeToString(data[:4]) != trc20TransferSelector {
		t.Errorf("前 4 字节不是选择子: %x", data[:4])
	}
	if !bytes.Equal(data[4:16], make([]byte, 12)) {
		t.Errorf("地址字左侧没补零: %x", data[4:16])
	}
	want, err := address.Base58ToAddress(tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	if got, want2 := hex.EncodeToString(data[16:36]), strings.TrimPrefix(want.Hex(), "0x41"); got != want2 {
		t.Errorf("地址字 %s 与合约地址的 EVM 形态 %s 不符", got, want2)
	}
	// 金额字必须是最小单位原样编码：这里倍乘一次就是 10^6 倍的事故。
	if got := new(big.Int).SetBytes(data[36:]); got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("金额字被改写成 %s", got)
	}
}

func TestTRC20CalldataRejectsBadInput(t *testing.T) {
	if _, err := trc20TransferCalldata(tronOfficialUSDT, -1); err == nil {
		t.Fatal("负金额必须拒绝：它会补成一串全 F 的 32 字节")
	}
	if _, err := trc20TransferCalldata("not-a-tron-address", 1); err == nil {
		t.Fatal("非法收款地址必须拒绝")
	}
}

func TestTxIDMatchesSDKImplementation(t *testing.T) {
	// 本包的 sha256(raw_data) 与 SDK 的 RecomputeTxID 来自不同代码库，必须逐字相等。
	// 广播结果不确定时手里这个号就是查证的唯一把手，它不能与链上那个号是两个。
	for i, tx := range sampleTransactions() {
		got, err := txIDOf(tx)
		if err != nil {
			t.Fatal(err)
		}
		ext := &api.TransactionExtention{Transaction: tx}
		if err := txcore.RecomputeTxID(ext); err != nil {
			t.Fatal(err)
		}
		if want := hex.EncodeToString(ext.GetTxid()); got != want {
			t.Errorf("样本 %d 的 txID 两份实现不一致: 本包 %s / SDK %s", i, got, want)
		}
	}
}

func TestTxIDIsStableAcrossSignature(t *testing.T) {
	// 「签名后、广播前就能确定交易号」是整条防双付链的前提：
	// txID 只由 raw_data 决定，加上签名不应改变它。
	tx := sampleTransactions()[0]
	idA, err := txIDOf(tx)
	if err != nil {
		t.Fatal(err)
	}
	signed := &core.Transaction{RawData: tx.GetRawData(), Signature: [][]byte{{1, 2, 3}}}
	idB, err := txIDOf(signed)
	if err != nil {
		t.Fatal(err)
	}
	if idA != idB {
		t.Fatalf("签名改变了 txID：%s vs %s，台账与查证将指向不同的笔", idA, idB)
	}
}

func TestTxIDOfRejectsMissingRawData(t *testing.T) {
	if _, err := txIDOf(&core.Transaction{}); err == nil {
		t.Fatal("没有 raw_data 的交易必须报错，不能回一个空号")
	}
}

func sampleTransactions() []*core.Transaction {
	mk := func(num, exp, amt int64, withContract bool) *core.Transaction {
		data, err := trc20TransferCalldata(tronOfficialUSDT, amt)
		if err != nil {
			panic(err)
		}
		raw := &core.TransactionRaw{
			RefBlockNum: num, RefBlockBytes: []byte{byte(num), byte(num >> 8)},
			Expiration: exp, Timestamp: exp - 60_000, FeeLimit: defaultTRC20FeeLimitSun, Data: data,
		}
		if withContract {
			value, err := anypb.New(&core.TriggerSmartContract{
				OwnerAddress:    addrBytes(tronOfficialUSDT),
				ContractAddress: addrBytes(tronRangeAcct),
				Data:            data,
			})
			if err != nil {
				panic(err)
			}
			raw.Contract = []*core.Transaction_Contract{{
				Type:      core.Transaction_Contract_TriggerSmartContract,
				Parameter: value,
			}}
		}
		return &core.Transaction{RawData: raw}
	}
	return []*core.Transaction{mk(1, 1700000000000, 1, false), mk(999999, 1700000060000, 3_000_000_000, true), mk(0, 0, 0, false)}
}

func addrBytes(s string) []byte {
	raw, err := decodeTRONAddress(s)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestDecodeTRONAddressMatchesSDK(t *testing.T) {
	for _, s := range []string{tronOfficialUSDT, tronRangeAcct} {
		got, err := decodeTRONAddress(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		want, err := address.Base58ToAddress(s)
		if err != nil {
			t.Fatal(err)
		}
		// 本包解出完整 25 字节（含 4 字节校验和），SDK 的 Address 是 21 字节（不含校验和）：
		// 前 21 字节必须逐字相等，这才是「两侧对同一个地址的理解一致」。
		if !bytes.Equal(got[:21], want.Bytes()) {
			t.Errorf("%s 解出的地址字节与 SDK 不一致: %x / %x", s, got[:21], want.Bytes())
		}
		if len(got) != 25 {
			t.Errorf("%s 应解出 25 字节（含校验和），实得 %d", s, len(got))
		}
	}
}

func TestDecodeTRONAddressRejectsBadChecksum(t *testing.T) {
	// 改掉末位字符就是校验和错，必须被拒：这是「格式看着对、实际是别人地址」那类错里
	// 唯一能在本地拦下的一段。
	if _, err := decodeTRONAddress(tronBogusChecksum); err == nil {
		t.Fatal("校验和被改坏的地址应当被拒")
	}
	if _, err := decodeTRONAddress(""); err == nil {
		t.Fatal("空地址应当被拒")
	}
}

func TestTrc20AddressParameterPadding(t *testing.T) {
	got, err := trc20AddressParameter(tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != tvmWordLen*2 {
		t.Fatalf("balanceOf 参数应为 %d 个十六进制字符，实得 %d", tvmWordLen*2, len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("0", 24)) {
		t.Errorf("地址参数左侧没补零: %s", got)
	}
}

func TestBigToInt64Bounds(t *testing.T) {
	if _, err := bigToInt64(new(big.Int).Lsh(big.NewInt(1), 63)); err == nil {
		t.Fatal("超出 int64 的链上数值必须报错，截断会被读成「余额够用」")
	}
	if _, err := bigToInt64(big.NewInt(-1)); err == nil {
		t.Fatal("负余额必须报错")
	}
	if v, err := bigToInt64(big.NewInt(3_000_000_000)); err != nil || v != 3_000_000_000 {
		t.Fatalf("正常值被改动: %d %v", v, err)
	}
}

func TestReceiptVerdict(t *testing.T) {
	// 判据只认回执里的执行结果，不认 contractResult 字节：USDT-TRC20 的 transfer
	// 不返回 bool，真机上那笔成功转账的 contractResult 是 32 字节全零。
	// 拿字节判会把每一笔真成功的出款读成失败，上游据此重下一单就是业务层双付。
	zeroWord := make([]byte, tvmWordLen)
	cases := []struct {
		name        string
		txResult    core.TransactionInfoCode
		contractRet core.Transaction_ResultContractResult
		hasReceipt  bool
		wantOK      bool
		wantKnown   bool
	}{
		{"成功", core.TransactionInfo_SUCESS, core.Transaction_Result_SUCCESS, true, true, true},
		{"合约回滚", core.TransactionInfo_SUCESS, core.Transaction_Result_REVERT, true, false, true},
		{"能量耗尽", core.TransactionInfo_SUCESS, core.Transaction_Result_OUT_OF_ENERGY, true, false, true},
		{"交易级失败", core.TransactionInfo_FAILED, core.Transaction_Result_SUCCESS, true, false, true},
		{"没有回执", core.TransactionInfo_SUCESS, core.Transaction_Result_DEFAULT, false, false, false},
		{"回执未回填执行结果", core.TransactionInfo_SUCESS, core.Transaction_Result_DEFAULT, true, false, false},
	}
	for _, cs := range cases {
		ok, known := receiptVerdict(cs.txResult, cs.contractRet, cs.hasReceipt)
		if ok != cs.wantOK || known != cs.wantKnown {
			t.Errorf("%s: 判成 success=%v known=%v，期望 %v/%v", cs.name, ok, known, cs.wantOK, cs.wantKnown)
		}
	}
	// 真机形态回归：USDT 成功转账的 contractResult 是全零字，判定必须仍为成功。
	ok, known := receiptVerdict(core.TransactionInfo_SUCESS, core.Transaction_Result_SUCCESS, true)
	if !ok || !known {
		t.Fatalf("全零 contractResult 的成功转账被判成 success=%v known=%v（%x）", ok, known, zeroWord)
	}
}
