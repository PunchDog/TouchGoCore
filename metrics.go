package touchgocore

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"touchgocore/config"
	"touchgocore/metrics"
	"touchgocore/vars"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	metricsServer   *http.Server
	metricsServerMu sync.Mutex
)

var (
	WSMetrics    = metrics.WS
	RPCMetrics   = metrics.RPC
	HTTPMetrics  = metrics.HTTP
	TimerMetrics = metrics.Timer
	LuaMetrics   = metrics.Lua
	DBMetrics    = metrics.DB
	LogMetrics   = metrics.Log
)

func InitMetrics() {
	metrics.Init()
	vars.Info("Prometheus 指标初始化完成")
}

func StartMetricsServer(port int, token string) {
	if port <= 0 {
		port = 9090
	}

	InitMetrics()

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsAuth(token, promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    "[::]:" + strconv.Itoa(port),
		Handler: mux,
	}

	metricsServerMu.Lock()
	metricsServer = server
	metricsServerMu.Unlock()

	go func() {
		if token == "" {
			vars.Warning("Prometheus /metrics 未配置 token，建议 metrics.token 或仅内网访问")
		}
		vars.Info("Prometheus metrics 服务器启动, 端口: %d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			vars.Error("Prometheus metrics 服务器错误: %v", err)
		}
	}()
}

func metricsAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func StartMetrics(cfg *config.MetricsConfig) {
	if cfg == nil || !cfg.Enabled {
		vars.Info("Prometheus 监控未启用")
		return
	}
	port := cfg.Port
	if port <= 0 {
		port = 9090
	}
	StartMetricsServer(port, cfg.Token)
}

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
