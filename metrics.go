package touchgocore

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/vars"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ==================== Prometheus 监控指标 ====================

var (
	promOnce        sync.Once
	metricsServer   *http.Server
	metricsServerMu sync.Mutex

	// 全局 Registry（使用自定义Registry避免与其他库冲突）
	registry = prometheus.NewRegistry()

	// ---- WebSocket 指标 ----
	wsConnectionsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "touchgocore_websocket_connections_current",
		Help: "当前 WebSocket 活跃连接数",
	})
	wsMessagesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_websocket_messages_total",
		Help: "WebSocket 消息总数",
	}, []string{"direction"}) // direction: inbound / outbound
	wsErrorsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_websocket_errors_total",
		Help: "WebSocket 错误总数",
	}, []string{"type"}) // type: parse / not_found / write

	// ---- gRPC 指标 ----
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

	// ---- HTTP (Gin) 指标 ----
	httpRequestsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_http_requests_total",
		Help: "HTTP 请求总数",
	}, []string{"method", "path", "status"})
	httpLatencyHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "touchgocore_http_request_duration_seconds",
		Help:    "HTTP 请求耗时分布",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	// ---- 定时器指标 ----
	timerActiveGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "touchgocore_timer_active_count",
		Help: "当前活跃定时器数量",
	})
	timerExecCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_timer_executions_total",
		Help: "定时器执行总数",
	}, []string{"type"}) // type: millisecond / second / minute / ten-minute / hour

	// ---- Lua 指标 ----
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

	// ---- 数据库指标 ----
	dbConnectionsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "touchgocore_db_connections",
		Help: "数据库连接数",
	}, []string{"db_type", "state"}) // state: active / idle

	// ---- 日志指标 ----
	logMessagesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "touchgocore_log_messages_total",
		Help: "日志消息总数",
	}, []string{"level"})
)

// InitMetrics 初始化 Prometheus 指标（注册到自定义Registry）
func InitMetrics() {
	promOnce.Do(func() {
		// 注册所有指标
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
		}

		for _, c := range collectors {
			registry.MustRegister(c)
		}

		vars.Info("Prometheus 指标初始化完成")
	})
}

// StartMetricsServer 启动独立的 metrics HTTP 服务器
// 默认在 :9090/metrics 暴露指标
func StartMetricsServer(port int) {
	if port <= 0 {
		port = 9090 // 默认端口
	}

	InitMetrics()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	// 健康检查端点
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    "[::]:" + strconv.Itoa(port),
		Handler: mux,
	}

	metricsServerMu.Lock()
	metricsServer = server
	metricsServerMu.Unlock()

	go func() {
		vars.Info("Prometheus metrics 服务器启动, 端口: %d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			vars.Error("Prometheus metrics 服务器错误: %v", err)
		}
	}()
}

// ==================== 便捷指标操作函数 ====================

// WSMetrics WebSocket指标便捷操作
var WSMetrics = wsMetrics{}

type wsMetrics struct{}

func (wsMetrics) SetConnections(n float64)     { wsConnectionsGauge.Set(n) }
func (wsMetrics) IncConnection()               { wsConnectionsGauge.Inc() }
func (wsMetrics) DecConnection()               { wsConnectionsGauge.Dec() }
func (wsMetrics) IncMessages(direction string) { wsMessagesCounter.WithLabelValues(direction).Inc() }
func (wsMetrics) IncErrors(errType string)     { wsErrorsCounter.WithLabelValues(errType).Inc() }

// RPCMetrics gRPC指标便捷操作
var RPCMetrics = rpcMetrics{}

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

// HTTPMetrics HTTP指标便捷操作
var HTTPMetrics = httpMetrics{}

type httpMetrics struct{}

func (httpMetrics) IncRequests(method, path, status string) {
	httpRequestsCounter.WithLabelValues(method, path, status).Inc()
}
func (httpMetrics) ObserveLatency(method, path string, duration time.Duration) {
	httpLatencyHistogram.WithLabelValues(method, path).Observe(duration.Seconds())
}

// TimerMetrics 定时器指标便捷操作
var TimerMetrics = timerMetrics{}

type timerMetrics struct{}

func (timerMetrics) SetActive(n float64) { timerActiveGauge.Set(n) }
func (timerMetrics) IncExecutions(timerType string) {
	timerExecCounter.WithLabelValues(timerType).Inc()
}

// LuaMetrics Lua指标便捷操作
var LuaMetrics = luaMetrics{}

type luaMetrics struct{}

func (luaMetrics) SetInstances(n float64)   { luaInstancesGauge.Set(n) }
func (luaMetrics) IncCalls(funcName string) { luaCallCounter.WithLabelValues(funcName).Inc() }
func (luaMetrics) ObserveCallLatency(funcName string, duration time.Duration) {
	luaCallLatency.WithLabelValues(funcName).Observe(duration.Seconds())
}

// DBMetrics 数据库指标便捷操作
var DBMetrics = dbMetrics{}

type dbMetrics struct{}

func (dbMetrics) SetConnections(dbType, state string, n float64) {
	dbConnectionsGauge.WithLabelValues(dbType, state).Set(n)
}

// LogMetrics 日志指标便捷操作
var LogMetrics = logMetrics{}

type logMetrics struct{}

func (logMetrics) IncMessages(level string) { logMessagesCounter.WithLabelValues(level).Inc() }

// ==================== 配置集成 ====================

// MetricsConfig 监控配置
type MetricsConfig struct {
	Enabled bool `json:"enabled"` // 是否启用监控
	Port    int  `json:"port"`    // metrics 端口
}

// StartMetrics 从配置启动监控
func StartMetrics(cfg *config.MetricsConfig) {
	if cfg == nil || !cfg.Enabled {
		vars.Info("Prometheus 监控未启用")
		return
	}
	port := cfg.Port
	if port <= 0 {
		port = 9090
	}
	StartMetricsServer(port)
}

// ShutdownMetrics 关闭 metrics 服务器
func ShutdownMetrics(ctx context.Context) {
	metricsServerMu.Lock()
	srv := metricsServer
	metricsServerMu.Unlock()
	if srv == nil {
		return
	}
	if err := srv.Shutdown(ctx); err != nil {
		vars.Error("Prometheus metrics 关闭失败: %v", err)
		return
	}
	vars.Info("Prometheus metrics 服务器关闭")
}
