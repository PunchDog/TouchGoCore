package gin

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
)

// ==================== 被测路由 ====================

type benchRouter struct {
	// 模拟真实业务里的共享可变状态，用来检验并发安全性
	counter int64
}

func (b *benchRouter) RouterType() []string { return nil }

// Ping 返回 map，走 sendResponse 的 Ptr/Map/Slic/Struct 分支
func (b *benchRouter) Ping(ctx *gin.Context) gin.H {
	n := atomic.AddInt64(&b.counter, 1)
	return gin.H{"code": 0, "msg": "ok", "seq": n}
}

// Echo 返回 string，走 sendResponse 的 String 分支，并顺便读一下 ctx 参数
func (b *benchRouter) Echo(ctx *gin.Context) string {
	return "echo:" + ctx.Request.URL.Path
}

// Busy 模拟一点 CPU 工作，贴近真实接口
func (b *benchRouter) Busy(ctx *gin.Context) gin.H {
	s := 0
	for i := 0; i < 64; i++ {
		s += i * i
	}
	return gin.H{"sum": s}
}

var benchOnce sync.Once

// registerBench 注册基准路由，幂等
func registerBench() {
	benchOnce.Do(func() {
		RegisterRouter(&benchRouter{}, map[string]int64{})
	})
}

// ==================== 1. Handler 层并发基准（剥离网络，直测封装开销） ====================

func BenchmarkWrappedHandler_Ping(b *testing.B) {
	gin.SetMode(gin.TestMode)
	registerBench()
	fn := routerMap["/benchrouter/ping"]
	if fn == nil {
		b.Fatal("route not registered")
	}

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/benchrouter/ping", nil)
			fn(c)
		}
	})
}

func BenchmarkWrappedHandler_Echo(b *testing.B) {
	gin.SetMode(gin.TestMode)
	registerBench()
	fn := routerMap["/benchrouter/echo"]
	if fn == nil {
		b.Fatal("route not registered")
	}

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/benchrouter/echo", nil)
			fn(c)
		}
	})
}

// ==================== 2. 方法缓存并发基准（验证 sync.Map 无竞争） ====================

func BenchmarkMethodCacheLookup(b *testing.B) {
	registerBench()
	rcvr := reflect.ValueOf(&benchRouter{})

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// 并发反复命中缓存，应全部走 sync.Map.Load，无锁竞争
			if _, err := getMethodCacheEntry(rcvr, "benchRouter", "Ping"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ==================== 3. 真实 HTTP 并发压测（全栈：gin 引擎 + CORS + 封装） ====================

// freePort 获取一个当前空闲的 TCP 端口
func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestHttpConcurrencyLoad 启动真实 HTTP 服务并施压，输出 QPS 与延迟分布
func TestHttpConcurrencyLoad(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	// 将 vars 初始化为 off 模式，使桥接后的 gin 访问日志（vars.Debug）完全静默：
	// 既真实执行桥接链路，又不产生任何 I/O，避免访问日志写出干扰吞吐测量。
	// 生产环境由业务侧 vars.Run(...) 按配置决定级别与落盘。
	// 注：off 模式下 vars 仍会打开一个日志文件句柄；用自建临时目录并在退出时
	// 忽略删除错误来规避 go test 对 t.TempDir 的强制清理失败。
	logDir, err := os.MkdirTemp("", "ginbench")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	vars.Run(logDir, "bench", "off")
	defer func() {
		vars.Shutdown()
		_ = os.RemoveAll(logDir)
	}()

	port := freePort(t)
	cfg := &config.Cfg{
		Web: &config.WebConfig{
			HTTPPort:     port,
			AllowOrigins: []string{"*"},
		},
	}
	registerBench()

	ctx, cancel := context.WithCancel(corectx.WithCfg(context.Background(), cfg))
	defer cancel()
	if err := Run(ctx); err != nil {
		t.Fatalf("run server: %v", err)
	}
	defer Stop(context.Background())

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// 使用带连接池的 Transport，避免默认 MaxIdleConnsPerHost=2 在高并发下
	// 不断新建连接、goroutine 爆炸导致 thread exhaustion
	transport := &http.Transport{
		MaxIdleConns:        400,
		MaxIdleConnsPerHost: 400,
		MaxConnsPerHost:     400,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	// 压测参数
	const (
		concurrency = 300    // 并发客户端数
		totalReqs   = 100000 // 总请求数
	)

	// 预热 + 建池：并发数与目标一致，带重试吸收 Windows accept 队列在 t=0 的
	// 瞬时溢出（connection refused）。预热成功后并发条 keep-alive 连接留在池中，
	// 实测阶段直接复用，不再新建连接 → 消除首波拨号毛刺。
	var wgWarm sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wgWarm.Add(1)
		go func() {
			defer wgWarm.Done()
			if resp, err := dialWithRetry(client, base+"/benchrouter/ping", 50); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wgWarm.Wait()
	time.Sleep(50 * time.Millisecond) // 让连接回到 idle 池

	// ---- 实测阶段 ----
	durations := make(chan time.Duration, totalReqs)
	var (
		okCount   int64
		errCount  int64
		bytesSum  int64
		startTime = time.Now()
	)
	var errMu sync.Mutex
	errSamples := make([]string, 0, 5) // 仅记录前几个错误用于诊断

	var wg sync.WaitGroup
	reqsPerWorker := totalReqs / concurrency

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < reqsPerWorker; j++ {
				t0 := time.Now()
				resp, err := client.Get(base + "/benchrouter/ping")
				d := time.Since(t0)
				durations <- d
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					errMu.Lock()
					if len(errSamples) < cap(errSamples) {
						errSamples = append(errSamples, err.Error())
					}
					errMu.Unlock()
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				atomic.AddInt64(&bytesSum, int64(n))
				resp.Body.Close()
				if resp.StatusCode == 200 {
					atomic.AddInt64(&okCount, 1)
				} else {
					atomic.AddInt64(&errCount, 1)
					errMu.Lock()
					if len(errSamples) < cap(errSamples) {
						errSamples = append(errSamples, fmt.Sprintf("HTTP %d", resp.StatusCode))
					}
					errMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	close(durations)
	elapsed := time.Since(startTime)

	// 排序计算分位延迟
	samples := make([]time.Duration, 0, totalReqs)
	for d := range durations {
		samples = append(samples, d)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	total := int64(len(samples))
	qps := float64(total) / elapsed.Seconds()

	t.Logf("====== Gin 封装并发压测 ======")
	t.Logf("并发数        : %d", concurrency)
	t.Logf("总请求数      : %d", total)
	t.Logf("成功/失败     : %d / %d", okCount, errCount)
	if total > 0 {
		t.Logf("错误率        : %.3f%%", 100*float64(errCount)/float64(total))
	}
	if len(errSamples) > 0 {
		t.Logf("错误样例      : %v", errSamples)
	}
	t.Logf("总耗时        : %v", elapsed.Round(time.Millisecond))
	t.Logf("QPS           : %.0f req/s", qps)
	t.Logf("平均吞吐      : %.2f MB/s", float64(bytesSum)/elapsed.Seconds()/1024/1024)
	if total > 0 {
		t.Logf("延迟 P50      : %v", percentile(samples, 0.50))
		t.Logf("延迟 P95      : %v", percentile(samples, 0.95))
		t.Logf("延迟 P99      : %v", percentile(samples, 0.99))
		t.Logf("延迟 Max      : %v", samples[total-1])
	}

	// 压测对瞬时并发毛刺容忍：错误率超过 1% 才视为异常
	if total > 0 && float64(errCount)/float64(total) > 0.01 {
		t.Errorf("并发压测错误率过高: %d/%d (%.2f%%)", errCount, total, 100*float64(errCount)/float64(total))
	}
}

// ==================== 辅助 ====================

// dialWithRetry 发起 GET，遇到连接被拒绝（accept 队列瞬时溢出）时退避重试，
// 用于预热阶段建立连接池；非连接类错误直接返回。
func dialWithRetry(client *http.Client, url string, maxRetry int) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		resp, err := client.Get(url)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "refused") {
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
