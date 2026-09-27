package cache

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ==================== M5：FlushNow 不得吞掉 flushOnce 的错误 ====================
//
// 事故形状：FlushNow 里 flushOnce 的返回错误只进 vars.Error，函数继续往下走断言。
// 断言只看「我点名的那些键」，于是「别的键 final flush 仍失败、账本与 Redis 原样
// 保留」这种真的丢数据事实被一句日志糊过去，FlushNow 对调用方返回 nil——
// 资金级入口撒了「已落库」的谎。修复后底层错误一律透传，与断言错误 errors.Join。

// keyFailSaver 只对 failKeys 里的键报错，其余正常落库（错误可定位到具体键，
// 不像 recSaver.saveErr 那样一刀切，用来构造「目标键成功、别的键失败」）。
type keyFailSaver struct {
	mu       sync.Mutex
	saved    map[string]*testVal
	deleted  map[string]int
	failKeys map[string]bool
}

func newKeyFailSaver(failKeys ...string) *keyFailSaver {
	fk := make(map[string]bool, len(failKeys))
	for _, k := range failKeys {
		fk[k] = true
	}
	return &keyFailSaver{
		saved:    map[string]*testVal{},
		deleted:  map[string]int{},
		failKeys: fk,
	}
}

func (s *keyFailSaver) Save(_ context.Context, key string, val *testVal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failKeys[key] {
		return fmt.Errorf("db 拒绝写入 %s", key)
	}
	s.saved[key] = val
	return nil
}

func (s *keyFailSaver) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failKeys[key] {
		return fmt.Errorf("db 拒绝删除 %s", key)
	}
	s.deleted[key]++
	return nil
}

// allowAll 清空失败注入（模拟 DB 恢复）
func (s *keyFailSaver) allowAll() {
	s.mu.Lock()
	s.failKeys = map[string]bool{}
	s.mu.Unlock()
}

func (s *keyFailSaver) get(key string) *testVal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[key]
}

func (s *keyFailSaver) savedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

func (s *keyFailSaver) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.deleted {
		n += c
	}
	return n
}

var _ Saver[string, testVal] = (*keyFailSaver)(nil)

// 核心回归：点名的键落库成功，同批另一个键 final flush 仍失败。
// 修复前 FlushNow 返回 nil（错误只进了日志），修复后必须透传。
func TestFlushNow_PropagatesFlushErrorOfOtherKeys(t *testing.T) {
	sn := newKeyFailSaver("bad")
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()

	for _, k := range []string{"good", "bad"} {
		if err := h.c.Write(ctx, k, &testVal{N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	err := h.c.FlushNow(ctx, "good") // 只点名 good，它确实落库了
	if err == nil {
		t.Fatal("同批其它键 final flush 失败时 FlushNow 不得返回 nil（吞错）")
	}
	if !strings.Contains(err.Error(), "未落库") {
		t.Fatalf("应透传 flushOnce 的错误文本，得: %v", err)
	}
	if errors.Is(err, ErrFlushNotDurable) {
		t.Fatalf("good 已落库，不该误报为断言失败: %v", err)
	}
	if sn.get("good") == nil {
		t.Fatal("good 键应已落库")
	}
	if !h.c.Pending("bad") {
		t.Fatal("失败键必须留在缓冲等重试（final 语义不丢弃）")
	}
	if h.c.Pending("good") {
		t.Fatal("已落库的键不该仍报脏")
	}
}

// 点名键自身失败：断言错误与底层 flush 错误必须一起给出（errors.Join 两个信号都不丢）
func TestFlushNow_JoinsAssertionAndFlushError(t *testing.T) {
	sn := newKeyFailSaver("k1")
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	if err := h.c.Write(ctx, "k1", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	err := h.c.FlushNow(ctx, "k1")
	if err == nil {
		t.Fatal("落库失败时 FlushNow 绝不能返回 nil")
	}
	if !errors.Is(err, ErrFlushNotDurable) {
		t.Fatalf("仍脏必须可判定为 ErrFlushNotDurable: %v", err)
	}
	if !strings.Contains(err.Error(), "final flush") {
		t.Fatalf("底层 flushOnce 错误必须一并透传，得: %v", err)
	}
	if !h.c.Pending("k1") {
		t.Fatal("未落库脏数据必须留在缓冲")
	}
}

// 正常路径不受影响：全部落库成功时仍是 nil
func TestFlushNow_NoErrorWhenFlushClean(t *testing.T) {
	sn := newKeyFailSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	for _, k := range []string{"a", "b"} {
		if err := h.c.Write(ctx, k, &testVal{N: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.c.FlushNow(ctx, "a", "b"); err != nil {
		t.Fatalf("干净落库应返回 nil: %v", err)
	}
	if sn.savedCount() != 2 {
		t.Fatalf("两键都该落库: %d", sn.savedCount())
	}
}

// ==================== M7：saveBatch 单条路径的协程必须有界 ====================
//
// 事故形状：单条落库路径「每条目一个 goroutine + 信号量卡在 Save 前」。信号量确实
// 把并发 Save 限在 SaveConcurrency，但协程数等于条目数——BatchSize=200 的一批就是
// 200 个协程堆在信号量上，大批量叠加多分片时协程数与调度抖动都不可控。
// 修复后取活式 worker pool：协程数恒为 min(SaveConcurrency, len(entries))。

// gateSaver 计数钩子：观测同时停在 Save 里的协程数峰值，并可阻塞到测试放行。
// 阻塞点放在 Save 内部，是为了让所有 worker 确定性地同时在场，
// 于是 runtime.NumGoroutine 的净增量就是本批真实存活的协程数。
type gateSaver struct {
	mu       sync.Mutex
	saved    map[string]*testVal
	inFlight int
	peak     int
	gate     chan struct{}
}

func (s *gateSaver) Save(_ context.Context, key string, val *testVal) error {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
	}
	s.mu.Lock()
	s.inFlight--
	s.saved[key] = val
	s.mu.Unlock()
	return nil
}

func (s *gateSaver) Delete(_ context.Context, _ string) error { return nil }

func (s *gateSaver) inFlightNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

func (s *gateSaver) peakOf() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

func (s *gateSaver) savedOf(key string) *testVal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[key]
}

var _ Saver[string, testVal] = (*gateSaver)(nil)

// 200 条脏数据一次 flush：净增协程数必须贴着 SaveConcurrency，而不是等于条目数。
func TestSaveBatch_WorkerPoolBoundsGoroutines(t *testing.T) {
	const entries = 200
	gate := make(chan struct{})
	sn := &gateSaver{saved: map[string]*testVal{}, gate: gate}
	cfg := testConfig()
	cfg.SaveConcurrency = 4
	cfg.BatchSize = 500 // 一批装下全部条目，确保只走一次 saveBatch
	h := newHarness(t, WithSaver[string, testVal](sn), WithConfig[string, testVal](cfg))
	ctx := context.Background()

	want := make(map[string]int, entries)
	for i := 0; i < entries; i++ {
		k := fmt.Sprintf("k%03d", i)
		want[k] = i
		if err := h.c.Write(ctx, k, &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.c.Stats().Snapshot().Dirty; got != entries {
		t.Fatalf("前置：脏键数=%d 期望 %d", got, entries)
	}

	before := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() { done <- h.c.Flush(ctx) }()

	// 等 4 个 worker 全部卡在 Save 里（此刻协程数就是本批的真实规模）
	waitFor(t, 5*time.Second, func() bool {
		return sn.inFlightNow() == cfg.SaveConcurrency
	}, "worker 未全部进入 Save（并发度没被用满）")

	delta := runtime.NumGoroutine() - before
	if delta > cfg.SaveConcurrency+8 {
		t.Fatalf("单条落库路径协程数必须有界：净增 %d（SaveConcurrency=%d，条目 %d）",
			delta, cfg.SaveConcurrency, entries)
	}
	if peak := sn.peakOf(); peak != cfg.SaveConcurrency {
		t.Fatalf("同时落库的协程峰值=%d 期望 %d", peak, cfg.SaveConcurrency)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// 落库语义不变：每条都落、值正确、脏计数清零
	if sn.savedCount() != entries {
		t.Fatalf("落库条数=%d 期望 %d", sn.savedCount(), entries)
	}
	for k, n := range want {
		if v := sn.savedOf(k); v == nil || v.N != n {
			t.Fatalf("键 %s 落库值不对: %v", k, v)
		}
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 0 {
		t.Fatalf("flush 后 dirty=%d 期望 0", got)
	}
}

func (s *gateSaver) savedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// worker pool 不得改变错误聚合：部分键失败 → 失败集合回插重试、成功集合照常销账。
func TestSaveBatch_PartialFailureAggregatesInPool(t *testing.T) {
	sn := newKeyFailSaver("k2", "k5")
	cfg := testConfig()
	cfg.SaveConcurrency = 4
	h := newHarness(t, WithSaver[string, testVal](sn), WithConfig[string, testVal](cfg))
	ctx := context.Background()

	keys := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("k%d", i)
		keys = append(keys, k)
		if err := h.c.Write(ctx, k, &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatalf("非 final flush 失败应回插而非报错: %v", err)
	}
	if sn.savedCount() != 8 {
		t.Fatalf("成功集合应为 8 条，得 %d", sn.savedCount())
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 2 {
		t.Fatalf("失败集合应回插，dirty=%d 期望 2", got)
	}
	if !h.c.Pending("k2") || !h.c.Pending("k5") {
		t.Fatal("失败的两个键都该留在缓冲")
	}
	if h.c.Pending("k1") || h.c.Pending("k9") {
		t.Fatal("成功的键不该仍脏")
	}

	// DB 恢复 → 再 flush 全部落库
	sn.allowAll()
	if err := h.c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if sn.savedCount() != 10 {
		t.Fatalf("重试后应全部落库，得 %d", sn.savedCount())
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 0 {
		t.Fatalf("dirty=%d 期望 0", got)
	}
}

// 混合 op：pool 里 upsert 走 Save、tombstone 走 Delete，语义与旧写法一致。
func TestSaveBatch_PoolHandlesUpsertAndDelete(t *testing.T) {
	sn := newKeyFailSaver()
	cfg := testConfig()
	cfg.SaveConcurrency = 3
	h := newHarness(t, WithSaver[string, testVal](sn), WithConfig[string, testVal](cfg))
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		k := fmt.Sprintf("k%d", i)
		if err := h.c.Write(ctx, k, &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"k1", "k4"} {
		if err := h.c.Remove(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if sn.savedCount() != 4 {
		t.Fatalf("应落 4 条 upsert，得 %d", sn.savedCount())
	}
	if sn.deleteCount() != 2 {
		t.Fatalf("应落 2 条删除，得 %d", sn.deleteCount())
	}
	if sn.get("k1") != nil || sn.get("k4") != nil {
		t.Fatal("被 Remove 的键不该出现在 upsert 结果里")
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 0 {
		t.Fatalf("dirty=%d 期望 0", got)
	}
}

// 条目数少于并发度：worker 数收敛到条目数，不留空转协程。
func TestSaveBatch_WorkersCappedByEntryCount(t *testing.T) {
	gate := make(chan struct{})
	sn := &gateSaver{saved: map[string]*testVal{}, gate: gate}
	cfg := testConfig()
	cfg.SaveConcurrency = 16
	h := newHarness(t, WithSaver[string, testVal](sn), WithConfig[string, testVal](cfg))
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := h.c.Write(ctx, fmt.Sprintf("k%d", i), &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.c.Flush(ctx) }()
	waitFor(t, 5*time.Second, func() bool { return sn.inFlightNow() == 2 }, "两条目应同时在 Save 里")
	if peak := sn.peakOf(); peak > 2 {
		t.Fatalf("峰值协程=%d 不该超过条目数 2", peak)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sn.savedCount() != 2 {
		t.Fatalf("两条都该落库，得 %d", sn.savedCount())
	}
}
