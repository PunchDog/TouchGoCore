package nft

// 修复闸（Lead 自写，对应 tasks/software-dev-team/nft-2026-10-01/SECURITY.md 台账）。
// 一条闸一条发现：F1 重定向凭证外泄、F2 幂等闸竞态、F3 只读报文保留字、F4 错误回显基址、
// F5 坏回执/超限回执的存疑语义、F6 导出 Call 绕严格闸、F7 数值上限、F8 基址形态、
// F9 单号形态、F10 指纹含 Memo、F11 回显控制符、F12 双凭证头同名；
// 另两条修法自身的闸：告警注入接线（provider.go 纯 stdlib 的代价）、门面随 error 带回 UNKNOWN 回执。
// 命名前缀 qa_fix_，避开被 gitignore 的 p[0-3]_*_test.go。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/util"
)

func qaFixEndpointAll(path string) func(string) (string, bool) {
	return func(string) (string, bool) { return path, true }
}

// qaFixProvider 构造指向 baseURL 的通用驱动；tokenHeader 为空走默认头名。
func qaFixProvider(t *testing.T, baseURL, tokenHeader string) *Provider {
	t.Helper()
	p, err := NewProvider(ProviderOptions{
		Name: "nft", Driver: DriverGeneric, AppID: "fix-app", MerchantID: "fix-m",
		SecretKey: "fix-secret", BaseURL: baseURL, TokenHeader: tokenHeader,
		Endpoint: qaFixEndpointAll("/e"),
	})
	if err != nil {
		t.Fatalf("NewProvider(%s): %v", baseURL, err)
	}
	return p
}

// ---- NF-F1 ----

// TestQaFixRedirectStaysOnOrigin 钉死「凭证只回同 scheme+host」：
// 跨主机重定向必须当场把 3xx 交给解析层，签名头与会话 token 不得出现在第二主机上。
func TestQaFixRedirectStaysOnOrigin(t *testing.T) {
	t.Run("跨主机302不得带签名头与token", func(t *testing.T) {
		var (
			mu         sync.Mutex
			secondSign string
			secondTok  string
			hitsB      int64
		)
		srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			secondSign = r.Header.Get("X-Sign")
			secondTok = r.Header.Get("X-Qa-Token")
			mu.Unlock()
			atomic.AddInt64(&hitsB, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK,
				"data": map[string]any{"status": "success", "token_id": "4"}})
		}))
		defer srvB.Close()

		var hitsA int64
		srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hitsA, 1)
			w.Header().Set("Location", srvB.URL+"/e")
			w.WriteHeader(http.StatusFound)
		}))
		defer srvA.Close()

		// 会话凭证刻意用一个不在 Go 跨主机自动剔除名单里的头名——那正是被攻击的面。
		p := qaFixProvider(t, srvA.URL, "X-Qa-Token")
		p.SetToken("sess-tok-leakcanary")
		res, err := p.Mint(context.Background(), qaMintOrder(t.Name()+"-cross"))
		if err == nil {
			t.Fatal("跨主机重定向应被判为非受理而报错")
		}
		mu.Lock()
		gotSign, gotTok := secondSign, secondTok
		mu.Unlock()
		if gotSign != "" || gotTok != "" {
			t.Fatalf("签名头/会话凭证头不得送到第二主机，实得 sign=%q token=%q", gotSign, gotTok)
		}
		if atomic.LoadInt64(&hitsB) != 0 {
			t.Fatalf("请求不该落到第二主机，实得 %d 次", hitsB)
		}
		if got := atomic.LoadInt64(&hitsA); got != 1 {
			t.Fatalf("3xx 应原样交给解析层（不外跳、不重发），第一主机应 1 次，实得 %d", got)
		}
		pe, ok := ProviderErrorOf(err)
		if !ok || pe.HTTPStatus != http.StatusFound {
			t.Fatalf("3xx 应按非 2xx 定性并透传状态码，实得 %v ok=%v", err, ok)
		}
		if pe.Retryable {
			t.Fatal("严格路径的 3xx 不得自动重发")
		}
		if res == nil || res.Status != StatusUnknown {
			t.Fatalf("已外发的失败必须给 UNKNOWN 回执，实得 %+v", res)
		}
	})

	t.Run("同主机重定向仍可用", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&hits, 1)
			if r.URL.Path == "/e" {
				w.Header().Set("Location", "/hop")
				w.WriteHeader(http.StatusFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": CodeOK, "data": map[string]any{"status": "success", "token_id": "5"},
			})
		}))
		defer srv.Close()
		p := qaFixProvider(t, srv.URL, "")
		res, err := p.Mint(context.Background(), qaMintOrder(t.Name()+"-same"))
		if err != nil {
			t.Fatalf("同主机一跳重定向应正常完成: %v", err)
		}
		if res.Status != StatusSuccess {
			t.Fatalf("应归一 success，实得 %+v", res)
		}
		if got := atomic.LoadInt64(&hits); got == 1 {
			t.Fatal("同主机重定向没跟（一跳都没落到落点）")
		}
	})
}

// ---- NF-F2 ----

// 幂等闸接缝 guardLoadGap 定义在 provider.go（生产侧默认 nil），本文件只负责把它撑开。

// TestQaFixIdempotencyGateAtomic 钉死幂等闸的原子性（NF-F2）：
// 第二个提交者必须在 Load 时看到第一个提交者已登记的指纹——
// 先 Load 后 Store 的写法会让两路同号异文都放行，那就是二次铸造窗口。
func TestQaFixIdempotencyGateAtomic(t *testing.T) {
	t.Run("Load与Store之间不得有放行窗口", func(t *testing.T) {
		const orderNo = "qa-fix-f2-window"
		orderLedger.Delete(orderNo)
		t.Cleanup(func() {
			orderLedger.Delete(orderNo)
			guardLoadGap = nil
		})

		second := make(chan string, 1)
		gate := make(chan struct{})
		guardLoadGap = func(string) {
			// A 已过 Load（还没登记）⇒ B 在这里完成整次登记，A 醒来时必须看见 B 的指纹。
			second <- "a-at-gap"
			<-gate
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[0] = guardIdempotency(orderNo, "fingerprint-A")
		}()
		<-second
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[1] = guardIdempotency(orderNo, "fingerprint-B")
		}()
		time.Sleep(20 * time.Millisecond) // B 走完整条路径，A 仍停在窗口里。
		close(gate)
		wg.Wait()
		guardLoadGap = nil

		if errs[1] == nil {
			t.Fatal("第二个提交者（异文）应被拒绝，实得放行——同号异文双发的窗口就是它")
		}
		if !strings.Contains(errs[1].Error(), "不得变更") {
			t.Fatalf("后到者的拒绝文案应是幂等键冲突，实得 %v", errs[1])
		}
		// 先登记者必须放行：它已经发起了这一单，拦它等于把合法提交判死。
		if errs[0] != nil {
			t.Fatalf("先登记者应放行，实得 %v（两路都拒说明账本写坏）", errs[0])
		}
	})

	t.Run("同号异文并发不得双发", func(t *testing.T) {
		// 端到端计数版：不刻意撑窗口，跑多轮同号异文并发，任何一轮出现 2 次外发即缺陷。
		const rounds = 40
		for i := 0; i < rounds; i++ {
			var hits int64
			srv := qaCountingServer(t, &hits, "1")
			orderNo := fmt.Sprintf("qa-fix-f2-e2e-%d", i) // 每轮换单号，避开包级账本的跨轮污染。
			var wg sync.WaitGroup
			start := make(chan struct{})
			ready := make(chan struct{}, 2)
			errs := make([]error, 2)
			orders := []*NftOrder{
				{OrderNo: orderNo, Chain: ChainETH, Network: NetworkMainnet,
					ToAddress: "0xAAAA", Quantity: 1, MetadataURI: "ipfs://m1"},
				{OrderNo: orderNo, Chain: ChainETH, Network: NetworkMainnet,
					ToAddress: "0xBBBB", Quantity: 1, MetadataURI: "ipfs://m1"},
			}
			for j := range orders {
				wg.Add(1)
				go func(j int) {
					defer wg.Done()
					p, err := NewProvider(ProviderOptions{Name: "nft", Driver: DriverGeneric,
						AppID: "fix-app", SecretKey: "fix-secret", BaseURL: srv.URL,
						Endpoint: qaFixEndpointAll("/e")})
					if err != nil {
						errs[j] = err
						return
					}
					ready <- struct{}{}
					<-start
					_, errs[j] = p.Mint(context.Background(), orders[j])
				}(j)
			}
			<-ready
			<-ready
			close(start)
			wg.Wait()
			srv.Close()
			if got := atomic.LoadInt64(&hits); got > 1 {
				t.Fatalf("第 %d 轮同号异文外发了 %d 次（幂等闸必须只放一路）", i, got)
			}
		}
	})

	t.Run("同号同文并发都放行", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": CodeOK, "data": map[string]any{"status": "success", "token_id": "8", "quantity": 1},
			})
		}))
		defer srv.Close()

		const orderNo = "qa-fix-f2-same-content"
		mk := func() *NftOrder {
			return &NftOrder{OrderNo: orderNo, Chain: ChainETH, Network: NetworkMainnet,
				ToAddress: "0xCCCC", Quantity: 1, MetadataURI: "ipfs://m2"}
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				p, err := NewProvider(ProviderOptions{Name: "nft", Driver: DriverGeneric,
					AppID: "fix-app", SecretKey: "fix-secret", BaseURL: srv.URL,
					Endpoint: qaFixEndpointAll("/e")})
				if err != nil {
					errs[i] = err
					return
				}
				ready <- struct{}{}
				<-start
				_, errs[i] = p.Mint(context.Background(), mk())
			}(i)
		}
		<-ready
		<-ready
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("同号同文并发卡死（透明重发不该被拦）")
		}
		if got := atomic.LoadInt64(&hits); got != 2 {
			t.Fatalf("合法透明重发不得被拦，应 2 次外发，实得 %d（errs=%v）", got, errs)
		}
	})
}

// ---- NF-F4 ----

// TestQaFixErrorMessagesDoNotEchoBaseURL 钉死「基址与签名入参不进错误链」：
// 传输层/构造层/签名层错误都不得把基址（含 path 与 query）或签名入参写进错误文案。
func TestQaFixErrorMessagesDoNotEchoBaseURL(t *testing.T) {
	t.Run("transport错误不回显基址", func(t *testing.T) {
		c, err := NewClient(Options{Name: "nft", BaseURL: "https://nft-secret-host.example/some/path"})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		_, err = c.Do(context.Background(), CallSpec{Path: "/e", Body: []byte("{}")})
		if err == nil {
			t.Fatal("不可达基址应报错")
		}
		pe, ok := ProviderErrorOf(err)
		if !ok || pe.Code != "transport" {
			t.Fatalf("应为 transport 类，实得 %v ok=%v", err, ok)
		}
		for _, leak := range []string{"nft-secret-host.example", "some/path", "/e", "://"} {
			if strings.Contains(pe.Msg, leak) || strings.Contains(err.Error(), leak) {
				t.Fatalf("错误文案回显了基址片段 %q: %v", leak, err)
			}
		}
	})

	t.Run("请求构造失败不回显URL", func(t *testing.T) {
		c, err := NewClient(Options{Name: "nft", BaseURL: "http://127.0.0.1:8080"})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		// 非法百分号转义让 Go 在「构造请求」这一步就失败，错误文案里带的是完整 URL。
		_, err = c.Do(context.Background(), CallSpec{Path: "/e%zz"})
		pe, ok := ProviderErrorOf(err)
		if !ok || pe.Code != "bad_request" {
			t.Fatalf("非法 URL 应给 bad_request，实得 %v ok=%v", err, ok)
		}
		if strings.Contains(err.Error(), "8080") {
			t.Fatalf("bad_request 不得回显完整 URL: %v", err)
		}
	})

	t.Run("签名失败不回显签名入参", func(t *testing.T) {
		srv := qaCountingServer(t, new(int64), "1")
		c, err := NewClient(Options{Name: "nft", BaseURL: srv.URL})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		const canary = "ipfs://metadata-canary-must-not-leak"
		_, derr := c.Do(context.Background(), CallSpec{
			Path: "/e", Body: []byte("{}"),
			Sign: func(_ *http.Request, _ []byte) error {
				return errors.New("缺少入参: " + canary)
			},
		})
		pe, ok := ProviderErrorOf(derr)
		if !ok || pe.Code != "sign_failed" {
			t.Fatalf("签名失败应给 sign_failed，实得 %v ok=%v", derr, ok)
		}
		if strings.Contains(derr.Error(), canary) {
			t.Fatalf("sign_failed 不得回显签名入参: %v", derr)
		}
	})
}

// ---- NF-F8 ----

// TestQaFixBaseURLShapeRejected 钉死基址形态闸：凭证形态、明文通道、无主机都不得构造成功。
func TestQaFixBaseURLShapeRejected(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		wantSub string
	}{
		{"userinfo凭证", "https://merchant:pass@127.0.0.1:9", "不得内嵌凭证"},
		{"明文非本机", "http://api.example.com", "必须 https"},
		{"缺主机", "https:", "解析不出主机"},
		{"相对路径", "/v1/api", "解析不出主机"},
		{"非法scheme", "ftp://127.0.0.1:9", "必须 https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(Options{Name: "nft", BaseURL: tc.base})
			if err == nil {
				t.Fatalf("基址 %q 应拒绝构造", tc.base)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("报错应含 %q，实得 %v", tc.wantSub, err)
			}
			if strings.Contains(err.Error(), "pass@") {
				t.Fatalf("报错不得回显凭证: %v", err)
			}
		})
	}
	// 本机联调与 https 公网必须仍可用（httptest 全走 127.0.0.1，不能被打死）。
	for _, ok := range []string{"http://127.0.0.1:9", "http://localhost:9", "http://[::1]:9", "https://api.example.com"} {
		if _, err := NewClient(Options{Name: "nft", BaseURL: ok}); err != nil {
			t.Fatalf("合法基址 %q 被误拒: %v", ok, err)
		}
	}
}

// ---- NF-F7 ----

// TestQaFixNumericBoundsClamped 钉死三个数值面的上限：坏配置与恶意 Retry-After 都拖不住调用方。
func TestQaFixNumericBoundsClamped(t *testing.T) {
	t.Run("构造期夹取", func(t *testing.T) {
		c, err := NewClient(Options{Name: "nft", BaseURL: "http://127.0.0.1:9",
			MaxRetry: 1_000_000, Timeout: 24 * time.Hour})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if c.maxRetry != maxRetryCap {
			t.Fatalf("maxRetry 应夹到 %d，实得 %d", maxRetryCap, c.maxRetry)
		}
		if c.timeout != maxTimeout {
			t.Fatalf("timeout 应夹到 %v，实得 %v", maxTimeout, c.timeout)
		}
		c2, err := NewClient(Options{Name: "nft", BaseURL: "http://127.0.0.1:9", MaxRetry: -1})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if c2.maxRetry != DefaultMaxRetry {
			t.Fatalf("负值应回落默认 %d，实得 %d", DefaultMaxRetry, c2.maxRetry)
		}
	})

	t.Run("RetryAfter本地封顶", func(t *testing.T) {
		var hdr http.Header
		// 一年（远超任何合理退避）与超过 maxBackoff 的值都必须被夹到 maxBackoff 以内。
		for _, v := range []string{"315360000", "600"} {
			hdr = http.Header{}
			hdr.Set("Retry-After", v)
			if got := retryAfterOf(hdr); got <= 0 || got > maxBackoff {
				t.Fatalf("Retry-After=%s 应夹进 (0, %v]，实得 %v", v, maxBackoff, got)
			}
		}
		if hdr = (http.Header{}); retryAfterOf(hdr) != 0 {
			t.Fatal("无 Retry-After 应为 0")
		}
		hdr.Set("Retry-After", "-5")
		if retryAfterOf(hdr) != 0 {
			t.Fatal("负值 Retry-After 应忽略")
		}
		hdr.Set("Retry-After", "4")
		if retryAfterOf(hdr) != 4*time.Second {
			t.Fatalf("上限内的 Retry-After 应原样生效，实得 %v", retryAfterOf(hdr))
		}
	})

	t.Run("退避真的不超过上限", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if atomic.AddInt64(&hits, 1) == 1 {
				w.Header().Set("Retry-After", "600") // 十分钟：必须夹到 maxBackoff 以内
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK,
				"data": map[string]any{"status": "success"}})
		}))
		defer srv.Close()
		c, err := NewClient(Options{Name: "nft", BaseURL: srv.URL, MaxRetry: 1,
			BackoffBase: time.Millisecond})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, e := c.Do(context.Background(), CallSpec{Path: "/e", Body: []byte("{}")})
			done <- e
		}()
		select {
		case e := <-done:
			if e != nil {
				t.Fatalf("第二次应成功: %v", e)
			}
		case <-time.After(maxBackoff + 30*time.Second):
			t.Fatal("Retry-After 未封顶：本次退避超过 maxBackoff+30s")
		}
		if atomic.LoadInt64(&hits) != 2 {
			t.Fatalf("应重发恰好一次，实得 %d", hits)
		}
	})
}

// ---- NF-F3 ----

// TestQaFixReadOnlyPayloadKeysReserved 钉死只读报文的已签名量也是保留字：
// 特有字段撞 address/page_no/page_size 必须发送前拒绝。
func TestQaFixReadOnlyPayloadKeysReserved(t *testing.T) {
	const canary = "0xATTACKERADDRESS"
	for _, key := range []string{"address", "page_no", "page_size"} {
		t.Run(key, func(t *testing.T) {
			var hits int64
			srv := qaCountingServer(t, &hits, "1")
			p := qaFixProvider(t, srv.URL, "")
			p.opt.Extras = []ExtraField{{Name: key, Value: canary}}
			_, err := p.QueryHoldings(context.Background(), &HoldingsQuery{Address: "0xMyAddress"})
			if err == nil {
				t.Fatalf("特有字段撞 %s 应拒单", key)
			}
			if !strings.Contains(err.Error(), "已签名量") {
				t.Fatalf("报错应含「已签名量」，实得 %v", err)
			}
			if got := atomic.LoadInt64(&hits); got != 0 {
				t.Fatalf("撞名拒单不得外发，实得 %d 次", got)
			}
		})
	}
	// 未登记的键仍可正常追加（不把好用的面一并打死）。
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "gas_station") {
			t.Errorf("特有字段应进报文，实得 %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK,
			"data": map[string]any{"total": 0, "items": []any{}}})
	}))
	defer srv.Close()
	p := qaFixProvider(t, srv.URL, "")
	p.opt.Extras = []ExtraField{{Name: "gas_station", Value: "1"}}
	if _, err := p.QueryHoldings(context.Background(), &HoldingsQuery{Address: "0xMy"}); err != nil {
		t.Fatalf("合法特有字段不应被拒: %v", err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("应外发一次，实得 %d", hits)
	}
}

// ---- NF-F5 ----

// TestQaFixBad2xxBodyIsSubmitDoubt 钉死「2xx 但回执读不出」属受理存疑：
// 它不能落在「没发过」一侧，否则上游会另起新单号二次铸造。
func TestQaFixBad2xxBodyIsSubmitDoubt(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte("<<<not-json>>>"))
	}))
	defer srv.Close()
	res, err := qaFixProvider(t, srv.URL, "").Mint(context.Background(), qaMintOrder(t.Name()+"-badbody"))
	if err == nil {
		t.Fatal("2xx 非 JSON 不得判成功")
	}
	pe, ok := ProviderErrorOf(err)
	if !ok {
		t.Fatalf("应为 ProviderError，实得 %T", err)
	}
	if pe.Code != "bad_body" {
		t.Fatalf("应给专用哨兵 bad_body，实得 %q", pe.Code)
	}
	if pe.HTTPStatus != http.StatusOK {
		t.Fatalf("HTTPStatus 仍要透传 200，实得 %d", pe.HTTPStatus)
	}
	if pe.Retryable {
		t.Fatal("坏回执不得自动重发")
	}
	if !InSubmitDoubt(err) {
		t.Fatal("坏回执属 InSubmitDoubt（可能已受理）")
	}
	if !NeedsReconcile(res, err) {
		t.Fatal("坏回执必须 NeedsReconcile==true")
	}
	if res == nil || res.Status != StatusUnknown {
		t.Fatalf("请求已发出，必须给 UNKNOWN 回执而不是空白，实得 %+v", res)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("不得重发，应 1 次，实得 %d", got)
	}
}

// TestQaFixOversizedBodyIsSubmitDoubt 钉死响应体超限不是「静默截断后随便解读」：
// 必须按 read_body（受理存疑）落账，且严格路径不重发。
func TestQaFixOversizedBodyIsSubmitDoubt(t *testing.T) {
	// 刻意写死 1MiB+4KiB，不引用 maxResponseBytes：夹具若自指被测常量，
	// 把上限改大的变异会让本用例先 OOM（分配 1TiB）而不是报红，变异台账就失去裁决价值。
	junk := strings.Repeat("x", (1<<20)+4096)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"0","data":{"status":"`+junk+`"}}`)
	}))
	defer srv.Close()
	res, err := qaFixProvider(t, srv.URL, "").Mint(context.Background(), qaMintOrder(t.Name()+"-oversize"))
	if err == nil {
		t.Fatal("超限响应不得判成功")
	}
	pe, ok := ProviderErrorOf(err)
	if !ok || pe.Code != "read_body" {
		t.Fatalf("超限应归 read_body，实得 %v ok=%v", err, ok)
	}
	if pe.Retryable {
		t.Fatal("严格路径超限不得自动重发")
	}
	if !InSubmitDoubt(err) || !NeedsReconcile(res, err) {
		t.Fatalf("超限回执必须存疑并进对账，实得 err=%v res=%+v", err, res)
	}
	if res == nil || res.Status != StatusUnknown {
		t.Fatalf("应给 UNKNOWN 回执，实得 %+v", res)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("应恰好 1 次外呼，实得 %d", got)
	}
}

// TestQaFixAssetErrorsCarryUnknownReceipt 钉死「发出去之后失败」一律带 UNKNOWN 回执：
// 前置校验/端点缺失这类没发出去的失败仍给 nil。
func TestQaFixAssetErrorsCarryUnknownReceipt(t *testing.T) {
	t.Run("网关5xx也带UNKNOWN", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		res, err := qaFixProvider(t, srv.URL, "").Mint(context.Background(), qaMintOrder(t.Name()+"-502"))
		if err == nil {
			t.Fatal("502 应报错")
		}
		if res == nil || res.Status != StatusUnknown {
			t.Fatalf("已外发的失败必须给 UNKNOWN，实得 %+v", res)
		}
		if NeedsReconcile(res, err) != true {
			t.Fatal("必须进对账")
		}
	})

	t.Run("未发送的失败不给回执", func(t *testing.T) {
		srv := qaCountingServer(t, new(int64), "1")
		p := qaFixProvider(t, srv.URL, "")
		if _, err := p.Mint(context.Background(), &NftOrder{OrderNo: "qa-f5-nil"}); err == nil {
			t.Fatal("前置校验失败应报错")
		}
		pNoEnd, err := NewProvider(ProviderOptions{Name: "nft", Driver: DriverGeneric,
			SecretKey: "k", BaseURL: srv.URL,
			Endpoint: func(string) (string, bool) { return "", false }})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		res, err := pNoEnd.Mint(context.Background(), qaMintOrder(t.Name()+"-noendpoint"))
		if err == nil || res != nil {
			t.Fatalf("端点缺失属未发送，应 nil 回执 + 报错，实得 res=%+v err=%v", res, err)
		}
	})
}

// ---- NF-F6 ----

// TestQaFixCallRejectsAssetEndpoints 钉死导出 Call() 不是资产动作的逃生口。
func TestQaFixCallRejectsAssetEndpoints(t *testing.T) {
	for _, ep := range []string{EndpointMint, EndpointTransfer} {
		t.Run(ep, func(t *testing.T) {
			var hits int64
			srv := qaCountingServer(t, &hits, "1")
			p := qaFixProvider(t, srv.URL, "")
			_, err := p.Call(context.Background(), ep, MintRequest{OrderNo: "qa-c1"}, []string{"a"})
			if err == nil || !strings.Contains(err.Error(), "必须经 Mint/Transfer") {
				t.Fatalf("Call(%s) 应就地拒绝，实得 %v", ep, err)
			}
			if got := atomic.LoadInt64(&hits); got != 0 {
				t.Fatalf("拒绝不得外发，实得 %d", got)
			}
		})
	}

	t.Run("只读端点仍可Call", func(t *testing.T) {
		var hits int64
		srv := qaCountingServer(t, &hits, "1")
		p := qaFixProvider(t, srv.URL, "")
		if _, err := p.Call(context.Background(), EndpointQuery, QueryRequest{OrderNo: "qa-c3"},
			[]string{"a"}); err != nil {
			t.Fatalf("只读 Call 应可用: %v", err)
		}
		if atomic.LoadInt64(&hits) != 1 {
			t.Fatalf("只读 Call 应外发一次，实得 %d", hits)
		}
	})
}

// ---- NF-F12 ----

// TestQaFixDuplicateCredentialHeadersRejected 钉死两个凭证头不得同名。
func TestQaFixDuplicateCredentialHeadersRejected(t *testing.T) {
	for _, tc := range []struct{ a, b string }{{"X-Cred", "x-cred"}, {"X-Cred", "X-Cred"}} {
		_, err := NewProvider(ProviderOptions{Name: "nft", Driver: DriverGeneric,
			SecretKey: "k", BaseURL: "http://127.0.0.1:9", AppID: "a",
			AuthHeader: tc.a, TokenHeader: tc.b, Endpoint: qaFixEndpointAll("/e")})
		if err == nil || !strings.Contains(err.Error(), "不得是同一个请求头") {
			t.Fatalf("auth=%q token=%q 应构造期拒绝，实得 %v", tc.a, tc.b, err)
		}
	}
	if _, err := NewProvider(ProviderOptions{Name: "nft", Driver: DriverGeneric,
		SecretKey: "k", BaseURL: "http://127.0.0.1:9",
		Endpoint: qaFixEndpointAll("/e")}); err != nil {
		t.Fatalf("默认头名应可用: %v", err)
	}
}

// ---- NF-F9 ----

// TestQaFixOrderNoShapeGate 钉死单号形态闸：控制符与超长不得进日志与幂等账本。
func TestQaFixOrderNoShapeGate(t *testing.T) {
	cases := []struct {
		name string
		no   string
		sub  string
	}{
		{"换行注入", "qa\r\nFAKE LOG LINE", "订单号形态"},
		{"超长", strings.Repeat("9", 65), "长度超过"},
		{"空白", "  ", "为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOrderNo(tc.no)
			if err == nil || !strings.Contains(err.Error(), tc.sub) {
				t.Fatalf("CheckOrderNo(%q) 应报 %q，实得 %v", tc.no, tc.sub, err)
			}
		})
	}
	var hits int64
	srv := qaCountingServer(t, &hits, "1")
	p := qaFixProvider(t, srv.URL, "")
	// placeAsset 侧同样要在发送前拦住（单号是用户可控输入）。
	_, err := p.Mint(context.Background(), &NftOrder{OrderNo: "qa\r\nx", Chain: ChainETH,
		Network: NetworkMainnet, ToAddress: "0xA", Quantity: 1, MetadataURI: "ipfs://m"})
	if err == nil || !strings.Contains(err.Error(), "订单号形态") {
		t.Fatalf("铸造应在发送前拒非法单号，实得 %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("非法单号不得外发，实得 %d", got)
	}
	// 合法形态必须照过。
	for _, ok := range []string{"o1", "NFT-2026_10-01-0001", strings.Repeat("a", 64)} {
		if err := CheckOrderNo(ok); err != nil {
			t.Fatalf("合法单号被误拒 %q: %v", ok, err)
		}
	}
}

// ---- NF-F11 ----

// TestQaFixProviderEchoSanitized 钉死供应商回显串不进控制符：
// 日志伪造与终端污染的路径要在归一层就掐掉。
func TestQaFixProviderEchoSanitized(t *testing.T) {
	t.Run("未登记状态词", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK,
				"data": map[string]any{"status": "ok\r\nFAKE-LOG-LINE"}})
		}))
		defer srv.Close()
		res, err := qaFixProvider(t, srv.URL, "").Mint(context.Background(), qaMintOrder(t.Name()+"-echo"))
		if err != nil {
			t.Fatalf("未登记状态词不应报错: %v", err)
		}
		if strings.ContainsAny(res.RawNote, "\r\n\t") {
			t.Fatalf("RawNote 含控制符: %q", res.RawNote)
		}
		if res.RawNote == "" {
			t.Fatal("RawNote 仍要记下未登记状态词")
		}
	})

	t.Run("回显单号不得越权覆盖", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK,
				"data": map[string]any{"order_no": "OTHER\r\nINJECT", "status": "success", "token_id": "3"}})
		}))
		defer srv.Close()
		_, err := qaFixProvider(t, srv.URL, "").Mint(context.Background(), qaMintOrder(t.Name()+"-echo2"))
		if err == nil || !strings.Contains(err.Error(), "回执订单号") {
			t.Fatalf("回显单号不一致应报错，实得 %v", err)
		}
		if strings.ContainsAny(err.Error(), "\r\n") {
			t.Fatalf("错误文案含控制符: %q", err.Error())
		}
	})

	t.Run("包络失败消息", func(t *testing.T) {
		var hits int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "9001", "msg": "bad\tvalue\r\nFAKE"})
		}))
		defer srv.Close()
		_, err := qaFixProvider(t, srv.URL, "").Mint(context.Background(), qaMintOrder(t.Name()+"-echo3"))
		if err == nil {
			t.Fatal("包络失败应报错")
		}
		if strings.ContainsAny(err.Error(), "\r\n\t") {
			t.Fatalf("包络 msg 未净: %q", err.Error())
		}
	})
}

// ---- NF-F10 ----

// TestQaFixFingerprintIncludesMemo 钉死链上备注参与幂等指纹：
// 同号换备注的报文与签名都变了，本地闸必须拦。
func TestQaFixFingerprintIncludesMemo(t *testing.T) {
	var hits int64
	srv := qaCountingServer(t, &hits, "1")
	p := qaFixProvider(t, srv.URL, "")
	base := "qa-fix-f10-memo"
	o1 := &NftOrder{OrderNo: base, Chain: ChainETH, Network: NetworkMainnet,
		Contract: "0xC", TokenID: "12", ToAddress: "0xA", Quantity: 1, Memo: "first"}
	if _, err := p.Transfer(context.Background(), o1); err != nil {
		t.Fatalf("首单应成功: %v", err)
	}
	o2 := &NftOrder{OrderNo: base, Chain: ChainETH, Network: NetworkMainnet,
		Contract: "0xC", TokenID: "12", ToAddress: "0xA", Quantity: 1, Memo: "second"}
	_, err := p.Transfer(context.Background(), o2)
	if err == nil || !strings.Contains(err.Error(), "不得变更") {
		t.Fatalf("同号换 Memo 应被幂等闸拒绝，实得 %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("换备注不得二次外发，实得 %d", got)
	}
}

// ---- 附带收紧：白名单入口硬拒「受理未知」类码 ----

// TestQaFixGatewayDoubtCodesNeverWhitelisted 把「502/504 严禁进重试白名单」
// 从运维警示升级为注册入口硬拒（7eb7b89 的教训不该只靠注释守）。
func TestQaFixGatewayDoubtCodesNeverWhitelisted(t *testing.T) {
	t.Cleanup(func() {
		DeleteRetryableCode("59901")
		DeleteRetryableCode(qaFixWhitelistCode)
	})
	for _, code := range []string{"502", "504", "transport", "read_body", "bad_body"} {
		if IsRetryableCode(code) {
			t.Fatalf("%q 成了白名单成员（受理未知类码原单重发=二次铸造）", code)
		}
	}
	SetRetryableCodes("502", "504", "transport", "read_body", "bad_body", "59901", "", qaFixWhitelistCode)
	for _, code := range []string{"502", "504", "transport", "read_body", "bad_body"} {
		if IsRetryableCode(code) {
			t.Fatalf("入口硬拒失效：%q 被登记进白名单", code)
		}
	}
	if !IsRetryableCode("59901") || !IsRetryableCode(qaFixWhitelistCode) {
		t.Fatal("普通业务码仍应可登记（白名单面没被打死）")
	}
}

const qaFixWhitelistCode = "qa-fix-9001"

// ---- 附带收紧：告警接线与门面回执传递 ----

// TestQaFixWarningHookWired 钉死两件事：
//  1. provider.go 的告警出口真的被 nft.go init 接上（否则「纯 stdlib」的分层修法会把
//     白名单硬拒和账本告警变成静默死代码——分层守住了，运维什么都看不见）。
//  2. 没接线时 warnLog 不 panic（warnf 为 nil 是合法状态，不能让一条日志打挂资金路径）。
func TestQaFixWarningHookWired(t *testing.T) {
	if warnf == nil {
		t.Fatal("warnf 未接入 vars（nft.go init 的注入丢了：两处告警全是静默死代码）")
	}
	orig := warnf
	t.Cleanup(func() { warnf = orig })

	var msgs []string
	warnf = func(format string, args ...any) {
		msgs = append(msgs, fmt.Sprintf(format, args...))
	}

	if got := IsRetryableCode("502"); got {
		t.Fatal("前置条件破了：502 已在白名单")
	}
	SetRetryableCodes("502", "read_body")
	if len(msgs) != 2 {
		t.Fatalf("两次硬拒应各报警一次，实得 %d 条：%q", len(msgs), msgs)
	}
	for _, m := range msgs {
		if !strings.Contains(m, "受理存疑码") || !strings.Contains(m, "二次铸造") {
			t.Fatalf("告警文案须点明拒绝理由，实得 %q", m)
		}
	}

	msgs = nil
	warnedBase := orderLedgerWarned.Load()
	orderLedgerWarned.Store(false)
	t.Cleanup(func() { orderLedgerWarned.Store(warnedBase) })
	warnOrderLedgerGrowth(orderLedgerWarnAt - 1)
	if len(msgs) != 0 {
		t.Fatalf("未过阈值不应报警，实得 %q", msgs)
	}
	warnOrderLedgerGrowth(orderLedgerWarnAt)
	if len(msgs) != 1 {
		t.Fatalf("过阈值应报警一次，实得 %q", msgs)
	}
	warnOrderLedgerGrowth(orderLedgerWarnAt * 2)
	if len(msgs) != 1 {
		t.Fatalf("告警应一次性（不刷屏），实得 %q", msgs)
	}
	if !strings.Contains(msgs[0], "不淘汰") {
		t.Fatalf("账本告警须写明「不淘汰」的处置口径，实得 %q", msgs[0])
	}

	warnf = nil // 未接线形态：静默，不 panic。
	warnLog("静默路径 %d", 1)
	warnOrderLedgerGrowth(orderLedgerWarnAt * 3)
	SetRetryableCodes(qaFixWhitelistCode)
	DeleteRetryableCode(qaFixWhitelistCode)
}

// TestQaFixFacadeCarriesUnknownReceipt 钉死 NF-F5③ 穿过门面：
// 已外发的失败必须把 UNKNOWN 回执连同 error 一起带回调用方，且错误路径不广播。
// 门面上游拿 nil 回执会走「没发过」分支另起新单号重下——那才是二次铸造的入口。
func TestQaFixFacadeCarriesUnknownReceipt(t *testing.T) {
	var hits int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	p := qaFixProvider(t, bad.URL, "")

	active.Store(&client{ch: p, chain: ChainETH, network: NetworkMainnet})
	t.Cleanup(func() { active.Store(nil) })

	var broadcasts int64
	id := util.DefaultCallFunc.Register(util.CallNftMsg+"Mint", func(*NftResult) {
		atomic.AddInt64(&broadcasts, 1)
	})
	defer util.DefaultCallFunc.Unregister(util.CallNftMsg+"Mint", id)

	res, err := NftMint(context.Background(), qaMintOrder(t.Name()+"-facade"))
	if err == nil {
		t.Fatal("502 应报错")
	}
	if res == nil || res.Status != StatusUnknown {
		t.Fatalf("门面必须把 UNKNOWN 回执随 error 带回，实得 %+v", res)
	}
	if !NeedsReconcile(res, err) {
		t.Fatal("门面返回的组合必须判进对账")
	}
	if got := atomic.LoadInt64(&broadcasts); got != 0 {
		t.Fatalf("回执不可信的错误路径不得广播，实得 %d", got)
	}
	if atomic.LoadInt64(&hits) == 0 {
		t.Fatal("前置不成立：请求根本没外发，那这条闸就是空跑")
	}

	// 只读路径口径不同：查询失败没有回执可带回（它本来就是在「求一个结论」），
	// 但 InSubmitDoubt 必须为真，让 NeedsReconcile(nil, err) 仍判进对账——
	// 否则「查单也 502」会被下游读成「没得对账」而停在原地不动。
	qres, qerr := NftQuery(context.Background(), qaFitOrder(t.Name()+"-facade-q"))
	if qerr == nil {
		t.Fatal("查询 502 应报错")
	}
	if qres != nil {
		t.Fatalf("只读失败不该造回执（它求的就是回执），实得 %+v", qres)
	}
	if !InSubmitDoubt(qerr) || !NeedsReconcile(qres, qerr) {
		t.Fatalf("只读 502 仍须进对账，实得 doubt=%v res=%+v err=%v", InSubmitDoubt(qerr), qres, qerr)
	}
}
