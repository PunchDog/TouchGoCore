package pay

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// CodeOK 是包络里的成功码。三家供应商的真实成功码等文档确认，在此之前统一按 "0" 判。
const CodeOK = "0"

// 请求头默认名，配置留空时生效。
const (
	DefaultAuthHeader  = "X-Sign"
	DefaultTokenHeader = "Authorization"
)

// SignMD5 按「签名字段值直连拼接 + 密钥」取 32 位小写 MD5。
//
// 用 MD5 不是这里挑的口令哈希方案，而是供应商接口规定的形态：值直连拼接、
// 无分隔符、末尾追加密钥。字段顺序必须由调用方逐字照文档登记——顺序错一位
// 就是全量验签失败，而且本地永远算得过、服务端永远全拒，最难查。
func SignMD5(secret string, values ...string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(v)
	}
	b.WriteString(secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// RetryableCodes 登记「可以原单重发」的供应商业务错误码。
//
// 空表是刻意的：文档没到位之前，任何业务码一律按不可重试处理。把「余额不足」
// 当限流来重试只是重复挨拒；把出款失败当可重试则有双花风险。确认安全的码
// （限流、网关超时）之后在这里逐字登记，比在调用点散落 if 好收口。
var RetryableCodes = map[string]struct{}{}

// envelope 是响应包络（注入点：真实包络形态确定后改这一个结构与其 succeeded/parse）。
type envelope struct {
	Code    string          `json:"code"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// succeeded 有 code 就按 code 判；只有 success 布尔形态的供应商才退回布尔判定。
func (e *envelope) succeeded() bool {
	if e.Code != "" {
		return e.Code == CodeOK
	}
	return e.Success != nil && *e.Success
}

// ProviderOptions 是构造一个通道供应商客户端所需的全部输入。
//
// Endpoint 是个取值函数而不是 map：配置结构体留在 config 包（否则成环），
// 调用方把 (*config.PaySDKConfig).Endpoint 直接传进来即可。
type ProviderOptions struct {
	Name string
	// Driver 是驱动标记（注册表里的键名），只用于日志与错误定位，不参与判定。
	Driver string
	// MerchantID 是我方在供应商侧的商户号，进报文并参与签名域。
	// 一个供应商下的多个商户账户各自开一条通道，PayOrder.AccountID 由通道包
	// 用来挑哪条通道，不在报文里改这个号。
	MerchantID string
	BaseURL    string
	Timeout    time.Duration
	MaxRetry   int
	AppID      string
	SecretKey  string
	AuthHeader string
	// TokenHeader 是会话凭证所在请求头名。
	TokenHeader string
	NotifyURL   string
	Endpoint    func(name string) (string, bool)
	// Extras 是通道特有字段（如 USDT 的 contract、TON 的 jetton）。
	// 登记顺序就是签名域追加顺序：这些字段由配置给出、在报文里追加在通用字段之后，
	// 若签名域不跟着追加，供应商按「报文有、签名没有」会全量拒签。
	Extras []ExtraField
}

// ExtraField 是一个通道特有字段的「JSON 键名 + 取值」。
type ExtraField struct {
	Name  string
	Value string
}

// Provider 是一条资金通道的供应商客户端：签名、包络解析、登录态持有都在这。
//
// 三个通道包共用它，各自只保留「报文长什么样、签哪几个字段」这两件真正
// 因供应商而异的事。
type Provider struct {
	hc   *Client
	opt  ProviderOptions
	name string
	// token 是登录后拿到的会话凭证，可能与 SecretKey 同样敏感：只进请求头，
	// 不进日志与错误文案。
	token atomic.Pointer[string]
}

// NewProvider 构造供应商客户端。凭证缺失就地拒绝——资金接口带着空密钥出去
// 只会在供应商侧留下一堆验签失败日志，本地却看起来「启动成功」。
func NewProvider(opt ProviderOptions) (*Provider, error) {
	if strings.TrimSpace(opt.SecretKey) == "" {
		return nil, fmt.Errorf("通道[%s]未配置 secret_key", opt.Name)
	}
	if opt.Endpoint == nil {
		return nil, fmt.Errorf("通道[%s]未提供 endpoints 取值函数", opt.Name)
	}
	hc, err := NewClient(Options{
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
		authHeader = DefaultAuthHeader
	}
	tokenHeader := strings.TrimSpace(opt.TokenHeader)
	if tokenHeader == "" {
		tokenHeader = DefaultTokenHeader
	}
	// 请求头名写错（含空格或冒号）会让每一次请求都在传输层失败，
	// 报错还长在供应商侧。这种配置必须在构造期就拒掉。
	if err := checkHeaderName(opt.Name, "auth_header", authHeader); err != nil {
		return nil, err
	}
	if err := checkHeaderName(opt.Name, "token_header", tokenHeader); err != nil {
		return nil, err
	}
	opt.AuthHeader = authHeader
	opt.TokenHeader = tokenHeader
	opt.AppID = strings.TrimSpace(opt.AppID)
	opt.MerchantID = strings.TrimSpace(opt.MerchantID)
	opt.NotifyURL = strings.TrimSpace(opt.NotifyURL)
	if strings.TrimSpace(opt.Driver) == "" {
		opt.Driver = opt.Name
	}
	return &Provider{hc: hc, opt: opt, name: opt.Name}, nil
}

// checkHeaderName 校验请求头名是可用的 token 形态。
func checkHeaderName(channel, field, name string) error {
	if name == "" || strings.ContainsAny(name, " :;\t\r\n") {
		return fmt.Errorf("通道[%s]的 %s 不是合法请求头名", channel, field)
	}
	return nil
}

// BaseURL 返回归一化后的供应商基址，可用于日志（其中不含凭证）。
func (p *Provider) BaseURL() string { return p.hc.BaseURL() }

// Driver 返回驱动标记，用于日志里区分「同一通道的不同供应商实现」。
func (p *Provider) Driver() string { return p.opt.Driver }

// MerchantID 返回默认商户号；它不是凭证，可以进日志。
func (p *Provider) MerchantID() string { return p.opt.MerchantID }

// AppID 返回应用标识，供各通道包组报文（它参与签名域，但不是凭证）。
func (p *Provider) AppID() string { return p.opt.AppID }

// SetToken 写入登录会话凭证。空串等同清除。
func (p *Provider) SetToken(token string) {
	t := strings.TrimSpace(token)
	if t == "" {
		p.token.Store(nil)
		return
	}
	p.token.Store(&t)
}

// Token 返回当前会话凭证；未登录时为空串。
func (p *Provider) Token() string {
	if p == nil {
		return ""
	}
	if ptr := p.token.Load(); ptr != nil {
		return *ptr
	}
	return ""
}

// Call 按逻辑端点名发一次 POST：取路径 → 序列化 → 签名 → 解包络。
//
// signValues 必须与 payload 里参与签名的字段一一对应且顺序一致。两者写在同一段
// 代码里是刻意的——报文加了字段却忘了同步签名域，是供应商全量拒签的典型成因。
//
// 本方法不追加 Options.Extras，报文就是传进来的 payload；资金四类操作走
// Recharge/Withdraw/QueryOrder/QueryAccount，它们会按登记顺序追加特有字段。
func (p *Provider) Call(ctx context.Context, endpoint string, payload any, signValues []string) (json.RawMessage, error) {
	return p.call(ctx, endpoint, payload, signValues, false)
}

func (p *Provider) call(ctx context.Context, endpoint string, payload any, signValues []string, withExtras bool) (json.RawMessage, error) {
	path, ok := p.opt.Endpoint(endpoint)
	if !ok {
		return nil, fmt.Errorf("通道[%s]未配置 endpoints.%s，该操作不可用", p.name, endpoint)
	}
	var (
		body []byte
		err  error
	)
	if withExtras {
		body, err = marshalPayload(payload, p.opt.Extras)
		for _, e := range p.opt.Extras {
			if e.Value != "" {
				signValues = append(signValues, e.Value)
			}
		}
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		return nil, fmt.Errorf("通道[%s]序列化请求失败: %w", p.name, err)
	}
	sign := func(req *http.Request, _ []byte) error {
		req.Header.Set(p.opt.AuthHeader, SignMD5(p.opt.SecretKey, signValues...))
		if t := p.Token(); t != "" {
			req.Header.Set(p.opt.TokenHeader, t)
		}
		return nil
	}
	return p.hc.Do(ctx, CallSpec{
		Method: http.MethodPost,
		Path:   path,
		Body:   body,
		Sign:   sign,
		Parse:  p.parse,
	})
}

// marshalPayload 把通用报文与通道特有字段并成同一层 JSON。
//
// 数字用 UseNumber 展开成 map 再回序列化，是为了让 int64 金额原样落回报文：
// 默认解码会把数字变成 float64，大额出款经过这一步就会在最小单位上丢精度。
func marshalPayload(base any, extras []ExtraField) ([]byte, error) {
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	fields := map[string]any{}
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("报文展开失败: %w", err)
	}
	for _, e := range extras {
		if e.Name == "" || e.Value == "" {
			continue
		}
		fields[e.Name] = e.Value
	}
	return json.Marshal(fields)
}

// parse 把响应包络拆成 data 载荷；业务失败转成 ProviderError 并带上可重试标记。
func (p *Provider) parse(status int, hdr http.Header, body []byte) (json.RawMessage, error) {
	// 非 2xx 先按传输层判掉：网关/反代拦下来的 502 页面里偶然出现 "code":"0"
	// 并不能算供应商受理，认成功就等于把一笔没出去的单记成已出款。
	if status < 200 || status >= 300 {
		return nil, &ProviderError{
			Channel:    p.name,
			Code:       strconv.Itoa(status),
			Msg:        http.StatusText(status),
			HTTPStatus: status,
			Retryable:  status == http.StatusTooManyRequests || status >= 500,
		}
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		// 2xx 却拿到非 JSON：不能当成受理成功，理由同上。
		return nil, &ProviderError{
			Channel:    p.name,
			Code:       strconv.Itoa(status),
			Msg:        "响应不是合法 JSON",
			HTTPStatus: status,
			Retryable:  false,
		}
	}
	if !env.succeeded() {
		_, retryable := RetryableCodes[env.Code]
		return nil, &ProviderError{
			Channel:    p.name,
			Code:       env.Code,
			Msg:        env.Msg,
			HTTPStatus: status,
			Retryable:  retryable,
		}
	}
	if len(env.Data) == 0 {
		return json.RawMessage(body), nil
	}
	return env.Data, nil
}

// OrderRequest 是充值/提现下单报文。字段名等真实文档到位后在本结构与
// SignValues 两处一起改，通道包与调用方都不感知。
type OrderRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	OrderNo    string `json:"order_no"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency,omitempty"`
	Network    string `json:"network,omitempty"`
	Address    string `json:"address,omitempty"`
	Phone      string `json:"phone,omitempty"`
	NotifyURL  string `json:"notify_url,omitempty"`
}

// NewOrderRequest 从 PayOrder 组下单报文，通道特有字段由调用方再补。
func (p *Provider) NewOrderRequest(o *PayOrder) OrderRequest {
	return OrderRequest{
		AppID:      p.opt.AppID,
		MerchantID: p.opt.MerchantID,
		OrderNo:    o.OrderNo,
		Amount:     o.Amount,
		Currency:   o.Currency,
		Network:    o.Network,
		Address:    o.Address,
		Phone:      o.Phone,
		NotifyURL:  p.opt.NotifyURL,
	}
}

// SignValues 返回下单报文参与签名的字段值，顺序即签名域顺序。
func (r *OrderRequest) SignValues() []string {
	return []string{r.AppID, r.MerchantID, r.OrderNo, strconv.FormatInt(r.Amount, 10), r.Currency, r.Address, r.Phone}
}

// QueryRequest 是订单查询报文：幂等键是唯一输入。
type QueryRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	OrderNo    string `json:"order_no"`
}

// SignValues 查询报文签名域。
func (r *QueryRequest) SignValues() []string {
	return []string{r.AppID, r.MerchantID, r.OrderNo}
}

// AccountRequest 是商户账户查询报文。
type AccountRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
}

// SignValues 账户查询报文签名域。
func (r *AccountRequest) SignValues() []string {
	return []string{r.AppID, r.MerchantID, r.Currency}
}

// AccountData 是账户回执里的载荷字段。
//
// fee_rate_bps 只接受整数万分比：供应商若给 "0.3%" 这类字符串，真实文档到位后
// 在本结构与归一函数之间做一次显式换算，不在这里塞浮点字段。
type AccountData struct {
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	Balance    int64  `json:"balance"`
	Available  int64  `json:"available"`
	Frozen     int64  `json:"frozen"`
	Credit     int64  `json:"credit"`
	FeeRateBps int64  `json:"fee_rate_bps"`
	Status     string `json:"status"`
}

// OrderData 是下单/查询回执里的载荷字段。
type OrderData struct {
	OrderNo string `json:"order_no"`
	TradeNo string `json:"trade_no"`
	Status  string `json:"status"`
	Amount  int64  `json:"amount"`
}

// NormalizeStatus 把供应商状态词映射到本包的四态。
//
// 映射不到的一律算 UNKNOWN，而不是 PENDING：PENDING 会让上游以为「再等等就有」
// 而停止对账，FAILED 会诱发重下单，只有 UNKNOWN 会把它推到人工核对那条路上。
func NormalizeStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "ok", "success", "succeeded", "paid", "complete", "completed", "successed":
		return StatusSuccess
	case "-1", "fail", "failed", "error", "reject", "rejected", "cancel", "cancelled":
		return StatusFailed
	case "0", "pending", "processing", "waiting", "paying", "handling":
		return StatusPending
	default:
		return StatusUnknown
	}
}

// Result 把回执载荷归一成 PayResult。载荷读不出状态时给 UNKNOWN 而不是报错：
// 请求已经发出去了，此刻报错会让上游以为「没发过」，从而重发同一张单。
func (p *Provider) Result(data json.RawMessage, orderNo string) *PayResult {
	res := &PayResult{OrderNo: orderNo, Status: StatusUnknown}
	if len(data) == 0 {
		res.RawNote = "空响应"
		return res
	}
	var d OrderData
	if err := json.Unmarshal(data, &d); err != nil {
		res.RawNote = "回执载荷不可解析"
		return res
	}
	if d.OrderNo != "" {
		res.OrderNo = d.OrderNo
	}
	res.TradeNo = d.TradeNo
	res.Amount = d.Amount
	res.Status = NormalizeStatus(d.Status)
	if res.Status == StatusUnknown {
		res.RawNote = "状态词未登记: " + clip(d.Status, 64)
	}
	return res
}

// Recharge 充值下单：把一单充值请求送给供应商，返回归一回执。
func (p *Provider) Recharge(ctx context.Context, o *PayOrder) (*PayResult, error) {
	return p.placeOrder(ctx, EndpointRecharge, o, false)
}

// Withdraw 提现下单。比充值多一道「收款地址非空」的前置校验。
func (p *Provider) Withdraw(ctx context.Context, o *PayOrder) (*PayResult, error) {
	return p.placeOrder(ctx, EndpointWithdraw, o, true)
}

func (p *Provider) placeOrder(ctx context.Context, endpoint string, o *PayOrder, needAddress bool) (*PayResult, error) {
	if err := CheckOrder(o, needAddress); err != nil {
		return nil, err
	}
	req := p.NewOrderRequest(o)
	data, err := p.call(ctx, endpoint, req, req.SignValues(), true)
	if err != nil {
		return nil, err
	}
	return p.Result(data, o.OrderNo), nil
}

// QueryOrder 按幂等键查单。未查到不等于失败：本方法只在传输与包络都正常时给结论，
// 状态词认不出来的情况交给 Result 归一成 UNKNOWN，由上游对账而不是就地重下单。
func (p *Provider) QueryOrder(ctx context.Context, orderNo string) (*PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("订单号（幂等键）为空")
	}
	req := QueryRequest{AppID: p.opt.AppID, MerchantID: p.opt.MerchantID, OrderNo: orderNo}
	data, err := p.call(ctx, EndpointQuery, req, req.SignValues(), true)
	if err != nil {
		return nil, err
	}
	return p.Result(data, orderNo), nil
}

// QueryAccount 查我方在供应商侧的商户账户（余额/可用/授信/费率/状态）。
//
// 通道特有字段同样追加进这一单：同一 SDK 下 USDT 与 TON 的账户可能按合约区分，
// 少签一个字段就是全量拒签，宁可多带也不能少带。
func (p *Provider) QueryAccount(ctx context.Context, q *AccountQuery) (*AccountInfo, error) {
	if q == nil {
		q = &AccountQuery{}
	}
	req := AccountRequest{
		AppID:      p.opt.AppID,
		MerchantID: firstNonEmpty(strings.TrimSpace(q.MerchantID), p.opt.MerchantID),
		Currency:   strings.TrimSpace(q.Currency),
	}
	data, err := p.call(ctx, EndpointAccount, req, req.SignValues(), true)
	if err != nil {
		return nil, err
	}
	return p.account(data, req)
}

// account 归一账户载荷。载荷解析不出来时报错而不是返回全零账户：
// 全零会被下游读成「余额不足」，把一个查询故障伪装成一次业务拒绝。
func (p *Provider) account(data json.RawMessage, req AccountRequest) (*AccountInfo, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("通道[%s]账户查询返回空载荷", p.name)
	}
	var d AccountData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("通道[%s]账户回执不可解析", p.name)
	}
	info := &AccountInfo{
		MerchantID: firstNonEmpty(d.MerchantID, req.MerchantID),
		Currency:   firstNonEmpty(d.Currency, req.Currency),
		Balance:    d.Balance,
		Available:  d.Available,
		Frozen:     d.Frozen,
		Credit:     d.Credit,
		FeeRateBps: d.FeeRateBps,
		Status:     NormalizeAccountStatus(d.Status),
	}
	if info.Status == AcctUnknown {
		info.RawNote = "账户状态词未登记: " + clip(d.Status, 64)
	}
	return info, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// CheckOrder 下单前置校验：幂等键必须有，金额必须是正的最小单位整数。
//
// 提现还要求收款地址：地址为空的一单出去，钱就找不到主人了。
func CheckOrder(o *PayOrder, needAddress bool) error {
	switch {
	case o == nil:
		return errors.New("订单为 nil")
	case strings.TrimSpace(o.OrderNo) == "":
		return errors.New("订单号（幂等键）为空")
	case o.Amount <= 0:
		return errors.New("金额必须为正整数最小单位")
	case needAddress && strings.TrimSpace(o.Address) == "":
		return errors.New("收款地址为空")
	}
	return nil
}

// clip 截断长度，避免把整段响应塞进日志与摘要字段。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
