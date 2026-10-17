package tron

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
)

// 读侧兜底的可信度取决于它把节点响应读成了什么：这里逐字段验，全离线打假 TronGrid。

func newTestHTTP(t *testing.T, handler http.HandlerFunc) (*httpGateway, *int) {
	t.Helper()
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if got := r.Header.Get(tronAPIKeyHeader); got != fakeAPIKey {
			t.Errorf("%s 未带 %s 头，实得 %q", r.URL.Path, tronAPIKeyHeader, got)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	gw, err := newHTTPGateway(srv.URL, fakeAPIKey, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	return gw, hits
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestHTTPLookupTxReadsReceipt(t *testing.T) {
	gw, hits := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/walletsolidity/gettransactioninfobyid" {
			t.Errorf("路径应为钱包读接口，实得 %s", r.URL.Path)
		}
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["value"] != "abc123" {
			t.Errorf("查询条件没带交易号: %+v", body)
		}
		// 字段位置按真机响应：合约执行结果在 receipt.result，总扣费在顶层 fee。
		writeJSON(w, map[string]any{
			"id": "abc123", "blockNumber": 42, "fee": 13_000_100,
			"receipt": map[string]any{"energy_fee": 13_000_000, "net_fee": 100, "result": "SUCCESS"},
		})
	})
	rec, err := gw.lookupTx(context.Background(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Found || !rec.InBlock || !rec.Success || !rec.VerdictKnown || rec.BlockNumber != 42 || rec.FeeSun != 13_000_100 {
		t.Fatalf("回执读错: %+v", rec)
	}
	if *hits != 1 {
		t.Fatalf("应当只发一次请求，实得 %d", *hits)
	}
}

func TestHTTPLookupTxVerdictFromReceipt(t *testing.T) {
	// 真机形态：成功的 USDT 转账顶层 result 直接省略，只有 receipt.result=SUCCESS
	// （2026-10-16 主网 txid ae07c52c…bba9）。两层都没表态时必须是「判定不明」，
	// 因为判成 FAILED 会让上游重下一单，而链上那一笔可能已经把币转出去了。
	cases := map[string]struct {
		txResult, receiptResult string
		wantOK, wantKnown       bool
	}{
		"真机成功形态":  {"", "SUCCESS", true, true},
		"两层都成功":   {"SUCCESS", "SUCCESS", true, true},
		"合约回滚":    {"", "REVERTED", false, true},
		"能量耗尽":    {"", "OUT_OF_ENERGY", false, true},
		"交易级失败":   {"FAILED", "SUCCESS", false, true},
		"只有交易级成功": {"SUCCESS", "", true, true},
		"两层都没表态":  {"", "", false, false},
	}
	for name, cs := range cases {
		gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"id": "x", "result": cs.txResult, "blockNumber": 1,
				"receipt": map[string]any{"result": cs.receiptResult}})
		})
		rec, err := gw.lookupTx(context.Background(), "x")
		if err != nil {
			t.Fatal(err)
		}
		if rec.Success != cs.wantOK || rec.VerdictKnown != cs.wantKnown {
			t.Errorf("%s: 判成 success=%v known=%v，期望 %v/%v（%+v）",
				name, rec.Success, rec.VerdictKnown, cs.wantOK, cs.wantKnown, rec)
		}
	}
}

// TestHTTPLookupTxFeeFallsBackToReceipt 顶层 fee 缺失时用 energy_fee+net_fee 兜底。
func TestHTTPLookupTxFeeFallsBackToReceipt(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "x", "blockNumber": 3,
			"receipt": map[string]any{"result": "SUCCESS", "energy_fee": 4_957_100, "net_fee": 345_000}})
	})
	rec, err := gw.lookupTx(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.FeeSun != 5_302_100 {
		t.Fatalf("手续费兜底算错: %d（真机同形态那笔是 5302100 sun）", rec.FeeSun)
	}
}

func TestHTTPLookupTxEmptyMeansNotFound(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	rec, err := gw.lookupTx(context.Background(), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Found || rec.Success {
		t.Fatalf("空回执必须读成「没上链」而不是「成功」: %+v", rec)
	}
}

func TestHTTPRetriesOnServerError(t *testing.T) {
	var n int
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"id": "x", "result": "SUCCESS", "blockNumber": 7})
	})
	rec, err := gw.lookupTx(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.InBlock || n != 2 {
		t.Fatalf("5xx 应当由共用 HTTP 层原样重发一次: hits=%d rec=%+v", n, rec)
	}
}

func TestHTTPRefusesWrites(t *testing.T) {
	// 兜底路径只读不写：会动钱的第二条路径必须不存在，而不是「配了才生效」。
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("读侧兜底不该发出任何请求，路径 %s", r.URL.Path)
	})
	if _, err := gw.buildTransfer(context.Background(), "T1", "T2", "T3", 1, 1); !errors.Is(err, errWriteOverHTTP) {
		t.Errorf("构造交易必须被拒，实得 %v", err)
	}
	if _, err := gw.broadcast(context.Background(), nil); !errors.Is(err, errWriteOverHTTP) {
		t.Errorf("广播必须被拒，实得 %v", err)
	}
}

func TestHTTPOutboundTransfersQueryAndParse(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/accounts/") || !strings.HasSuffix(r.URL.Path, "/transactions/trc20") {
			t.Errorf("反查路径不对: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("only_from") != "true" || q.Get("contract_address") != tronOfficialUSDT || q.Get("min_timestamp") != "1700000000000" {
			t.Errorf("反查条件不对: %v", q)
		}
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"transaction_id": "tx1", "to": tronAllZeroAcct, "value": "3000000000", "block_timestamp": 1, "type": "Transfer"},
			{"transaction_id": "tx2", "to": tronAllZeroAcct, "value": "1", "block_timestamp": 2, "type": "Approval"},
			{"transaction_id": "", "to": tronAllZeroAcct, "value": "9", "block_timestamp": 3, "type": "Transfer"},
		}})
	})
	out, err := gw.outboundTransfers(context.Background(), tronRangeAcct, tronOfficialUSDT, 1700000000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TxID != "tx1" || out[0].Amount != 3_000_000_000 {
		t.Fatalf("反查结果不对（Approval 与非 Transfer 必须被滤掉）: %+v", out)
	}
}

func TestHTTPOutboundRejectsFractionalAmount(t *testing.T) {
	// 链上 value 是最小单位整数字符串。把它当小数读，比对就永远不相等，
	// 于是台账丢失后本该拦下的那一笔会被放行——这是双付路径上最便宜的一道闸。
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"transaction_id": "tx1", "to": tronAllZeroAcct, "value": "3000.0", "type": "Transfer"},
		}})
	})
	if _, err := gw.outboundTransfers(context.Background(), tronRangeAcct, tronOfficialUSDT, 1); err == nil {
		t.Fatal("非整数金额必须报错，不能静默跳过")
	}
}

func TestHTTPTokenBalance(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wallet/triggersmartcontract" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["function_selector"] != "balanceOf(address)" || body["constant"] != true {
			t.Errorf("常量调用参数不对: %+v", body)
		}
		if len(body["parameter"].(string)) != tvmWordLen*2 {
			t.Errorf("balanceOf 参数长度不对: %v", body["parameter"])
		}
		// owner_address 缺了真机就回 OTHER_ERROR，而那条码看不出是请求体缺字段。
		if body["owner_address"] == "" {
			t.Error("常量调用缺 owner_address")
		}
		writeJSON(w, map[string]any{"result": map[string]any{"result": true},
			"constant_result": []string{padHex(3_000_000_000)}})
	})
	v, err := gw.tokenBalance(context.Background(), tronRangeAcct, tronOfficialUSDT)
	if err != nil {
		t.Fatal(err)
	}
	if v != 3_000_000_000 {
		t.Fatalf("余额读成 %d", v)
	}
}

func TestHTTPTokenBalanceNodeRejection(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"result": map[string]any{"code": "CONTRACT_VALIDATE_ERROR", "message": "contract not exists"}})
	})
	if _, err := gw.tokenBalance(context.Background(), tronRangeAcct, tronOfficialUSDT); err == nil {
		t.Fatal("节点拒了常量调用必须报错，不能读成 0 余额")
	}
}

// padHex 造一个 32 字节左对齐的十六进制载荷，模拟合约的常量返回值。
func padHex(v int64) string {
	h := hexOf(v)
	return strings.Repeat("0", tvmWordLen*2-len(h)) + h
}

func hexOf(v int64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	out := make([]byte, 0, 16)
	for v > 0 {
		out = append([]byte{digits[v%16]}, out...)
		v /= 16
	}
	return string(out)
}

func TestHTTPHeadBlockAndAccount(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wallet/getnowblock":
			writeJSON(w, map[string]any{"block_header": map[string]any{"raw_data": map[string]any{"number": 12345678}}})
		case "/wallet/getaccount":
			writeJSON(w, map[string]any{"address": tronRangeAcct, "balance": 500_000_000, "create_time": 1})
		default:
			t.Errorf(" unexpected path %s", r.URL.Path)
		}
	})
	head, err := gw.headBlock(context.Background())
	if err != nil || head != 12345678 {
		t.Fatalf("链头读错: %d %v", head, err)
	}
	acct, err := gw.account(context.Background(), tronRangeAcct)
	if err != nil || acct.BalanceSun != 500_000_000 {
		t.Fatalf("账户读错: %+v %v", acct, err)
	}
}

func TestHTTPAccountNotActivatedIsNotAnError(t *testing.T) {
	// 未激活账户是链上事实：回空对象要读成「不存在」，不能报故障，也不能报余额 0 之后照样发。
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	acct, err := gw.account(context.Background(), tronAllZeroAcct)
	if err != nil {
		t.Fatal(err)
	}
	if acct.BalanceSun != 0 {
		t.Fatalf("未激活账户读成了 %+v", acct)
	}
}

func TestHTTPNewGatewayRequiresBase(t *testing.T) {
	if _, err := newHTTPGateway("", fakeAPIKey, time.Second, 0); err == nil {
		t.Fatal("没配 http_base 时必须拒绝构造")
	}
}

func TestAPIKeyNeverLeaksIntoErrors(t *testing.T) {
	gw, _ := newTestHTTP(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	_, err := gw.lookupTx(context.Background(), "x")
	if err != nil && strings.Contains(err.Error(), fakeAPIKey) {
		t.Fatalf("错误文案含 API Key: %v", err)
	}
}

// ---- 分层网关：写只走正门，读只在「接入点没答上来」时换路 ----

type stubRead struct {
	recv    *txReceipt
	rerr    error
	calls   int
	fellOff bool
}

func (s *stubRead) buildTransfer(context.Context, string, string, string, int64, int64) (*core.Transaction, error) {
	s.fellOff = true
	return nil, errWriteOverHTTP
}

func (s *stubRead) broadcast(context.Context, *core.Transaction) (*broadcastResult, error) {
	s.fellOff = true
	return nil, errWriteOverHTTP
}

func (s *stubRead) lookupTx(context.Context, string) (*txReceipt, error) {
	s.calls++
	return s.recv, s.rerr
}

func (s *stubRead) tokenBalance(context.Context, string, string) (int64, error) {
	s.calls++
	return 0, s.rerr
}

func (s *stubRead) account(context.Context, string) (*accountState, error) {
	return &accountState{}, s.rerr
}
func (s *stubRead) headBlock(context.Context) (int64, error) { return 0, s.rerr }
func (s *stubRead) outboundTransfers(context.Context, string, string, int64) ([]outboundTransfer, error) {
	return nil, s.rerr
}
func (s *stubRead) close() error { return nil }

func TestTieredFallsBackOnlyOnTransportErrors(t *testing.T) {
	cases := map[string]struct {
		primaryErr error
		wantFall   bool
	}{
		"超时换读路":     {errors.New("context deadline exceeded"), true},
		"连接不可用换读路":  {errors.New("rpc error: code = Unavailable desc: connection refused"), true},
		"节点没答上来换读路": {errors.New("client not connected"), true},
		"回执解析不对不换":  {errors.New("链上回执不可解析: bad json"), false},
		"业务拒绝不换":    {errors.New("节点拒绝常量调用 code=XYZ"), false},
	}
	for name, cs := range cases {
		primary := &stubRead{rerr: cs.primaryErr}
		reader := &stubRead{recv: &txReceipt{Found: true, InBlock: true, Success: true}}
		tiered := &tieredGateway{primary: primary, reader: reader}
		_, err := tiered.lookupTx(context.Background(), "x")
		if cs.wantFall && (err != nil || !reader.recv.Found) {
			t.Errorf("%s: 应当回落到读侧，实得 err=%v", name, err)
		}
		if !cs.wantFall && reader.calls != 0 {
			t.Errorf("%s: 不该回落，却调了读侧 %d 次", name, reader.calls)
		}
		if primary.calls != 1 {
			t.Errorf("%s: 正门应当只碰一次，实得 %d", name, primary.calls)
		}
	}
}

func TestTieredNeverRoutesWritesToReader(t *testing.T) {
	reader := &stubRead{}
	tiered := &tieredGateway{primary: &stubRead{}, reader: reader}
	if _, err := tiered.broadcast(context.Background(), nil); err == nil {
		t.Fatal("正门的广播错误必须原样抛出")
	}
	if reader.fellOff {
		t.Fatal("写操作落到了读侧兜底上")
	}
}
