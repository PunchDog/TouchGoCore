package metrics

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/vars"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	once     sync.Once
	registry = prometheus.NewRegistry()

	wsConnectionsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "touchgocore_websocket_connections_current",
		Help: "当前 WebSocket 活跃连接数",
	})
	wsMessagesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_websocket_messages_total",
		Help: "WebSocket 消息总数",
	}, []string{"direction"})
	wsErrorsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_websocket_errors_total",
		Help: "WebSocket 错误总数",
	}, []string{"type"})

	rpcRequestsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_grpc_requests_total",
		Help: "gRPC 请求总数",
	}, []string{"service", "method"})
	rpcLatencyHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "touchgocore_grpc_request_duration_seconds",
		Help:    "gRPC 请求耗时分布",
		Buckets: prometheus.DefBuckets,
	}, []string{"service", "method"})
	rpcErrorsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_grpc_errors_total",
		Help: "gRPC 错误总数",
	}, []string{"service", "method", "error_type"})

	httpRequestsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_http_requests_total",
		Help: "HTTP 请求总数",
	}, []string{"method", "path", "status"})
	httpLatencyHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "touchgocore_http_request_duration_seconds",
		Help:    "HTTP 请求耗时分布",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	timerActiveGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "touchgocore_timer_active_count",
		Help: "当前活跃定时器数量",
	}, []string{"name"})
	timerExecCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_timer_executions_total",
		Help: "定时器执行总数",
	}, []string{"type"})

	luaInstancesGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "touchgocore_lua_instances_active",
		Help: "当前活跃 Lua 实例数",
	})
	luaCallCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_lua_calls_total",
		Help: "Lua 函数调用总数",
	}, []string{"function"})
	luaCallLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "touchgocore_lua_call_duration_seconds",
		Help:    "Lua 函数调用耗时",
		Buckets: prometheus.DefBuckets,
	}, []string{"function"})

	dbConnectionsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "touchgocore_db_connections",
		Help: "数据库连接数",
	}, []string{"db_type", "state"})

	logMessagesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_log_messages_total",
		Help: "日志消息总数",
	}, []string{"level"})
)

// Init 把本包的采集器装配进独立 registry。幂等，且永不 panic：
// 作为库被引入时，注册失败（重名、registry 已被替换）只能让对应指标缺失，
// 不能把 MustRegister 的 panic 抛进宿主进程的启动路径。
func Init() {
	once.Do(func() {
		collectors := []prometheus.Collector{
			wsConnectionsGauge,
			wsMessagesCounter,
			wsErrorsCounter,
			rpcRequestsCounter,
			rpcLatencyHistogram,
			rpcErrorsCounter,
			httpRequestsCounter,
			httpLatencyHistogram,
			timerActiveGauge,
			timerExecCounter,
			luaInstancesGauge,
			luaCallCounter,
			luaCallLatency,
			dbConnectionsGauge,
			logMessagesCounter,
			// 不带 Go/Process 采集器时，/metrics 只有一堆业务计数器，
			// 内存、goroutine、CPU 这些最常见的告警维度全部缺失。
			prometheus.NewGoCollector(),
			prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		}
		for _, c := range collectors {
			if err := registry.Register(c); err != nil {
				vars.Error("Prometheus 指标注册失败: %v", err)
			}
		}
	})
}

func Registry() *prometheus.Registry {
	Init()
	return registry
}

var WS = wsMetrics{}

type wsMetrics struct{}

func (wsMetrics) SetConnections(n float64) { Init(); wsConnectionsGauge.Set(n) }
func (wsMetrics) IncConnection()           { Init(); wsConnectionsGauge.Inc() }
func (wsMetrics) DecConnection()           { Init(); wsConnectionsGauge.Dec() }
func (wsMetrics) IncMessages(direction string) {
	Init()
	wsMessagesCounter.WithLabelValues(direction).Inc()
}
func (wsMetrics) IncErrors(errType string) {
	Init()
	wsErrorsCounter.WithLabelValues(errType).Inc()
}

var RPC = rpcMetrics{}

type rpcMetrics struct{}

func (rpcMetrics) IncRequests(service, method string) {
	Init()
	rpcRequestsCounter.WithLabelValues(service, method).Inc()
}
func (rpcMetrics) ObserveLatency(service, method string, duration time.Duration) {
	Init()
	rpcLatencyHistogram.WithLabelValues(service, method).Observe(duration.Seconds())
}
func (rpcMetrics) IncErrors(service, method, errType string) {
	Init()
	rpcErrorsCounter.WithLabelValues(service, method, errType).Inc()
}

var HTTP = httpMetrics{}

type httpMetrics struct{}

// maxHTTPPathLabels 限制 path 标签的序列数：每个唯一路径一条时间序列，
// 未做模板收敛时 /user/1、/user/2… 会把 Prometheus 的基数打爆。
const maxHTTPPathLabels = 256

const httpOtherLabel = "other"

var (
	httpPathSeen  sync.Map // 已放行的 path 标签
	httpPathCount atomic.Int64
)

// httpPathLabel 收敛 path 标签基数：路由模板（含 ':'、'*' 或 '{'）原样保留，
// 裸路径超过上限后统一归并为 other。
func httpPathLabel(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return httpOtherLabel
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if t := strings.Trim(p, "/"); t == "" {
		return "/"
	}
	if strings.ContainsAny(p, ":*{") {
		return p // 已是路由模板，序列数由路由条数决定
	}
	if _, ok := httpPathSeen.Load(p); ok {
		return p
	}
	if httpPathCount.Add(1) > maxHTTPPathLabels {
		return httpOtherLabel
	}
	httpPathSeen.Store(p, struct{}{})
	return p
}

func (httpMetrics) IncRequests(method, path, status string) {
	Init()
	httpRequestsCounter.WithLabelValues(method, httpPathLabel(path), status).Inc()
}
func (httpMetrics) ObserveLatency(method, path string, duration time.Duration) {
	Init()
	httpLatencyHistogram.WithLabelValues(method, httpPathLabel(path)).Observe(duration.Seconds())
}

var Timer = timerMetrics{}

type timerMetrics struct{}

// SetActive 记录某一类定时器的活跃数；name 由调用方给出（如档位或管理器名），
// 否则多管理器进程里所有活跃数会互相覆盖成最后一个写入者的值。
func (timerMetrics) SetActive(name string, n float64) {
	Init()
	timerActiveGauge.WithLabelValues(name).Set(n)
}
func (timerMetrics) IncExecutions(timerType string) {
	Init()
	timerExecCounter.WithLabelValues(timerType).Inc()
}

var Lua = luaMetrics{}

type luaMetrics struct{}

func (luaMetrics) SetInstances(n float64) { Init(); luaInstancesGauge.Set(n) }
func (luaMetrics) IncCalls(funcName string) {
	Init()
	luaCallCounter.WithLabelValues(funcName).Inc()
}
func (luaMetrics) ObserveCallLatency(funcName string, duration time.Duration) {
	Init()
	luaCallLatency.WithLabelValues(funcName).Observe(duration.Seconds())
}

var DB = dbMetrics{}

type dbMetrics struct{}

func (dbMetrics) SetConnections(dbType, state string, n float64) {
	Init()
	dbConnectionsGauge.WithLabelValues(dbType, state).Set(n)
}

var Log = logMetrics{}

type logMetrics struct{}

func (logMetrics) IncMessages(level string) {
	Init()
	logMessagesCounter.WithLabelValues(level).Inc()
}
