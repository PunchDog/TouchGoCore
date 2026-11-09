package zalo

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amrakk/zcago"
	zcagoapi "github.com/amrakk/zcago/api"
	zcagoconfig "github.com/amrakk/zcago/config"
	zcagoerrs "github.com/amrakk/zcago/errs"
	"github.com/amrakk/zcago/model"
	"github.com/amrakk/zcago/session"
	"github.com/amrakk/zcago/session/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
)

// wsZaloQREnv 是一套端到端测试现场：假 manager + 阻塞的假 QR SDK + httptest 服务。
type wsZaloQREnv struct {
	mgr     *zaManager
	sdk     *pairFakeSDK
	session *QRSession
	url     string // ws://.../zalo/qr/<sid>
	srv     *httptest.Server
}

// wsStartZaloQREnv 装配现场，并把 web.allow_origins 快照写进运行配置（CheckOrigin 从这里读）。
func wsStartZaloQREnv(t *testing.T, allowOrigins []string) *wsZaloQREnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	sdk := &pairFakeSDK{block: true}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	sess := pairStart(t, m, "acct")

	setRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Web: &config.WebConfig{AllowOrigins: allowOrigins},
	}))
	t.Cleanup(func() { setRunCtx(context.Background()) })

	engine := gin.New()
	r := &zaQRRouter{}
	engine.GET(zaQRWSPath, r.Stream)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	return &wsZaloQREnv{
		mgr:     m,
		sdk:     sdk,
		session: sess,
		url:     "ws" + strings.TrimPrefix(srv.URL, "http") + "/zalo/qr/" + sess.ID,
		srv:     srv,
	}
}

// dial 带指定 Origin 头拨号；origin 为空则不带头。
func (e *wsZaloQREnv) dial(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	var hdr http.Header
	if origin != "" {
		hdr = http.Header{"Origin": []string{origin}}
	}
	return websocket.DefaultDialer.Dial(e.url, hdr)
}

// wsWaitSubs 等到 WS handler 完成订阅（handler 先订阅再升级）。
func wsWaitSubs(t *testing.T, s *QRSession) {
	t.Helper()
	mgrWaitTrue(t, "WS 订阅登记", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.subs) > 0
	})
}

// TestZAZaloQRWSAllowedOriginReceivesFrames 白名单内的 Origin：握手成功，二维码与
// 终局事件逐帧送达，会话结束后服务端按 1000 正常关闭。
func TestZAZaloQRWSAllowedOriginReceivesFrames(t *testing.T) {
	env := wsStartZaloQREnv(t, []string{"https://ok.example"})
	conn, _, err := env.dial(t, "https://ok.example")
	if err != nil {
		t.Fatalf("白名单 Origin 握手失败: %v", err)
	}
	defer conn.Close()
	wsWaitSubs(t, env.session)

	env.session.emit(QREvent{Event: qrEventQR, QRCode: "QR-WS-1", ImageBase64: "aGVsbG8="})
	var ev QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("读取二维码帧失败: %v", err)
	}
	if ev.Event != qrEventQR || ev.QRCode != "QR-WS-1" || ev.SessionID != env.session.ID {
		t.Fatalf("帧内容不符: %+v", ev)
	}

	// 终局：success 之后会话关闭订阅，服务端应当写 close(1000)。
	env.session.emitTerminal(QREvent{Event: qrEventSuccess, UID: "uid-ws"})
	var last QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&last); err != nil {
		t.Fatalf("读取成功帧失败: %v", err)
	}
	if last.Event != qrEventSuccess || last.UID != "uid-ws" || last.SessionID != env.session.ID {
		t.Fatalf("成功帧不符: %+v", last)
	}
	_, _, err = conn.ReadMessage()
	var ce *websocket.CloseError
	if err == nil || !errors.As(err, &ce) || ce.Code != websocket.CloseNormalClosure {
		t.Fatalf("终局后应当收到 1000 正常关闭，实得 err=%v", err)
	}
}

// TestZAZaloQRWSOriginPolicy 跨域白名单三档裁决：
//   - 白名单外拒 403：二维码等于登录凭据，写 return true 会让任何网页连进来偷码；
//   - 不带 Origin 放行：原生客户端/服务间调用不发 Origin，浏览器跨站握手必带；
//   - 白名单显式 "*" 放行任意 Origin。
func TestZAZaloQRWSOriginPolicy(t *testing.T) {
	cases := []struct {
		name   string
		allow  []string
		origin string
		wantOK bool
	}{
		{"白名单外拒绝", []string{"https://ok.example"}, "https://evil.example", false},
		{"不带 Origin 放行", nil, "", true},
		{"通配放行任意 Origin", []string{"*"}, "https://anywhere.example", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := wsStartZaloQREnv(t, c.allow)
			conn, resp, err := env.dial(t, c.origin)
			if c.wantOK {
				if err != nil {
					t.Fatalf("Origin %q 应当放行: %v", c.origin, err)
				}
				conn.Close()
				return
			}
			if err == nil {
				conn.Close()
				t.Fatal("白名单外 Origin 竟然握手成功")
			}
			if resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("拒绝应当是 403，实得 resp=%v err=%v", resp, err)
			}
		})
	}
}

// TestZAZaloQRWSUnknownSession404 查无会话按 404 JSON 拒绝，且响应体干净
// （不被框架 sendResponse 的 "success" 追加污染——hijackSafeWriter 的行为断言）。
func TestZAZaloQRWSUnknownSession404(t *testing.T) {
	env := wsStartZaloQREnv(t, nil)
	resp, err := http.Get(env.srv.URL + "/zalo/qr/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("查无会话应当 404，实得 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("404 响应不是合法 JSON（可能被 sendResponse 追加污染）: %v", err)
	}
	if body["error"] != "session not found" {
		t.Fatalf("404 响应体不符: %v", body)
	}
}

// TestZAZaloQRWSManagerDown503 通道未启动时端点报 503 而不是 panic。
func TestZAZaloQRWSManagerDown503(t *testing.T) {
	env := wsStartZaloQREnv(t, nil)
	zaMgr.Store(nil) // 模拟未启动；t.Cleanup 会恢复旧值，无副作用
	resp, err := http.Get(env.srv.URL + "/zalo/qr/" + env.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未启动应当 503，实得 %d", resp.StatusCode)
	}
}

var (
	// errAccStoreBoom 是写库失败的注入错误，用于断言 errors.Is 能穿过安全包装。
	errAccStoreBoom = errors.New("acc store boom")
	// errAccSnapBoom 是凭证快照失败的注入错误。
	errAccSnapBoom = errors.New("acc snapshot boom")
)

// accReadExampleCfg 读取仓库随附的 config/example.json 并反序列化成 Cfg。
func accReadExampleCfg(t *testing.T) *config.Cfg {
	t.Helper()
	path := filepath.Join("..", "config", "example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var cfg config.Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", path, err)
	}
	return &cfg
}

// TestZAAcceptanceExampleJSONZaloDisabledByDefault 钉住示例配置不默认开启 Zalo：
// 非官方协议客户端默认不连 Zalo，示例若写成 on，等于教出一批照抄即上线的部署。
// 同时要求 sessions 段在示例里出现（否则用户不知道有这组限频键）。
func TestZAAcceptanceExampleJSONZaloDisabledByDefault(t *testing.T) {
	cfg := accReadExampleCfg(t)
	if cfg.Zalo == nil {
		t.Fatal(`example.json 缺少 "zalo" 配置块`)
	}
	if cfg.Zalo.Sessions == nil {
		t.Fatal(`example.json 缺少 "zalo"."sessions" 配置段：限频键没有可照抄的形态`)
	}
	if cfg.Zalo.Enabled() {
		t.Fatalf(`example.json 的 zalo.enable=%q：示例不得默认开启非官方协议客户端`, cfg.Zalo.Enable)
	}

	// 反证：同一份配置把 enable 改成 on 后判定必须翻转，证明上面的断言不是恒真。
	cfg.Zalo.Enable = "on"
	if !cfg.Zalo.Enabled() {
		t.Fatal("zalo.enable=on 时 Enabled() 仍为 false，示例断言失去判别力")
	}
}

// TestZAAcceptanceEnsureConnStoreFailureNoInstall 覆盖按需登录链路的失败不安装：
// 写库失败与凭证快照失败都必须返回错误、不安装 conn、不再写库。
// 既有 TestZAManagerEnsureConnStoreErrors 只覆盖 store.Get 失败，这两条是缺口。
func TestZAAcceptanceEnsureConnStoreFailureNoInstall(t *testing.T) {
	cases := []struct {
		name string
		// prep 在 seed 之后注入故障。
		prep     func(store *fakeZAStore, sdk *fakeZASDK)
		wantErr  error
		wantText string
		// wantUpserts 是本轮到达 store.Upsert 的次数：写库失败用例会尝试一次
		// （随后失败），快照失败用例一次都不该到达写库。
		wantUpserts int
	}{
		{
			name: "upsert_failure",
			prep: func(store *fakeZAStore, _ *fakeZASDK) {
				store.upsertErr = errAccStoreBoom
			},
			wantErr:     errAccStoreBoom,
			wantText:    "zalo 会话凭证写库失败",
			wantUpserts: 1,
		},
		{
			name: "snapshot_failure",
			prep: func(_ *fakeZAStore, sdk *fakeZASDK) {
				sdk.factory = func(zcago.Credentials) *fakeZAConn {
					return &fakeZAConn{uid: "uid-live", snapErr: errAccSnapBoom}
				}
			},
			wantErr:     errAccSnapBoom,
			wantText:    "zalo 会话快照失败，请重新扫码或导入",
			wantUpserts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeZAStore()
			store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
			sdk := newFakeZASDK()
			tc.prep(store, sdk)
			m := mgrNewTestManager(t, store, sdk, nil)

			acc, err := m.accountOrCreate("acct")
			if err != nil {
				t.Fatalf("accountOrCreate: %v", err)
			}
			conn, err := acc.ensureConn(context.Background())
			if conn != nil {
				t.Fatal("失败路径仍返回了 conn")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ensureConn err=%v，期望 errors.Is(%v)", err, tc.wantErr)
			}
			if err.Error() != tc.wantText {
				t.Fatalf("错误文案=%q，期望固定安全文案 %q（原始错误只进 Unwrap）", err.Error(), tc.wantText)
			}
			if acc.currentConn() != nil {
				t.Fatal("失败路径在账号上安装了 conn")
			}
			if got := store.upsertCount(); got != tc.wantUpserts {
				t.Fatalf("store.Upsert 到达 %d 次，期望 %d", got, tc.wantUpserts)
			}
			// 写库失败不得改写已有行：库里仍是 seed 的那份。
			if rec, ok := store.record("acct"); !ok || rec.UID != "uid-seed" {
				t.Fatalf("失败路径改写了会话行: %+v ok=%v", rec, ok)
			}
		})
	}
}

// TestZAAcceptanceLoginWithCookieUpsertFailureNoInstall 钉住 cookie 导入的写库语义：
// Upsert 失败按导入失败处理，不安装 conn、不把账号标成在线。
// 既有 TestZAManagerLoginWithCookie 只覆盖成功路径，这条是缺口。
func TestZAAcceptanceLoginWithCookieUpsertFailureNoInstall(t *testing.T) {
	store := newFakeZAStore()
	store.upsertErr = errAccStoreBoom
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	raw := []byte(`[{"name":"zpw_sek","value":"not-a-real-token","domain":"zalo.me"}]`)
	info, err := m.LoginWithCookie(context.Background(), "acct", raw)
	if info != nil {
		t.Fatalf("写库失败仍返回了账号信息: %+v", info)
	}
	if !errors.Is(err, errAccStoreBoom) {
		t.Fatalf("LoginWithCookie err=%v，期望 errors.Is(%v)", err, errAccStoreBoom)
	}
	if err.Error() != "zalo 会话凭证写库失败" {
		t.Fatalf("错误文案=%q，期望固定安全文案", err.Error())
	}
	if got := sdk.loginCount(); got != 1 {
		t.Fatalf("登录调用 %d 次，期望 1", got)
	}
	if got := store.upsertCount(); got != 1 {
		t.Fatalf("写库调用 %d 次，期望 1", got)
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("写库失败却留下了会话行")
	}
	if acc, ok := m.account("acct"); ok && acc.currentConn() != nil {
		t.Fatal("写库失败仍安装了 conn")
	}
}

// TestZAAcceptanceEnsureConnCorruptStoredCredentials 覆盖库里的凭证 JSON 损坏：
// 必须拒绝登录（不打 SDK、不写库、不安装），而不是拿零值凭证继续。
func TestZAAcceptanceEnsureConnCorruptStoredCredentials(t *testing.T) {
	store := newFakeZAStore()
	// 截断的 JSON：既不是完整 Credentials，也不能被当成空凭证继续登录。
	store.seed("acct", "uid-seed", []byte(`{"imei":"MUST_NOT_LEAK"`))
	sdk := newFakeZASDK()
	m := mgrNewTestManager(t, store, sdk, nil)

	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	conn, err := acc.ensureConn(context.Background())
	if conn != nil {
		t.Fatal("凭证损坏仍返回了 conn")
	}
	if err == nil {
		t.Fatal("凭证损坏应返回错误")
	}
	if err.Error() != "zalo 会话凭证损坏，请重新扫码或导入" {
		t.Fatalf("错误文案=%q，期望固定安全文案", err.Error())
	}
	if got := sdk.loginCount(); got != 0 {
		t.Fatalf("凭证损坏仍调了 SDK 登录 %d 次", got)
	}
	if got := store.upsertCount(); got != 0 {
		t.Fatalf("凭证损坏仍写了 %d 次库", got)
	}
	if acc.currentConn() != nil {
		t.Fatal("凭证损坏仍安装了 conn")
	}
}

// 本文件只测 za_cache.go 的缓存/单飞/限频原语，全部使用假时钟与假 fetch，
// 不连网；TTL 与间隔断言只依赖 clock.Advance，不读真实时间。

var errCacheBoom = errors.New("cache fetch boom")

// cacheClock 是可控假时钟：Now 返回内部时间，Sleep 只记录时长并立即返回。
type cacheClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

// newCacheClock 从固定时刻起步，保证测试可复现。
func newCacheClock() *cacheClock {
	return &cacheClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now 返回内部时间。
func (c *cacheClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep 记录本次睡眠时长，不真正阻塞。
func (c *cacheClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return nil
}

// Advance 推进内部时间。
func (c *cacheClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Sleeps 返回已记录的睡眠时长副本。
func (c *cacheClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// cacheBarrierClock 在 Now 被调用 target 次后关闭 all，用于确定性地拉起并发等待者，
// 避免用真实时间猜调度。Sleep 不会被单飞测试触发。
type cacheBarrierClock struct {
	nowMu sync.Mutex
	now   time.Time

	mu     sync.Mutex
	calls  int
	target int
	once   sync.Once
	all    chan struct{}
}

// newCacheBarrierClock 构造 target 次 Now 即放行的假时钟。
func newCacheBarrierClock(target int) *cacheBarrierClock {
	return &cacheBarrierClock{
		now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		target: target,
		all:    make(chan struct{}),
	}
}

// Now 返回内部时间，并在达到 target 次调用时放行。
func (c *cacheBarrierClock) Now() time.Time {
	c.nowMu.Lock()
	t := c.now
	c.nowMu.Unlock()

	c.mu.Lock()
	c.calls++
	reached := c.calls >= c.target
	c.mu.Unlock()
	if reached {
		c.once.Do(func() { close(c.all) })
	}
	return t
}

// Sleep 只返回 nil：本类型只服务于不需要等闸的用例。
func (c *cacheBarrierClock) Sleep(ctx context.Context, d time.Duration) error { return nil }

// TestZACacheFirstFetchAndTTLHit 覆盖首次拉取、TTL 内命中不拉取、返回值与缓存隔离。
func TestZACacheFirstFetchAndTTLHit(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: "u1", DisplayName: "Alice"}}, nil
	}

	var c zaCache[UserInfo]
	got, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u1" || got[0].DisplayName != "Alice" {
		t.Fatalf("首次 List 结果不符: %+v", got)
	}

	// 调用方改返回值不得污染缓存。
	got[0].UserID = "mutated"
	clock.Advance(4 * time.Minute)
	hit, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("TTL 内 List 失败: %v", err)
	}
	if len(hit) != 1 || hit[0].UserID != "u1" {
		t.Fatalf("TTL 内命中被返回值污染: %+v", hit)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("TTL 内命中不应再 fetch，实际 fetch %d 次", n)
	}
}

// TestZACacheTTLExpiryRefetch 覆盖 TTL 过期后重新拉取。
func TestZACacheTTLExpiryRefetch(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		n := atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: fmt.Sprintf("u%d", n)}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	clock.Advance(6 * time.Minute)
	got, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("过期后 List 失败: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u2" {
		t.Fatalf("过期后应拿到新数据，实际 %+v", got)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望 fetch 2 次，实际 %d 次", n)
	}
}

// TestZACacheMinIntervalGate 覆盖过期 List 与 Refresh 在 minInterval 内都被拒绝，
// 且冷却结束后恢复可拉取。
func TestZACacheMinIntervalGate(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}

	clock.Advance(2 * time.Minute) // 缓存已过期，但距上次拉取只有 2 分钟
	if _, err := c.List(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("过期 List 应被 minInterval 拒绝，实际 err=%v", err)
	}
	if _, err := c.Refresh(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("Refresh 应被 minInterval 拒绝，实际 err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("被拒绝期间不得 fetch，实际 %d 次", n)
	}

	clock.Advance(3 * time.Minute) // 累计 5 分钟，闸放开
	if _, err := c.List(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("冷却结束后 List 应成功: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望 fetch 2 次，实际 %d 次", n)
	}
}

// TestZACacheRefreshBypassesCache 覆盖 Refresh 跳过 TTL 命中判断。
func TestZACacheRefreshBypassesCache(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		n := atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: fmt.Sprintf("u%d", n)}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	refreshed, err := c.Refresh(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if len(refreshed) != 1 || refreshed[0].UserID != "u2" {
		t.Fatalf("Refresh 应绕过缓存拿到新数据，实际 %+v", refreshed)
	}
	hit, err := c.List(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("Refresh 后 List 失败: %v", err)
	}
	if len(hit) != 1 || hit[0].UserID != "u2" {
		t.Fatalf("Refresh 后 List 应命中新缓存，实际 %+v", hit)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望 fetch 2 次，实际 %d 次", n)
	}
}

// TestZACacheConcurrentSingleFlight 覆盖 20 个并发调用合并为一次 fetch。
// barrier clock 保证所有调用都已进入 List 后才放行 leader 的 fetch。
func TestZACacheConcurrentSingleFlight(t *testing.T) {
	const callers = 20
	clock := newCacheBarrierClock(callers)
	release := make(chan struct{})
	var fetchCalls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&fetchCalls, 1)
		<-release
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	results := make(chan []UserInfo, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.List(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos)
			errs <- err
			results <- got
		}()
	}

	select {
	case <-clock.all:
	case <-time.After(10 * time.Second):
		t.Fatal("等待 20 个调用进入 List 超时")
	}
	close(release)
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		if err != nil {
			t.Fatalf("并发 List 返回错误: %v", err)
		}
	}
	for got := range results {
		if len(got) != 1 || got[0].UserID != "u1" {
			t.Fatalf("并发 List 结果不符: %+v", got)
		}
	}
	if n := atomic.LoadInt32(&fetchCalls); n != 1 {
		t.Fatalf("20 并发应合并为 1 次 fetch，实际 %d 次", n)
	}
}

// TestZACacheFetchFailureKeepsCache 覆盖失败保留旧缓存、失败写 lastAttempt 后的冷却。
func TestZACacheFetchFailureKeepsCache(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fail := false
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&calls, 1)
		if fail {
			return nil, errCacheBoom
		}
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}

	clock.Advance(6 * time.Minute) // 超过 minInterval，Refresh 可真正发起
	fail = true
	if _, err := c.Refresh(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, errCacheBoom) {
		t.Fatalf("Refresh 失败应返回原错误，实际 %v", err)
	}
	// 旧缓存仍在且仍在 TTL 内：List 直接返回它，不再打接口。
	got, err := c.List(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("失败后 List 应返回旧缓存: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u1" {
		t.Fatalf("失败后旧缓存丢失: %+v", got)
	}
	// 失败也写了 lastAttempt：立刻再 Refresh 必须被冷却闸拒绝。
	if _, err := c.Refresh(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("失败后冷却应返回 ErrZaloFetchTooSoon，实际 %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("冷却期间不得 fetch，实际 %d 次", n)
	}

	clock.Advance(5 * time.Minute)
	fail = false
	if _, err := c.Refresh(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("冷却结束后 Refresh 应成功: %v", err)
	}
}

// TestZACacheWaiterContextCancel 覆盖等待者 ctx 取消只影响自己，leader 继续完成。
func TestZACacheWaiterContextCancel(t *testing.T) {
	clock := newCacheClock()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var fetchCalls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&fetchCalls, 1)
		once.Do(func() { close(started) })
		<-release
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.List(context.Background(), time.Minute, 0, clock, fetch, cloneUserInfos)
		leaderDone <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := c.List(ctx, time.Minute, 0, clock, fetch, cloneUserInfos)
		waiterDone <- err
	}()
	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("等待者应返回 context.Canceled，实际 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("等待者未在 ctx 取消后返回")
	}

	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader 不应被等待者取消影响: %v", err)
	}
	if n := atomic.LoadInt32(&fetchCalls); n != 1 {
		t.Fatalf("期望 fetch 1 次，实际 %d 次", n)
	}
}

// TestZACacheRequiresClone 覆盖 clone/fetch 缺失时的编程错误。
func TestZACacheRequiresClone(t *testing.T) {
	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), time.Minute, 0, newCacheClock(), nil, cloneUserInfos); !errors.Is(err, errZACacheNoFetch) {
		t.Fatalf("缺 fetch 应报错，实际 %v", err)
	}
	fetch := func(context.Context) ([]UserInfo, error) { return nil, nil }
	if _, err := c.List(context.Background(), time.Minute, 0, newCacheClock(), fetch, nil); !errors.Is(err, errZACacheNoClone) {
		t.Fatalf("缺 clone 应报错，实际 %v", err)
	}
}

// infoFetchRecorder 记录用户/群资料请求的批次。
type infoFetchRecorder struct {
	mu      sync.Mutex
	batches [][]string
}

// record 保存一批请求 ID 的副本。
func (r *infoFetchRecorder) record(ids []string) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]string(nil), ids...))
	r.mu.Unlock()
}

// Batches 返回已记录批次的副本。
func (r *infoFetchRecorder) Batches() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.batches))
	for i, b := range r.batches {
		out[i] = append([]string(nil), b...)
	}
	return out
}

// Calls 返回已记录的请求次数。
func (r *infoFetchRecorder) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

// infoUsersFor 为每个 ID 造一条用户资料。
func infoUsersFor(ids []string) []UserInfo {
	out := make([]UserInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, UserInfo{UserID: id, DisplayName: "dn-" + id})
	}
	return out
}

// infoGroupsFor 为每个 ID 造一条群资料。
func infoGroupsFor(ids []string) []GroupInfo {
	out := make([]GroupInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, GroupInfo{GroupID: id, Name: "gn-" + id})
	}
	return out
}

// TestZAInfoCacheBatchesAndThrottle 覆盖 120 个 ID 分 50/50/20 三批、批间 1.5 秒、
// 命中批次不再等闸。
func TestZAInfoCacheBatchesAndThrottle(t *testing.T) {
	clock := newCacheClock()
	ids := make([]string, 120)
	for i := range ids {
		ids[i] = fmt.Sprintf("u%03d", i)
	}
	rec := &infoFetchRecorder{}
	fetch := func(_ context.Context, batch []string) ([]UserInfo, error) {
		rec.record(batch)
		return infoUsersFor(batch), nil
	}

	c := newZAInfoCache()
	got, err := c.Users(context.Background(), ids, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("Users 失败: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("应返回 120 条资料，实际 %d", len(got))
	}

	batches := rec.Batches()
	if len(batches) != 3 {
		t.Fatalf("应分 3 批请求，实际 %d 批: %v", len(batches), batches)
	}
	for i, want := range []int{50, 50, 20} {
		if len(batches[i]) != want {
			t.Fatalf("第 %d 批应为 %d 个 ID，实际 %d", i+1, want, len(batches[i]))
		}
	}

	sleeps := clock.Sleeps()
	if len(sleeps) != 2 {
		t.Fatalf("批间应等闸 2 次，实际 %d 次: %v", len(sleeps), sleeps)
	}
	for i, d := range sleeps {
		if d != 1500*time.Millisecond {
			t.Fatalf("第 %d 次等闸应为 1.5s，实际 %s", i+1, d)
		}
	}

	// 全部命中：不请求、不等闸。
	again, err := c.Users(context.Background(), ids, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("缓存命中 Users 失败: %v", err)
	}
	if len(again) != 120 {
		t.Fatalf("缓存命中应返回 120 条，实际 %d", len(again))
	}
	if rec.Calls() != 3 {
		t.Fatalf("缓存命中不应再请求，实际共 %d 批", rec.Calls())
	}
	if extra := clock.Sleeps(); len(extra) != 2 {
		t.Fatalf("缓存命中不应等闸，实际睡眠记录 %v", extra)
	}
}

// TestZAInfoCacheUsersGroupsSharedThrottle 覆盖 users 与 groups 共用同一把 throttle。
func TestZAInfoCacheUsersGroupsSharedThrottle(t *testing.T) {
	clock := newCacheClock()
	var userCalls, groupCalls int32
	users := func(_ context.Context, batch []string) ([]UserInfo, error) {
		atomic.AddInt32(&userCalls, 1)
		return infoUsersFor(batch), nil
	}
	groups := func(_ context.Context, batch []string) ([]GroupInfo, error) {
		atomic.AddInt32(&groupCalls, 1)
		return infoGroupsFor(batch), nil
	}

	c := newZAInfoCache()
	if _, err := c.Users(context.Background(), []string{"u1"}, 10*time.Minute, 1500*time.Millisecond, clock, users); err != nil {
		t.Fatalf("Users 失败: %v", err)
	}
	if sleeps := clock.Sleeps(); len(sleeps) != 0 {
		t.Fatalf("首个请求不应等闸，实际 %v", sleeps)
	}

	// 紧接着的群资料请求必须等同一个闸。
	if _, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, groups); err != nil {
		t.Fatalf("Groups 失败: %v", err)
	}
	sleeps := clock.Sleeps()
	if len(sleeps) != 1 || sleeps[0] != 1500*time.Millisecond {
		t.Fatalf("群资料应等闸 1.5s，实际 %v", sleeps)
	}

	// 两边都命中缓存：不再请求、不再等闸。
	if _, err := c.Users(context.Background(), []string{"u1"}, 10*time.Minute, 1500*time.Millisecond, clock, users); err != nil {
		t.Fatalf("Users 命中失败: %v", err)
	}
	if _, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, groups); err != nil {
		t.Fatalf("Groups 命中失败: %v", err)
	}
	if extra := clock.Sleeps(); len(extra) != 1 {
		t.Fatalf("缓存命中不应等闸，实际 %v", extra)
	}
	if n := atomic.LoadInt32(&userCalls); n != 1 {
		t.Fatalf("Users 应只请求 1 次，实际 %d", n)
	}
	if n := atomic.LoadInt32(&groupCalls); n != 1 {
		t.Fatalf("Groups 应只请求 1 次，实际 %d", n)
	}
}

// TestZAInfoCacheMissingIDs 覆盖缺失 ID 只请求一次、返回条目不伪造、缺失项不缓存。
func TestZAInfoCacheMissingIDs(t *testing.T) {
	clock := newCacheClock()
	rec := &infoFetchRecorder{}
	stage := "a"
	fetch := func(_ context.Context, batch []string) ([]UserInfo, error) {
		rec.record(batch)
		if stage == "a" {
			return []UserInfo{{UserID: "ua"}}, nil
		}
		return []UserInfo{{UserID: "ub"}}, nil
	}

	c := newZAInfoCache()
	got, err := c.Users(context.Background(), []string{"ua", "ub"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("首次 Users 失败: %v", err)
	}
	if len(got) != 1 || got["ua"].UserID != "ua" {
		t.Fatalf("只应返回服务端存在的 ua，实际 %+v", got)
	}
	if _, ok := got["ub"]; ok {
		t.Fatal("服务端没返回的 ub 不得伪造")
	}

	clock.Advance(1500 * time.Millisecond)
	stage = "b"
	got, err = c.Users(context.Background(), []string{"ua", "ub"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("第二次 Users 失败: %v", err)
	}
	if len(got) != 2 || got["ua"].UserID != "ua" || got["ub"].UserID != "ub" {
		t.Fatalf("第二次应返回 ua+ub，实际 %+v", got)
	}
	batches := rec.Batches()
	if len(batches) != 2 {
		t.Fatalf("应请求 2 批，实际 %d", len(batches))
	}
	if want := []string{"ub"}; !reflect.DeepEqual(batches[1], want) {
		t.Fatalf("第二批只应请求缺失的 ub，实际 %v", batches[1])
	}

	// 两个 ID 都已缓存：不再请求。
	if _, err := c.Users(context.Background(), []string{"ua", "ub"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch); err != nil {
		t.Fatalf("第三次 Users 失败: %v", err)
	}
	if rec.Calls() != 2 {
		t.Fatalf("全命中不应再请求，实际 %d 批", rec.Calls())
	}
}

// TestZAInfoCacheGroupsDeletion 覆盖服务端没返回的群 ID 从缓存删除、全缺失返回空 map。
func TestZAInfoCacheGroupsDeletion(t *testing.T) {
	clock := newCacheClock()
	c := newZAInfoCache()
	c.storeGroups([]string{"g1"}, []GroupInfo{{GroupID: "g1", Name: "old"}}, clock.Now())

	clock.Advance(11 * time.Minute) // 缓存已过期
	var requested [][]string
	fetch := func(_ context.Context, batch []string) ([]GroupInfo, error) {
		requested = append(requested, append([]string(nil), batch...))
		return nil, nil
	}

	got, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("Groups 失败: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("全缺失应返回空 map，实际 %#v", got)
	}
	if len(requested) != 1 || !reflect.DeepEqual(requested[0], []string{"g1"}) {
		t.Fatalf("应请求过期群 g1，实际 %v", requested)
	}
	if _, ok := c.CachedGroup("g1", 10*time.Minute, clock.Now()); ok {
		t.Fatal("服务端没返回的群 ID 应从缓存删除")
	}
}

// TestZAInfoCacheCachedDrop 覆盖 CachedGroup/DropGroups、TTL 与深拷贝。
func TestZAInfoCacheCachedDrop(t *testing.T) {
	clock := newCacheClock()
	now := clock.Now()
	c := newZAInfoCache()
	// 直接写入缓存（与 Groups 成功后的写入路径一致）。
	c.storeGroups([]string{"g1"}, []GroupInfo{{GroupID: "g1", Name: "G1", AdminIDs: []string{"a1", "a2"}}}, now)

	g, ok := c.CachedGroup("g1", 10*time.Minute, now)
	if !ok || g.Name != "G1" || !reflect.DeepEqual(g.AdminIDs, []string{"a1", "a2"}) {
		t.Fatalf("写入缓存后应能读到 g1，实际 ok=%v %+v", ok, g)
	}
	g.Name = "mutated"
	g.AdminIDs[0] = "hacked"
	again, ok := c.CachedGroup("g1", 10*time.Minute, now)
	if !ok || again.Name != "G1" || again.AdminIDs[0] != "a1" {
		t.Fatalf("CachedGroup 必须深拷贝，实际 %+v", again)
	}

	if _, ok := c.CachedGroup("g1", 10*time.Minute, now.Add(10*time.Minute)); ok {
		t.Fatal("TTL 到期应返回 false")
	}
	if _, ok := c.CachedGroup("g1", 11*time.Minute, now.Add(10*time.Minute)); !ok {
		t.Fatal("TTL 内应返回 true")
	}
	if _, ok := c.CachedGroup("", 10*time.Minute, now); ok {
		t.Fatal("空 ID 不应命中")
	}

	c.DropGroups([]string{"g1", "", "unknown"})
	if _, ok := c.CachedGroup("g1", 11*time.Minute, now); ok {
		t.Fatal("DropGroups 后不应再命中")
	}
}

// TestZAInfoCacheGroupsResultDeepCopy 覆盖 Groups 返回的群资料与缓存隔离。
func TestZAInfoCacheGroupsResultDeepCopy(t *testing.T) {
	clock := newCacheClock()
	c := newZAInfoCache()
	fetch := func(_ context.Context, batch []string) ([]GroupInfo, error) {
		out := infoGroupsFor(batch)
		for i := range out {
			out[i].AdminIDs = []string{"admin-" + out[i].GroupID}
		}
		return out, nil
	}
	got, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("Groups 失败: %v", err)
	}
	g := got["g1"]
	g.AdminIDs[0] = "hacked"
	cached, ok := c.CachedGroup("g1", 10*time.Minute, clock.Now())
	if !ok || cached.AdminIDs[0] != "admin-g1" {
		t.Fatalf("Groups 返回值不得污染缓存，实际 %+v", cached)
	}
}

// TestZAInfoCacheFetchError 覆盖 user/group 拉取错误原样上抛。
func TestZAInfoCacheFetchError(t *testing.T) {
	clock := newCacheClock()
	c := newZAInfoCache()
	users := func(context.Context, []string) ([]UserInfo, error) { return nil, errCacheBoom }
	if _, err := c.Users(context.Background(), []string{"u1"}, 10*time.Minute, 0, clock, users); !errors.Is(err, errCacheBoom) {
		t.Fatalf("Users 应返回 fetch 错误，实际 %v", err)
	}
	groups := func(context.Context, []string) ([]GroupInfo, error) { return nil, errCacheBoom }
	if _, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 0, clock, groups); !errors.Is(err, errCacheBoom) {
		t.Fatalf("Groups 应返回 fetch 错误，实际 %v", err)
	}
}

// TestZAInfoCacheCloneHelpers 覆盖深拷贝辅助函数与 ID 归一化。
func TestZAInfoCacheCloneHelpers(t *testing.T) {
	if got, want := uniqueNonEmptyIDs([]string{"a", "", "b", "a", "", "c"}), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("uniqueNonEmptyIDs 不符: got %v want %v", got, want)
	}
	if got := uniqueNonEmptyIDs(nil); len(got) != 0 {
		t.Fatalf("nil 输入应返回空切片，实际 %v", got)
	}

	if cloneUserInfos(nil) != nil {
		t.Fatal("cloneUserInfos(nil) 应为 nil")
	}
	srcUsers := []UserInfo{{UserID: "u1"}}
	clonedUsers := cloneUserInfos(srcUsers)
	clonedUsers[0].UserID = "mutated"
	if srcUsers[0].UserID != "u1" {
		t.Fatal("cloneUserInfos 必须复制底层数组")
	}

	if cloneGroupInfos(nil) != nil {
		t.Fatal("cloneGroupInfos(nil) 应为 nil")
	}
	srcGroups := []GroupInfo{
		{GroupID: "g1", AdminIDs: []string{"a1"}},
		{GroupID: "g2"},
	}
	clonedGroups := cloneGroupInfos(srcGroups)
	clonedGroups[0].AdminIDs[0] = "hacked"
	clonedGroups[0].GroupID = "mutated"
	clonedGroups[1].GroupID = "mutated"
	if srcGroups[0].AdminIDs[0] != "a1" || srcGroups[0].GroupID != "g1" || srcGroups[1].GroupID != "g2" {
		t.Fatalf("cloneGroupInfos 必须深拷贝，原值被改动: %+v", srcGroups)
	}

	noAdmins := cloneGroupInfo(GroupInfo{GroupID: "g3"})
	if noAdmins.AdminIDs != nil {
		t.Fatal("nil AdminIDs 应保持 nil")
	}
}

// TestZaloDeleteChatRoutesThread 断言单聊/群聊各走一次 conn.DeleteChat，
// 参数原样透传，OwnerID 留空时回填账号 UID。
func TestZaloDeleteChatRoutesThread(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	ctx := context.Background()

	if err := ZaloDeleteChat(ctx, "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"}); err != nil {
		t.Fatalf("单聊清会话: %v", err)
	}
	if err := ZaloDeleteChat(ctx, "acct", "group-1", true, ChatBoundary{OwnerID: "uid-other", CliMsgID: "c-9", GlobalMsgID: "g-1"}); err != nil {
		t.Fatalf("群聊清会话: %v", err)
	}

	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("清会话后没有 conn")
	}
	calls := conn.recordedDeletes()
	if len(calls) != 2 {
		t.Fatalf("DeleteChat 调用 %d 次，期望 2", len(calls))
	}
	if calls[0].group || calls[0].threadID != "peer-1" {
		t.Fatalf("单聊参数不符: %+v", calls[0])
	}
	if calls[0].boundary.OwnerID != "uid-fac" {
		t.Fatalf("OwnerID 留空应回填账号 UID，实得 %q", calls[0].boundary.OwnerID)
	}
	if calls[0].boundary.CliMsgID != "m-1" || calls[0].boundary.GlobalMsgID != "m-1" {
		t.Fatalf("边界归一后应为 cliMsgId=globalMsgId: %+v", calls[0].boundary)
	}
	if !calls[1].group || calls[1].threadID != "group-1" {
		t.Fatalf("群聊参数不符: %+v", calls[1])
	}
	if calls[1].boundary != (ChatBoundary{OwnerID: "uid-other", CliMsgID: "c-9", GlobalMsgID: "g-1"}) {
		t.Fatalf("调用方给的 boundary 被改写: %+v", calls[1].boundary)
	}
}

// TestZaloDeleteChatRejectsBadInput 覆盖空 threadID 与 conn 错误：
// 前者不该打到 conn，后者必须能 errors.Is 穿包装。
func TestZaloDeleteChatRejectsBadInput(t *testing.T) {
	t.Run("empty_thread", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		err := ZaloDeleteChat(context.Background(), "acct", "  ", false, ChatBoundary{GlobalMsgID: "m-1"})
		if !errors.Is(err, errZAFacadeEmptyThread) {
			t.Fatalf("空 threadID err=%v，期望 errZAFacadeEmptyThread", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recordedDeletes()) != 0 {
			t.Fatalf("空 threadID 仍调了 DeleteChat")
		}
	})

	t.Run("conn_error", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.deleteErr = errFacBoom }
		err := ZaloDeleteChat(context.Background(), "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"})
		if !errors.Is(err, errFacBoom) {
			t.Fatalf("conn 错误 err=%v，期望可 errors.Is(errFacBoom)", err)
		}
	})
}

// TestZaloDeleteChatDoesNotConsumeSendQuota 钉住「清会话不是发消息」：
// 小时配额 1 时，发一条 + 清一次后，第二条仍应被配额拒绝。
func TestZaloDeleteChatDoesNotConsumeSendQuota(t *testing.T) {
	facNewSendFixture(t, 1)
	ctx := context.Background()

	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}
	if err := ZaloDeleteChat(ctx, "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"}); err != nil {
		t.Fatalf("发送后清会话: %v", err)
	}
	if _, err := ZaloSendMessage(ctx, "acct", "peer-2", "hello"); !errors.Is(err, ErrZaloSendLimit) {
		t.Fatalf("清会话后第二条 err=%v，期望 ErrZaloSendLimit（清会话不该占发送配额）", err)
	}
}

// TestZaloDeleteChatFillsBoundary 钉住边界归一（2026-10-27 真实接口核实）：
//   - OwnerID 留空 ⇒ 取当前登录 UID（空 owner 会被上游拒成 114）；
//   - CliMsgID 留空 ⇒ 用 GlobalMsgID 顶替（空 cliMsgId 会被上游拒成 112，
//     整次删除不生效 —— 线上「消息发出去但对话没清」就是这个原因）。
func TestZaloDeleteChatFillsBoundary(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)

	if err := ZaloDeleteChat(context.Background(), "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"}); err != nil {
		t.Fatalf("清会话: %v", err)
	}
	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("清会话后没有 conn")
	}
	calls := conn.recordedDeletes()
	if len(calls) != 1 {
		t.Fatalf("DeleteChat 调用 %d 次，期望 1", len(calls))
	}
	if calls[0].boundary.OwnerID != "uid-fac" {
		t.Fatalf("OwnerID 留空应回填当前登录 UID，实得 %q", calls[0].boundary.OwnerID)
	}
	if calls[0].boundary.CliMsgID != "m-1" {
		t.Fatalf("CliMsgID 留空应回填 GlobalMsgID，实得 %q", calls[0].boundary.CliMsgID)
	}

	// 调用方给了值就不能覆盖。
	if err := ZaloDeleteChat(context.Background(), "acct", "peer-1", false,
		ChatBoundary{OwnerID: "uid-other", CliMsgID: "c-9", GlobalMsgID: "m-2"}); err != nil {
		t.Fatalf("清会话: %v", err)
	}
	calls = conn.recordedDeletes()
	last := calls[len(calls)-1]
	if last.boundary.OwnerID != "uid-other" || last.boundary.CliMsgID != "c-9" {
		t.Fatalf("调用方给的边界被改写: %+v", last.boundary)
	}
}

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

// errFacBoom 是门面测试注入的通用失败，用于断言 errors.Is 能穿过包装。
var errFacBoom = errors.New("facade test boom")

// facRecordedSend 是一次到达 conn.SendText 的发送记录。
type facRecordedSend struct {
	group    bool
	threadID string
	text     string
}

// facRecordedCard 是一次到达 conn.SendCard 的发送记录。
type facRecordedCard struct {
	group    bool
	threadID string
	card     OutboundCard
}

// facConn 在 fakeZAConn 之上覆盖 SendText/SendCard：记录参数，返回可注入的
// msgID/error。其余方法（UID/快照/好友/群/listener）由嵌入的 fakeZAConn 提供。
type facConn struct {
	*fakeZAConn

	recMu   sync.Mutex
	sends   []facRecordedSend
	cards   []facRecordedCard
	msgID   string
	sendErr error
	cardMsg string
	cardErr error
}

var _ zaConn = (*facConn)(nil)

// SendText 记录本次调用的全部参数并返回注入结果。
func (c *facConn) SendText(_ context.Context, threadID string, group bool, text string) (string, error) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.sends = append(c.sends, facRecordedSend{group: group, threadID: threadID, text: text})
	if c.sendErr != nil {
		return "", c.sendErr
	}
	return c.msgID, nil
}

// SendCard 记录本次调用的全部参数并返回注入结果。
func (c *facConn) SendCard(_ context.Context, threadID string, group bool, card OutboundCard) (string, error) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.cards = append(c.cards, facRecordedCard{group: group, threadID: threadID, card: card})
	if c.cardErr != nil {
		return "", c.cardErr
	}
	return c.cardMsg, nil
}

// recordedCards 返回卡片发送记录副本。
func (c *facConn) recordedCards() []facRecordedCard {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	return append([]facRecordedCard(nil), c.cards...)
}

// setCardMsgID 设置 SendCard 返回的 msgID。
func (c *facConn) setCardMsgID(id string) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.cardMsg = id
}

// setCardErr 设置 SendCard 返回的错误（nil 表示恢复正常）。
func (c *facConn) setCardErr(err error) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.cardErr = err
}

// recorded 返回发送记录副本。
func (c *facConn) recorded() []facRecordedSend {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	return append([]facRecordedSend(nil), c.sends...)
}

// setSendMsgID 设置 SendText 返回的 msgID。
func (c *facConn) setSendMsgID(id string) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.msgID = id
}

// setSendErr 设置 SendText 返回的错误（nil 表示恢复正常）。
func (c *facConn) setSendErr(err error) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	c.sendErr = err
}

// setUsers 写入 GetUserInfo 的返回数据；fakeZAConn 只提供 GetUserInfo 读取器，
// 没有对应 setter，这里在 fake 的锁内补齐。
func (c *facConn) setUsers(users map[string]UserInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users = users
}

// userInfoCallCount 返回 GetUserInfo 的请求批次数。
func (c *facConn) userInfoCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.userInfoCalls)
}

// facSDK 是 zaSDK 的假件：每次 Login 返回一个新的 *facConn，可注入登录错误与
// conn 初始化回调（用于预先塞好友/群/资料数据）。
type facSDK struct {
	mu         sync.Mutex
	loginCalls int
	conns      []*facConn
	loginErr   error
	configure  func(*facConn)
}

var _ zaSDK = (*facSDK)(nil)

// Login 记录调用并按注入行为返回新 conn。
func (s *facSDK) Login(_ context.Context, _ zcago.Credentials) (zaConn, error) {
	s.mu.Lock()
	s.loginCalls++
	err := s.loginErr
	configure := s.configure
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c := &facConn{
		fakeZAConn: &fakeZAConn{
			uid:  "uid-fac",
			snap: zcago.Credentials{IMEI: "imei-fac", UserAgent: "ua-fac"},
		},
		msgID:   "msg-fac",
		cardMsg: "card-fac",
	}
	if configure != nil {
		configure(c)
	}
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
	return c, nil
}

// LoginQR 不在本文件使用。
func (s *facSDK) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zaConn, error) {
	return nil, errors.New("facade test: LoginQR 未实现")
}

// loginCount 返回 Login 调用次数。
func (s *facSDK) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginCalls
}

// lastConn 返回最近一次登录得到的 conn；未登录返回 nil。
func (s *facSDK) lastConn() *facConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return nil
	}
	return s.conns[len(s.conns)-1]
}

// facBlockClock 是可在 Sleep 中阻塞的假时钟：block 为 true 时 Sleep 等到 ctx 取消
// 或测试显式 unblock；Now/Advance/Sleeps 复用 cacheClock。
type facBlockClock struct {
	*cacheClock

	mu      sync.Mutex
	block   bool
	entered chan struct{}
}

// newFacBlockClock 构造会阻塞 Sleep 的假时钟。
func newFacBlockClock() *facBlockClock {
	return &facBlockClock{cacheClock: newCacheClock(), block: true, entered: make(chan struct{}, 4)}
}

// Sleep 在 block 为 true 时阻塞，否则退化为 cacheClock 的记录语义。
func (c *facBlockClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	block := c.block
	c.mu.Unlock()
	if !block {
		return c.cacheClock.Sleep(ctx, d)
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("facade test: 等待取消超时")
	}
}

// unblock 让后续 Sleep 立即返回。
func (c *facBlockClock) unblock() {
	c.mu.Lock()
	c.block = false
	c.mu.Unlock()
}

// facNewManager 构造 manager 并登记停机清理；clock 为 nil 时用可控假时钟。
func facNewManager(t *testing.T, limits zaLimits, clock zaClock) (*zaManager, *fakeZAStore, *facSDK) {
	t.Helper()
	if clock == nil {
		clock = newCacheClock()
	}
	store := newFakeZAStore()
	sdk := &facSDK{}
	m := newZAManager(context.Background(), limits, store, sdk, clock, nil)
	t.Cleanup(m.Stop)
	return m, store, sdk
}

// facInstall 把 manager 装进进程级快照，并在测试结束时清空（先于 Stop 执行）。
func facInstall(t *testing.T, m *zaManager) {
	t.Helper()
	zaMgr.Store(m)
	t.Cleanup(func() { zaMgr.Store(nil) })
}

// facNewSendFixture 构造可发送的账号：库里有一行凭证、随机间隔固定为 0，
// maxPerHour>0 时覆盖小时上限。
func facNewSendFixture(t *testing.T, maxPerHour int) (*zaManager, *facSDK, *cacheClock) {
	t.Helper()
	limits := mgrTestLimits()
	if maxPerHour > 0 {
		limits.sendMaxPerHour = maxPerHour
	}
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	m.randDelay = func(time.Duration, time.Duration) time.Duration { return 0 }
	facInstall(t, m)
	return m, sdk, clock
}

// TestZAFacadeNotStarted 断言通道未启动时全部公开入口返回 ErrZaloNotStarted。
func TestZAFacadeNotStarted(t *testing.T) {
	old := zaMgr.Load()
	zaMgr.Store(nil)
	t.Cleanup(func() { zaMgr.Store(old) })

	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{name: "login_with_cookie", call: func() error {
			_, err := ZaloLoginWithCookie(ctx, "acct", []byte("[]"))
			return err
		}},
		{name: "start_client", call: func() error {
			_, err := ZaloStartClient(ctx, "acct")
			return err
		}},
		{name: "delete_account", call: func() error { return ZaloDeleteAccount(ctx, "acct") }},
		{name: "list_accounts", call: func() error {
			_, err := ZaloListAccounts(ctx)
			return err
		}},
		{name: "send_message", call: func() error {
			_, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello")
			return err
		}},
		{name: "send_group_message", call: func() error {
			_, err := ZaloSendGroupMessage(ctx, "acct", "group-1", "hello")
			return err
		}},
		{name: "delete_chat", call: func() error {
			return ZaloDeleteChat(ctx, "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m1"})
		}},
		{name: "list_friends", call: func() error {
			_, err := ZaloListFriends(ctx, "acct")
			return err
		}},
		{name: "sync_friends", call: func() error {
			_, err := ZaloSyncFriends(ctx, "acct")
			return err
		}},
		{name: "list_groups", call: func() error {
			_, err := ZaloListGroups(ctx, "acct")
			return err
		}},
		{name: "sync_groups", call: func() error {
			_, err := ZaloSyncGroups(ctx, "acct")
			return err
		}},
		{name: "get_user_info", call: func() error {
			_, err := ZaloGetUserInfo(ctx, "acct", "u1")
			return err
		}},
		{name: "get_group_info", call: func() error {
			_, err := ZaloGetGroupInfo(ctx, "acct", "g1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrZaloNotStarted) {
				t.Fatalf("未启动 err=%v，期望 ErrZaloNotStarted", err)
			}
		})
	}
}

// TestZAFacadeBadAccountID 断言通道已启动时非法 accountID 一律返回
// ErrZaloBadAccountID，且不触发任何登录/发送。
func TestZAFacadeBadAccountID(t *testing.T) {
	m, store, sdk := facNewManager(t, mgrTestLimits(), nil)
	facInstall(t, m)

	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{name: "login_with_cookie", call: func() error {
			_, err := ZaloLoginWithCookie(ctx, "..", []byte(`[{"name":"a","value":"b","domain":"zalo.me"}]`))
			return err
		}},
		{name: "start_client", call: func() error {
			_, err := ZaloStartClient(ctx, "..")
			return err
		}},
		{name: "delete_account", call: func() error { return ZaloDeleteAccount(ctx, "..") }},
		{name: "send_message", call: func() error {
			_, err := ZaloSendMessage(ctx, "..", "peer-1", "hello")
			return err
		}},
		{name: "send_group_message", call: func() error {
			_, err := ZaloSendGroupMessage(ctx, "..", "group-1", "hello")
			return err
		}},
		{name: "delete_chat", call: func() error {
			return ZaloDeleteChat(ctx, "..", "peer-1", false, ChatBoundary{GlobalMsgID: "m1"})
		}},
		{name: "list_friends", call: func() error {
			_, err := ZaloListFriends(ctx, "..")
			return err
		}},
		{name: "sync_friends", call: func() error {
			_, err := ZaloSyncFriends(ctx, "..")
			return err
		}},
		{name: "list_groups", call: func() error {
			_, err := ZaloListGroups(ctx, "..")
			return err
		}},
		{name: "sync_groups", call: func() error {
			_, err := ZaloSyncGroups(ctx, "..")
			return err
		}},
		{name: "get_user_info", call: func() error {
			_, err := ZaloGetUserInfo(ctx, "..", "u1")
			return err
		}},
		{name: "get_group_info", call: func() error {
			_, err := ZaloGetGroupInfo(ctx, "..", "g1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrZaloBadAccountID) {
				t.Fatalf("非法账号 id err=%v，期望 ErrZaloBadAccountID", err)
			}
		})
	}
	if got := sdk.loginCount(); got != 0 {
		t.Fatalf("非法 id 触发了 %d 次登录", got)
	}
	if got := store.upsertCount(); got != 0 {
		t.Fatalf("非法 id 写了 %d 次库", got)
	}
}

// TestZAFacadeSendRoutesThread 断言单聊/群聊把 group 与 threadID 正确传给
// conn.SendText，并原样返回 msgID。
func TestZAFacadeSendRoutesThread(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	ctx := context.Background()

	id, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello")
	if err != nil {
		t.Fatalf("ZaloSendMessage: %v", err)
	}
	if id != "msg-fac" {
		t.Fatalf("msgID=%q，期望 msg-fac", id)
	}
	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("发送后没有 conn")
	}

	id, err = ZaloSendGroupMessage(ctx, "acct", "group-1", "hi group")
	if err != nil {
		t.Fatalf("ZaloSendGroupMessage: %v", err)
	}
	if id != "msg-fac" {
		t.Fatalf("群聊 msgID=%q，期望 msg-fac", id)
	}

	sends := conn.recorded()
	if len(sends) != 2 {
		t.Fatalf("SendText 调用 %d 次，期望 2", len(sends))
	}
	if sends[0].group {
		t.Fatalf("单聊被标成群聊: %+v", sends[0])
	}
	if sends[0].threadID != "peer-1" || sends[0].text != "hello" {
		t.Fatalf("单聊参数不符: %+v", sends[0])
	}
	if !sends[1].group {
		t.Fatalf("群聊没有标 group: %+v", sends[1])
	}
	if sends[1].threadID != "group-1" || sends[1].text != "hi group" {
		t.Fatalf("群聊参数不符: %+v", sends[1])
	}
}

// TestZAFacadeSendRejectsBadInput 覆盖空正文、空 threadID、空 msgID、conn 错误，
// 并断言错误文案不回显正文。
func TestZAFacadeSendRejectsBadInput(t *testing.T) {
	t.Run("empty_text", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		_, err := ZaloSendMessage(context.Background(), "acct", "peer-1", "  \n\t ")
		if !errors.Is(err, errZAFacadeEmptyText) {
			t.Fatalf("空正文 err=%v，期望 errZAFacadeEmptyText", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recorded()) != 0 {
			t.Fatalf("空正文仍调了 SendText: %+v", conn.recorded())
		}
	})

	t.Run("empty_thread", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		_, err := ZaloSendGroupMessage(context.Background(), "acct", "   ", "hello")
		if !errors.Is(err, errZAFacadeEmptyThread) {
			t.Fatalf("空 threadID err=%v，期望 errZAFacadeEmptyThread", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recorded()) != 0 {
			t.Fatalf("空 threadID 仍调了 SendText: %+v", conn.recorded())
		}
	})

	t.Run("empty_msg_id", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.setSendMsgID("  ") }
		_, err := ZaloSendMessage(context.Background(), "acct", "peer-1", "hello")
		if !errors.Is(err, ErrZaloEmptyMessageID) {
			t.Fatalf("空 msgID err=%v，期望 ErrZaloEmptyMessageID", err)
		}
	})

	t.Run("conn_error_not_echo_body", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.setSendErr(errFacBoom) }
		const body = "S3CRET-BODY-9182"
		_, err := ZaloSendMessage(context.Background(), "acct", "peer-1", body)
		if !errors.Is(err, errFacBoom) {
			t.Fatalf("conn 错误 err=%v，期望可 errors.Is(errFacBoom)", err)
		}
		if strings.Contains(err.Error(), body) {
			t.Fatalf("错误文案回显了正文: %q", err.Error())
		}
	})
}

// TestZAFacadeSendCardRoutesThread 断言单聊/群聊把链接卡片原样交给 conn.SendCard。
func TestZAFacadeSendCardRoutesThread(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	ctx := context.Background()
	card := OutboundCard{
		Text:    "首行\n第二行",
		LinkURL: "https://example.com/jump",
	}

	id, err := ZaloSendCard(ctx, "acct", "peer-1", card)
	if err != nil {
		t.Fatalf("ZaloSendCard: %v", err)
	}
	if id != "card-fac" {
		t.Fatalf("msgID=%q，期望 card-fac", id)
	}

	id, err = ZaloSendGroupCard(ctx, "acct", "group-1", card)
	if err != nil {
		t.Fatalf("ZaloSendGroupCard: %v", err)
	}
	if id != "card-fac" {
		t.Fatalf("群聊 msgID=%q，期望 card-fac", id)
	}

	cards := sdk.lastConn().recordedCards()
	if len(cards) != 2 {
		t.Fatalf("SendCard 调用 %d 次，期望 2", len(cards))
	}
	if cards[0].group || cards[0].threadID != "peer-1" || cards[0].card != card {
		t.Fatalf("单聊参数不符: %+v", cards[0])
	}
	if !cards[1].group || cards[1].threadID != "group-1" || cards[1].card != card {
		t.Fatalf("群聊参数不符: %+v", cards[1])
	}
}

// TestZAFacadeSendCardRejectsBadInput 覆盖三部分字段校验、空 threadID 与空 msgID。
func TestZAFacadeSendCardRejectsBadInput(t *testing.T) {
	valid := OutboundCard{
		Text:    "hello",
		LinkURL: "https://example.com/jump",
	}

	t.Run("bad_link", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		card := valid
		card.LinkURL = "ftp://example.com/x"
		if _, err := ZaloSendCard(context.Background(), "acct", "peer-1", card); !errors.Is(err, ErrZaloCardBadLink) {
			t.Fatalf("非法外链 err=%v，期望 ErrZaloCardBadLink", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recordedCards()) != 0 {
			t.Fatalf("非法外链仍调了 SendCard")
		}
	})

	t.Run("empty_text", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		card := valid
		card.Text = "  \n\t "
		if _, err := ZaloSendCard(context.Background(), "acct", "peer-1", card); !errors.Is(err, ErrZaloCardEmptyText) {
			t.Fatalf("空文字 err=%v，期望 ErrZaloCardEmptyText", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recordedCards()) != 0 {
			t.Fatalf("空文字仍调了 SendCard")
		}
	})

	t.Run("empty_thread", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		if _, err := ZaloSendGroupCard(context.Background(), "acct", "   ", valid); !errors.Is(err, errZAFacadeEmptyThread) {
			t.Fatalf("空 threadID err=%v，期望 errZAFacadeEmptyThread", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recordedCards()) != 0 {
			t.Fatalf("空 threadID 仍调了 SendCard")
		}
	})

	t.Run("empty_msg_id", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.setCardMsgID("  ") }
		if _, err := ZaloSendCard(context.Background(), "acct", "peer-1", valid); !errors.Is(err, ErrZaloEmptyMessageID) {
			t.Fatalf("空 msgID err=%v，期望 ErrZaloEmptyMessageID", err)
		}
	})
}

// TestZAFacadeSendCardSharesQuota 断言卡片与文本共用同一本滚动小时配额。
func TestZAFacadeSendCardSharesQuota(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 1)
	ctx := context.Background()
	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "first"); err != nil {
		t.Fatalf("首条文本: %v", err)
	}
	_, err := ZaloSendCard(ctx, "acct", "peer-1", OutboundCard{
		Text:    "card",
		LinkURL: "https://example.com/x",
	})
	if !errors.Is(err, ErrZaloSendLimit) {
		t.Fatalf("卡片 err=%v，期望与文本共用配额并返回 ErrZaloSendLimit", err)
	}
	if cards := sdk.lastConn().recordedCards(); len(cards) != 0 {
		t.Fatalf("配额已满仍调了 SendCard: %+v", cards)
	}
}

// TestZAFacadeSendCardConnErrorIsWrapped 断言 conn 错误可 errors.Is 且不回显卡片文字。
func TestZAFacadeSendCardConnErrorIsWrapped(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	sdk.configure = func(c *facConn) { c.setCardErr(errFacBoom) }
	const body = "S3CRET-CARD-7781"
	_, err := ZaloSendCard(context.Background(), "acct", "peer-1", OutboundCard{
		Text:    body,
		LinkURL: "https://example.com/x",
	})
	if !errors.Is(err, errFacBoom) {
		t.Fatalf("conn 错误 err=%v，期望可 errors.Is(errFacBoom)", err)
	}
	if strings.Contains(err.Error(), body) {
		t.Fatalf("错误文案回显了卡片文字: %q", err.Error())
	}
}

// zaTestAPIErr 构造带 Zalo 业务码的错误，用于驱动拒收降级分支。
func zaTestAPIErr(code int) error {
	c := zcagoerrs.ZaloErrorCode(code)
	return zcagoerrs.NewZaloAPIError("rejected", &c)
}

// TestZAFacadeSendCardFallsBackOnRejection 断言卡片被业务码拒收后降级为「文字 + 链接」，
// 且第二次发送仍走同一账号的发送路由。
func TestZAFacadeSendCardFallsBackOnRejection(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	sdk.configure = func(c *facConn) { c.setCardErr(zaTestAPIErr(114)) }
	card := OutboundCard{
		Text:    "首行",
		LinkURL: "https://example.com/jump",
	}
	id, err := ZaloSendCard(context.Background(), "acct", "peer-1", card)
	if err != nil {
		t.Fatalf("降级发送: %v", err)
	}
	if id != "msg-fac" {
		t.Fatalf("降级后 msgID=%q，期望文本发送的 msg-fac", id)
	}
	conn := sdk.lastConn()
	if got := len(conn.recordedCards()); got != 1 {
		t.Fatalf("SendCard 调用 %d 次，期望 1", got)
	}
	sends := conn.recorded()
	if len(sends) != 1 {
		t.Fatalf("降级后 SendText 调用 %d 次，期望 1", len(sends))
	}
	if sends[0].group || sends[0].threadID != "peer-1" {
		t.Fatalf("降级发送路由不符: %+v", sends[0])
	}
	if sends[0].text != "首行\nhttps://example.com/jump" {
		t.Fatalf("降级正文=%q", sends[0].text)
	}
}

// TestZAFacadeSendCardNoFallback 断言网络错误、限流与服务端错误都不降级，避免重复发送。
func TestZAFacadeSendCardNoFallback(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"transport_599", zaTestAPIErr(599)},
		{"rate_limit_429", zaTestAPIErr(429)},
		{"server_500", zaTestAPIErr(500)},
		{"non_api_error", errFacBoom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, sdk, _ := facNewSendFixture(t, 0)
			sdk.configure = func(fc *facConn) { fc.setCardErr(c.err) }
			_, err := ZaloSendCard(context.Background(), "acct", "peer-1", OutboundCard{
				Text:    "card",
				LinkURL: "https://example.com/x",
			})
			if !errors.Is(err, c.err) {
				t.Fatalf("err=%v，期望可 errors.Is 原始错误", err)
			}
			if sends := sdk.lastConn().recorded(); len(sends) != 0 {
				t.Fatalf("不应降级，实际 SendText %d 次", len(sends))
			}
		})
	}
}

// TestZAFacadeSendCardFallbackSharesQuota 断言降级是第二次逻辑发送：
// 首条已占槽，配额为 1 时降级发送必须被配额拦下。
func TestZAFacadeSendCardFallbackSharesQuota(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 1)
	sdk.configure = func(c *facConn) { c.setCardErr(zaTestAPIErr(114)) }
	_, err := ZaloSendCard(context.Background(), "acct", "peer-1", OutboundCard{
		Text:    "card",
		LinkURL: "https://example.com/x",
	})
	if !errors.Is(err, ErrZaloSendLimit) {
		t.Fatalf("err=%v，期望降级发送被 ErrZaloSendLimit 拦下", err)
	}
	if sends := sdk.lastConn().recorded(); len(sends) != 0 {
		t.Fatalf("配额已满仍调了 SendText: %+v", sends)
	}
}

// TestZAFacadeSendCardFallbackDoesNotDuplicateLink 断言正文已含链接时不重复拼接。
func TestZAFacadeSendCardFallbackDoesNotDuplicateLink(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	sdk.configure = func(c *facConn) { c.setCardErr(zaTestAPIErr(112)) }
	const text = "看这里 https://example.com/jump"
	_, err := ZaloSendCard(context.Background(), "acct", "peer-1", OutboundCard{
		Text:    text,
		LinkURL: "https://example.com/jump",
	})
	if err != nil {
		t.Fatalf("降级发送: %v", err)
	}
	sends := sdk.lastConn().recorded()
	if len(sends) != 1 || sends[0].text != text {
		t.Fatalf("降级正文=%+v，期望原文不重复链接", sends)
	}
}

// TestZACardRejected 覆盖拒收判定：业务码/4xx 降级，599/429/5xx/非 API 错误不降级。
func TestZACardRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"业务码114", zaTestAPIErr(114), true},
		{"业务码112", zaTestAPIErr(112), true},
		{"http400", zaTestAPIErr(400), true},
		{"限流429", zaTestAPIErr(429), false},
		{"服务端500", zaTestAPIErr(500), false},
		{"传输599", zaTestAPIErr(599), false},
		{"非API错误", errFacBoom, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := zaCardRejected(c.err); got != c.want {
				t.Fatalf("zaCardRejected=%v, want %v", got, c.want)
			}
		})
	}
}

// TestZAFacadeSendDelay 断言首条不等待、第二条睡眠在 [sendMin,sendMax] 内，
// 且随机间隔参数取自 limits。
func TestZAFacadeSendDelay(t *testing.T) {
	limits := mgrTestLimits()
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	var (
		delayCalls int
		gotMin     time.Duration
		gotMax     time.Duration
	)
	m.randDelay = func(min, max time.Duration) time.Duration {
		delayCalls++
		gotMin, gotMax = min, max
		return 4 * time.Second
	}
	facInstall(t, m)
	ctx := context.Background()

	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "first"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}
	if got := clock.Sleeps(); len(got) != 0 {
		t.Fatalf("首条不应等待，实际睡眠 %v", got)
	}
	if delayCalls != 0 {
		t.Fatalf("首条不应计算随机间隔，实际调用 %d 次", delayCalls)
	}

	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "second"); err != nil {
		t.Fatalf("第二条发送: %v", err)
	}
	if delayCalls != 1 {
		t.Fatalf("第二条应计算一次随机间隔，实际 %d 次", delayCalls)
	}
	if gotMin != limits.sendMin || gotMax != limits.sendMax {
		t.Fatalf("间隔参数=(%v,%v)，期望 (%v,%v)", gotMin, gotMax, limits.sendMin, limits.sendMax)
	}
	sleeps := clock.Sleeps()
	if len(sleeps) != 1 || sleeps[0] != 4*time.Second {
		t.Fatalf("睡眠记录=%v，期望 [4s]", sleeps)
	}
	if conn := sdk.lastConn(); len(conn.recorded()) != 2 {
		t.Fatalf("SendText 调用 %d 次，期望 2", len(conn.recorded()))
	}
}

// TestZAFacadeSendHourlyLimit 覆盖滚动小时配额：第 N+1 条立即被拒、失败发送也占槽、
// 恰好满一小时的时间戳被剪掉。
func TestZAFacadeSendHourlyLimit(t *testing.T) {
	t.Run("third_rejected", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 2)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "ok"); err != nil {
				t.Fatalf("第 %d 条发送: %v", i+1, err)
			}
		}
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "over"); !errors.Is(err, ErrZaloSendLimit) {
			t.Fatalf("第 3 条 err=%v，期望 ErrZaloSendLimit", err)
		}
		if got := len(sdk.lastConn().recorded()); got != 2 {
			t.Fatalf("被拒后 SendText 调用 %d 次，期望 2", got)
		}
	})

	t.Run("failed_send_uses_slot", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 2)
		sdk.configure = func(c *facConn) { c.setSendErr(errFacBoom) }
		ctx := context.Background()

		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "boom"); !errors.Is(err, errFacBoom) {
			t.Fatalf("首次失败 err=%v，期望注入错误", err)
		}
		conn := sdk.lastConn()
		conn.setSendErr(nil)
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "ok"); err != nil {
			t.Fatalf("第二次发送: %v", err)
		}
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "over"); !errors.Is(err, ErrZaloSendLimit) {
			t.Fatalf("失败发送未占槽，第 3 条 err=%v", err)
		}
	})

	t.Run("window_prunes_at_boundary", func(t *testing.T) {
		_, _, clock := facNewSendFixture(t, 2)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "ok"); err != nil {
				t.Fatalf("第 %d 条发送: %v", i+1, err)
			}
		}
		clock.Advance(time.Hour)
		if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "after-hour"); err != nil {
			t.Fatalf("满一小时后发送: %v", err)
		}
	})
}

// TestZAFacadeSendCancelDuringDelay 断言随机间隔睡眠被 ctx 取消时不占小时槽、
// 不推进 lastSend，且闸在取消后仍可继续工作。
func TestZAFacadeSendCancelDuringDelay(t *testing.T) {
	limits := mgrTestLimits()
	clock := newFacBlockClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	m.randDelay = func(time.Duration, time.Duration) time.Duration { return 4 * time.Second }
	facInstall(t, m)

	start := clock.Now()
	if _, err := ZaloSendMessage(context.Background(), "acct", "peer-1", "first"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}
	clock.Advance(7 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ZaloSendMessage(ctx, "acct", "peer-2", "second")
		done <- err
	}()
	select {
	case <-clock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第二条没有进入随机间隔等待")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("等待取消 err=%v，期望 context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后发送未返回")
	}

	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("账号不在内存")
	}
	acc.sendMu.Lock()
	windowLen := len(acc.sendWindow)
	lastSend := acc.lastSend
	acc.sendMu.Unlock()
	if windowLen != 1 {
		t.Fatalf("取消的发送占了小时槽：窗口 %d 条，期望 1", windowLen)
	}
	if !lastSend.Equal(start) {
		t.Fatalf("取消的发送推进了 lastSend=%v，期望仍为 %v", lastSend, start)
	}
	if got := len(sdk.lastConn().recorded()); got != 1 {
		t.Fatalf("取消的发送打到了 SendText：调用 %d 次，期望 1", got)
	}

	clock.unblock()
	if _, err := ZaloSendMessage(context.Background(), "acct", "peer-3", "third"); err != nil {
		t.Fatalf("取消后继续发送: %v", err)
	}
	acc.sendMu.Lock()
	windowLen = len(acc.sendWindow)
	acc.sendMu.Unlock()
	if windowLen != 2 {
		t.Fatalf("恢复发送后窗口 %d 条，期望 2", windowLen)
	}
}

// TestZAFacadeFriendsCache 断言好友列表 5 分钟缓存经门面生效：两次 List 只打一次
// 接口，间隔内的 Sync 返回 ErrZaloFetchTooSoon，过闸后可强刷。
func TestZAFacadeFriendsCache(t *testing.T) {
	limits := mgrTestLimits()
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk.configure = func(c *facConn) {
		c.setFriends([]UserInfo{{UserID: "u1", DisplayName: "Alice"}})
	}
	facInstall(t, m)
	ctx := context.Background()

	first, err := ZaloListFriends(ctx, "acct")
	if err != nil {
		t.Fatalf("首次 ListFriends: %v", err)
	}
	if len(first) != 1 || first[0].UserID != "u1" {
		t.Fatalf("首次好友列表不符: %+v", first)
	}
	second, err := ZaloListFriends(ctx, "acct")
	if err != nil {
		t.Fatalf("二次 ListFriends: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("二次好友列表不符: %+v", second)
	}
	conn := sdk.lastConn()
	if got := conn.friendsCallCount(); got != 1 {
		t.Fatalf("5 分钟内 List 两次打了 %d 次接口，期望 1", got)
	}
	if _, err := ZaloSyncFriends(ctx, "acct"); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("间隔内 SyncFriends err=%v，期望 ErrZaloFetchTooSoon", err)
	}
	if got := conn.friendsCallCount(); got != 1 {
		t.Fatalf("被闸拒绝仍拉取：%d 次", got)
	}

	clock.Advance(limits.friendsTTL)
	synced, err := ZaloSyncFriends(ctx, "acct")
	if err != nil {
		t.Fatalf("过闸后 SyncFriends: %v", err)
	}
	if len(synced) != 1 {
		t.Fatalf("过闸后好友列表不符: %+v", synced)
	}
	if got := conn.friendsCallCount(); got != 2 {
		t.Fatalf("过闸后 SyncFriends 未拉取：%d 次，期望 2", got)
	}
}

// TestZAFacadeGroupsAndInfo 断言群列表缓存/同步闸与用户、群资料的批量查询经门面
// 正确透传，资料缓存命中不再打接口。
func TestZAFacadeGroupsAndInfo(t *testing.T) {
	limits := mgrTestLimits()
	clock := newCacheClock()
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk.configure = func(c *facConn) {
		c.setGroups(map[string]string{"g1": "v1", "g2": "v1"})
		c.setGroupInfo(map[string]GroupInfo{
			"g1": {GroupID: "g1", Name: "Alpha"},
			"g2": {GroupID: "g2", Name: "Beta"},
		})
		c.setUsers(map[string]UserInfo{"u1": {UserID: "u1", DisplayName: "Alice"}})
	}
	facInstall(t, m)
	ctx := context.Background()

	groups, err := ZaloListGroups(ctx, "acct")
	if err != nil {
		t.Fatalf("ZaloListGroups: %v", err)
	}
	if len(groups) != 2 || groups[0].Name != "Alpha" || groups[1].Name != "Beta" {
		t.Fatalf("群列表不符: %+v", groups)
	}
	if groups[0].Version != "v1" {
		t.Fatalf("群版本未填: %+v", groups[0])
	}
	conn := sdk.lastConn()
	if got := conn.groupsCallCount(); got != 1 {
		t.Fatalf("首次 ListGroups 调用 GetAllGroups %d 次，期望 1", got)
	}
	if _, err := ZaloListGroups(ctx, "acct"); err != nil {
		t.Fatalf("二次 ZaloListGroups: %v", err)
	}
	if got := conn.groupsCallCount(); got != 1 {
		t.Fatalf("TTL 内二次 ListGroups 又拉取：%d 次", got)
	}
	if _, err := ZaloSyncGroups(ctx, "acct"); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("间隔内 SyncGroups err=%v，期望 ErrZaloFetchTooSoon", err)
	}

	users, err := ZaloGetUserInfo(ctx, "acct", "u1")
	if err != nil {
		t.Fatalf("ZaloGetUserInfo: %v", err)
	}
	if len(users) != 1 || users["u1"].DisplayName != "Alice" {
		t.Fatalf("用户资料不符: %+v", users)
	}
	if _, err := ZaloGetUserInfo(ctx, "acct", "u1"); err != nil {
		t.Fatalf("二次 ZaloGetUserInfo: %v", err)
	}
	if got := conn.userInfoCallCount(); got != 1 {
		t.Fatalf("资料缓存命中仍请求：%d 批，期望 1", got)
	}

	infos, err := ZaloGetGroupInfo(ctx, "acct", "g1")
	if err != nil {
		t.Fatalf("ZaloGetGroupInfo: %v", err)
	}
	if len(infos) != 1 || infos["g1"].Name != "Alpha" {
		t.Fatalf("群资料不符: %+v", infos)
	}
	if got := len(conn.groupInfoRequests()); got != 1 {
		t.Fatalf("群资料缓存命中仍请求：%d 批，期望 1", got)
	}
}

// TestZAFacadeAccountDelegation 覆盖账号类门面的委托：cookie 导入、显式拉起、
// 删除、列表合并，以及坏凭证/无凭证/查库失败的错误透传。
func TestZAFacadeAccountDelegation(t *testing.T) {
	m, store, sdk := facNewManager(t, mgrTestLimits(), nil)
	facInstall(t, m)
	ctx := context.Background()

	raw := []byte(`[{"name":"zpw_sek","value":"not-a-real-token","domain":"zalo.me"}]`)
	info, err := ZaloLoginWithCookie(ctx, "acct", raw)
	if err != nil {
		t.Fatalf("ZaloLoginWithCookie: %v", err)
	}
	if info == nil || info.ID != "acct" || info.UID != "uid-fac" || !info.LoggedIn {
		t.Fatalf("导入返回信息不符: %+v", info)
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("导入后库里没有会话行")
	}
	if got := sdk.loginCount(); got != 1 {
		t.Fatalf("导入触发登录 %d 次，期望 1", got)
	}

	list, err := ZaloListAccounts(ctx)
	if err != nil {
		t.Fatalf("ZaloListAccounts: %v", err)
	}
	if len(list) != 1 || list[0].ID != "acct" || !list[0].LoggedIn {
		t.Fatalf("列表不符: %+v", list)
	}

	store.seed("seeded", "uid-seeded", []byte(mgrTestCredJSON))
	seeded, err := ZaloStartClient(ctx, "seeded")
	if err != nil {
		t.Fatalf("ZaloStartClient: %v", err)
	}
	if seeded == nil || seeded.ID != "seeded" || !seeded.LoggedIn {
		t.Fatalf("StartClient 返回不符: %+v", seeded)
	}

	list, err = ZaloListAccounts(ctx)
	if err != nil {
		t.Fatalf("二次 ZaloListAccounts: %v", err)
	}
	if len(list) != 2 || list[0].ID != "acct" || list[1].ID != "seeded" {
		t.Fatalf("列表排序/内容不符: %+v", list)
	}

	if err := ZaloDeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("ZaloDeleteAccount: %v", err)
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("删除后库行仍在")
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("删除后账号仍在内存")
	}

	if _, err := ZaloStartClient(ctx, "ghost"); !errors.Is(err, ErrZaloNotLoggedIn) {
		t.Fatalf("无凭证 StartClient err=%v，期望 ErrZaloNotLoggedIn", err)
	}
	if _, err := ZaloLoginWithCookie(ctx, "bad", []byte(`{"nope":true}`)); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("坏凭证 err=%v，期望 ErrZaloBadCredentials", err)
	}

	store.mu.Lock()
	store.listErr = errFacBoom
	store.mu.Unlock()
	if got, err := ZaloListAccounts(ctx); !errors.Is(err, errFacBoom) {
		t.Fatalf("查库失败 err=%v，期望注入错误", err)
	} else if got != nil {
		t.Fatalf("查库失败返回了半份列表: %+v", got)
	}
}

// failTransport 模拟传输层失败（DNS/连接/超时）。
type failTransport struct{ err error }

func (t failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

func TestZAFallbackTransportTurnsErrorIntoResponse(t *testing.T) {
	tr := zaFallbackTransport{base: failTransport{err: errors.New("dial tcp: lookup wpa.chat.zalo.me: no such host")}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://wpa.chat.zalo.me/api/x", nil)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("兜底后不应再返回 err: %v", err)
	}
	if resp == nil {
		t.Fatal("兜底后必须给出一个非 nil response")
	}
	if resp.StatusCode != 599 {
		t.Fatalf("状态码=%d，期望 599", resp.StatusCode)
	}
	if resp.Body == nil {
		t.Fatal("Body 不能为 nil：上游 endpoint 会 defer resp.Body.Close()")
	}
	if resp.Header == nil {
		t.Fatal("Header 不能为 nil：上游会读 resp.Header.Get")
	}
	// 上游拿到的错误文案里不能再出现 URL（URL 带加密参数与会话标识）。
	if strings.Contains(resp.Status, "wpa.chat.zalo.me/api") {
		t.Fatalf("状态行回显了 URL: %q", resp.Status)
	}
	if !strings.Contains(resp.Status, "no such host") {
		t.Fatalf("状态行应保留可排查的原因: %q", resp.Status)
	}
}

// TestZAFallbackTransportPassesSuccess 断言成功响应原样透传，不被改写。
func TestZAFallbackTransportPassesSuccess(t *testing.T) {
	want := &http.Response{StatusCode: 200, Body: http.NoBody}
	tr := zaFallbackTransport{base: okTransport{resp: want}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://wpa.chat.zalo.me/api/x", nil)

	got, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != want {
		t.Fatalf("成功响应被改写: %+v", got)
	}
}

type okTransport struct{ resp *http.Response }

func (t okTransport) RoundTrip(*http.Request) (*http.Response, error) { return t.resp, nil }

// TestZATransportErrTextDropsURL 断言 url.Error 的 URL 不进文案。
func TestZATransportErrTextDropsURL(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://wpa.chat.zalo.me/api/login/getServerInfo?signkey=SECRET",
		Err: errors.New("dial tcp: i/o timeout"),
	}
	text := zaTransportErrText(err)
	if strings.Contains(text, "SECRET") || strings.Contains(text, "wpa.chat.zalo.me") {
		t.Fatalf("文案泄露了 URL/参数: %q", text)
	}
	if !strings.Contains(text, "i/o timeout") {
		t.Fatalf("文案丢了根因: %q", text)
	}
	if strings.ContainsAny(text, "\r\n") {
		t.Fatalf("文案必须单行: %q", text)
	}
}

// lstStrPtr 返回字符串指针，便于构造 Content.String。
func lstStrPtr(s string) *string { return &s }

// TestZAStartListenerNilAPI 断言没有登录句柄时 StartListener 立即报错而不是 panic，
// 未启动状态下的 StopListener 可重复调用。真实启停/重连/关闭码由 fake conn 与真机验证覆盖。
func TestZAStartListenerNilAPI(t *testing.T) {
	t.Parallel()

	c := &zaRealConn{}
	if err := c.StartListener(context.Background(), nil, nil); !errors.Is(err, errZANilAPI) {
		t.Fatalf("StartListener err=%v，期望 %v", err, errZANilAPI)
	}
	c.StopListener()
	c.StopListener()
}

// lstFakeMessage 是一个不被 messageToInbound 认识的 model.Message，
// 用于覆盖类型开关的 default 分支。
type lstFakeMessage struct{}

func (lstFakeMessage) Type() model.ThreadType { return model.ThreadTypeUser }
func (lstFakeMessage) ThreadID() string       { return "thread-x" }
func (lstFakeMessage) IsSelf() bool           { return false }

// TestZAMessageToInbound 表驱动覆盖映射与过滤规则。
func TestZAMessageToInbound(t *testing.T) {
	t.Parallel()

	ptrUser := model.NewUserMessage("self-uid", model.TMessage{
		MsgID:   "m-ptr",
		UIDFrom: "peer-ptr",
		IDTo:    "self-uid",
		DName:   "Ptr User",
		TS:      "7",
		Content: model.Content{String: lstStrPtr("from pointer")},
	})
	ptrGroup := model.NewGroupMessage("self-uid", model.TGroupMessage{TMessage: model.TMessage{
		MsgID:   "gm-ptr",
		UIDFrom: "member-ptr",
		IDTo:    "group-ptr",
		DName:   "Ptr Member",
		TS:      "8",
		Content: model.Content{String: lstStrPtr("group pointer")},
	}})

	cases := []struct {
		name   string
		msg    model.Message
		want   InboundMessage
		wantOK bool
	}{
		{
			name: "user_text",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				MsgID:   "m-1",
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				DName:   "Alice",
				TS:      "1700000000123",
				Content: model.Content{String: lstStrPtr("hello")},
			}),
			want: InboundMessage{
				ThreadID:    "peer-1",
				ThreadType:  "user",
				IsSelf:      false,
				MsgID:       "m-1",
				FromID:      "peer-1",
				ToID:        "self-uid",
				SenderName:  "Alice",
				Text:        "hello",
				TimestampMS: 1700000000123,
			},
			wantOK: true,
		},
		{
			name: "user_self",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				MsgID:   "m-2",
				UIDFrom: "0",
				IDTo:    "peer-1",
				DName:   "Me",
				TS:      "1",
				Content: model.Content{String: lstStrPtr("yo")},
			}),
			want: InboundMessage{
				ThreadID:    "peer-1",
				ThreadType:  "user",
				IsSelf:      true,
				MsgID:       "m-2",
				FromID:      "self-uid",
				ToID:        "peer-1",
				SenderName:  "Me",
				Text:        "yo",
				TimestampMS: 1,
			},
			wantOK: true,
		},
		{
			name: "group_text",
			msg: model.NewGroupMessage("self-uid", model.TGroupMessage{TMessage: model.TMessage{
				MsgID:   "gm-1",
				UIDFrom: "member-1",
				IDTo:    "group-1",
				DName:   "Bob",
				TS:      "42",
				Content: model.Content{String: lstStrPtr("hi group")},
			}}),
			want: InboundMessage{
				ThreadID:    "group-1",
				ThreadType:  "group",
				IsSelf:      false,
				MsgID:       "gm-1",
				FromID:      "member-1",
				ToID:        "group-1",
				SenderName:  "Bob",
				Text:        "hi group",
				TimestampMS: 42,
			},
			wantOK: true,
		},
		{
			name: "group_self",
			msg: model.NewGroupMessage("self-uid", model.TGroupMessage{TMessage: model.TMessage{
				MsgID:   "gm-2",
				UIDFrom: "0",
				IDTo:    "group-1",
				DName:   "Me",
				TS:      "43",
				Content: model.Content{String: lstStrPtr("self in group")},
			}}),
			want: InboundMessage{
				ThreadID:    "group-1",
				ThreadType:  "group",
				IsSelf:      true,
				MsgID:       "gm-2",
				FromID:      "self-uid",
				ToID:        "group-1",
				SenderName:  "Me",
				Text:        "self in group",
				TimestampMS: 43,
			},
			wantOK: true,
		},
		{
			name: "user_pointer",
			msg:  &ptrUser,
			want: InboundMessage{
				ThreadID:    "peer-ptr",
				ThreadType:  "user",
				MsgID:       "m-ptr",
				FromID:      "peer-ptr",
				ToID:        "self-uid",
				SenderName:  "Ptr User",
				Text:        "from pointer",
				TimestampMS: 7,
			},
			wantOK: true,
		},
		{
			name: "group_pointer",
			msg:  &ptrGroup,
			want: InboundMessage{
				ThreadID:    "group-ptr",
				ThreadType:  "group",
				MsgID:       "gm-ptr",
				FromID:      "member-ptr",
				ToID:        "group-ptr",
				SenderName:  "Ptr Member",
				Text:        "group pointer",
				TimestampMS: 8,
			},
			wantOK: true,
		},
		{
			name: "text_kept_verbatim",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
				Content: model.Content{String: lstStrPtr("  spaced  ")},
			}),
			want: InboundMessage{
				ThreadID:    "peer-1",
				ThreadType:  "user",
				FromID:      "peer-1",
				ToID:        "self-uid",
				Text:        "  spaced  ",
				TimestampMS: 5,
			},
			wantOK: true,
		},
		{
			name: "blank_text_skipped",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
				Content: model.Content{String: lstStrPtr("   \t\n")},
			}),
			wantOK: false,
		},
		{
			name: "nil_text_skipped",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
			}),
			wantOK: false,
		},
		{
			name: "attachment_skipped",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
				Content: model.Content{Attachment: &model.TAttachmentContent{Title: "file"}},
			}),
			wantOK: false,
		},
		{
			name: "bad_ts_zeroed",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "not-a-number",
				Content: model.Content{String: lstStrPtr("ok")},
			}),
			want: InboundMessage{
				ThreadID:   "peer-1",
				ThreadType: "user",
				FromID:     "peer-1",
				ToID:       "self-uid",
				Text:       "ok",
			},
			wantOK: true,
		},
		{
			name: "empty_ts_zeroed",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "",
				Content: model.Content{String: lstStrPtr("ok")},
			}),
			want: InboundMessage{
				ThreadID:   "peer-1",
				ThreadType: "user",
				FromID:     "peer-1",
				ToID:       "self-uid",
				Text:       "ok",
			},
			wantOK: true,
		},
		{
			name:   "unknown_type_skipped",
			msg:    lstFakeMessage{},
			wantOK: false,
		},
		{
			name:   "nil_pointer_skipped",
			msg:    (*model.UserMessage)(nil),
			wantOK: false,
		},
		{
			name:   "nil_interface_skipped",
			msg:    nil,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := messageToInbound(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v，期望 %v（got=%+v）", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				if got != (InboundMessage{}) {
					t.Fatalf("被过滤的消息返回了非零值：%+v", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("InboundMessage=%+v，期望 %+v", got, tc.want)
			}
		})
	}
}

// panicZALogin 模拟 zcago 登录链对 nil response 的 nil 解引用。
type panicZALogin struct{}

var _ zcago.Zalo = panicZALogin{}

func (panicZALogin) Login(context.Context, zcago.Credentials) (zcago.API, error) {
	panic("runtime error: invalid memory address or nil pointer dereference")
}

func (panicZALogin) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zcago.API, error) {
	panic("runtime error: invalid memory address or nil pointer dereference")
}

// installPanicZALogin 把句柄工厂换成 panic 假件，测试结束还原。
func installPanicZALogin(t *testing.T) {
	t.Helper()
	prev := zaNewZaloHandle
	zaNewZaloHandle = func() zcago.Zalo { return panicZALogin{} }
	t.Cleanup(func() { zaNewZaloHandle = prev })
}

func TestZALoginRecoversUpstreamPanic(t *testing.T) {
	installPanicZALogin(t)
	sdk := newZASDK()

	conn, err := sdk.Login(context.Background(), zcago.Credentials{})
	if conn != nil {
		t.Fatalf("panic 收口后不应交出 conn，实得 %#v", conn)
	}
	if !errors.Is(err, errZALoginUpstreamPanic) {
		t.Fatalf("Login err=%v，期望 errZALoginUpstreamPanic", err)
	}
	// 文案要能指导下步排查：点名端点与网络/代理，而不是只给 panic 值。
	if !strings.Contains(err.Error(), "getServerInfo") {
		t.Fatalf("错误文案缺少端点提示: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "nil pointer dereference") {
		t.Fatalf("错误文案缺少 panic 值: %q", err.Error())
	}

	conn, err = sdk.LoginQR(context.Background(), nil, nil)
	if conn != nil {
		t.Fatalf("LoginQR panic 收口后不应交出 conn，实得 %#v", conn)
	}
	if !errors.Is(err, errZALoginUpstreamPanic) {
		t.Fatalf("LoginQR err=%v，期望 errZALoginUpstreamPanic", err)
	}
}

// TestZALoginNormalErrorUntouched 确认收口不吞普通错误：非 panic 的错误原样返回。
func TestZALoginNormalErrorUntouched(t *testing.T) {
	prev := zaNewZaloHandle
	zaNewZaloHandle = func() zcago.Zalo { return errZALogin{} }
	t.Cleanup(func() { zaNewZaloHandle = prev })

	_, err := newZASDK().Login(context.Background(), zcago.Credentials{})
	if !errors.Is(err, errFacBoom) {
		t.Fatalf("普通错误 err=%v，期望原样返回 errFacBoom", err)
	}
	if errors.Is(err, errZALoginUpstreamPanic) {
		t.Fatalf("普通错误被误判成上游 panic: %v", err)
	}
}

// errZALogin 模拟登录返回普通错误（如 cookie 失效）。
type errZALogin struct{}

var _ zcago.Zalo = errZALogin{}

func (errZALogin) Login(context.Context, zcago.Credentials) (zcago.API, error) {
	return nil, errFacBoom
}

func (errZALogin) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zcago.API, error) {
	return nil, errFacBoom
}

// errMgrBoom 是被注入的通用失败，用于断言 errors.Is 能穿过安全包装。
var errMgrBoom = errors.New("mgr test boom")

// fakeZAStore 是 zaStore 的记账假件：map 存储、调用计数、可注入错误。
type fakeZAStore struct {
	mu   sync.Mutex
	recs map[string]zaSessionRecord

	getCalls, listCalls, upsertCalls, deleteCalls, closeCalls int
	getErr, listErr, upsertErr, deleteErr, closeErr           error
	lastUpsert                                                zaSessionRecord
	hasUpsert                                                 bool
}

// newFakeZAStore 构造空库。
func newFakeZAStore() *fakeZAStore {
	return &fakeZAStore{recs: make(map[string]zaSessionRecord)}
}

// seed 写入一行会话（固定更新时间）。
func (s *fakeZAStore) seed(id, uid string, cred []byte) {
	s.seedAt(id, uid, cred, time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
}

// seedAt 写入一行带指定更新时间的会话。
func (s *fakeZAStore) seedAt(id, uid string, cred []byte, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[id] = zaSessionRecord{
		AccountID:   id,
		UID:         uid,
		Credentials: append([]byte(nil), cred...),
		UpdatedAt:   at,
	}
}

// Get 返回记录副本；查无返回 (nil,nil)。
func (s *fakeZAStore) Get(_ context.Context, accountID string) (*zaSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	rec, ok := s.recs[accountID]
	if !ok {
		return nil, nil
	}
	cp := rec
	cp.Credentials = append([]byte(nil), rec.Credentials...)
	return &cp, nil
}

// List 返回全部元数据（故意按 map 顺序，调用方必须自行排序）。
func (s *fakeZAStore) List(_ context.Context) ([]zaSessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]zaSessionMeta, 0, len(s.recs))
	for _, rec := range s.recs {
		out = append(out, zaSessionMeta{AccountID: rec.AccountID, UID: rec.UID, UpdatedAt: rec.UpdatedAt})
	}
	return out, nil
}

// Upsert 覆盖写入并记账最后一次载荷。
func (s *fakeZAStore) Upsert(_ context.Context, rec zaSessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertCalls++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	cp := rec
	cp.Credentials = append([]byte(nil), rec.Credentials...)
	// 与真库同口径：Upsert 忽略调用方 UpdatedAt，写路径统一取 now()。
	cp.UpdatedAt = time.Now().UTC()
	s.recs[rec.AccountID] = cp
	s.lastUpsert = cp
	s.hasUpsert = true
	return nil
}

// Delete 删除行；未知 ID 也算成功（幂等）。
func (s *fakeZAStore) Delete(_ context.Context, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.recs, accountID)
	return nil
}

// Close 只记账。
func (s *fakeZAStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return s.closeErr
}

// getCount / upsertCount / deleteCount / closeCount 返回对应调用计数。
func (s *fakeZAStore) getCount() int    { s.mu.Lock(); defer s.mu.Unlock(); return s.getCalls }
func (s *fakeZAStore) upsertCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.upsertCalls }
func (s *fakeZAStore) deleteCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.deleteCalls }
func (s *fakeZAStore) closeCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.closeCalls }

// record 返回某账号的当前行。
func (s *fakeZAStore) record(id string) (zaSessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[id]
	return rec, ok
}

// lastUpsertRec 返回最后一次 Upsert 的载荷。
func (s *fakeZAStore) lastUpsertRec() (zaSessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUpsert, s.hasUpsert
}

// fakeZASDK 是登录 seam 的假件。
//
// gate 非 nil 时 Login 会一直等到 gate 关闭且**忽略 ctx**：这样「停机期间登录
// 成功」的路径才能被真实覆盖（否则 ctx 取消会让 Login 直接报错）。
type fakeZASDK struct {
	mu         sync.Mutex
	loginCalls int
	qrCalls    int
	loginErr   error
	gate       chan struct{}
	entered    chan struct{}
	factory    func(zcago.Credentials) *fakeZAConn
}

// newFakeZASDK 构造默认假件（Login 立即返回带默认 UID 的假 conn）。
func newFakeZASDK() *fakeZASDK { return &fakeZASDK{} }

// Login 记录调用并按 gate/factory 行为返回。
func (s *fakeZASDK) Login(_ context.Context, cred zcago.Credentials) (zaConn, error) {
	s.mu.Lock()
	s.loginCalls++
	gate := s.gate
	entered := s.entered
	err := s.loginErr
	factory := s.factory
	s.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return &fakeZAConn{uid: "uid-default"}, nil
	}
	return factory(cred), nil
}

// LoginQR 不在本文件使用。
func (s *fakeZASDK) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zaConn, error) {
	s.mu.Lock()
	s.qrCalls++
	s.mu.Unlock()
	return nil, errMgrBoom
}

// loginCount / qrCount 返回调用计数。
func (s *fakeZASDK) loginCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.loginCalls }

// fakeZAConn 是 zaConn 的假件：可注入数据/错误，保存 listener handler 供手动 emit。
type fakeZAConn struct {
	uid     string
	snap    zcago.Credentials
	snapErr error

	mu sync.Mutex

	startCalls, stopCalls int
	startErr              error
	onMessage             func(InboundMessage)
	onTerminal            func(int, string)

	friends      []UserInfo
	friendsErr   error
	friendsCalls int

	groups      map[string]string
	groupsErr   error
	groupsCalls int

	groupInfo      map[string]GroupInfo
	groupInfoErr   error
	groupInfoCalls [][]string

	users         map[string]UserInfo
	userInfoErr   error
	userInfoCalls [][]string

	sendErr   error
	sendMsgID string

	deleteErr   error
	deleteCalls []fakeZADeleteCall
}

// fakeZADeleteCall 是一次到达 conn.DeleteChat 的调用记录。
type fakeZADeleteCall struct {
	threadID string
	group    bool
	boundary ChatBoundary
}

// UID 返回固定 UID。
func (c *fakeZAConn) UID() string { return c.uid }

// SnapshotCredentials 返回固定快照或注入错误。
func (c *fakeZAConn) SnapshotCredentials() (zcago.Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapErr != nil {
		return zcago.Credentials{}, c.snapErr
	}
	return c.snap, nil
}

// SendText 记录并返回注入的 msgID（本文件不测发送）。
func (c *fakeZAConn) SendText(context.Context, string, bool, string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendMsgID, c.sendErr
}

// SendCard 返回注入的 msgID/错误（本文件不测卡片发送）。
func (c *fakeZAConn) SendCard(context.Context, string, bool, OutboundCard) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendMsgID, c.sendErr
}

// DeleteChat 记录边界参数并返回注入错误。
func (c *fakeZAConn) DeleteChat(_ context.Context, threadID string, group bool, boundary ChatBoundary) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteCalls = append(c.deleteCalls, fakeZADeleteCall{threadID: threadID, group: group, boundary: boundary})
	return c.deleteErr
}

// recordedDeletes 返回删除调用记录副本。
func (c *fakeZAConn) recordedDeletes() []fakeZADeleteCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeZADeleteCall(nil), c.deleteCalls...)
}

// GetAllFriends 返回好友副本。
func (c *fakeZAConn) GetAllFriends(ctx context.Context) ([]UserInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friendsCalls++
	if c.friendsErr != nil {
		return nil, c.friendsErr
	}
	out := make([]UserInfo, len(c.friends))
	copy(out, c.friends)
	return out, nil
}

// GetAllGroups 返回版本 map 副本。
func (c *fakeZAConn) GetAllGroups(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupsCalls++
	if c.groupsErr != nil {
		return nil, c.groupsErr
	}
	out := make(map[string]string, len(c.groups))
	for id, ver := range c.groups {
		out[id] = ver
	}
	return out, nil
}

// GetGroupInfo 返回请求 ID 中真实存在的资料并记录每批 ID。
func (c *fakeZAConn) GetGroupInfo(ctx context.Context, ids ...string) ([]GroupInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupInfoCalls = append(c.groupInfoCalls, append([]string(nil), ids...))
	if c.groupInfoErr != nil {
		return nil, c.groupInfoErr
	}
	out := make([]GroupInfo, 0, len(ids))
	for _, id := range ids {
		if g, ok := c.groupInfo[id]; ok {
			if g.GroupID == "" {
				g.GroupID = id
			}
			out = append(out, g)
		}
	}
	return out, nil
}

// GetUserInfo 返回请求 ID 中真实存在的资料并记录每批 ID。
func (c *fakeZAConn) GetUserInfo(ctx context.Context, ids ...string) ([]UserInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userInfoCalls = append(c.userInfoCalls, append([]string(nil), ids...))
	if c.userInfoErr != nil {
		return nil, c.userInfoErr
	}
	out := make([]UserInfo, 0, len(ids))
	for _, id := range ids {
		if u, ok := c.users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// StartListener 保存 handler；emit 由测试驱动。
func (c *fakeZAConn) StartListener(_ context.Context, onMessage func(InboundMessage), onTerminal func(int, string)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startCalls++
	c.onMessage = onMessage
	c.onTerminal = onTerminal
	return c.startErr
}

// StopListener 只计数。
func (c *fakeZAConn) StopListener() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopCalls++
}

// emitMessage 手动投递一条入站消息。
func (c *fakeZAConn) emitMessage(msg InboundMessage) {
	c.mu.Lock()
	h := c.onMessage
	c.mu.Unlock()
	if h != nil {
		h(msg)
	}
}

// emitTerminal 手动投递一条 listener 关闭事件。
func (c *fakeZAConn) emitTerminal(code int, reason string) {
	c.mu.Lock()
	h := c.onTerminal
	c.mu.Unlock()
	if h != nil {
		h(code, reason)
	}
}

// setFriends / setFriendsErr 设置好友数据。
func (c *fakeZAConn) setFriends(list []UserInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friends = list
}

func (c *fakeZAConn) setFriendsErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friendsErr = err
}

// setGroups / setGroupInfo 设置群版本与资料。
func (c *fakeZAConn) setGroups(vers map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groups = vers
}

func (c *fakeZAConn) setGroupInfo(infos map[string]GroupInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupInfo = infos
}

// startCount / stopCount / friendsCallCount / groupsCallCount / groupInfoCallCount
// 返回计数器与请求批次快照。
func (c *fakeZAConn) startCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.startCalls }
func (c *fakeZAConn) stopCount() int  { c.mu.Lock(); defer c.mu.Unlock(); return c.stopCalls }

func (c *fakeZAConn) friendsCallCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.friendsCalls }
func (c *fakeZAConn) groupsCallCount() int  { c.mu.Lock(); defer c.mu.Unlock(); return c.groupsCalls }

func (c *fakeZAConn) groupInfoRequests() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]string, len(c.groupInfoCalls))
	for i, ids := range c.groupInfoCalls {
		out[i] = append([]string(nil), ids...)
	}
	return out
}

// mgrTestCredJSON 是一份最小的可解码凭证（假 SDK 不校验 cookie）。
const mgrTestCredJSON = `{"imei":"imei-seed","userAgent":"ua-seed","language":"vi"}`

// mgrTestLimits 是测试用限频：TTL/间隔取生产下限，超时缩短。
func mgrTestLimits() zaLimits {
	return zaLimits{
		qrTTL:          5 * time.Minute,
		requestTimeout: 5 * time.Second,
		listener:       true,
		friendsTTL:     5 * time.Minute,
		groupsTTL:      5 * time.Minute,
		infoMin:        1500 * time.Millisecond,
		infoTTL:        10 * time.Minute,
		sendMin:        3 * time.Second,
		sendMax:        5 * time.Second,
		sendMaxPerHour: 30,
		userAgent:      "test-ua",
		language:       "vi",
	}
}

// mgrNewTestManager 构造 manager 并登记停机清理。
func mgrNewTestManager(t *testing.T, store zaStore, sdk zaSDK, clock zaClock) *zaManager {
	t.Helper()
	if clock == nil {
		clock = newCacheClock()
	}
	m := newZAManager(context.Background(), mgrTestLimits(), store, sdk, clock, nil)
	t.Cleanup(m.Stop)
	return m
}

// mgrWaitTrue 用 Gosched 自旋等待条件成立，避免用睡眠猜调度。
func mgrWaitTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时", what)
		}
		runtime.Gosched()
	}
}

// TestZAManagerEnsureConnSingleFlight 断言 100 个并发 ensureConn 只读一次库、
// 只登录一次，且拿到同一个 conn。
func TestZAManagerEnsureConnSingleFlight(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))

	sdk := newFakeZASDK()
	sdk.gate = make(chan struct{})
	sdk.entered = make(chan struct{}, 1)
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}

	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	const workers = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	conns := make([]zaConn, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			conns[i], errs[i] = acc.ensureConn(context.Background())
		}(i)
	}
	close(start)
	select {
	case <-sdk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("登录未被调用")
	}
	close(sdk.gate)
	wg.Wait()

	if got := store.getCount(); got != 1 {
		t.Fatalf("store.Get 调用 %d 次，期望 1", got)
	}
	if got := sdk.loginCount(); got != 1 {
		t.Fatalf("sdk.Login 调用 %d 次，期望 1", got)
	}
	if got := store.upsertCount(); got != 1 {
		t.Fatalf("store.Upsert 调用 %d 次，期望 1", got)
	}
	first := conns[0]
	if first == nil {
		t.Fatal("第一个等待者拿到 nil conn")
	}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d 失败: %v", i, errs[i])
		}
		if conns[i] != first {
			t.Fatalf("worker %d 拿到不同 conn", i)
		}
	}
	if rec, ok := store.lastUpsertRec(); !ok || rec.UID != "uid-live" {
		t.Fatalf("落库 UID=%q ok=%v，期望 uid-live", rec.UID, ok)
	}
}

// TestZAManagerEnsureConnStoreErrors 覆盖库缺失/库句柄哨兵/库故障/登录失败：
// 一律不安装 conn、不写库。
func TestZAManagerEnsureConnStoreErrors(t *testing.T) {
	cases := []struct {
		name     string
		seed     bool
		getErr   error
		loginErr error
		want     error
	}{
		{name: "missing_row", want: ErrZaloNotLoggedIn},
		{name: "store_unopened", getErr: errZAGStoreUnopened, want: ErrZaloNotStarted},
		{name: "store_closed", getErr: errZAGStoreClosed, want: ErrZaloNotStarted},
		{name: "store_failed", getErr: errMgrBoom, want: errMgrBoom},
		{name: "login_failed", seed: true, loginErr: errMgrBoom, want: errMgrBoom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeZAStore()
			if tc.seed {
				store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
			}
			store.getErr = tc.getErr
			sdk := newFakeZASDK()
			sdk.loginErr = tc.loginErr
			m := mgrNewTestManager(t, store, sdk, nil)

			acc, err := m.accountOrCreate("acct")
			if err != nil {
				t.Fatalf("accountOrCreate: %v", err)
			}
			conn, err := acc.ensureConn(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("ensureConn err=%v，期望 errors.Is(%v)", err, tc.want)
			}
			if conn != nil {
				t.Fatal("失败路径安装了 conn")
			}
			if acc.currentConn() != nil {
				t.Fatal("失败路径账号上有 conn")
			}
			if store.upsertCount() != 0 {
				t.Fatal("失败路径写了库")
			}
			if tc.loginErr == nil && sdk.loginCount() != 0 {
				t.Fatal("读库失败/查无时不应调登录")
			}
		})
	}
}

// TestZAManagerEnsureConnWaiterCancel 断言等待者 ctx 取消只影响自己，leader
// 继续完成登录。
func TestZAManagerEnsureConnWaiterCancel(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.gate = make(chan struct{})
	sdk.entered = make(chan struct{}, 1)
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	leaderDone := make(chan struct{})
	var leaderConn zaConn
	var leaderErr error
	go func() {
		defer close(leaderDone)
		leaderConn, leaderErr = acc.ensureConn(context.Background())
	}()
	<-sdk.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acc.ensureConn(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待者 err=%v，期望 context.Canceled", err)
	}

	close(sdk.gate)
	<-leaderDone
	if leaderErr != nil {
		t.Fatalf("leader 失败: %v", leaderErr)
	}
	if leaderConn == nil {
		t.Fatal("leader 没有拿到 conn")
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("store.Get 调用 %d 次，期望 1", got)
	}
}

// TestZAManagerStopDuringLogin 断言 Stop 开始后即使登录成功也不写库、不安装。
func TestZAManagerStopDuringLogin(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.gate = make(chan struct{})
	sdk.entered = make(chan struct{}, 1)
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	leaderErr := make(chan error, 1)
	go func() {
		_, err := acc.ensureConn(context.Background())
		leaderErr <- err
	}()
	<-sdk.entered

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	mgrWaitTrue(t, "manager 进入 stopping", func() bool { return m.stopping.Load() })
	close(sdk.gate)

	if err := <-leaderErr; !errors.Is(err, ErrZaloNotStarted) {
		t.Fatalf("停机中登录 err=%v，期望 ErrZaloNotStarted", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 未在预算内返回")
	}
	if got := store.upsertCount(); got != 0 {
		t.Fatalf("停机期间写了 %d 次库，期望 0", got)
	}
	if acc.currentConn() != nil {
		t.Fatal("停机期间安装出了 conn")
	}
	if got := store.closeCount(); got != 1 {
		t.Fatalf("store.Close 调用 %d 次，期望 1", got)
	}
}

// TestZAManagerListAccountsMergesOnline 断言查库元数据与内存在线状态合并、
// 按 ID 排序；查库失败返回错误而不是半份列表。
func TestZAManagerListAccountsMergesOnline(t *testing.T) {
	store := newFakeZAStore()
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	store.seedAt("b", "uid-b-db", []byte(mgrTestCredJSON), at)
	store.seedAt("a", "uid-a-db", []byte(mgrTestCredJSON), at)

	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-a-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	if _, err := m.StartClient(context.Background(), "a"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}

	list, err := m.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("账号数 %d，期望 2", len(list))
	}
	if list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("排序错误: %+v", list)
	}
	if !list[0].LoggedIn || list[0].UID != "uid-a-live" {
		t.Fatalf("在线账号状态错误: %+v", list[0])
	}
	if list[0].UpdatedAt.IsZero() {
		t.Fatal("在线账号的 UpdatedAt 应来自库时间")
	}
	if list[1].LoggedIn || list[1].UID != "uid-b-db" {
		t.Fatalf("离线账号状态错误: %+v", list[1])
	}
	if list[1].UpdatedAt != at {
		t.Fatalf("离线账号 UpdatedAt=%v，期望库时间 %v", list[1].UpdatedAt, at)
	}

	store.mu.Lock()
	store.listErr = errMgrBoom
	store.mu.Unlock()
	got, err := m.ListAccounts(context.Background())
	if !errors.Is(err, errMgrBoom) {
		t.Fatalf("查库失败 err=%v", err)
	}
	if got != nil {
		t.Fatalf("查库失败返回了半份列表: %+v", got)
	}
}

// TestZAManagerLogoutAndDeleteIdempotent 覆盖登出保留库行、删除移除库行/内存，
// 以及两者的幂等与未知账号语义。
func TestZAManagerLogoutAndDeleteIdempotent(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)
	ctx := context.Background()

	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("StartClient 后账号不在内存")
	}
	conn, _ := acc.online()
	if conn == nil {
		t.Fatal("StartClient 后没有 conn")
	}
	live := conn.(*fakeZAConn)

	if err := m.Logout(ctx, "acct"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if acc.currentConn() != nil {
		t.Fatal("Logout 后 conn 未清")
	}
	if live.stopCount() == 0 {
		t.Fatal("Logout 未停 listener")
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("Logout 不应删除库行")
	}
	if err := m.Logout(ctx, "acct"); err != nil {
		t.Fatalf("重复 Logout: %v", err)
	}
	if err := m.Logout(ctx, "unknown"); err != nil {
		t.Fatalf("未知账号 Logout 返回错误: %v", err)
	}

	// 登出后可以再次按需登录。
	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("登出后 StartClient: %v", err)
	}
	if got := sdk.loginCount(); got != 2 {
		t.Fatalf("登录次数 %d，期望 2", got)
	}

	if err := m.DeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("DeleteAccount 后账号仍在内存")
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("DeleteAccount 未删除库行")
	}
	if err := m.DeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("重复 DeleteAccount: %v", err)
	}
	if err := m.DeleteAccount(ctx, ".."); !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("非法 ID DeleteAccount err=%v", err)
	}
}

// TestZAManagerDeleteAccountStoreError 断言删库失败返回错误且账号保留（已停），
// 清掉故障后重试收敛。
func TestZAManagerDeleteAccountStoreError(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	m := mgrNewTestManager(t, store, newFakeZASDK(), nil)
	ctx := context.Background()

	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}
	store.mu.Lock()
	store.deleteErr = errMgrBoom
	store.mu.Unlock()

	if err := m.DeleteAccount(ctx, "acct"); !errors.Is(err, errMgrBoom) {
		t.Fatalf("DeleteAccount err=%v，期望注入错误", err)
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("删库失败后账号被移出内存")
	}
	if acc.currentConn() != nil {
		t.Fatal("删库失败后仍留着 conn")
	}

	store.mu.Lock()
	store.deleteErr = nil
	store.mu.Unlock()
	if err := m.DeleteAccount(ctx, "acct"); err != nil {
		t.Fatalf("重试 DeleteAccount: %v", err)
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("重试后库行仍在")
	}
}

// TestZAManagerLoginWithCookie 断言 cookie 导入：写库成功才安装 conn，
// 返回在线信息，并触发 QR 取消钩子。
func TestZAManagerLoginWithCookie(t *testing.T) {
	store := newFakeZAStore()
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-cookie", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	var qrCanceled []string
	m.qrCancel = func(id string) { qrCanceled = append(qrCanceled, id) }

	raw := []byte(`[{"name":"zpw_sek","value":"not-a-real-token","domain":"zalo.me"}]`)
	info, err := m.LoginWithCookie(context.Background(), "acct", raw)
	if err != nil {
		t.Fatalf("LoginWithCookie: %v", err)
	}
	if info == nil || !info.LoggedIn || info.ID != "acct" || info.UID != "uid-cookie" {
		t.Fatalf("返回信息不符: %+v", info)
	}
	rec, ok := store.record("acct")
	if !ok {
		t.Fatal("导入后库中没有会话行")
	}
	if rec.UID != "uid-cookie" {
		t.Fatalf("落库 UID=%q", rec.UID)
	}
	var parsed zcago.Credentials
	if err := json.Unmarshal(rec.Credentials, &parsed); err != nil {
		t.Fatalf("落库凭证不是 Credentials JSON: %v", err)
	}
	if parsed.IMEI != "snap-imei" {
		t.Fatalf("落库凭证应取会话快照的 IMEI，实得 %q", parsed.IMEI)
	}
	acc, ok := m.account("acct")
	if !ok || acc.currentConn() == nil {
		t.Fatal("导入后没有安装 conn")
	}
	if len(qrCanceled) != 1 || qrCanceled[0] != "acct" {
		t.Fatalf("QR 取消钩子调用=%v", qrCanceled)
	}
}

// TestZAManagerLoginWithCookieBadInput 断言解析失败不打接口、不创建账号。
func TestZAManagerLoginWithCookieBadInput(t *testing.T) {
	store := newFakeZAStore()
	sdk := newFakeZASDK()
	m := mgrNewTestManager(t, store, sdk, nil)
	ctx := context.Background()

	if _, err := m.LoginWithCookie(ctx, "acct", []byte(`{"nope":true}`)); !errors.Is(err, ErrZaloBadCredentials) {
		t.Fatalf("坏凭证 err=%v，期望 ErrZaloBadCredentials", err)
	}
	if sdk.loginCount() != 0 {
		t.Fatal("解析失败仍调了登录")
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("解析失败仍创建了账号")
	}

	valid := []byte(`[{"name":"zpw_sek","value":"x","domain":"zalo.me"}]`)
	if _, err := m.LoginWithCookie(ctx, "..", valid); !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("非法 ID err=%v，期望 ErrZaloBadAccountID", err)
	}
	if sdk.loginCount() != 0 {
		t.Fatal("非法 ID 仍调了登录")
	}
}

// TestZAManagerInboundBroadcast 断言 listener 回调把 AccountID 填成本账号并广播
// 到 CallZaloMsg+"Inbound"。
func TestZAManagerInboundBroadcast(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	conn, ok := acc.currentConn().(*fakeZAConn)
	if !ok {
		t.Fatal("当前 conn 不是 fake")
	}
	mgrWaitTrue(t, "listener 启动", func() bool { return conn.startCount() == 1 })

	key := util.CallZaloMsg + "Inbound"
	got := make(chan InboundMessage, 4)
	id := util.DefaultCallFunc.Register(key, func(msg *InboundMessage) { got <- *msg })
	defer util.DefaultCallFunc.Unregister(key, id)

	conn.emitMessage(InboundMessage{ThreadID: "peer-1", ThreadType: "user", Text: "hello"})
	select {
	case msg := <-got:
		if msg.AccountID != "acct" {
			t.Fatalf("AccountID=%q，期望 acct", msg.AccountID)
		}
		if msg.ThreadID != "peer-1" || msg.Text != "hello" {
			t.Fatalf("广播载荷被改写: %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Inbound 回调未收到消息")
	}
}

// TestZAManagerListenerTerminalInvalidatesConn 断言 3003/3000 清 conn 让下次
// 收发重新登录，普通关闭码不清 conn。
func TestZAManagerListenerTerminalInvalidatesConn(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	first := acc.currentConn().(*fakeZAConn)
	mgrWaitTrue(t, "listener 启动", func() bool { return first.startCount() == 1 })

	first.emitTerminal(3003, "kicked")
	if acc.currentConn() != nil {
		t.Fatal("被踢后 conn 未清")
	}

	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("被踢后重登: %v", err)
	}
	second := acc.currentConn().(*fakeZAConn)
	if second == first {
		t.Fatal("重登应换新 conn")
	}
	mgrWaitTrue(t, "第二个 listener 启动", func() bool { return second.startCount() == 1 })

	second.emitTerminal(1000, "normal-close")
	if acc.currentConn() != second {
		t.Fatal("普通关闭码不应清 conn")
	}
}

// TestZAManagerGroupVersionDiff 覆盖：版本不变不调 GetGroupInfo、变化/新增才调、
// 移除项丢弃、缺资料返回错误且不更新版本表。
func TestZAManagerGroupVersionDiff(t *testing.T) {
	clock := newCacheClock()
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, clock)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	conn := acc.currentConn().(*fakeZAConn)
	ctx := context.Background()

	conn.setGroups(map[string]string{"g1": "v1", "g2": "v1"})
	conn.setGroupInfo(map[string]GroupInfo{
		"g1": {GroupID: "g1", Name: "Beta"},
		"g2": {GroupID: "g2", Name: "Alpha"},
	})

	got, err := acc.listGroups(ctx, false)
	if err != nil {
		t.Fatalf("首次 listGroups: %v", err)
	}
	if len(got) != 2 || got[0].GroupID != "g2" || got[1].GroupID != "g1" {
		t.Fatalf("首次结果/排序不符: %+v", got)
	}
	if got[0].Version != "v1" || got[1].Version != "v1" {
		t.Fatalf("Version 未填: %+v", got)
	}
	if reqs := conn.groupInfoRequests(); len(reqs) != 1 || len(reqs[0]) != 2 {
		t.Fatalf("首次应一次批量请求两个群: %v", reqs)
	}

	// TTL 内命中：不再打任何接口。
	if _, err := acc.listGroups(ctx, false); err != nil {
		t.Fatalf("二次 listGroups: %v", err)
	}
	if conn.groupsCallCount() != 1 || len(conn.groupInfoRequests()) != 1 {
		t.Fatalf("TTL 内又打了接口: groups=%d info=%v", conn.groupsCallCount(), conn.groupInfoRequests())
	}

	// TTL 过期但版本未变：只刷新版本表，不调 GetGroupInfo（资料缓存仍新鲜）。
	clock.Advance(5 * time.Minute)
	if _, err := acc.listGroups(ctx, false); err != nil {
		t.Fatalf("版本不变刷新: %v", err)
	}
	if conn.groupsCallCount() != 2 {
		t.Fatalf("GetAllGroups 调用 %d 次，期望 2", conn.groupsCallCount())
	}
	if len(conn.groupInfoRequests()) != 1 {
		t.Fatalf("版本不变却调了 GetGroupInfo: %v", conn.groupInfoRequests())
	}

	// 版本变化 + 新增 + 移除：只请变化的 g1 与新 g3，g2 从资料缓存丢弃。
	conn.setGroups(map[string]string{"g1": "v2", "g3": "v1"})
	conn.setGroupInfo(map[string]GroupInfo{
		"g1": {GroupID: "g1", Name: "Beta2"},
		"g3": {GroupID: "g3", Name: "Gamma"},
	})
	clock.Advance(5 * time.Minute)
	got, err = acc.listGroups(ctx, true)
	if err != nil {
		t.Fatalf("版本变化刷新: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("版本变化后群数=%d，期望 2: %+v", len(got), got)
	}
	for _, g := range got {
		if g.GroupID == "g2" {
			t.Fatalf("已移除的群仍在结果里: %+v", g)
		}
		if g.GroupID == "g1" && g.Version != "v2" {
			t.Fatalf("g1 Version=%q，期望 v2", g.Version)
		}
	}
	reqs := conn.groupInfoRequests()
	if len(reqs) != 2 {
		t.Fatalf("GetGroupInfo 调用 %d 次，期望 2", len(reqs))
	}
	if !reflect.DeepEqual(reqs[1], []string{"g1", "g3"}) {
		t.Fatalf("第二轮请求 ID=%v，期望 [g1 g3]", reqs[1])
	}
	if _, ok := acc.infoCache.CachedGroup("g2", m.limits.infoTTL, clock.Now()); ok {
		t.Fatal("已移除群的资料仍在缓存")
	}

	// 缺资料：返回错误、不交半份列表、不更新 groupVer。
	conn.setGroups(map[string]string{"g1": "v3"})
	conn.setGroupInfo(map[string]GroupInfo{})
	clock.Advance(5 * time.Minute)
	got, err = acc.listGroups(ctx, true)
	if err == nil {
		t.Fatal("缺资料应返回错误")
	}
	if got != nil {
		t.Fatalf("缺资料仍返回了半份列表: %+v", got)
	}
	if !strings.Contains(err.Error(), "群资料缺失") {
		t.Fatalf("缺资料错误文案=%q，期望包含「群资料缺失」", err.Error())
	}
	acc.groupVerMu.Lock()
	ver := make(map[string]string, len(acc.groupVer))
	for id, v := range acc.groupVer {
		ver[id] = v
	}
	acc.groupVerMu.Unlock()
	if !reflect.DeepEqual(ver, map[string]string{"g1": "v2", "g3": "v1"}) {
		t.Fatalf("失败刷新污染了 groupVer: %v", ver)
	}
}

// TestZAManagerListFriendsCacheWindow 断言好友列表 5 分钟内第二次不拉取，
// 强刷也受 5 分钟闸；刷新失败返回错误而不是空列表。
func TestZAManagerListFriendsCacheWindow(t *testing.T) {
	clock := newCacheClock()
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, clock)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	if _, err := acc.ensureConn(context.Background()); err != nil {
		t.Fatalf("ensureConn: %v", err)
	}
	conn := acc.currentConn().(*fakeZAConn)
	conn.setFriends([]UserInfo{{UserID: "u1", DisplayName: "Alice"}})
	ctx := context.Background()

	first, err := acc.listFriends(ctx, false)
	if err != nil {
		t.Fatalf("首次 listFriends: %v", err)
	}
	if len(first) != 1 || first[0].UserID != "u1" {
		t.Fatalf("首次结果不符: %+v", first)
	}
	if _, err := acc.listFriends(ctx, false); err != nil {
		t.Fatalf("二次 listFriends: %v", err)
	}
	if got := conn.friendsCallCount(); got != 1 {
		t.Fatalf("5 分钟内第二次仍拉取，调用 %d 次", got)
	}

	clock.Advance(5 * time.Minute)
	if _, err := acc.listFriends(ctx, true); err != nil {
		t.Fatalf("过闸后强刷: %v", err)
	}
	if got := conn.friendsCallCount(); got != 2 {
		t.Fatalf("过闸后强刷未拉取，调用 %d 次", got)
	}
	if _, err := acc.listFriends(ctx, true); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("5 分钟内重复强刷 err=%v，期望 ErrZaloFetchTooSoon", err)
	}
	if got := conn.friendsCallCount(); got != 2 {
		t.Fatalf("被闸拒绝仍拉取，调用 %d 次", got)
	}

	conn.setFriendsErr(errMgrBoom)
	clock.Advance(5 * time.Minute)
	got, err := acc.listFriends(ctx, true)
	if !errors.Is(err, errMgrBoom) {
		t.Fatalf("刷新失败 err=%v", err)
	}
	if got != nil {
		t.Fatalf("刷新失败返回了列表: %+v", got)
	}
}

// TestZALimitsFromConfig 表驱动覆盖缺省值与下限 clamp。
func TestZALimitsFromConfig(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name string
		cfg  *config.ZaloSessionsConfig
		want zaLimits
	}{
		{
			name: "nil_config",
			cfg:  nil,
			want: zaLimits{
				qrTTL: 5 * time.Minute, requestTimeout: 30 * time.Second, listener: false,
				friendsTTL: 1 * time.Minute, groupsTTL: 1 * time.Minute,
				infoMin: 1500 * time.Millisecond, infoTTL: 10 * time.Minute,
				sendMin: 3 * time.Second, sendMax: 5 * time.Second, sendMaxPerHour: 30,
			},
		},
		{
			name: "below_floors",
			cfg: &config.ZaloSessionsConfig{
				QRTTLSec: 10, RequestTimeoutSec: 1, ListenerEnabled: boolPtr(false),
				FriendsCacheTTLSec: 60, GroupsCacheTTLSec: 60,
				InfoMinIntervalMs: 100, InfoCacheTTLSec: 60,
				SendMinDelayMs: 100, SendMaxDelayMs: 200, SendMaxPerHour: -1,
				UserAgent: "  ua-x  ", Language: " vi ",
			},
			want: zaLimits{
				qrTTL: 2 * time.Minute, requestTimeout: 5 * time.Second, listener: false,
				friendsTTL: 1 * time.Minute, groupsTTL: 1 * time.Minute,
				infoMin: 1500 * time.Millisecond, infoTTL: 10 * time.Minute,
				sendMin: 1 * time.Second, sendMax: 1 * time.Second, sendMaxPerHour: 30,
				userAgent: "ua-x", language: "vi",
			},
		},
		{
			name: "explicit_values",
			cfg: &config.ZaloSessionsConfig{
				QRTTLSec: 600, RequestTimeoutSec: 60, ListenerEnabled: boolPtr(true),
				FriendsCacheTTLSec: 900, GroupsCacheTTLSec: 1200,
				InfoMinIntervalMs: 2000, InfoCacheTTLSec: 1800,
				SendMinDelayMs: 4000, SendMaxDelayMs: 7000, SendMaxPerHour: 50,
				UserAgent: "ua-y", Language: "en",
			},
			want: zaLimits{
				qrTTL: 10 * time.Minute, requestTimeout: time.Minute, listener: true,
				friendsTTL: 15 * time.Minute, groupsTTL: 20 * time.Minute,
				infoMin: 2 * time.Second, infoTTL: 30 * time.Minute,
				sendMin: 4 * time.Second, sendMax: 7 * time.Second, sendMaxPerHour: 50,
				userAgent: "ua-y", language: "en",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := zaLimitsFromConfig(tc.cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("zaLimitsFromConfig=%+v，期望 %+v", got, tc.want)
			}
		})
	}
}

// TestZARandomDelay 断言 max<=min 时返回 min，其余落在闭区间内。
func TestZARandomDelay(t *testing.T) {
	if got := zaRandomDelay(2*time.Second, time.Second); got != 2*time.Second {
		t.Fatalf("max<min 返回 %v，期望 min", got)
	}
	if got := zaRandomDelay(time.Second, time.Second); got != time.Second {
		t.Fatalf("max==min 返回 %v，期望 min", got)
	}
	for i := 0; i < 500; i++ {
		d := zaRandomDelay(3*time.Second, 5*time.Second)
		if d < 3*time.Second || d > 5*time.Second {
			t.Fatalf("随机间隔越界: %v", d)
		}
	}
}

// TestZACurrentManager 断言未启动返回哨兵、存入后可读，且构造器补全 nil 依赖。
func TestZACurrentManager(t *testing.T) {
	old := zaMgr.Load()
	zaMgr.Store(nil)
	defer zaMgr.Store(old)

	if _, err := currentManager(); !errors.Is(err, ErrZaloNotStarted) {
		t.Fatalf("未启动 currentManager err=%v", err)
	}

	store := newFakeZAStore()
	m := newZAManager(nil, mgrTestLimits(), store, newFakeZASDK(), nil, nil)
	defer m.Stop()
	if m.root == nil || m.clock == nil || m.randDelay == nil || m.cancel == nil {
		t.Fatal("newZAManager 未补全 nil 依赖")
	}

	zaMgr.Store(m)
	got, err := currentManager()
	if err != nil || got != m {
		t.Fatalf("currentManager=%v err=%v，期望存入的 manager", got, err)
	}
}

// errPairBoom 是扫码测试注入的通用失败，用于断言安全文案不泄漏底层错误。
var errPairBoom = errors.New("pair test boom")

// pairFakeSDK 是可编程的 zaSDK 假件：LoginQR 把 option/callback 交给测试，
// 并按 block/hold/conn/loginErr 决定返回时机。Login 不在本文件使用。
type pairFakeSDK struct {
	mu       sync.Mutex
	qrCalls  int
	opt      *zcago.LoginQROption
	cb       zcago.LoginQRCallback
	conn     zaConn
	loginErr error
	block    bool
	hold     chan struct{}
	relOnce  sync.Once
}

// Login 不在扫码测试范围内。
func (s *pairFakeSDK) Login(context.Context, zcago.Credentials) (zaConn, error) {
	return nil, errPairBoom
}

// LoginQR 记录调用并等待测试放行（hold）或 ctx 结束（block）。
func (s *pairFakeSDK) LoginQR(ctx context.Context, opt *zcago.LoginQROption, cb zcago.LoginQRCallback) (zaConn, error) {
	s.mu.Lock()
	s.qrCalls++
	s.opt = opt
	s.cb = cb
	conn, retErr, block, hold := s.conn, s.loginErr, s.block, s.hold
	s.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return conn, retErr
}

// release 放行被 hold 的 LoginQR（可重复调用）。
func (s *pairFakeSDK) release() {
	s.relOnce.Do(func() {
		s.mu.Lock()
		h := s.hold
		s.mu.Unlock()
		if h != nil {
			close(h)
		}
	})
}

// qrCount 返回 LoginQR 调用次数。
func (s *pairFakeSDK) qrCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qrCalls
}

// pairFakeActions 实现 auth.GeneratedActions/CommonActions，记录 Retry/Abort 次数。
// zcago 的 qrController 未导出，测试用这个假件塞进事件里即可。
type pairFakeActions struct {
	mu      sync.Mutex
	retries int
	aborts  int
}

func (a *pairFakeActions) Retry(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retries++
	return nil
}

func (a *pairFakeActions) Abort(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborts++
	return nil
}

func (a *pairFakeActions) SaveToFile(context.Context, string) error { return nil }

// counts 返回 Retry/Abort 次数。
func (a *pairFakeActions) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.retries, a.aborts
}

// pairNewManager 构造带停机清理的 manager；qrTTL>0 时覆盖缺省 TTL。
func pairNewManager(t *testing.T, store *fakeZAStore, sdk *pairFakeSDK, qrTTL time.Duration) *zaManager {
	t.Helper()
	lim := mgrTestLimits()
	if qrTTL > 0 {
		lim.qrTTL = qrTTL
	}
	m := newZAManager(context.Background(), lim, store, sdk, nil, nil)
	t.Cleanup(m.Stop)
	return m
}

// pairInstallManager 把 manager 存进进程级快照（ZaloStartPairing 经 currentManager 取）。
func pairInstallManager(t *testing.T, m *zaManager) {
	t.Helper()
	old := zaMgr.Load()
	zaMgr.Store(m)
	t.Cleanup(func() { zaMgr.Store(old) })
}

// pairStart 发起配对并取回会话对象。
func pairStart(t *testing.T, m *zaManager, accountID string) *QRSession {
	t.Helper()
	sid, err := ZaloStartPairing(context.Background(), accountID)
	if err != nil {
		t.Fatalf("ZaloStartPairing(%q): %v", accountID, err)
	}
	s, ok := m.LookupQRSession(sid)
	if !ok {
		t.Fatalf("刚创建的会话 %s 查不到", sid)
	}
	return s
}

// pairWaitCallback 等到 LoginQR 注册回调。
func pairWaitCallback(t *testing.T, sdk *pairFakeSDK) zcago.LoginQRCallback {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sdk.mu.Lock()
		cb := sdk.cb
		sdk.mu.Unlock()
		if cb != nil {
			return cb
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 LoginQR 注册回调超时")
		}
		runtime.Gosched()
	}
}

// pairNext 读一条事件；通道关闭返回 ok=false。
func pairNext(t *testing.T, ch <-chan QREvent) (QREvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("等待事件超时")
		return QREvent{}, false
	}
}

// pairDrain 读到通道关闭为止（终态后会话关闭订阅）。
func pairDrain(t *testing.T, ch <-chan QREvent) []QREvent {
	t.Helper()
	out := make([]QREvent, 0, 4)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("等待事件通道关闭超时（已收到 %d 条）", len(out))
		}
	}
}

// pairHistory 返回会话事件历史快照。
func pairHistory(s *QRSession) []QREvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QREvent(nil), s.history...)
}

// pairEventCount 统计历史里某类事件的条数。
func pairEventCount(events []QREvent, name string) int {
	n := 0
	for _, ev := range events {
		if ev.Event == name {
			n++
		}
	}
	return n
}

// pairQRCounts 返回活动表规模（qrByID, qrByAccount）。
func pairQRCounts(m *zaManager) (int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.qrByID), len(m.qrByAccount)
}

// TestZAPairingQRImageRawPNGBase64 断言 zcago 的原始 PNG 字节被原样 base64
// （含非 UTF-8 字节），而不是把图片当成已是 base64 再编码一次。
func TestZAPairingQRImageRawPNGBase64(t *testing.T) {
	sdk := &pairFakeSDK{hold: make(chan struct{})}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	t.Cleanup(sdk.release)

	sess := pairStart(t, m, "acct")
	cb := pairWaitCallback(t, sdk)
	sub, _ := sess.Subscribe()

	// 事件面同时覆盖 util.CallZaloMsg+"QR"（冻结 QREvent 注释承诺的广播口径）。
	pubKey := util.CallZaloMsg + "QR"
	pubCh := make(chan QREvent, 1)
	pubID := util.DefaultCallFunc.Register(pubKey, func(ev *QREvent) {
		select {
		case pubCh <- *ev:
		default:
		}
	})
	defer util.DefaultCallFunc.Unregister(pubKey, pubID)

	// 前 8 字节是 PNG 魔数；0xff/0xfe/0x80 都不是合法 UTF-8 单字节，
	// 若实现走 string 再当 base64 解（或 JSON 直传）会在这里露馅。
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x7f, 0x80}
	cb(auth.EventQRCodeGenerated{Data: auth.QRGeneratedData{Code: "code-1", Image: string(raw)}})

	select {
	case pub := <-pubCh:
		if pub.Event != qrEventQR || pub.QRCode != "code-1" || pub.SessionID != sess.ID {
			t.Fatalf("QR 回调广播载荷不符: %+v", pub)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("util.CallZaloMsg+QR 未收到二维码事件")
	}

	ev, ok := pairNext(t, sub)
	if !ok {
		t.Fatal("收到通道关闭，期望二维码帧")
	}
	if ev.Event != qrEventQR || ev.QRCode != "code-1" {
		t.Fatalf("二维码帧不符: %+v", ev)
	}
	if ev.SessionID != sess.ID || ev.AccountID != "acct" {
		t.Fatalf("会话标识未填充: %+v", ev)
	}
	got, err := base64.StdEncoding.DecodeString(ev.ImageBase64)
	if err != nil {
		t.Fatalf("ImageBase64 不是合法 base64: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("base64 往返后字节不一致: got=%x want=%x", got, raw)
	}
	if !bytes.Contains(got, []byte{0xff, 0xfe}) || !bytes.Contains(got, []byte{0x80}) {
		t.Fatal("非 UTF-8 字节在编码中被改写")
	}
	if ev.TimeoutMS <= 0 {
		t.Fatalf("TimeoutMS=%d，期望正的剩余 TTL", ev.TimeoutMS)
	}

	sdk.mu.Lock()
	opt := sdk.opt
	sdk.mu.Unlock()
	if opt == nil || opt.UserAgent != "test-ua" || opt.Language != "vi" {
		t.Fatalf("LoginQROption=%+v，期望取自 limits 的 ua/vi", opt)
	}
}

// TestZAPairingLateSubscribeReplaysHistory 断言「先 emit 二维码、后 Subscribe」
// 的迟到连接仍能拿到二维码图片。
func TestZAPairingLateSubscribeReplaysHistory(t *testing.T) {
	sdk := &pairFakeSDK{hold: make(chan struct{})}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	t.Cleanup(sdk.release)

	sess := pairStart(t, m, "acct")
	cb := pairWaitCallback(t, sdk)
	raw := []byte("png-early")
	cb(auth.EventQRCodeGenerated{Data: auth.QRGeneratedData{Code: "code-early", Image: string(raw)}})
	mgrWaitTrue(t, "二维码事件入历史", func() bool { return len(pairHistory(sess)) == 1 })

	sub, _ := sess.Subscribe()
	ev, ok := pairNext(t, sub)
	if !ok {
		t.Fatal("迟到订阅拿到关闭通道，期望补投历史")
	}
	if ev.Event != qrEventQR || ev.QRCode != "code-early" {
		t.Fatalf("补投事件不符: %+v", ev)
	}
	if ev.ImageBase64 != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("补投的图片 base64=%q", ev.ImageBase64)
	}
}

// TestZAPairingExpiredRetriesAndDeclinedTerminal 覆盖两个回调控制流：
// expired 非终态且每个过期的码恰好 Retry 一次；declined 是终态且只发一次。
func TestZAPairingExpiredRetriesAndDeclinedTerminal(t *testing.T) {
	t.Run("expired_retries", func(t *testing.T) {
		sdk := &pairFakeSDK{hold: make(chan struct{})}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)
		t.Cleanup(sdk.release)

		sess := pairStart(t, m, "acct")
		cb := pairWaitCallback(t, sdk)
		sub, _ := sess.Subscribe()
		actions := &pairFakeActions{}

		cb(auth.EventQRCodeExpired{Actions: actions})
		ev, ok := pairNext(t, sub)
		if !ok || ev.Event != qrEventExpired {
			t.Fatalf("首个过期事件不符: %+v ok=%v", ev, ok)
		}
		mgrWaitTrue(t, "首个 Retry", func() bool { r, _ := actions.counts(); return r == 1 })
		if sess.isClosed() {
			t.Fatal("expired 是非终态，不应关闭会话")
		}

		cb(auth.EventQRCodeExpired{Actions: actions})
		ev, ok = pairNext(t, sub)
		if !ok || ev.Event != qrEventExpired {
			t.Fatalf("第二个过期事件不符: %+v ok=%v", ev, ok)
		}
		mgrWaitTrue(t, "第二个 Retry", func() bool { r, _ := actions.counts(); return r == 2 })
		if _, a := actions.counts(); a != 0 {
			t.Fatalf("expired 不应调 Abort，实得 %d 次", a)
		}
	})

	t.Run("declined_terminal_once", func(t *testing.T) {
		sdk := &pairFakeSDK{hold: make(chan struct{})}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)
		t.Cleanup(sdk.release)

		sess := pairStart(t, m, "acct")
		cb := pairWaitCallback(t, sdk)
		sub, _ := sess.Subscribe()
		actions := &pairFakeActions{}

		cb(auth.EventQRCodeDeclined{Actions: actions})
		events := pairDrain(t, sub)
		if len(events) != 1 || events[0].Event != qrEventDeclined {
			t.Fatalf("declined 终态不符: %+v", events)
		}
		if r, a := actions.counts(); r != 0 || a != 1 {
			t.Fatalf("Retry/Abort=%d/%d，期望 0/1", r, a)
		}
		if !sess.isClosed() {
			t.Fatal("declined 后会话应关闭")
		}

		// 第二次 declined：终态只发一次，不得再进历史。
		cb(auth.EventQRCodeDeclined{Actions: actions})
		if n := pairEventCount(pairHistory(sess), qrEventDeclined); n != 1 {
			t.Fatalf("declined 终态出现 %d 次，期望 1", n)
		}
	})
}

// TestZAPairingTTLTimeout 断言会话 TTL 到点时 LoginQR 被 ctx 打断，
// 终态是 timeout（唯一一条），且会话从活动表摘除。
func TestZAPairingTTLTimeout(t *testing.T) {
	sdk := &pairFakeSDK{block: true}
	m := pairNewManager(t, newFakeZAStore(), sdk, 40*time.Millisecond)
	pairInstallManager(t, m)

	sess := pairStart(t, m, "acct")
	sub, _ := sess.Subscribe()
	events := pairDrain(t, sub)
	if len(events) != 1 || events[0].Event != qrEventTimeout {
		t.Fatalf("TTL 终态不符: %+v", events)
	}
	if events[0].Error != "" || events[0].QRCode != "" {
		t.Fatalf("timeout 事件不应带错误/二维码: %+v", events[0])
	}
	if !errors.Is(sess.ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("会话 ctx err=%v，期望 DeadlineExceeded", sess.ctx.Err())
	}
	mgrWaitTrue(t, "会话从活动表移除", func() bool {
		_, ok := m.LookupQRSession(sess.ID)
		return !ok
	})
}

// TestZAPairingAccountIdempotentAndBusy 断言同账号二次 Pairing 返回原 sessionID、
// 不同账号互不影响、已在线账号返回 ErrZaloAccountBusy，以及非法账号 ID 透传。
func TestZAPairingAccountIdempotentAndBusy(t *testing.T) {
	sdk := &pairFakeSDK{hold: make(chan struct{})}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	t.Cleanup(sdk.release)

	sess := pairStart(t, m, "acct")
	mgrWaitTrue(t, "LoginQR 进入", func() bool { return sdk.qrCount() == 1 })

	sid2, err := ZaloStartPairing(context.Background(), "acct")
	if err != nil {
		t.Fatalf("同账号二次 Pairing: %v", err)
	}
	if sid2 != sess.ID {
		t.Fatalf("同账号二次 Pairing 返回新 id=%s，期望原 id=%s", sid2, sess.ID)
	}
	if got := sdk.qrCount(); got != 1 {
		t.Fatalf("幂等路径仍新建了 LoginQR，调用 %d 次", got)
	}

	if other := pairStart(t, m, "other"); other.ID == sess.ID {
		t.Fatal("不同账号复用了同一个 sessionID")
	}

	if _, err := ZaloStartPairing(context.Background(), ".."); !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("非法账号 err=%v，期望 ErrZaloBadAccountID", err)
	}

	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("账号不在内存")
	}
	acc.opMu.Lock()
	installed := acc.installConnLocked(&fakeZAConn{uid: "uid-live"})
	acc.opMu.Unlock()
	if !installed {
		t.Fatal("预置在线 conn 失败")
	}
	if _, err := ZaloStartPairing(context.Background(), "acct"); !errors.Is(err, ErrZaloAccountBusy) {
		t.Fatalf("已在线账号 Pairing err=%v，期望 ErrZaloAccountBusy", err)
	}
}

// TestZAPairingSuccessInstallsConn 断言成功路径：落库、安装 conn、success 带 UID、
// 会话从活动表摘除。
func TestZAPairingSuccessInstallsConn(t *testing.T) {
	store := newFakeZAStore()
	conn := &fakeZAConn{uid: "uid-qr", snap: zcago.Credentials{IMEI: "imei-snap", UserAgent: "ua-snap"}}
	sdk := &pairFakeSDK{conn: conn, hold: make(chan struct{})}
	m := pairNewManager(t, store, sdk, 0)
	pairInstallManager(t, m)

	sess := pairStart(t, m, "acct")
	sub, _ := sess.Subscribe()
	sdk.release()

	events := pairDrain(t, sub)
	if len(events) != 1 || events[0].Event != qrEventSuccess || events[0].UID != "uid-qr" {
		t.Fatalf("success 终态不符: %+v", events)
	}
	if store.upsertCount() != 1 {
		t.Fatalf("store.Upsert 调用 %d 次，期望 1", store.upsertCount())
	}
	rec, ok := store.record("acct")
	if !ok || rec.UID != "uid-qr" {
		t.Fatalf("落库行不符: %+v ok=%v", rec, ok)
	}
	var parsed zcago.Credentials
	if err := json.Unmarshal(rec.Credentials, &parsed); err != nil {
		t.Fatalf("落库凭证不是 Credentials JSON: %v", err)
	}
	if parsed.IMEI != "imei-snap" {
		t.Fatalf("落库 IMEI=%q，期望会话快照值", parsed.IMEI)
	}
	acc, ok := m.account("acct")
	if !ok || acc.currentConn() != conn {
		t.Fatal("成功后未安装 conn")
	}
	mgrWaitTrue(t, "会话从活动表移除", func() bool {
		_, found := m.LookupQRSession(sess.ID)
		return !found
	})
}

// TestZAPairingUpsertFailureNoInstall 断言写库失败发安全文案的 error 终态、
// 不安装 conn，且底层错误不进事件。
func TestZAPairingUpsertFailureNoInstall(t *testing.T) {
	store := newFakeZAStore()
	store.upsertErr = errPairBoom
	conn := &fakeZAConn{uid: "uid-qr", snap: zcago.Credentials{IMEI: "imei-snap", UserAgent: "ua-snap"}}
	sdk := &pairFakeSDK{conn: conn, hold: make(chan struct{})}
	m := pairNewManager(t, store, sdk, 0)
	pairInstallManager(t, m)

	sess := pairStart(t, m, "acct")
	sub, _ := sess.Subscribe()
	sdk.release()

	events := pairDrain(t, sub)
	if len(events) != 1 || events[0].Event != qrEventError {
		t.Fatalf("写库失败终态不符: %+v", events)
	}
	if events[0].Error != qrSafeLoginError {
		t.Fatalf("Error=%q，期望固定安全文案 %q", events[0].Error, qrSafeLoginError)
	}
	if strings.Contains(events[0].Error, "boom") {
		t.Fatal("底层错误泄漏进了终态事件")
	}
	if store.upsertCount() != 1 {
		t.Fatalf("store.Upsert 调用 %d 次，期望 1", store.upsertCount())
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("账号不在内存")
	}
	if acc.currentConn() != nil {
		t.Fatal("写库失败仍安装了 conn")
	}
}

// TestZAPairingCancelAndStopClosesSessions 断言运行中取消/停机都会关闭会话、
// 清空两张活动表，且不向订阅者发误导性终态。
func TestZAPairingCancelAndStopClosesSessions(t *testing.T) {
	t.Run("cancel_by_account", func(t *testing.T) {
		sdk := &pairFakeSDK{block: true}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)

		sess := pairStart(t, m, "acct")
		sub, _ := sess.Subscribe()
		m.cancelQRByAccount("acct")

		if events := pairDrain(t, sub); len(events) != 0 {
			t.Fatalf("取消不应发终态，实得 %+v", events)
		}
		if !sess.isClosed() {
			t.Fatal("取消后会话未关闭")
		}
		if ids, accs := pairQRCounts(m); ids != 0 || accs != 0 {
			t.Fatalf("取消后活动表未清空: byID=%d byAccount=%d", ids, accs)
		}
	})

	t.Run("stop_closes_all", func(t *testing.T) {
		sdk := &pairFakeSDK{block: true}
		m := pairNewManager(t, newFakeZAStore(), sdk, 0)
		pairInstallManager(t, m)

		sess := pairStart(t, m, "acct")
		sub, _ := sess.Subscribe()
		m.Stop()

		if events := pairDrain(t, sub); len(events) != 0 {
			t.Fatalf("停机不应发终态，实得 %+v", events)
		}
		if !sess.isClosed() {
			t.Fatal("停机后会话未关闭")
		}
		if ids, accs := pairQRCounts(m); ids != 0 || accs != 0 {
			t.Fatalf("停产后活动表未清空: byID=%d byAccount=%d", ids, accs)
		}
	})
}

// TestZANewZASDK 断言工厂返回可用的真实现。
func TestZANewZASDK(t *testing.T) {
	t.Parallel()

	sdk := newZASDK()
	if sdk == nil {
		t.Fatal("newZASDK 返回 nil")
	}
	if _, ok := sdk.(*zaRealSDK); !ok {
		t.Fatalf("newZASDK 类型=%T，期望 *zaRealSDK", sdk)
	}
}

// TestZANewZAHTTPClientIsolated 钉住 P0：每次登录必须拿到独立的 client 与
// cookie jar。zcago 默认 client 是 http.DefaultClient，且 newContextImpl 会把
// 新建 jar 写回它（session/options.go:70、context_impl.go:48-53）；共用会让
// 第二个账号复用第一个账号的 cookie。
func TestZANewZAHTTPClientIsolated(t *testing.T) {
	t.Parallel()

	c1, c2 := newZAHTTPClient(), newZAHTTPClient()
	if c1 == c2 {
		t.Fatal("两次登录复用了同一个 http.Client")
	}
	if c1.Jar == nil || c2.Jar == nil {
		t.Fatal("私有 client 必须带 cookie jar")
	}
	if c1.Jar == c2.Jar {
		t.Fatal("两次登录复用了同一个 cookie jar")
	}
	u := &url.URL{Scheme: "https", Host: "chat.zalo.me"}
	c1.Jar.SetCookies(u, []*http.Cookie{{Name: "secret", Value: "a", Path: "/", Secure: true}})
	if got := c2.Jar.Cookies(u); len(got) != 0 {
		t.Fatalf("账号 A 的 cookie 泄漏到账号 B 的 jar: %v", got)
	}
}

// TestZAMapZAUser 断言 model.User → UserInfo 的字段一一对应。
func TestZAMapZAUser(t *testing.T) {
	t.Parallel()

	in := model.User{
		UserId:      "u-1",
		Username:    "alice",
		DisplayName: "Alice A.",
		ZaloName:    "Alice Zalo",
		PhoneNumber: "+84000000000",
		Avatar:      "https://example.invalid/avatar.png",
		Status:      "active",
	}
	want := UserInfo{
		UserID:      "u-1",
		Username:    "alice",
		DisplayName: "Alice A.",
		ZaloName:    "Alice Zalo",
		Phone:       "+84000000000",
		Avatar:      "https://example.invalid/avatar.png",
		Status:      "active",
	}

	if got := mapZAUser(in); got != want {
		t.Fatalf("mapZAUser=%+v，期望 %+v", got, want)
	}
}

// TestZAMapZAUsersSort 断言按 UserID 升序，且不依赖输入顺序。
func TestZAMapZAUsersSort(t *testing.T) {
	t.Parallel()

	in := []model.User{
		{UserId: "u-3", DisplayName: "C"},
		{UserId: "u-1", DisplayName: "A"},
		{UserId: "u-2", DisplayName: "B"},
	}
	got := mapZAUsers(in)
	wantIDs := []string{"u-1", "u-2", "u-3"}
	gotIDs := make([]string, 0, len(got))
	for _, u := range got {
		gotIDs = append(gotIDs, u.UserID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("mapZAUsers 顺序=%v，期望 %v", gotIDs, wantIDs)
	}
	// 输入切片的顺序不能被改写（排序只作用于新切片）。
	if in[0].UserId != "u-3" || in[2].UserId != "u-2" {
		t.Fatalf("mapZAUsers 改写了输入序列：%+v", in)
	}
}

// TestZAMapZAUsersEmpty 断言空输入的返回是空切片而不是 nil，调用方可直接序列化。
func TestZAMapZAUsersEmpty(t *testing.T) {
	t.Parallel()

	if got := mapZAUsers(nil); got == nil {
		t.Fatal("mapZAUsers(nil) 返回 nil，期望空切片")
	} else if len(got) != 0 {
		t.Fatalf("mapZAUsers(nil) 长度=%d，期望 0", len(got))
	}
}

// TestZAMapZAGroupFields 断言群字段映射，含 Type 转 int、LockSendMsg 的 0/非 0 口径。
func TestZAMapZAGroupFields(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		lock      int
		wantLock  bool
		groupType model.GroupType
	}{
		{name: "unlocked", lock: 0, wantLock: false, groupType: model.GroupTypeGroup},
		{name: "locked", lock: 1, wantLock: true, groupType: model.GroupTypeCommunity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := zcagoapi.GroupInfo{
				GroupInfo: model.GroupInfo{
					GroupID:     "g-1",
					Name:        "Group One",
					Description: "desc",
					CreatorID:   "u-owner",
					Avatar:      "https://example.invalid/g.png",
					Version:     "ver-9",
					TotalMember: 12,
					MaxMember:   200,
					Type:        tc.groupType,
					AdminIDs:    []string{"u-admin-1", "u-admin-2"},
					Setting:     model.GroupSetting{LockSendMsg: tc.lock},
				},
			}
			want := GroupInfo{
				GroupID:     "g-1",
				Name:        "Group One",
				Description: "desc",
				CreatorID:   "u-owner",
				Avatar:      "https://example.invalid/g.png",
				Version:     "ver-9",
				TotalMember: 12,
				MaxMember:   200,
				Type:        int(tc.groupType),
				AdminIDs:    []string{"u-admin-1", "u-admin-2"},
				LockSendMsg: tc.wantLock,
			}

			if got := mapZAGroup(in); !reflect.DeepEqual(got, want) {
				t.Fatalf("mapZAGroup=%+v，期望 %+v", got, want)
			}
		})
	}
}

// TestZAMapZAGroupsKeyFallback 断言 map 键在 GroupID 为空时补齐，
// 内嵌 GroupID 非空时以它为准，结果按 GroupID 升序。
func TestZAMapZAGroupsKeyFallback(t *testing.T) {
	t.Parallel()

	in := map[string]zcagoapi.GroupInfo{
		"g-key-2": {GroupInfo: model.GroupInfo{Name: "fallback"}},
		"g-key-1": {GroupInfo: model.GroupInfo{GroupID: "g-explicit", Name: "explicit"}},
	}
	got := mapZAGroups(in)
	if len(got) != 2 {
		t.Fatalf("mapZAGroups 长度=%d，期望 2", len(got))
	}
	if got[0].GroupID != "g-explicit" || got[0].Name != "explicit" {
		t.Fatalf("首个元素=%+v，期望 GroupID=g-explicit（显式值优先）", got[0])
	}
	if got[1].GroupID != "g-key-2" || got[1].Name != "fallback" {
		t.Fatalf("次个元素=%+v，期望 GroupID=g-key-2（map 键补齐）", got[1])
	}
}

// TestZAMapZAGroupAdminIDsDeepCopy 断言 AdminIDs 与输入切片互不影响。
func TestZAMapZAGroupAdminIDsDeepCopy(t *testing.T) {
	t.Parallel()

	admins := []string{"u-admin-1", "u-admin-2"}
	in := zcagoapi.GroupInfo{
		GroupInfo: model.GroupInfo{GroupID: "g-1", AdminIDs: admins},
	}
	got := mapZAGroup(in)
	if len(got.AdminIDs) != 2 {
		t.Fatalf("AdminIDs 长度=%d，期望 2", len(got.AdminIDs))
	}

	admins[0] = "mutated-input"
	if got.AdminIDs[0] != "u-admin-1" {
		t.Fatalf("修改输入污染了输出：AdminIDs[0]=%q", got.AdminIDs[0])
	}

	got.AdminIDs[1] = "mutated-output"
	if admins[1] != "u-admin-2" {
		t.Fatalf("修改输出污染了输入：admins[1]=%q", admins[1])
	}
}

// secRelease 把一次性 close 包成可重复调用的释放函数，避免测试失败路径泄漏
// 一个永远等 gate 的登录协程。
func secRelease(gate chan struct{}) func() {
	var once = make(chan struct{})
	return func() {
		select {
		case <-once:
			return
		default:
			close(once)
			close(gate)
		}
	}
}

// TestZASecLogoutDuringLoginNoInstall 在飞登录被 Logout 取代后，即使 SDK 最终
// 返回了 conn，也不能安装（代际检查），库行仍保留（Logout 的语义）。
func TestZASecLogoutDuringLoginNoInstall(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sdk.gate = gate
	sdk.entered = entered
	release := secRelease(gate)
	defer release()

	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	loginDone := make(chan error, 1)
	go func() {
		_, err := acc.ensureConn(context.Background())
		loginDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("登录未进入 SDK")
	}

	logoutDone := make(chan error, 1)
	go func() { logoutDone <- m.Logout(context.Background(), "acct") }()
	mgrWaitTrue(t, "Logout 递增登录代际", func() bool {
		acc.mu.Lock()
		defer acc.mu.Unlock()
		return acc.loginEpoch >= 1
	})

	release()
	if err := <-loginDone; err == nil {
		t.Fatal("被 Logout 取代的登录应返回错误")
	}
	if err := <-logoutDone; err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if acc.currentConn() != nil {
		t.Fatal("Logout 后仍安装了在飞登录的 conn")
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("Logout 不应删除会话库行")
	}
}

// TestZASecDeleteDuringLoginNoResurrect 在飞登录被 Delete 取代后不得复活库行，
// 也不得安装 conn。
func TestZASecDeleteDuringLoginNoResurrect(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sdk.gate = gate
	sdk.entered = entered
	release := secRelease(gate)
	defer release()

	m := mgrNewTestManager(t, store, sdk, nil)
	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}

	loginDone := make(chan error, 1)
	go func() {
		_, err := acc.ensureConn(context.Background())
		loginDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("登录未进入 SDK")
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- m.DeleteAccount(context.Background(), "acct") }()
	mgrWaitTrue(t, "Delete 标记 stopped", func() bool {
		acc.mu.Lock()
		defer acc.mu.Unlock()
		return acc.stopped
	})

	release()
	if err := <-loginDone; err == nil {
		t.Fatal("被 Delete 取代的登录应返回错误")
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, ok := m.account("acct"); ok {
		t.Fatal("删除成功后账号仍在内存")
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("删除后库行被在飞登录复活")
	}
	if acc.currentConn() != nil {
		t.Fatal("删除后仍安装了在飞登录的 conn")
	}
}

// TestZASecDeleteFailureAllowsReuse 删库失败必须解除 stopped：账号保留、可重新
// 登录使用，而不是在本进程里永久不可用。
func TestZASecDeleteFailureAllowsReuse(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	m := mgrNewTestManager(t, store, sdk, nil)
	ctx := context.Background()

	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("StartClient: %v", err)
	}
	store.mu.Lock()
	store.deleteErr = errMgrBoom
	store.mu.Unlock()
	if err := m.DeleteAccount(ctx, "acct"); !errors.Is(err, errMgrBoom) {
		t.Fatalf("DeleteAccount err=%v，期望注入错误", err)
	}

	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("删库失败后账号被移出内存")
	}
	if acc.stopped {
		t.Fatal("删库失败后账号仍处于 stopped，无法恢复")
	}
	if acc.currentConn() != nil {
		t.Fatal("删库失败后仍留着旧 conn")
	}

	store.mu.Lock()
	store.deleteErr = nil
	store.mu.Unlock()
	if _, err := m.StartClient(ctx, "acct"); err != nil {
		t.Fatalf("删库失败后重新登录: %v", err)
	}
	if acc.currentConn() == nil {
		t.Fatal("重新登录后没有 conn")
	}
}

// secBlockClock 在 Sleep 中阻塞到测试释放；用于把发送卡在 3-5 秒间隔里。
type secBlockClock struct {
	*cacheClock
	entered chan struct{}
	release chan struct{}
}

func newSecBlockClock() *secBlockClock {
	return &secBlockClock{
		cacheClock: newCacheClock(),
		entered:    make(chan struct{}, 1),
		release:    make(chan struct{}),
	}
}

func (c *secBlockClock) Sleep(ctx context.Context, _ time.Duration) error {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return nil
	}
}

// TestZASecSendAfterLogoutRejected 发送卡在间隔睡眠时登出：醒来后必须复核 conn，
// 不得再用旧 conn 发出第二条。
func TestZASecSendAfterLogoutRejected(t *testing.T) {
	clock := newSecBlockClock()
	limits := mgrTestLimits()
	limits.sendMin = time.Millisecond
	limits.sendMax = time.Millisecond
	m, store, sdk := facNewManager(t, limits, clock)
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	facInstall(t, m)

	ctx := context.Background()
	if _, err := ZaloSendMessage(ctx, "acct", "peer", "one"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}

	sendDone := make(chan error, 1)
	go func() {
		_, err := ZaloSendMessage(ctx, "acct", "peer", "two")
		sendDone <- err
	}()
	select {
	case <-clock.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第二条发送未进入间隔睡眠")
	}

	if err := ZaloLogout(ctx, "acct"); err != nil {
		t.Fatalf("ZaloLogout: %v", err)
	}
	close(clock.release)

	err := <-sendDone
	if !errors.Is(err, ErrZaloNotLoggedIn) && !errors.Is(err, ErrZaloNotStarted) {
		t.Fatalf("登出后发送 err=%v，期望未登录/未启动", err)
	}
	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("没有记录到登录 conn")
	}
	if got := len(conn.recorded()); got != 1 {
		t.Fatalf("登出后仍发出了消息: sends=%d，期望 1", got)
	}
}

// TestZAAccountIDValidation 账号 id 是 DB 主键与日志标识，规则必须逐字收口：
// 字母/数字开头，只含字母/数字/点/下划线/连字符，长度 1-64；".", "..", 空白、
// 路径分隔符、非 ASCII 一律拒绝。
func TestZAAccountIDValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{name: "single_letter", id: "a", ok: true},
		{name: "single_digit", id: "7", ok: true},
		{name: "default", id: "default", ok: true},
		{name: "mixed_punctuation", id: "Acct-1.2_x", ok: true},
		{name: "max_len", id: strings.Repeat("a", 64), ok: true},
		{name: "empty", id: "", ok: false},
		{name: "space_only", id: " ", ok: false},
		{name: "dot", id: ".", ok: false},
		{name: "dotdot", id: "..", ok: false},
		{name: "leading_dot", id: ".a", ok: false},
		{name: "leading_dash", id: "-a", ok: false},
		{name: "leading_underscore", id: "_a", ok: false},
		{name: "inner_space", id: "a b", ok: false},
		{name: "trailing_space", id: "a ", ok: false},
		{name: "slash", id: "a/b", ok: false},
		{name: "backslash", id: `a\b`, ok: false},
		{name: "colon", id: "a:b", ok: false},
		{name: "newline", id: "a\nb", ok: false},
		{name: "too_long", id: strings.Repeat("a", 65), ok: false},
		{name: "non_ascii", id: "账号", ok: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateZAAccountID(tc.id)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("应合法，实得错误: %v", err)
			case !tc.ok && err == nil:
				t.Fatal("应拒绝，实得 nil")
			case !tc.ok && !errors.Is(err, ErrZaloBadAccountID):
				t.Fatalf("错误必须包装 ErrZaloBadAccountID，实得 %v", err)
			}
		})
	}
}

// TestZAAccountIDErrorHidesInput 错误文案只描述规则：id 可能来自外部请求，
// 把原文拼进 error 会让日志被不可信内容污染。
func TestZAAccountIDErrorHidesInput(t *testing.T) {
	t.Parallel()

	const bad = "Evil/../Account"
	err := validateZAAccountID(bad)
	if err == nil {
		t.Fatal("应拒绝")
	}
	if !errors.Is(err, ErrZaloBadAccountID) {
		t.Fatalf("应包装 ErrZaloBadAccountID，实得 %v", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatal("错误文案回显了输入全文")
	}
}

// TestZAMaskDSN 三层脱敏：整串替换、keyword 的 password=、URL 的 :pass@。
// DSN 本身绝不能顺着 error 进日志。
func TestZAMaskDSN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		dsn     string
		msg     string
		wantSub []string
	}{
		{
			name:    "whole_dsn_echoed",
			dsn:     "postgres://app:pw-alpha@db:5432/zalo?sslmode=disable",
			msg:     "打开会话库失败: 连接 postgres://app:pw-alpha@db:5432/zalo?sslmode=disable 被拒",
			wantSub: []string{"***"},
		},
		{
			name:    "keyword_password_layer",
			dsn:     "",
			msg:     "open failed: host=db password=pw-beta user=app",
			wantSub: []string{"password=***"},
		},
		{
			name:    "keyword_single_quoted_password",
			dsn:     "",
			msg:     "open failed: host=db password='pw beta quoted' user=app",
			wantSub: []string{"password=***"},
		},
		{
			name:    "keyword_double_quoted_password",
			dsn:     "",
			msg:     `open failed: password="pw beta double" user=app`,
			wantSub: []string{"password=***"},
		},
		{
			name:    "url_password_layer",
			dsn:     "",
			msg:     "dial tcp: postgres://app:pw-gamma@db:5432/zalo: connection refused",
			wantSub: []string{"postgres://app:***@db:5432/zalo"},
		},
		{
			name:    "clean_message_untouched",
			dsn:     "postgres://app:pw-delta@db:5432/zalo",
			msg:     "connection refused",
			wantSub: []string{"connection refused"},
		},
		{
			name:    "empty_dsn_and_message",
			dsn:     "",
			msg:     "",
			wantSub: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := maskDSN(tc.msg, tc.dsn)
			for _, sub := range tc.wantSub {
				if !strings.Contains(got, sub) {
					t.Fatalf("脱敏结果缺少 %q，实得 %q", sub, got)
				}
			}
			// 断言口令本体不残留（测试用假口令，只判断是否被抹掉）。
			for _, secret := range []string{"pw-alpha", "pw-beta", "pw-gamma", "pw-delta", "pw beta quoted", "pw beta double"} {
				if strings.Contains(got, secret) {
					t.Fatalf("口令未被脱敏（%s 残留）", secret)
				}
			}
		})
	}
}

// TestZAPGStoreTableNameWhitelist 表名会被拼进 SQL，只允许小写字母开头的
// [a-z0-9_]，并限制在 PostgreSQL 标识符上限 63 字节内。
func TestZAPGStoreTableNameWhitelist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "production", in: zaSessionsTable, ok: true},
		{name: "integration", in: "zalo_sessions_it_1712345678901234567", ok: true},
		{name: "single", in: "a", ok: true},
		{name: "max_len", in: strings.Repeat("a", 63), ok: true},
		{name: "empty", in: "", ok: false},
		{name: "too_long", in: strings.Repeat("a", 64), ok: false},
		{name: "uppercase", in: "ZaloSessions", ok: false},
		{name: "leading_digit", in: "1abc", ok: false},
		{name: "leading_underscore", in: "_abc", ok: false},
		{name: "dash", in: "zalo-sessions", ok: false},
		{name: "space", in: "zalo sessions", ok: false},
		{name: "dot", in: "zalo.sessions", ok: false},
		{name: "semicolon", in: "zalo;drop", ok: false},
		{name: "quote", in: `zalo"`, ok: false},
		{name: "schema_qualified", in: "public.zalo_sessions", ok: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validZATableName(tc.in); got != tc.ok {
				t.Fatalf("validZATableName=%v，期望 %v", got, tc.ok)
			}
		})
	}
}

// TestZAPGStoreConstructorValidation 构造期参数闸：空 DSN 报 ErrZaloNotStarted，
// 非法表名在 sql.Open 之前就被拒（用不可达 DSN 证明不会真的拨号）。
func TestZAPGStoreConstructorValidation(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{"", "   "} {
		if _, err := newZAPGStore(t.Context(), dsn, zaSessionsTable); !errors.Is(err, ErrZaloNotStarted) {
			t.Fatalf("空 DSN 应包装 ErrZaloNotStarted，实得 %v", err)
		}
	}
	// 127.0.0.1:1 不可达：若实现先拨号再校验表名，这里会卡在 Ping 超时上。
	if _, err := newZAPGStore(t.Context(), "postgres://user:pw@127.0.0.1:1/none?sslmode=disable", "Bad-Name"); err == nil {
		t.Fatal("非法表名应返回错误")
	}
}

// TestZAPGStoreZeroValueGuards 零值 store（绕过构造函数）应给错误而不是 panic；
// Close 对 nil 接收者与零值都幂等。
func TestZAPGStoreZeroValueGuards(t *testing.T) {
	t.Parallel()

	var nilStore *zaPGStore
	if err := nilStore.Close(); err != nil {
		t.Fatalf("nil 接收者 Close 应为 nil，实得 %v", err)
	}
	zero := &zaPGStore{}
	if err := zero.Close(); err != nil {
		t.Fatalf("零值 Close 应为 nil，实得 %v", err)
	}
	if err := zero.Upsert(t.Context(), zaSessionRecord{AccountID: "ok", Credentials: []byte(`{}`)}); err == nil {
		t.Fatal("零值 store 的 Upsert 应返回错误")
	}
	if _, err := zero.Get(t.Context(), "ok"); err == nil {
		t.Fatal("零值 store 的 Get 应返回错误")
	}
	if _, err := zero.List(t.Context()); err == nil {
		t.Fatal("零值 store 的 List 应返回错误")
	}
	if err := zero.Delete(t.Context(), "ok"); err == nil {
		t.Fatal("零值 store 的 Delete 应返回错误")
	}
}

// TestZAPGStoreClosedAfterClose 关库后的迟到调用必须给「已关闭」哨兵：
// manager 关停期若还有 ensureConn 在飞，它应该直接拒绝，而不是把 sql.ErrDBClosed
// 当成可重试的库故障反复重试。
func TestZAPGStoreClosedAfterClose(t *testing.T) {
	t.Parallel()

	// sql.Open 只解析 DSN，不拨号；这里不需要真库。
	db, err := sql.Open("pgx", "postgres://user:pw@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open 失败: %v", err)
	}
	store := &zaPGStore{db: db}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := store.ready(); !errors.Is(err, errZAGStoreClosed) {
		t.Fatalf("Close 后 ready 应返回 errZAGStoreClosed，实得 %v", err)
	}
	if _, err := store.Get(context.Background(), "ok"); !errors.Is(err, errZAGStoreClosed) {
		t.Fatalf("Close 后 Get 应返回 errZAGStoreClosed，实得 %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
}

// TestPayoutInterfaceStaysWired 三个资金门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = ZaloWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = ZaloQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = ZaloAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, Currency: pay.CurrencyUSDT, To: &pay.Party{Address: "TTargetAddr"}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := ZaloWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := ZaloQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := ZaloAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := ZaloWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
	// 空订单号也一样：收口前后都是同一个哨兵，不给「参数校验错」留第二条出口。
	if _, err := ZaloQuery(nil, "  "); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单号的拒绝口径不符: %v", err)
	}
}

// TestOperationsRefuseWhenNotStarted 未启动时 14 个公开 API 全部返回
// ErrZaloNotStarted，而不是 panic、空结果或 nil error。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	ZaloStop(nil)
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"start_pairing", func() error {
			_, err := ZaloStartPairing(ctx, "acct")
			return err
		}},
		{"login_with_cookie", func() error {
			_, err := ZaloLoginWithCookie(ctx, "acct", []byte("[]"))
			return err
		}},
		{"start_client", func() error {
			_, err := ZaloStartClient(ctx, "acct")
			return err
		}},
		{"logout", func() error { return ZaloLogout(ctx, "acct") }},
		{"delete_account", func() error { return ZaloDeleteAccount(ctx, "acct") }},
		{"list_accounts", func() error {
			_, err := ZaloListAccounts(ctx)
			return err
		}},
		{"send_message", func() error {
			_, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello")
			return err
		}},
		{"send_group_message", func() error {
			_, err := ZaloSendGroupMessage(ctx, "acct", "group-1", "hello")
			return err
		}},
		{"list_friends", func() error {
			_, err := ZaloListFriends(ctx, "acct")
			return err
		}},
		{"sync_friends", func() error {
			_, err := ZaloSyncFriends(ctx, "acct")
			return err
		}},
		{"list_groups", func() error {
			_, err := ZaloListGroups(ctx, "acct")
			return err
		}},
		{"sync_groups", func() error {
			_, err := ZaloSyncGroups(ctx, "acct")
			return err
		}},
		{"get_user_info", func() error {
			_, err := ZaloGetUserInfo(ctx, "acct", "u1")
			return err
		}},
		{"get_group_info", func() error {
			_, err := ZaloGetGroupInfo(ctx, "acct", "g1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrZaloNotStarted) {
				t.Fatalf("未启动 err=%v，期望 ErrZaloNotStarted", err)
			}
		})
	}
}

// TestZaloLogoutClearsConnKeepsStore 钉住 ZaloLogout 的口径：停 listener、清内存
// conn，但不删会话库行（下次使用按需重新登录）。
func TestZaloLogoutClearsConnKeepsStore(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	zaMgr.Store(m)
	t.Cleanup(func() { zaMgr.Store(nil) })

	ctx := context.Background()
	if _, err := ZaloStartClient(ctx, "acct"); err != nil {
		t.Fatalf("ZaloStartClient: %v", err)
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("ZaloStartClient 后账号不在内存")
	}
	if acc.currentConn() == nil {
		t.Fatal("ZaloStartClient 后没有 conn")
	}

	if err := ZaloLogout(ctx, "acct"); err != nil {
		t.Fatalf("ZaloLogout: %v", err)
	}
	if acc.currentConn() != nil {
		t.Fatal("ZaloLogout 后 conn 未清")
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("ZaloLogout 不应删除会话库行")
	}
}

// TestZaloStartWithoutSessionsDoesNotInstallManager 覆盖「enable=on 但整段
// sessions 缺失」：只记日志，不打开会话库、不安装 manager。走 ctx 注入配置，
// 不触碰真实 Postgres。
func TestZaloStartWithoutSessionsDoesNotInstallManager(t *testing.T) {
	ZaloStop(nil)
	t.Cleanup(func() {
		ZaloStop(nil)
		setRunCtx(context.Background())
	})

	cfg := &config.Cfg{Zalo: &config.ZaloConfig{Enable: "on"}}
	ctx := corectx.WithCfg(context.Background(), cfg)
	setRunCtx(ctx)

	ZaloStart(ctx)
	if zaMgr.Load() != nil {
		t.Fatal("zalo.sessions 缺失时不应安装 manager")
	}
}
