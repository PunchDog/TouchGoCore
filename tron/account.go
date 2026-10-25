package tron

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"touchgocore/internal/httpx"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件是 TRON 侧的供应商账户查询客户端。
//
// 它从 pay.Provider 精简而来，只留「查一个商户账户」这一件事：出款已经全部上链，
// 供应商侧还剩的用途只有读额度。留下来的是一个窄得多的问题——组一份报文、
// 按文档签几个字段、把回执归一成 AccountInfo。
//
// 三家通道共用过那一份实现，代价是它同时装着「报文怎么排」「端点怎么取」
// 「重试怎么算」和「响应怎么解」四层。拆开之后：传输归 internal/httpx，
// 报文归这里，而 telegram 那边按文档接受一份同构的重复——两条链的供应商
// 不一定是同一家，抽一层「通用供应商客户端」只会把两家的差异挤进同一个袋子里。
//
// 与旧实现的一处刻意保留：通道特有字段（USDT 的 contract）仍然追加进报文
// 与签名域。同一 SDK 下 USDT 与原生币的账户可能按合约区分，少签一个字段
// 就是全量拒签——宁可多带也不能少带。

const (
	// codeOK 是包络里的成功码。真实成功码等文档确认，在此之前统一按 "0" 判。
	codeOK = "0"
	// defaultAuthHeader 是签名请求头名的缺省值，配置留空时生效。
	defaultAuthHeader = "X-Sign"
)

// accountQuerier 是链上通道对「供应商侧账户」的唯一依赖。
//
// 宽度只有 QueryAccount 一个方法：链上通道不需要 Withdraw/QueryOrder，
// 而按老接口 pay.Channel 收一个供应商实现，等于允许它在某天被误当成出款路径。
// 窄接口在这里不是风格，是把「出款只能上链」这条纪律写进类型里。
type accountQuerier interface {
	QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error)
}

// accountClient 是供应商侧账户查询的实现。
type accountClient struct {
	hc   *httpx.Client
	name string
	opt  channelOptions
}

// signMD5 按「签名字段值直连拼接 + 密钥」取 32 位小写 MD5。
//
// 用 MD5 不是这里挑的口令哈希方案，而是供应商接口规定的形态：值直连拼接、
// 无分隔符、末尾追加密钥。字段顺序必须由调用方逐字照文档登记——顺序错一位
// 就是全量验签失败，而且本地永远算得过、服务端永远全拒，最难查。
func signMD5(secret string, values ...string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(v)
	}
	b.WriteString(secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// checkHeaderName 校验请求头名是可用的 token 形态。
//
// 头名写错（含空格或冒号）会让每一次请求都在传输层失败，报错还长在供应商侧。
func checkHeaderName(channel, field, name string) error {
	if name == "" || strings.ContainsAny(name, " :;\t\r\n") {
		return fmt.Errorf("通道[%s]的 %s 不是合法请求头名", channel, field)
	}
	return nil
}

// newAccountClient 构造账户查询客户端。
//
// 凭证缺失就地拒绝：资金接口带着空密钥出去只会在供应商侧留下一堆验签失败日志，
// 本地却看起来「启动成功」。
func newAccountClient(opt channelOptions) (*accountClient, error) {
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
		authHeader = defaultAuthHeader
	}
	if err := checkHeaderName(opt.Name, "auth_header", authHeader); err != nil {
		return nil, err
	}
	opt.AuthHeader = authHeader
	opt.AppID = strings.TrimSpace(opt.AppID)
	opt.MerchantID = strings.TrimSpace(opt.MerchantID)
	return &accountClient{hc: hc, name: opt.Name, opt: opt}, nil
}

// accountRequest 是商户账户查询报文。
type accountRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
}

// signValues 账户查询报文签名域（顺序即签名域顺序）。
func (r *accountRequest) signValues() []string {
	return []string{r.AppID, r.MerchantID, r.Currency}
}

// accountData 是账户回执里的载荷字段。
//
// fee_rate_bps 只接受整数万分比：供应商若给 "0.3%" 这类字符串，真实文档到位后
// 在本结构与归一函数之间做一次显式换算，不在这里塞浮点字段。
type accountData struct {
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	Balance    int64  `json:"balance"`
	Available  int64  `json:"available"`
	Frozen     int64  `json:"frozen"`
	Credit     int64  `json:"credit"`
	FeeRateBps int64  `json:"fee_rate_bps"`
	Status     string `json:"status"`
}

// QueryAccount 查我方在供应商侧的商户账户（余额/可用/授信/费率/状态）。
//
// 通道特有字段同样追加进这一单：同一 SDK 下 USDT 与原生币的账户可能按合约区分，
// 少签一个字段就是全量拒签，宁可多带也不能少带。
func (c *accountClient) QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	if q == nil {
		q = &pay.AccountQuery{}
	}
	req := accountRequest{
		AppID:      c.opt.AppID,
		MerchantID: firstNonEmpty(strings.TrimSpace(q.MerchantID), c.opt.MerchantID),
		Currency:   strings.TrimSpace(q.Currency),
	}
	data, err := c.call(ctx, pay.EndpointAccount, req, req.signValues())
	if err != nil {
		return nil, err
	}
	return c.toAccountInfo(data, req)
}

// call 按逻辑端点名发一次 POST：取路径 → 序列化（含特有字段）→ 签名 → 解包络。
//
// signValues 必须与报文里参与签名的字段一一对应且顺序一致。两者写在同一段代码里
// 是刻意的——报文加了字段却忘了同步签名域，是供应商全量拒签的典型成因。
func (c *accountClient) call(ctx context.Context, endpoint string, payload any, signValues []string) (json.RawMessage, error) {
	fields, extraSigns, err := mergeExtraFields(c.opt.Extras)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]特有字段不可用: %w", c.name, err)
	}
	body, err := marshalPayload(payload, fields)
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
			req.Header.Set(header, signMD5(secret, append(signValues, extraSigns...)...))
			return nil
		},
		Parse: c.parse,
	})
}

// parse 把响应包络拆成 data 载荷；业务失败转成 ProviderError 并带上可重试标记。
//
// 账户查询是幂等读，走宽容口径（429 与 5xx 默认可重试）。本仓已无 HTTP 写路径，
// 严格口径（写侧原单重发要靠白名单背书）留在 internal/httpx 的 StrictParse 里。
func (c *accountClient) parse(status int, _ http.Header, body []byte) (json.RawMessage, error) {
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
	return parseEnvelopeData(c.name, status, body)
}

// parseEnvelopeData 把 2xx 的响应体解成 data 载荷。
//
// 与状态码判定分开，是因为「非 2xx」那一段已经按传输层判完了，再走一遍会把
// 网关页里的假包络当成业务回执。这里只处理「状态码说成了，包络说没成」这一层。
func parseEnvelopeData(channel string, status int, body []byte) (json.RawMessage, error) {
	var env supplierEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// 2xx 却拿到非 JSON：不能当成受理成功，理由同上。
		return nil, &pay.ProviderError{
			Channel:    channel,
			Code:       strconv.Itoa(status),
			Msg:        "响应不是合法 JSON",
			HTTPStatus: status,
			Retryable:  false,
		}
	}
	if !env.succeeded() {
		return nil, &pay.ProviderError{
			Channel:    channel,
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

// supplierEnvelope 是供应商的响应包络（注入点：真实包络形态确定后改这一个结构）。
type supplierEnvelope struct {
	Code    string          `json:"code"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// succeeded 有 code 就按 code 判；只有 success 布尔形态的供应商才退回布尔判定。
func (e *supplierEnvelope) succeeded() bool {
	if e.Code != "" {
		return e.Code == codeOK
	}
	return e.Success != nil && *e.Success
}

// toAccountInfo 归一账户载荷。载荷解析不出来时报错而不是返回全零账户：
// 全零会被下游读成「余额不足」，把一个查询故障伪装成一次业务拒绝。
func (c *accountClient) toAccountInfo(data json.RawMessage, req accountRequest) (*pay.AccountInfo, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("通道[%s]账户查询返回空载荷", c.name)
	}
	var d accountData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("通道[%s]账户回执不可解析", c.name)
	}
	info := &pay.AccountInfo{
		MerchantID: firstNonEmpty(d.MerchantID, req.MerchantID),
		Currency:   firstNonEmpty(d.Currency, req.Currency),
		Balance:    d.Balance,
		Available:  d.Available,
		Frozen:     d.Frozen,
		Credit:     d.Credit,
		FeeRateBps: d.FeeRateBps,
		Status:     pay.NormalizeAccountStatus(d.Status),
	}
	if info.Status == pay.AcctUnknown {
		info.RawNote = "账户状态词未登记: " + util.Clip(d.Status)
	}
	return info, nil
}

// mergeExtraFields 把通道特有字段并成「要进报文的键值」，
// 同时按同一次遍历的顺序产出「要追加进签名域的值」。空名或空值两侧一起跳过。
//
// 键不许撞上已签名量（见 accountPayloadKeys）：允许撞名的话 Extra{"merchant_id":"X"}
// 就能覆盖报文里的商户号，而签名域里签的还是原来那个值，验签看着完全正确。
// 配置写错一个键名的表现与恶意覆盖一模一样，必须在构造出的第一次调用就报出来。
//
// 配置侧内部重名一律拒绝：fields 是 map 只会留一个 JSON 值，signs 却会 append 两次,
// 报文一值、签名域两值，验签失败或异常受理，比本地就地报错更难查。
func mergeExtraFields(extras []extraField) (map[string]string, []string, error) {
	fields := make(map[string]string, len(extras))
	signs := make([]string, 0, len(extras))
	for _, e := range extras {
		if e.Name == "" || e.Value == "" {
			continue
		}
		if _, reserved := accountPayloadKeys[e.Name]; reserved {
			return nil, nil, fmt.Errorf("通道特有字段 %s 与报文已签名量同名，会覆盖签名域", e.Name)
		}
		if _, dup := fields[e.Name]; dup {
			return nil, nil, fmt.Errorf("通道特有字段 %s 重复登记，报文一值却会双签", e.Name)
		}
		fields[e.Name] = e.Value
		signs = append(signs, e.Value)
	}
	return fields, signs, nil
}

// accountPayloadKeys 是账户查询报文各字段的 JSON 键名集合，用反射取，
// 免得报文加一个字段就要人手补一份保留字名单（补漏的那次就是护栏失效的那次）。
var accountPayloadKeys = func() map[string]struct{} {
	names := map[string]struct{}{}
	t := reflect.TypeOf(accountRequest{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name := strings.TrimSpace(strings.Split(tag, ",")[0])
		if name != "" && name != "-" {
			names[name] = struct{}{}
		}
	}
	return names
}()

// marshalPayload 把通用报文与特有字段并成同一层 JSON。
//
// 数字用 UseNumber 展开成 map 再回序列化，是为了让 int64 金额原样落回报文：
// 默认解码会把数字变成 float64，大额出款经过这一步就会在最小单位上丢精度。
func marshalPayload(base any, extras map[string]string) ([]byte, error) {
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
	for k, v := range extras {
		fields[k] = v
	}
	return json.Marshal(fields)
}

// firstNonEmpty 取第一个非空值。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
