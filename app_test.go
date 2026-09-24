package touchgocore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/mapmanager"
	"touchgocore/rpc"
	"touchgocore/syncmap"
	"touchgocore/websocket"
)

type mockService struct {
	name   string
	starts int
	stops  int
}

func (m *mockService) Name() string { return m.name }
func (m *mockService) Start(context.Context) error {
	m.starts++
	return nil
}
func (m *mockService) Stop(context.Context) error {
	m.stops++
	return nil
}

func TestAppRegistryFallback(t *testing.T) {
	app := &App{
		rpcClients: syncmap.NewMap[string, *rpc.RpcClient](),
		rpcServers: syncmap.NewMap[string, *rpc.RpcServer](),
		wsClients:  syncmap.NewShardedMap[int64, *websocket.Client](0),
	}
	if app.GetRpcClient("missing") != nil {
		t.Fatal("expected nil rpc client")
	}
	if app.GetRpcServer("missing") != nil {
		t.Fatal("expected nil rpc server")
	}
	if app.GetWSClient(1) != nil {
		t.Fatal("expected nil ws client")
	}
}

func TestAppStartShutdownOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &mockService{name: "a"}
	b := &mockService{name: "b"}
	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		services: []Service{a, b},
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	if a.starts != 1 || b.starts != 1 {
		t.Fatalf("starts a=%d b=%d", a.starts, b.starts)
	}
	if err := app.Shutdown(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if a.stops != 1 || b.stops != 1 {
		t.Fatalf("stops a=%d b=%d", a.stops, b.stops)
	}
	if app.started {
		t.Fatal("expected started=false")
	}
}

// blockingService 记录自己拿到的停止预算，然后一直阻塞到预算耗尽
type blockingService struct {
	name    string
	mu      sync.Mutex
	budgets []time.Duration
}

func (s *blockingService) Name() string                { return s.name }
func (s *blockingService) Start(context.Context) error { return nil }
func (s *blockingService) Stop(ctx context.Context) error {
	budget := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}
	s.mu.Lock()
	s.budgets = append(s.budgets, budget)
	s.mu.Unlock()

	<-ctx.Done()
	return nil
}

func (s *blockingService) observed() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.budgets...)
}

// TestApp_ShutdownHonorsPerServiceBudget 校验总预算被按服务数均分且不低于下限，
// 前面的服务卡死不会把后面的服务预算压成 0（旧实现共享同一个关闭 ctx）。
func TestApp_ShutdownHonorsPerServiceBudget(t *testing.T) {
	services := []*blockingService{
		{name: "slow-1"},
		{name: "slow-2"},
		{name: "slow-3"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := &App{ctx: ctx, cancel: cancel, started: true}
	for _, svc := range services {
		app.services = append(app.services, svc)
	}

	// 总预算 3s，3 个服务均分只有 1s，因此每个都应被抬到最小预算 2s
	err := app.Shutdown(3 * time.Second)
	if err == nil {
		t.Fatal("服务全部超时时 Shutdown 应返回错误")
	}

	for _, svc := range services {
		observed := svc.observed()
		if len(observed) != 1 {
			t.Fatalf("服务[%s] 被停止 %d 次，期望 1 次", svc.name, len(observed))
		}
		if b := observed[0]; b < minServiceStopBudget-time.Millisecond {
			t.Fatalf("服务[%s] 拿到的预算 %v 低于下限 %v", svc.name, b, minServiceStopBudget)
		}
	}
}

// failStartService 启动即失败的服务
type failStartService struct {
	name string
	err  error
}

func (s *failStartService) Name() string                { return s.name }
func (s *failStartService) Start(context.Context) error { return s.err }
func (s *failStartService) Stop(context.Context) error  { return nil }

// TestApp_StartFailureRollsBackWithBudget 启动失败回滚时，卡死的 Stop 不能把进程拖住
func TestApp_StartFailureRollsBackWithBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocker := &blockingService{name: "blocker"}
	app := &App{
		ctx:    ctx,
		cancel: cancel,
		services: []Service{
			blocker,
			&failStartService{name: "bad", err: errors.New("启动失败")},
		},
	}

	done := make(chan error, 1)
	go func() { done <- app.Start() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("启动失败应返回错误")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("回滚未在预算内结束，卡死的 Stop 拖住了启动失败退出")
	}

	if got := blocker.observed(); len(got) != 1 {
		t.Fatalf("回滚停止已启动服务 %d 次，期望 1 次", len(got))
	}
	if app.started {
		t.Fatal("启动失败后 started 应为 false")
	}
}

// appCtxService 只有看到 app.ctx 被取消才认为停止完成
type appCtxService struct {
	appCtx context.Context
}

func (s *appCtxService) Name() string                { return "appctx" }
func (s *appCtxService) Start(context.Context) error { return nil }
func (s *appCtxService) Stop(ctx context.Context) error {
	select {
	case <-s.appCtx.Done():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("Stop 期间 app.ctx 仍未取消")
	}
}

// TestApp_ShutdownCancelsAppContext 服务的 Run 循环靠 app.ctx 退出，
// Stop 必须能看到取消信号，否则与循环互等死锁。
func TestApp_ShutdownCancelsAppContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{ctx: ctx, cancel: cancel, started: true}
	app.services = []Service{&appCtxService{appCtx: ctx}}

	if err := app.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown 返回错误: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("Shutdown 后 app.ctx 应已取消")
	}
}

func TestApp_ShutdownNotStartedCancelsAppContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{ctx: ctx, cancel: cancel}

	if err := app.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown 返回错误: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("未启动直接关闭时也应取消 app.ctx")
	}
}

func TestAppSetupMaxProcs(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))

	app := &App{Cfg: &config.Cfg{}}
	app.setupMaxProcs()
	if got := runtime.GOMAXPROCS(0); got < 1 {
		t.Fatalf("未配置时 GOMAXPROCS 应保持有效值, got %d", got)
	}

	if runtime.NumCPU() < 2 {
		t.Skip("CPU 核数不足，跳过显式设置用例")
	}
	target := runtime.NumCPU() - 1
	app.Cfg.Server = &config.ServerConfig{MaxProcs: target}
	app.setupMaxProcs()
	if got := runtime.GOMAXPROCS(0); got != target {
		t.Fatalf("GOMAXPROCS = %d, 期望 %d", got, target)
	}

	// 非正数配置不应改动运行时值
	app.Cfg.Server = &config.ServerConfig{MaxProcs: -1}
	app.setupMaxProcs()
	if got := runtime.GOMAXPROCS(0); got != target {
		t.Fatalf("MaxProcs<=0 时不应改动 GOMAXPROCS, got %d", got)
	}
}

func doMetricsRequest(handler http.Handler, path, remoteAddr, authorization, forwardedFor, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path+query, nil)
	req.RemoteAddr = remoteAddr
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
		req.Header.Set("X-Real-IP", forwardedFor)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestMetricsAuth(t *testing.T) {
	mux := newMetricsMux("secret")

	cases := []struct {
		name          string
		path          string
		remoteAddr    string
		authorization string
		forwardedFor  string
		query         string
		wantStatus    int
	}{
		{"带token放行", "/metrics", "203.0.113.10:1234", "Bearer secret", "", "", http.StatusOK},
		{"query带token放行", "/metrics", "203.0.113.10:1234", "", "", "?token=secret", http.StatusOK},
		{"错误token拒绝", "/metrics", "127.0.0.1:1234", "Bearer wrong", "", "", http.StatusUnauthorized},
		{"无token外网拒绝", "/metrics", "203.0.113.10:1234", "", "", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doMetricsRequest(mux, tc.path, tc.remoteAddr, tc.authorization, tc.forwardedFor, tc.query)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s 状态码 = %d, 期望 %d", tc.path, rec.Code, tc.wantStatus)
			}
		})
	}

	t.Run("带token时pprof可用", func(t *testing.T) {
		for _, path := range []string{"/debug/pprof/", "/debug/pprof/goroutine"} {
			rec := doMetricsRequest(mux, path, "203.0.113.10:1234", "Bearer secret", "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s 状态码 = %d, 期望 %d", path, rec.Code, http.StatusOK)
			}
		}
	})

	t.Run("带token时pprof错误token拒绝", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/debug/pprof/", "127.0.0.1:1234", "Bearer wrong", "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("pprof 状态码 = %d, 期望 %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("健康检查公开", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/health", "203.0.113.10:1234", "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("/health 状态码 = %d, 期望 %d", rec.Code, http.StatusOK)
		}
	})
}

func TestMetricsAuthWithoutToken(t *testing.T) {
	mux := newMetricsMux("")

	t.Run("回环地址放行", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/metrics", "127.0.0.1:54321", "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("/metrics 状态码 = %d, 期望 %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("IPv6回环放行", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/metrics", "[::1]:54321", "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("/metrics 状态码 = %d, 期望 %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("外网拒绝", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/metrics", "203.0.113.10:54321", "", "", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("/metrics 状态码 = %d, 期望 %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("伪造XFF不能绕过", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/metrics", "203.0.113.10:54321", "", "127.0.0.1", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("/metrics 状态码 = %d, 期望 %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("无token时pprof关闭", func(t *testing.T) {
		rec := doMetricsRequest(mux, "/debug/pprof/", "127.0.0.1:54321", "", "", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("pprof 状态码 = %d, 期望 %d", rec.Code, http.StatusNotFound)
		}
	})
}

// TestMetricsServerRealAddrOverLoopback 走真实监听，校验真实 RemoteAddr 格式下的放行判定
func TestMetricsServerRealAddrOverLoopback(t *testing.T) {
	srv := httptest.NewServer(newMetricsMux(""))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics 状态码 = %d, 期望 %d（本机连接）", resp.StatusCode, http.StatusOK)
	}

	resp, err = http.Get(srv.URL + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("请求 pprof 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pprof 状态码 = %d, 期望 %d（无 token 应关闭）", resp.StatusCode, http.StatusNotFound)
	}
}

func TestIsLoopbackRequest(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:1234":        true,
		"[::1]:1234":            true,
		"127.0.0.1":             true,
		"10.0.0.5:1234":         false,
		"[::ffff:8.8.8.8]:1234": false,
		"":                      false,
		"not-an-ip:1234":        false,
	}
	for addr, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = addr
		if got := isLoopbackRequest(req); got != want {
			t.Errorf("isLoopbackRequest(%q) = %v, 期望 %v", addr, got, want)
		}
	}
}

// TestServiceStartOrderPutsMapBeforeLua 地图服务必须先于 Lua 启动：RunMap 负责把
// Npc 类注册进 Lua 运行时并装载地图，顺序颠倒时脚本里的 Npc()/SetMapId 全部落空，
// 且只留下「配置在未知的地图上」这种误导性日志。
func TestServiceStartOrderPutsMapBeforeLua(t *testing.T) {
	app := &App{}
	app.registerServices()

	order := map[string]int{}
	for i, s := range app.services {
		order[s.Name()] = i
	}
	for _, name := range []string{"timer", "map", "lua"} {
		if _, ok := order[name]; !ok {
			t.Fatalf("未注册服务: %s", name)
		}
	}
	if order["map"] > order["lua"] {
		t.Fatalf("✘ 地图必须早于 Lua 启动: map=%d lua=%d", order["map"], order["lua"])
	}
	if order["timer"] > order["map"] {
		t.Fatalf("✘ 定时器必须最先启动: timer=%d map=%d", order["timer"], order["map"])
	}
}

// TestCacheServiceRegisteredLast 缓存服务必须排在服务列表末尾：Shutdown 按注册
// 反序停止，排最后＝最先停——cache 的 final flush（把写缓冲残余落库）由 Stop
// 同步完成，必须早于 Shutdown 尾部 closeDatabase 关掉 MySQL/Mongo，否则退出时
// 丢最后一批脏数据。
func TestCacheServiceRegisteredLast(t *testing.T) {
	app := &App{}
	app.registerServices()

	if len(app.services) == 0 {
		t.Fatal("未注册任何服务")
	}
	last := app.services[len(app.services)-1]
	if last.Name() != "cache" {
		t.Fatalf("cache 服务必须在列表末尾，实际末尾是 %s", last.Name())
	}
}

// TestStartRunsNpcValidationWithoutBlocking 启动流程必须真的跑一遍 NPC 配置校验
// （ValidateNpcs 导出后一度没有任何生产调用方），但校验只出告警：
// 一处配置写错不该让整个进程起不来。
func TestStartRunsNpcValidationWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "990001.json")
	if err := os.WriteFile(path, []byte(`{"mapid":990001,"node":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&mapmanager.Map{}).Load(path); err != nil {
		t.Fatalf("装载测试地图失败: %v", err)
	}
	t.Cleanup(func() { mapmanager.StopMap(context.Background()) })

	// 无名称、无外观：ValidateNpcs 必然报问题
	broken := &mapmanager.Npc{}
	broken.SetMapId(990001)
	if problems := mapmanager.ValidateNpcs(); len(problems) == 0 {
		t.Fatal("✘ 前置条件：这份 NPC 配置应被校验报出问题")
	}

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		Cfg:      &config.Cfg{},
		services: []Service{&mockService{name: "fake"}},
	}
	if err := app.Start(); err != nil {
		t.Fatalf("✘ NPC 配置问题不该阻断启动: %v", err)
	}
	if !app.started {
		t.Fatal("✘ 启动未完成")
	}
}
