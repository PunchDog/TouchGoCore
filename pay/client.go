package pay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout 单次请求超时，与 ai 包客户端保持同量级。
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRetry 重试次数（不含首次请求）。
	DefaultMaxRetry = 2
	// maxBackoff 退避上限，避免供应商长时间挂住时重试间隔失控。
	maxBackoff = 10 * time.Second
	// 读取响应体的上限，防止异常供应商把内存吃光。
	maxResponseBytes = 1 << 20
)

// Options 是客户端构造参数。通道配置结构体到这里的逐字段转换留在各通道包，
// pay 不认识 config，避免成环。
type Options struct {
	// Name 是通道名（whatsapp/ustd/ton），进错误文案，不进凭证。
	Name        string
	BaseURL     string
	Timeout     time.Duration
	MaxRetry    int
	BackoffBase time.Duration
	UserAgent   string
}

// Client 是带超时、上下文取消与指数退避重试的出站 HTTP 客户端。
//
// http.Client 在构造时建一次并全程共享：每次请求新建会让连接池失效，
// 出款接口本来就该低频，池一没就有冷启动握手叠加超时。
type Client struct {
	name        string
	baseURL     string
	timeout     time.Duration
	maxRetry    int
	backoffBase time.Duration
	userAgent   string
	http        *http.Client
}

// NewClient 构造客户端，缺省项就地归一。baseURL 为空时返回 error，
// 调用方据此走「配置不全 ⇒ 不启动」的路径。
func NewClient(opt Options) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opt.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("通道[%s]base_url 为空", opt.Name)
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("通道[%s]base_url 非法: %w", opt.Name, err)
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxRetry := opt.MaxRetry
	if maxRetry < 0 {
		maxRetry = DefaultMaxRetry
	}
	backoff := opt.BackoffBase
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	return &Client{
		name:        opt.Name,
		baseURL:     base,
		timeout:     timeout,
		maxRetry:    maxRetry,
		backoffBase: backoff,
		userAgent:   strings.TrimSpace(opt.UserAgent),
		http:        &http.Client{},
	}, nil
}

// BaseURL 返回归一化后的基址（末尾无斜杠）。
func (c *Client) BaseURL() string { return c.baseURL }

// CallSpec 描述一次调用。
//
// Sign 与 Parse 是留给真实供应商接口文档的两个注入点：签名怎么算、包络长什么样，
// 都由通道包各自提供，本层只负责送出去、按分类决定要不要重发。
type CallSpec struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
	// Header 是本次调用的静态请求头（token 头之类），在 Sign 之前写入。
	Header http.Header
	// Sign 往请求上补签名头。此时 URL 查询参数与 Body 都已定稿，
	// 需要把二者一起纳入签名域的算法在这里都能拿到。
	Sign func(req *http.Request, body []byte) error
	// Parse 把响应换成归一载荷。返回 *ProviderError 且 Retryable=true 时本层会重发。
	// 为 nil 时使用 DefaultParse（按 2xx / 429 / 5xx 分类）。
	Parse func(status int, hdr http.Header, body []byte) (json.RawMessage, error)
}

// Do 发送请求并按错误分类重试，成功返回归一载荷。
//
// 重发用的是同一个 spec 与同一个 Body，也就是同一张订单号——这是幂等键存在的意义：
// 换单重发等于重复出款。
func (c *Client) Do(ctx context.Context, spec CallSpec) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		payload, retryAfter, err := c.attempt(ctx, spec)
		if err == nil {
			return payload, nil
		}
		lastErr = err
		if !IsProviderRetryable(err) || attempt >= c.maxRetry {
			break
		}
		delay := c.backoffDuration(attempt)
		if retryAfter > delay {
			delay = retryAfter // 服务端明确要求等多久，就至少等多久
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt 发一次请求。返回的 retryAfter 仅在本次失败且服务端给了 Retry-After 时为正。
func (c *Client) attempt(ctx context.Context, spec CallSpec) (json.RawMessage, time.Duration, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	fullURL := c.baseURL + spec.Path
	if len(spec.Query) > 0 {
		fullURL += "?" + spec.Query.Encode()
	}
	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, method, fullURL, bytes.NewReader(spec.Body))
	if err != nil {
		return nil, 0, &ProviderError{Channel: c.name, Code: "bad_request", Msg: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.userAgent != "" {
		httpReq.Header.Set("User-Agent", c.userAgent)
	}
	for k, vs := range spec.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if spec.Sign != nil {
		if err := spec.Sign(httpReq, spec.Body); err != nil {
			// 签名失败说明本地入参或算法有问题，重发只会一样失败。
			return nil, 0, &ProviderError{Channel: c.name, Code: "sign_failed", Msg: err.Error()}
		}
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, &ProviderError{Channel: c.name, Code: "transport", Msg: err.Error(), Retryable: retryableByCtx(ctx, err)}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, &ProviderError{Channel: c.name, Code: "read_body", Msg: "读取响应失败", Retryable: true}
	}

	parse := spec.Parse
	if parse == nil {
		parse = DefaultParse(c.name)
	}
	out, err := parse(resp.StatusCode, resp.Header, data)
	if err != nil {
		return nil, retryAfterOf(resp.Header), err
	}
	return out, 0, nil
}

// retryableByCtx 区分「调用方主动取消」与「请求自己超时」。
//
// 父 ctx 已取消时重发没有意义（上层已经不等了），必须判为不可重试；
// 而单次请求超时属于供应商侧抖动，可以重发。
func retryableByCtx(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// retryAfterOf 解析 Retry-After，仅支持秒数形态。
func retryAfterOf(hdr http.Header) time.Duration {
	v := strings.TrimSpace(hdr.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// DefaultParse 返回按传输层状态码分类的解析器：
// 2xx 原样透出响应体；429 与 5xx 标记可重试；其余 4xx 直接失败。
//
// 真实包络通常是 HTTP 200 + 业务码，届时各通道用自己的 Parse 覆盖这里，
// 由它来决定哪个业务码算「结果未知、不可自动重下单」。
func DefaultParse(channel string) func(int, http.Header, []byte) (json.RawMessage, error) {
	return func(status int, hdr http.Header, body []byte) (json.RawMessage, error) {
		if status >= 200 && status < 300 {
			return json.RawMessage(body), nil
		}
		return nil, &ProviderError{
			Channel:    channel,
			Code:       strconv.Itoa(status),
			Msg:        http.StatusText(status),
			HTTPStatus: status,
			Retryable:  status == http.StatusTooManyRequests || status >= 500,
		}
	}
}

// backoffDuration 指数退避带抖动，避免多个通道同时恢复时撞成重试雪崩。
func (c *Client) backoffDuration(attempt int) time.Duration {
	shift := attempt
	if shift > 10 {
		shift = 10
	}
	d := c.backoffBase * time.Duration(1<<uint(shift))
	if d > maxBackoff {
		d = maxBackoff
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// sleepCtx 可被上下文取消的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
