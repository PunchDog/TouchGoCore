package touchgocore

// 阶段11 复核用例：监控端点的生命周期与鉴权解析。
// 本机无 gcc，go test -race 不可用，端口是否真的释放用「能否重新 bind」判定。

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// holdPort 占一个空闲端口并返回仍然持有它的监听器（避免测试间隙被别处抢走）。
func holdPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	return ln.Addr().(*net.TCPAddr).Port, ln
}

// freePort 取一个空闲端口并立刻释放，交给 StartMetricsServer 去绑。
func freePort(t *testing.T) int {
	t.Helper()
	port, ln := holdPort(t)
	_ = ln.Close()
	return port
}

// pollPortFree 轮询直到 addr 可重新绑定。
func pollPortFree(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func metricsGet(t *testing.T, url, authorization string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestStartMetricsServerSecondStartReleasesOldPort 回归（S61）：换端口/换 token
// 二次启动必须收掉上一轮。修复前只覆盖 metricsServer 指针，旧监听器与旧协程
// 一起留在进程里，监控端口越开越多且再也关不掉。
func TestStartMetricsServerSecondStartReleasesOldPort(t *testing.T) {
	oldPort := freePort(t)
	newPort := freePort(t)
	oldAddr := "[::]:" + strconv.Itoa(oldPort)

	StartMetricsServer(oldPort, "token-a")
	StartMetricsServer(newPort, "token-b")
	t.Cleanup(func() {
		ShutdownMetrics(context.Background())
		http.DefaultClient.CloseIdleConnections()
	})

	if !pollPortFree(oldAddr, 3*time.Second) {
		t.Fatal("✘ 二次启动后旧监控端口仍被占用（监听器/协程泄漏）")
	}
	metricsServerMu.Lock()
	cur := metricsServer
	metricsServerMu.Unlock()
	if cur == nil || cur.Addr != "[::]:"+strconv.Itoa(newPort) {
		t.Fatalf("✘ 当前监控服务器不是新一轮: %v", cur)
	}
}

// TestShutdownMetricsReleasesPortAfterFailedRestart 回归（S61）：新端口绑定失败时
// 不能把旧服务的句柄丢掉。修复前 metricsServer 被一个起不来的实例覆盖，
// 随后的 ShutdownMetrics 只对空壳生效，旧端口永久占住。
func TestShutdownMetricsReleasesPortAfterFailedRestart(t *testing.T) {
	port := freePort(t)
	addr := "[::]:" + strconv.Itoa(port)
	StartMetricsServer(port, "token-a")
	t.Cleanup(func() { ShutdownMetrics(context.Background()) })

	StartMetricsServer(port, "token-b") // 同一端口，必然绑不上
	ShutdownMetrics(context.Background())
	if !pollPortFree(addr, 3*time.Second) {
		t.Fatal("✘ 启动失败后旧监控服务无法关闭，端口被永久占住")
	}
}

// TestMetricsBearerSchemeCaseInsensitive 回归（S61）：Authorization 的 scheme
// 按 RFC 7235 大小写不敏感解析，且 scheme 与凭据之间允许多个空白。
func TestMetricsBearerSchemeCaseInsensitive(t *testing.T) {
	for _, h := range []string{"Bearer secret", "bearer secret", "BEARER\tsecret", "  Bearer secret  ", "secret"} {
		if got := bearerPrefixOf(h); got != "secret" {
			t.Fatalf("✘ %q 解析成 %q, 应为 secret", h, got)
		}
	}
	if got := bearerPrefixOf("Bearerish"); got != "Bearerish" {
		t.Fatalf("✘ 非 scheme 前缀被误剥: %q", got)
	}

	mux := newMetricsMux("secret")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if code := metricsGet(t, srv.URL+"/metrics", "bearer secret"); code != http.StatusOK {
		t.Fatalf("✘ 小写 bearer 被拒: status=%d", code)
	}
	if code := metricsGet(t, srv.URL+"/metrics", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Fatalf("✘ 错误 token 未被拒: status=%d", code)
	}
}
