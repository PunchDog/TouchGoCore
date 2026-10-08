package nft

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// CodeOK 是包络里的成功码。真实供应商成功码等文档确认，在此之前统一按 "0" 判。
const CodeOK = "0"

// CodeBadBody 是「2xx 但回执读不出」的专用错误码（NF-F5②）。
//
// 与 "transport"/"read_body" 同属「受理存疑」集合：请求已经写出、结论没能读回。
// 用独立哨兵而不是复用状态码 "200"，是为了让这条分支不可能被读成「成功码里的可重试项」，
// 也便于白名单入口硬拒（见 SetRetryableCodes）。
const CodeBadBody = "bad_body"

// 请求头默认名，配置留空时生效。
const (
	DefaultAuthHeader  = "X-Sign"
	DefaultTokenHeader = "Authorization"
)

// warnf 是本包唯一的告警出口，由 nft.go 的 init 注入 vars.Error
// （§1 文件划分表：provider.go 纯 stdlib，不得 import touchgocore/*）。默认 nil = 静默。
var warnf func(format string, args ...any)

// warnLog nil-safe 转调告警出口。这两处告警（重试白名单硬拒、幂等账本软阈值）是「运维要
// 看得见」的告警而不是错误返回值：塞进 error 会改掉调用方的控制流，而直接 import vars
// 破分层，故走依赖倒置的注入钩子。TestQaWarningHookWired 钉住注入未丢，接线一断即红。
func warnLog(format string, args ...any) {
	if warnf != nil {
		warnf(format, args...)
	}
}

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

// retryableCodes 登记「可以原单重发」的供应商业务错误码。
//
// 空表是刻意的：文档没到位之前，任何业务码一律按不可重试处理（7eb7b89 口径的 nft 版）。
// ADR-6：不复刻 pay.RetryableCodes 的裸导出 Deprecated 面——nft 无历史包袱，从第一天
// 只暴露下面三个带锁入口，race 面根本不建立。
var retryableCodes = map[string]struct{}{}

// retryableCodesMu 护住 retryableCodes 的包内读写：解析响应（读）可能发生在任何一次
// 铸造/查询的 goroutine 里，登记（写）可能发生在运行期配置热更时，无锁并发读写 map
// 在 Go runtime 是 fatal，不是「偶尔读到旧值」。
var retryableCodesMu sync.RWMutex

// doubtCodes 是「无法证明未被受理」的错误码，白名单入口硬拒（7eb7b89 教训的机械化）。
//
// 这一组不能靠注释里的运维警示守：把它们登记进来等于宣布「这个码下原单重发不会二次铸造」，
// 而 transport/read_body/bad_body/502/504 的真实语义恰恰是「受理状态未知」。
// 一次误登记就把严格路径的两层闸全部抹平，且事故形态是资产双付，事后无法回滚。
var doubtCodes = map[string]struct{}{
	"transport": {}, "read_body": {}, CodeBadBody: {}, "502": {}, "504": {},
}

// SetRetryableCodes 登记「可以原单重发」的错误码（带锁写入，可重复调用）。空串与首尾空白
// 归一后跳过；受理存疑类码（doubtCodes）硬拒。
//
// 警示（§5.5，7eb7b89 教训逐字沿用 pay/provider.go:62-68）：Mint/Transfer（strict 路径）
// 白名单严禁纳入 502/504 这类网关状态码——502=请求可能已被供应商受理、只是回执没穿回来，
// 受理状态未知；把「未知」当「失败」原单重发就是二次铸造/双花转账。
// 同理，任何「不能证明请求未被受理」的码都不得登记。
func SetRetryableCodes(codes ...string) {
	retryableCodesMu.Lock()
	defer retryableCodesMu.Unlock()
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, doubt := doubtCodes[c]; doubt {
			warnLog("nft: 拒绝把受理存疑码 %q 登记进重试白名单（原单重发=二次铸造窗口）", c)
			continue
		}
		retryableCodes[c] = struct{}{}
	}
}

// DeleteRetryableCode 撤销一个已登记的错误码（带锁写入）。
func DeleteRetryableCode(code string) {
	retryableCodesMu.Lock()
	defer retryableCodesMu.Unlock()
	delete(retryableCodes, code)
}

// IsRetryableCode 查询错误码是否在「可原单重发」白名单里（带锁读取）。
// 包内所有判定必须走本函数，不得直读 map。
func IsRetryableCode(code string) bool {
	retryableCodesMu.RLock()
	defer retryableCodesMu.RUnlock()
	_, ok := retryableCodes[code]
	return ok
}

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

// ExtraField 是一个通道特有字段的「JSON 键名 + 取值」。
type ExtraField struct {
	Name  string
	Value string
}

// ProviderOptions 是构造一个通道供应商客户端所需的全部输入（对 pay.ProviderOptions 同构）。
//
// Endpoint 是个取值函数而不是 map：配置结构体留在 config 包（否则成环），
// 装配层把 (*config.NftSDKConfig).Endpoint 直接传进来即可（ADR-1）。
type ProviderOptions struct {
	Name string
	// Driver 是驱动标记（注册表里的键名），只用于日志与错误定位，不参与判定。
	Driver string
	// MerchantID 是我方在供应商侧的签发主体号，进报文并参与签名域。
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
	// Extras 是通道特有字段；登记顺序就是签名域追加顺序：这些字段由配置给出、在报文里
	// 追加在通用字段之后，若签名域不跟着追加，供应商按「报文有、签名没有」会全量拒签。
	// 同名字段不得重复登记，不得与单内 Extra 撞名，也不得与报文已签名量（payloadKeyNames）
	// 撞名，否则配置就能改写件数等字段而签名域看不见。
	Extras []ExtraField
}

// Provider 是一条 NFT 通道的供应商客户端：签名、包络解析、登录态持有、幂等本地闸都在这。
type Provider struct {
	hc   *Client
	opt  ProviderOptions
	name string
	// token 是会话凭证，可能与 SecretKey 同样敏感：只进请求头，不进日志与错误文案。
	token atomic.Pointer[string]
}

// orderLedger 是幂等键的进程内指纹闸（§3.4，ADR-7：pay 无同位物）。
//
// key=orderNo, value=fingerprint。表结构用包级 sync.Map，不设上限不淘汰——淘汰等于
// 给「同号换内容」开门缝。边界：内存表、进程重启即清空；跨重启的「一单一动作」保证
// 依赖供应商侧幂等 + 下游落库，本包只提供进程内的第一道闸（对齐 pay 把落库划给下游的红线）。
var orderLedger sync.Map

// NewProvider 构造供应商客户端。凭证缺失就地拒绝——资产接口带着空密钥出去
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
	// 两个凭证头不能是同一个头名（NF-F12）：HTTP 头名大小写不敏感，同名时后一次
	// Header.Set 覆盖前一次，签名与会话凭证必丢其一——每一单都死在供应商验签上，
	// 而报错长在对方系统里，定位成本极高（与上面「头名写错要在构造期拒掉」同口径）。
	if strings.EqualFold(authHeader, tokenHeader) {
		return nil, fmt.Errorf("通道[%s]的 auth_header 与 token_header 不得是同一个请求头（大小写不敏感）：%s 头会互相覆盖，签名与凭证必丢其一", opt.Name, authHeader)
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

// seconds 把配置里的秒数换算成超时时长；<=0 交给 NewClient 用 DefaultTimeout，不在这里另立默认。
// 装配层（wire.go）调用它，好让 wire.go 自身不必 import time（§1.5 的 import 面）。
func seconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// BaseURL 返回归一化后的供应商基址，可用于日志（其中不含凭证）。
func (p *Provider) BaseURL() string { return p.hc.BaseURL() }

// Driver 返回驱动标记，用于日志里区分「同一通道的不同供应商实现」。
func (p *Provider) Driver() string { return p.opt.Driver }

// MerchantID 返回默认签发主体号；它不是凭证，可以进日志。
func (p *Provider) MerchantID() string { return p.opt.MerchantID }

// AppID 返回应用标识，供组报文（它参与签名域，但不是凭证）。
func (p *Provider) AppID() string { return p.opt.AppID }

// SetToken 写入会话凭证。空串等同清除。
func (p *Provider) SetToken(token string) {
	t := strings.TrimSpace(token)
	if t == "" {
		p.token.Store(nil)
		return
	}
	p.token.Store(&t)
}

// Token 返回当前会话凭证；未设置时为空串。
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
// signValues 必须与 payload 里参与签名的字段一一对应且顺序一致。本方法不追加任何
// 特有字段，报文就是传进来的 payload；资产类操作走 Mint/Transfer，只读走 callWithExtras。
//
// 资产端点在这里直接拒绝（NF-F6）：Call 是导出面，任何第三方驱动都能拿
// EndpointMint/EndpointTransfer 这两个导出常量进来，若它恒走宽松重试（strictRetry=false），
// 「严格口径只能经 Mint/Transfer 置位」的设计就被从旁边绕过去了——transport/5xx 自动重发
// 在资产动作上是二次铸造/双花转账（7eb7b89 提现双付的同型缺口），不是抖动。
// 幂等闸也救不了：它只防同号异文，同文重发按设计放行。
func (p *Provider) Call(ctx context.Context, endpoint string, payload any, signValues []string) (json.RawMessage, error) {
	if endpoint == EndpointMint || endpoint == EndpointTransfer {
		return nil, fmt.Errorf("通道[%s]资产动作（%s）必须经 Mint/Transfer，不得走 Call：那条路径不带幂等闸与严格重试口径", p.name, endpoint)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]序列化请求失败: %w", p.name, err)
	}
	return p.send(ctx, endpoint, body, signValues, false)
}

// buildRequest 把通用报文与特有字段定稿成「待发送的报文 + 追加后的签名域」。
//
// 这一步的失败全部发生在「一个字节都还没写出去」之前：特有字段撞名、序列化不出来。
// placeAsset 靠这条边界把「未受理」与「受理存疑」分开——后者的错误必须带 UNKNOWN 回执
// （NF-F5③），把两者混在一个函数里返回就无法判定。
func (p *Provider) buildRequest(payload any, signValues []string, orderExtra map[string]string) ([]byte, []string, error) {
	fields, extraSigns, err := mergeExtras(p.opt.Extras, orderExtra)
	if err != nil {
		return nil, nil, fmt.Errorf("通道[%s]特有字段不可用: %w", p.name, err)
	}
	body, err := marshalPayload(payload, fields)
	if err != nil {
		return nil, nil, fmt.Errorf("通道[%s]序列化请求失败: %w", p.name, err)
	}
	return body, append(signValues, extraSigns...), nil
}

// callWithExtras 在通用报文之后追加特有字段：先是 Options.Extras（配置给的、按登记顺序），
// 再是这一单的 NftOrder.Extra（按 key 字典序，取值不依赖 map 遍历顺序）。
//
// 追加值与并入报文的键值由 mergeExtras 同一次遍历产出：分成两次遍历迟早会漂移，而漂移的
// 表现是「字段发出去了却没签进签名域」——被供应商整批拒签。
func (p *Provider) callWithExtras(ctx context.Context, endpoint string, payload any, signValues []string, orderExtra map[string]string, strictRetry bool) (json.RawMessage, error) {
	body, values, err := p.buildRequest(payload, signValues, orderExtra)
	if err != nil {
		return nil, err
	}
	return p.send(ctx, endpoint, body, values, strictRetry)
}

// send 解析端点路径后交给 dispatch。
func (p *Provider) send(ctx context.Context, endpoint string, body []byte, signValues []string, strictRetry bool) (json.RawMessage, error) {
	path, ok := p.opt.Endpoint(endpoint)
	if !ok {
		return nil, fmt.Errorf("通道[%s]未配置 endpoints.%s，该操作不可用", p.name, endpoint)
	}
	// 自锁（NF-F6 的第二层）：即便调用方把 strictRetry 传成 false（或将来新增内部调用忘记传），
	// 资产端点也只能按严格口径发。判定权收在本函数，不依赖调用点自觉。
	if endpoint == EndpointMint || endpoint == EndpointTransfer {
		strictRetry = true
	}
	return p.dispatch(ctx, path, body, signValues, strictRetry)
}

// dispatch 挂签名头并把已定稿的报文交给 HTTP 客户端。
//
// 从本函数返回的 error 一律是「报文可能已经写出」那一侧的：解析层分类（parse/parseStrict）
// 与传输层（transport/read_body）。
// strictRetry 为 true 时（Mint/Transfer 路径）两处同时收紧：响应解析走 parseStrict 白名单，
// 且 CallSpec.noTransportRetry 置位——transport/read_body 错误一律不可自动重发。
func (p *Provider) dispatch(ctx context.Context, path string, body []byte, signValues []string, strictRetry bool) (json.RawMessage, error) {
	sign := func(req *http.Request, _ []byte) error {
		req.Header.Set(p.opt.AuthHeader, SignMD5(p.opt.SecretKey, signValues...))
		if t := p.Token(); t != "" {
			req.Header.Set(p.opt.TokenHeader, t)
		}
		return nil
	}
	parseFn := p.parse
	if strictRetry {
		parseFn = p.parseStrict
	}
	return p.hc.Do(ctx, CallSpec{
		Method: http.MethodPost,
		Path:   path,
		Body:   body,
		Sign:   sign,
		Parse:  parseFn,
		// 严格口径必须覆盖传输层：只收紧 parseStrict 的话，白名单空表挡得住 5xx，
		// 却挡不住「铸造已发出、响应超时/断连」后的自动重发——那是二次铸造，不是抖动。
		noTransportRetry: strictRetry,
	})
}

// mergeExtras 把通道特有字段与单内特有字段并成「要进报文的键值」，同时按同一次遍历的
// 顺序产出「要追加进签名域的值」。空名或空值两侧一起跳过。
//
// 两侧的键都不许撞上已签名量（见 payloadKeyNames）。Options.Extras 内部重名、以及单内
// Extra 与配置 Extras 撞名，一律拒绝（报文一值却会双签）。
func mergeExtras(extras []ExtraField, orderExtra map[string]string) (map[string]string, []string, error) {
	fields := make(map[string]string, len(extras)+len(orderExtra))
	signs := make([]string, 0, len(extras)+len(orderExtra))
	for _, e := range extras {
		if e.Name == "" || e.Value == "" {
			continue
		}
		if _, reserved := payloadKeyNames[e.Name]; reserved {
			return nil, nil, fmt.Errorf("通道特有字段 %s 与报文已签名量同名，会覆盖签名域", e.Name)
		}
		if _, dup := fields[e.Name]; dup {
			return nil, nil, fmt.Errorf("通道特有字段 %s 重复登记，报文一值却会双签", e.Name)
		}
		fields[e.Name] = e.Value
		signs = append(signs, e.Value)
	}
	if len(orderExtra) == 0 {
		return fields, signs, nil
	}
	keys := make([]string, 0, len(orderExtra))
	for k := range orderExtra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := orderExtra[k]
		if k == "" || v == "" {
			continue
		}
		if _, reserved := payloadKeyNames[k]; reserved {
			return nil, nil, fmt.Errorf("特有字段键 %s 与报文已签名量同名，会覆盖签名域", k)
		}
		if _, dup := fields[k]; dup {
			return nil, nil, fmt.Errorf("单内特有字段 %s 与通道配置 Extras 同名，报文一值却会双签", k)
		}
		fields[k] = v
		signs = append(signs, v)
	}
	return fields, signs, nil
}

// payloadKeyNames 是五张报文（铸造/转账/查单/持有/单币）标量字段的 JSON 键名并集，用反射取。
//
// 必须是并集而不是只取两张资产单（NF-F3）：Extras 撞名检查发生在 mergeExtras，
// 而 mergeExtras 是四条链路共用的——只读报文同样经它展开。若保留字只登记资产单字段，
// 配置就能用特有字段覆盖 address/page_no/page_size 而签名域看不见：
// 报文里发出去的地址与件数已不是 SignValues 里那几个，供应商按报文验签会拒，
// 但一旦对方按「多余字段不参与签名」实现，就是拿未签名的坐标查别人的持有。
var payloadKeyNames = func() map[string]struct{} {
	names := map[string]struct{}{}
	for _, t := range []reflect.Type{
		reflect.TypeOf(MintRequest{}),
		reflect.TypeOf(TransferRequest{}),
		reflect.TypeOf(QueryRequest{}),
		reflect.TypeOf(HoldingsRequest{}),
		reflect.TypeOf(TokenRequest{}),
	} {
		for i := 0; i < t.NumField(); i++ {
			tag := t.Field(i).Tag.Get("json")
			name := strings.TrimSpace(strings.Split(tag, ",")[0])
			if name != "" && name != "-" {
				names[name] = struct{}{}
			}
		}
	}
	return names
}()

// marshalPayload 把通用报文与特有字段并成同一层 JSON。
//
// 数字用 UseNumber 展开成 map 再回序列化，是为了让 int64 件数原样落回报文：
// 默认解码会把数字变成 float64，大额件数经过这一步就会丢精度（ADR-8）。
func marshalPayload(base any, fieldsExtra map[string]string) ([]byte, error) {
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
	for k, v := range fieldsExtra {
		fields[k] = v
	}
	return json.Marshal(fields)
}

// parse 把响应包络拆成 data 载荷；业务失败转成 ProviderError 并带上可重试标记。
// 只读三动作（Holdings/Token/Query）用本方法：429 与 5xx 默认可重试。
func (p *Provider) parse(status int, hdr http.Header, body []byte) (json.RawMessage, error) {
	return p.parseResponse(status, hdr, body, false)
}

// parseStrict 是 Mint/Transfer 路径的解析器：HTTP 状态码仅按 retryableCodes 白名单判定
// 可重试，5xx 不再默认 Retryable。资产动作重试依赖供应商幂等，风险高——白名单空表时任何
// 「响应层」错误都不可自动重发；「传输层」错误由 CallSpec.noTransportRetry 在 attempt 层
// 同样钉死为不可重发，两层合起来才是完整口径。
func (p *Provider) parseStrict(status int, hdr http.Header, body []byte) (json.RawMessage, error) {
	return p.parseResponse(status, hdr, body, true)
}

func (p *Provider) parseResponse(status int, hdr http.Header, body []byte, strictRetry bool) (json.RawMessage, error) {
	// 非 2xx 先按传输层判掉：网关/反代拦下来的 502 页面里偶然出现 "code":"0"
	// 并不能算供应商受理，认成功就等于把一笔没出去的单记成已铸成。
	if status < 200 || status >= 300 {
		retryable := status == http.StatusTooManyRequests || status >= 500
		if strictRetry {
			retryable = IsRetryableCode(strconv.Itoa(status))
		}
		return nil, &ProviderError{
			Channel:    p.name,
			Code:       strconv.Itoa(status),
			Msg:        http.StatusText(status),
			HTTPStatus: status,
			Retryable:  retryable,
		}
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		// 2xx 却拿到非 JSON：不能当成受理成功，理由同上。
		// Code 用专用哨兵而不是状态码（NF-F5②）：状态码形态的 Code 会被
		// 「200 可重试」这类误读吃掉，而这一分支的真实语义是「已发出、回执读不出」，
		// 必须落在 InSubmitDoubt 集合里，逼上游走对账而不是另起新单。
		return nil, &ProviderError{
			Channel:    p.name,
			Code:       CodeBadBody,
			Msg:        "响应不是合法 JSON",
			HTTPStatus: status,
			Retryable:  false,
		}
	}
	if !env.succeeded() {
		retryable := IsRetryableCode(env.Code)
		return nil, &ProviderError{
			Channel:    p.name,
			Code:       env.Code,
			Msg:        sanitizeEcho(env.Msg),
			HTTPStatus: status,
			Retryable:  retryable,
		}
	}
	if len(env.Data) == 0 {
		return json.RawMessage(body), nil
	}
	return env.Data, nil
}

// MintRequest 是铸造提交报文。字段名等真实文档到位后在本结构与 SignValues 两处一起改。
type MintRequest struct {
	AppID       string `json:"app_id,omitempty"`
	MerchantID  string `json:"merchant_id,omitempty"`
	OrderNo     string `json:"order_no"`
	Chain       string `json:"chain"`
	Network     string `json:"network"`
	Contract    string `json:"contract,omitempty"`
	Standard    string `json:"standard,omitempty"`
	ToAddress   string `json:"to_address"`
	Quantity    int64  `json:"quantity"`
	MetadataURI string `json:"metadata_uri"`
	NotifyURL   string `json:"notify_url,omitempty"`
	// Extra 是本单特有字段，从 NftOrder.Extra 原样带过来。打成 json:"-"：真正进报文的
	// 路径是 marshalPayload 摊平那一步，不在这里嵌一层对象。
	Extra map[string]string `json:"-"`
}

// SignValues 铸造报文签名域，顺序即文档顺序，11 位定长（空值占位撑边界，抄 pay 论证）。
func (r *MintRequest) SignValues() []string {
	return []string{
		r.AppID,
		r.MerchantID,
		r.OrderNo,
		r.Chain,
		r.Network,
		r.Contract,
		r.Standard,
		r.ToAddress,
		strconv.FormatInt(r.Quantity, 10),
		r.MetadataURI,
		r.NotifyURL,
	}
}

// TransferRequest 是转账提交报文。
type TransferRequest struct {
	AppID      string            `json:"app_id,omitempty"`
	MerchantID string            `json:"merchant_id,omitempty"`
	OrderNo    string            `json:"order_no"`
	Chain      string            `json:"chain"`
	Network    string            `json:"network"`
	Contract   string            `json:"contract"`
	TokenID    string            `json:"token_id"`
	FromAddr   string            `json:"from_address,omitempty"`
	ToAddress  string            `json:"to_address"`
	Quantity   int64             `json:"quantity"`
	Memo       string            `json:"memo,omitempty"`
	NotifyURL  string            `json:"notify_url,omitempty"`
	Extra      map[string]string `json:"-"`
}

// SignValues 转账报文签名域，12 位定长。
func (r *TransferRequest) SignValues() []string {
	return []string{
		r.AppID,
		r.MerchantID,
		r.OrderNo,
		r.Chain,
		r.Network,
		r.Contract,
		r.TokenID,
		r.FromAddr,
		r.ToAddress,
		strconv.FormatInt(r.Quantity, 10),
		r.Memo,
		r.NotifyURL,
	}
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

// HoldingsRequest 是按地址查持有的报文。
type HoldingsRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Address    string `json:"address"`
	Chain      string `json:"chain,omitempty"`
	Network    string `json:"network,omitempty"`
	Contract   string `json:"contract,omitempty"`
	PageNo     int64  `json:"page_no"`
	PageSize   int64  `json:"page_size"`
}

// SignValues 持有查询报文签名域。
func (r *HoldingsRequest) SignValues() []string {
	return []string{
		r.AppID,
		r.MerchantID,
		r.Address,
		r.Chain,
		r.Network,
		r.Contract,
		strconv.FormatInt(r.PageNo, 10),
		strconv.FormatInt(r.PageSize, 10),
	}
}

// TokenRequest 是按四元组定位单币的报文。
type TokenRequest struct {
	AppID      string `json:"app_id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Chain      string `json:"chain"`
	Network    string `json:"network"`
	Contract   string `json:"contract"`
	TokenID    string `json:"token_id"`
}

// SignValues 单币详情报文签名域。
func (r *TokenRequest) SignValues() []string {
	return []string{r.AppID, r.MerchantID, r.Chain, r.Network, r.Contract, r.TokenID}
}

// OrderData 是铸造/转账/查单回执里的载荷字段。
type OrderData struct {
	OrderNo       string `json:"order_no"`
	TradeNo       string `json:"trade_no"`
	Status        string `json:"status"`
	TokenID       string `json:"token_id"`
	TxHash        string `json:"tx_hash"`
	Quantity      int64  `json:"quantity"`
	GasFee        int64  `json:"gas_fee"`
	GasFeeToken   string `json:"gas_fee_token"`
	Confirmations *int64 `json:"confirmations"`
}

// HoldingsData 是持有回执里的载荷字段。
type HoldingsData struct {
	Address string        `json:"address"`
	Total   int64         `json:"total"`
	Items   []HoldingItem `json:"items"`
}

// TokenData 是单币详情回执里的载荷字段（TokenInfo 去 RawNote 的线上形态）。
type TokenData struct {
	Chain       string `json:"chain"`
	Network     string `json:"network"`
	Contract    string `json:"contract"`
	TokenID     string `json:"token_id"`
	Standard    string `json:"standard"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ImageURI    string `json:"image_uri"`
	MetadataURI string `json:"metadata_uri"`
	Owner       string `json:"owner"`
	TotalSupply string `json:"total_supply"`
}

// NormalizeStatus 把供应商状态词映射到本包的四态。
//
// 映射不到的一律算 UNKNOWN，而不是 PENDING：PENDING 会让上游以为「再等等就有」而停止
// 对账，FAILED 会诱发重下单，只有 UNKNOWN 会把它推到核对路径上（词表对 pay/provider.go:585-596
// 同构复刻，理由逐字沿用 pay 注释）。
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

// Result 把回执载荷归一成 NftResult。载荷读不出状态时给 UNKNOWN 而不是报错：
// 请求已经发出去了，此刻报错会让上游以为「没发过」，从而重发同一张单
// （pay/provider.go:598-628 论证同构复刻）。
func (p *Provider) Result(data json.RawMessage, orderNo string) *NftResult {
	res := &NftResult{OrderNo: orderNo, Status: StatusUnknown}
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
		// 回显单号是对方给的字符串，却会进本包的对账错误文案与门面日志（NF-F11）：
		// 控制符不滤就等于让供应商往日志里插行。
		res.OrderNo = clip(sanitizeEcho(d.OrderNo), maxOrderNoLen)
	}
	res.TradeNo = d.TradeNo
	res.TokenID = d.TokenID
	res.TxHash = d.TxHash
	res.Quantity = d.Quantity
	res.GasFee = d.GasFee
	res.GasFeeToken = d.GasFeeToken
	res.Confirmations = d.Confirmations
	res.Status = NormalizeStatus(d.Status)
	if res.Status == StatusUnknown {
		res.RawNote = "状态词未登记: " + clip(sanitizeEcho(d.Status), 64)
	}
	return res
}

// fingerprintOf 算幂等键的内容指纹：参与资产动作的字段 TrimSpace 后用 \x00 连接。
// Extra 不进指纹——它并进报文同层且参与签名，漂移由签名域暴露，不需要二次机制（§3.4）。
// Memo 必须进（NF-F10）：它同样进报文、同样占签名域的一位，却没有任何「漂移由签名域暴露」
// 的外部防线能替本地闸拦住「同号换备注重发」——供应商在同一幂等键上会收到
// 两个不同签名的报文，回执语义与对账口径当场含糊。
func fingerprintOf(o *NftOrder) string {
	parts := []string{
		strings.TrimSpace(o.Chain),
		strings.TrimSpace(o.Network),
		strings.TrimSpace(o.Contract),
		strings.TrimSpace(o.Standard),
		strings.TrimSpace(o.TokenID),
		strings.TrimSpace(o.FromAddress),
		strings.TrimSpace(o.ToAddress),
		strconv.FormatInt(o.Quantity, 10),
		strings.TrimSpace(o.MetadataURI),
		strings.TrimSpace(o.Memo),
	}
	return strings.Join(parts, "\x00")
}

// guardLoadGap 是给测试留的接缝（默认 nil，零成本）：在「账本登记成功」之后停一下，
// 让同号异文并发这类缺陷能被确定性复现——不撑开调度窗口就只能碰运气，
// 那条用例今天绿、明天也绿，探不到竞态。生产路径永不赋值。
var guardLoadGap func(orderNo string)

// orderLedgerWarnAt 是幂等账本的软告警阈值（NF-F9）：账本按设计不淘汰——
// 淘汰等于给「同号换内容」开门缝，那是资损级换取内存级。所以超阈值只报警、不清理，
// 由运维决定是否需要重启或改单号策略，而不是让内存无声增长。
const orderLedgerWarnAt = 100_000

var (
	orderLedgerLen    atomic.Int64
	orderLedgerWarned atomic.Bool
)

// warnOrderLedgerGrowth 在账本首次越过软阈值时报警一次（一次性闸门，不刷屏）。
// 单独成函数是为了让这条分支可被直接驱动验证：靠真灌 10 万条单号来触发一次告警，
// 既污染包级账本又慢，那条分支就只能当死代码留着。
func warnOrderLedgerGrowth(n int64) {
	if n >= orderLedgerWarnAt && orderLedgerWarned.CompareAndSwap(false, true) {
		warnLog("NFT 幂等账本已达 %d 条（软阈值 %d）：账本按设计不淘汰，请核对单号是否被随机灌入", n, orderLedgerWarnAt)
	}
}

// guardIdempotency 是发送前的幂等键本地闸（§3.4）：
//   - 未见过的 OrderNo ⇒ 登记并放行。
//   - 同 OrderNo 同指纹 ⇒ 放行（合法的透明重发）。
//   - 同 OrderNo 异指纹 ⇒ 就地拒绝、不发请求。
//
// 读与写必须是同一次原子操作（NF-F2）：先 Load 判空再 Store 登记之间存在窗口，
// 两个并发提交者都能「看到没有」，于是同号异文双双放行——本地闸形同虚设，
// 供应商侧收到两张同一幂等键、不同内容的铸造单，就是二次铸造。
// LoadOrStore 把「查」与「占」并进一步，后到者必然读到先登记者的指纹。
func guardIdempotency(orderNo, fp string) error {
	old, loaded := orderLedger.LoadOrStore(orderNo, fp)
	if !loaded {
		warnOrderLedgerGrowth(orderLedgerLen.Add(1))
		if guardLoadGap != nil {
			guardLoadGap(orderNo)
		}
		return nil
	}
	if old.(string) != fp {
		return errors.New("nft: 幂等键 " + orderNo + " 已用于不同内容的提交，不得变更")
	}
	return nil
}

// Mint 铸造提交：走严格重试路径 + 幂等本地闸。
func (p *Provider) Mint(ctx context.Context, o *NftOrder) (*NftResult, error) {
	return p.placeAsset(ctx, EndpointMint, o, true)
}

// Transfer 转账提交：与 Mint 共用 placeAsset，差异在校验规则。
func (p *Provider) Transfer(ctx context.Context, o *NftOrder) (*NftResult, error) {
	return p.placeAsset(ctx, EndpointTransfer, o, false)
}

// placeAsset 是铸造/转账共用的收尾流程：前置校验 → 幂等本地闸 → 组报文 → 外呼（严格路径）
// → 归一回执 → 回执对账（§3.3 三条款）。两种动作同闸。
func (p *Provider) placeAsset(ctx context.Context, endpoint string, o *NftOrder, isMint bool) (*NftResult, error) {
	var checkErr error
	if isMint {
		checkErr = CheckMint(o)
	} else {
		checkErr = CheckTransfer(o)
	}
	if checkErr != nil {
		return nil, checkErr
	}
	orderNo := strings.TrimSpace(o.OrderNo)
	// 单号形态闸排在登记之前：它是用户可控输入，控制符一旦进账本 key 就会跟着
	// 错误文案与门面日志一路外溢（NF-F9）。
	if err := CheckOrderNo(orderNo); err != nil {
		return nil, err
	}
	// 幂等键本地闸排在发送之前：同号换内容的拒绝必须发生在「不发请求」这一侧（§3.4）。
	if err := guardIdempotency(orderNo, fingerprintOf(o)); err != nil {
		return nil, err
	}

	var req any
	var signValues []string
	var orderExtra map[string]string
	if isMint {
		mr := MintRequest{
			AppID:       p.opt.AppID,
			MerchantID:  p.opt.MerchantID,
			OrderNo:     orderNo,
			Chain:       strings.TrimSpace(o.Chain),
			Network:     strings.TrimSpace(o.Network),
			Contract:    strings.TrimSpace(o.Contract),
			Standard:    strings.TrimSpace(o.Standard),
			ToAddress:   strings.TrimSpace(o.ToAddress),
			Quantity:    o.Quantity,
			MetadataURI: strings.TrimSpace(o.MetadataURI),
			NotifyURL:   p.opt.NotifyURL,
			Extra:       o.Extra,
		}
		req, signValues, orderExtra = mr, mr.SignValues(), mr.Extra
	} else {
		tr := TransferRequest{
			AppID:      p.opt.AppID,
			MerchantID: p.opt.MerchantID,
			OrderNo:    orderNo,
			Chain:      strings.TrimSpace(o.Chain),
			Network:    strings.TrimSpace(o.Network),
			Contract:   strings.TrimSpace(o.Contract),
			TokenID:    strings.TrimSpace(o.TokenID),
			FromAddr:   strings.TrimSpace(o.FromAddress),
			ToAddress:  strings.TrimSpace(o.ToAddress),
			Quantity:   o.Quantity,
			Memo:       strings.TrimSpace(o.Memo),
			NotifyURL:  p.opt.NotifyURL,
			Extra:      o.Extra,
		}
		req, signValues, orderExtra = tr, tr.SignValues(), tr.Extra
	}

	body, values, err := p.buildRequest(req, signValues, orderExtra)
	if err != nil {
		return nil, err
	}
	path, ok := p.opt.Endpoint(endpoint)
	if !ok {
		return nil, fmt.Errorf("通道[%s]未配置 endpoints.%s，该操作不可用", p.name, endpoint)
	}
	// 上面这一行是「还没发出去」的最后一道；从 dispatch 起，失败一律发生在
	// 「报文已交出」之后，语义上不可能再返回 nil 回执（NF-F5③）。
	data, err := p.dispatch(ctx, path, body, values, true)
	if err != nil {
		return unknownReceipt(orderNo, endpoint), err
	}
	res := p.Result(data, orderNo)
	return res, p.reconcileReceipt(res, orderNo, o, isMint)
}

// unknownReceipt 是「已外呼、回执读不出」的归一结果（NF-F5③）。
//
// 口径与 Result 的兜底一致：请求发出去了就绝不能回报「没发过」。nil 回执加一个错误
// 会让上游走进「失败可重下」分支，用新单号再铸一次——那是二次铸造，不是重试。
// 带 UNKNOWN 的回执会把上游逼到 NftReconcile 这条唯一出路上（§9.2-1）。
func unknownReceipt(orderNo, endpoint string) *NftResult {
	return &NftResult{
		OrderNo: orderNo,
		Status:  StatusUnknown,
		RawNote: "已外呼 " + endpoint + " 但回执不可读，受理状态存疑：需对账收敛，不得另起新单重下",
	}
}

// reconcileReceipt 是回执对账三条款（§3.3）。任一条不一致返回 error（不落广播）。
func (p *Provider) reconcileReceipt(res *NftResult, orderNo string, o *NftOrder, isMint bool) error {
	// 1. 回执单号非空且与请求单号不一致。
	if res.OrderNo != "" && res.OrderNo != orderNo {
		return fmt.Errorf("通道[%s]回执订单号 %s 与请求 %s 不一致", p.name, res.OrderNo, orderNo)
	}
	// 2. success 且回执件数非零且与请求件数不一致。
	if res.Status == StatusSuccess && res.Quantity != 0 && res.Quantity != o.Quantity {
		return fmt.Errorf("通道[%s]回执件数 %d 与请求 %d 不一致", p.name, res.Quantity, o.Quantity)
	}
	// 3. Mint 成功却不给币号：铸成功等于账没法记。
	if isMint && res.Status == StatusSuccess && strings.TrimSpace(res.TokenID) == "" {
		return fmt.Errorf("通道[%s]铸造成功回执未带 token_id", p.name)
	}
	// GasFee 非零而币种为空：nft 侧不猜、不兜底（gas 币种与资产币种天然不同种，兜底即臆断）。
	return nil
}

// QueryOrder 按幂等键查单。未查到不等于失败：本方法只在传输与包络都正常时给结论，
// 状态词认不出的情况交给 Result 归一成 UNKNOWN，由对账侧处置而非就地重下单。
func (p *Provider) QueryOrder(ctx context.Context, orderNo string) (*NftResult, error) {
	if err := CheckOrderNo(orderNo); err != nil {
		return nil, err
	}
	orderNo = strings.TrimSpace(orderNo)
	req := QueryRequest{AppID: p.opt.AppID, MerchantID: p.opt.MerchantID, OrderNo: orderNo}
	data, err := p.callWithExtras(ctx, EndpointQuery, req, req.SignValues(), nil, false)
	if err != nil {
		return nil, err
	}
	return p.Result(data, orderNo), nil
}

// QueryHoldings 按地址查名下 NFT（只读，宽松重试）。
func (p *Provider) QueryHoldings(ctx context.Context, q *HoldingsQuery) (*HoldingsResult, error) {
	if q == nil || strings.TrimSpace(q.Address) == "" {
		return nil, errors.New("持有查询缺少地址")
	}
	req := HoldingsRequest{
		AppID:      p.opt.AppID,
		MerchantID: p.opt.MerchantID,
		Address:    strings.TrimSpace(q.Address),
		Chain:      strings.TrimSpace(q.Chain),
		Network:    strings.TrimSpace(q.Network),
		Contract:   strings.TrimSpace(q.Contract),
		PageNo:     q.PageNo,
		PageSize:   q.PageSize,
	}
	data, err := p.callWithExtras(ctx, EndpointHoldings, req, req.SignValues(), nil, false)
	if err != nil {
		return nil, err
	}
	return p.holdings(data, req.Address)
}

// holdings 归一持有载荷。载荷不可解析时报错而不是返回空清单：
// 把查询故障伪装成「没持有」会造成误判资损（§3.1 失败语义）。
func (p *Provider) holdings(data json.RawMessage, address string) (*HoldingsResult, error) {
	res := &HoldingsResult{Address: address, Items: []HoldingItem{}}
	if len(data) == 0 {
		return res, nil
	}
	var d HoldingsData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("通道[%s]持有回执不可解析", p.name)
	}
	res.Address = firstNonEmpty(d.Address, address)
	res.Total = d.Total
	if d.Items != nil {
		res.Items = d.Items
	}
	return res, nil
}

// QueryToken 按四元组查单币详情（只读，宽松重试）。
func (p *Provider) QueryToken(ctx context.Context, q *TokenQuery) (*TokenInfo, error) {
	if err := checkTokenQuery(q); err != nil {
		return nil, err
	}
	req := TokenRequest{
		AppID:      p.opt.AppID,
		MerchantID: p.opt.MerchantID,
		Chain:      strings.TrimSpace(q.Chain),
		Network:    strings.TrimSpace(q.Network),
		Contract:   strings.TrimSpace(q.Contract),
		TokenID:    strings.TrimSpace(q.TokenID),
	}
	data, err := p.callWithExtras(ctx, EndpointToken, req, req.SignValues(), nil, false)
	if err != nil {
		return nil, err
	}
	return p.tokenInfo(data, req)
}

// tokenInfo 归一单币详情载荷。载荷不可解析时报错而不是返回全零详情。
func (p *Provider) tokenInfo(data json.RawMessage, req TokenRequest) (*TokenInfo, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("通道[%s]单币查询返回空载荷", p.name)
	}
	var d TokenData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("通道[%s]单币回执不可解析", p.name)
	}
	info := &TokenInfo{
		Chain:       firstNonEmpty(d.Chain, req.Chain),
		Network:     firstNonEmpty(d.Network, req.Network),
		Contract:    firstNonEmpty(d.Contract, req.Contract),
		TokenID:     firstNonEmpty(d.TokenID, req.TokenID),
		Standard:    d.Standard,
		Name:        d.Name,
		Description: d.Description,
		ImageURI:    d.ImageURI,
		MetadataURI: d.MetadataURI,
		Owner:       d.Owner,
		TotalSupply: d.TotalSupply,
	}
	return info, nil
}

// checkTokenQuery 四元组是资产的唯一坐标，缺一即拒（少一维就是把 A 链的 tokenId
// 拿去问 B 链，§2.3）。
func checkTokenQuery(q *TokenQuery) error {
	switch {
	case q == nil:
		return errors.New("单币查询入参为 nil")
	case strings.TrimSpace(q.Chain) == "":
		return errors.New("单币查询缺少链标识")
	case strings.TrimSpace(q.Network) == "":
		return errors.New("单币查询缺少网络标识")
	case strings.TrimSpace(q.Contract) == "":
		return errors.New("单币查询缺少合约地址")
	case strings.TrimSpace(q.TokenID) == "":
		return errors.New("单币查询缺少 token_id")
	}
	return nil
}

// maxOrderNoLen 是幂等键的长度上限，按 rune 计（NF-F9）。
//
// 取 64：够装「业务前缀 + 日期 + 序号」这类常见单号形态，又把单号能占的内存、
// 能拼进日志与错误文案的体量钉成常数。单号是用户可控输入，无长度上限等于
// 让调用方（或上游的恶意用户）决定本进程每条账要占多少字节。
// 按 rune 而不是字节计：字节数会把中文/emoji 单号按 3-4 倍惩罚，误杀合法的
// 多语言业务单号；上限依然是常数（64 rune ≤ 256 字节），内存界限没有松动。
const maxOrderNoLen = 64

// CheckOrderNo 校验幂等键形态：去空白后非空、长度受限、不含控制符。
//
// 这里刻意不套 `[A-Za-z0-9_-]` 白名单字符集（对 SECURITY.md NF-F9 建议的勘误，
// 理由记在 tasks/software-dev-team/THOUGHTS.md）：本包从不把单号放进 URL、shell
// 或 SQL（SECURITY.md §114 已核），F9 真正命名的两个危害是
// 「无界内存增长」与「\r\n 日志注入」——长度上限 + 控制符拒绝 + 账本软告警
// 已经把它们封死；再收字符集只会误杀业务侧合法的日期型单号（如 NFT/2026/10/01/0001），
// 换不到任何安全增量。
func CheckOrderNo(orderNo string) error {
	s := strings.TrimSpace(orderNo)
	if s == "" {
		return errors.New("订单号（幂等键）为空")
	}
	if utf8.RuneCountInString(s) > maxOrderNoLen {
		return fmt.Errorf("订单号（幂等键）长度超过 %d", maxOrderNoLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("订单号形态非法：不得含控制字符（会伪造日志行）")
		}
	}
	return nil
}

// CheckMint 铸造前置校验：幂等键、件数、接收地址、MetadataURI 必填，TokenID 必须为空
// （铸出的号由回执给，请求里带号=拿铸造单改铸别人）；Contract 可空（供应商部署新合约）。
func CheckMint(o *NftOrder) error {
	switch {
	case o == nil:
		return errors.New("订单为 nil")
	case strings.TrimSpace(o.OrderNo) == "":
		return errors.New("订单号（幂等键）为空")
	case o.Quantity < 1:
		return errors.New("件数必须为正整数")
	case strings.TrimSpace(o.ToAddress) == "":
		return errors.New("接收地址为空")
	case strings.TrimSpace(o.TokenID) != "":
		return errors.New("铸造请求不得携带 token_id")
	case strings.TrimSpace(o.MetadataURI) == "":
		return errors.New("铸造缺少 metadata_uri")
	}
	return nil
}

// CheckTransfer 转账前置校验：幂等键、件数、接收地址必填，且 Contract/TokenID 必填
// （转的是已铸成的币）；MetadataURI 必须为空（转账不改写元数据）。
func CheckTransfer(o *NftOrder) error {
	switch {
	case o == nil:
		return errors.New("订单为 nil")
	case strings.TrimSpace(o.OrderNo) == "":
		return errors.New("订单号（幂等键）为空")
	case o.Quantity < 1:
		return errors.New("件数必须为正整数")
	case strings.TrimSpace(o.ToAddress) == "":
		return errors.New("接收地址为空")
	case strings.TrimSpace(o.Contract) == "":
		return errors.New("转账缺少合约地址")
	case strings.TrimSpace(o.TokenID) == "":
		return errors.New("转账缺少 token_id")
	case strings.TrimSpace(o.MetadataURI) != "":
		return errors.New("转账请求不得携带 metadata_uri")
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// clip 截断长度，避免把整段响应塞进日志与摘要字段。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sanitizeEcho 把外部（供应商）回显串里的控制字符换成空格（NF-F11）。
//
// 覆盖 C0、DEL 与 C1（unicode.IsControl 三段都算）：\r\n 是日志注入，
// \x1b 是终端控制序列，两者都会随错误文案、RawNote 一路进 vars 日志面。
// 只用于「对方写给我们的字符串」，本包自产的文案不过这里。
func sanitizeEcho(s string) string {
	if s == "" {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
