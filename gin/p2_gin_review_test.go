package gin

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 阶段9 复核整改回归（S48）：启动失败必须释放已绑定的端口，
// 且全局 httpServer 不得指向一个从未起来过的 server。
// ============================================================================

func ginWebEnv(t *testing.T, port int, tls *config.TLSConfig) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	prev := config.Cfg_
	config.Cfg_ = &config.Cfg{Web: &config.WebConfig{HTTPPort: port, TLS: tls}}
	t.Cleanup(func() { config.Cfg_ = prev })
}

func freeGinPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestServeFailureReleasesPort TLS 证书读不到时 ServeTLS 立刻返回错误，
// 但它不会替调用方关已绑定的监听器：不补一次 Close，端口就被永久占住，
// 修好配置后也只能重启整个进程。
func TestServeFailureReleasesPort(t *testing.T) {
	port := freeGinPort(t)
	ginWebEnv(t, port, &config.TLSConfig{Enable: true, CertFile: "no-such-cert.crt", KeyFile: "no-such-key.key"})

	if err := Run(corectx.WithCfg(context.Background(), config.Cfg_)); err == nil {
		t.Fatal("✘ 证书不存在却启动成功")
	}

	ln, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("✘ 启动失败后端口仍被占用，下次启动只能报 address already in use: %v", err)
	}
	_ = ln.Close()

	httpMu.Lock()
	left := httpServer
	httpMu.Unlock()
	if left != nil {
		t.Fatal("✘ 启动失败后 httpServer 仍指向未起监听的 server，Stop 会去 Shutdown 一个死对象")
	}
}

// TestListenFailureKeepsServerNil 端口被占时 Run 必须报错，
// 并且不能把新 server 挂到全局变量上（否则 Stop 会关掉别人的连接）。
func TestListenFailureKeepsServerNil(t *testing.T) {
	port := freeGinPort(t)
	occupied, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	ginWebEnv(t, port, nil)
	if err := Run(corectx.WithCfg(context.Background(), config.Cfg_)); err == nil {
		t.Fatal("✘ 端口被占用却启动成功（修复前 ListenAndServe 在协程里异步失败，只靠 sleep 判定）")
	}
	httpMu.Lock()
	left := httpServer
	httpMu.Unlock()
	if left != nil {
		t.Fatal("✘ 启动失败仍写入了全局 httpServer")
	}
}

// TestStopAfterFailedRunIsNoop 启动失败后 Stop 不得 panic，也不得重复关闭。
func TestStopAfterFailedRunIsNoop(t *testing.T) {
	port := freeGinPort(t)
	ginWebEnv(t, port, &config.TLSConfig{Enable: true, CertFile: "no-such-cert.crt", KeyFile: "no-such-key.key"})
	_ = Run(corectx.WithCfg(context.Background(), config.Cfg_))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Stop(context.Background()); err != nil {
			t.Errorf("✘ 启动失败后 Stop 报错: %v", err)
		}
		if err := Stop(context.Background()); err != nil {
			t.Errorf("重复 Stop 报错: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("✘ Stop 阻塞")
	}
}
