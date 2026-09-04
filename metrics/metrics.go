package metrics

import (
	"sync"
	"time"

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

	timerActiveGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "touchgocore_timer_active_count",
		Help: "当前活跃定时器数量",
	})
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

func Init() {
	once.Do(func() {
		for _, c := range []prometheus.Collector{
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
		} {
			registry.MustRegister(c)
		}
	})
}

func Registry() *prometheus.Registry {
	Init()
	return registry
}

var WS = wsMetrics{}

type wsMetrics struct{}

func (wsMetrics) SetConnections(n float64)     { wsConnectionsGauge.Set(n) }
func (wsMetrics) IncConnection()               { wsConnectionsGauge.Inc() }
func (wsMetrics) DecConnection()               { wsConnectionsGauge.Dec() }
func (wsMetrics) IncMessages(direction string) { wsMessagesCounter.WithLabelValues(direction).Inc() }
func (wsMetrics) IncErrors(errType string)     { wsErrorsCounter.WithLabelValues(errType).Inc() }

var RPC = rpcMetrics{}

type rpcMetrics struct{}

func (rpcMetrics) IncRequests(service, method string) {
	rpcRequestsCounter.WithLabelValues(service, method).Inc()
}
func (rpcMetrics) ObserveLatency(service, method string, duration time.Duration) {
	rpcLatencyHistogram.WithLabelValues(service, method).Observe(duration.Seconds())
}
func (rpcMetrics) IncErrors(service, method, errType string) {
	rpcErrorsCounter.WithLabelValues(service, method, errType).Inc()
}

var HTTP = httpMetrics{}

type httpMetrics struct{}

func (httpMetrics) IncRequests(method, path, status string) {
	httpRequestsCounter.WithLabelValues(method, path, status).Inc()
}
func (httpMetrics) ObserveLatency(method, path string, duration time.Duration) {
	httpLatencyHistogram.WithLabelValues(method, path).Observe(duration.Seconds())
}

var Timer = timerMetrics{}

type timerMetrics struct{}

func (timerMetrics) SetActive(n float64) { timerActiveGauge.Set(n) }
func (timerMetrics) IncExecutions(timerType string) {
	timerExecCounter.WithLabelValues(timerType).Inc()
}

var Lua = luaMetrics{}

type luaMetrics struct{}

func (luaMetrics) SetInstances(n float64)   { luaInstancesGauge.Set(n) }
func (luaMetrics) IncCalls(funcName string) { luaCallCounter.WithLabelValues(funcName).Inc() }
func (luaMetrics) ObserveCallLatency(funcName string, duration time.Duration) {
	luaCallLatency.WithLabelValues(funcName).Observe(duration.Seconds())
}

var DB = dbMetrics{}

type dbMetrics struct{}

func (dbMetrics) SetConnections(dbType, state string, n float64) {
	dbConnectionsGauge.WithLabelValues(dbType, state).Set(n)
}

var Log = logMetrics{}

type logMetrics struct{}

func (logMetrics) IncMessages(level string) { logMessagesCounter.WithLabelValues(level).Inc() }
