package util

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
