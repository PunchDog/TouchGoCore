package util

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"math"
	randv2 "math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/random"
	"touchgocore/syncmap"
)

// TestConcurrentDoWithRetPerCallIsolation 并发触发同一 key 时，每次调用只能拿到自己的返回值。
// 旧的 Do+SetDoRet 机制把返回值写进接收者级共享切片，跨调用串味；
// 该通道已删除，返回值一律走调用栈。
func TestConcurrentDoWithRetPerCallIsolation(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("echo-tag", func(s string) string { return "ret:" + s })

	const n = 64
	var wg sync.WaitGroup
	mismatch := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := strconv.Itoa(i)
			rets, ok := c.DoWithRet("echo-tag", tag)
			if !ok || len(rets) != 1 {
				mismatch <- fmt.Sprintf("调用 %s 没有拿到返回值", tag)
				return
			}
			v, _ := rets[0].Interface().(string)
			if want := "ret:" + tag; v != want {
				mismatch <- fmt.Sprintf("返回值串味: got %q want %q", v, want)
			}
		}(i)
	}
	wg.Wait()
	close(mismatch)
	for msg := range mismatch {
		t.Error(msg)
	}
}

// TestDoAndDoWithRetInterleaveNoCrossTalk Do 不再影响带返回值路径的结果。
func TestDoAndDoWithRetInterleaveNoCrossTalk(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("shared", func(s string) string { return s })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if !c.Do("shared", strconv.Itoa(i)) {
				t.Errorf("Do 应返回 ok")
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			rets, ok := c.DoWithRet("shared", "v"+strconv.Itoa(i))
			if !ok || len(rets) != 1 {
				t.Errorf("DoWithRet 应返回 1 个值")
				return
			}
			if got, _ := rets[0].Interface().(string); got != "v"+strconv.Itoa(i) {
				t.Errorf("DoWithRet 结果被 Do 污染: %q", got)
			}
		}(i)
	}
	wg.Wait()
}

// TestDoOnlyReportsSuccess Do 只表达调用是否成功：回调 panic 时 ok=false，且不保留返回值。
func TestDoOnlyReportsSuccess(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("boom", func() string { panic("业务回调崩溃") })
	if ok := c.Do("boom"); ok {
		t.Fatal("回调 panic 时 Do 应返回 false")
	}

	c2 := &CallFunction{fn: syncmap.NewAny()}
	c2.Register("fine", func() string { return "x" })
	if ok := c2.Do("fine"); !ok {
		t.Fatal("回调正常时 Do 应返回 true")
	}
}

// TestDoWithRetKeepsGroupSemantics 同一 key 多个回调的返回值仍按注册顺序累积（既有契约，不得因删除全局通道而退化）。
func TestDoWithRetKeepsGroupSemantics(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("group", func(i int) int { return i + 1 })
	c.Register("group", func(i int) int { return i + 2 })
	rets, ok := c.DoWithRet("group", 10)
	if !ok || len(rets) != 2 {
		t.Fatalf("应累积 2 个返回值, got %d ok=%v", len(rets), ok)
	}
	if rets[0].Interface() != reflect.ValueOf(11).Interface() {
		t.Fatalf("第一个回调结果错误: %v", rets[0])
	}
	if rets[1].Interface() != reflect.ValueOf(12).Interface() {
		t.Fatalf("第二个回调结果错误: %v", rets[1])
	}
}

func newTestCallFunc() *CallFunction {
	return &CallFunction{fn: syncmap.NewAny()}
}

func TestCallback_UnregisterByIDRemovesExactlyOne(t *testing.T) {
	c := newTestCallFunc()
	var mu sync.Mutex
	hits := map[string]int{}

	id1 := c.Register("k", func() { mu.Lock(); hits["a"]++; mu.Unlock() })
	c.Register("k", func() { mu.Lock(); hits["b"]++; mu.Unlock() })

	if !c.Unregister("k", id1) {
		t.Fatal("按 ID 注销应成功")
	}
	c.Do("k")
	mu.Lock()
	defer mu.Unlock()
	if hits["a"] != 0 || hits["b"] != 1 {
		t.Fatalf("注销后应只剩一个回调: %v", hits)
	}
}

func TestCallback_UnregisterByFuncValue(t *testing.T) {
	c := newTestCallFunc()
	var mu sync.Mutex
	hit := 0
	fn := func() { mu.Lock(); hit++; mu.Unlock() }

	c.Register("k", fn)
	if !c.Unregister("k", fn) {
		t.Fatal("按函数值注销应成功（旧实现 DeepEqual 对 func 恒 false）")
	}
	c.Do("k")
	mu.Lock()
	defer mu.Unlock()
	if hit != 0 {
		t.Fatal("注销后回调不应再执行")
	}
}

func TestCallback_UnregisterConcurrentWithDo(t *testing.T) {
	c := newTestCallFunc()
	var wg sync.WaitGroup
	var counter atomic.Int64

	id := c.Register("k", func() { counter.Add(1) })
	ids := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		ids = append(ids, c.Register("k", func() { counter.Add(1) }))
	}

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Do("k")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Unregister("k", id)
		for _, one := range ids {
			c.Unregister("k", one)
		}
	}()
	wg.Wait()

	if _, has := c.fn.Load("k"); has {
		t.Fatal("全部注销后 key 应被删除")
	}
}

// panic 隔离：一个回调 panic 不应阻止其余回调执行
func TestCallback_PanicIsolation(t *testing.T) {
	c := newTestCallFunc()
	var mu sync.Mutex
	after := 0
	c.Register("k", func() { panic("boom") })
	c.Register("k", func() { mu.Lock(); after++; mu.Unlock() })

	c.Do("k")
	mu.Lock()
	defer mu.Unlock()
	if after != 1 {
		t.Fatal("前一回调 panic 后，后续回调仍应执行")
	}
}

func TestParseFSMessageRejectsUnregistered(t *testing.T) {
	fs := NewFSMessageWithID(91, 92, 1, wrapperspb.String("nope"))
	if fs == nil {
		t.Fatal("pack failed")
	}
	if got := ParseFSMessage(fs); got != nil {
		t.Fatalf("unregistered protocol should be rejected, got %T", got)
	}
}

func TestParseFSMessageRegisteredRoundTrip(t *testing.T) {
	RegisterProtocolType(81, 82, wrapperspb.String(""))
	fs := NewFSMessageWithID(81, 82, 7, wrapperspb.String("hi"))
	got := ParseFSMessage(fs)
	sv, ok := got.(*wrapperspb.StringValue)
	if !ok || sv.GetValue() != "hi" {
		t.Fatalf("got %#v", got)
	}
	if fs.GetHead().GetRequestId() != 7 {
		t.Fatalf("request_id=%d", fs.GetHead().GetRequestId())
	}
}

// 本文件回答两件事：RandInt/RandRange 换成 random 包之后分布还均不均匀，
// 以及换来的锁开销是多少。基准与断言放在一起，是为了让两者在同一次运行里可对照。

// distDraws 是每组检验的取值次数。10 万次对 1000 桶仍是每桶期望 100 次，
// 卡方近似成立；桶数取到 10000 时才会掉到期望 10 次以下，这里不往上加。
const distDraws = 100000

// chiSquare 返回观测频次对「各桶等概率」这一原假设的卡方统计量。
func chiSquare(counts []int) float64 {
	var total float64
	for _, c := range counts {
		total += float64(c)
	}
	expected := total / float64(len(counts))

	var x2 float64
	for _, c := range counts {
		d := float64(c) - expected
		x2 += d * d / expected
	}
	return x2
}

// chiCrit 给出自由度 df 在 α=1e-4 下的临界值（Wilson–Hilferty 近似）。
// 显著性取得比教科书松，是因为断言多、又要跟着每轮提交反复跑；
// 真偏斜时统计量是几万量级，不会因此漏判。
func chiCrit(df int) float64 {
	const z = 3.8906
	k := float64(df)
	t := 1 - 2.0/(9.0*k) + z*math.Sqrt(2.0/(9.0*k))
	return k * t * t * t
}

func accept(t *testing.T, name string, counts []int) {
	t.Helper()

	x2 := chiSquare(counts)
	df := len(counts) - 1
	if crit := chiCrit(df); x2 > crit {
		t.Errorf("%s：卡方 %.1f 超过临界值 %.1f（df=%d, α=1e-4），分布不均匀", name, x2, crit, df)
	} else {
		t.Logf("%s：卡方 %.1f ≤ %.1f（df=%d）通过", name, x2, crit, df)
	}
}

// TestRandInt_ResidueUniform 按调用方的真实用法检验残余分布：
// 对小 n 取模选桶（n=3 与 n=62 分别对应对话文本条数量级与 RandomStr 的字符集大小）。
func TestRandInt_ResidueUniform(t *testing.T) {
	for _, n := range []int{2, 3, 8, 62, 1000} {
		counts := make([]int, n)
		for i := 0; i < distDraws; i++ {
			counts[int(RandInt(int64(n)))]++
		}
		accept(t, fmt.Sprintf("RandInt(%d)", n), counts)
	}
}

// TestRandRange_ResidueUniform 检验带偏移区间的分布：命中应铺满 [3,103) 的每个整数。
func TestRandRange_ResidueUniform(t *testing.T) {
	const (
		lo = int64(3)
		hi = int64(103)
	)
	counts := make([]int, hi-lo)
	for i := 0; i < distDraws; i++ {
		v := RandRange(hi, lo)
		if v < lo || v >= hi {
			t.Fatalf("RandRange(%d,%d) = %d 越界", hi, lo, v)
		}
		counts[v-lo]++
	}
	accept(t, "RandRange(103,3)", counts)
}

// TestRandRange_HugeWidth 钉住换源顺带收掉的宕机面：
// 宽度大到 int64 溢出成负数时，旧实现把它交给 randv2.Int64N 会 panic。
func TestRandRange_HugeWidth(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("RandRange 大区间 panic: %v", r)
		}
	}()

	if got := RandRange(math.MaxInt64, -1); got != -1 {
		t.Errorf("宽度溢出时 RandRange(MaxInt64,-1) = %d，期望按 RandInt 的兜底返回 min=-1", got)
	}
	_ = RandRange(math.MaxInt64, math.MinInt64)
	_ = RandInt(math.MaxInt64)
}

// BenchmarkRandInt 是换源之后的实际开销：每次取值一把全局互斥锁 + 两路引擎。
func BenchmarkRandInt(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = RandInt(1000)
	}
}

// BenchmarkUnderlyingNextInt64 把 RandInt 底下那次取值单独量出来，
// 用同一个时间窗把「取模 + 边界判断」这一层的开销从锁与引擎里剥出去。
func BenchmarkUnderlyingNextInt64(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = random.NextInt64()
	}
}

// BenchmarkRandIntStdlibRef 保留被换掉的 math/rand/v2 路径作为同窗参照：
// 它走 per-P 无锁缓存且用 rejection-free 归约，与生产代码无关，只用于量化换源代价。
func BenchmarkRandIntStdlibRef(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = randv2.Int64N(1000)
	}
}

// TestClip 对外摘要的截断口径：短串原样返回，超过 160 个字符按 rune 截断并追加省略号。
// 按 rune 而不是字节截，避免在多字节字符中间切出乱码。
func TestClip(t *testing.T) {
	if got := Clip("  abc  "); got != "  abc  " {
		t.Errorf("短串被改动: %q", got)
	}
	long := strings.Repeat("错", 200)
	got := Clip(long)
	if r := []rune(got); len(r) != 161 || !strings.HasSuffix(got, "…") {
		t.Errorf("长串截断不符: %d 个字符（期望 160 + 省略号）", len(r))
	}
	if got := Clip(strings.Repeat("a", 160)); len([]rune(got)) != 160 {
		t.Errorf("恰好 160 字符不该被截断: %d", len([]rune(got)))
	}
}

type memVTStore struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

func newMemVTStore() *memVTStore {
	return &memVTStore{data: make(map[string]map[string]string)}
}

func (m *memVTStore) HGetAll(_ context.Context, key string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.data[key]
	if src == nil {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

func (m *memVTStore) HSet(_ context.Context, key string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := m.data[key]
	if dst == nil {
		dst = make(map[string]string)
		m.data[key] = dst
	}
	for k, v := range fields {
		dst[k] = vtFieldString(v)
	}
	return nil
}

func (m *memVTStore) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func vtFieldString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

func TestVirtualTimeRedisKeyIncludesGameGroup(t *testing.T) {
	prev := GameGroup
	GameGroup = "unit"
	t.Cleanup(func() { GameGroup = prev })
	want := VirtualTimeKey + ":unit"
	if VirtualTimeRedisKey() != want {
		t.Fatalf("key=%s want=%s", VirtualTimeRedisKey(), want)
	}
}

func TestResetVirtualTimeUsesGroupKey(t *testing.T) {
	prev := GameGroup
	GameGroup = "g1"
	t.Cleanup(func() {
		GameGroup = prev
		useVirtualTimeBackend(nil)
	})
	store := newMemVTStore()
	useVirtualTimeBackend(store)
	ctx := context.Background()
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now+time.Hour.Nanoseconds()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data[VirtualTimeRedisKey()]; !ok {
		t.Fatal("expected group key written")
	}
	if _, ok := store.data[VirtualTimeKey]; ok {
		t.Fatal("should not write bare VirtualTimeKey")
	}
	if err := ResetVirtualTime(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data[VirtualTimeRedisKey()]; ok {
		t.Fatal("reset should delete group key")
	}
}

func TestCurrentTimeFollowsRedisGroupOffset(t *testing.T) {
	prev := GameGroup
	GameGroup = "shared"
	t.Cleanup(func() {
		GameGroup = prev
		useVirtualTimeBackend(nil)
	})
	store := newMemVTStore()
	useVirtualTimeBackend(store)
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(context.Background(), now, now+2*time.Hour.Nanoseconds()); err != nil {
		t.Fatal(err)
	}
	got := CurrentTime()
	if got.Sub(time.Now()) < time.Hour {
		t.Fatalf("expected ~2h offset, got %v", got)
	}
	if err := ResetVirtualTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	RefreshVirtualTime(context.Background())
	delta := time.Since(CurrentTime())
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Second {
		t.Fatalf("after redis reset, expected real time, drift=%v", delta)
	}
}

func TestGroupServersShareRedisOffset(t *testing.T) {
	prev := GameGroup
	GameGroup = "party"
	t.Cleanup(func() {
		GameGroup = prev
		useVirtualTimeBackend(nil)
	})
	store := newMemVTStore()
	useVirtualTimeBackend(store)
	now := time.Now().UnixNano()
	offset := 90 * time.Minute.Nanoseconds()
	if err := SetVirtualTimeData(context.Background(), now, now+offset); err != nil {
		t.Fatal(err)
	}

	// 模拟同组另一进程：清空本地快照后从同一 Redis key 拉取
	storeSnapshot(nil, false)
	RefreshVirtualTime(context.Background())
	got := CurrentTime()
	if got.Sub(time.Now()) < 60*time.Minute {
		t.Fatalf("peer should see shared redis offset, got %v", got)
	}
}

func TestCurrentTimeWithoutBackendIsRealTime(t *testing.T) {
	useVirtualTimeBackend(nil)
	delta := time.Since(CurrentTime())
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Second {
		t.Fatalf("expected real time, drift=%v", delta)
	}
}

// ============================================================================
// CalculateVirtualTime：新能力（节流）+ 旧行为（回落/故障保留快照）双闸
//
// 2026-11-05 全链路虚拟时间改造引入。这些用例同时是「业务侧把每个响应包络、
// 每条业务时间戳都换成 CalculateVirtualTime」的安全前提：它必须能在热路径上
// 被高频调用而不把 Redis 打爆，同时未配置/未初始化/Redis 故障三条回落路径
// 逐字保持改造前语义。
// ⚠️ 本组用例共享包级全局（vtBackend/vtSnap/vtLastRefresh/GameGroup），
// 一律不得 t.Parallel()，且每个用例必须 Cleanup 还原。
// ============================================================================

// countingVTStore 在 memVTStore 语义上补 HGetAll 次数与可注入的瞬时故障。
type countingVTStore struct {
	mu       sync.Mutex
	fields   map[string]string
	reads    atomic.Int64
	failNext atomic.Bool
}

func newCountingVTStore() *countingVTStore {
	return &countingVTStore{fields: make(map[string]string)}
}

func (c *countingVTStore) HGetAll(_ context.Context, _ string) (map[string]string, error) {
	c.reads.Add(1)
	if c.failNext.Swap(false) {
		return nil, errors.New("injected redis failure")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.fields))
	for k, v := range c.fields {
		out[k] = v
	}
	return out, nil
}

func (c *countingVTStore) HSet(_ context.Context, _ string, fields map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range fields {
		c.fields[k] = vtFieldString(v)
	}
	return nil
}

func (c *countingVTStore) Del(_ context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fields = make(map[string]string)
	return nil
}

func useCountingVT(t *testing.T, store *countingVTStore) {
	t.Helper()
	prevGroup := GameGroup
	GameGroup = "vtcalc"
	t.Cleanup(func() {
		useVirtualTimeBackend(nil)
		SetVirtualTimePollInterval(defaultVTPoll)
		GameGroup = prevGroup
	})
	useVirtualTimeBackend(store)
}

// TestCalculateVirtualTimeFollowsRedisOffset：设置偏移后返回正确虚拟时间。
func TestCalculateVirtualTimeFollowsRedisOffset(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	ctx := context.Background()
	now := time.Now().UnixNano()
	offset := int64(3 * time.Hour)
	if err := SetVirtualTimeData(ctx, now, now+offset); err != nil {
		t.Fatal(err)
	}
	got, err := CalculateVirtualTime(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	delta := got.Sub(time.Now())
	if delta < time.Duration(offset)-time.Minute || delta > time.Duration(offset)+time.Minute {
		t.Fatalf("virtual offset drift=%v want ~%v", delta, time.Duration(offset))
	}
}

// TestCalculateVirtualTimeNotSetReturnsRealTime：未配置偏移必须回真实时间，
// 且错误必须是 ErrVirtualTimeNotSet —— 这是资金语义的判定边界，不得改动。
func TestCalculateVirtualTimeNotSetReturnsRealTime(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	got, err := CalculateVirtualTime(context.Background())
	if !errors.Is(err, ErrVirtualTimeNotSet) {
		t.Fatalf("err=%v want ErrVirtualTimeNotSet", err)
	}
	delta := time.Since(got)
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Second {
		t.Fatalf("expected real time on missing offset, drift=%v", delta)
	}
}

// TestCalculateVirtualTimeWithoutBackendReturnsError：redis 未初始化时返回
// 「redis client not initialized」，且**不是** ErrVirtualTimeNotSet（两者语义不同）。
func TestCalculateVirtualTimeWithoutBackendReturnsError(t *testing.T) {
	t.Cleanup(func() { useVirtualTimeBackend(nil) })
	useVirtualTimeBackend(nil)
	got, err := CalculateVirtualTime(context.Background())
	if err == nil {
		t.Fatal("expected error when redis client is not initialized")
	}
	if errors.Is(err, ErrVirtualTimeNotSet) {
		t.Fatal("must not be ErrVirtualTimeNotSet when backend is nil")
	}
	delta := time.Since(got)
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Second {
		t.Fatalf("expected real time fallback, drift=%v", delta)
	}
}

// TestVirtualTimeDataIsNanosecondsNotMilliseconds：钉死字段单位。曾把注释写成
// 「Unix 毫秒」，按那个口径传值会算出 1970 年的时间；实现与调用方都是纳秒。
func TestVirtualTimeDataIsNanosecondsNotMilliseconds(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	ctx := context.Background()
	nowNs := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, nowNs, nowNs); err != nil {
		t.Fatal(err)
	}
	got, err := CalculateVirtualTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() < 2000 {
		t.Fatalf("got %v — offset looks like epoch, units are wrong", got)
	}
	delta := time.Since(got)
	if delta < 0 {
		delta = -delta
	}
	if delta > 2*time.Second {
		t.Fatalf("zero nanosecond offset should track real time, drift=%v", delta)
	}
}

// TestCalculateVirtualTimeThrottlesRedisReads：热路径节流——200 次调用不得
// 触发 200 次 HGetAll。这是把 nowMs()/业务时间戳换成 CalculateVirtualTime 的前提。
func TestCalculateVirtualTimeThrottlesRedisReads(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	SetVirtualTimePollInterval(time.Hour)
	ctx := context.Background()
	// 先落一份零偏移：未配置时 CalculateVirtualTime 会带 ErrVirtualTimeNotSet，
	// 本用例只数 Redis 读取次数，不该被「未设置」这条正常回落分支干扰。
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now); err != nil {
		t.Fatal(err)
	}
	before := store.reads.Load()
	for i := 0; i < 200; i++ {
		if _, err := CalculateVirtualTime(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.reads.Load() - before; got > 2 {
		t.Fatalf("200 CalculateVirtualTime calls hit redis %d times, want <=2", got)
	}
}

// TestCalculateVirtualTimeRefreshesWhenSnapshotStale：快照过期时仍须强刷一次，
// 节流不能把「组内别的进程改了偏移」这一路径彻底堵死。
func TestCalculateVirtualTimeRefreshesWhenSnapshotStale(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	SetVirtualTimePollInterval(time.Hour)
	ctx := context.Background()
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now); err != nil {
		t.Fatal(err)
	}
	vtLastRefresh.Store(0) // 强制判定为过期
	before := store.reads.Load()
	if _, err := CalculateVirtualTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.reads.Load() - before; got != 1 {
		t.Fatalf("stale snapshot should trigger exactly 1 redis read, got %d", got)
	}
}

// TestSetVirtualTimeThenCalculateAvoidsExtraRedisRead：SetVirtualTimeData 已
// 写入本地快照并 Publish，紧接着的 CalculateVirtualTime 不该再打一次 Redis。
func TestSetVirtualTimeThenCalculateAvoidsExtraRedisRead(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	SetVirtualTimePollInterval(time.Hour)
	ctx := context.Background()
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now+int64(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := store.reads.Load()
	if _, err := CalculateVirtualTime(ctx); err != nil {
		t.Fatal(err)
	}
	if got := store.reads.Load() - before; got != 0 {
		t.Fatalf("Set already pushed snapshot, Calculate re-read redis %d times", got)
	}
}

// TestRefreshVirtualTimeKeepsSnapshotOnRedisError：旧行为不回归——Redis 瞬时
// 故障时必须保留上一份快照，否则同组一部分节点会突然跳回真实时间。
func TestRefreshVirtualTimeKeepsSnapshotOnRedisError(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	ctx := context.Background()
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now+int64(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store.failNext.Store(true)
	RefreshVirtualTime(ctx) // 显式强刷失败
	if got := CurrentTime(); got.Sub(time.Now()) < time.Hour {
		t.Fatalf("snapshot lost on transient redis failure, got %v", got)
	}
	// 失败不推进刷新时刻：后续调用仍会重试，不会永久卡在旧快照上。
	store.failNext.Store(false)
	vtLastRefresh.Store(0)
	before := store.reads.Load()
	if _, err := CalculateVirtualTime(ctx); err != nil {
		t.Fatal(err)
	}
	if got := store.reads.Load() - before; got != 1 {
		t.Fatalf("after failure, expected 1 retry read, got %d", got)
	}
}

// TestCalculateVirtualTimeConcurrent：-race 下并发调用不得有数据竞争，
// 且惊群被 vtRefreshMu 收口后 Redis 读取次数保持有界。
func TestCalculateVirtualTimeConcurrent(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	SetVirtualTimePollInterval(time.Millisecond)
	ctx := context.Background()
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now+int64(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := store.reads.Load()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := CalculateVirtualTime(ctx); err != nil {
					t.Errorf("concurrent call: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := store.reads.Load() - before; got > 1000 {
		t.Fatalf("1000 concurrent calls hit redis %d times — thundering herd", got)
	}
}

// TestSetVirtualTimeUsesNanoseconds：类型安全入口必须把 time.Time 正确映射到
// 纳秒偏移，调用方永远不该碰 int64 单位。
func TestSetVirtualTimeUsesNanoseconds(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	ctx := context.Background()
	target := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	if err := SetVirtualTime(ctx, target); err != nil {
		t.Fatal(err)
	}
	got, err := CalculateVirtualTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 允许几秒误差：target 截断到秒，且中间有真实时间流逝。
	if diff := got.Sub(target); diff < -5*time.Second || diff > 5*time.Second {
		t.Fatalf("SetVirtualTime target=%v got=%v diff=%v", target, got, diff)
	}
	if got.Year() < 2000 {
		t.Fatalf("got %v — nanoseconds/unit mismatch", got)
	}
}

// TestGetVirtualTimeState：未配置是配置态（Enabled=false, err=nil）；
// 配置后能还原出 time.Time 视图与 Redis key。
func TestGetVirtualTimeState(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	ctx := context.Background()

	state, err := GetVirtualTime(ctx)
	if err != nil {
		t.Fatalf("unconfigured must not be an error: %v", err)
	}
	if state.Enabled {
		t.Fatal("fresh store must report Enabled=false")
	}
	if state.RedisKey != VirtualTimeRedisKey() {
		t.Fatalf("redis key=%q want %q", state.RedisKey, VirtualTimeRedisKey())
	}

	target := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	if err := SetVirtualTime(ctx, target); err != nil {
		t.Fatal(err)
	}
	state, err = GetVirtualTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled {
		t.Fatal("after SetVirtualTime, Enabled must be true")
	}
	if diff := state.Anchor.Sub(target); diff < -time.Second || diff > time.Second {
		t.Fatalf("anchor=%v want ~%v", state.Anchor, target)
	}
	if state.AnchorReal.IsZero() {
		t.Fatal("AnchorReal must be populated")
	}
}

// TestRealTimeIgnoresVirtualOffset：基础设施时钟红线——虚拟时间 ±2h，
// RealTime/RealMS 必须仍贴近真实时间，否则 deadline、计时轮、time.Since 全乱。
func TestRealTimeIgnoresVirtualOffset(t *testing.T) {
	store := newCountingVTStore()
	useCountingVT(t, store)
	ctx := context.Background()
	if err := SetVirtualTime(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d := RealTime().Sub(time.Now()); d < -time.Second || d > time.Second {
		t.Fatalf("RealTime drifted by %v under virtual offset", d)
	}
	if d := RealMS() - time.Now().UnixMilli(); d < -1000 || d > 1000 {
		t.Fatalf("RealMS drifted by %dms under virtual offset", d)
	}
	if CurrentTime().Sub(time.Now()) < time.Hour {
		t.Fatal("CurrentTime should still follow the virtual offset")
	}
}
