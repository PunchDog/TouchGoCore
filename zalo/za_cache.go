package zalo

// 本文件是 Zalo 通道的两层缓存/限频原语，不依赖 manager/facade/存储：
//
//   - zaCache[T]：好友列表（UserInfo）与群列表（GroupInfo）的全量缓存。TTL 内命中直接
//     返回；过期时同一键只允许一个 leader 打接口（单飞），其余调用方等结果；任何一次
//     真实拉取（无论成败）都记 lastAttempt，minInterval 内再次刷新返回 ErrZaloFetchTooSoon，
//     而不是睡 5 分钟或连续打接口。List 看缓存，Refresh 跳过缓存但仍受间隔闸。
//   - zaInfoCache：按 ID 缓存的用户/群资料。TTL 由调用方传入（生产 10 分钟）；所有
//     user/group 资料请求共用一把 1.5 秒的 throttle 闸，缓存命中路径完全不经过该闸，
//     不会因为别人在等闸而被拖慢。缺失 ID 去重后按 50 个一批，每批重新检查缓存、
//     过闸后再请求；服务端没返回的 ID 从缓存删除，不伪造空壳。
//
// 并发口径：缓存数据只替换、不原地修改，因此 clone 可以在锁外对快照执行；throttle
// 用独立 Mutex 且允许持锁睡眠（睡眠本身就是闸），与缓存 RWMutex 互不嵌套。
//
// 错误口径：限频拒绝一律 %w 包装 ErrZaloFetchTooSoon，文案只含剩余等待时间与规则，
// 不含任何凭证、cookie、IMEI。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// zaInfoBatchSize 是用户/群资料单次请求的 ID 上限，与 zcago 的批量接口口径一致。
const zaInfoBatchSize = 50

var (
	// errZACacheNoClone 表示调用方没有提供 clone 函数：没有它就无法保证返回给
	// 调用方的切片与缓存不共享底层数组，属于编程错误，直接拒绝。
	errZACacheNoClone = errors.New("zalo 缓存缺少 clone 函数")
	// errZACacheNoFetch 表示调用方没有提供拉取函数。
	errZACacheNoFetch = errors.New("zalo 缓存缺少 fetch 函数")
	// errZACacheFetchPanic 只发给单飞的等待者：leader 的 panic 仍会原样抛给 leader
	// 的调用方，但等待者不能因此永久阻塞在 flight 上。
	errZACacheFetchPanic = errors.New("zalo 缓存拉取发生 panic")
)

// zaClock 是缓存层唯一的时间来源，测试用假时钟替换真实等待。
type zaClock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// realZAClock 是生产时钟：Now 取系统时间，Sleep 用可取消的定时器。
type realZAClock struct{}

// Now 返回当前系统时间。
func (realZAClock) Now() time.Time { return time.Now() }

// Sleep 等待 d 或 ctx 取消，二者先到先返回；d<=0 不等直接返回 nil。
// 不用 time.Sleep：throttle 等待必须能被调用方 ctx 取消。
func (realZAClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// zaFlight 是一次进行中的拉取。done 关闭前 items/err 不可读；关闭后由 channel
// 的 happens-before 保证安全，等待者无需再抢缓存锁。
type zaFlight[T any] struct {
	done  chan struct{}
	items []T
	err   error
}

// zaCache 是一份带 TTL、最小间隔与单飞的全量列表缓存。
//
// 字段全部由 mu 保护；items 一旦写入就不再原地修改（替换语义），所以快照可以在
// 锁外 clone。零值可用：首次 List 即拉取。
type zaCache[T any] struct {
	mu          sync.Mutex
	items       []T
	ok          bool
	fetchedAt   time.Time
	lastAttempt time.Time
	flight      *zaFlight[T]
}

// List 返回缓存列表；缓存过期或缺失时按单飞拉取。
//
// 命中条件为 ok && now-fetchedAt < ttl，命中不修改 lastAttempt。未命中时：
//   - 已有 flight 则等待它（调用方 ctx 取消只影响自己）；
//   - 否则 lastAttempt 距 now 不足 minInterval 直接返回 ErrZaloFetchTooSoon；
//   - 否则本调用成为 leader，锁内记 lastAttempt、建 flight，锁外执行 fetch。
//
// 成功更新缓存；失败保留旧缓存并返回错误。fetch 的结果先 clone 一份入缓存，
// 返回给调用方的切片始终不与缓存共享底层数组。clone 还必须深拷贝元素里的引用
// 字段（群资料的 AdminIDs），否则调用方改自己的返回值仍会串改缓存。
func (c *zaCache[T]) List(ctx context.Context, ttl, minInterval time.Duration, clock zaClock, fetch func(context.Context) ([]T, error), clone func([]T) []T) ([]T, error) {
	return c.load(ctx, false, ttl, minInterval, clock, fetch, clone)
}

// Refresh 跳过缓存命中判断强制拉取，但仍受 lastAttempt 的 minInterval 闸约束，
// 供 ZaloSyncFriends/ZaloSyncGroups 使用：5 分钟内重复 Sync 返回 ErrZaloFetchTooSoon。
func (c *zaCache[T]) Refresh(ctx context.Context, ttl, minInterval time.Duration, clock zaClock, fetch func(context.Context) ([]T, error), clone func([]T) []T) ([]T, error) {
	return c.load(ctx, true, ttl, minInterval, clock, fetch, clone)
}

// load 是 List/Refresh 的共同实现。force 为 true 时跳过缓存命中判断。
func (c *zaCache[T]) load(ctx context.Context, force bool, ttl, minInterval time.Duration, clock zaClock, fetch func(context.Context) ([]T, error), clone func([]T) []T) ([]T, error) {
	if clone == nil {
		return nil, errZACacheNoClone
	}
	if fetch == nil {
		return nil, errZACacheNoFetch
	}
	if clock == nil {
		clock = realZAClock{}
	}
	now := clock.Now()

	c.mu.Lock()
	if !force && c.ok && now.Sub(c.fetchedAt) < ttl {
		snapshot := c.items
		c.mu.Unlock()
		return clone(snapshot), nil
	}
	if f := c.flight; f != nil {
		c.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return nil, f.err
			}
			return clone(f.items), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if minInterval > 0 && !c.lastAttempt.IsZero() {
		if elapsed := now.Sub(c.lastAttempt); elapsed < minInterval {
			remain := minInterval - elapsed
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: 还需等待 %s", ErrZaloFetchTooSoon, remain.Round(time.Millisecond))
		}
	}
	c.lastAttempt = now
	f := &zaFlight[T]{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()

	return c.runFlight(ctx, f, clock, fetch, clone)
}

// runFlight 由 leader 执行 fetch 并结算 flight。
//
// defer 保证 panic 时也会关闭 done：leader 的 panic 原样抛给 leader 的调用方，
// 等待者收到 errZACacheFetchPanic 而不是永久阻塞。
func (c *zaCache[T]) runFlight(ctx context.Context, f *zaFlight[T], clock zaClock, fetch func(context.Context) ([]T, error), clone func([]T) []T) (items []T, err error) {
	defer func() {
		if r := recover(); r != nil {
			c.finishFlight(clock, f, nil, errZACacheFetchPanic, clone)
			panic(r)
		}
		c.finishFlight(clock, f, items, err, clone)
	}()
	return fetch(ctx)
}

// finishFlight 结算一次拉取：成功才更新缓存（失败保留旧缓存），随后共享错误、
// 关闭 done、清掉 flight。clone 在锁外完成，避免把调用方代码带进临界区。
func (c *zaCache[T]) finishFlight(clock zaClock, f *zaFlight[T], items []T, err error, clone func([]T) []T) {
	var stored []T
	var fetchedAt time.Time
	if err == nil {
		stored = clone(items)
		fetchedAt = clock.Now()
	}

	c.mu.Lock()
	if err == nil {
		c.items = stored
		c.fetchedAt = fetchedAt
		c.ok = true
		f.items = stored
	}
	f.err = err
	if c.flight == f {
		c.flight = nil
	}
	close(f.done)
	c.mu.Unlock()
}

// zaCachedUser 是一条带写入时间的用户资料缓存。
type zaCachedUser struct {
	info      UserInfo
	fetchedAt time.Time
}

// zaCachedGroup 是一条带写入时间的群资料缓存。
type zaCachedGroup struct {
	info      GroupInfo
	fetchedAt time.Time
}

// zaInfoCache 是用户/群资料按 ID 缓存 + 全局限速闸。
//
// mu 只保护两张缓存表（读写都不睡眠、不发请求）；throttleMu 单独保护 lastInfo，
// 允许持锁睡眠以串行化所有 user/group 资料请求。两个锁不嵌套，缓存命中永远不
// 会被正在等闸的请求阻塞。
type zaInfoCache struct {
	mu     sync.RWMutex
	users  map[string]zaCachedUser
	groups map[string]zaCachedGroup

	throttleMu sync.Mutex
	lastInfo   time.Time
}

// newZAInfoCache 构造空的资料缓存。
func newZAInfoCache() *zaInfoCache {
	return &zaInfoCache{
		users:  make(map[string]zaCachedUser),
		groups: make(map[string]zaCachedGroup),
	}
}

// Users 返回 ids 对应的用户资料，只请求缺失（或已过期）的 ID。
//
// 缺失 ID 去重后按 50 个一批：每批先重查缓存，再过共用 throttle（不足
// minInterval 则 clock.Sleep），过闸后再查一次缓存，仍有缺失才调 fetch。
// fetch 返回的条目写入缓存；本批请求了但服务端没返回的 ID 从缓存删除。
// 最终返回请求 ID 中实际存在的条目；全缺失时返回空 map 与 nil。
func (c *zaInfoCache) Users(ctx context.Context, ids []string, ttl, minInterval time.Duration, clock zaClock, fetch func(context.Context, []string) ([]UserInfo, error)) (map[string]UserInfo, error) {
	if fetch == nil {
		return nil, errZACacheNoFetch
	}
	if clock == nil {
		clock = realZAClock{}
	}
	want := uniqueNonEmptyIDs(ids)
	out := make(map[string]UserInfo, len(want))
	if len(want) == 0 {
		return out, nil
	}

	missing := c.missingUsers(want, ttl, clock.Now())
	for len(missing) > 0 {
		n := len(missing)
		if n > zaInfoBatchSize {
			n = zaInfoBatchSize
		}
		batch := missing[:n]
		missing = missing[n:]

		// 到闸前先重查：并发请求可能已经把这批填上了。
		batch = c.missingUsers(batch, ttl, clock.Now())
		if len(batch) == 0 {
			continue
		}
		if err := c.throttle(ctx, minInterval, clock); err != nil {
			return nil, err
		}
		// 等闸期间缓存可能又被填上，过闸后再确认一次。
		batch = c.missingUsers(batch, ttl, clock.Now())
		if len(batch) == 0 {
			continue
		}
		infos, err := fetch(ctx, batch)
		if err != nil {
			return nil, err
		}
		c.storeUsers(batch, infos, clock.Now())
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, id := range want {
		if e, ok := c.users[id]; ok {
			out[id] = e.info
		}
	}
	return out, nil
}

// Groups 与 Users 同口径，只是作用于群资料表并使用深拷贝返回 AdminIDs。
func (c *zaInfoCache) Groups(ctx context.Context, ids []string, ttl, minInterval time.Duration, clock zaClock, fetch func(context.Context, []string) ([]GroupInfo, error)) (map[string]GroupInfo, error) {
	if fetch == nil {
		return nil, errZACacheNoFetch
	}
	if clock == nil {
		clock = realZAClock{}
	}
	want := uniqueNonEmptyIDs(ids)
	out := make(map[string]GroupInfo, len(want))
	if len(want) == 0 {
		return out, nil
	}

	missing := c.missingGroups(want, ttl, clock.Now())
	for len(missing) > 0 {
		n := len(missing)
		if n > zaInfoBatchSize {
			n = zaInfoBatchSize
		}
		batch := missing[:n]
		missing = missing[n:]

		batch = c.missingGroups(batch, ttl, clock.Now())
		if len(batch) == 0 {
			continue
		}
		if err := c.throttle(ctx, minInterval, clock); err != nil {
			return nil, err
		}
		batch = c.missingGroups(batch, ttl, clock.Now())
		if len(batch) == 0 {
			continue
		}
		infos, err := fetch(ctx, batch)
		if err != nil {
			return nil, err
		}
		c.storeGroups(batch, infos, clock.Now())
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, id := range want {
		if e, ok := c.groups[id]; ok {
			out[id] = cloneGroupInfo(e.info)
		}
	}
	return out, nil
}

// CachedGroup 按 TTL 返回群资料深拷贝；不存在或已过期返回 false。
func (c *zaInfoCache) CachedGroup(id string, ttl time.Duration, now time.Time) (GroupInfo, bool) {
	if id == "" {
		return GroupInfo{}, false
	}
	c.mu.RLock()
	e, ok := c.groups[id]
	c.mu.RUnlock()
	if !ok || now.Sub(e.fetchedAt) >= ttl {
		return GroupInfo{}, false
	}
	return cloneGroupInfo(e.info), true
}

// DropGroups 从缓存删除指定群资料；未知 ID 与空串忽略。
func (c *zaInfoCache) DropGroups(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		delete(c.groups, id)
	}
}

// throttle 是 user/group 资料请求共用的闸：距上次请求开始不足 minInterval 时
// 持锁睡眠，睡醒后把本次请求开始时间写回 lastInfo。
//
// 持锁睡眠是有意为之：所有资料请求必须串行占用这条时间线；缓存命中路径不经过
// 这里，所以不会被等闸请求拖慢。Sleep 返回 ctx 错误时不更新 lastInfo。
func (c *zaInfoCache) throttle(ctx context.Context, minInterval time.Duration, clock zaClock) error {
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()

	now := clock.Now()
	if minInterval > 0 && !c.lastInfo.IsZero() {
		if elapsed := now.Sub(c.lastInfo); elapsed < minInterval {
			if err := clock.Sleep(ctx, minInterval-elapsed); err != nil {
				return fmt.Errorf("zalo 资料请求等待被取消: %w", err)
			}
		}
	}
	c.lastInfo = clock.Now()
	return nil
}

// missingUsers 在 RLock 下筛出没有新鲜缓存的 ID，保持输入顺序。
func (c *zaInfoCache) missingUsers(ids []string, ttl time.Duration, now time.Time) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if e, ok := c.users[id]; ok && now.Sub(e.fetchedAt) < ttl {
			continue
		}
		out = append(out, id)
	}
	return out
}

// missingGroups 是 missingUsers 的群资料版本。
func (c *zaInfoCache) missingGroups(ids []string, ttl time.Duration, now time.Time) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if e, ok := c.groups[id]; ok && now.Sub(e.fetchedAt) < ttl {
			continue
		}
		out = append(out, id)
	}
	return out
}

// storeUsers 写入本次服务端返回的用户资料，并删除本批请求中服务端未返回的 ID。
// 空 UserID 不写入，避免用无效键污染缓存。
func (c *zaInfoCache) storeUsers(requested []string, infos []UserInfo, now time.Time) {
	returned := make(map[string]struct{}, len(infos))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.users == nil {
		c.users = make(map[string]zaCachedUser)
	}
	for _, u := range infos {
		if u.UserID == "" {
			continue
		}
		returned[u.UserID] = struct{}{}
		c.users[u.UserID] = zaCachedUser{info: u, fetchedAt: now}
	}
	for _, id := range requested {
		if _, ok := returned[id]; !ok {
			delete(c.users, id)
		}
	}
}

// storeGroups 是 storeUsers 的群资料版本，写入时深拷贝 AdminIDs。
func (c *zaInfoCache) storeGroups(requested []string, infos []GroupInfo, now time.Time) {
	returned := make(map[string]struct{}, len(infos))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.groups == nil {
		c.groups = make(map[string]zaCachedGroup)
	}
	for _, g := range infos {
		if g.GroupID == "" {
			continue
		}
		returned[g.GroupID] = struct{}{}
		c.groups[g.GroupID] = zaCachedGroup{info: cloneGroupInfo(g), fetchedAt: now}
	}
	for _, id := range requested {
		if _, ok := returned[id]; !ok {
			delete(c.groups, id)
		}
	}
}

// cloneUserInfos 复制用户资料切片；UserInfo 无引用字段，复制底层数组即可。
func cloneUserInfos(in []UserInfo) []UserInfo {
	if in == nil {
		return nil
	}
	out := make([]UserInfo, len(in))
	copy(out, in)
	return out
}

// cloneGroupInfos 复制群资料切片，并对每项做深拷贝。
func cloneGroupInfos(in []GroupInfo) []GroupInfo {
	if in == nil {
		return nil
	}
	out := make([]GroupInfo, len(in))
	for i, g := range in {
		out[i] = cloneGroupInfo(g)
	}
	return out
}

// cloneGroupInfo 深拷贝一条群资料：AdminIDs 必须有独立底层数组，
// 否则调用方改自己的返回值会串改缓存。
func cloneGroupInfo(in GroupInfo) GroupInfo {
	out := in
	if in.AdminIDs != nil {
		out.AdminIDs = append([]string(nil), in.AdminIDs...)
	}
	return out
}

// uniqueNonEmptyIDs 去重并去掉空串，保持首次出现顺序；不 trim，避免改变调用方
// 期望的 map 键。
func uniqueNonEmptyIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
