package zalo

// 本文件是 Zalo 凭证的全部处理面：JSON 形态识别、cookie 归一化、快照、IMEI 生成、
// 安全错误包装。它不碰文件系统、不碰数据库、不发网络请求——输入是字节与会话上下文，
// 输出是可直接交给 zcago.Login 的 Credentials。
//
// 为什么 cookie 必须归一化（zcago v0.3.0 的域名陷阱，已读源码核实）：
//   - 登录接口在 https://wpa.chat.zalo.me/api/login/getLoginInfo
//     （session/auth/login.go:57），而恢复会话时 BuildCookieJar 用的默认 URL 是
//     https://chat.zalo.me（config/config.go DefaultURL、zcago.go loginCookie）；
//   - BuildCookieJar 把 cookie 设成哪个域由 cookie 自身 Domain 决定；若 Domain 为空，
//     Go 的 jar 只会把它记成 host-only chat.zalo.me，子域 wpa.chat.zalo.me 收不到，
//     于是「刚存下的凭证重启后登录失败」；
//   - jar.Cookies() 只返回 Name/Value，Domain 已经丢了；zcago 的 Cookie.FromHTTPCookie
//     还会按 Domain 是否为空反推 HTTPOnly（credentials.go:114），不能用。
//
// 所以本文件不走 zcago.NewHTTPCookie / FromHTTPCookie，而是逐条构造 zcago.Cookie：
// Domain 固定 zalo.me、Path "/"、Secure/Session/HTTPOnly 全开。这只把会话 cookie
// 扩到 Zalo 自己的全部子域（*.zalo.me 都是 Zalo 域，不跨站），是恢复会话的必要条件。

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/amrakk/zcago"
	zcagoconfig "github.com/amrakk/zcago/config"
	zcagoerrs "github.com/amrakk/zcago/errs"
	"github.com/amrakk/zcago/session"
)

// zaCookieHosts 是收集 cookie 时的探测优先级：登录票据在 wpa.chat.zalo.me，
// 网页在 chat.zalo.me，身份票据在 id.zalo.me，其余散落在 zalo.me。
// 同名 cookie 保留先出现者（高优先级 host 的那一份）。
var zaCookieHosts = []string{"wpa.chat.zalo.me", "chat.zalo.me", "id.zalo.me", "zalo.me"}

// zaZaloCookieDomain 是归一化后写入的唯一 Domain。
const zaZaloCookieDomain = "zalo.me"

// zaSafeError 是「安全文案 + 原始错误」的包装。
//
// Error() 只给固定文案，保证 cookie/IMEI/原始 JSON 不会顺着 error 进日志；
// Unwrap() 保留原错误，调用方仍可用 errors.Is/As 判定哨兵与底层原因。
type zaSafeError struct {
	msg string
	err error
}

// Error 返回安全文案，永不拼接底层错误的字符串。
func (e *zaSafeError) Error() string { return e.msg }

// Unwrap 暴露原始错误，仅用于 errors.Is/As，不用于打印。
func (e *zaSafeError) Unwrap() error { return e.err }

var _ error = (*zaSafeError)(nil)

// zaSafeWrap 把 err 包成安全文案；err 为 nil 时返回 nil（与 fmt.Errorf %w 口径一致）。
func zaSafeWrap(msg string, err error) error {
	if err == nil {
		return nil
	}
	return &zaSafeError{msg: msg, err: err}
}

// zaSafeReason 从上游错误里取一段**可安全回显**的原因，供「失败要回前端」的场景用。
//
// 只认两类：
//   - Zalo API 错误（zcago errs.ZaloAPIError）：自带错误码与服务端消息，消息本身是
//     Zalo 的短状态说明（如「Đăng nhập thất bại」），不含凭据；
//   - 本包传输层兜底造的 599（zaFallbackTransport）：文案已去掉 URL 与参数。
//
// 其余错误（JSON 解析、加密失败等）可能带响应体，一律不给 —— 宁可少给信息，
// 也不能把会话材料顺进前端文案。
func zaSafeReason(err error) string {
	if err == nil {
		return ""
	}
	var apiErr zcagoerrs.ZaloAPIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	msg := strings.TrimSpace(apiErr.Message)
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 120 {
		msg = msg[:120]
	}
	if apiErr.Code != nil {
		if msg == "" {
			return fmt.Sprintf("code=%d", *apiErr.Code)
		}
		return fmt.Sprintf("code=%d %s", *apiErr.Code, msg)
	}
	return msg
}

// zaFetchError 组装「拉取类」失败的安全文案：有可回显原因就带上原因。
func zaFetchError(msg string, err error) error {
	if err == nil {
		return nil
	}
	if reason := zaSafeReason(err); reason != "" {
		return zaSafeWrap(msg+"："+reason, err)
	}
	return zaSafeWrap(msg, err)
}

// parseZACredentials 解析导入的凭证字节，支持四种输入：
//
//  1. 信封 {"account_id","uid","updated_at","credentials":{...}}（本包 ZaloListAccounts
//     之外的导出格式；信封里的 account_id 与参数不一致直接拒绝，防止把 A 的会话
//     落到 B 名下）；
//  2. 完整 zcago Credentials（imei/userAgent/cookie/language）；
//  3. 裸 cookie 数组 [{name,value,domain,...}]；
//  4. J2Cookie {"url":"https://chat.zalo.me","cookies":[...]}。
//
// 完整 Credentials 的 IMEI/UA 原样保留（它是扫码/上次登录的真实设备标识，另生成
// 会让 Zalo 侧视为换设备）；裸 cookie/J2Cookie 没有 IMEI，才用 newZAImei 生成，
// UA 缺省用 defaultUA。defaultLang 非空且输入没有 language 时补上。
//
// 所有失败都包 ErrZaloBadCredentials，文案只描述形态问题，不回显原始 JSON。
func parseZACredentials(raw []byte, accountID, defaultUA, defaultLang string) (zcago.Credentials, error) {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return zcago.Credentials{}, fmt.Errorf("%w: 凭证 JSON 为空", ErrZaloBadCredentials)
	}

	if body[0] == '{' {
		var env struct {
			AccountID   string          `json:"account_id"`
			Credentials json.RawMessage `json:"credentials"`
		}
		if err := json.Unmarshal(body, &env); err == nil && !isJSONNull(env.Credentials) {
			if env.AccountID != "" && accountID != "" && env.AccountID != accountID {
				return zcago.Credentials{}, fmt.Errorf("%w: 信封 account_id 与目标账号不一致", ErrZaloBadCredentials)
			}
			if env.AccountID != "" {
				if err := validateZAAccountID(env.AccountID); err != nil {
					return zcago.Credentials{}, fmt.Errorf("%w: 信封 account_id 不符合账号规则", ErrZaloBadCredentials)
				}
			}
			return parseZACredentialBody(env.Credentials, defaultUA, defaultLang)
		}
	}
	return parseZACredentialBody(body, defaultUA, defaultLang)
}

// parseZACredentialBody 识别非信封的三种形态。
func parseZACredentialBody(body []byte, defaultUA, defaultLang string) (zcago.Credentials, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return zcago.Credentials{}, fmt.Errorf("%w: 凭证 JSON 为空", ErrZaloBadCredentials)
	}
	if body[0] == '[' {
		return parseZACookieArray(body, defaultUA, defaultLang)
	}
	if body[0] != '{' {
		return zcago.Credentials{}, fmt.Errorf("%w: 凭证必须是 JSON 对象或数组", ErrZaloBadCredentials)
	}

	// 用 probe 判断对象形态：三种对象的键集互斥（cookie 单数=完整凭证，
	// cookies 复数=J2Cookie），不会误判。
	var probe struct {
		IMEI      *string         `json:"imei"`
		UserAgent *string         `json:"userAgent"`
		Cookie    json.RawMessage `json:"cookie"`
		Cookies   json.RawMessage `json:"cookies"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return zcago.Credentials{}, fmt.Errorf("%w: 凭证 JSON 解析失败", ErrZaloBadCredentials)
	}
	if probe.IMEI != nil || probe.UserAgent != nil || !isJSONNull(probe.Cookie) {
		return parseZACompleteCredentials(body, defaultLang)
	}
	if !isJSONNull(probe.Cookies) {
		return parseZAJ2Cookie(body, defaultUA, defaultLang)
	}
	return zcago.Credentials{}, fmt.Errorf("%w: 无法识别的凭证形态（需要完整 Credentials、cookie 数组或 J2Cookie）", ErrZaloBadCredentials)
}

// parseZACompleteCredentials 处理完整 zcago Credentials：IMEI/UA 原样保留，
// cookie 仍走归一化（它可能来自别的导出器，域名/标志位不可信）。
func parseZACompleteCredentials(body []byte, defaultLang string) (zcago.Credentials, error) {
	var creds zcago.Credentials
	if err := json.Unmarshal(body, &creds); err != nil {
		return zcago.Credentials{}, fmt.Errorf("%w: 凭证 JSON 解析失败", ErrZaloBadCredentials)
	}
	imei := strings.TrimSpace(creds.IMEI)
	if imei == "" {
		return zcago.Credentials{}, fmt.Errorf("%w: 完整凭证缺少 imei", ErrZaloBadCredentials)
	}
	ua := strings.TrimSpace(creds.UserAgent)
	if ua == "" {
		return zcago.Credentials{}, fmt.Errorf("%w: 完整凭证缺少 userAgent", ErrZaloBadCredentials)
	}

	// CookieUnion 的内部结构不对外暴露（GetCookies 不给 J2Cookie 的 URL），
	// 所以内嵌 J2Cookie 的 URL 只能从原始 JSON 再看一眼：外域地址同样要拒。
	var cookieShape struct {
		Cookie json.RawMessage `json:"cookie"`
	}
	if err := json.Unmarshal(body, &cookieShape); err == nil {
		trimmed := bytes.TrimSpace(cookieShape.Cookie)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var j struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(trimmed, &j); err == nil {
				if err := validateZAJ2CookieURL(j.URL); err != nil {
					return zcago.Credentials{}, err
				}
			}
		}
	}

	union, err := normalizeZACookieUnion(creds.Cookie)
	if err != nil {
		return zcago.Credentials{}, err
	}
	return zcago.Credentials{
		IMEI:      imei,
		Cookie:    &union,
		UserAgent: ua,
		Language:  zaLanguageOr(creds.Language, defaultLang),
	}, nil
}

// parseZACookieArray 处理裸 cookie 数组。
func parseZACookieArray(body []byte, defaultUA, defaultLang string) (zcago.Credentials, error) {
	var arr []zcago.Cookie
	if err := json.Unmarshal(body, &arr); err != nil {
		return zcago.Credentials{}, fmt.Errorf("%w: cookie 数组解析失败", ErrZaloBadCredentials)
	}
	cookies := make([]*http.Cookie, 0, len(arr))
	for i := range arr {
		cookies = append(cookies, arr[i].ToHTTPCookie())
	}
	return buildZACredentialsFromCookies(cookies, defaultUA, defaultLang)
}

// parseZAJ2Cookie 处理 J2Cookie：URL 必须是 zalo.me 或其子域（空 URL 可接受）。
func parseZAJ2Cookie(body []byte, defaultUA, defaultLang string) (zcago.Credentials, error) {
	var j zcago.J2Cookie
	if err := json.Unmarshal(body, &j); err != nil {
		return zcago.Credentials{}, fmt.Errorf("%w: J2Cookie 解析失败", ErrZaloBadCredentials)
	}
	if err := validateZAJ2CookieURL(j.URL); err != nil {
		return zcago.Credentials{}, err
	}
	cookies := make([]*http.Cookie, 0, len(j.Cookies))
	for i := range j.Cookies {
		cookies = append(cookies, j.Cookies[i].ToHTTPCookie())
	}
	return buildZACredentialsFromCookies(cookies, defaultUA, defaultLang)
}

// buildZACredentialsFromCookies 把「只有 cookie」的输入补全成可登录的 Credentials：
// UA 用 defaultUA，defaultUA 为空时回落 zcago 默认 UA；IMEI 新生成；language 用
// defaultLang（没有就交给 zcago 默认）。
func buildZACredentialsFromCookies(cookies []*http.Cookie, defaultUA, defaultLang string) (zcago.Credentials, error) {
	ua := strings.TrimSpace(defaultUA)
	if ua == "" {
		// zcago 的 Credentials.IsValid 要求 UA 非空；裸 cookie 导入没有 UA 时用
		// zcago 自己的默认值，与 QR 登录的缺省一致。
		ua = zcagoconfig.DefaultUserAgent
	}
	union, err := normalizeZACookies(cookies)
	if err != nil {
		return zcago.Credentials{}, err
	}
	imei, err := newZAImei(ua)
	if err != nil {
		return zcago.Credentials{}, err
	}
	return zcago.Credentials{
		IMEI:      imei,
		Cookie:    &union,
		UserAgent: ua,
		Language:  zaLanguageOr(nil, defaultLang),
	}, nil
}

// normalizeZACookieUnion 把 zcago 的 CookieUnion（数组或 J2Cookie 内嵌）转成
// http.Cookie 再走同一套归一化。
func normalizeZACookieUnion(cu *zcago.CookieUnion) (zcago.CookieUnion, error) {
	if cu == nil {
		return zcago.CookieUnion{}, fmt.Errorf("%w: 凭证没有 cookie", ErrZaloBadCredentials)
	}
	raw := cu.GetCookies()
	cookies := make([]*http.Cookie, 0, len(raw))
	for i := range raw {
		cookies = append(cookies, raw[i].ToHTTPCookie())
	}
	return normalizeZACookies(cookies)
}

// normalizeZACookies 把任意来源的 cookie 集归一化为「只属于 zalo.me」的形态：
// 每条写死 Domain=zalo.me、Path=/、Secure/Session/HTTPOnly=true、HostOnly=false、
// SameSite 默认；按 Name 去重（先出现者胜）并按 Name 排序（让写库结果稳定、可对比）。
//
// 空集、纯无名条目、外域 cookie 一律拒绝：一条外域 cookie 说明导入源选错了，
// 放过它会让整个会话在 wpa.chat.zalo.me 上缺票据。
func normalizeZACookies(cookies []*http.Cookie) (zcago.CookieUnion, error) {
	if len(cookies) == 0 {
		return zcago.CookieUnion{}, fmt.Errorf("%w: 凭证没有 cookie", ErrZaloBadCredentials)
	}
	seen := make(map[string]struct{}, len(cookies))
	out := make([]zcago.Cookie, 0, len(cookies))
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		if !zaCookieDomainAllowed(c.Domain) {
			return zcago.CookieUnion{}, fmt.Errorf("%w: cookie 域不属于 zalo.me", ErrZaloBadCredentials)
		}
		if _, dup := seen[c.Name]; dup {
			continue
		}
		seen[c.Name] = struct{}{}
		out = append(out, zcago.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   zaZaloCookieDomain,
			Path:     "/",
			Secure:   true,
			Session:  true,
			HTTPOnly: true,
			HostOnly: false,
			SameSite: zcago.SameSiteDefault,
		})
	}
	if len(out) == 0 {
		return zcago.CookieUnion{}, fmt.Errorf("%w: 凭证没有有效 cookie", ErrZaloBadCredentials)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return zcago.NewCookieArray(out), nil
}

// zaCookieDomainAllowed 判断导入 cookie 声明的域是否属于 zalo.me。
// 空 Domain 视为未声明（浏览器导出的裸 name/value 常见），归一化时会补成 zalo.me。
func zaCookieDomainAllowed(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return true
	}
	d = strings.TrimPrefix(d, ".")
	return d == zaZaloCookieDomain || strings.HasSuffix(d, "."+zaZaloCookieDomain)
}

// collectZACookies 从 jar 里按 zaCookieHosts 的优先级收集 cookie。
//
// Go 的 cookiejar 按 URL 域选择性返回 cookie：host-only 的 chat.zalo.me cookie
// 不会出现在 wpa.chat.zalo.me 的查询里，而 Domain=zalo.me 的会出现在所有子域查询里。
// 按「最具体 host 优先」的顺序遍历并做同名去重，等价于浏览器发请求时的那套选择逻辑。
func collectZACookies(jar http.CookieJar) []*http.Cookie {
	if jar == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(zaCookieHosts)*2)
	out := make([]*http.Cookie, 0, len(zaCookieHosts)*2)
	for _, host := range zaCookieHosts {
		u := &url.URL{Scheme: "https", Host: host}
		for _, c := range jar.Cookies(u) {
			if c == nil || c.Name == "" {
				continue
			}
			if _, dup := seen[c.Name]; dup {
				continue
			}
			seen[c.Name] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// snapshotZACredentials 把「当前活会话」快照成可落库的 Credentials。
//
// 登录成功后 cookie 可能被服务端刷新（zcago 内部 jar 是权威），所以持久化必须取
// 会话此刻的 CookieJar/IMEI/UA/Language，而不是回显导入的值。扫码流程生成的 IMEI
// 也只在这里拿得到。IMEI/UA/cookie 任一为空都返回错误——写一条空壳行等于埋一个
// 「重启后悄悄登录失败」的坑。
func snapshotZACredentials(sc session.Context) (zcago.Credentials, error) {
	if sc == nil {
		return zcago.Credentials{}, fmt.Errorf("%w: 会话为空", ErrZaloBadCredentials)
	}
	union, err := normalizeZACookies(collectZACookies(sc.CookieJar()))
	if err != nil {
		return zcago.Credentials{}, zaSafeWrap("zalo 会话快照失败: 当前会话没有可用 cookie", err)
	}
	imei := strings.TrimSpace(sc.IMEI())
	if imei == "" {
		return zcago.Credentials{}, fmt.Errorf("%w: 当前会话没有 IMEI", ErrZaloBadCredentials)
	}
	ua := strings.TrimSpace(sc.UserAgent())
	if ua == "" {
		return zcago.Credentials{}, fmt.Errorf("%w: 当前会话没有 User-Agent", ErrZaloBadCredentials)
	}
	var lang *string
	if l := strings.TrimSpace(sc.Language()); l != "" {
		lang = &l
	}
	return zcago.Credentials{
		IMEI:      imei,
		Cookie:    &union,
		UserAgent: ua,
		Language:  lang,
	}, nil
}

// newZAImei 生成与 zcago generateZaloUUID 同形的 IMEI：UUID v4 字符串 + "-" +
// md5(UA) 的 hex（zcago.go:171-175）。用 crypto/rand 自己拼 v4，不把 google/uuid
// 作为直接依赖。
func newZAImei(ua string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", zaSafeWrap("生成 zalo IMEI 失败", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	u := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	sum := md5.Sum([]byte(ua))
	return u + "-" + hex.EncodeToString(sum[:]), nil
}

// validateZAJ2CookieURL 校验 J2Cookie 的 url 字段：空串可接受（有些导出器不写），
// 非空时 host 必须是 zalo.me 或其子域。
func validateZAJ2CookieURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: J2Cookie url 不可解析", ErrZaloBadCredentials)
	}
	host := strings.ToLower(u.Hostname())
	if host == zaZaloCookieDomain || strings.HasSuffix(host, "."+zaZaloCookieDomain) {
		return nil
	}
	return fmt.Errorf("%w: J2Cookie url 不属于 zalo.me", ErrZaloBadCredentials)
}

// zaLanguageOr 在输入没有 language 时回填缺省值；两者都空则返回 nil，
// 交给 zcago 用它自己的默认语言。
func zaLanguageOr(lang *string, defaultLang string) *string {
	if lang != nil && strings.TrimSpace(*lang) != "" {
		v := strings.TrimSpace(*lang)
		return &v
	}
	if strings.TrimSpace(defaultLang) != "" {
		v := strings.TrimSpace(defaultLang)
		return &v
	}
	return nil
}

// isJSONNull 判断 json.RawMessage 是否代表「字段缺失或 null」。
func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}
