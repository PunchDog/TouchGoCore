package nft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"touchgocore/vars"
)

// 常量：SPEC §8 未启动错误的逐字全文，T7 断言按逐字匹配。
const notStartedMsg = "NFT 通道未启动（检查 nft.provider 是否引用了已开启的 nft_sdks 段与签发账户）"

// newProviderOn 构造一个指向 srv 的通用驱动 Provider（五个逻辑端点都指向 /e）。
func newProviderOn(t *testing.T, srvURL, secret string) *Provider {
	t.Helper()
	endpoints := map[string]string{
		EndpointHoldings: "/e", EndpointToken: "/e", EndpointMint: "/e",
		EndpointTransfer: "/e", EndpointQuery: "/e",
	}
	p, err := NewProvider(ProviderOptions{
		Name:      "nft",
		Driver:    DriverGeneric,
		AppID:     "nft-app",
		SecretKey: secret,
		BaseURL:   srvURL,
		Endpoint: func(name string) (string, bool) {
			v, ok := endpoints[name]
			return v, ok
		},
	})
	if err != nil {
		t.Fatalf("NewProvider 失败: %v", err)
	}
	return p
}

// successEnvelope 是铸造成功回执（带 token_id，过 §3.3 回执对账）；
// order_no 回显自请求体，避免与 §3.3 单号一致性检查相互打架。
func successEnvelope(tokenID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		orderNo, _ := body["order_no"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": CodeOK,
			"data": map[string]any{"order_no": orderNo, "status": "success", "token_id": tokenID},
		})
	}
}

func mintOrder(orderNo string) *NftOrder {
	return &NftOrder{
		OrderNo: orderNo, Chain: ChainETH, Network: NetworkMainnet,
		ToAddress: "0xToAddress", Quantity: 1, MetadataURI: "ipfs://meta",
	}
}

// ---- SPEC §9.2-3：幂等键拒变更 ----

func TestOrderIdempotencyKeyImmutable(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		successEnvelope("123")(w, r)
	}))
	defer srv.Close()
	p := newProviderOn(t, srv.URL, "s3cr3t-key")

	base := t.Name() + "-X"
	ctx := context.Background()

	// 1) 未见过的 OrderNo ⇒ 登记并放行，发送一次。
	if _, err := p.Mint(ctx, mintOrder(base)); err != nil {
		t.Fatalf("首次 Mint 应放行: %v", err)
	}
	// 2) 同 OrderNo 同指纹 ⇒ 放行（合法的透明重发），再发送一次。
	if _, err := p.Mint(ctx, mintOrder(base)); err != nil {
		t.Fatalf("同指纹重发应放行: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("两次同指纹 Mint 应各发送一次，httptest 计数=2，实得 %d", got)
	}

	// 3) 同 OrderNo 改 ToAddress ⇒ 就地拒绝、不发请求。
	changed := mintOrder(base)
	changed.ToAddress = "0xAnotherAddress"
	_, err := p.Mint(ctx, changed)
	if err == nil {
		t.Fatal("改 ToAddress 应被幂等闸拒绝")
	}
	if !strings.Contains(err.Error(), "幂等键") || !strings.Contains(err.Error(), "不得变更") {
		t.Fatalf("拒绝文案应含「幂等键」+「不得变更」，实得: %v", err)
	}

	// 4) 同 OrderNo 改 Quantity ⇒ 同样就地拒绝。
	changed2 := mintOrder(base)
	changed2.Quantity = 5
	if _, err := p.Mint(ctx, changed2); err == nil {
		t.Fatal("改 Quantity 应被幂等闸拒绝")
	}

	// 计数不增加 ⇒ 证明拒绝发生在发送之前（§3.4）。
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("异指纹拒绝不得发送，httptest 计数应仍为 2，实得 %d", got)
	}
}

// ---- SPEC §9.2-2：注册表拒覆盖 ----

func TestRegistryRejectsDuplicateDriver(t *testing.T) {
	registryMu.Lock()
	delete(registry, "dup_probe") // 自带清理口，保证 -count≥2 可重复（§9.3 附注）
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, "dup_probe")
		registryMu.Unlock()
	})

	f := func(_ ProviderOptions) (NftChannel, error) { return &stubChannel{tag: "first"}, nil }
	g := func(_ ProviderOptions) (NftChannel, error) { return &stubChannel{tag: "second"}, nil }

	if err := Register("dup_probe", f); err != nil {
		t.Fatalf("首次登记应成功: %v", err)
	}
	err := Register("dup_probe", g)
	if err == nil {
		t.Fatal("重名登记应拒绝覆盖")
	}
	if !strings.Contains(err.Error(), "已登记，不得覆盖") {
		t.Fatalf("拒绝文案应含「已登记，不得覆盖」，实得: %v", err)
	}

	// 第一个 Factory 仍可被 Open 取到（未被替换）。
	ch, err := Open("dup_probe", ProviderOptions{})
	if err != nil {
		t.Fatalf("Open 应命中第一个 Factory: %v", err)
	}
	if ch.Driver() != "first" {
		t.Fatalf("Open 取到的应是第一个 Factory（first），实得 %q", ch.Driver())
	}
}

// ---- SPEC §9.2-1：对账 UNKNOWN 保持非终态、不自动重下 ----

func TestReconcileUnknownStaysNonFinal(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		// 供应商查单回未登记状态词。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": CodeOK,
			"data": map[string]any{"order_no": "Q1", "status": "totally-unregistered-word"},
		})
	}))
	defer srv.Close()
	p := newProviderOn(t, srv.URL, "k")

	// 直接经 provider 查回 UNKNOWN，验证归一 + NeedsReconcile 组合（不依赖门面 active）。
	res, err := p.QueryOrder(context.Background(), "Q1")
	if err != nil {
		t.Fatalf("QueryOrder 归一不应报错: %v", err)
	}
	if res.Status != StatusUnknown {
		t.Fatalf("未登记状态词应归一 UNKNOWN，实得 %q", res.Status)
	}
	if res.IsFinal() {
		t.Fatal("UNKNOWN 不是终态")
	}
	if !NeedsReconcile(res, nil) {
		t.Fatal("UNKNOWN 应 NeedsReconcile==true")
	}
	// 组合断言：InSubmitDoubt(nil)==false 但 NeedsReconcile==true ⇒
	// UNKNOWN 只能靠再查收敛，不能靠重发收敛。
	if InSubmitDoubt(nil) {
		t.Fatal("InSubmitDoubt(nil) 必须为 false")
	}

	// NftReconcile 门面口径：读回仍 UNKNOWN 也返回 (res, nil)——本包不自动重下单。
	// 用包内直连装配一个 active client 走门面广播路径。
	startStubClient(t, p, ChainETH, NetworkMainnet)
	rr, rerr := NftReconcile(context.Background(), "Q1")
	if rerr != nil {
		t.Fatalf("NftReconcile 读回 UNKNOWN 应返回 (res,nil): %v", rerr)
	}
	if rr.Status != StatusUnknown || rr.IsFinal() {
		t.Fatalf("NftReconcile 应保持 UNKNOWN 非终态，实得 status=%q", rr.Status)
	}
	// 只查了一次（两次命中：一次直接 QueryOrder + 一次门面 Reconcile；无自动重下单第三次）。
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("应恰好 2 次外呼（无自动重下单），实得 %d", got)
	}
}

// ---- SPEC §9.2-4：凭证脱敏 ----

func TestSecretKeyNeverLeaks(t *testing.T) {
	const secret = "sup3r-s3cret"
	const sessToken = "sess-tok-9f9"
	const metadataURI = "ipfs://secret-metadata-never-logged"

	// (a) 包络失败码
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "-1", "msg": "provider rejected"})
	}))
	defer srvFail.Close()
	// (b) 非法 JSON
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<<<not-json>>>"))
	}))
	defer srvBad.Close()
	// (c) 未登记状态词
	srvUnknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": CodeOK,
			"data": map[string]any{"order_no": "X", "status": "quantum"}})
	}))
	defer srvUnknown.Close()

	type probe struct {
		name string
		url  string
	}
	probes := []probe{{"fail-envelope", srvFail.URL}, {"bad-json", srvBad.URL}, {"unknown-status", srvUnknown.URL}}

	for i, pr := range probes {
		p := newProviderOn(t, pr.url, secret)
		p.SetToken(sessToken)
		order := mintOrder(t.Name() + "-" + pr.name)
		order.MetadataURI = metadataURI
		res, err := p.Mint(context.Background(), order)

		var errText string
		if err != nil {
			errText = err.Error()
		}
		var rawNote string
		if res != nil {
			rawNote = res.RawNote
		}
		combined := errText + "|" + rawNote
		for _, banned := range []string{secret, sessToken, metadataURI} {
			if strings.Contains(combined, banned) {
				t.Fatalf("[%s] error/RawNote 泄露凭证/元数据 %q: %q", pr.name, banned, combined)
			}
		}
		_ = i
	}

	// Drivers()/Open 报错文案不含基址以外凭证。
	dj := strings.Join(Drivers(), ",")
	if strings.Contains(dj, secret) {
		t.Fatal("Drivers() 不应含密钥")
	}
	if _, err := Open("no_such_driver_xyz", ProviderOptions{SecretKey: secret}); err == nil {
		t.Fatal("Open 未登记驱动应报错")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("Open 报错不应含密钥: %v", err)
	}

	// 捕获 vars 日志输出：publish 在无人订阅时走 vars.Debug，这条真实日志不得含凭证。
	vars.CloseConsole() // 幂等；关闭后写入退回直接 os.Stdout，便于管道捕获
	old := os.Stdout
	r, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = wr
	p := newProviderOn(t, srvUnknown.URL, secret)
	p.SetToken(sessToken)
	res, _ := p.Mint(context.Background(), mintOrder(t.Name()+"-cap"))
	publish("Mint", res) // 触发 vars.Debug 留痕
	_ = wr.Close()
	os.Stdout = old
	captured, _ := io.ReadAll(r)
	for _, banned := range []string{secret, sessToken, metadataURI} {
		if strings.Contains(string(captured), banned) {
			t.Fatalf("vars 日志泄露凭证 %q: %q", banned, string(captured))
		}
	}
}

// ---- 行为闸（§9.3）：空白名单默认不可重试（7eb7b89 口径 nft 版）----

func TestEmptyRetryableCodesByDefault(t *testing.T) {
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "4009", "msg": "busy"})
	}))
	defer srvFail.Close()
	srv502 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv502.Close()

	p := newProviderOn(t, srvFail.URL, "k")
	_, err := p.Mint(context.Background(), mintOrder(t.Name()+"-code"))
	pe, ok := ProviderErrorOf(err)
	if !ok {
		t.Fatalf("包络失败应转 ProviderError，实得 %v", err)
	}
	if pe.Retryable {
		t.Fatal("空白名单下包络业务码不得可重试")
	}

	p2 := newProviderOn(t, srv502.URL, "k")
	_, err = p2.Mint(context.Background(), mintOrder(t.Name()+"-502"))
	pe2, ok := ProviderErrorOf(err)
	if !ok {
		t.Fatalf("502 应转 ProviderError，实得 %v", err)
	}
	if pe2.Retryable {
		t.Fatal("Mint 严格路径下 502 严禁可重试（7eb7b89）")
	}
	if !InSubmitDoubt(err) {
		t.Fatal("502 属 InSubmitDoubt 集合")
	}
}

// Extra 键撞已签名量 ⇒ 拒单（§5.2）
func TestExtrasKeyCollisionRejected(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		successEnvelope("9")(w, r)
	}))
	defer srv.Close()
	p := newProviderOn(t, srv.URL, "k")
	order := mintOrder(t.Name() + "-collide")
	order.Extra = map[string]string{"quantity": "9"}
	_, err := p.Mint(context.Background(), order)
	if err == nil {
		t.Fatal("Extra 撞 quantity 应拒单")
	}
	if !strings.Contains(err.Error(), "已签名量") {
		t.Fatalf("报错应含「已签名量」，实得: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("撞名拒单不得发送，httptest 应零命中，实得 %d", got)
	}
}

// Open 未登记驱动 ⇒ 错误含「可用驱动」名单
func TestOpenUnknownDriverListsRegistered(t *testing.T) {
	_, err := Open("definitely_not_registered", ProviderOptions{})
	if err == nil {
		t.Fatal("未登记驱动应报错")
	}
	if !strings.Contains(err.Error(), "可用驱动") {
		t.Fatalf("报错应含「可用驱动」名单，实得: %v", err)
	}
	if !strings.Contains(err.Error(), DriverGeneric) {
		t.Fatalf("名单应含内置驱动 %q，实得: %v", DriverGeneric, err)
	}
}

// 未启动就地拒绝：五个门面 + NftReconcile 各返回逐字未启动错误。
func TestNotStartedRejectsInPlace(t *testing.T) {
	NftStop(nil) // 确保 active=nil，可重复
	ctx := context.Background()

	check := func(name string, err error) {
		if err == nil {
			t.Fatalf("%s 未启动应报错", name)
		}
		if err.Error() != notStartedMsg {
			t.Fatalf("%s 未启动错误应逐字匹配，实得: %q", name, err.Error())
		}
	}
	_, err := NftMint(ctx, mintOrder(t.Name()+"-m"))
	check("NftMint", err)
	_, err = NftTransfer(ctx, mintOrder(t.Name()+"-t"))
	check("NftTransfer", err)
	_, err = NftQuery(ctx, "Q")
	check("NftQuery", err)
	_, err = NftHoldings(ctx, &HoldingsQuery{Address: "0xabc"})
	check("NftHoldings", err)
	_, err = NftToken(ctx, &TokenQuery{Chain: ChainETH, Network: NetworkMainnet, Contract: "0xc", TokenID: "1"})
	check("NftToken", err)
	_, err = NftReconcile(ctx, "Q")
	check("NftReconcile", err)
}

// 状态归一方向性（P0-5）：读不懂→unknown，而非 pending/success。
func TestStatusNormalizationDirection(t *testing.T) {
	cases := map[string]string{
		"success":   StatusSuccess,
		"PAID":      StatusSuccess,
		"failed":    StatusFailed,
		"rejected":  StatusFailed,
		"pending":   StatusPending,
		"0":         StatusPending,
		"gibberish": StatusUnknown,
		"":          StatusUnknown,
	}
	for raw, want := range cases {
		if got := NormalizeStatus(raw); got != want {
			t.Errorf("NormalizeStatus(%q)=%q, 期望 %q", raw, got, want)
		}
	}
}

// IsTestnet 认不出按主网（§5.4 安全方向）
func TestIsTestnetDefaultsToMainnet(t *testing.T) {
	if IsTestnet("garbage-network-value") {
		t.Fatal("认不出的网络值应按主网处理")
	}
	if !IsTestnet(NetworkChapel) || !IsTestnet(NetworkDevnet) || !IsTestnet("testnet") {
		t.Fatal("已知测试网名应判测试网")
	}
	if IsTestnet(NetworkMainnet) {
		t.Fatal("mainnet 不是测试网")
	}
}

// ---- -race 不可用（本机无 C 工具链）下的并发压测闸：对共享状态路径施压 ----
// race 由 Lead 合流批以并发用例替代；本用例覆盖 orderLedger(sync.Map)/retryableCodes(RWMutex)/
// registry(RWMutex)/token(atomic)/publish 的并发读写，-count=3 连跑须全绿。
func TestConcurrentSharedState(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		successEnvelope("1")(w, r)
	}))
	defer srv.Close()

	const N = 64
	var wg sync.WaitGroup
	// 每个 goroutine 一把独立 Provider（token 是 per-provider atomic），但共享包级 orderLedger/
	// retryableCodes/registry。orderNo 按 i 唯一 ⇒ 并发写 sync.Map 不触发指纹冲突。
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := newProviderOn(t, srv.URL, "k")
			p.SetToken(fmt.Sprintf("tok-%d", i))
			order := mintOrder(fmt.Sprintf("%s-c%d", t.Name(), i))
			if _, err := p.Mint(context.Background(), order); err != nil {
				t.Errorf("并发 Mint[%d] 失败: %v", i, err)
			}
		}(i)
	}
	// 并发读白名单 + 写白名单 + 列驱动，验证锁面不 fatal（无锁并发读写 map 在 Go runtime 即崩）。
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := fmt.Sprintf("retry-%d", i)
			SetRetryableCodes(code)
			_ = IsRetryableCode(code)
			_ = Drivers()
			DeleteRetryableCode(code)
			_, _ = Open(DriverGeneric, ProviderOptions{SecretKey: "k", BaseURL: srv.URL,
				Endpoint: func(string) (string, bool) { return "/e", true }})
		}(i)
	}
	// 并发 publish（读 util 回调表）+ 门面读 active（atomic）
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			publish("Query", &NftResult{OrderNo: fmt.Sprintf("q%d", i), Status: StatusUnknown})
			_ = NeedsReconcile(&NftResult{Status: StatusPending}, nil)
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt64(&hits); got != N {
		t.Fatalf("并发 Mint 应各发送一次，httptest 计数=%d，实得 %d", N, got)
	}
}

// startStubClient 把给定 Provider 装进包级 active，让门面（NftReconcile 等）可用。
func startStubClient(t *testing.T, p *Provider, chain, network string) {
	t.Helper()
	active.Store(&client{ch: p, chain: chain, network: network})
	t.Cleanup(func() { active.Store(nil) })
}

// stubChannel 是注册表测试用的最小 NftChannel 实现。
type stubChannel struct{ tag string }

func (s *stubChannel) Driver() string     { return s.tag }
func (s *stubChannel) MerchantID() string { return "" }
func (s *stubChannel) QueryHoldings(context.Context, *HoldingsQuery) (*HoldingsResult, error) {
	return &HoldingsResult{}, nil
}
func (s *stubChannel) QueryToken(context.Context, *TokenQuery) (*TokenInfo, error) {
	return &TokenInfo{}, nil
}
func (s *stubChannel) Mint(context.Context, *NftOrder) (*NftResult, error) {
	return &NftResult{Status: StatusUnknown}, nil
}
func (s *stubChannel) Transfer(context.Context, *NftOrder) (*NftResult, error) {
	return &NftResult{Status: StatusUnknown}, nil
}
func (s *stubChannel) QueryOrder(context.Context, string) (*NftResult, error) {
	return &NftResult{Status: StatusUnknown}, nil
}

var _ = context.TODO
