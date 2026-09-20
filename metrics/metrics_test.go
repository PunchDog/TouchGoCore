package metrics

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInit_RegistryNonNil(t *testing.T) {
	Init()
	if Registry() == nil {
		t.Fatal("Registry 不应为 nil")
	}
}

func TestWSMetrics_BasicOps(t *testing.T) {
	WS.SetConnections(5)
	WS.IncConnection()
	WS.IncMessages("in")
	WS.IncErrors("decode")
	// 不 panic 即视为通过
}

func TestRPCMetrics_BasicOps(t *testing.T) {
	RPC.IncRequests("svc", "Method")
	RPC.ObserveLatency("svc", "Method", 100_000_000) // 100ms
	RPC.IncErrors("svc", "Method", "timeout")
}

func TestHTTPMetrics_BasicOps(t *testing.T) {
	HTTP.IncRequests("GET", "/api/v1/health", "200")
	HTTP.ObserveLatency("GET", "/api/v1/health", 5_000_000)
}

func TestTimerMetrics_BasicOps(t *testing.T) {
	Timer.SetActive("second", 10)
	Timer.IncExecutions("millisecond")
}

func TestLuaMetrics_BasicOps(t *testing.T) {
	Lua.SetInstances(1)
	Lua.IncCalls("update")
	Lua.ObserveCallLatency("update", 1_000_000)
}

func TestDBMetrics_BasicOps(t *testing.T) {
	DB.SetConnections("mysql", "open", 3)
}

func TestLogMetrics_BasicOps(t *testing.T) {
	Log.IncMessages("info")
}

// ----------------------------------------------------------------------------
// S61：注册时机、注册失败语义与标签基数
// ----------------------------------------------------------------------------

// TestHelperRegistersWithoutExplicitInit 回归：不调 InitMetrics/Init 的宿主
// 只调 helper，指标也必须进入 registry。修复前 helper 不触发注册，
// /metrics 里一条业务指标都没有，且没有任何告警——静默失效。
//
// 同时覆盖「注册失败不 panic」：这里把 once 复位后重复注册同一批采集器，
// registry 必然回 AlreadyRegistered，旧代码的 MustRegister 会直接 panic。
func TestHelperRegistersWithoutExplicitInit(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("✘ 指标注册失败把 panic 抛给了宿主进程: %v", r)
		}
	}()

	once = sync.Once{} // 让 helper 自己走一遍注册路径
	DB.SetConnections("redis", "idle", 7)
	family := "touchgocore_db_connections"
	if n := testutil.CollectAndCount(registry, family); n == 0 {
		t.Fatalf("✘ 仅调用 helper 时 %s 未注册，指标静默丢失", family)
	}
	if got := testutil.ToFloat64(dbConnectionsGauge.WithLabelValues("redis", "idle")); got != 7 {
		t.Fatalf("✘ helper 写入未反映到 registry: got=%v want=7", got)
	}
	once.Do(func() {}) // 恢复「已初始化」状态，避免污染后续用例
}

// TestGoCollectorRegistered 回归：/metrics 要带运行时指标，否则内存/goroutine
// 泄漏这类最常见的告警维度完全缺失。
func TestGoCollectorRegistered(t *testing.T) {
	Init()
	if n := testutil.CollectAndCount(registry, "go_goroutines"); n == 0 {
		t.Fatal("✘ registry 未挂 GoCollector，运行时指标全部缺失")
	}
}

// TestTimerActiveGaugeKeepsPerName 回归：活跃定时器数必须按 name 分列。
// 修复前是单值 Gauge，多个管理器互相覆盖，最后只剩最后一个写入者的数字。
func TestTimerActiveGaugeKeepsPerName(t *testing.T) {
	Timer.SetActive("millisecond", 3)
	Timer.SetActive("second", 5)

	if got := testutil.ToFloat64(timerActiveGauge.WithLabelValues("millisecond")); got != 3 {
		t.Fatalf("✘ millisecond 档活跃数被其它档位覆盖: got=%v want=3", got)
	}
	if got := testutil.ToFloat64(timerActiveGauge.WithLabelValues("second")); got != 5 {
		t.Fatalf("✘ second 档活跃数错误: got=%v want=5", got)
	}
	if n := testutil.CollectAndCount(registry, "touchgocore_timer_active_count"); n < 2 {
		t.Fatalf("✘ 活跃定时器未按 name 分列: series=%d", n)
	}
}

// TestHTTPPathLabelBoundsCardinality 回归：path 标签必须有基数护栏。
// 修复前把裸路径直接当标签，/user/1…/user/N 每条都生成一个时间序列，
// 一次爬虫或一个 ID 型路由就能把监控端打爆。
func TestHTTPPathLabelBoundsCardinality(t *testing.T) {
	httpPathSeen.Range(func(k, _ any) bool {
		httpPathSeen.Delete(k)
		return true
	})
	httpPathCount.Store(0)

	// 基数放大前先验单路径归一化规则，否则它们也会被上限吞成 other
	if got := httpPathLabel("/api/v1/list?page=2"); got != "/api/v1/list" {
		t.Fatalf("✘ 查询串未剥掉，会按参数散落序列: %q", got)
	}
	if got := httpPathLabel("/user/:id"); got != "/user/:id" {
		t.Fatalf("✘ 路由模板被误归并: %q", got)
	}
	if got := httpPathLabel(""); got != httpOtherLabel {
		t.Fatalf("✘ 空路径未归并: %q", got)
	}
	if got := httpPathLabel("/"); got != "/" {
		t.Fatalf("✘ 根路径被改写成 %q", got)
	}

	for i := 0; i < maxHTTPPathLabels+200; i++ {
		HTTP.IncRequests("GET", fmt.Sprintf("/user/%d", i), "200")
	}
	series := testutil.CollectAndCount(registry, "touchgocore_http_requests_total")
	if series > maxHTTPPathLabels+8 {
		t.Fatalf("✘ path 标签未设基数上限: series=%d cap=%d", series, maxHTTPPathLabels)
	}
	if got := httpPathLabel("/user/999999"); got != httpOtherLabel {
		t.Fatalf("✘ 超限路径应归并为 %q, got=%q", httpOtherLabel, got)
	}
}

// TestProcessCollectorRegistered 回归：进程采集器必须真的在 registry 里。
// Windows 上 ProcessCollector 不产出任何时间序列，所以只能按「重复注册被拒」
// 来验证它挂上了，不能靠 Gather 的输出。
func TestProcessCollectorRegistered(t *testing.T) {
	Init()

	err := registry.Register(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	are, ok := err.(prometheus.AlreadyRegisteredError)
	if !ok {
		t.Fatalf("✘ 进程采集器未注册（重复注册本应被拒）: err=%v", err)
	}
	if !strings.Contains(strings.ToLower(fmt.Sprintf("%T", are.ExistingCollector)), "processcollector") {
		t.Fatalf("✘ registry 里同名采集器不是进程采集器: %T", are.ExistingCollector)
	}
}
