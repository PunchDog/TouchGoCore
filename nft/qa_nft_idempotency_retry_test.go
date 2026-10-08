package nft

// T7 包级行为闸（qa_nft 前缀，与既有 nft_test.go 互补不重复）。
// 覆盖：SPEC §3.4 幂等本地闸（跨 Provider 与发送前零请求）、§5.5 重试白名单
// （502/504/transport/read_body 禁入 Mint/Transfer 重试路径；白名单命中才重发、计数可证）、
// §3.3 回执对账三条款、§3.1/§3.2 状态机（unknown 不自动重下、success 终态不再入队）。
// 本文件自带 qa* 私有 helper，不依赖 nft_test.go 里的 helper，便于沙箱副本单独变异跑。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// qaProvider 构造指向 srv 的通用驱动 Provider；maxRetry<0 走 DefaultMaxRetry。
func qaProvider(t *testing.T, srvURL, secret string, maxRetry int) *Provider {
	t.Helper()
	endpoints := map[string]string{
		EndpointHoldings: "/e", EndpointToken: "/e", EndpointMint: "/e",
		EndpointTransfer: "/e", EndpointQuery: "/e",
	}
	p, err := NewProvider(ProviderOptions{
		Name:      "nft",
		Driver:    DriverGeneric,
		AppID:     "qa-app",
		SecretKey: secret,
		BaseURL:   srvURL,
		MaxRetry:  maxRetry,
		Endpoint: func(name string) (string, bool) {
			v, ok := endpoints[name]
			return v, ok
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// qaFitOrder 把「测试名当单号」的用例折进 CheckOrderNo 的形态闸（NF-F9 上限 64 rune）。
//
// 测试名带用例前缀与中文子测试名，常常超限；真实业务单号不会这么长，所以修的是用例
// 而不是放宽闸。不超限的名字原样返回（幂等类用例依赖的「同一输入恒得同一单号」不变），
// 超限的用 FNV-32a 摘要顶替——仍然恒等、仍然唯一。
func qaFitOrder(name string) string {
	if utf8.RuneCountInString(name) <= maxOrderNoLen {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("qa-fit-%08x", h.Sum32())
}

func qaMintOrder(orderNo string) *NftOrder {
	orderNo = qaFitOrder(orderNo)
	return &NftOrder{
		OrderNo: orderNo, Chain: ChainETH, Network: NetworkMainnet,
		ToAddress: "0xQaToAddress", Quantity: 1, MetadataURI: "ipfs://qa-meta",
	}
}

func qaTransferOrder(orderNo string) *NftOrder {
	orderNo = qaFitOrder(orderNo)
	return &NftOrder{
		OrderNo: orderNo, Chain: ChainETH, Network: NetworkMainnet,
		Contract: "0xQaContract", TokenID: "170141183460469231731687303715884105728", // uint256 级十进制字符串
		ToAddress: "0xQaToAddress", Quantity: 1,
	}
}

// qaCountingServer 起一个恒回铸造成功回执的假供应商（order_no 回显自请求体）。
func qaCountingServer(t *testing.T, hits *int64, tokenID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		orderNo, _ := body["order_no"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": CodeOK,
			"data": map[string]any{"order_no": orderNo, "status": "success", "token_id": tokenID, "quantity": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- 闸 1（补强）：幂等本地闸是包级账本，拒绝发生在「发送前」——
// 对全新假供应商的命中计数必须恒为 0；既有 nft_test.go 只测了单 Provider 内重提交。

func TestQaIdempotencyGateIsPackageLedgerAndPreSend(t *testing.T) {
	var hitsA, hitsB int64
	srvA := qaCountingServer(t, &hitsA, "1001")
	srvB := qaCountingServer(t, &hitsB, "1002")

	pA := qaProvider(t, srvA.URL, "sA", -1)
	pB := qaProvider(t, srvB.URL, "sB", -1)
	ctx := context.Background()
	base := t.Name() + "-order"

	// 1) 首见 OrderNo：pA 登记指纹并放行。
	if _, err := pA.Mint(ctx, qaMintOrder(base)); err != nil {
		t.Fatalf("首见 OrderNo 应放行: %v", err)
	}

	// 2) 换一把 Provider（pB）以同 OrderNo 改 ToAddress 重提交：
	//    闸必须仍拒绝（账本是包级 sync.Map，不随 Provider 实例走），
	//    且 pB 的假供应商 srvB 收到零请求——拒绝严格发生在发送之前。
	changed := qaMintOrder(base)
	changed.ToAddress = "0xQaChangedAddress"
	_, err := pB.Mint(ctx, changed)
	if err == nil {
		t.Fatal("跨 Provider 同 OrderNo 改字段重提交应被幂等闸拒绝")
	}
	if !strings.Contains(err.Error(), "幂等键") || !strings.Contains(err.Error(), "不得变更") {
		t.Fatalf("拒绝文案应含「幂等键」+「不得变更」，实得: %v", err)
	}
	if want := "nft: 幂等键 " + base + " 已用于不同内容的提交，不得变更"; err.Error() != want {
		t.Fatalf("拒绝文案应逐字为 %q，实得 %q", want, err.Error())
	}
	if got := atomic.LoadInt64(&hitsB); got != 0 {
		t.Fatalf("异指纹拒绝不得发送：srvB 计数应为 0，实得 %d", got)
	}

	// 3) 同 OrderNo 换「动作维度」重提交（Mint 已登记的单改走 Transfer，指纹必含差集字段）：
	//    同样发送前拒绝，srvB 仍零请求。一单一动作，OrderNo 不得跨动作复用。
	if _, err := pB.Transfer(ctx, qaTransferOrder(base)); err == nil {
		t.Fatal("Mint 已登记的 OrderNo 不得被 Transfer 复用")
	} else if !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("跨动作拒绝也应含「幂等键」，实得: %v", err)
	}
	if got := atomic.LoadInt64(&hitsB); got != 0 {
		t.Fatalf("跨动作拒绝不得发送：srvB 计数应为 0，实得 %d", got)
	}

	// 4) 同 OrderNo 同指纹经 pB 重提交 ⇒ 放行（合法透明重发，供应商幂等兜底），
	//    且报文落在 srvB 上（srvA 计数不变）。
	if _, err := pB.Mint(ctx, qaMintOrder(base)); err != nil {
		t.Fatalf("同指纹跨 Provider 重发应放行: %v", err)
	}
	if got := atomic.LoadInt64(&hitsB); got != 1 {
		t.Fatalf("同指纹重发应发送 1 次，srvB 计数=1，实得 %d", got)
	}
	if got := atomic.LoadInt64(&hitsA); got != 1 {
		t.Fatalf("重发不得回灌 srvA，srvA 计数=1，实得 %d", got)
	}
}

// ---- 闸 3：重试白名单（7eb7b89 口径 nft 版，补强既有空白名单用例）----

func TestQaRetryWhitelistMembersExcluded(t *testing.T) {
	// 默认白名单里绝不含 502/504 与「受理未知」类码——它们根本不该是白名单成员。
	for _, code := range []string{"502", "504", "transport", "read_body"} {
		if IsRetryableCode(code) {
			t.Fatalf("默认白名单不得含 %q（受理未知类码，原单重发=二次铸造/双花）", code)
		}
	}
}

func TestQaStrictPathNeverResends502GatewayPages(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"502-with-success-envelope", http.StatusBadGateway},
		{"504-with-success-envelope", http.StatusGatewayTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&hits, 1)
				w.WriteHeader(tc.status)
				// 网关页里偶然出现的 "code":"0" 不得被当作受理（§5.3）。
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": CodeOK, "data": map[string]any{"status": "success", "token_id": "66"},
				})
			}))
			defer srv.Close()
			p := qaProvider(t, srv.URL, "k", -1)
			order := qaMintOrder(t.Name())
			res, err := p.Mint(context.Background(), order)
			if err == nil {
				t.Fatal("非 2xx 网关页即使包络 code=0 也不得判成功")
			}
			// NF-F5③ 订正了本用例原先「非 2xx 不得产出归一回执」的断言：请求已经写出，
			// nil 回执会让上游按「没发过」分支另起新单号重下——那是二次铸造。
			// 已外呼的失败必须带 UNKNOWN 回执并进对账。
			if res == nil || res.Status != StatusUnknown {
				t.Fatalf("已外发的非 2xx 必须给 UNKNOWN 回执，实得 %+v", res)
			}
			if !NeedsReconcile(res, err) {
				t.Fatal("非 2xx 网关页必须 NeedsReconcile==true")
			}
			pe, ok := ProviderErrorOf(err)
			if !ok {
				t.Fatalf("应为 ProviderError，实得 %T %v", err, err)
			}
			if pe.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus 应透传 %d，实得 %d", tc.status, pe.HTTPStatus)
			}
			if pe.Retryable {
				t.Fatalf("%d 在严格路径且未登记白名单时必须不可重试", tc.status)
			}
			if !InSubmitDoubt(err) {
				t.Fatalf("%d 属 InSubmitDoubt 集合（可能已受理）", tc.status)
			}
			// 计数可证：恰好 1 次外呼，无自动重发。
			if got := atomic.LoadInt64(&hits); got != 1 {
				t.Fatalf("严格路径未命中白名单应恰好发送 1 次，实得 %d", got)
			}
		})
	}
}

func TestQaWhitelistedEnvelopeCodeResendsExactlyCounted(t *testing.T) {
	const qaBusyCode = "qa9001" // 演示用「明确未受理」业务码，测试结束撤销登记。
	t.Cleanup(func() { DeleteRetryableCode(qaBusyCode) })
	DeleteRetryableCode(qaBusyCode) // -count≥2 幂等前置。

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": qaBusyCode, "msg": "queued, not accepted"})
	}))
	defer srv.Close()

	// 未登记白名单：命中同码也不重发（1 次）。
	p0 := qaProvider(t, srv.URL, "k", 1) // MaxRetry=1 ⇒ 命中白名单时恰好 1+1=2 次外呼。
	if _, err := p0.Mint(context.Background(), qaMintOrder(t.Name()+"-off")); err == nil {
		t.Fatal("包络失败码应报错")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("白名单外业务码不得重发，应 1 次，实得 %d", got)
	}

	// 登记白名单后：命中该码才重发；MaxRetry=1 ⇒ 恰好 1+1=2 次外呼，计数可证。
	atomic.StoreInt64(&hits, 0)
	SetRetryableCodes(qaBusyCode)
	if !IsRetryableCode(qaBusyCode) {
		t.Fatal("登记后应为白名单成员")
	}
	_, err := p0.Mint(context.Background(), qaMintOrder(t.Name()+"-on"))
	if err == nil {
		t.Fatal("白名单码最终仍应报错（重试耗尽）")
	}
	if !IsProviderRetryable(err) {
		t.Fatal("白名单命中时 Retryable 应为 true")
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("MaxRetry=1 且白名单命中应恰好 2 次外呼，实得 %d", got)
	}

	// Transfer 同闸：白名单码在转账严格路径同样只按登记重发，计数可证。
	atomic.StoreInt64(&hits, 0)
	if _, err := p0.Transfer(context.Background(), qaTransferOrder(t.Name()+"-tr")); err == nil {
		t.Fatal("转账包络失败应报错")
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("Transfer 命中白名单也应 2 次外呼，实得 %d", got)
	}

	// 撤销登记后回到不重发（新 OrderNo，1 次）。
	DeleteRetryableCode(qaBusyCode)
	atomic.StoreInt64(&hits, 0)
	if _, err := p0.Mint(context.Background(), qaMintOrder(t.Name()+"-del")); err == nil {
		t.Fatal("包络失败应报错")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("撤销登记后不得重发，应 1 次，实得 %d", got)
	}
}

func TestQaStrictTransportErrorsNotResentDoubtTrue(t *testing.T) {
	srv := qaCountingServer(t, new(int64), "1")
	badURL := srv.URL
	srv.Close() // 关闭后连接被拒 ⇒ transport 类「受理未知」错误。

	p := qaProvider(t, badURL, "k", 2) // MaxRetry=2 也不可被用上：严格路径钉死传输层。
	_, err := p.Mint(context.Background(), qaMintOrder(t.Name()+"-tx"))
	pe, ok := ProviderErrorOf(err)
	if !ok {
		t.Fatalf("应为 ProviderError，实得 %T %v", err, err)
	}
	if pe.Code != "transport" {
		t.Fatalf("应为 transport 类错误，实得 code=%q", pe.Code)
	}
	if pe.Retryable {
		t.Fatal("Mint 严格路径下 transport 错误必须不可自动重发（noTransportRetry）")
	}
	if !InSubmitDoubt(err) {
		t.Fatal("transport 属 InSubmitDoubt 集合")
	}
	if NeedsReconcile(nil, err) != true {
		t.Fatal("transport 失败应 NeedsReconcile==true（进对账而非重发）")
	}

	// Transfer 同样钉死。
	_, err = p.Transfer(context.Background(), qaTransferOrder(t.Name()+"-tx2"))
	if pe, ok := ProviderErrorOf(err); !ok || pe.Code != "transport" || pe.Retryable {
		t.Fatalf("Transfer 严格路径 transport 应不可重发，实得 %+v ok=%v", pe, ok)
	}
	if !InSubmitDoubt(err) {
		t.Fatal("Transfer 的 transport 也属 InSubmitDoubt")
	}
}

func TestQaReadOnlyActionsRetryOnServerErrors(t *testing.T) {
	// 只读三动作走宽松路径：5xx 默认可重试——与严格路径互为对照，
	// 证明「重发只发生在允许重发的一侧」。
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := qaProvider(t, srv.URL, "k", 1) // 1 次重试 ⇒ 1+1=2 次外呼。
	if _, err := p.QueryOrder(context.Background(), t.Name()+"-ro"); err == nil {
		t.Fatal("503 应报错")
	} else if pe, ok := ProviderErrorOf(err); !ok || !pe.Retryable {
		t.Fatalf("只读路径 5xx 应标记可重试，实得 %+v", pe)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("只读路径 5xx 应重发一次（共 2 次外呼），实得 %d", got)
	}
}

// ---- 闸 2：状态机与回执对账补强（既有文件未覆盖 §3.3 三条款与 success/unknown 处置面）----

func TestQaReceiptConsistencyClauses(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(o *NftOrder)
		data    map[string]any
		wantSub string
	}{
		{
			name:    "回执订单号不一致",
			mutate:  func(_ *NftOrder) {},
			data:    map[string]any{"order_no": "OTHER-NO", "status": "success", "token_id": "9", "quantity": 1},
			wantSub: "回执订单号",
		},
		{
			name:    "回执件数不一致",
			mutate:  func(o *NftOrder) { o.Quantity = 3 },
			data:    map[string]any{"status": "success", "token_id": "9", "quantity": 2},
			wantSub: "回执件数",
		},
		{
			name:    "mint 成功未带 token_id",
			mutate:  func(_ *NftOrder) {},
			data:    map[string]any{"status": "success", "quantity": 1},
			wantSub: "未带 token_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK, "data": tc.data})
			}))
			defer srv.Close()
			p := qaProvider(t, srv.URL, "k", -1)
			order := qaMintOrder(t.Name())
			tc.mutate(order)
			res, err := p.Mint(context.Background(), order)
			if err == nil {
				t.Fatalf("%s 应报错", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("报错应含 %q，实得 %v", tc.wantSub, err)
			}
			if res == nil {
				t.Fatal("对账不一致时归一回执仍应带回（供上游核对）")
			}
			if got := atomic.LoadInt64(&hits); got != 1 {
				t.Fatalf("对账不一致不得重发，应 1 次外呼，实得 %d", got)
			}
		})
	}
}

func TestQaSuccessTerminalFinalAndUnknownNeverResends(t *testing.T) {
	t.Run("success 终态不可逆料", func(t *testing.T) {
		var hits int64
		srv := qaCountingServer(t, &hits, "77")
		p := qaProvider(t, srv.URL, "k", -1)
		res, err := p.Mint(context.Background(), qaMintOrder(t.Name()))
		if err != nil {
			t.Fatalf("成功铸造不应报错: %v", err)
		}
		if res.Status != StatusSuccess || !res.IsFinal() {
			t.Fatalf("success 应为终态，实得 %+v", res)
		}
		if NeedsReconcile(res, nil) {
			t.Fatal("success 终态不得再进对账队列（终态不可逆料的判据面）")
		}
		if IsFinal(StatusPending) || IsFinal(StatusUnknown) {
			t.Fatal("pending/unknown 不是终态")
		}
		// Result 归一不得把 success 改写为其它状态。
		if got := p.Result(json.RawMessage(`{"status":"success","token_id":"77"}`), "X").Status; got != StatusSuccess {
			t.Fatalf("归一不得改写 success，实得 %q", got)
		}
	})

	t.Run("unknown 不自动重下不判死", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": CodeOK,
				"data": map[string]any{"status": "frozen-in-space"}, // 未登记状态词
			})
		}))
		defer srv.Close()
		p := qaProvider(t, srv.URL, "k", 2) // 即便允许 2 次重试，unknown 也不得触发重发。
		res, err := p.Mint(context.Background(), qaMintOrder(t.Name()))
		if err != nil {
			t.Fatalf("unknown 归一不应报错: %v", err)
		}
		if res.Status != StatusUnknown || res.IsFinal() {
			t.Fatalf("应归一 UNKNOWN 非终态，实得 %+v", res)
		}
		if !NeedsReconcile(res, nil) {
			t.Fatal("UNKNOWN 必须 NeedsReconcile==true")
		}
		if !strings.Contains(res.RawNote, "状态词未登记") {
			t.Fatalf("RawNote 应记未登记状态词，实得 %q", res.RawNote)
		}
		if got := atomic.LoadInt64(&hits); got != 1 {
			t.Fatalf("unknown 不得自动重下单，应恰好 1 次外呼，实得 %d", got)
		}
	})

	t.Run("空载荷归一 unknown 不报错", func(t *testing.T) {
		// §3.2：请求已发出，读空载荷不得报错（报错=上游误以为没发过而重发同单）。
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK}) // data 空、body 透出
		}))
		defer srv.Close()
		p := qaProvider(t, srv.URL, "k", -1)
		res := p.Result(json.RawMessage(""), "X")
		if res.Status != StatusUnknown || res.RawNote != "空响应" {
			t.Fatalf("空载荷应归一 UNKNOWN+空响应，实得 %+v", res)
		}
		r2, err2 := p.QueryOrder(context.Background(), qaFitOrder(t.Name()+"-emptypayload"))
		// 该 srv 回 code=0 无 data ⇒ Data 空透出整个 body ⇒ 载荷不可解析 ⇒ UNKNOWN。
		if err2 != nil {
			t.Fatalf("空 data 归一不应报错: %v", err2)
		}
		if r2.Status != StatusUnknown {
			t.Fatalf("应归一 UNKNOWN，实得 %q", r2.Status)
		}
		if got := atomic.LoadInt64(&hits); got != 1 {
			t.Fatalf("不得因归一而重发，实得 %d", got)
		}
	})
}
