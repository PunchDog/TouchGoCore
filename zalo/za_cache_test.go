package zalo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件只测 za_cache.go 的缓存/单飞/限频原语，全部使用假时钟与假 fetch，
// 不连网；TTL 与间隔断言只依赖 clock.Advance，不读真实时间。

var errCacheBoom = errors.New("cache fetch boom")

// cacheClock 是可控假时钟：Now 返回内部时间，Sleep 只记录时长并立即返回。
type cacheClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

// newCacheClock 从固定时刻起步，保证测试可复现。
func newCacheClock() *cacheClock {
	return &cacheClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now 返回内部时间。
func (c *cacheClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep 记录本次睡眠时长，不真正阻塞。
func (c *cacheClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return nil
}

// Advance 推进内部时间。
func (c *cacheClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Sleeps 返回已记录的睡眠时长副本。
func (c *cacheClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// cacheBarrierClock 在 Now 被调用 target 次后关闭 all，用于确定性地拉起并发等待者，
// 避免用真实时间猜调度。Sleep 不会被单飞测试触发。
type cacheBarrierClock struct {
	nowMu sync.Mutex
	now   time.Time

	mu     sync.Mutex
	calls  int
	target int
	once   sync.Once
	all    chan struct{}
}

// newCacheBarrierClock 构造 target 次 Now 即放行的假时钟。
func newCacheBarrierClock(target int) *cacheBarrierClock {
	return &cacheBarrierClock{
		now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		target: target,
		all:    make(chan struct{}),
	}
}

// Now 返回内部时间，并在达到 target 次调用时放行。
func (c *cacheBarrierClock) Now() time.Time {
	c.nowMu.Lock()
	t := c.now
	c.nowMu.Unlock()

	c.mu.Lock()
	c.calls++
	reached := c.calls >= c.target
	c.mu.Unlock()
	if reached {
		c.once.Do(func() { close(c.all) })
	}
	return t
}

// Sleep 只返回 nil：本类型只服务于不需要等闸的用例。
func (c *cacheBarrierClock) Sleep(ctx context.Context, d time.Duration) error { return nil }

// TestZACacheFirstFetchAndTTLHit 覆盖首次拉取、TTL 内命中不拉取、返回值与缓存隔离。
func TestZACacheFirstFetchAndTTLHit(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: "u1", DisplayName: "Alice"}}, nil
	}

	var c zaCache[UserInfo]
	got, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u1" || got[0].DisplayName != "Alice" {
		t.Fatalf("首次 List 结果不符: %+v", got)
	}

	// 调用方改返回值不得污染缓存。
	got[0].UserID = "mutated"
	clock.Advance(4 * time.Minute)
	hit, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("TTL 内 List 失败: %v", err)
	}
	if len(hit) != 1 || hit[0].UserID != "u1" {
		t.Fatalf("TTL 内命中被返回值污染: %+v", hit)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("TTL 内命中不应再 fetch，实际 fetch %d 次", n)
	}
}

// TestZACacheTTLExpiryRefetch 覆盖 TTL 过期后重新拉取。
func TestZACacheTTLExpiryRefetch(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		n := atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: fmt.Sprintf("u%d", n)}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	clock.Advance(6 * time.Minute)
	got, err := c.List(context.Background(), 5*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("过期后 List 失败: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u2" {
		t.Fatalf("过期后应拿到新数据，实际 %+v", got)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望 fetch 2 次，实际 %d 次", n)
	}
}

// TestZACacheMinIntervalGate 覆盖过期 List 与 Refresh 在 minInterval 内都被拒绝，
// 且冷却结束后恢复可拉取。
func TestZACacheMinIntervalGate(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}

	clock.Advance(2 * time.Minute) // 缓存已过期，但距上次拉取只有 2 分钟
	if _, err := c.List(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("过期 List 应被 minInterval 拒绝，实际 err=%v", err)
	}
	if _, err := c.Refresh(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("Refresh 应被 minInterval 拒绝，实际 err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("被拒绝期间不得 fetch，实际 %d 次", n)
	}

	clock.Advance(3 * time.Minute) // 累计 5 分钟，闸放开
	if _, err := c.List(context.Background(), time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("冷却结束后 List 应成功: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望 fetch 2 次，实际 %d 次", n)
	}
}

// TestZACacheRefreshBypassesCache 覆盖 Refresh 跳过 TTL 命中判断。
func TestZACacheRefreshBypassesCache(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		n := atomic.AddInt32(&calls, 1)
		return []UserInfo{{UserID: fmt.Sprintf("u%d", n)}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	refreshed, err := c.Refresh(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if len(refreshed) != 1 || refreshed[0].UserID != "u2" {
		t.Fatalf("Refresh 应绕过缓存拿到新数据，实际 %+v", refreshed)
	}
	hit, err := c.List(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("Refresh 后 List 失败: %v", err)
	}
	if len(hit) != 1 || hit[0].UserID != "u2" {
		t.Fatalf("Refresh 后 List 应命中新缓存，实际 %+v", hit)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望 fetch 2 次，实际 %d 次", n)
	}
}

// TestZACacheConcurrentSingleFlight 覆盖 20 个并发调用合并为一次 fetch。
// barrier clock 保证所有调用都已进入 List 后才放行 leader 的 fetch。
func TestZACacheConcurrentSingleFlight(t *testing.T) {
	const callers = 20
	clock := newCacheBarrierClock(callers)
	release := make(chan struct{})
	var fetchCalls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&fetchCalls, 1)
		<-release
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	results := make(chan []UserInfo, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.List(context.Background(), 5*time.Minute, 0, clock, fetch, cloneUserInfos)
			errs <- err
			results <- got
		}()
	}

	select {
	case <-clock.all:
	case <-time.After(10 * time.Second):
		t.Fatal("等待 20 个调用进入 List 超时")
	}
	close(release)
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		if err != nil {
			t.Fatalf("并发 List 返回错误: %v", err)
		}
	}
	for got := range results {
		if len(got) != 1 || got[0].UserID != "u1" {
			t.Fatalf("并发 List 结果不符: %+v", got)
		}
	}
	if n := atomic.LoadInt32(&fetchCalls); n != 1 {
		t.Fatalf("20 并发应合并为 1 次 fetch，实际 %d 次", n)
	}
}

// TestZACacheFetchFailureKeepsCache 覆盖失败保留旧缓存、失败写 lastAttempt 后的冷却。
func TestZACacheFetchFailureKeepsCache(t *testing.T) {
	clock := newCacheClock()
	var calls int32
	fail := false
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&calls, 1)
		if fail {
			return nil, errCacheBoom
		}
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}

	clock.Advance(6 * time.Minute) // 超过 minInterval，Refresh 可真正发起
	fail = true
	if _, err := c.Refresh(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, errCacheBoom) {
		t.Fatalf("Refresh 失败应返回原错误，实际 %v", err)
	}
	// 旧缓存仍在且仍在 TTL 内：List 直接返回它，不再打接口。
	got, err := c.List(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos)
	if err != nil {
		t.Fatalf("失败后 List 应返回旧缓存: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u1" {
		t.Fatalf("失败后旧缓存丢失: %+v", got)
	}
	// 失败也写了 lastAttempt：立刻再 Refresh 必须被冷却闸拒绝。
	if _, err := c.Refresh(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); !errors.Is(err, ErrZaloFetchTooSoon) {
		t.Fatalf("失败后冷却应返回 ErrZaloFetchTooSoon，实际 %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("冷却期间不得 fetch，实际 %d 次", n)
	}

	clock.Advance(5 * time.Minute)
	fail = false
	if _, err := c.Refresh(context.Background(), 10*time.Minute, 5*time.Minute, clock, fetch, cloneUserInfos); err != nil {
		t.Fatalf("冷却结束后 Refresh 应成功: %v", err)
	}
}

// TestZACacheWaiterContextCancel 覆盖等待者 ctx 取消只影响自己，leader 继续完成。
func TestZACacheWaiterContextCancel(t *testing.T) {
	clock := newCacheClock()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var fetchCalls int32
	fetch := func(context.Context) ([]UserInfo, error) {
		atomic.AddInt32(&fetchCalls, 1)
		once.Do(func() { close(started) })
		<-release
		return []UserInfo{{UserID: "u1"}}, nil
	}

	var c zaCache[UserInfo]
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.List(context.Background(), time.Minute, 0, clock, fetch, cloneUserInfos)
		leaderDone <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := c.List(ctx, time.Minute, 0, clock, fetch, cloneUserInfos)
		waiterDone <- err
	}()
	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("等待者应返回 context.Canceled，实际 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("等待者未在 ctx 取消后返回")
	}

	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader 不应被等待者取消影响: %v", err)
	}
	if n := atomic.LoadInt32(&fetchCalls); n != 1 {
		t.Fatalf("期望 fetch 1 次，实际 %d 次", n)
	}
}

// TestZACacheRequiresClone 覆盖 clone/fetch 缺失时的编程错误。
func TestZACacheRequiresClone(t *testing.T) {
	var c zaCache[UserInfo]
	if _, err := c.List(context.Background(), time.Minute, 0, newCacheClock(), nil, cloneUserInfos); !errors.Is(err, errZACacheNoFetch) {
		t.Fatalf("缺 fetch 应报错，实际 %v", err)
	}
	fetch := func(context.Context) ([]UserInfo, error) { return nil, nil }
	if _, err := c.List(context.Background(), time.Minute, 0, newCacheClock(), fetch, nil); !errors.Is(err, errZACacheNoClone) {
		t.Fatalf("缺 clone 应报错，实际 %v", err)
	}
}

// infoFetchRecorder 记录用户/群资料请求的批次。
type infoFetchRecorder struct {
	mu      sync.Mutex
	batches [][]string
}

// record 保存一批请求 ID 的副本。
func (r *infoFetchRecorder) record(ids []string) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]string(nil), ids...))
	r.mu.Unlock()
}

// Batches 返回已记录批次的副本。
func (r *infoFetchRecorder) Batches() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.batches))
	for i, b := range r.batches {
		out[i] = append([]string(nil), b...)
	}
	return out
}

// Calls 返回已记录的请求次数。
func (r *infoFetchRecorder) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

// infoUsersFor 为每个 ID 造一条用户资料。
func infoUsersFor(ids []string) []UserInfo {
	out := make([]UserInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, UserInfo{UserID: id, DisplayName: "dn-" + id})
	}
	return out
}

// infoGroupsFor 为每个 ID 造一条群资料。
func infoGroupsFor(ids []string) []GroupInfo {
	out := make([]GroupInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, GroupInfo{GroupID: id, Name: "gn-" + id})
	}
	return out
}

// TestZAInfoCacheBatchesAndThrottle 覆盖 120 个 ID 分 50/50/20 三批、批间 1.5 秒、
// 命中批次不再等闸。
func TestZAInfoCacheBatchesAndThrottle(t *testing.T) {
	clock := newCacheClock()
	ids := make([]string, 120)
	for i := range ids {
		ids[i] = fmt.Sprintf("u%03d", i)
	}
	rec := &infoFetchRecorder{}
	fetch := func(_ context.Context, batch []string) ([]UserInfo, error) {
		rec.record(batch)
		return infoUsersFor(batch), nil
	}

	c := newZAInfoCache()
	got, err := c.Users(context.Background(), ids, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("Users 失败: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("应返回 120 条资料，实际 %d", len(got))
	}

	batches := rec.Batches()
	if len(batches) != 3 {
		t.Fatalf("应分 3 批请求，实际 %d 批: %v", len(batches), batches)
	}
	for i, want := range []int{50, 50, 20} {
		if len(batches[i]) != want {
			t.Fatalf("第 %d 批应为 %d 个 ID，实际 %d", i+1, want, len(batches[i]))
		}
	}

	sleeps := clock.Sleeps()
	if len(sleeps) != 2 {
		t.Fatalf("批间应等闸 2 次，实际 %d 次: %v", len(sleeps), sleeps)
	}
	for i, d := range sleeps {
		if d != 1500*time.Millisecond {
			t.Fatalf("第 %d 次等闸应为 1.5s，实际 %s", i+1, d)
		}
	}

	// 全部命中：不请求、不等闸。
	again, err := c.Users(context.Background(), ids, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("缓存命中 Users 失败: %v", err)
	}
	if len(again) != 120 {
		t.Fatalf("缓存命中应返回 120 条，实际 %d", len(again))
	}
	if rec.Calls() != 3 {
		t.Fatalf("缓存命中不应再请求，实际共 %d 批", rec.Calls())
	}
	if extra := clock.Sleeps(); len(extra) != 2 {
		t.Fatalf("缓存命中不应等闸，实际睡眠记录 %v", extra)
	}
}

// TestZAInfoCacheUsersGroupsSharedThrottle 覆盖 users 与 groups 共用同一把 throttle。
func TestZAInfoCacheUsersGroupsSharedThrottle(t *testing.T) {
	clock := newCacheClock()
	var userCalls, groupCalls int32
	users := func(_ context.Context, batch []string) ([]UserInfo, error) {
		atomic.AddInt32(&userCalls, 1)
		return infoUsersFor(batch), nil
	}
	groups := func(_ context.Context, batch []string) ([]GroupInfo, error) {
		atomic.AddInt32(&groupCalls, 1)
		return infoGroupsFor(batch), nil
	}

	c := newZAInfoCache()
	if _, err := c.Users(context.Background(), []string{"u1"}, 10*time.Minute, 1500*time.Millisecond, clock, users); err != nil {
		t.Fatalf("Users 失败: %v", err)
	}
	if sleeps := clock.Sleeps(); len(sleeps) != 0 {
		t.Fatalf("首个请求不应等闸，实际 %v", sleeps)
	}

	// 紧接着的群资料请求必须等同一个闸。
	if _, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, groups); err != nil {
		t.Fatalf("Groups 失败: %v", err)
	}
	sleeps := clock.Sleeps()
	if len(sleeps) != 1 || sleeps[0] != 1500*time.Millisecond {
		t.Fatalf("群资料应等闸 1.5s，实际 %v", sleeps)
	}

	// 两边都命中缓存：不再请求、不再等闸。
	if _, err := c.Users(context.Background(), []string{"u1"}, 10*time.Minute, 1500*time.Millisecond, clock, users); err != nil {
		t.Fatalf("Users 命中失败: %v", err)
	}
	if _, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, groups); err != nil {
		t.Fatalf("Groups 命中失败: %v", err)
	}
	if extra := clock.Sleeps(); len(extra) != 1 {
		t.Fatalf("缓存命中不应等闸，实际 %v", extra)
	}
	if n := atomic.LoadInt32(&userCalls); n != 1 {
		t.Fatalf("Users 应只请求 1 次，实际 %d", n)
	}
	if n := atomic.LoadInt32(&groupCalls); n != 1 {
		t.Fatalf("Groups 应只请求 1 次，实际 %d", n)
	}
}

// TestZAInfoCacheMissingIDs 覆盖缺失 ID 只请求一次、返回条目不伪造、缺失项不缓存。
func TestZAInfoCacheMissingIDs(t *testing.T) {
	clock := newCacheClock()
	rec := &infoFetchRecorder{}
	stage := "a"
	fetch := func(_ context.Context, batch []string) ([]UserInfo, error) {
		rec.record(batch)
		if stage == "a" {
			return []UserInfo{{UserID: "ua"}}, nil
		}
		return []UserInfo{{UserID: "ub"}}, nil
	}

	c := newZAInfoCache()
	got, err := c.Users(context.Background(), []string{"ua", "ub"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("首次 Users 失败: %v", err)
	}
	if len(got) != 1 || got["ua"].UserID != "ua" {
		t.Fatalf("只应返回服务端存在的 ua，实际 %+v", got)
	}
	if _, ok := got["ub"]; ok {
		t.Fatal("服务端没返回的 ub 不得伪造")
	}

	clock.Advance(1500 * time.Millisecond)
	stage = "b"
	got, err = c.Users(context.Background(), []string{"ua", "ub"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("第二次 Users 失败: %v", err)
	}
	if len(got) != 2 || got["ua"].UserID != "ua" || got["ub"].UserID != "ub" {
		t.Fatalf("第二次应返回 ua+ub，实际 %+v", got)
	}
	batches := rec.Batches()
	if len(batches) != 2 {
		t.Fatalf("应请求 2 批，实际 %d", len(batches))
	}
	if want := []string{"ub"}; !reflect.DeepEqual(batches[1], want) {
		t.Fatalf("第二批只应请求缺失的 ub，实际 %v", batches[1])
	}

	// 两个 ID 都已缓存：不再请求。
	if _, err := c.Users(context.Background(), []string{"ua", "ub"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch); err != nil {
		t.Fatalf("第三次 Users 失败: %v", err)
	}
	if rec.Calls() != 2 {
		t.Fatalf("全命中不应再请求，实际 %d 批", rec.Calls())
	}
}

// TestZAInfoCacheGroupsDeletion 覆盖服务端没返回的群 ID 从缓存删除、全缺失返回空 map。
func TestZAInfoCacheGroupsDeletion(t *testing.T) {
	clock := newCacheClock()
	c := newZAInfoCache()
	c.storeGroups([]string{"g1"}, []GroupInfo{{GroupID: "g1", Name: "old"}}, clock.Now())

	clock.Advance(11 * time.Minute) // 缓存已过期
	var requested [][]string
	fetch := func(_ context.Context, batch []string) ([]GroupInfo, error) {
		requested = append(requested, append([]string(nil), batch...))
		return nil, nil
	}

	got, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("Groups 失败: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("全缺失应返回空 map，实际 %#v", got)
	}
	if len(requested) != 1 || !reflect.DeepEqual(requested[0], []string{"g1"}) {
		t.Fatalf("应请求过期群 g1，实际 %v", requested)
	}
	if _, ok := c.CachedGroup("g1", 10*time.Minute, clock.Now()); ok {
		t.Fatal("服务端没返回的群 ID 应从缓存删除")
	}
}

// TestZAInfoCacheCachedDrop 覆盖 CachedGroup/DropGroups、TTL 与深拷贝。
func TestZAInfoCacheCachedDrop(t *testing.T) {
	clock := newCacheClock()
	now := clock.Now()
	c := newZAInfoCache()
	// 直接写入缓存（与 Groups 成功后的写入路径一致）。
	c.storeGroups([]string{"g1"}, []GroupInfo{{GroupID: "g1", Name: "G1", AdminIDs: []string{"a1", "a2"}}}, now)

	g, ok := c.CachedGroup("g1", 10*time.Minute, now)
	if !ok || g.Name != "G1" || !reflect.DeepEqual(g.AdminIDs, []string{"a1", "a2"}) {
		t.Fatalf("写入缓存后应能读到 g1，实际 ok=%v %+v", ok, g)
	}
	g.Name = "mutated"
	g.AdminIDs[0] = "hacked"
	again, ok := c.CachedGroup("g1", 10*time.Minute, now)
	if !ok || again.Name != "G1" || again.AdminIDs[0] != "a1" {
		t.Fatalf("CachedGroup 必须深拷贝，实际 %+v", again)
	}

	if _, ok := c.CachedGroup("g1", 10*time.Minute, now.Add(10*time.Minute)); ok {
		t.Fatal("TTL 到期应返回 false")
	}
	if _, ok := c.CachedGroup("g1", 11*time.Minute, now.Add(10*time.Minute)); !ok {
		t.Fatal("TTL 内应返回 true")
	}
	if _, ok := c.CachedGroup("", 10*time.Minute, now); ok {
		t.Fatal("空 ID 不应命中")
	}

	c.DropGroups([]string{"g1", "", "unknown"})
	if _, ok := c.CachedGroup("g1", 11*time.Minute, now); ok {
		t.Fatal("DropGroups 后不应再命中")
	}
}

// TestZAInfoCacheGroupsResultDeepCopy 覆盖 Groups 返回的群资料与缓存隔离。
func TestZAInfoCacheGroupsResultDeepCopy(t *testing.T) {
	clock := newCacheClock()
	c := newZAInfoCache()
	fetch := func(_ context.Context, batch []string) ([]GroupInfo, error) {
		out := infoGroupsFor(batch)
		for i := range out {
			out[i].AdminIDs = []string{"admin-" + out[i].GroupID}
		}
		return out, nil
	}
	got, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 1500*time.Millisecond, clock, fetch)
	if err != nil {
		t.Fatalf("Groups 失败: %v", err)
	}
	g := got["g1"]
	g.AdminIDs[0] = "hacked"
	cached, ok := c.CachedGroup("g1", 10*time.Minute, clock.Now())
	if !ok || cached.AdminIDs[0] != "admin-g1" {
		t.Fatalf("Groups 返回值不得污染缓存，实际 %+v", cached)
	}
}

// TestZAInfoCacheFetchError 覆盖 user/group 拉取错误原样上抛。
func TestZAInfoCacheFetchError(t *testing.T) {
	clock := newCacheClock()
	c := newZAInfoCache()
	users := func(context.Context, []string) ([]UserInfo, error) { return nil, errCacheBoom }
	if _, err := c.Users(context.Background(), []string{"u1"}, 10*time.Minute, 0, clock, users); !errors.Is(err, errCacheBoom) {
		t.Fatalf("Users 应返回 fetch 错误，实际 %v", err)
	}
	groups := func(context.Context, []string) ([]GroupInfo, error) { return nil, errCacheBoom }
	if _, err := c.Groups(context.Background(), []string{"g1"}, 10*time.Minute, 0, clock, groups); !errors.Is(err, errCacheBoom) {
		t.Fatalf("Groups 应返回 fetch 错误，实际 %v", err)
	}
}

// TestZAInfoCacheCloneHelpers 覆盖深拷贝辅助函数与 ID 归一化。
func TestZAInfoCacheCloneHelpers(t *testing.T) {
	if got, want := uniqueNonEmptyIDs([]string{"a", "", "b", "a", "", "c"}), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("uniqueNonEmptyIDs 不符: got %v want %v", got, want)
	}
	if got := uniqueNonEmptyIDs(nil); len(got) != 0 {
		t.Fatalf("nil 输入应返回空切片，实际 %v", got)
	}

	if cloneUserInfos(nil) != nil {
		t.Fatal("cloneUserInfos(nil) 应为 nil")
	}
	srcUsers := []UserInfo{{UserID: "u1"}}
	clonedUsers := cloneUserInfos(srcUsers)
	clonedUsers[0].UserID = "mutated"
	if srcUsers[0].UserID != "u1" {
		t.Fatal("cloneUserInfos 必须复制底层数组")
	}

	if cloneGroupInfos(nil) != nil {
		t.Fatal("cloneGroupInfos(nil) 应为 nil")
	}
	srcGroups := []GroupInfo{
		{GroupID: "g1", AdminIDs: []string{"a1"}},
		{GroupID: "g2"},
	}
	clonedGroups := cloneGroupInfos(srcGroups)
	clonedGroups[0].AdminIDs[0] = "hacked"
	clonedGroups[0].GroupID = "mutated"
	clonedGroups[1].GroupID = "mutated"
	if srcGroups[0].AdminIDs[0] != "a1" || srcGroups[0].GroupID != "g1" || srcGroups[1].GroupID != "g2" {
		t.Fatalf("cloneGroupInfos 必须深拷贝，原值被改动: %+v", srcGroups)
	}

	noAdmins := cloneGroupInfo(GroupInfo{GroupID: "g3"})
	if noAdmins.AdminIDs != nil {
		t.Fatal("nil AdminIDs 应保持 nil")
	}
}
