package touchgocore

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/metrics"
	"touchgocore/vars"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	metricsServer   *http.Server
	metricsServerMu sync.Mutex
)

// metricsReadHeaderTimeout 防止慢客户端只发半截请求头就把监控端口占住。
const metricsReadHeaderTimeout = 5 * time.Second

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
	// 时间轮的实时积压由拉取式采集器提供：注册动作放在这里，宿主不调
	// StartMetricsServer 就不会多挂任何采集器。
	registerTimerBacklogCollector()
	vars.Info("Prometheus 指标初始化完成")
}

func StartMetricsServer(port int, token string) {
	if port <= 0 {
		port = 9090
	}

	InitMetrics()

	addr := "[::]:" + strconv.Itoa(port)
	// 先绑新端口再动旧服务：直接换掉 metricsServer 的话，端口被占用时
	// 旧监控已被 Shutdown、新的又起不来，进程从此没有 /metrics。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		vars.Error("Prometheus metrics 监听 %s 失败: %v", addr, err)
		return
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           newMetricsMux(token),
		ReadHeaderTimeout: metricsReadHeaderTimeout,
	}

	metricsServerMu.Lock()
	prev := metricsServer
	metricsServer = server
	metricsServerMu.Unlock()

	if prev != nil {
		// 二次启动（改端口/换 token）必须收掉上一轮：否则旧端口和旧协程
		// 一直挂在进程里，监控端口越开越多。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := prev.Shutdown(ctx); err != nil {
			vars.Error("旧 Prometheus metrics 服务器关闭失败: %v", err)
		}
		cancel()
	}

	go func() {
		if token == "" {
			vars.Warning("Prometheus 未配置 token，/metrics 仅允许本机访问，/debug/pprof 已关闭")
		}
		vars.Info("Prometheus metrics 服务器启动, 端口: %d", port)
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			vars.Error("Prometheus metrics 服务器错误: %v", err)
			metricsServerMu.Lock()
			if metricsServer == server {
				metricsServer = nil
			}
			metricsServerMu.Unlock()
		}
	}()
}

// newMetricsMux 组装监控端点。token 为空时 /metrics 只放行回环地址，pprof 直接关闭。
func newMetricsMux(token string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsAuth(token, promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// pprof 端点（生产环境建议在反代层做 IP 白名单）
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/pprof/", pprof.Index)
	pprofMux.HandleFunc("/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/pprof/trace", pprof.Trace)
	pprofMux.Handle("/pprof/heap", pprof.Handler("heap"))
	pprofMux.Handle("/pprof/goroutine", pprof.Handler("goroutine"))
	pprofMux.Handle("/pprof/allocs", pprof.Handler("allocs"))
	pprofMux.Handle("/pprof/block", pprof.Handler("block"))
	pprofMux.Handle("/pprof/mutex", pprof.Handler("mutex"))
	pprofMux.Handle("/pprof/threadcreate", pprof.Handler("threadcreate"))
	// 内层 mux 注册的是 /pprof/... 路径，因此只剥掉 /debug 前缀
	mux.Handle("/debug/pprof/", profileAuth(token, http.StripPrefix("/debug", pprofMux)))

	return mux
}

// metricsAuth 保护 /metrics：有 token 时校验；无 token 时仅放行回环地址。
// 只信任连接来源，不读 X-Forwarded-For / X-Real-IP，避免被伪造。
func metricsAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			if !isLoopbackRequest(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimSpace(bearerPrefixOf(r.Header.Get("Authorization")))
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

// bearerPrefixOf 取出 Authorization 头里的凭据。RFC 7235 的 scheme 大小写不敏感，
// 只 TrimPrefix("Bearer") 会把 "bearer xxx" 的合法客户端整个当成 token 比对而拒掉。
// 不带 scheme 前缀时按裸凭据处理，沿用原有的宽松行为。
func bearerPrefixOf(raw string) string {
	raw = strings.TrimSpace(raw)
	const scheme = "Bearer"
	if len(raw) > len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
		if c := raw[len(scheme)]; c == ' ' || c == '\t' {
			return strings.TrimSpace(raw[len(scheme):])
		}
	}
	return raw
}

// profileAuth 保护 pprof：性能剖面会泄露堆内容与内部信息，未配置 token 时直接 404。
func profileAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return http.NotFoundHandler()
	}
	return metricsAuth(token, next)
}

// isLoopbackRequest 判断请求是否来自本机连接
func isLoopbackRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	// 摘除只认「还是我这一轮」：期间可能已有新的 StartMetricsServer 接管
	metricsServerMu.Lock()
	if metricsServer == srv {
		metricsServer = nil
	}
	metricsServerMu.Unlock()
	if err := srv.Shutdown(ctx); err != nil {
		vars.Error("Prometheus metrics 关闭失败: %v", err)
		// Shutdown 超时说明还有连接不放手；不 Close 的话端口一直被占，
		// 调用方重试或换端口都起不来。
		_ = srv.Close()
		return
	}
	vars.Info("Prometheus metrics 服务器关闭")
}
