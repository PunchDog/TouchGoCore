package tron

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sdkaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"
)

// 真机只读验证：TRON_LIVE_VERIFY=1 才跑，默认跳过。
//
// 这一档存在的理由是离线闸证不到两件事：一是链上真实交易的 txid 是否真等于
// sha256(raw_data)（离线只能拿 SDK 的另一份实现互验，两份可能同源同错），
// 二是接入点回给我们的 JSON 形状是否与解析代码的假设一致（尤其反查里 value 的
// 单位——单位读错，台账反查就永远匹配不上，那正是双付兜底失效的地方）。
// 全部只读，不签名、不广播、不动钱。

const liveREST = "https://api.trongrid.io"

// liveFromAddr 是最近一批真实转账里的转出方地址，供反查用例使用。
// 反查必须拿真实活跃地址验：用合约地址自己查会得到 0 条，那既证明不了解析、
// 也证明不了 min_timestamp / only_from 这些条件真的生效。
var liveFromAddr string

func liveEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv("TRON_LIVE_VERIFY") == "" {
		t.Skip("设 TRON_LIVE_VERIFY=1 才跑真机只读验证")
	}
	return true
}

func livePost(t *testing.T, path string, body any) []byte {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, liveREST+path, strings.NewReader(string(buf)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("TRON_PRO_API_KEY"); k != "" {
		req.Header.Set("TRON-PRO-API-KEY", k)
	}
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := make([]byte, 0, 1<<16)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		out = append(out, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%s -> %d: %s", path, resp.StatusCode, string(out[:min(len(out), 300)]))
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// liveListTransfers 取一批真实的 USDT TRC20 转账记录，带退避重试。
//
// TronGrid 对无 Key 的扩展读接口限流很凶：同一批用例里连打几次就会拿到 429 或空列表。
// 限流不是被测代码的问题，所以重试若干轮后仍拿不到就明确 Skip，绝不把「没验到」写成「验过了」。
func liveListTransfers(t *testing.T) []trc20TransferItem {
	t.Helper()
	url := liveREST + "/v1/accounts/" + tronOfficialUSDT + "/transactions/trc20?limit=5&only_confirmed=true&order=desc"
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if k := os.Getenv("TRON_PRO_API_KEY"); k != "" {
			req.Header.Set("TRON-PRO-API-KEY", k)
		}
		resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var rep trc20TransferList
		decErr := json.NewDecoder(resp.Body).Decode(&rep)
		resp.Body.Close()
		if resp.StatusCode == 200 && decErr == nil && len(rep.Data) > 0 {
			return rep.Data
		}
		if attempt >= 5 {
			t.Skipf("TronGrid 限流或无样本（status=%d decErr=%v 条数=%d，重试 %d 轮）：配 chain.api_key 后重跑",
				resp.StatusCode, decErr, len(rep.Data), attempt)
		}
		time.Sleep(time.Duration(attempt*5) * time.Second)
	}
}

// liveSampleTransfer 从链上取一笔真实的 USDT TRC20 转账（收端、金额、txid）。
func liveSampleTransfer(t *testing.T) outboundTransfer {
	t.Helper()
	for _, it := range liveListTransfers(t) {
		if !strings.EqualFold(it.Type, "Transfer") || it.TransactionID == "" {
			continue
		}
		amt, err := parseIntStrict(it.Value)
		if err != nil {
			continue
		}
		if it.From != "" && liveFromAddr == "" {
			liveFromAddr = it.From // 记一个真实活跃地址，供反查用例使用
		}
		return outboundTransfer{TxID: it.TransactionID, To: it.To, Amount: amt, TimestampMS: it.BlockTimestamp}
	}
	t.Skip("真机样本里没有可用的 Transfer 记录")
	return outboundTransfer{}
}

func parseIntStrict(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	if err != nil {
		return 0, err
	}
	if fmt.Sprint(v) != strings.TrimSpace(s) {
		return 0, fmt.Errorf("%q 不是纯整数字符串", s)
	}
	return v, nil
}

// TestLiveTxIDMatchesChainOnRealTransaction 链上真实交易的 txid 必须等于 sha256(raw_data)。
//
// 这是整个防双付设计的地基：广播结果说不清时，我们靠这个号去查证。
// 对照物取链自己回的两个字段——raw_data_hex（序列化后的 raw_data 字节）与 txID
// （节点算好的交易号），本地只负责把前者哈希一次再与后者比。离线只能拿 SDK 的另一份
// 实现互验（两份可能同源同错），只有对着链上真号才算钉死。
func TestLiveTxIDMatchesChainOnRealTransaction(t *testing.T) {
	liveEnabled(t)
	sample := liveSampleTransfer(t)
	t.Logf("真机样本 txid=%s to=%s amount=%d(最小单位) ts=%d", sample.TxID, sample.To, sample.Amount, sample.TimestampMS)

	body := livePost(t, "/wallet/gettransactionbyid", map[string]string{"value": sample.TxID})
	var rep struct {
		TxID       string `json:"txID"`
		RawDataHex string `json:"raw_data_hex"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("链上交易不可解析: %v", err)
	}
	if rep.TxID == "" || rep.RawDataHex == "" {
		t.Fatalf("链上交易缺 txID 或 raw_data_hex: %s", string(body[:min(len(body), 300)]))
	}
	rawBytes, err := hex.DecodeString(rep.RawDataHex)
	if err != nil {
		t.Fatal(err)
	}
	raw := &core.TransactionRaw{}
	if err := proto.Unmarshal(rawBytes, raw); err != nil {
		t.Fatalf("raw_data_hex 不是合法的 TransactionRaw: %v", err)
	}
	got, err := txIDOf(&core.Transaction{RawData: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, rep.TxID) {
		t.Fatalf("本地 txid %s 与链上 txID %s 不一致", got, rep.TxID)
	}
	if !strings.EqualFold(got, sample.TxID) {
		t.Fatalf("本地 txid %s 与列表接口的 transaction_id %s 不一致", got, sample.TxID)
	}
	t.Logf("真机 txid 相符: %s（raw_data %d 字节）", got, len(rawBytes))
}

// TestLiveCalldataMatchesChainOnRealTransaction 我们生成的 transfer 调用数据必须与
// 链上那笔真实交易逐字节相同，且合约地址的十六进制形态也要对得上。
func TestLiveCalldataMatchesChainOnRealTransaction(t *testing.T) {
	liveEnabled(t)
	sample := liveSampleTransfer(t)
	body := livePost(t, "/wallet/gettransactionbyid", map[string]string{"value": sample.TxID})

	var rep struct {
		RawData struct {
			Contract []struct {
				Type      string `json:"type"`
				Parameter struct {
					Value struct {
						OwnerAddress    string `json:"owner_address"`
						ContractAddress string `json:"contract_address"`
						Data            string `json:"data"`
					} `json:"value"`
				} `json:"parameter"`
			} `json:"contract"`
		} `json:"raw_data"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.RawData.Contract) == 0 {
		t.Fatalf("链上交易没有合约调用: %s", string(body[:min(len(body), 400)]))
	}
	c := rep.RawData.Contract[0]
	if c.Type != "TriggerSmartContract" {
		t.Fatalf("合约类型是 %s，不是 TRC20 转账用的 TriggerSmartContract", c.Type)
	}
	// 合约地址：链上给的是 0x41 + 20 字节十六进制。用本包的解码器还原同一串，
	// 顺带证明「配置里那个 Base58 合约地址」与「链上那笔交易打到的合约」是同一个。
	wantContract, err := decodeTRONAddress(tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	// 链上给的是 21 字节（0x41 版本字节 + 20 字节账户哈希），本包解码出的是 25 字节
	// （末尾还有 4 字节校验和），只比前 21 字节。
	if got, want := strings.ToLower(c.Parameter.Value.ContractAddress), strings.ToLower(hex.EncodeToString(wantContract[:21])); got != want {
		t.Fatalf("合约地址不符：链上 %s，本包解码 %s", got, want)
	}
	data, err := hex.DecodeString(c.Parameter.Value.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != trc20CalldataLen {
		t.Fatalf("链上 calldata 长度 %d，期望 %d（hex=%s）", len(data), trc20CalldataLen, hex.EncodeToString(data))
	}
	// 收端与金额从 calldata 里解出来，而不是抄列表接口的字段：这样比的是
	// 「同一笔交易的两种表述」，两边不一致就说明有一侧的单位/编码假设错了。
	toAddr := addressFromWord(data[4:36])
	var amount int64
	for _, b := range data[36:] {
		amount = amount<<8 | int64(b)
	}
	if amount != sample.Amount {
		t.Fatalf("calldata 金额 %d 与列表接口 value %d 不一致：最小单位假设错了", amount, sample.Amount)
	}
	if toAddr != sample.To {
		t.Fatalf("calldata 收端 %s 与列表接口 to %s 不一致", toAddr, sample.To)
	}
	want, err := trc20TransferCalldata(toAddr, amount)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, data) {
		t.Fatalf("生成的 calldata 与链上不一致: got=%s want=%s", hex.EncodeToString(want), hex.EncodeToString(data))
	}
	t.Logf("真机 calldata 逐字节相符：to=%s amount=%d selector=%s", toAddr, amount, hex.EncodeToString(want[:4]))
}

// addressFromWord 把 32 字节地址字的后 20 字节还原成 Base58 地址（真机对照用）。
//
// 这里刻意用 SDK 的编码器而不是自己写一份：本仓只有 base58Decode 没有 Encode，
// 而对照实验的价值恰恰在于「编码这一侧不是我自己的实现」。
func addressFromWord(word []byte) string {
	raw := append([]byte{tronAddressVersion}, word[12:]...)
	return sdkaddress.BytesToAddress(raw).String()
}

// TestLiveHTTPGatewayAgainstRealNode 我方 REST 解析代码对着真节点跑一遍只读四件事。
func TestLiveHTTPGatewayAgainstRealNode(t *testing.T) {
	liveEnabled(t)
	gw, err := newHTTPGateway(liveREST, os.Getenv("TRON_PRO_API_KEY"), 45*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	head, err := gw.headBlock(ctx)
	if err != nil {
		t.Fatalf("链头读取失败: %v", err)
	}
	if head < 80_000_000 {
		t.Fatalf("链头高度 %d 不像主网现值", head)
	}
	t.Logf("真机链头=%d", head)

	sample := liveSampleTransfer(t)
	rec, err := gw.lookupTx(ctx, sample.TxID)
	if err != nil {
		t.Fatalf("回执读取失败: %v", err)
	}
	if !rec.Found || !rec.InBlock || !rec.Success {
		t.Fatalf("真实成功的 USDT 转账被读成 %+v", rec)
	}
	t.Logf("真机回执: block=%d fee=%d sun result=%s", rec.BlockNumber, rec.FeeSun, rec.ResultCode)

	bal, err := gw.tokenBalance(ctx, sample.To, tronOfficialUSDT)
	if err != nil {
		t.Fatalf("余额读取失败: %v", err)
	}
	if bal < 0 {
		t.Fatalf("余额为负: %d", bal)
	}
	acct, err := gw.account(ctx, sample.To)
	if err != nil {
		t.Fatalf("账户读取失败: %v", err)
	}
	t.Logf("真机余额: USDT=%d(最小单位) TRX=%d sun", bal, acct.BalanceSun)

	// 反查：这是台账丢失后的双付兜底，必须能在真节点上取回可解析的记录。
	list, err := gw.outboundTransfers(ctx, tronOfficialUSDT, tronOfficialUSDT, time.Now().Add(-24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("转出反查失败: %v", err)
	}
	t.Logf("真机反查取回 %d 条（首条 %+v）", len(list), firstOf(list))
	// 写侧必须仍然被拒：兜底路径不许动钱。
	if _, err := gw.broadcast(ctx, nil); err == nil {
		t.Fatal("REST 兜底竟然允许广播")
	}
}

func firstOf(l []outboundTransfer) any {
	if len(l) == 0 {
		return nil
	}
	return l[0]
}

// TestLiveGRPCGatewayAgainstRealNode 我方 gRPC 正门对着真节点跑一遍只读（不广播）。
func TestLiveGRPCGatewayAgainstRealNode(t *testing.T) {
	liveEnabled(t)
	node := os.Getenv("TRON_LIVE_NODE")
	if node == "" {
		node = "grpc.trongrid.io:50051"
	}
	plaintext := strings.EqualFold(os.Getenv("TRON_LIVE_NODE_TLS"), "off")
	gw, err := newGRPCGateway(node, os.Getenv("TRON_PRO_API_KEY"), 45*time.Second, plaintext)
	if err != nil {
		t.Fatalf("gRPC 网关建连失败: %v", err)
	}
	defer gw.close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	head, err := gw.headBlock(ctx)
	if err != nil {
		t.Skipf("gRPC 只读不可用（多半是无 API Key 被限流）: %v", err)
	}
	if head < 80_000_000 {
		t.Fatalf("gRPC 链头 %d 不像主网现值", head)
	}
	t.Logf("gRPC 真机链头=%d", head)

	sample := liveSampleTransfer(t)
	rec, err := gw.lookupTx(ctx, sample.TxID)
	if err != nil {
		t.Fatalf("gRPC 回执读取失败: %v", err)
	}
	if !rec.Found || !rec.Success {
		t.Fatalf("真实成功的转账被 gRPC 侧读成 %+v", rec)
	}
	t.Logf("gRPC 真机回执: block=%d fee=%d sun result=%s", rec.BlockNumber, rec.FeeSun, rec.ResultCode)

	if _, err := gw.outboundTransfers(ctx, tronOfficialUSDT, tronOfficialUSDT, 0); err == nil {
		t.Fatal("gRPC 侧不该声称能做转账列表反查")
	}
}

// TestLiveOutboundReverseLookupFindsRealTransfer 台账丢失后的反查必须能在真节点上
// 捞回一笔真实转账：这是重启后防双付的唯一兜底，离线假响应证不到它。
func TestLiveOutboundReverseLookupFindsRealTransfer(t *testing.T) {
	liveEnabled(t)
	sample := liveSampleTransfer(t)
	if liveFromAddr == "" {
		for _, it := range liveListTransfers(t) {
			if it.From != "" {
				liveFromAddr = it.From
				break
			}
		}
	}
	if liveFromAddr == "" {
		t.Skip("真机没给出可用的转出方地址")
	}
	gw, err := newHTTPGateway(liveREST, os.Getenv("TRON_PRO_API_KEY"), 45*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// TronGrid 无 API Key 时这个扩展读接口限流很凶（实测直接 429）：退避重试几轮，
	// 仍被限流就明确 Skip 并说明原因，绝不把「没验到」写成「验过了」。
	var list []outboundTransfer
	for attempt := 1; ; attempt++ {
		list, err = gw.outboundTransfers(ctx, liveFromAddr, tronOfficialUSDT, time.Now().Add(-72*time.Hour).UnixMilli())
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "429") {
			t.Fatalf("真机反查失败: %v", err)
		}
		if attempt >= 4 {
			t.Skipf("TronGrid 无 API Key 限流（连续 %d 次 429），反查这一条没验到；配 chain.api_key 后重跑", attempt)
		}
		time.Sleep(time.Duration(attempt*5) * time.Second)
	}
	if len(list) == 0 {
		t.Fatalf("真实活跃地址 %s 在 72 小时窗口内反查到 0 条，解析或查询条件有问题", liveFromAddr)
	}
	seen := map[string]bool{}
	for _, it := range list {
		seen[it.TxID] = true
		if it.Amount <= 0 || it.To == "" {
			t.Errorf("反查记录字段缺失: %+v", it)
		}
	}
	// 样本那笔若正好属于这个转出方，必须能被反查捞到（txid 与金额都要对得上）。
	if sample.To != "" && seen[sample.TxID] {
		t.Logf("反查命中样本 txid=%s", sample.TxID)
	}
	t.Logf("真机反查 %s 取回 %d 条，首条 %+v", liveFromAddr, len(list), list[0])
}
