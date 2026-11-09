package touchgocore

import (
	"context"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/localtimer"
	"touchgocore/mapmanager"
	"touchgocore/metrics"
	"touchgocore/nft"
	"touchgocore/pay"
	"touchgocore/rpc"
	"touchgocore/syncmap"
	"touchgocore/ton"
	"touchgocore/util"
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

	// 服务侧是用 ctx.Deadline() 反算预算的，从建 ctx 到 Stop 真正被调度进来
	// 有一段调度开销，必然略小于标称值；容差取 50ms，与「预算被压成 1s/0」
	// 这类真实回归差一个数量级，不会掩盖问题。
	const budgetSlack = 50 * time.Millisecond
	for _, svc := range services {
		observed := svc.observed()
		if len(observed) != 1 {
			t.Fatalf("服务[%s] 被停止 %d 次，期望 1 次", svc.name, len(observed))
		}
		if b := observed[0]; b < minServiceStopBudget-budgetSlack {
			t.Fatalf("服务[%s] 拿到的预算 %v 低于下限 %v（容差 %v）", svc.name, b, minServiceStopBudget, budgetSlack)
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

type backlogNoopTimer struct {
	localtimer.Timer
}

func (b *backlogNoopTimer) Tick() {}

// gatherTimerFamilies 抓取一次并挑出时间轮相关的指标族。
func gatherTimerFamilies(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := make(map[string]*dto.MetricFamily)
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "touchgocore_timer_") ||
			strings.HasPrefix(mf.GetName(), "touchgocore_async_timer_") {
			out[mf.GetName()] = mf
		}
	}
	return out
}

// seriesValue 取某个序列的值。labelKey 为空表示匹配「没有标签的那一条」。
func seriesValue(mf *dto.MetricFamily, labelKey, labelVal string) (float64, bool) {
	if mf == nil {
		return 0, false
	}
	for _, m := range mf.GetMetric() {
		if labelKey == "" {
			if len(m.GetLabel()) != 0 {
				continue
			}
		} else {
			matched := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelKey && lp.GetValue() == labelVal {
					matched = true
				}
			}
			if !matched {
				continue
			}
		}
		switch {
		case m.GetGauge() != nil:
			return m.GetGauge().GetValue(), true
		case m.GetCounter() != nil:
			return m.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

func TestTimerBacklogMetricsExposed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localtimer.Run(ctx)
	defer localtimer.TimeStop(context.Background())

	// 间隔取 60 秒而不是几十毫秒：毫秒级定时器会在断言期间被派发、续期、离链，
	// 「在链数」随之波动，断言只能写成 >=1 这种恒真的宽松形式。分钟轮上的它整个
	// 用例期间都不会迁移，数值可精确核对。
	tm, err := localtimer.NewTimer[*backlogNoopTimer](60*1000, localtimer.InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := localtimer.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	// 等入链通道被轮协程消化（否则 queue_depth 与 in_wheel 都还是 0）
	time.Sleep(150 * time.Millisecond)

	poolBefore, ok := seriesValue(mustGather(t)["touchgocore_timer_pool_in_use"], "", "")
	if !ok {
		t.Fatal("✘ 缺少池外借指标")
	}

	InitMetrics()
	fams := mustGather(t)

	depth := fams["touchgocore_timer_queue_depth"]
	if depth == nil {
		t.Fatalf("✘ 缺少调度通道积压指标，现有时间轮指标：%v", keysOf(fams))
	}
	if _, ok := seriesValue(depth, "queue", "schedule"); !ok {
		t.Fatal("✘ queue_depth 未含 schedule 序列")
	}
	for _, wheel := range wheelSeriesNames {
		if _, ok := seriesValue(depth, "queue", "wheel:"+wheel); !ok {
			t.Fatalf("✘ queue_depth 缺少入链通道序列 wheel:%s", wheel)
		}
	}

	// 容量不能只断「族存在」：它是告警表达式里的分母，读成 0 会让所有
	// 「深度/容量」比率告警恒假，而这正是最容易写错又最难发现的一种缺失。
	capacity := fams["touchgocore_timer_queue_capacity"]
	shardDepth := fams["touchgocore_timer_schedule_shard_depth"]
	shardCapacity := fams["touchgocore_timer_schedule_shard_capacity"]
	if shardDepth == nil || shardCapacity == nil {
		t.Fatalf("✘ 缺少调度分片积压指标，现有时间轮指标：%v", keysOf(fams))
	}
	wantSchedule := float64(localtimer.MaxTimerChannelNum)
	if v, ok := seriesValue(capacity, "queue", "schedule"); !ok || v != wantSchedule {
		t.Fatalf("✘ schedule 容量异常: got=%v want=%v", v, wantSchedule)
	}
	for _, wheel := range wheelSeriesNames {
		want := float64(localtimer.MaxAddTimerChannelNum)
		if v, ok := seriesValue(capacity, "queue", "wheel:"+wheel); !ok || v != want {
			t.Fatalf("✘ wheel:%s 容量异常: got=%v ok=%v want=%v", wheel, v, ok, want)
		}
	}

	// 逐片序列（S68）另用一组指标名，不塞进 queue 标签：那样不按计划过滤的
	// sum(queue_depth) 会把总量与各片量重复计入。默认单片也必须出这片序列，
	// 否则开关一开整组序列凭空出现，看板上历史全断。
	shards := localtimer.ScheduleShards()
	sumShardCap := float64(0)
	for i := 0; i < shards; i++ {
		name := strconv.Itoa(i)
		if _, ok := seriesValue(shardDepth, "shard", name); !ok {
			t.Fatalf("✘ schedule_shard_depth 缺少分片序列 %s（共 %d 片）", name, shards)
		}
		v, ok := seriesValue(shardCapacity, "shard", name)
		if !ok || v <= 0 {
			t.Fatalf("✘ schedule_shard_capacity 缺少分片序列 %s: got=%v ok=%v", name, v, ok)
		}
		sumShardCap += v
	}
	// 各片容量 = 总容量整除片数，不整除时余数弃掉，所以允许比总容量小 (片数-1)。
	if sumShardCap > wantSchedule || wantSchedule-sumShardCap >= float64(shards) {
		t.Fatalf("✘ 各片容量与总容量不是一套: sum=%v schedule=%v shards=%d",
			sumShardCap, wantSchedule, shards)
	}

	inWheel := fams["touchgocore_timer_in_wheel"]
	total, ok := seriesValue(inWheel, "wheel", "all")
	if !ok {
		t.Fatal("✘ 缺少在链定时器总数指标")
	}
	if total != 1 {
		t.Fatalf("✘ 在链总数与用例注册数不符: got=%v want=1", total)
	}
	// 按档序列：总数相同的「毫秒轮爆满」与「均匀分布」必须长得不一样
	perWheel := 0.0
	for _, wheel := range wheelSeriesNames {
		v, ok := seriesValue(inWheel, "wheel", wheel)
		if !ok {
			t.Fatalf("✘ in_wheel 缺少按档序列 wheel=%s", wheel)
		}
		perWheel += v
	}
	if v, _ := seriesValue(inWheel, "wheel", "minute"); v != 1 {
		t.Fatalf("✘ 60 秒间隔的定时器没落在分钟档: got=%v", v)
	}
	if perWheel != total {
		t.Fatalf("✘ 各档在链数与总数对不上: sum=%v all=%v", perWheel, total)
	}

	for _, name := range []string{"touchgocore_timer_dropped_total",
		"touchgocore_timer_reschedule_failed_total"} {
		if v, ok := seriesValue(fams[name], "", ""); !ok {
			t.Fatalf("✘ 缺少指标 %s", name)
		} else if v < 0 {
			t.Fatalf("✘ %s 出现负值: %v", name, v)
		}
	}
	asyncWorkers := localtimer.GetAsyncWorkerStats()
	asyncMetrics := map[string]float64{
		"touchgocore_async_timer_worker_limit": float64(asyncWorkers.Limit),
		"touchgocore_async_timer_worker_count": float64(asyncWorkers.Workers),
		"touchgocore_async_timer_queue_depth":  float64(asyncWorkers.Queued),
		"touchgocore_async_timer_in_flight":    float64(asyncWorkers.InFlight),
	}
	for name, want := range asyncMetrics {
		if got, ok := seriesValue(fams[name], "", ""); !ok || got != want {
			t.Fatalf("✘ async worker metric %s = %v (present=%t), want %v", name, got, ok, want)
		}
	}

	// 外借量必须能「涨」：只断非负的话，把口径写反成 Puts-Gets（负数）或恒 0 都能通过
	tm2, err := localtimer.NewTimer[*backlogNoopTimer](60*1000, localtimer.InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tm2.Remove()
	if after, ok := seriesValue(mustGather(t)["touchgocore_timer_pool_in_use"], "", ""); !ok || after < poolBefore+1 {
		t.Fatalf("✘ 新认领一个定时器后外借量没有增加: before=%v after=%v", poolBefore, after)
	}

	tm.Remove()
	t.Log("✔ 时间轮积压、容量、按档在链数、丢弃/续期失败与池外借量均可从 /metrics 读到")
}

// mustGather 抓取一次并只保留时间轮指标族。
func mustGather(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	InitMetrics()
	return gatherTimerFamilies(t)
}

// wheelSeriesNames 五档轮的标签文本，与 TimerType.String() 逐字一致。
var wheelSeriesNames = []string{"millisecond", "second", "minute", "ten-minute", "hour"}

// TestTimerBacklogSeriesSurviveWithoutManager 回归（阶段12 复核 F2）：默认管理器
// 不存在（尚未 Run、或 TimeStop 之后）时，按档序列必须仍以 0 出现。
//
// Prometheus 里「序列缺失」和「取值为 0」不是一回事：整档序列凭空消失时，
// 按 wheel 配的告警规则查不到数据而不是查到 0，等于在最需要观察的启动/收尾窗口
// 静默失明。
func TestTimerBacklogSeriesSurviveWithoutManager(t *testing.T) {
	localtimer.TimeStop(context.Background()) // 幂等；此时 defaultTimerManager 为空
	if mgr := localtimer.GetDefaultManager(); mgr != nil {
		t.Skip("默认管理器仍在，前序用例未收尾")
	}

	fams := mustGather(t)
	for _, name := range []string{"touchgocore_timer_queue_depth", "touchgocore_timer_queue_capacity",
		"touchgocore_timer_in_wheel"} {
		labelKey, prefix := "queue", "wheel:"
		if name == "touchgocore_timer_in_wheel" {
			labelKey, prefix = "wheel", ""
		}
		for _, wheel := range wheelSeriesNames {
			key := prefix + wheel
			if v, ok := seriesValue(fams[name], labelKey, key); !ok {
				t.Fatalf("✘ 未 Run 时 %s 缺少 %s 序列（序列缺失 ≠ 0，告警规则会失明）", name, key)
			} else if v != 0 {
				t.Fatalf("✘ 未 Run 时 %s 的 %s 应为 0, got=%v", name, key, v)
			}
		}
	}

	// 分片序列同理（S68）：未 Run 时深度与容量都报 0（此刻没有通道，报「理论每片
	// 容量」会让 depth/capacity 算出 0% 而看着像健康运行），但序列本身必须在。
	for _, name := range []string{"touchgocore_timer_schedule_shard_depth",
		"touchgocore_timer_schedule_shard_capacity"} {
		for i := 0; i < localtimer.ScheduleShards(); i++ {
			key := strconv.Itoa(i)
			if v, ok := seriesValue(fams[name], "shard", key); !ok {
				t.Fatalf("✘ 未 Run 时 %s 缺少 shard=%s 序列", name, key)
			} else if v != 0 {
				t.Fatalf("✘ 未 Run 时 %s 的 shard=%s 应为 0, got=%v", name, key, v)
			}
		}
	}
}

// TestTimerBacklogCollectorRegistersOnce 回归（S66）：InitMetrics 会被宿主启动流程
// 与 StartMetricsServer 重复调用，采集器注册必须幂等，否则重复抓取会看到翻倍序列。
func TestTimerBacklogCollectorRegistersOnce(t *testing.T) {
	InitMetrics()
	mf := gatherTimerFamilies(t)["touchgocore_timer_queue_depth"]
	if mf == nil {
		t.Fatal("✘ 采集器未注册")
	}
	before := len(mf.GetMetric())

	InitMetrics()
	InitMetrics()
	after := len(gatherTimerFamilies(t)["touchgocore_timer_queue_depth"].GetMetric())
	if before != after {
		t.Fatalf("✘ 重复 InitMetrics 导致序列翻倍: %d -> %d", before, after)
	}
	// 光比序列条数还证明不了「本采集器真的在 registry 里」——同名指标族也可能
	// 被别处注册出来。手动再注册一次必须撞 AlreadyRegistered，才算坐实。
	if err := metrics.Registry().Register(timerBacklogCollector{}); err == nil {
		t.Fatal("✘ 重复注册竟然成功，说明 InitMetrics 里根本没把采集器装进去")
	}
}

func keysOf(m map[string]*dto.MetricFamily) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ==================== 基准：每次抓取新增的代价 ====================

func BenchmarkTimerBacklogCollect(b *testing.B) {
	InitMetrics()
	c := timerBacklogCollector{}
	ch := make(chan prometheus.Metric, 64)
	drained := make(chan struct{})
	go func() {
		for range ch {
		}
		close(drained)
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Collect(ch)
	}
	b.StopTimer()
	close(ch)
	<-drained
}

func BenchmarkMetricsGatherWithTimerCollector(b *testing.B) {
	InitMetrics()
	benchmarkGather(b)
}

// BenchmarkMetricsGatherWithoutTimerCollector 同一 registry 摘掉时间轮采集器后的
// 抓取开销，两者之差即本项给每次抓取加的钱。
func BenchmarkMetricsGatherWithoutTimerCollector(b *testing.B) {
	InitMetrics()
	if !metrics.Registry().Unregister(timerBacklogCollector{}) {
		b.Fatal("时间轮采集器未注册，无法做对比")
	}
	defer func() {
		if err := metrics.Registry().Register(timerBacklogCollector{}); err != nil {
			b.Error(err)
		}
	}()
	benchmarkGather(b)
}

func benchmarkGather(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := metrics.Registry().Gather(); err != nil {
			b.Fatal(err)
		}
	}
}

// nftWireCfg 造一份「配好就能起链路」的 nft 段配置，指向调用方给的假供应商。
func nftWireCfg(baseURL string) (*config.Cfg, *config.NftSDKConfig) {
	sdk := &config.NftSDKConfig{
		Enable: "on", Driver: nft.DriverGeneric, BaseURL: baseURL,
		AppID: "wiring-app", SecretKey: "wiring-secret",
		Endpoints: map[string]string{"mint": "/mint", "query": "/query"},
		Accounts: map[string]*config.NftMerchantAccount{
			"main": {Enable: "on", MerchantID: "m-wire"},
		},
	}
	return &config.Cfg{
		Nft: &config.NftConfig{
			Provider: &config.NftSDKRef{SDK: "mynft", Account: "main"},
			Chain:    nft.ChainETH, Network: nft.NetworkMainnet,
		},
		NftSDks: map[string]*config.NftSDKConfig{"mynft": sdk},
	}, sdk
}

// TestNftServiceRegisteredInStartupList 服务列表必须真的登记了 nft 通道，且早于 gin：
// 网关先于资产通道起来时，第一批对外请求拿到的是「NFT 通道未启动」而不是结果。
func TestNftServiceRegisteredInStartupList(t *testing.T) {
	app := &App{}
	app.registerServices()

	order := map[string]int{}
	for i, s := range app.services {
		order[s.Name()] = i
	}
	if _, ok := order["nft"]; !ok {
		t.Fatal("服务列表漏登记 nft 通道：NFT 门面永远拿不到客户端")
	}
	if ginIdx, ok := order["gin"]; ok && order["nft"] > ginIdx {
		t.Fatalf("✘ nft 必须早于 gin 启动: nft=%d gin=%d", order["nft"], ginIdx)
	}
}

// TestNftServiceAdapterDelegatesToChannel 适配器的 Start/Stop 必须转调 nft 包，
// 而不是只「返回 nil 看着像成功了」。
func TestNftServiceAdapterDelegatesToChannel(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	cfg, _ := nftWireCfg(srv.URL)
	ctx := corectx.WithCfg(context.Background(), cfg)
	t.Cleanup(func() { nft.NftStop(nil) })

	s := &nftService{}
	if s.Name() != "nft" {
		t.Fatalf("服务名必须是 nft（配置段与运维口径认这个名字），实得 %q", s.Name())
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("已配好的 nft 段启动不应报错: %v", err)
	}
	_, mintErr := nft.NftMint(ctx, &nft.NftOrder{
		OrderNo: "wiring-mint-1", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://wiring",
	})
	if mintErr != nil && strings.Contains(mintErr.Error(), "未启动") {
		t.Fatalf("Start 没有真的把通道装配起来（nft.NftStart 是死调用？）: %v", mintErr)
	}
	if atomic.LoadInt64(&hits) == 0 {
		t.Fatalf("前置不成立：装配后一次外呼都没发生，这条闸就是空跑（mintErr=%v）", mintErr)
	}

	// Stop 必须摘掉客户端：可重复调用，且停后门面逐字拒绝。
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop 不应报错: %v", err)
	}
	if _, err := nft.NftMint(ctx, &nft.NftOrder{
		OrderNo: "wiring-mint-2", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://wiring",
	}); err == nil || !strings.Contains(err.Error(), "未启动") {
		t.Fatalf("Stop 后门面必须报未启动，实得 err=%v", err)
	}
}

// TestNftServiceStartWithoutConfigDoesNotBlockApp nft 段没配时 Start 必须返回 nil：
// 一个可选通道配错，不该让整机起不来（口径同 bscService）。
func TestNftServiceStartWithoutConfigDoesNotBlockApp(t *testing.T) {
	t.Cleanup(func() { nft.NftStop(nil) })

	if err := (&nftService{}).Start(corectx.WithCfg(context.Background(), &config.Cfg{})); err != nil {
		t.Fatalf("未配置 nft 段必须不阻断整机，实得: %v", err)
	}
	if _, err := nft.NftMint(context.Background(), &nft.NftOrder{
		OrderNo: "wiring-mint-3", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://wiring",
	}); err == nil || !strings.Contains(err.Error(), "未启动") {
		t.Fatalf("未配置就不该有可用链路，实得 err=%v", err)
	}
}

// TestNftCallbackPrefixContract 回调前缀是与下游（游戏逻辑/Lua 侧订阅方）的书面契约，
// 下游按字面量 "NftMsg"+动作 注册，本包常量一改它们就静默收不到回执。
// 自指引用（util.CallNftMsg）探不到这种改动，故这里按字面量钉，并证明广播真走这个键。
func TestNftCallbackPrefixContract(t *testing.T) {
	if util.CallNftMsg != "NftMsg" || util.CallNftSDKMsg != "NftSDK" {
		t.Fatalf("回调前缀契约漂移: CallNftMsg=%q CallNftSDKMsg=%q", util.CallNftMsg, util.CallNftSDKMsg)
	}

	var hits, broadcasts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","msg":"ok","data":{"order_no":"prefix-mint-1","status":"success","token_id":"1001","tx_hash":"0xtx"}}`))
	}))
	defer srv.Close()
	cfg, _ := nftWireCfg(srv.URL)
	ctx := corectx.WithCfg(context.Background(), cfg)
	t.Cleanup(func() { nft.NftStop(nil) })

	// 键名逐字写死，不用 util.CallNftMsg 拼——否则常量漂移时两侧一起变，探不到。
	id := util.DefaultCallFunc.Register("NftMsgMint", func(*nft.NftResult) {
		atomic.AddInt64(&broadcasts, 1)
	})
	defer util.DefaultCallFunc.Unregister("NftMsgMint", id)

	if err := (&nftService{}).Start(ctx); err != nil {
		t.Fatalf("启动不应报错: %v", err)
	}
	res, err := nft.NftMint(ctx, &nft.NftOrder{
		OrderNo: "prefix-mint-1", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://prefix",
	})
	if err != nil {
		t.Fatalf("夹具应拿到成功回执，实得 err=%v res=%+v", err, res)
	}
	if atomic.LoadInt64(&hits) == 0 {
		t.Fatal("前置不成立：一次外呼都没发生")
	}
	if atomic.LoadInt64(&broadcasts) != 1 {
		t.Fatalf("下游按契约键名注册后必须收到 1 次回执，实得 %d", broadcasts)
	}
}

// TestTonServiceRegistered TON 已从 telegramService 拆出，必须独立注册。
func TestTonServiceRegistered(t *testing.T) {
	app := &App{}
	app.registerServices()
	names := map[string]bool{}
	for _, s := range app.services {
		names[s.Name()] = true
	}
	if !names["ton"] {
		t.Fatal("启动服务列表里没有 ton")
	}
	if !names["telegram"] {
		t.Fatal("启动服务列表里丢了 telegram（Bot 仍在）")
	}
}

// TestTonServiceStartsWithoutConfig 没配 ton.native 时服务启动不报错、不绑通道。
func TestTonServiceStartsWithoutConfig(t *testing.T) {
	t.Cleanup(func() {
		ton.TonStop(context.Background())
		pay.Unbind(pay.KindTON)
	})
	svc := &tonService{}
	if svc.Name() != "ton" {
		t.Fatalf("服务名=%q，期望 ton", svc.Name())
	}
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("空配置启动 TON 服务不该报错: %v", err)
	}
	if _, err := pay.TransferFor(pay.KindTON); err == nil {
		t.Fatal("空配置把 TON 通道绑上了分派表")
	}
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("停止 TON 服务失败: %v", err)
	}
}
