package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"touchgocore/config"
)

const (
	defaultTimeout    = 60 * time.Second
	defaultMaxRetries = 2
	maxBackoff        = 10 * time.Second
)

// Client OpenAI 兼容模型 API 客户端
type Client struct {
	name        string // 提供方名
	cfg         *config.ModelProviderConfig
	baseURL     string
	apiKey      string
	model       string      // 默认模型名
	models      []ModelInfo // 该提供方已配置的模型列表（参与策略比价）
	timeout     time.Duration
	maxRetry    int
	backoffBase time.Duration // 退避基数，默认 500ms（测试可调小）
	http        *http.Client
}

// Name 返回提供方名
func (c *Client) Name() string { return c.name }

// DefaultModel 返回提供方的默认模型名
func (c *Client) DefaultModel() string { return c.model }

// Models 返回提供方已配置的模型列表（副本，避免调用方改动内部状态）
func (c *Client) Models() []ModelInfo {
	out := make([]ModelInfo, len(c.models))
	copy(out, c.models)
	return out
}

// HasModel 是否拥有该模型（含默认模型，兼容只配置 model 的旧配置）
func (c *Client) HasModel(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if name == c.model {
		return true
	}
	for _, m := range c.models {
		if m.Name == name {
			return true
		}
	}
	return false
}

// NewClient 创建客户端。name 为提供方名（用于查找与日志）。
func NewClient(name string, cfg *config.ModelProviderConfig) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("模型提供方[%s]配置为 nil", name)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("模型提供方[%s]base_url 为空", name)
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = defaultTimeout
	}
	retries := cfg.MaxRetries
	if retries < 0 {
		retries = defaultMaxRetries
	}
	models := make([]ModelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m == nil || strings.TrimSpace(m.Name) == "" {
			continue
		}
		models = append(models, ModelInfo{
			Name:          strings.TrimSpace(m.Name),
			InputPrice:    m.InputPrice,
			OutputPrice:   m.OutputPrice,
			SupportsTools: m.SupportsTools,
		})
	}
	return &Client{
		name:      name,
		cfg:       cfg,
		baseURL:   base,
		apiKey:    strings.TrimSpace(cfg.APIKey),
		model:     cfg.DefaultModel(),
		models:    models,
		timeout:   timeout,
		maxRetry:  retries,
		http:      &http.Client{}, // 默认 Transport，连接池复用
	}, nil
}

// Chat 同步调用 chat/completions，429/5xx 自动重试。
// 不修改入参 req：需要补全模型名时先浅拷贝再发送，避免并发复用同一请求时相互污染。
func (c *Client) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}
	out := *req // 浅拷贝，messages/tools 切片只读共享
	if out.Model == "" {
		out.Model = c.model
	}
	if out.Model == "" {
		return nil, fmt.Errorf("模型提供方[%s] 未配置默认模型，且请求未指定 model", c.name)
	}
	body, err := json.Marshal(&out)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.doRequest(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		var re *retryableError
		if !errors.As(err, &re) || attempt >= c.maxRetry {
			break
		}
		delay := c.backoffDuration(attempt)
		if re.retryAfter > delay {
			delay = re.retryAfter // 尊重服务端 Retry-After
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// doRequest 发送单次请求并解析响应
func (c *Client) doRequest(ctx context.Context, body []byte) (*ChatResponse, error) {
	reqCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		// 网络错误/超时：可重试
		return nil, &retryableError{err: err}
	}
	defer httpResp.Body.Close()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &retryableError{err: fmt.Errorf("读取响应失败: %w", err)}
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, parseHTTPError(httpResp.StatusCode, httpResp.Header.Get("Retry-After"), data)
	}

	var out ChatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return &out, nil
}

// parseHTTPError 解析错误状态码与标准错误体
func parseHTTPError(status int, retryAfter string, data []byte) error {
	var eb errorBody
	msg := http.StatusText(status)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &eb); err == nil && eb.Error.Message != "" {
			msg = eb.Error.Message
		}
	}
	apiErr := &APIError{
		Message: fmt.Sprintf("模型API请求失败: HTTP %d %s", status, msg),
		Type:    "http_error",
		Code:    status,
	}
	// 仅 429/5xx 可重试，其他 4xx 直接返回
	if status == http.StatusTooManyRequests || status >= 500 {
		var ra time.Duration
		if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
			ra = time.Duration(secs) * time.Second
		}
		return &retryableError{err: apiErr, retryAfter: ra}
	}
	return apiErr
}

// retryableError 标记可重试错误
type retryableError struct {
	err        error
	retryAfter time.Duration // 0 表示未设置
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// backoffDuration 指数退避 + 抖动
func (c *Client) backoffDuration(attempt int) time.Duration {
	base := c.backoffBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	shift := attempt
	if shift > 10 {
		shift = 10
	}
	d := base * time.Duration(1<<uint(shift))
	if d > maxBackoff {
		d = maxBackoff
	}
	// 抖动避免重试雪崩
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// sleepCtx 可被 context 取消的等待
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
