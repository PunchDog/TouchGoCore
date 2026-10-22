package telegram

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"touchgocore/internal/httpx"
	"touchgocore/pay"
)

// 本文件是 TON 侧的供应商账户查询客户端。
//
// 它与 tron/account.go 刻意同构而不共用：出款已经上链，供应商侧只剩「读一次额度」
// 这一件事，两份各约百行的实现换来的是「改 TON 不必回头看 TRON」。共用一份的代价
// 在旧实现里已经付过——pay.Provider 同时装着报文、端点、重试与解析四层，
// 谁都动不了手。
//
// 与 TRON 侧的一处真实差异：本通道只发原生 TON，账户查询没有通道特有字段，
// 所以这里没有 extras 那套「追加进报文与签名域」的机器。将来真要接 jetton，
// 得把那套机器一起搬过来——报文有、签名域没有，供应商会全量拒签（理由见
// tron/account.go 的 mergeExtraFields）。

const (
	// tonCodeOK 是包络里的成功码。真实成功码等文档确认，在此之前统一按 "0" 判。
	tonCodeOK = "0"
	// tonDefaultAuthHeader 是签名请求头名的缺省值，配置留空时生效。
	tonDefaultAuthHeader = "X-Sign"
)

// tonAccountQuerier 是链上通道对「供应商侧账户」的唯一依赖。
//
// 宽度只有 QueryAccount 一个方法：链上通道不需要 Withdraw/QueryOrder，
// 而按旧接口收一个供应商实现，等于允许它在某天被误当成出款路径。
type tonAccountQuerier interface {
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
}

// tonAccountClient 是供应商侧账户查询的实现。
type tonAccountClient struct {
	hc   *httpx.Client
	name string
	opt  channelOptions
}

// tonSignMD5 按「签名字段值直连拼接 + 密钥」取 32 位小写 MD5。
//
// 用 MD5 不是这里挑的口令哈希方案，而是供应商接口规定的形态：值直连拼接、
// 无分隔符、末尾追加密钥。字段顺序必须由调用方逐字照文档登记——顺序错一位
// 就是全量验签失败，而且本地永远算得过、服务端永远全拒，最难查。
func tonSignMD5(secret string, values ...string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(v)
	}
	b.WriteString(secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// tonCheckHeaderName 校验请求头名是可用的 token 形态。
func tonCheckHeaderName(channel, field, name string) error {
	if name == "" || strings.ContainsAny(name, " :;\t\r\n") {
		return fmt.Errorf("通道[%s]的 %s 不是合法请求头名", channel, field)
	}
	return nil
}

// newTonAccountClient 构造账户查询客户端。
//
// 凭证缺失就地拒绝：资金接口带着空密钥出去只会在供应商侧留下一堆验签失败日志，
// 本地却看起来「启动成功」。
func newTonAccountClient(opt channelOptions) (*tonAccountClient, error) {
	if strings.TrimSpace(opt.SecretKey) == "" {
		return nil, fmt.Errorf("通道[%s]未配置 secret_key", opt.Name)
	}
	if opt.Endpoint == nil {
		return nil, fmt.Errorf("通道[%s]未提供 endpoints 取值函数", opt.Name)
	}
	hc, err := httpx.NewClient(httpx.Options{
		Name:      opt.Name,
		BaseURL:   opt.BaseURL,
		Timeout:   opt.Timeout,
		MaxRetry:  opt.MaxRetry,
		UserAgent: "touchgocore-" + opt.Name,
	})
	if err != nil {
		return nil, err
	}
	authHeader := strings.TrimSpace(opt.AuthHeader)
	if authHeader == "" {
		authHeader = tonDefaultAuthHeader
	}
	if err := tonCheckHeaderName(opt.Name, "auth_header", authHeader); err != nil {
		return nil, err
	}
	opt.AuthHeader = authHeader
	opt.AppID = strings.TrimSpace(opt.AppID)
	opt.MerchantID = strings.TrimSpace(opt.MerchantID)
	return &tonAccountClient{hc: hc, name: opt.Name, opt: opt}, nil
}

// tonAccountRequest 是商户账户查询报文。
type tonAccountRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
}

// signValues 账户查询报文签名域（顺序即签名域顺序）。
func (r *tonAccountRequest) signValues() []string {
	return []string{r.AppID, r.MerchantID, r.Currency}
}

// tonAccountData 是账户回执里的载荷字段。
//
// fee_rate_bps 只接受整数万分比：供应商若给 "0.3%" 这类字符串，真实文档到位后
// 在本结构与归一函数之间做一次显式换算，不在这里塞浮点字段。
type tonAccountData struct {
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	Balance    int64  `json:"balance"`
	Available  int64  `json:"available"`
	Frozen     int64  `json:"frozen"`
	Credit     int64  `json:"credit"`
	FeeRateBps int64  `json:"fee_rate_bps"`
	Status     string `json:"status"`
}

// QueryAccount 查我方在供应商侧的 TON 商户账户（余额/可用/授信/费率/状态）。
//
// 这是读操作：不广播回执。下游要的是当场判断（能不能提这么多），而额度账本
// 与风控都在业务侧，本包给的只是一次供应商侧的快照。
func (c *tonAccountClient) QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	if q == nil {
		q = &pay.AccountQuery{}
	}
	req := tonAccountRequest{
		AppID:      c.opt.AppID,
		MerchantID: tonFirstNonEmpty(strings.TrimSpace(q.MerchantID), c.opt.MerchantID),
		Currency:   strings.TrimSpace(q.Currency),
	}
	data, err := c.call(ctx, pay.EndpointAccount, req, req.signValues())
	if err != nil {
		return nil, err
	}
	return c.toAccountInfo(data, req)
}

// call 按逻辑端点名发一次 POST：取路径 → 序列化 → 签名 → 解包络。
//
// signValues 必须与报文里参与签名的字段一一对应且顺序一致。两者写在同一段代码里
// 是刻意的——报文加了字段却忘了同步签名域，是供应商全量拒签的典型成因。
func (c *tonAccountClient) call(ctx context.Context, endpoint string, payload any, signValues []string) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]序列化请求失败: %w", c.name, err)
	}
	path, ok := c.opt.Endpoint(endpoint)
	if !ok {
		return nil, fmt.Errorf("通道[%s]未配置 endpoints.%s，该操作不可用", c.name, endpoint)
	}
	secret, header := c.opt.SecretKey, c.opt.AuthHeader
	return c.hc.Do(ctx, httpx.CallSpec{
		Method: http.MethodPost,
		Path:   path,
		Body:   body,
		Sign: func(req *http.Request, _ []byte) error {
			req.Header.Set(header, tonSignMD5(secret, signValues...))
			return nil
		},
		Parse: c.parse,
	})
}

// parse 把响应包络拆成 data 载荷；业务失败转成 ProviderError 并带上可重试标记。
//
// 账户查询是幂等读，走宽容口径（429 与 5xx 默认可重试）。本仓已无 HTTP 写路径，
// 严格口径（写侧原单重发要靠白名单背书）留在 internal/httpx 的 StrictParse 里。
func (c *tonAccountClient) parse(status int, _ http.Header, body []byte) (json.RawMessage, error) {
	// 非 2xx 先按传输层判掉：网关/反代拦下来的 502 页面里偶然出现 "code":"0"
	// 并不能算供应商受理，认成功就等于把一次读故障记成了一次成功查询。
	if status < 200 || status >= 300 {
		return nil, &pay.ProviderError{
			Channel:    c.name,
			Code:       strconv.Itoa(status),
			Msg:        http.StatusText(status),
			HTTPStatus: status,
			Retryable:  status == http.StatusTooManyRequests || status >= 500,
		}
	}
	var env tonEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// 2xx 却拿到非 JSON：不能当成受理成功，理由同上。
		return nil, &pay.ProviderError{
			Channel:    c.name,
			Code:       strconv.Itoa(status),
			Msg:        "响应不是合法 JSON",
			HTTPStatus: status,
			Retryable:  false,
		}
	}
	if !env.succeeded() {
		return nil, &pay.ProviderError{
			Channel:    c.name,
			Code:       env.Code,
			Msg:        env.Msg,
			HTTPStatus: status,
			Retryable:  httpx.IsRetryableCode(env.Code),
		}
	}
	if len(env.Data) == 0 {
		return json.RawMessage(body), nil
	}
	return env.Data, nil
}

// tonEnvelope 是供应商的响应包络（注入点：真实包络形态确定后改这一个结构）。
type tonEnvelope struct {
	Code    string          `json:"code"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// succeeded 有 code 就按 code 判；只有 success 布尔形态的供应商才退回布尔判定。
func (e *tonEnvelope) succeeded() bool {
	if e.Code != "" {
		return e.Code == tonCodeOK
	}
	return e.Success != nil && *e.Success
}

// toAccountInfo 归一账户载荷。载荷解析不出来时报错而不是返回全零账户：
// 全零会被下游读成「余额不足」，把一个查询故障伪装成一次业务拒绝。
func (c *tonAccountClient) toAccountInfo(data json.RawMessage, req tonAccountRequest) (*pay.AccountInfo, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("通道[%s]账户查询返回空载荷", c.name)
	}
	var d tonAccountData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("通道[%s]账户回执不可解析", c.name)
	}
	info := &pay.AccountInfo{
		MerchantID: tonFirstNonEmpty(d.MerchantID, req.MerchantID),
		Currency:   tonFirstNonEmpty(d.Currency, req.Currency),
		Balance:    d.Balance,
		Available:  d.Available,
		Frozen:     d.Frozen,
		Credit:     d.Credit,
		FeeRateBps: d.FeeRateBps,
		Status:     pay.NormalizeAccountStatus(d.Status),
	}
	if info.Status == pay.AcctUnknown {
		info.RawNote = "账户状态词未登记: " + clipTON(d.Status)
	}
	return info, nil
}

// tonFirstNonEmpty 取第一个非空值。
func tonFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
