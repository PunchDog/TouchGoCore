package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
)

// testClient 构造测试客户端，backoffBase 调小加速重试测试
func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := NewClient("test", &config.ModelProviderConfig{
		BaseURL:    baseURL,
		APIKey:     "test-key",
		Model:      "test-model",
		Timeout:    5,
		MaxRetries: 2,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.backoffBase = time.Millisecond
	return c
}

func TestChatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header: %q", r.Header.Get("Authorization"))
		}
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q, want test-model", req.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{
			ID:    "chatcmpl-1",
			Model: "test-model",
			Choices: []Choice{{
				Index:        0,
				Message:      Message{Role: RoleAssistant, Content: "hello"},
				FinishReason: "stop",
			}},
			Usage: Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.Chat(context.Background(), &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello" {
		t.Errorf("content = %q, want hello", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 2 {
		t.Errorf("usage.total = %d, want 2", resp.Usage.TotalTokens)
	}
}

func TestChatErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error should contain server message, got: %v", err)
	}
}

func TestChatRetryOn500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Choices: []Choice{{Message: Message{Role: RoleAssistant, Content: "ok"}}},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Choices[0].Message.Content)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestChatRetryExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("want error after retries exhausted")
	}
	// max_retries=2 → 共 3 次请求
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestChatNoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("want error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (4xx 不应重试)", got)
	}
}

func TestChatToolCallsRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		// 请求含 role=tool 消息时返回最终答案
		for _, m := range req.Messages {
			if m.Role == RoleTool && m.ToolCallID == "call_1" {
				_ = json.NewEncoder(w).Encode(ChatResponse{
					Choices: []Choice{{Message: Message{Role: RoleAssistant, Content: "final"}}},
				})
				return
			}
		}
		// 首轮：返回 tool_calls
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Choices: []Choice{{
				Message: Message{
					Role: RoleAssistant,
					ToolCalls: []ToolCall{{
						ID:   "call_1",
						Type: "function",
						Function: FunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Beijing"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)

	// 第 1 轮：带 tools 定义
	resp, err := c.Chat(context.Background(), &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "weather in Beijing?"}},
		Tools: []Tool{{
			Type: "function",
			Function: FunctionDef{
				Name:        "get_weather",
				Description: "get weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat round1: %v", err)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", tc.Function.Name)
	}

	// 第 2 轮：回传 tool 结果
	resp2, err := c.Chat(context.Background(), &ChatRequest{
		Messages: []Message{
			{Role: RoleUser, Content: "weather in Beijing?"},
			resp.Choices[0].Message,
			{Role: RoleTool, ToolCallID: tc.ID, Content: `{"temp":20}`},
		},
	})
	if err != nil {
		t.Fatalf("Chat round2: %v", err)
	}
	if resp2.Choices[0].Message.Content != "final" {
		t.Errorf("content = %q, want final", resp2.Choices[0].Message.Content)
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("t", nil); err == nil {
		t.Error("want error for nil cfg")
	}
	if _, err := NewClient("t", &config.ModelProviderConfig{BaseURL: "  "}); err == nil {
		t.Error("want error for empty base_url")
	}
}

// echoServer 返回固定回复的模拟服务端
func echoServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Choices: []Choice{{Message: Message{Role: RoleAssistant, Content: content}}},
		})
	}))
}

func TestFacadeMultiProvider(t *testing.T) {
	srvA := echoServer(t, "A")
	srvB := echoServer(t, "B")
	defer srvA.Close()
	defer srvB.Close()

	cfg := &config.Cfg{
		ModelAPI: &config.ModelAPIConfig{
			Enable:  "on",
			Default: "a",
			Providers: map[string]*config.ModelProviderConfig{
				"a": {BaseURL: srvA.URL, APIKey: "k", Model: "m-a"},
				"b": {BaseURL: srvB.URL, APIKey: "k", Model: "m-b"},
			},
		},
	}
	ctx := corectx.WithCfg(context.Background(), cfg)
	if err := Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 默认提供方
	resp, err := Chat(context.Background(), "", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat default: %v", err)
	}
	if resp.Choices[0].Message.Content != "A" {
		t.Errorf("default content = %q, want A", resp.Choices[0].Message.Content)
	}

	// 指定提供方
	resp, err = Chat(context.Background(), "b", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat b: %v", err)
	}
	if resp.Choices[0].Message.Content != "B" {
		t.Errorf("b content = %q, want B", resp.Choices[0].Message.Content)
	}

	// 不存在的提供方
	if _, err := Chat(context.Background(), "nope", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Error("want error for unknown provider")
	}

	// 提供方列表
	names := ListProviders()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("ListProviders = %v, want [a b]", names)
	}

	// 停止后不可用
	Stop(context.Background())
	if _, err := Chat(context.Background(), "", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Error("want error after Stop")
	}
}

func TestFacadeSingleProviderDefault(t *testing.T) {
	srv := echoServer(t, "only")
	defer srv.Close()

	cfg := &config.Cfg{
		ModelAPI: &config.ModelAPIConfig{
			Enable: "on",
			Providers: map[string]*config.ModelProviderConfig{
				"only": {BaseURL: srv.URL, APIKey: "k", Model: "m"},
			},
		},
	}
	ctx := corectx.WithCfg(context.Background(), cfg)
	if err := Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer Stop(context.Background())

	resp, err := Chat(context.Background(), "", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "only" {
		t.Errorf("content = %q, want only", resp.Choices[0].Message.Content)
	}
}

func TestFacadeNotConfigured(t *testing.T) {
	// 无 model_api 配置时 Run 直接返回 nil，Chat 报错
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	if err := Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := Chat(context.Background(), "", &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Error("want error when not configured")
	}
}
