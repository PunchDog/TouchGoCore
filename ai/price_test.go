package ai

import (
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
)

// litellm 格式样例（美元/token）
const litellmSample = `{
	"gpt-x":        {"input_cost_per_token":0.000002,"output_cost_per_token":0.000006},
	"GPT-Y":        {"input_cost_per_token":0.000001,"output_cost_per_token":0.000002},
	"openai/gpt-z": {"input_cost_per_token":0.000003,"output_cost_per_token":0.000009}
}`

// newTestPriceSource 用 httptest 构造价格源
func newTestPriceSource(t *testing.T, provider, body string, status int) *PriceSource {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewPriceSource(&config.PriceSourceConfig{Provider: provider, URL: srv.URL, Timeout: 2})
}

func TestPriceSourceDisabled(t *testing.T) {
	if NewPriceSource(nil) != nil {
		t.Error("未配置价格源时应返回 nil")
	}
	if NewPriceSource(&config.PriceSourceConfig{Provider: "off"}) != nil {
		t.Error("provider=off 时应返回 nil")
	}
	if NewPriceSource(&config.PriceSourceConfig{Provider: "unknown"}) != nil {
		t.Error("未知 provider 应返回 nil")
	}
}

func TestLiteLLMParseAndLookup(t *testing.T) {
	src := newTestPriceSource(t, "litellm", litellmSample, http.StatusOK)

	// 精确匹配 + ×1e6 换算（0.000002 美元/token → 2 美元/百万 token）
	if q, ok := src.Lookup("openai", "gpt-x"); !ok || q.Input != 2 || q.Output != 6 {
		t.Errorf("Lookup(gpt-x) = %+v ok=%v, want input=2 output=6", q, ok)
	}
	// 大小写不敏感
	if q, ok := src.Lookup("openai", "gpt-y"); !ok || q.Input != 1 {
		t.Errorf("Lookup(gpt-y) = %+v ok=%v, want input=1", q, ok)
	}
	// 提供方名前缀匹配（LiteLLM 键风格）
	if q, ok := src.Lookup("openai", "gpt-z"); !ok || q.Input != 3 {
		t.Errorf("Lookup(gpt-z) = %+v ok=%v, want input=3（前缀匹配）", q, ok)
	}
	// 未命中
	if _, ok := src.Lookup("openai", "nope"); ok {
		t.Error("未命中应返回 ok=false")
	}
}

func TestOpenRouterParseAndLookup(t *testing.T) {
	body := `{"data":[
		{"id":"deepseek/deepseek-chat","pricing":{"prompt":"0.00000027","completion":"0.0000011"}},
		{"id":"free-model","pricing":{"prompt":"0","completion":"0"}},
		{"id":"bad-model","pricing":{"prompt":"-1","completion":"-1"}}
	]}`
	src := newTestPriceSource(t, "openrouter", body, http.StatusOK)

	if q, ok := src.Lookup("openrouter", "deepseek/deepseek-chat"); !ok ||
		math.Abs(q.Input-0.27) > 1e-9 || math.Abs(q.Output-1.1) > 1e-9 {
		t.Errorf("Lookup(deepseek-chat) = %+v ok=%v, want 0.27/1.1", q, ok)
	}
	// 免费模型（价格 0）仍视为有价格
	if q, ok := src.Lookup("openrouter", "free-model"); !ok || q.Input != 0 || q.Output != 0 {
		t.Errorf("Lookup(free-model) = %+v ok=%v, want 0/0 且 ok=true", q, ok)
	}
	// 价格全非法的条目被跳过
	if _, ok := src.Lookup("openrouter", "bad-model"); ok {
		t.Error("价格全非法的条目应被跳过")
	}
}

func TestPriceSourceTTLRefresh(t *testing.T) {
	var version atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version.Load() == 0 {
			_, _ = w.Write([]byte(`{"m":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000001}}`))
			return
		}
		_, _ = w.Write([]byte(`{"m":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000002}}`))
	}))
	defer srv.Close()

	src := NewPriceSource(&config.PriceSourceConfig{Provider: "litellm", URL: srv.URL, Timeout: 2})
	src.ttl = 30 * time.Millisecond // 缩短 TTL 便于测试

	if q, _ := src.Lookup("openai", "m"); q.Input != 1 {
		t.Fatalf("首次查询 = %+v, want input=1", q)
	}
	version.Store(1)
	time.Sleep(60 * time.Millisecond) // 等待缓存过期
	if q, _ := src.Lookup("openai", "m"); q.Input != 2 {
		t.Errorf("缓存过期后应重新拉取, got %+v, want input=2", q)
	}
}

func TestPriceSourceFailBackoff(t *testing.T) {
	var okSrv atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !okSrv.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"m":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000001}}`))
	}))
	defer srv.Close()

	src := NewPriceSource(&config.PriceSourceConfig{Provider: "litellm", URL: srv.URL, Timeout: 2})
	if _, ok := src.Lookup("openai", "m"); ok {
		t.Fatal("拉取失败时应返回 ok=false")
	}
	okSrv.Store(true)
	// 失败退避期内即使服务恢复也不重试
	if _, ok := src.Lookup("openai", "m"); ok {
		t.Error("失败退避期内不应重新拉取")
	}
	// 退避期过后恢复
	src.failAt = time.Now().Add(-failBackoff)
	if q, ok := src.Lookup("openai", "m"); !ok || q.Input != 1 {
		t.Errorf("退避期后应重新拉取成功, got %+v ok=%v", q, ok)
	}
}

func TestPriceSourceSingleFlight(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(50 * time.Millisecond) // 放大并发窗口
		_, _ = w.Write([]byte(`{"m":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000001}}`))
	}))
	defer srv.Close()

	src := NewPriceSource(&config.PriceSourceConfig{Provider: "litellm", URL: srv.URL, Timeout: 5})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src.Lookup("openai", "m")
		}()
	}
	wg.Wait()
	if got := fetches.Load(); got != 1 {
		t.Errorf("并发查询拉取次数 = %d, want 1（单飞刷新）", got)
	}
}
