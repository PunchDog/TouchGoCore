package nft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout 单次请求超时，与 pay 侧客户端保持同量级。
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRetry 重试次数（不含首次请求）。
	DefaultMaxRetry = 2
	// maxBackoff 退避上限，避免供应商长时间挂住时重试间隔失控。
	maxBackoff = 10 * time.Second
	// 读取响应体的上限，防止异常供应商把内存吃光。
	maxResponseBytes = 1 << 20
	// maxRetryCap 是重试次数的硬上限：坏配置（max_retries 填成一百万）与恶意供应商
	// 都能把一次外呼放大成小时级风暴，上限必须钉在本地而不是靠配置自觉（NF-F7）。
	maxRetryCap = 5
	// maxTimeout 是单次请求超时的硬上限，同上：timeout_sec 写成一天不该让调用方挂一天。
	maxTimeout = 2 * time.Minute
)

// Options 是客户端构造参数。通道配置结构体到这里的逐字段转换留在装配层（wire.go），
// nft 核心层不认识 config，避免成环。
type Options struct {
	// Name 是通道名（"nft"），进错误文案，不进凭证。
	Name        string
	BaseURL     string
	Timeout     time.Duration
	MaxRetry    int
	BackoffBase time.Duration
	UserAgent   string
}

// Client 是带超时、上下文取消与指数退避重试的出站 HTTP 客户端。
//
// http.Client 在构造时建一次并全程共享：每次请求新建会让连接池失效。
// 构造后字段只读。
type Client struct {
	name        string
	baseURL     string
	baseHost    string
	timeout     time.Duration
	maxRetry    int
	backoffBase time.Duration
	userAgent   string
	http        *http.Client
}

// NewClient 构造客户端，缺省项就地归一。baseURL 为空时返回 error，
// 调用方据此走「配置不全 ⇒ 不启动」的路径。
//
// 基址形态在这里一次钉死（NF-F4/F7/F8）：它同时是「凭证会不会跟着请求走漏」的第一道闸——
// 内嵌 userinfo 的基址会原样出现在传输层错误文案里，明文 scheme 会把签名头与会话 token
// 摆到可被链路监听的通道上，过大的重试/超时让坏配置或恶意供应商能拖死调用方。
func NewClient(opt Options) (*Client, error) {
	raw := strings.TrimSpace(opt.BaseURL)
	if raw == "" {
		return nil, fmt.Errorf("通道[%s]base_url 为空", opt.Name)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// 不回显 err 原文：*url.Error 里带着完整 URL（可能含凭证）。
		return nil, fmt.Errorf("通道[%s]base_url 无法解析（非法转义或语法错）", opt.Name)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("通道[%s]base_url 解析不出主机，请写完整 URL（如 https://api.example.com）", opt.Name)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("通道[%s]base_url 不得内嵌凭证（userinfo 会随错误文案与日志外泄）", opt.Name)
	}
	scheme := strings.ToLower(parsed.Scheme)
	// 签名头与会话 token 走明文等于送给链路监听者；只对本机联调与 httptest 放开 http。
	// 其它 scheme（ftp/file/…）连「明文通道」都算不上，一律按非法形态拒掉。
	if scheme != "https" && (scheme != "http" || !isLoopbackHost(parsed.Hostname())) {
		return nil, fmt.Errorf("通道[%s]base_url 必须 https（明文通道仅允许 http://127.0.0.1/localhost/[::1]）", opt.Name)
	}
	base := strings.TrimRight(raw, "/")

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	} else if timeout > maxTimeout {
		timeout = maxTimeout
	}
	maxRetry := opt.MaxRetry
	if maxRetry < 0 {
		maxRetry = DefaultMaxRetry
	} else if maxRetry > maxRetryCap {
		maxRetry = maxRetryCap
	}
	backoff := opt.BackoffBase
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	hc := &http.Client{}
	// 重定向策略（NF-F1）：Go 默认跟随至多 10 跳，且跨主机时只自动剔除
	// Authorization/Cookie 等四类头——本通道的签名头是自定义名（X-Sign 之类），
	// 会话凭证头名由配置给出，都不在那份名单里。基址被劫持或反代被投毒时，
	// MD5(签名域∥secret) 会连同可观测的签名域一起送到攻击者主机上，可被离线爆破还原密钥。
	// 于是：跨 scheme/host 一律不回跳、不改投，把 3xx 原样交给解析层按「非受理」定性。
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse
		}
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &Client{
		name:        opt.Name,
		baseURL:     base,
		baseHost:    parsed.Host,
		timeout:     timeout,
		maxRetry:    maxRetry,
		backoffBase: backoff,
		userAgent:   strings.TrimSpace(opt.UserAgent),
		http:        hc,
	}, nil
}

// isLoopbackHost 是否本机回环地址（含 localhost 与 127/8 网段，IPv6 的 [::1] 已在
// Hostname() 中去掉方括号）。
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// BaseURL 返回归一化后的基址（末尾无斜杠）。
func (c *Client) BaseURL() string { return c.baseURL }

// redact 抹掉错误文案里可能带出的出站地址信息（NF-F4）。
//
// Go 的 *url.Error 形如 `Post "完整URL": dial tcp: lookup 主机名: no such host`：
// 前半段带着基址与路径，后半段还额外重复一次裸主机名。基址与查询参数都可能带
// 凭证形态的段（token 查询参数、path 里的会话串），而错误会一路进日志与上游回执，
// 所以这一段只能在本层抹平——Go 自己不知道哪些片段是敏感的。
// 保留结构性信息（`invalid URL escape "%zz"`、`connection refused`）以便定位。
func (c *Client) redact(s, fullURL string) string {
	if fullURL != "" {
		s = strings.ReplaceAll(s, fullURL, "<url>")
	}
	if c.baseURL != "" {
		s = strings.ReplaceAll(s, c.baseURL, "<url>")
	}
	if c.baseHost != "" {
		s = strings.ReplaceAll(s, c.baseHost, "<host>")
	}
	return s
}

// CallSpec 描述一次调用。
//
// Sign 与 Parse 是留给真实供应商接口文档的两个注入点：签名怎么算、包络长什么样，
// 都由 provider 侧提供，本层只负责送出去、按分类决定要不要重发。
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
	// noTransportRetry 是严格路径标记（Mint/Transfer 专用，由 Provider.send 置位）：
	// 为 true 时 transport / read_body 错误一律 Retryable=false。
	// 这两类错误发生在「请求已经写出、响应没能读回」的窗口里——供应商可能已经
	// 受理了这一单，客户端原单重发在幂等不完备时就是二次铸造/双花转账。响应解析层的
	// 白名单（parseStrict）管不到这里，必须在本层单独收口。
	// 刻意不导出：通道包不得自行构造带此标记的 CallSpec，严格口径的判定权留在 nft 内部
	// （同 pay：只能经 Provider.Mint/Transfer 置位）。
	noTransportRetry bool
}

// Do 发送请求并按错误分类重试，成功返回归一载荷。
//
// 重发用的是同一个 spec 与同一个 Body，也就是同一张订单号——这是幂等键存在的意义：
// 换单重发等于重复出资产动作。
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
		// 只透出抹平后的原因：原始文案带着完整 URL（基址可能含凭证段）。
		return nil, 0, &ProviderError{Channel: c.name, Code: "bad_request",
			Msg: c.redact(err.Error(), fullURL)}
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
			// 文案一律不透出：签名入参按定义就是签名域 ∥ secret_key 与元数据 URI，
			// 供应商实现常在报错里把这些原样拼上，回显等于把密钥写进日志（NF-F4）。
			return nil, 0, &ProviderError{Channel: c.name, Code: "sign_failed",
				Msg: "签名生成失败（本地签名实现报错，原因可能含签名域与密钥，已抑制）"}
		}
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// 严格路径下传输层错误不自动重发：请求可能已被供应商受理，只是响应没回来。
		retryable := retryableByCtx(ctx, err) && !spec.noTransportRetry
		return nil, 0, &ProviderError{Channel: c.name, Code: "transport",
			Msg: c.redact(err.Error(), fullURL), Retryable: retryable}
	}
	defer resp.Body.Close()

	// 多读 1 字节用来区分「正好读满」与「被截断」：截断意味着受理结论（code/data）
	// 可能根本没读全，此时把残体交给解析层是拿猜测当回执（NF-F5）。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		// 读体失败时状态行与头已经到了，但载荷（含受理结论）丢了——严格路径同样不得重发。
		return nil, 0, &ProviderError{Channel: c.name, Code: "read_body", Msg: "读取响应失败", Retryable: !spec.noTransportRetry}
	}
	if len(data) > maxResponseBytes {
		return nil, 0, &ProviderError{Channel: c.name, Code: "read_body",
			Msg:        "响应体超过上限，受理结论无法读出",
			HTTPStatus: resp.StatusCode,
			Retryable:  !spec.noTransportRetry}
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
//
// 返回值一律夹在 (0, maxBackoff]（NF-F7）：「服务端说等多久就至少等多久」这句原话
// 给了对方一个拖死调用方的口子——被攻陷或恶意的供应商对只读查询回
// Retry-After: 315360000，每个 goroutine 就挂一年，而门面允许 ctx 为 nil 回落 Background，
// 连 deadline 都救不了。封顶后仍保留「至少等对方说的时间」的语义，只是不超过本地退避上限。
func retryAfterOf(hdr http.Header) time.Duration {
	v := strings.TrimSpace(hdr.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// DefaultParse 返回按传输层状态码分类的解析器：
// 2xx 原样透出响应体；429 与 5xx 标记可重试；其余 4xx 直接失败。
//
// 真实包络通常是 HTTP 200 + 业务码，届时由 Provider.parse 覆盖这里，
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
