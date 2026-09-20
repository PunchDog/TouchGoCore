package gin

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"

	"github.com/gin-gonic/gin"
)

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

// isolateRegistry 让用例在干净的注册表上运行。
func isolateRegistry(t *testing.T) {
	t.Helper()
	routerMu.Lock()
	prev := routerMap
	routerMap = make(map[string]func(ctx *gin.Context))
	routerMu.Unlock()
	methodCache.Clear()
	t.Cleanup(func() {
		routerMu.Lock()
		routerMap = prev
		routerMu.Unlock()
		methodCache.Clear()
	})
}

// invokeRoute 调用注册表里唯一的 handler，返回响应体。
func invokeRoute(t *testing.T, path string) string {
	t.Helper()
	routerMu.Lock()
	var fn func(ctx *gin.Context)
	for k, v := range routerMap {
		if path == "" || k == path {
			fn = v
			break
		}
	}
	routerMu.Unlock()
	if fn == nil {
		t.Fatalf("✘ 路由 %q 未注册（现有: %v）", path, routerKeys())
	}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/"+path, nil)
	fn(c)
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
	RegisterRouter(a, nil)
	RegisterRouter(b, nil)

	if got := invokeRoute(t, "/singlerecv/hello"); got != "hello:B" {
		t.Fatalf("✘ 后注册实例 B 的路由执行了 %q（应为 hello:B，修复前会串到 A）", got)
	}
}

// TestSingleInstanceStillWorks 只有一个实例时行为不变。
func TestSingleInstanceStillWorks(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&singleRecv{tag: "C"}, nil)
	if got := invokeRoute(t, "/singlerecv/hello"); got != "hello:C" {
		t.Fatalf("✘ 响应 %q != hello:C", got)
	}
}

// TestRouterTypeSuffixKeepsPathFilter 带 RouterType 的实例仍按方法名注册到可解析的 key。
func TestRouterTypeSuffixKeepsPathFilter(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&getRecv{tag: "D"}, nil)
	keys := routerKeys()
	if len(keys) != 1 {
		t.Fatalf("注册 key %v", keys)
	}
	if got := invokeRoute(t, keys[0]); got != "ping:D" {
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
				RegisterRouter(&singleRecv{tag: strconv.Itoa(n)}, nil)
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
