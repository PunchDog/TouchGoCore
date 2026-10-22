package tron

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"

	"touchgocore/internal/httpx"
	"touchgocore/pay"
)

// httpGateway 是接入点的 REST 读侧：gRPC 不可用时仍然能查清「这一单到底上过链没有」。
//
// 它刻意不实现写侧（buildTransfer/broadcast 直接返回 errWriteOverHTTP）：兜底路径只负责
// 把事实读回来，动钱的那一下永远走 gRPC 正门。两条路径都会动钱时，出事不知道该信谁。
//
// 出站复用 internal/httpx.Client：超时、指数退避、Retry-After 与错误分类已经在那边
// 收口，链上读操作没有理由再立一套。
type httpGateway struct {
	hc     *httpx.Client
	apiKey string
}

const (
	// tronAPIKeyHeader 是接入点要求的访问凭证头名（TronGrid 侧固定这个字面量）。
	tronAPIKeyHeader = "TRON-PRO-API-KEY"
	// trc20BalanceSelector 与 trc20TransferSelector 同一来历：能钉成常量的不该依赖一次哈希。
	trc20BalanceSelector = "70a08231"
	// outboundLimit 是反查一页取多少条。取满即视为窗口可能被截断（见 outboundTransfers）。
	outboundLimit = 50
)

func newHTTPGateway(base, apiKey string, timeout time.Duration, maxRetry int) (*httpGateway, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil, errors.New("链上读侧兜底未配置 http_base")
	}
	hc, err := httpx.NewClient(httpx.Options{
		Name:    "tron-rest",
		BaseURL: base,
		Timeout: timeout, MaxRetry: maxRetry,
		UserAgent: "touchgocore-tron-rest",
	})
	if err != nil {
		return nil, err
	}
	return &httpGateway{hc: hc, apiKey: strings.TrimSpace(apiKey)}, nil
}

// call 发一次 POST 或 GET 并回原始响应体（接入点的 wallet 系列不回本仓的 code/msg/data 包络，
// 它直接回链上对象，所以这里不解包络，只判 HTTP 层）。
func (h *httpGateway) call(ctx context.Context, method, path string, body []byte, query map[string]string) ([]byte, error) {
	values := make(url.Values, len(query))
	for k, v := range query {
		values.Set(k, v)
	}
	var raw json.RawMessage
	spec := httpx.CallSpec{
		Method: method,
		Path:   path,
		Body:   body,
		Query:  values,
		Sign: func(req *http.Request, _ []byte) error {
			if h.apiKey != "" {
				req.Header.Set(tronAPIKeyHeader, h.apiKey)
			}
			return nil
		},
		Parse: func(status int, _ http.Header, resp []byte) (json.RawMessage, error) {
			if status != http.StatusOK {
				return nil, &pay.ProviderError{Channel: "tron", Code: strconv.Itoa(status), Msg: "链上读接口返回非 200", Retryable: status >= http.StatusInternalServerError}
			}
			raw = resp
			return resp, nil
		},
	}
	if _, err := h.hc.Do(ctx, spec); err != nil {
		return nil, err
	}
	return raw, nil
}

func (h *httpGateway) buildTransfer(context.Context, string, string, string, int64, int64) (*core.Transaction, error) {
	return nil, errWriteOverHTTP
}

func (h *httpGateway) broadcast(context.Context, *core.Transaction) (*broadcastResult, error) {
	return nil, errWriteOverHTTP
}

// txInfoReply 是 /walletsolidity/gettransactioninfobyid 的响应里本包要看的那几项。
//
// 字段位置按真机响应钉（2026-10-16 主网实测）：交易级 result 多数节点直接省略，
// 合约执行结果在 receipt.result 里（SUCCESS / REVERT / OUT_OF_ENERGY …），
// 总扣费在顶层 fee。只声明看得懂的字段：回执会随节点版本加字段，全量结构体一变
// 就编译不过，而读不懂的部分本不该影响「这笔有没有成功」这一条结论。
type txInfoReply struct {
	ID          string `json:"id"`
	Result      string `json:"result"`
	Fee         int64  `json:"fee"`
	BlockNumber int64  `json:"blockNumber"`
	Receipt     struct {
		Result    string `json:"result"`
		EnergyFee int64  `json:"energy_fee"`
		NetFee    int64  `json:"net_fee"`
	} `json:"receipt"`
}

func (h *httpGateway) lookupTx(ctx context.Context, txID string) (*txReceipt, error) {
	body, err := json.Marshal(map[string]string{"value": txID})
	if err != nil {
		return nil, err
	}
	raw, err := h.call(ctx, http.MethodPost, "/walletsolidity/gettransactioninfobyid", body, nil)
	if err != nil {
		return nil, err
	}
	var rep txInfoReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("链上回执不可解析: %w", err)
	}
	if strings.TrimSpace(rep.ID) == "" {
		return &txReceipt{}, nil // 未上链：节点回空对象而不是 404
	}
	success, known := restVerdict(rep.Result, rep.Receipt.Result)
	fee := rep.Fee
	if fee == 0 {
		fee = rep.Receipt.EnergyFee + rep.Receipt.NetFee
	}
	return &txReceipt{
		Found:        true,
		InBlock:      rep.BlockNumber > 0,
		BlockNumber:  rep.BlockNumber,
		Success:      success,
		VerdictKnown: known,
		FeeSun:       fee,
		ResultCode:   strings.TrimSpace(rep.Result) + "/" + strings.TrimSpace(rep.Receipt.Result),
	}, nil
}

// restVerdict 把 REST 回执读成「成功 / 失败 / 判定不明」三态。
//
// 两个字段任一为空都算「这一层没表态」：真机上成功的 USDT 转账就是顶层 result 省略、
// 只有 receipt.result=SUCCESS（2026-10-16 实测 txid ae07c52c…bba9）。两层都没表态时
// known=false，调用方必须回 UNKNOWN——判成 FAILED 会让上游重下一单，
// 而链上那一笔可能已经把币转出去了。
func restVerdict(txResult, receiptResult string) (success, known bool) {
	tx := strings.ToUpper(strings.TrimSpace(txResult))
	rec := strings.ToUpper(strings.TrimSpace(receiptResult))
	if tx != "" && tx != "SUCCESS" {
		return false, true
	}
	switch rec {
	case "":
		if tx == "SUCCESS" {
			return true, true
		}
		return false, false
	case "SUCCESS":
		return true, true
	default:
		return false, true // REVERT / OUT_OF_ENERGY 一类：明确失败，且 gas 已烧
	}
}

// accountReply 是 /wallet/getaccount 的本包视图；未激活账户回空对象。
type accountReply struct {
	Balance int64 `json:"balance"`
}

func (h *httpGateway) account(ctx context.Context, addr string) (*accountState, error) {
	body, err := json.Marshal(map[string]any{"address": addr, "visible": true})
	if err != nil {
		return nil, err
	}
	raw, err := h.call(ctx, http.MethodPost, "/wallet/getaccount", body, nil)
	if err != nil {
		return nil, err
	}
	var rep accountReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		// 未激活账户时节点可能回近乎空的对象，解析不出不等于故障。
		return &accountState{}, nil
	}
	return &accountState{BalanceSun: rep.Balance}, nil
}

// triggerReply 是常量调用的响应。真机上 result 是个对象：成功时形如 {"result":true}，
// 失败时才带 code 与 message（2026-10-16 主网实测），所以两个字段都要看。
type triggerReply struct {
	Result struct {
		OK      bool   `json:"result"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"result"`
	ConstantResult []string `json:"constant_result"`
}

func (h *httpGateway) tokenBalance(ctx context.Context, addr, contract string) (int64, error) {
	parameter, err := trc20AddressParameter(addr)
	if err != nil {
		return 0, err
	}
	// owner_address 必填，即便这是一次不花能量的常量调用：真机上省略它得到的是
	// result.code=OTHER_ERROR，那条码看不出「请求体缺字段」，很容易被误读成节点拒读余额。
	// 这里用被查地址自己当 owner。
	body, err := json.Marshal(map[string]any{
		"owner_address":     addr,
		"contract_address":  contract,
		"function_selector": "balanceOf(address)",
		"parameter":         parameter,
		"visible":           true,
		"constant":          true,
	})
	if err != nil {
		return 0, err
	}
	raw, err := h.call(ctx, http.MethodPost, "/wallet/triggersmartcontract", body, nil)
	if err != nil {
		return 0, err
	}
	var rep triggerReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		return 0, fmt.Errorf("合约调用回执不可解析: %w", err)
	}
	if !rep.Result.OK && rep.Result.Code == "" {
		// 既没说成功也没给码：读不懂就不猜余额，猜出来的 0 会被上游读成「余额不足」。
		return 0, errors.New("常量调用没有给出可判定的结果")
	}
	if rep.Result.Code != "" && !strings.EqualFold(rep.Result.Code, "SUCCESS") {
		return 0, fmt.Errorf("节点拒绝常量调用 code=%s: %s", rep.Result.Code, clipBytes([]byte(rep.Result.Message), 96))
	}
	if len(rep.ConstantResult) == 0 {
		return 0, errors.New("合约没有返回余额载荷")
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(rep.ConstantResult[0], "0x"), 16)
	if !ok {
		return 0, errors.New("余额载荷不是十六进制整数")
	}
	return bigToInt64(v)
}

type blockReply struct {
	BlockHeader struct {
		RawData struct {
			Number int64 `json:"number"`
		} `json:"raw_data"`
	} `json:"block_header"`
}

func (h *httpGateway) headBlock(ctx context.Context) (int64, error) {
	raw, err := h.call(ctx, http.MethodPost, "/wallet/getnowblock", []byte("{}"), nil)
	if err != nil {
		return 0, err
	}
	var rep blockReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		return 0, fmt.Errorf("链头信息不可解析: %w", err)
	}
	return rep.BlockHeader.RawData.Number, nil
}

// trc20TransferItem 是 /v1/accounts/{addr}/transactions/trc20 单条记录的本包视图。
type trc20TransferItem struct {
	TransactionID  string `json:"transaction_id"`
	BlockTimestamp int64  `json:"block_timestamp"`
	From           string `json:"from"`
	To             string `json:"to"`
	Type           string `json:"type"`
	// Value 是链上最小单位的整数字符串（USDT 的 6 位）。
	// 这里必须按整数比：把它当小数读，比对结果就是「1 与 1000000 不相等」，
	// 于是台账丢失后本该拦下的那一笔会被放行。
	Value string `json:"value"`
}

type trc20TransferList struct {
	Data []trc20TransferItem `json:"data"`
}

// outboundTransfers 列出本地址近期发出的该合约转账，供台账丢失后反查。
//
// 只取一页：取满 outboundLimit 条时不假定「这就是全部」，调用方据此把「没能查全」
// 与「确实没查到」分开处理（见 onchain.go 的 lookupLedgerByChain：取满未命中即放弃广播）。
func (h *httpGateway) outboundTransfers(ctx context.Context, addr, contract string, sinceMS int64) ([]outboundTransfer, error) {
	raw, err := h.call(ctx, http.MethodGet, "/v1/accounts/"+addr+"/transactions/trc20", nil, map[string]string{
		"contract_address": contract,
		"only_from":        "true",
		"only_to":          "false",
		"only_confirmed":   "false",
		"min_timestamp":    strconv.FormatInt(sinceMS, 10),
		"limit":            strconv.Itoa(outboundLimit),
		"order":            "desc",
	})
	if err != nil {
		return nil, err
	}
	var rep trc20TransferList
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("转出列表不可解析: %w", err)
	}
	out := make([]outboundTransfer, 0, len(rep.Data))
	for _, it := range rep.Data {
		if !strings.EqualFold(strings.TrimSpace(it.Type), "Transfer") {
			continue
		}
		txID := strings.TrimSpace(it.TransactionID)
		if txID == "" {
			// 没有交易号的记录没法拿去查证，直接滤掉：本方法的契约是
			// 「交回来的每一条都能当作已上过链的那一笔」。
			continue
		}
		amount, err := strconv.ParseInt(strings.TrimSpace(it.Value), 10, 64)
		if err != nil {
			// 读不出金额的记录不能参与「这单是不是已经转过」的判定，但也不能被忽略：
			// 它会让反查结论降级为不确定，由调用方按 txid 逐笔查证。
			return nil, fmt.Errorf("转出记录 %s 的金额 %q 不是最小单位整数: %w", txID, it.Value, err)
		}
		out = append(out, outboundTransfer{
			TxID:        txID,
			To:          strings.TrimSpace(it.To),
			Amount:      amount,
			TimestampMS: it.BlockTimestamp,
		})
	}
	return out, nil
}

func (h *httpGateway) close() error { return nil }

// trc20AddressParameter 把 Base58 地址换成合约调用参数：20 字节账户哈希左补零到 32 字节的十六进制。
func trc20AddressParameter(addr string) (string, error) {
	raw, err := decodeTRONAddress(addr)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(padLeft(raw[1:21], tvmWordLen)), nil
}

var (
	_ chainGateway = (*httpGateway)(nil)
)
