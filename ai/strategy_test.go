package ai

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
)

// recordingServer 记录最近一次请求的模型名与调用次数，用于断言路由结果
type recordingServer struct {
	*httptest.Server
	mu    sync.Mutex
	model string
	calls int
}

func newRecordingServer(t *testing.T, content string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rs.mu.Lock()
		rs.model = req.Model
		rs.calls++
		rs.mu.Unlock()
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Model:   req.Model,
			Choices: []Choice{{Message: Message{Role: RoleAssistant, Content: content}}},
		})
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (r *recordingServer) lastModel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.model
}

func (r *recordingServer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// startService 按给定配置启动门面，测试结束自动停止
func startService(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	if err := Run(corectx.WithCfg(context.Background(), cfg)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { Stop(context.Background()) })
}

func TestParseStrategy(t *testing.T) {
	cases := map[string]Strategy{
		"":          StrategyDefault,
		"default":   StrategyDefault,
		"cheapest":  StrategyCheapest,
		"CHEAPEST":  StrategyCheapest,
		" cheapest": StrategyCheapest,
		"unknown":   StrategyDefault,
	}
	for in, want := range cases {
		if got := ParseStrategy(in); got != want {
			t.Errorf("ParseStrategy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	if got := EstimatePromptTokens(nil); got != 1 {
		t.Errorf("nil 请求 token 估算 = %d, want 1", got)
	}
	small := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	big := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: strings.Repeat("a", 400)}}}
	if got := EstimatePromptTokens(small); got < 1 {
		t.Errorf("小请求 token 估算 = %d, want >= 1", got)
	}
	if EstimatePromptTokens(big) <= EstimatePromptTokens(small) {
		t.Errorf("长内容估算(%d) 应大于短内容(%d)", EstimatePromptTokens(big), EstimatePromptTokens(small))
	}
	withTool := &ChatRequest{
		Messages: small.Messages,
		Tools: []Tool{{
			Type:     "function",
			Function: FunctionDef{Name: "f", Description: strings.Repeat("d", 100)},
		}},
	}
	if EstimatePromptTokens(withTool) <= EstimatePromptTokens(small) {
		t.Error("工具定义应计入输入 token 估算")
	}
}

func TestEstimateCost(t *testing.T) {
	m := ModelInfo{Name: "m", InputPrice: 1, OutputPrice: 2}
	// 输入 token = 4 字节/2+1 = 3；成本 = (3*1 + 1000*2)/1e6
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "aaaa"}}, MaxTokens: 1000}
	if got, want := EstimateCost(m, req), 2003.0/1e6; math.Abs(got-want) > 1e-12 {
		t.Errorf("EstimateCost = %v, want %v", got, want)
	}
	// 未设置 max_tokens 时按默认输出 token 估计
	req2 := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "aaaa"}}}
	if got, want := EstimateCost(m, req2), (3*1+defaultEstimateOutputTokens*2)/1e6; math.Abs(got-want) > 1e-12 {
		t.Errorf("EstimateCost(默认输出) = %v, want %v", got, want)
	}
	if !m.Priced() {
		t.Error("配置了价格的模型 Priced() 应为 true")
	}
	if (ModelInfo{}).Priced() {
		t.Error("未配置价格的模型 Priced() 应为 false")
	}
}

// 同一提供方配置多个模型：策略在提供方内部选更便宜的，显式指定则用指定模型
func TestCheapestWithinSingleProviderMultiModels(t *testing.T) {
	srv := newRecordingServer(t, "OK")

	startService(t, &config.Cfg{ModelAPI: &config.ModelAPIConfig{
		Enable:   "on",
		Strategy: "cheapest",
		Providers: map[string]*config.ModelProviderConfig{
			"only": {BaseURL: srv.URL, APIKey: "k", Model: "big-model", Models: []*config.ModelInfoConfig{
				{Name: "big-model", InputPrice: 5, OutputPrice: 15, SupportsTools: true},
				{Name: "small-model", InputPrice: 0.1, OutputPrice: 0.4, SupportsTools: true},
			}},
		},
	}})

	if got := ListProviders(); len(got) != 1 || got[0] != "only" {
		t.Fatalf("ListProviders = %v, want [only]", got)
	}
	if got := CurrentStrategy(); got != StrategyCheapest {
		t.Fatalf("CurrentStrategy = %q, want cheapest", got)
	}

	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	sel, err := SelectModel(req)
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	if sel.Provider != "only" || sel.Model != "small-model" {
		t.Errorf("选择结果 = %s, want only/small-model", sel)
	}

	if _, err := Chat(context.Background(), "", req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := srv.lastModel(); got != "small-model" {
		t.Errorf("请求模型 = %q, want small-model", got)
	}
	if req.Model != "" {
		t.Errorf("入参 req.Model 被改写为 %q，应保持为空", req.Model)
	}

	// 显式指定更贵的模型
	if _, err := Chat(context.Background(), "", &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "big-model",
	}); err != nil {
		t.Fatalf("Chat(big-model): %v", err)
	}
	if got := srv.lastModel(); got != "big-model" {
		t.Errorf("请求模型 = %q, want big-model", got)
	}
}

// 跨提供方比价：选中预估成本最低的提供方与模型
func TestSelectCheapestPicksLowestCost(t *testing.T) {
	srvA := newRecordingServer(t, "A")
	srvB := newRecordingServer(t, "B")

	startService(t, &config.Cfg{ModelAPI: &config.ModelAPIConfig{
		Enable:   "on",
		Default:  "b",
		Strategy: "cheapest",
		Providers: map[string]*config.ModelProviderConfig{
			"a": {BaseURL: srvA.URL, APIKey: "k", Model: "a-cheap", Models: []*config.ModelInfoConfig{
				{Name: "a-cheap", InputPrice: 0.1, OutputPrice: 0.1, SupportsTools: true},
			}},
			"b": {BaseURL: srvB.URL, APIKey: "k", Model: "b-pricey", Models: []*config.ModelInfoConfig{
				{Name: "b-pricey", InputPrice: 10, OutputPrice: 10, SupportsTools: true},
			}},
		},
	}})

	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	sel, err := SelectModel(req)
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	if sel.Provider != "a" || sel.Model != "a-cheap" {
		t.Errorf("选择结果 = %s, want a/a-cheap", sel)
	}
	if !sel.PriceKnown || sel.Fallback || sel.EstCost <= 0 {
		t.Errorf("选择结果标记异常: %s", sel)
	}

	resp, err := Chat(context.Background(), "", req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "A" {
		t.Errorf("content = %q, want A", resp.Choices[0].Message.Content)
	}
	if got := srvA.lastModel(); got != "a-cheap" {
		t.Errorf("供应方 a 收到模型 = %q, want a-cheap", got)
	}
	if got := srvB.callCount(); got != 0 {
		t.Errorf("供应方 b 调用次数 = %d, want 0", got)
	}
}

// 带 tools 的请求优先使用支持 Function Calling 的模型
func TestSelectCheapestPrefersToolCapable(t *testing.T) {
	srvA := newRecordingServer(t, "A") // 更便宜，但不支持工具调用
	srvB := newRecordingServer(t, "B") // 更贵，但支持工具调用

	startService(t, &config.Cfg{ModelAPI: &config.ModelAPIConfig{
		Enable:   "on",
		Default:  "a",
		Strategy: "cheapest",
		Providers: map[string]*config.ModelProviderConfig{
			"a": {BaseURL: srvA.URL, APIKey: "k", Model: "a-non-tool", Models: []*config.ModelInfoConfig{
				{Name: "a-non-tool", InputPrice: 0.01, OutputPrice: 0.01},
			}},
			"b": {BaseURL: srvB.URL, APIKey: "k", Model: "b-tool", Models: []*config.ModelInfoConfig{
				{Name: "b-tool", InputPrice: 1, OutputPrice: 1, SupportsTools: true},
			}},
		},
	}})

	plain := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	sel, err := SelectModel(plain)
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	if sel.Provider != "a" || sel.Model != "a-non-tool" {
		t.Errorf("无工具请求选择 = %s, want a/a-non-tool（取最便宜）", sel)
	}

	withTools := &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: FunctionDef{Name: "f"}}},
	}
	sel, err = SelectModel(withTools)
	if err != nil {
		t.Fatalf("SelectModel(tools): %v", err)
	}
	if sel.Provider != "b" || sel.Model != "b-tool" {
		t.Errorf("带工具请求选择 = %s, want b/b-tool（收敛到支持工具的模型）", sel)
	}

	resp, err := Chat(context.Background(), "", withTools)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "B" {
		t.Errorf("content = %q, want B", resp.Choices[0].Message.Content)
	}
	if got := srvB.lastModel(); got != "b-tool" {
		t.Errorf("供应方 b 收到模型 = %q, want b-tool", got)
	}
}

// 没有任何模型配置价格时：告警并回退到默认提供方默认模型，不阻断业务
func TestSelectCheapestFallbackWithoutPrice(t *testing.T) {
	srvA := newRecordingServer(t, "A")
	srvB := newRecordingServer(t, "B")

	startService(t, &config.Cfg{ModelAPI: &config.ModelAPIConfig{
		Enable:   "on",
		Default:  "a",
		Strategy: "cheapest",
		Providers: map[string]*config.ModelProviderConfig{
			"a": {BaseURL: srvA.URL, APIKey: "k", Models: []*config.ModelInfoConfig{{Name: "a-noprice"}}},
			"b": {BaseURL: srvB.URL, APIKey: "k", Models: []*config.ModelInfoConfig{{Name: "b-noprice"}}},
		},
	}})

	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	sel, err := SelectModel(req)
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	if !sel.Fallback || sel.Provider != "a" || sel.Model != "a-noprice" {
		t.Errorf("选择结果 = %s, want 回退 a/a-noprice", sel)
	}
	if sel.PriceKnown {
		t.Error("未配置价格时 PriceKnown 应为 false")
	}

	resp, err := Chat(context.Background(), "", req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "A" {
		t.Errorf("content = %q, want A", resp.Choices[0].Message.Content)
	}
	if got := srvB.callCount(); got != 0 {
		t.Errorf("供应方 b 调用次数 = %d, want 0", got)
	}
}

// 成本相同时结果确定：取提供方名最小者（与 default 无关）
func TestSelectCheapestTieBreakDeterministic(t *testing.T) {
	srvA := newRecordingServer(t, "A")
	srvB := newRecordingServer(t, "B")

	startService(t, &config.Cfg{ModelAPI: &config.ModelAPIConfig{
		Enable:   "on",
		Default:  "b",
		Strategy: "cheapest",
		Providers: map[string]*config.ModelProviderConfig{
			"a": {BaseURL: srvA.URL, APIKey: "k", Models: []*config.ModelInfoConfig{
				{Name: "m-a", InputPrice: 1, OutputPrice: 1, SupportsTools: true},
			}},
			"b": {BaseURL: srvB.URL, APIKey: "k", Models: []*config.ModelInfoConfig{
				{Name: "m-b", InputPrice: 1, OutputPrice: 1, SupportsTools: true},
			}},
		},
	}})

	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	for i := 0; i < 20; i++ {
		sel, err := SelectModel(req)
		if err != nil {
			t.Fatalf("SelectModel: %v", err)
		}
		if sel.Provider != "a" || sel.Model != "m-a" {
			t.Fatalf("第 %d 次选择 = %s，成本相同时应稳定取提供方名最小者", i, sel)
		}
	}
}

// 显式指定始终优先于策略：指定提供方 / 指定模型（跨提供方反查）/ 未登记模型透传
func TestFacadeExplicitRoutingBeatsStrategy(t *testing.T) {
	srvA := newRecordingServer(t, "A")
	srvB := newRecordingServer(t, "B")

	startService(t, &config.Cfg{ModelAPI: &config.ModelAPIConfig{
		Enable:   "on",
		Default:  "a",
		Strategy: "cheapest",
		Providers: map[string]*config.ModelProviderConfig{
			"a": {BaseURL: srvA.URL, APIKey: "k", Model: "m-a", Models: []*config.ModelInfoConfig{
				{Name: "m-a", InputPrice: 0.01, OutputPrice: 0.01, SupportsTools: true},
			}},
			"b": {BaseURL: srvB.URL, APIKey: "k", Model: "m-b", Models: []*config.ModelInfoConfig{
				{Name: "m-b", InputPrice: 9, OutputPrice: 9, SupportsTools: true},
			}},
		},
	}})

	// 指定提供方 b（尽管策略本会选 a）
	resp, err := Chat(context.Background(), "b", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat(指定提供方): %v", err)
	}
	if resp.Choices[0].Message.Content != "B" {
		t.Errorf("content = %q, want B", resp.Choices[0].Message.Content)
	}
	if got := srvB.lastModel(); got != "m-b" {
		t.Errorf("供应方 b 收到模型 = %q, want m-b", got)
	}

	// 指定模型 m-b：跨提供方反查到 b
	resp, err = Chat(context.Background(), "", &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "m-b",
	})
	if err != nil {
		t.Fatalf("Chat(指定模型): %v", err)
	}
	if resp.Choices[0].Message.Content != "B" {
		t.Errorf("content = %q, want B", resp.Choices[0].Message.Content)
	}

	// 未在任何提供方登记的模型：交给默认提供方透传
	resp, err = Chat(context.Background(), "", &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "unlisted-model",
	})
	if err != nil {
		t.Fatalf("Chat(未登记模型): %v", err)
	}
	if resp.Choices[0].Message.Content != "A" {
		t.Errorf("content = %q, want A", resp.Choices[0].Message.Content)
	}
	if got := srvA.lastModel(); got != "unlisted-model" {
		t.Errorf("供应方 a 收到模型 = %q, want unlisted-model", got)
	}
}

func TestSelectModelWhenNotRunning(t *testing.T) {
	Stop(context.Background())
	if _, err := SelectModel(&ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Error("服务未启动时 SelectModel 应报错")
	}
	if _, err := SelectModel(nil); err == nil {
		t.Error("请求为 nil 时应报错")
	}
	if got := CurrentStrategy(); got != StrategyDefault {
		t.Errorf("CurrentStrategy = %q, want default", got)
	}
	if _, err := Chat(context.Background(), "", nil); err == nil {
		t.Error("请求为 nil 时 Chat 应报错")
	}
}
