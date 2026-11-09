package gin

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/vars"
)

// ==================== 被测路由 ====================

type benchRouter struct {
	// 模拟真实业务里的共享可变状态，用来检验并发安全性
	counter int64
}

func (b *benchRouter) RouterType() []string { return nil }

// Ping 返回 map，走 sendResponse 的 Ptr/Map/Slic/Struct 分支
func (b *benchRouter) Ping(ctx *gin.Context) gin.H {
	n := atomic.AddInt64(&b.counter, 1)
	return gin.H{"code": 0, "msg": "ok", "seq": n}
}

// Echo 返回 string，走 sendResponse 的 String 分支，并顺便读一下 ctx 参数
func (b *benchRouter) Echo(ctx *gin.Context) string {
	return "echo:" + ctx.Request.URL.Path
}

// Busy 模拟一点 CPU 工作，贴近真实接口
func (b *benchRouter) Busy(ctx *gin.Context) gin.H {
	s := 0
	for i := 0; i < 64; i++ {
		s += i * i
	}
	return gin.H{"sum": s}
}

var benchOnce sync.Once

// registerBench 注册基准路由，幂等
func registerBench() {
	benchOnce.Do(func() {
		RegisterRouter(&benchRouter{})
	})
}

// routeFn 从注册表取出一条路由的 handler。
// lookupRoute 自带 routerMu 读锁（A-F1），调用侧不得再持锁，否则读锁重入死锁；
// 基准路由未声明 RouterType，落在通配键上。
func routeFn(t testing.TB, path string) func(*gin.Context) {
	t.Helper()
	e := lookupRoute(path, methodAny)
	if e == nil {
		t.Fatalf("route %s not registered", path)
	}
	return e.fn
}

// ==================== 1. Handler 层并发基准（剥离网络，直测封装开销） ====================

func BenchmarkWrappedHandler_Ping(b *testing.B) {
	gin.SetMode(gin.TestMode)
	registerBench()
	fn := routeFn(b, "/benchrouter/ping")

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/benchrouter/ping", nil)
			fn(c)
		}
	})
}

func BenchmarkWrappedHandler_Echo(b *testing.B) {
	gin.SetMode(gin.TestMode)
	registerBench()
	fn := routeFn(b, "/benchrouter/echo")

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/benchrouter/echo", nil)
			fn(c)
		}
	})
}

// ==================== 2. 方法缓存并发基准（验证 sync.Map 无竞争） ====================

func BenchmarkMethodCacheLookup(b *testing.B) {
	registerBench()
	rcvr := reflect.ValueOf(&benchRouter{})

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// 并发反复命中缓存，应全部走 sync.Map.Load，无锁竞争
			if _, err := getMethodCacheEntry(rcvr, "benchRouter", "Ping"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ==================== 3. 真实 HTTP 并发压测（全栈：gin 引擎 + CORS + 封装） ====================

// freePort 获取一个当前空闲的 TCP 端口
func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestHttpConcurrencyLoad 启动真实 HTTP 服务并施压，输出 QPS 与延迟分布
//
// 300 并发 × 10 万请求的真实压测，跑一轮按机器性能从数秒到数十秒，且结果只用于
// 观察吞吐趋势、不作断言，因此归到完整 CI；快跑（go test -short）跳过。
func TestHttpConcurrencyLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("短跑模式跳过真实 HTTP 并发负载用例")
	}

	gin.SetMode(gin.ReleaseMode)
	// 将 vars 初始化为 off 模式，使桥接后的 gin 访问日志（vars.Debug）完全静默：
	// 既真实执行桥接链路，又不产生任何 I/O，避免访问日志写出干扰吞吐测量。
	// 生产环境由业务侧 vars.Run(...) 按配置决定级别与落盘。
	// 注：off 模式下 vars 仍会打开一个日志文件句柄；用自建临时目录并在退出时
	// 忽略删除错误来规避 go test 对 t.TempDir 的强制清理失败。
	logDir, err := os.MkdirTemp("", "ginbench")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	vars.Run(logDir, "bench", "off")
	defer func() {
		vars.Shutdown()
		_ = os.RemoveAll(logDir)
	}()

	port := freePort(t)
	cfg := &config.Cfg{
		Web: &config.WebConfig{
			HTTPPort:     port,
			AllowOrigins: []string{"*"},
		},
	}
	registerBench()

	ctx, cancel := context.WithCancel(corectx.WithCfg(context.Background(), cfg))
	defer cancel()
	if err := Run(ctx); err != nil {
		t.Fatalf("run server: %v", err)
	}
	defer Stop(context.Background())

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// 使用带连接池的 Transport，避免默认 MaxIdleConnsPerHost=2 在高并发下
	// 不断新建连接、goroutine 爆炸导致 thread exhaustion
	transport := &http.Transport{
		MaxIdleConns:        400,
		MaxIdleConnsPerHost: 400,
		MaxConnsPerHost:     400,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	// 压测参数
	const (
		concurrency = 300    // 并发客户端数
		totalReqs   = 100000 // 总请求数
	)

	// 预热 + 建池：并发数与目标一致，带重试吸收 Windows accept 队列在 t=0 的
	// 瞬时溢出（connection refused）。预热成功后并发条 keep-alive 连接留在池中，
	// 实测阶段直接复用，不再新建连接 → 消除首波拨号毛刺。
	var wgWarm sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wgWarm.Add(1)
		go func() {
			defer wgWarm.Done()
			if resp, err := dialWithRetry(client, base+"/benchrouter/ping", 50); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wgWarm.Wait()
	time.Sleep(50 * time.Millisecond) // 让连接回到 idle 池

	// ---- 实测阶段 ----
	durations := make(chan time.Duration, totalReqs)
	var (
		okCount   int64
		errCount  int64
		bytesSum  int64
		startTime = time.Now()
	)
	var errMu sync.Mutex
	errSamples := make([]string, 0, 5) // 仅记录前几个错误用于诊断

	var wg sync.WaitGroup
	reqsPerWorker := totalReqs / concurrency

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < reqsPerWorker; j++ {
				t0 := time.Now()
				resp, err := client.Get(base + "/benchrouter/ping")
				d := time.Since(t0)
				durations <- d
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					errMu.Lock()
					if len(errSamples) < cap(errSamples) {
						errSamples = append(errSamples, err.Error())
					}
					errMu.Unlock()
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				atomic.AddInt64(&bytesSum, int64(n))
				resp.Body.Close()
				if resp.StatusCode == 200 {
					atomic.AddInt64(&okCount, 1)
				} else {
					atomic.AddInt64(&errCount, 1)
					errMu.Lock()
					if len(errSamples) < cap(errSamples) {
						errSamples = append(errSamples, fmt.Sprintf("HTTP %d", resp.StatusCode))
					}
					errMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	close(durations)
	elapsed := time.Since(startTime)

	// 排序计算分位延迟
	samples := make([]time.Duration, 0, totalReqs)
	for d := range durations {
		samples = append(samples, d)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	total := int64(len(samples))
	qps := float64(total) / elapsed.Seconds()

	t.Logf("====== Gin 封装并发压测 ======")
	t.Logf("并发数        : %d", concurrency)
	t.Logf("总请求数      : %d", total)
	t.Logf("成功/失败     : %d / %d", okCount, errCount)
	if total > 0 {
		t.Logf("错误率        : %.3f%%", 100*float64(errCount)/float64(total))
	}
	if len(errSamples) > 0 {
		t.Logf("错误样例      : %v", errSamples)
	}
	t.Logf("总耗时        : %v", elapsed.Round(time.Millisecond))
	t.Logf("QPS           : %.0f req/s", qps)
	t.Logf("平均吞吐      : %.2f MB/s", float64(bytesSum)/elapsed.Seconds()/1024/1024)
	if total > 0 {
		t.Logf("延迟 P50      : %v", percentile(samples, 0.50))
		t.Logf("延迟 P95      : %v", percentile(samples, 0.95))
		t.Logf("延迟 P99      : %v", percentile(samples, 0.99))
		t.Logf("延迟 Max      : %v", samples[total-1])
	}

	// 压测对瞬时并发毛刺容忍：错误率超过 1% 才视为异常
	if total > 0 && float64(errCount)/float64(total) > 0.01 {
		t.Errorf("并发压测错误率过高: %d/%d (%.2f%%)", errCount, total, 100*float64(errCount)/float64(total))
	}
}

// ==================== 辅助 ====================

// dialWithRetry 发起 GET，遇到连接被拒绝（accept 队列瞬时溢出）时退避重试，
// 用于预热阶段建立连接池；非连接类错误直接返回。
func dialWithRetry(client *http.Client, url string, maxRetry int) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		resp, err := client.Get(url)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "refused") {
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ============================================================================
// 根劫持 + 手动分发（NoRoute）的回归用例：
//  1. newRootHandler：命中路径+方法 -> 调用；路径在但方法不在白名单 -> 405+Allow；
//     未注册路径 -> 404；/ws 前缀静默 404；/static 前缀走文件服务；
//  2. Run 集成：显式/默认路由经根 handler 真实分发命中。
// 根 handler 直接读全局 routerMap（map[string]map[string]*routeEntry，二级 key=方法），
// 用例通过 isolateRegistry 在干净的注册表上写入条目后再构造 handler。
// 复用 run_route_test.go 的 isolateRegistry / freeTCPPort。
// ============================================================================

func newTestCtx(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	return c, w
}

func echoHandler(body string) func(*gin.Context) {
	return func(c *gin.Context) { c.String(http.StatusOK, body) }
}

// setRoute 在干净的注册表里写入一条分发条目（method==methodAny 表示放行所有方法）。
func setRoute(path, method string, fn func(*gin.Context)) {
	routerMu.Lock()
	byMethod := routerMap[path]
	if byMethod == nil {
		byMethod = make(map[string]*routeEntry)
		routerMap[path] = byMethod
	}
	byMethod[method] = &routeEntry{fn: fn}
	routerMu.Unlock()
}

// TestNewRootHandlerDispatch 命中/方法不匹配/未注册三类分支。
func TestNewRootHandlerDispatch(t *testing.T) {
	isolateRegistry(t)
	setRoute("/a", methodAny, echoHandler("A"))
	setRoute("/b", http.MethodPost, echoHandler("B"))
	h := newRootHandler(nil)

	// 命中：无方法限制
	c, w := newTestCtx(http.MethodGet, "/a")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "A" {
		t.Fatalf("✘ /a GET 未命中: %d %q", w.Code, w.Body.String())
	}
	// 命中：方法在白名单
	c, w = newTestCtx(http.MethodPost, "/b")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "B" {
		t.Fatalf("✘ /b POST 未命中: %d %q", w.Code, w.Body.String())
	}
	// 方法不在白名单 -> 405 + Allow（A-F8：路径已注册、方法未注册）
	c, w = newTestCtx(http.MethodGet, "/b")
	h(c)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ /b GET 方法不匹配应 405，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("✘ 405 应带 Allow: POST，实际 %q", allow)
	}
	// 未注册路径（含任意深度）-> 404
	for _, p := range []string{"/unknown", "/api1/api2", "/api1/api2/api3"} {
		c, w = newTestCtx(http.MethodGet, p)
		h(c)
		if w.Code != http.StatusNotFound {
			t.Fatalf("✘ %s 应 404，实际 %d", p, w.Code)
		}
	}
}

// TestNewRootHandlerWSSilent /ws 前缀静默 404：不得走带 warning 的业务 JSON 分支。
// （c.Status 不写 body，gin 会渲染默认 "404 page not found"；与未命中的 JSON 不同）
func TestNewRootHandlerWSSilent(t *testing.T) {
	isolateRegistry(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newRootHandler(nil))
	for _, p := range []string{"/ws", "/ws/sub"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("✘ %s 应 404，实际 %d", p, w.Code)
		}
		// 未命中分支会写 JSON（含 error/code）；静默分支不应如此。
		if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "application/json") {
			t.Fatalf("✘ %s 不应走带 warning 的业务 JSON 分支，实际 Content-Type=%q body=%q", p, ct, w.Body.String())
		}
	}
}

// TestNewRootHandlerStatic /static 前缀走文件服务。
func TestNewRootHandlerStatic(t *testing.T) {
	isolateRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newRootHandler(&dir)

	c, w := newTestCtx(http.MethodGet, "/static/f.txt")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "hello-file" {
		t.Fatalf("✘ 静态文件返回错误: %d %q", w.Code, w.Body.String())
	}
}

type dpRecv struct{}

func (*dpRecv) RouterType() []string       { return []string{"GET"} }
func (*dpRecv) Ping(_ *gin.Context) string { return "pong" }

// TestDispatchRunIntegration 经 Run 起真实服务器，验证根劫持分发命中已注册路由，
// 方法不匹配 405、未注册路径/ws 404。
func TestDispatchRunIntegration(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&dpRecv{})

	port := freeTCPPort(t)
	cfg := &config.Cfg{Web: &config.WebConfig{HTTPPort: port}}
	if err := Run(corectx.WithCfg(context.Background(), cfg)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Stop(context.Background()) }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	client := &http.Client{Timeout: 3 * time.Second}
	do := func(method, path string) (int, string) {
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("✘ %s %s 请求失败: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if status, body := do("GET", "/dprecv/ping"); status != http.StatusOK || body != "pong" {
		t.Fatalf("✘ 命中分发失败: %d %q", status, body)
	}
	if status, _ := do("POST", "/dprecv/ping"); status != http.StatusMethodNotAllowed {
		t.Fatalf("✘ 方法不匹配应 405，实际 %d", status)
	}
	if status, _ := do("GET", "/deep/not/registered"); status != http.StatusNotFound {
		t.Fatalf("✘ 未注册路径应 404，实际 %d", status)
	}
	if status, _ := do("GET", "/ws"); status != http.StatusNotFound {
		t.Fatalf("✘ /ws 应静默 404，实际 %d", status)
	}
}

// ============================================================================
// 反向 gRPC 路由代理（gin 侧）回归：字面 / :name 参数路由经 routeEntry.proxy 转发，
// 命中后把 MatchedPattern + Params 传给 proxy，响应回写网页端；Unregister 按属主精确回收。
// ============================================================================

// fakeProxy 记录收到的 ProxyRequest，并返回预置响应或错误。
type fakeProxy struct {
	got *ProxyRequest
	rsp *ProxyResponse
	err error
}

func (f *fakeProxy) Do(_ context.Context, req *ProxyRequest) (*ProxyResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.rsp, nil
}

// serve 用根劫持 handler 真实分发一次请求。
func serve(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := newRootHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(method, path, reader)
	h(c)
	return w
}

func TestGRPCProxyLiteralRoute(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusCreated, Header: http.Header{"X-Proxied": []string{"yes"}}, Body: []byte("hello-from-client")}}
	RegisterGRPCRoute("backend", 1, "/proxy/hello", http.MethodGet, fp)

	w := serve(t, http.MethodGet, "/proxy/hello", "q=1")
	if w.Code != http.StatusCreated {
		t.Fatalf("✘ 期望 201，实际 %d", w.Code)
	}
	if w.Body.String() != "hello-from-client" {
		t.Fatalf("✘ 响应体不符: %q", w.Body.String())
	}
	if w.Header().Get("X-Proxied") != "yes" {
		t.Fatalf("✘ 响应头未回写: %v", w.Header())
	}
	if fp.got == nil {
		t.Fatal("✘ proxy 未被调用")
	}
	if fp.got.Path != "/proxy/hello" || fp.got.MatchedPattern != "/proxy/hello" || fp.got.Method != http.MethodGet {
		t.Fatalf("✘ 请求快照不符: %+v", fp.got)
	}
	// 方法不匹配（GET 注册，POST 请求）应 405 + Allow（A-F8）：路径已注册、方法未注册。
	w = serve(t, http.MethodPost, "/proxy/hello", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ POST 未注册方法应 405，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("✘ 405 应带 Allow: GET，实际 %q", allow)
	}
}

func TestGRPCProxyParamRoute(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusOK, Body: []byte("user")}}
	RegisterGRPCRoute("backend", 1, "/user/:id", http.MethodGet, fp)

	w := serve(t, http.MethodGet, "/user/42", "")
	if w.Code != http.StatusOK {
		t.Fatalf("✘ 参数路由代理期望 200，实际 %d", w.Code)
	}
	if fp.got == nil {
		t.Fatal("✘ proxy 未被调用")
	}
	if fp.got.Path != "/user/42" {
		t.Fatalf("✘ 真实路径应为 /user/42，实际 %q", fp.got.Path)
	}
	if fp.got.MatchedPattern != "/user/:id" {
		t.Fatalf("✘ MatchedPattern 应为模板 /user/:id，实际 %q", fp.got.MatchedPattern)
	}
	if fp.got.Params["id"] != "42" {
		t.Fatalf("✘ 参数捕获 id=42 缺失，实际 %v", fp.got.Params)
	}
}

func TestGRPCProxyMethodAny(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusOK, Body: []byte("any")}}
	// method 传空 → 通配键，任意方法都命中。
	RegisterGRPCRoute("backend", 1, "/wild", "", fp)

	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if w := serve(t, m, "/wild", ""); w.Code != http.StatusOK {
			t.Fatalf("✘ %s /wild 应命中通配代理，实际 %d", m, w.Code)
		}
	}
}

func TestGRPCProxyErrorReturnsBadGateway(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{err: errors.New("client gone")}
	RegisterGRPCRoute("backend", 1, "/boom", http.MethodGet, fp)
	if w := serve(t, http.MethodGet, "/boom", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("✘ proxy 出错应回 502，实际 %d body=%q", w.Code, w.Body.String())
	}
}

func TestGRPCProxyUnregisterByOwner(t *testing.T) {
	isolateRegistry(t)
	// 本地路由（fn 型，owner 为空）不受影响
	setRoute("/local", methodAny, echoHandler("L"))
	// backend1：字面 + 参数；backend2：同一 paramRoute 的另一个方法
	RegisterGRPCRoute("backend1", 1, "/b1/hello", http.MethodGet, &fakeProxy{rsp: &ProxyResponse{Status: 200}})
	RegisterGRPCRoute("backend1", 1, "/b1/user/:id", http.MethodGet, &fakeProxy{rsp: &ProxyResponse{Status: 200}})
	RegisterGRPCRoute("backend2", 2, "/shared/:id", http.MethodPost, &fakeProxy{rsp: &ProxyResponse{Status: 200}})
	RegisterGRPCRoute("backend1", 1, "/shared/:id", http.MethodGet, &fakeProxy{rsp: &ProxyResponse{Status: 200}})

	UnregisterGRPCRoutes("backend1", 1)

	// backend1 字面路由应 404
	if w := serve(t, http.MethodGet, "/b1/hello", ""); w.Code != http.StatusNotFound {
		t.Fatalf("✘ backend1 字面路由未回收，实际 %d", w.Code)
	}
	// backend1 参数路由应 404
	if w := serve(t, http.MethodGet, "/b1/user/7", ""); w.Code != http.StatusNotFound {
		t.Fatalf("✘ backend1 参数路由未回收，实际 %d", w.Code)
	}
	// 本地路由仍在
	if w := serve(t, http.MethodGet, "/local", ""); w.Code != http.StatusOK || w.Body.String() != "L" {
		t.Fatalf("✘ 本地路由被误删: %d %q", w.Code, w.Body.String())
	}
	// /shared/:id 的 backend1(GET) 回收，但 backend2(POST) 保留；
	// GET 请求撞上仍在的 POST 模式 -> 405 + Allow（A-F8 口径），不再是 404。
	w := serve(t, http.MethodGet, "/shared/9", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ /shared GET(backend1) 应已回收且回 405，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("✘ 405 应带 Allow: POST，实际 %q", allow)
	}
	fp2 := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusOK, Body: []byte("post-ok")}}
	RegisterGRPCRoute("backend2", 2, "/shared/:id", http.MethodPost, fp2) // 覆盖确认仍在 backend2
	if w := serve(t, http.MethodPost, "/shared/9", ""); w.Code != http.StatusOK {
		t.Fatalf("✘ /shared POST(backend2) 应保留，实际 %d", w.Code)
	}
}

// ============================================================================
// 重复注册判定按「路径 + 方法」两级：
//  1. 同路径不同方法各自注册、互不覆盖（旧版整条 entry 替换会静默丢失先注册的方法）；
//  2. 同路径同方法才算重复注册，后者覆盖前者；
//  3. 精确方法条目优先于通配条目（RouterType 未声明时注册的放行所有方法条目）。
// 复用 run_route_test.go 的 isolateRegistry / invokeRoute。
// ============================================================================

// 两个类型都用显式路径注册到同一 URL /dup/api，方法白名单分别为 GET / POST。
type dupGetRecv struct{}

func (*dupGetRecv) RouterType() []string          { return []string{"GET"} }
func (*dupGetRecv) RouterPath() map[string]string { return map[string]string{"Do": "/dup/api"} }
func (*dupGetRecv) Do(_ *gin.Context) string      { return "get" }

type dupPostRecv struct{}

func (*dupPostRecv) RouterType() []string          { return []string{"POST"} }
func (*dupPostRecv) RouterPath() map[string]string { return map[string]string{"Do": "/dup/api"} }
func (*dupPostRecv) Do(_ *gin.Context) string      { return "post" }

// TestSamePathDifferentMethodsCoexist 两次 RegisterRouter 传入同一显式路径、
// 但方法分别为 GET / POST：不得判为重复注册，两个 handler 都必须可命中。
func TestSamePathDifferentMethodsCoexist(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&dupGetRecv{})
	RegisterRouter(&dupPostRecv{})

	if got := invokeRoute(t, "/dup/api", http.MethodGet); got != "get" {
		t.Fatalf("✘ GET handler 被 POST 注册覆盖，响应 %q（应为 get）", got)
	}
	if got := invokeRoute(t, "/dup/api", http.MethodPost); got != "post" {
		t.Fatalf("✘ POST handler 未各自注册，响应 %q（应为 post）", got)
	}
	// 既无精确条目也无通配条目的方法 -> 不命中注册表（lookupRoute 自带读锁）
	hit := lookupRoute("/dup/api", http.MethodDelete)
	if hit != nil {
		t.Fatal("✘ DELETE 未注册却命中了路由")
	}
}

// TestSamePathSameMethodOverwrite 同路径同方法重复注册：后者覆盖前者。
func TestSamePathSameMethodOverwrite(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&getRecv{tag: "old"})
	RegisterRouter(&getRecv{tag: "new"})

	if got := invokeRoute(t, "/getrecv/ping", http.MethodGet); got != "ping:new" {
		t.Fatalf("✘ 同方法重复注册未被后者覆盖，响应 %q（应为 ping:new）", got)
	}
}

// TestExactMethodBeatsWildcard 同一路径先注册通配（RouterType 为 nil）、
// 再注册精确 GET：GET 命中精确 handler，其余方法回退通配 handler。
func TestExactMethodBeatsWildcard(t *testing.T) {
	isolateRegistry(t)
	setRoute("/mix/api", methodAny, echoHandler("any"))
	setRoute("/mix/api", http.MethodGet, echoHandler("get"))

	h := newRootHandler(nil)
	c, w := newTestCtx(http.MethodGet, "/mix/api")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "get" {
		t.Fatalf("✘ 精确方法未优先于通配: %d %q", w.Code, w.Body.String())
	}
	c, w = newTestCtx(http.MethodPost, "/mix/api")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "any" {
		t.Fatalf("✘ 未回退通配条目: %d %q", w.Code, w.Body.String())
	}
}

// ============================================================================
// ":name" 参数段匹配的回归用例：
//   - 精确未命中时逐段匹配参数路由，":id" 段捕获非空单段并回填 ctx.Param；
//   - 方法门控与字面路由同口径（精确方法优先，再通配）；
//   - 尾斜杠（空参数段）/ 段数不符落 404；方法不匹配落 405+Allow（A-F8）；
//   - 字面路由优先于参数路由（同段数时精确命中不被参数模式抢占）。
// 复用 run_route_test.go 的 isolateRegistry / freeTCPPort。
// ============================================================================

type paramRecv struct{}

func (*paramRecv) RouterType() []string { return []string{"GET"} }
func (*paramRecv) RouterPath() map[string]string {
	return map[string]string{"Send": "/wst/api/m/mobile/sendMessage/:id"}
}

// Send 回显捕获到的 :id，验证 ctx.Param 已被分发回填。
func (*paramRecv) Send(ctx *gin.Context) string { return ctx.Param("id") }

// serveRoot 用真实 gin 引擎挂 NoRoute 根 handler 跑一次请求，返回状态码与响应体。
// 必须走 engine.ServeHTTP：裸 recorder 读不到 c.Status 分支的状态码。
func serveRoot(t *testing.T, method, path string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newRootHandler(nil))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code, w.Body.String()
}

// TestParamRouteMatchAndCapture 参数路由命中并捕获 :id。
func TestParamRouteMatchAndCapture(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&paramRecv{})

	if code, body := serveRoot(t, http.MethodGet, "/wst/api/m/mobile/sendMessage/5"); code != http.StatusOK || body != "5" {
		t.Fatalf("✘ 参数路由未命中或 :id 未捕获: %d %q（期望 200 \"5\"）", code, body)
	}
	// 不同取值各自捕获。
	if code, body := serveRoot(t, http.MethodGet, "/wst/api/m/mobile/sendMessage/abc123"); code != http.StatusOK || body != "abc123" {
		t.Fatalf("✘ :id 捕获错误: %d %q（期望 200 \"abc123\"）", code, body)
	}
}

// TestParamRouteMissBranches 空参数段 / 段数不符应 404；方法不匹配应 405+Allow（A-F8）。
func TestParamRouteMissBranches(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&paramRecv{})

	for _, tc := range []struct {
		name, method, path string
	}{
		{"尾斜杠空参数", http.MethodGet, "/wst/api/m/mobile/sendMessage/"},
		{"段数过多", http.MethodGet, "/wst/api/m/mobile/sendMessage/5/extra"},
		{"段数不足", http.MethodGet, "/wst/api/m/mobile/sendMessage"},
	} {
		if code, _ := serveRoot(t, tc.method, tc.path); code != http.StatusNotFound {
			t.Fatalf("✘ %s：%s %s 应 404，实际 %d", tc.name, tc.method, tc.path, code)
		}
	}
	// 模式命中但方法未注册（GET 注册，POST 请求）：405 + Allow。
	code, _ := serveRoot(t, http.MethodPost, "/wst/api/m/mobile/sendMessage/5")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ 方法不匹配：%s 应 405，实际 %d", "POST /wst/api/m/mobile/sendMessage/5", code)
	}
}

// TestLiteralRouteBeatsParam 同段数下字面路径必须优先于参数模式命中。
func TestLiteralRouteBeatsParam(t *testing.T) {
	isolateRegistry(t)
	// 参数模式 /lit/x/:id 与字面 /lit/x/one 段数相同；请求 /lit/x/one 应命中字面。
	setRoute("/lit/x/one", methodAny, echoHandler("literal"))
	RegisterRouter(&litParamRecv{})

	if code, body := serveRoot(t, http.MethodGet, "/lit/x/one"); code != http.StatusOK || body != "literal" {
		t.Fatalf("✘ 字面路由未优先于参数路由: %d %q（期望 200 \"literal\"）", code, body)
	}
	if code, body := serveRoot(t, http.MethodGet, "/lit/x/two"); code != http.StatusOK || body != "param:two" {
		t.Fatalf("✘ 非字面取值未落到参数路由: %d %q（期望 200 \"param:two\"）", code, body)
	}
}

type litParamRecv struct{}

func (*litParamRecv) RouterType() []string { return nil } // 通配方法
func (*litParamRecv) RouterPath() map[string]string {
	return map[string]string{"Get": "/lit/x/:id"}
}
func (*litParamRecv) Get(ctx *gin.Context) string { return "param:" + ctx.Param("id") }

// TestParamRouteWildcardMethod 未声明 RouterType 的参数路由（通配）应对任意方法放行。
func TestParamRouteWildcardMethod(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&litParamRecv{})
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		code, body := serveRoot(t, m, "/lit/x/"+strconv.Itoa(7))
		if code != http.StatusOK || body != "param:7" {
			t.Fatalf("✘ 通配参数路由 %s 未命中: %d %q", m, code, body)
		}
	}
}

// ============================================================================
// IRouterPath 显式路径扩展（方案 A）的回归用例：
//  1. 实现 IRouterPath 的类按显式路径注册；
//  2. RouterPath 方法本身不会被注册为路由 handler；
//  3. 未实现 IRouterPath 的类仍走默认推导，行为不变（proxywork 零回归）。
// 复用 run_route_test.go 的 isolateRegistry / invokeRoute / routerKeys。
// ============================================================================

type pathRecv struct{}

func (*pathRecv) RouterType() []string { return []string{"POST"} }
func (*pathRecv) RouterPath() map[string]string {
	return map[string]string{"Login": "/wst/api/m/login"}
}
func (*pathRecv) Login(_ *gin.Context) string { return "login-ok" }

type plainRecv struct{}

func (*plainRecv) RouterType() []string        { return []string{"GET"} }
func (*plainRecv) Hello(_ *gin.Context) string { return "hi" }

// TestIRouterPathExplicitPath 显式路径生效，且 RouterPath/RouterType 不注册为路由。
func TestIRouterPathExplicitPath(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&pathRecv{})

	keys := routerKeys()
	if len(keys) != 1 {
		t.Fatalf("✘ 应只注册 1 条路由，实际 %d: %v", len(keys), keys)
	}
	if keys[0] != "/wst/api/m/login" {
		t.Fatalf("✘ 显式路径未生效: %v", keys)
	}
	if got := invokeRoute(t, keys[0], http.MethodPost); got != "login-ok" {
		t.Fatalf("✘ handler 响应 %q != login-ok", got)
	}
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), "routerpath") {
			t.Fatalf("✘ RouterPath 被错误注册为路由: %v", keys)
		}
	}
}

// TestDefaultDerivationUnchanged 未实现 IRouterPath 的类仍走默认推导。
func TestDefaultDerivationUnchanged(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&plainRecv{})

	keys := routerKeys()
	if len(keys) != 1 || keys[0] != "/plainrecv/hello" {
		t.Fatalf("✘ 默认推导行为被改变: %v", keys)
	}
	if got := invokeRoute(t, keys[0], http.MethodGet); got != "hi" {
		t.Fatalf("✘ handler 响应 %q != hi", got)
	}
}

// TestPartialRouterPathFallsBack 实现了 IRouterPath 但方法未列出时回退默认推导。
func TestPartialRouterPathFallsBack(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&partialRecv{})

	keys := routerKeys()
	if len(keys) != 2 {
		t.Fatalf("✘ 应注册 2 条路由，实际 %d: %v", len(keys), keys)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	if !found["/wst/api/m/explicit"] {
		t.Fatalf("✘ 显式路径缺失: %v", keys)
	}
	if !found["/partialrecv/fallback"] {
		t.Fatalf("✘ 未列出方法未回退默认推导: %v", keys)
	}
}

type partialRecv struct{}

func (*partialRecv) RouterType() []string { return []string{"POST"} }
func (*partialRecv) RouterPath() map[string]string {
	return map[string]string{"Explicit": "/wst/api/m/explicit"}
}
func (*partialRecv) Explicit(_ *gin.Context) string { return "explicit" }
func (*partialRecv) Fallback(_ *gin.Context) string { return "fallback" }

// ============================================================================
// IRouterTimeout 可选接口（超时表由 RegisterRouter 第二参数收敛而来）的回归用例：
//  1. 命中路径的超时覆盖默认 15s；
//  2. 未实现 IRouterTimeout、或路径未列出时回退默认 15s；
//  3. 与 IRouterPath 联动：超时按显式路径索引也能命中；
//  4. RouterTimeout 方法本身不会被注册成路由 handler。
// 复用 run_route_test.go 的 isolateRegistry / routerKeys。
// ============================================================================

type timeoutRecv struct{}

func (*timeoutRecv) RouterType() []string { return []string{"POST"} }
func (*timeoutRecv) RouterTimeout() map[string]int64 {
	return map[string]int64{"/timeoutrecv/slow": 6}
}
func (*timeoutRecv) Slow(_ *gin.Context) string  { return "slow" }
func (*timeoutRecv) Quick(_ *gin.Context) string { return "quick" }

// explicitTimeoutRecv 同时实现 IRouterPath 与 IRouterTimeout，超时键用的是显式路径。
type explicitTimeoutRecv struct{}

func (*explicitTimeoutRecv) RouterType() []string { return []string{"POST"} }
func (*explicitTimeoutRecv) RouterPath() map[string]string {
	return map[string]string{"Login": "/wst/api/m/login"}
}
func (*explicitTimeoutRecv) RouterTimeout() map[string]int64 {
	return map[string]int64{"/wst/api/m/login": 45}
}
func (*explicitTimeoutRecv) Login(_ *gin.Context) string { return "login" }

// invokeForDeadline 把 handler 挂到真实 gin 引擎上跑一次请求，返回它给请求 ctx 设的超时时长。
// 必须走引擎：超时要按 ctx.FullPath() 查表，CreateTestContext 的 FullPath 是空串。
// handler 里的 defer cancel() 在返回时已执行，但 WithTimeout 的 deadline 取消后仍可读。
func invokeForDeadline(t *testing.T, path string) time.Duration {
	t.Helper()
	// lookupRoute 自带 routerMu 读锁（A-F1），外层不再持锁；fallback 扫描单独持读锁。
	var fn func(ctx *gin.Context)
	// 注册表是「路径:方法」二级索引，用 lookupRoute 取该路径下已注册的 handler。
	if e := lookupRoute(path, methodAny); e != nil {
		fn = e.fn
	} else {
		routerMu.RLock()
		for _, e := range routerMap[path] {
			fn = e.fn
			break
		}
		routerMu.RUnlock()
	}
	if fn == nil {
		t.Fatalf("✘ 路由 %q 未注册（现有: %v）", path, routerKeys())
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var deadline time.Time
	// 第二个 handler 在同一 Context 上读到前者替换过的 Request
	engine.POST(path, fn, func(c *gin.Context) {
		deadline, _ = c.Request.Context().Deadline()
	})
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))

	if deadline.IsZero() {
		t.Fatalf("✘ 路由 %q 的请求 ctx 没有超时", path)
	}
	return time.Until(deadline)
}

// within 判断实测剩余时长是否落在期望秒数附近（gin 测试上下文与反射调用有几毫秒开销）。
func within(d time.Duration, seconds int) bool {
	want := time.Duration(seconds) * time.Second
	return d > want-2*time.Second && d < want+2*time.Second
}

func TestRouterTimeoutOverridesDefault(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&timeoutRecv{})

	if got := invokeForDeadline(t, "/timeoutrecv/slow"); !within(got, 6) {
		t.Fatalf("✘ 命中 RouterTimeout 的路由超时 %v，应约 6s", got)
	}
	// 同一类型内未列出的路径仍是默认 15s
	if got := invokeForDeadline(t, "/timeoutrecv/quick"); !within(got, 15) {
		t.Fatalf("✘ 未列出路径的超时 %v，应回退默认 15s", got)
	}
	for _, k := range routerKeys() {
		if strings.Contains(strings.ToLower(k), "routertimeout") {
			t.Fatalf("✘ RouterTimeout 被错误注册为路由: %v", routerKeys())
		}
	}
}

func TestRouterTimeoutDefaultWithoutInterface(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&plainRecv{})

	if got := invokeForDeadline(t, "/plainrecv/hello"); !within(got, 15) {
		t.Fatalf("✘ 未实现 IRouterTimeout 的类型超时 %v，应回退默认 15s", got)
	}
}

func TestRouterTimeoutWithExplicitPath(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&explicitTimeoutRecv{})

	keys := routerKeys()
	if len(keys) != 1 || keys[0] != "/wst/api/m/login" {
		t.Fatalf("✘ 显式路径未生效: %v", keys)
	}
	if got := invokeForDeadline(t, "/wst/api/m/login"); !within(got, 45) {
		t.Fatalf("✘ 按显式路径索引的超时未命中，实际 %v，应约 45s", got)
	}
}

// ============================================================================
// S48：gin 路由注册的方法缓存按实例区分、routerMap 并发保护、端口抢占式启动判定。
// ============================================================================

type singleRecv struct{ tag string }

func (*singleRecv) RouterType() []string { return nil }
func (s *singleRecv) Hello(_ *gin.Context) string {
	return "hello:" + s.tag
}

type getRecv struct{ tag string }

func (*getRecv) RouterType() []string { return []string{"GET"} }
func (g *getRecv) Ping(_ *gin.Context) string {
	return "ping:" + g.tag
}

// isolateRegistry 让用例在干净的注册表上运行（字面表与参数表一并隔离）。
func isolateRegistry(t *testing.T) {
	t.Helper()
	routerMu.Lock()
	prev := routerMap
	routerMap = make(map[string]map[string]*routeEntry)
	routerMu.Unlock()
	paramMu.Lock()
	prevRoutes, prevIndex := paramRoutes, paramIndex
	paramRoutes, paramIndex = nil, make(map[string]*paramRoute)
	paramMu.Unlock()
	methodCache.Clear()
	t.Cleanup(func() {
		routerMu.Lock()
		routerMap = prev
		routerMu.Unlock()
		paramMu.Lock()
		paramRoutes, paramIndex = prevRoutes, prevIndex
		paramMu.Unlock()
		methodCache.Clear()
	})
}

// invokeRoute 按「路径 + 方法」调用注册表里的 handler，返回响应体。
// lookupRoute 自带 routerMu 读锁（A-F1），外层不得再持写锁，否则 RLock 死锁。
func invokeRoute(t *testing.T, path, method string) string {
	t.Helper()
	entry := lookupRoute(path, method)
	if entry == nil {
		t.Fatalf("✘ 路由 %q %s 未注册（现有: %v）", path, method, routerKeys())
	}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/"+path, nil)
	entry.fn(c)
	return w.Body.String()
}

func routerKeys() []string {
	routerMu.Lock()
	defer routerMu.Unlock()
	out := make([]string, 0, len(routerMap))
	for k := range routerMap {
		out = append(out, k)
	}
	return out
}

// TestMethodCacheIsPerReceiver 同类型第二个实例注册后，命中路由必须执行第二个实例的方法。
// 修复前缓存 key 只有「类型名.方法名」，B 的闭包会拿到 A 的绑定方法（实例串台，
// 且缓存长期钉住 A 让它无法回收）。
func TestMethodCacheIsPerReceiver(t *testing.T) {
	isolateRegistry(t)

	a := &singleRecv{tag: "A"}
	b := &singleRecv{tag: "B"}
	RegisterRouter(a)
	RegisterRouter(b)

	if got := invokeRoute(t, "/singlerecv/hello", http.MethodGet); got != "hello:B" {
		t.Fatalf("✘ 后注册实例 B 的路由执行了 %q（应为 hello:B，修复前会串到 A）", got)
	}
}

// TestSingleInstanceStillWorks 只有一个实例时行为不变。
func TestSingleInstanceStillWorks(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&singleRecv{tag: "C"})
	if got := invokeRoute(t, "/singlerecv/hello", http.MethodGet); got != "hello:C" {
		t.Fatalf("✘ 响应 %q != hello:C", got)
	}
}

// TestRouterTypeSuffixKeepsPathFilter 带 RouterType 的实例仍按方法名注册到可解析的 key。
func TestRouterTypeSuffixKeepsPathFilter(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&getRecv{tag: "D"})
	keys := routerKeys()
	if len(keys) != 1 {
		t.Fatalf("注册 key %v", keys)
	}
	if got := invokeRoute(t, keys[0], http.MethodGet); got != "ping:D" {
		t.Fatalf("✘ 响应 %q != ping:D", got)
	}
}

// TestRegisterRouterConcurrentWithRun routerMap 必须受锁保护：
// 注册与遍历并发进行会直接 fatal（concurrent map read and map write）。
func TestRegisterRouterConcurrentWithRun(t *testing.T) {
	isolateRegistry(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var snapped atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				RegisterRouter(&singleRecv{tag: strconv.Itoa(n)})
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			routerMu.Lock()
			n := len(routerMap)
			routerMu.Unlock()
			snapped.Add(int64(n))
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if snapped.Load() == 0 {
		t.Fatal("✘ 遍历侧没读到快照，用例没跑到并发路径")
	}
}

// freeTCPPort 抢占一个空闲端口并释放。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestRunFailsFastWhenPortTaken 端口被占时 Run 必须立刻返回错误，
// 而不是等一轮探测窗口后宣布启动成功。
func TestRunFailsFastWhenPortTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := &config.Cfg{Web: &config.WebConfig{HTTPPort: port}}
	started := time.Now()
	runErr := Run(corectx.WithCfg(context.Background(), cfg))
	elapsed := time.Since(started)
	if runErr == nil {
		t.Fatal("✘ 端口被占用仍报告启动成功")
	}
	// 修复前：先 ListenAndServe 再 sleep 100ms 才收错，且 goroutine 里的失败被吞掉
	if elapsed > 100*time.Millisecond {
		t.Fatalf("✘ 启动失败耗时 %v，未按端口抢占结果即时判定", elapsed)
	}
	if err := Stop(context.Background()); err != nil {
		t.Fatalf("✘ 失败路径不该留下半启动的 server: %v", err)
	}
}

// TestRunReportsSuccessAfterBind 正常启动能拿到端口，且 Stop 幂等。
func TestRunReportsSuccessAfterBind(t *testing.T) {
	port := freeTCPPort(t)
	cfg := &config.Cfg{Web: &config.WebConfig{HTTPPort: port}}
	if err := Run(corectx.WithCfg(context.Background(), cfg)); err != nil {
		t.Fatal(err)
	}
	addr := "http://127.0.0.1:" + strconv.Itoa(port) + "/ping"
	resp, err := http.Get(addr) //nolint:noctx // 冒烟探测
	if err != nil {
		t.Fatalf("✘ 端口未就绪: %v", err)
	}
	_ = resp.Body.Close()

	httpMu.Lock()
	hasServer := httpServer != nil
	httpMu.Unlock()
	if !hasServer {
		t.Fatal("✘ Run 成功后没有登记 server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Stop(ctx); err != nil {
		t.Fatal(err)
	}
	httpMu.Lock()
	left := httpServer
	httpMu.Unlock()
	if left != nil {
		t.Fatal("✘ Stop 后仍持有已关闭的 server，二次 Stop 会对同一实例再 Shutdown")
	}
	if err := Stop(ctx); err != nil {
		t.Fatalf("✘ 二次 Stop 返回错误: %v", err)
	}
}
