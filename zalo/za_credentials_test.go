package zalo

// 本文件是凭证层的活闸：四种输入形态、信封账号校验、外域 cookie 拒绝、域名归一化、
// 同名去重与排序、jar 收集优先级、IMEI 形状、安全错误包装、会话快照。
//
// 所有测试值都是合成串（不是真实 cookie/IMEI）；失败信息只报名字与数量，
// 不把 cookie/IMEI 值写进测试输出。

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/amrakk/zcago"
	zcagoconfig "github.com/amrakk/zcago/config"
	"github.com/amrakk/zcago/session"
)

// zaImeiShape 是带 UA 哈希后缀的 IMEI 形状：UUID v4 + "-" + 32 位 md5 hex。
var zaImeiShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}-[0-9a-f]{32}$`)

// zaCookieNames 取 cookie 的 Name 序列，便于断言顺序。
func zaCookieNames(cookies []zcago.Cookie) []string {
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		names = append(names, c.Name)
	}
	return names
}

// zaAssertNormalizedCookie 断言一条 cookie 已按归一化规则重写。
func zaAssertNormalizedCookie(t *testing.T, c zcago.Cookie) {
	t.Helper()
	if c.Domain != zaZaloCookieDomain {
		t.Fatalf("cookie %q 的 Domain=%q，期望 %q", c.Name, c.Domain, zaZaloCookieDomain)
	}
	if c.Path != "/" {
		t.Fatalf("cookie %q 的 Path=%q，期望 /", c.Name, c.Path)
	}
	if !c.Secure || !c.Session || !c.HTTPOnly {
		t.Fatalf("cookie %q 应同时是 Secure/Session/HTTPOnly，实得 %v/%v/%v", c.Name, c.Secure, c.Session, c.HTTPOnly)
	}
	if c.HostOnly {
		t.Fatalf("cookie %q 不应是 HostOnly", c.Name)
	}
	if c.SameSite != zcago.SameSiteDefault {
		t.Fatalf("cookie %q 的 SameSite=%q，期望默认", c.Name, c.SameSite)
	}
	if c.ExpirationDate != 0 {
		t.Fatalf("cookie %q 不该带过期时间（Session cookie）", c.Name)
	}
}

// TestZAParseCredentialsComplete 完整 Credentials：IMEI/UA 原样保留，
// language 原样保留，cookie 仍归一化。
func TestZAParseCredentialsComplete(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"imei":"imei-fixed-1",
		"cookie":[
			{"domain":".zalo.me","name":"zeta","value":"v-z","path":"/","secure":true,"session":true,"httpOnly":true},
			{"domain":"wpa.chat.zalo.me","name":"alpha","value":"v-a","path":"/deep","secure":false}
		],
		"userAgent":"ua-fixed/1.0",
		"language":"en"
	}`)

	creds, err := parseZACredentials(raw, "acct", "ua-default", "vi")
	if err != nil {
		t.Fatalf("解析完整凭证失败: %v", err)
	}
	if creds.IMEI != "imei-fixed-1" {
		t.Fatalf("IMEI 应原样保留，实得 %q", creds.IMEI)
	}
	if creds.UserAgent != "ua-fixed/1.0" {
		t.Fatalf("UserAgent 应原样保留，实得 %q", creds.UserAgent)
	}
	if creds.Language == nil || *creds.Language != "en" {
		t.Fatalf("language 应原样保留（en）")
	}
	if creds.Cookie == nil {
		t.Fatal("Cookie 不应为空")
	}
	cookies := creds.Cookie.GetCookies()
	if len(cookies) != 2 {
		t.Fatalf("cookie 数=%d，期望 2", len(cookies))
	}
	if cookies[0].Name != "alpha" || cookies[1].Name != "zeta" {
		t.Fatalf("cookie 应按 Name 排序，实得 %v", zaCookieNames(cookies))
	}
	for _, c := range cookies {
		zaAssertNormalizedCookie(t, c)
	}
}

// TestZAParseCredentialsBareCookies 裸 cookie 数组：缺 IMEI 生成（UA 哈希后缀），
// 缺 UA 用 defaultUA，缺 language 补 defaultLang；同名去重保留首个。
func TestZAParseCredentialsBareCookies(t *testing.T) {
	t.Parallel()

	const ua = "ua-cfg/2.0"
	raw := []byte(`[
		{"name":"zeta","value":"v1"},
		{"name":"alpha","value":"v2","domain":"chat.zalo.me"},
		{"name":"zeta","value":"v-dup"}
	]`)

	creds, err := parseZACredentials(raw, "acct", ua, "vi")
	if err != nil {
		t.Fatalf("解析裸 cookie 数组失败: %v", err)
	}
	if !zaImeiShape.MatchString(creds.IMEI) {
		t.Fatalf("生成的 IMEI 形状不符: %q", creds.IMEI)
	}
	sum := md5.Sum([]byte(ua))
	if !strings.HasSuffix(creds.IMEI, "-"+hex.EncodeToString(sum[:])) {
		t.Fatal("IMEI 的哈希后缀应等于 md5(UA)")
	}
	if creds.UserAgent != ua {
		t.Fatalf("UserAgent 应回落到 defaultUA，实得 %q", creds.UserAgent)
	}
	if creds.Language == nil || *creds.Language != "vi" {
		t.Fatal("language 应回落到 defaultLang")
	}
	cookies := creds.Cookie.GetCookies()
	if len(cookies) != 2 {
		t.Fatalf("同名 cookie 未去重: 数=%d，期望 2", len(cookies))
	}
	if cookies[0].Name != "alpha" || cookies[1].Name != "zeta" {
		t.Fatalf("cookie 顺序错: %v", zaCookieNames(cookies))
	}
	if cookies[1].Value != "v1" {
		t.Fatal("同名去重应保留首个出现的 cookie")
	}
	for _, c := range cookies {
		zaAssertNormalizedCookie(t, c)
	}
}

// TestZAParseCredentialsJ2Cookie J2Cookie 形态：url host 必须是 zalo.me 域。
func TestZAParseCredentialsJ2Cookie(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"url":"https://chat.zalo.me/pc","cookies":[{"name":"b","value":"2"},{"name":"a","value":"1"}]}`)
	creds, err := parseZACredentials(raw, "acct", "ua-j2", "vi")
	if err != nil {
		t.Fatalf("解析 J2Cookie 失败: %v", err)
	}
	cookies := creds.Cookie.GetCookies()
	if len(cookies) != 2 || cookies[0].Name != "a" || cookies[1].Name != "b" {
		t.Fatalf("J2Cookie 的 cookie 未正确归一化: %v", zaCookieNames(cookies))
	}
	if !zaImeiShape.MatchString(creds.IMEI) {
		t.Fatalf("J2Cookie 缺 IMEI 时应生成，实得形状不符")
	}

	// 空 url 可接受。
	if _, err := parseZACredentials([]byte(`{"cookies":[{"name":"a","value":"1"}]}`), "acct", "ua-j2", "vi"); err != nil {
		t.Fatalf("空 url 的 J2Cookie 应接受: %v", err)
	}
	// 外域 url 拒绝。
	_, err = parseZACredentials([]byte(`{"url":"https://evil.example/x","cookies":[{"name":"a","value":"1"}]}`), "acct", "ua-j2", "vi")
	if !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("外域 J2Cookie url 应被拒绝，实得 %v", err)
	}
}

// TestZAParseCredentialsEnvelope 信封形态：account_id 必须与目标账号一致。
func TestZAParseCredentialsEnvelope(t *testing.T) {
	t.Parallel()

	matching := []byte(`{
		"account_id":"acct",
		"uid":"uid-1",
		"updated_at":"2026-01-02T03:04:05Z",
		"credentials":{"imei":"imei-env","cookie":[{"name":"a","value":"1"}],"userAgent":"ua-env"}
	}`)
	creds, err := parseZACredentials(matching, "acct", "ua-default", "vi")
	if err != nil {
		t.Fatalf("匹配账号的信封应解析成功: %v", err)
	}
	if creds.IMEI != "imei-env" || creds.UserAgent != "ua-env" {
		t.Fatal("信封内完整凭证的 IMEI/UA 应原样保留")
	}

	mismatched := []byte(`{
		"account_id":"another",
		"credentials":{"imei":"imei-env","cookie":[{"name":"a","value":"1"}],"userAgent":"ua-env"}
	}`)
	if _, err := parseZACredentials(mismatched, "acct", "ua-default", "vi"); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("信封 account_id 不匹配应拒绝，实得 %v", err)
	}

	// 信封里是裸 cookie 数组也应支持。
	nested := []byte(`{"account_id":"acct","credentials":[{"name":"a","value":"1"}]}`)
	if _, err := parseZACredentials(nested, "acct", "ua-default", "vi"); err != nil {
		t.Fatalf("信封内裸 cookie 数组应解析成功: %v", err)
	}

	// 信封自带的 id 不符合账号规则时拒绝（即使与参数同名）。
	badID := []byte(`{"account_id":"..","credentials":[{"name":"a","value":"1"}]}`)
	if _, err := parseZACredentials(badID, "..", "ua-default", "vi"); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("非法信封 account_id 应拒绝，实得 %v", err)
	}
}

// TestZAParseCredentialsRejects 各类非法输入：一律 ErrZaloBadCredentials，
// 且错误文案绝不回显原始 JSON / cookie 值（用 S3CR3T 标记做探针）。
func TestZAParseCredentialsRejects(t *testing.T) {
	t.Parallel()

	const marker = "S3CR3T"
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "  \n\t "},
		{name: "null_literal", raw: "null"},
		{name: "empty_object", raw: "{}"},
		{name: "json_string", raw: `"` + marker + `"`},
		{name: "broken_json", raw: `{"imei":"` + marker + `",`},
		{name: "empty_cookie_array", raw: `[]`},
		{name: "nameless_cookie", raw: `[{"value":"` + marker + `"}]`},
		{name: "foreign_cookie_domain", raw: `[{"name":"a","value":"` + marker + `","domain":"evil.example"}]`},
		{name: "only_nameless_full", raw: `{"imei":"i","userAgent":"ua","cookie":[{"value":"` + marker + `"}]}`},
		{name: "full_missing_imei", raw: `{"userAgent":"ua","cookie":[{"name":"a","value":"` + marker + `"}]}`},
		{name: "full_missing_user_agent", raw: `{"imei":"i","cookie":[{"name":"a","value":"` + marker + `"}]}`},
		{name: "full_null_cookie", raw: `{"imei":"i","userAgent":"ua","cookie":null}`},
		{name: "full_empty_cookie", raw: `{"imei":"i","userAgent":"ua","cookie":[]}`},
		{name: "j2_foreign_url", raw: `{"url":"https://evil.example/x","cookies":[{"name":"a","value":"` + marker + `"}]}`},
		{name: "j2_bad_url", raw: `{"url":"not a url","cookies":[{"name":"a","value":"` + marker + `"}]}`},
		{name: "j2_empty_cookies", raw: `{"url":"https://chat.zalo.me","cookies":[]}`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseZACredentials([]byte(tc.raw), "acct", "ua-default", "vi")
			if err == nil {
				t.Fatal("应拒绝")
			}
			if !errors.Is(err, ErrZaloBadCredentials) {
				t.Fatalf("应包装 ErrZaloBadCredentials，实得 %v", err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("错误文案泄漏了原始值")
			}
		})
	}
}

// TestZAParseCredentialsBareCookiesUsesZcagoDefaultUA 裸 cookie 在 defaultUA
// 也为空时回落 zcago 默认 UA，而不是拒绝——这是「配置留空用 zcago 默认」的承诺。
func TestZAParseCredentialsBareCookiesUsesZcagoDefaultUA(t *testing.T) {
	t.Parallel()

	creds, err := parseZACredentials([]byte(`[{"name":"a","value":"1"}]`), "acct", "", "vi")
	if err != nil {
		t.Fatalf("缺 UA 时应回落 zcago 默认值: %v", err)
	}
	if creds.UserAgent != zcagoconfig.DefaultUserAgent {
		t.Fatalf("UserAgent=%q，期望 zcago 默认 %q", creds.UserAgent, zcagoconfig.DefaultUserAgent)
	}
	if creds.IMEI == "" {
		t.Fatal("应生成 IMEI")
	}
}

// TestZANormalizeZACookies 归一化：强制字段、同名去重（首个胜）、按 Name 排序。
func TestZANormalizeZACookies(t *testing.T) {
	t.Parallel()

	in := []*http.Cookie{
		nil,
		{Name: "b", Value: "b1", Domain: ".zalo.me", Path: "/deep"},
		{Name: "a", Value: "a1", Domain: "chat.zalo.me", Path: "/x/y", Secure: false},
		{Name: "b", Value: "b2", Domain: "zalo.me", Path: "/", Secure: true},
		{Value: "nameless"},
	}
	union, err := normalizeZACookies(in)
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	cookies := union.GetCookies()
	if len(cookies) != 2 {
		t.Fatalf("cookie 数=%d，期望 2（去重 + 跳过无名）", len(cookies))
	}
	if cookies[0].Name != "a" || cookies[1].Name != "b" {
		t.Fatalf("cookie 顺序错: %v", zaCookieNames(cookies))
	}
	if cookies[1].Value != "b1" {
		t.Fatal("同名 cookie 应保留首个出现的值")
	}
	for _, c := range cookies {
		zaAssertNormalizedCookie(t, c)
	}
}

// TestZANormalizeZACookieDomains 只接受 zalo.me 与它的子域；空域视为未声明。
func TestZANormalizeZACookieDomains(t *testing.T) {
	t.Parallel()

	allowed := []string{"", "zalo.me", ".zalo.me", "chat.zalo.me", ".wpa.chat.zalo.me", "ZALO.ME"}
	for _, domain := range allowed {
		if _, err := normalizeZACookies([]*http.Cookie{{Name: "a", Value: "1", Domain: domain}}); err != nil {
			t.Fatalf("域 %q 应被接受，实得 %v", domain, err)
		}
	}
	rejected := []string{"evil.example", "notzalo.me", "zalo.me.evil.example", "zalo.me.", "example.com"}
	for _, domain := range rejected {
		if _, err := normalizeZACookies([]*http.Cookie{{Name: "a", Value: "1", Domain: domain}}); !errors.Is(err, ErrZaloBadCredentials) {
			t.Fatalf("域 %q 应被拒绝，实得 %v", domain, err)
		}
	}
}

// TestZANormalizeZACookiesRejectsEmpty 空集与纯无名集都拒绝：
// 一条没有 cookie 的凭证登录必然失败，不能写库。
func TestZANormalizeZACookiesRejectsEmpty(t *testing.T) {
	t.Parallel()

	if _, err := normalizeZACookies(nil); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("nil 集应拒绝，实得 %v", err)
	}
	if _, err := normalizeZACookies([]*http.Cookie{}); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("空集应拒绝，实得 %v", err)
	}
	if _, err := normalizeZACookies([]*http.Cookie{{Value: "v"}}); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("纯无名集应拒绝，实得 %v", err)
	}
}

// TestZACollectZACookiesPriority 用真实 cookiejar 验证收集优先级：
// wpa.chat.zalo.me → chat.zalo.me → id.zalo.me → zalo.me，同名保留高优先级者，
// Domain=zalo.me 的 cookie 只出现一次。
func TestZACollectZACookiesPriority(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("建 jar 失败: %v", err)
	}
	set := func(host string, c *http.Cookie) {
		t.Helper()
		jar.SetCookies(&url.URL{Scheme: "https", Host: host}, []*http.Cookie{c})
	}

	// host-only：只对各自 host 生效。
	set("wpa.chat.zalo.me", &http.Cookie{Name: "wpa", Value: "1", Path: "/"})
	set("wpa.chat.zalo.me", &http.Cookie{Name: "priority", Value: "wpa", Path: "/"})
	set("chat.zalo.me", &http.Cookie{Name: "chat", Value: "1", Path: "/"})
	set("chat.zalo.me", &http.Cookie{Name: "priority", Value: "chat", Path: "/"})
	set("id.zalo.me", &http.Cookie{Name: "id", Value: "1", Path: "/"})
	set("zalo.me", &http.Cookie{Name: "root", Value: "1", Path: "/"})
	// 域 cookie：对全部子域生效，按优先级应在第一次探测（wpa）就被收进来。
	set("zalo.me", &http.Cookie{Name: "dom", Value: "1", Path: "/", Domain: ".zalo.me"})

	cookies := collectZACookies(jar)
	index := make(map[string]int, len(cookies))
	for i, c := range cookies {
		index[c.Name] = i
		if c.Name == "priority" && c.Value != "wpa" {
			t.Fatal("同名 cookie 应保留 wpa.chat.zalo.me 的那一份")
		}
	}
	if len(cookies) != 6 {
		t.Fatalf("收集到的 cookie 数=%d，期望 6（dom 只出现一次）", len(cookies))
	}
	for _, name := range []string{"wpa", "priority", "chat", "id", "root", "dom"} {
		if _, ok := index[name]; !ok {
			t.Fatalf("缺少预期 cookie %q", name)
		}
	}
	if !(index["wpa"] < index["chat"] && index["chat"] < index["id"] && index["id"] < index["root"]) {
		t.Fatal("收集顺序应为 wpa → chat → id → zalo.me")
	}
	if index["dom"] > index["chat"] {
		t.Fatal("Domain=zalo.me 的 cookie 应在第一次探测（wpa）就收集到")
	}

	if got := collectZACookies(nil); got != nil {
		t.Fatal("nil jar 应返回 nil")
	}
}

// TestZANewZAImei IMEI 形状：UUID v4 + "-" + md5(UA)；两次生成不重复。
func TestZANewZAImei(t *testing.T) {
	t.Parallel()

	const ua = "ua-for-imei/1.0"
	first, err := newZAImei(ua)
	if err != nil {
		t.Fatalf("生成 IMEI 失败: %v", err)
	}
	if !zaImeiShape.MatchString(first) {
		t.Fatalf("IMEI 形状不符: %q", first)
	}
	sum := md5.Sum([]byte(ua))
	if !strings.HasSuffix(first, "-"+hex.EncodeToString(sum[:])) {
		t.Fatal("IMEI 的哈希后缀应等于 md5(UA)")
	}

	second, err := newZAImei(ua)
	if err != nil {
		t.Fatalf("第二次生成 IMEI 失败: %v", err)
	}
	if first == second {
		t.Fatal("两次生成的 IMEI 不应相同")
	}

	other, err := newZAImei("ua-for-imei/2.0")
	if err != nil {
		t.Fatalf("换 UA 生成 IMEI 失败: %v", err)
	}
	sum2 := md5.Sum([]byte("ua-for-imei/2.0"))
	if !strings.HasSuffix(other, "-"+hex.EncodeToString(sum2[:])) {
		t.Fatal("换 UA 后哈希后缀应随 UA 变化")
	}
}

// TestZASafeWrap Error() 只给安全文案，Unwrap() 保留原错误供 errors.Is/As。
func TestZASafeWrap(t *testing.T) {
	t.Parallel()

	marker := errors.New("S3CR3T-raw-cause")
	wrapped := zaSafeWrap("zalo 登录失败", marker)
	if wrapped == nil {
		t.Fatal("非 nil 错误应返回包装")
	}
	if got := wrapped.Error(); got != "zalo 登录失败" {
		t.Fatalf("Error() 应只给安全文案，实得 %q", got)
	}
	if strings.Contains(wrapped.Error(), "S3CR3T") {
		t.Fatal("Error() 泄漏了底层错误文本")
	}
	if !errors.Is(wrapped, marker) {
		t.Fatal("errors.Is 应能看到原始错误")
	}
	var se *zaSafeError
	if !errors.As(wrapped, &se) || !errors.Is(se.Unwrap(), marker) {
		t.Fatal("Unwrap() 应返回原始错误")
	}
	if zaSafeWrap("x", nil) != nil {
		t.Fatal("nil 错误应返回 nil")
	}
}

// TestZASnapshotZACredentials 快照取活会话的 IMEI/UA/Language 与 jar 里的 cookie，
// 并归一化到 zalo.me；任一缺失都拒绝。
func TestZASnapshotZACredentials(t *testing.T) {
	t.Parallel()

	newSnapContext := func(t *testing.T) session.MutableContext {
		t.Helper()
		// 给独立 client，避免 SetCookieJar 改写 http.DefaultClient 的 Jar。
		return session.NewContext(session.WithHTTPClient(&http.Client{}))
	}
	newJarWithCookie := func(t *testing.T) http.CookieJar {
		t.Helper()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("建 jar 失败: %v", err)
		}
		jar.SetCookies(&url.URL{Scheme: "https", Host: "chat.zalo.me"},
			[]*http.Cookie{{Name: "sess", Value: "v", Path: "/"}})
		return jar
	}

	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		sc := newSnapContext(t)
		sc.SetIMEI("imei-snap")
		sc.SetUserAgent("ua-snap/1.0")
		sc.SetLanguage("en")
		sc.SetCookieJar(newJarWithCookie(t))

		creds, err := snapshotZACredentials(sc)
		if err != nil {
			t.Fatalf("快照失败: %v", err)
		}
		if creds.IMEI != "imei-snap" || creds.UserAgent != "ua-snap/1.0" {
			t.Fatal("快照应取会话当前的 IMEI/UA")
		}
		if creds.Language == nil || *creds.Language != "en" {
			t.Fatal("快照应取会话当前的 language")
		}
		cookies := creds.Cookie.GetCookies()
		if len(cookies) != 1 {
			t.Fatalf("cookie 数=%d，期望 1", len(cookies))
		}
		if cookies[0].Name != "sess" {
			t.Fatalf("cookie 名=%q，期望 sess", cookies[0].Name)
		}
		zaAssertNormalizedCookie(t, cookies[0])
	})

	t.Run("no_cookie", func(t *testing.T) {
		t.Parallel()
		sc := newSnapContext(t)
		sc.SetIMEI("imei-snap")
		sc.SetUserAgent("ua-snap/1.0")
		sc.SetCookieJar(nil)

		_, err := snapshotZACredentials(sc)
		if !errors.Is(err, ErrZaloBadCredentials) {
			t.Fatalf("无 cookie 应拒绝，实得 %v", err)
		}
	})

	t.Run("no_imei", func(t *testing.T) {
		t.Parallel()
		sc := newSnapContext(t)
		sc.SetUserAgent("ua-snap/1.0")
		sc.SetCookieJar(newJarWithCookie(t))

		_, err := snapshotZACredentials(sc)
		if !errors.Is(err, ErrZaloBadCredentials) {
			t.Fatalf("无 IMEI 应拒绝，实得 %v", err)
		}
	})

	t.Run("no_user_agent", func(t *testing.T) {
		t.Parallel()
		sc := newSnapContext(t)
		sc.SetIMEI("imei-snap")
		sc.SetCookieJar(newJarWithCookie(t))

		_, err := snapshotZACredentials(sc)
		if !errors.Is(err, ErrZaloBadCredentials) {
			t.Fatalf("无 UA 应拒绝，实得 %v", err)
		}
	})

	t.Run("nil_context", func(t *testing.T) {
		t.Parallel()
		if _, err := snapshotZACredentials(nil); !errors.Is(err, ErrZaloBadCredentials) {
			t.Fatalf("nil 会话应拒绝，实得 %v", err)
		}
	})
}
