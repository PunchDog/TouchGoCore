package util

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/vars"

	"github.com/redis/go-redis/v9"
)

const (
	// VirtualTimeKey Redis 中存储虚拟时间配置的 Key（同 GameGroup 共用）
	VirtualTimeKey = "game:virtual_time"
	// FieldRealTime    Redis Hash 中的真实时间字段名
	FieldRealTime = "real_time"
	// FieldVirtualTime Redis Hash 中的虚拟时间字段名
	FieldVirtualTime = "virtual_time"
	defaultVTPoll    = 200 * time.Millisecond
)

// ErrVirtualTimeNotSet Redis 中未配置该组虚拟时间，各节点应走真实时间。
var ErrVirtualTimeNotSet = errors.New("virtual time not set")

// virtualTimeBackend 组内共享存储（生产为 Redis）。
type virtualTimeBackend interface {
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HSet(ctx context.Context, key string, fields map[string]any) error
	Del(ctx context.Context, key string) error
}

type redisVTBackend struct {
	c redis.Cmdable
}

func (r redisVTBackend) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return r.c.HGetAll(ctx, key).Result()
}

func (r redisVTBackend) HSet(ctx context.Context, key string, fields map[string]any) error {
	return r.c.HSet(ctx, key, fields).Err()
}

func (r redisVTBackend) Del(ctx context.Context, key string) error {
	return r.c.Del(ctx, key).Err()
}

type redisSubscriber interface {
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

type vtSnapshot struct {
	data    *VirtualTimeData
	hasData bool
}

var (
	redisClient    redis.Cmdable
	vtBackend      virtualTimeBackend
	vtSnap         atomic.Pointer[vtSnapshot]
	vtPollInterval atomic.Int64
	vtSyncMu       sync.Mutex
	vtSyncCancel   context.CancelFunc

	// vtLastRefresh 是「上一次成功从 Redis 拉取偏移」的**真实**时刻（UnixNano）。
	// 只用于 CalculateVirtualTime 的刷新节流，绝不能用虚拟时间记账（否则自指）。
	// 由 200ms 轮询、PubSub 通知与本函数自身的强刷共同推进。
	vtLastRefresh atomic.Int64
	// vtRefreshMu 把「判定到期 → 拉 Redis」收口成进程内串行，避免热路径上
	// 同一瞬间几百个 goroutine 同时发现快照过期、一起打 Redis（惊群）。
	vtRefreshMu sync.Mutex
)

func init() {
	vtPollInterval.Store(int64(defaultVTPoll))
}

// VirtualTimeData 虚拟时间数据结构
//
// ⚠️ 两个字段都是 **Unix 纳秒**，不是毫秒：applyVirtualOffset 用
// time.Now().UnixNano() 与 time.Unix(0, ...) 换算，SetVirtualTimeData 的调用方
// （含 util/time_test.go）传的也是 UnixNano。曾误写为「毫秒」，按那个注释传值
// 会得到 1970 年的时间。StringToUnixTime/MSToTimeString 的毫秒口径与此无关。
type VirtualTimeData struct {
	RealTime    int64 // 记录时的真实时间（Unix 纳秒）
	VirtualTime int64 // 记录时的虚拟时间（Unix 纳秒）
}

func virtualTimeKey() string {
	return fmt.Sprintf("%s:%s", VirtualTimeKey, GameGroup)
}

// VirtualTimeRedisKey 返回当前进程使用的虚拟时间 Redis key（含 GameGroup）。
func VirtualTimeRedisKey() string {
	return virtualTimeKey()
}

func virtualTimeNotifyChannel() string {
	return virtualTimeKey() + ":notify"
}

func storeSnapshot(data *VirtualTimeData, hasData bool) {
	vtSnap.Store(&vtSnapshot{data: data, hasData: hasData})
}

func stopVirtualTimeSync() {
	vtSyncMu.Lock()
	defer vtSyncMu.Unlock()
	if vtSyncCancel != nil {
		vtSyncCancel()
		vtSyncCancel = nil
	}
}

func startVirtualTimeSync(client redis.Cmdable) {
	stopVirtualTimeSync()
	ctx, cancel := context.WithCancel(context.Background())
	vtSyncMu.Lock()
	vtSyncCancel = cancel
	vtSyncMu.Unlock()
	go vtPollLoop(ctx)
	if client != nil {
		go vtSubscribeLoop(ctx, client)
	}
}

func useVirtualTimeBackend(b virtualTimeBackend) {
	stopVirtualTimeSync()
	vtBackend = b
	vtLastRefresh.Store(0)
	if b == nil {
		redisClient = nil
		storeSnapshot(nil, false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	RefreshVirtualTime(ctx)
}

// RefreshVirtualTime 从 Redis（组内共享节点）拉取偏移并更新本地快照。
// 这是**显式强刷**入口：无条件打一次 Redis，成功（含「确认未设置」）后推进
// vtLastRefresh。CalculateVirtualTime 的节流只决定是否调用本函数，不改变
// 本函数语义，所以轮询/PubSub/测试注入三条路径的行为完全不变。
func RefreshVirtualTime(ctx context.Context) {
	if vtBackend == nil {
		storeSnapshot(nil, false)
		vtLastRefresh.Store(time.Now().UnixNano())
		return
	}
	data, err := GetVirtualTimeData(ctx)
	if err != nil {
		if errors.Is(err, ErrVirtualTimeNotSet) {
			storeSnapshot(nil, false)
			vtLastRefresh.Store(time.Now().UnixNano())
			return
		}
		// Redis 瞬时失败时保留上一份快照，避免同组一部分节点跳回真实时间。
		// 不推进 vtLastRefresh：故障期间保持「到期」状态，下次调用继续重试。
		return
	}
	storeSnapshot(data, true)
	vtLastRefresh.Store(time.Now().UnixNano())
}

func vtPollLoop(ctx context.Context) {
	d := time.Duration(vtPollInterval.Load())
	if d <= 0 {
		d = defaultVTPoll
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			RefreshVirtualTime(c)
			cancel()
		}
	}
}

func vtSubscribeLoop(ctx context.Context, client redis.Cmdable) {
	subber, ok := client.(redisSubscriber)
	if !ok {
		return
	}
	ps := subber.Subscribe(ctx, virtualTimeNotifyChannel())
	defer func() { _ = ps.Close() }()
	ch := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_ = msg
			c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			RefreshVirtualTime(c)
			cancel()
		}
	}
}

func vtPublish(ctx context.Context, action string) {
	if redisClient == nil {
		return
	}
	_ = redisClient.Publish(ctx, virtualTimeNotifyChannel(), action).Err()
}

// SetVirtualTimePollInterval 组内从 Redis 拉取偏移的间隔（CurrentTime 热路径不打 Redis）。
func SetVirtualTimePollInterval(d time.Duration) {
	if d <= 0 {
		d = defaultVTPoll
	}
	vtPollInterval.Store(int64(d))
}

// GetVirtualTimeData 读取 Redis 中该 GameGroup 的共享虚拟时间偏移。
func GetVirtualTimeData(ctx context.Context) (*VirtualTimeData, error) {
	if vtBackend == nil {
		return nil, errors.New("redis client not initialized")
	}

	result, err := vtBackend.HGetAll(ctx, virtualTimeKey())
	if err != nil {
		return nil, fmt.Errorf("get virtual time from redis: %w", err)
	}

	// 如果 Hash 不存在，返回 nil
	if len(result) == 0 {
		return nil, ErrVirtualTimeNotSet
	}

	realTime, err := strconv.ParseInt(result[FieldRealTime], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse real_time: %w", err)
	}

	virtualTime, err := strconv.ParseInt(result[FieldVirtualTime], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse virtual_time: %w", err)
	}

	return &VirtualTimeData{
		RealTime:    realTime,
		VirtualTime: virtualTime,
	}, nil
}

// SetVirtualTimeData 设置虚拟时间数据到 Redis
// realTime: 当前的真实时间（Unix 纳秒）
// virtualTime: 虚拟时间（Unix 纳秒）
func SetVirtualTimeData(ctx context.Context, realTime, virtualTime int64) error {
	if vtBackend == nil {
		return errors.New("redis client not initialized")
	}

	fields := map[string]any{
		FieldRealTime:    realTime,
		FieldVirtualTime: virtualTime,
	}
	if err := vtBackend.HSet(ctx, virtualTimeKey(), fields); err != nil {
		return fmt.Errorf("set virtual time to redis: %w", err)
	}
	storeSnapshot(&VirtualTimeData{RealTime: realTime, VirtualTime: virtualTime}, true)
	// 刚写完 Redis 且本地快照已是最新，推进刷新时刻，避免紧接着的
	// CalculateVirtualTime 立刻又打一次 HGetAll。
	vtLastRefresh.Store(time.Now().UnixNano())
	vtPublish(ctx, "set")
	return nil
}

// CalculateVirtualTime 根据 Redis 中的虚拟时间数据计算当前虚拟时间
// 公式：虚拟时间 = 记录虚拟时间 + (当前真实时间 - 记录真实时间)
// 如果 Redis 中没有虚拟时间配置，返回当前真实时间
func applyVirtualOffset(data *VirtualTimeData) time.Time {
	nowReal := time.Now().UnixNano()
	return time.Unix(0, data.VirtualTime+(nowReal-data.RealTime))
}

func snapshotOffset() *VirtualTimeData {
	e := vtSnap.Load()
	if e == nil || !e.hasData || e.data == nil {
		return nil
	}
	return e.data
}

// CalculateVirtualTime 按 Redis 组内共享偏移计算当前虚拟时间。
// 公式：虚拟时间 = 记录虚拟时间 + (当前真实时间 - 记录真实时间)
//
// 热路径节流（2026-11-05）：偏移本来由 200ms 轮询（vtPollLoop）+ Redis PubSub
// 通知同步，「每次调用都 HGetAll」读不到更新的值，却会把每个 HTTP 响应包络、
// 每条业务时间戳都变成一次 Redis 往返。因此只在快照到期（默认 vtPollInterval）
// 时才强刷，其余调用直接对本地快照做纳秒换算，成本与 CurrentTime() 相同。
// 语义不变：返回值始终是「当前虚拟时间」；设置方 SetVirtualTimeData 会立即
// storeSnapshot + Publish，同组节点靠 PubSub 在毫秒级跟上，不依赖本函数的刷新。
// 需要无条件强刷的调用方仍可显式调 RefreshVirtualTime(ctx)。
func CalculateVirtualTime(ctx context.Context) (time.Time, error) {
	refreshVirtualTimeIfDue(ctx)
	if data := snapshotOffset(); data != nil {
		return applyVirtualOffset(data), nil
	}
	if vtBackend == nil {
		return time.Now(), errors.New("redis client not initialized")
	}
	return time.Now(), ErrVirtualTimeNotSet
}

// refreshVirtualTimeIfDue 把「判定到期 → 拉 Redis」在进程内串行化，避免惊群。
// 到期判据用真实时钟，绝不能用虚拟时间（否则偏移会反过来影响自己的刷新节奏）。
func refreshVirtualTimeIfDue(ctx context.Context) {
	vtRefreshMu.Lock()
	defer vtRefreshMu.Unlock()
	d := time.Duration(vtPollInterval.Load())
	if d <= 0 {
		d = defaultVTPoll
	}
	last := vtLastRefresh.Load()
	if last != 0 && time.Now().UnixNano()-last < int64(d) {
		return
	}
	RefreshVirtualTime(ctx)
}

// CurrentTime 返回当前时间。偏移以 Redis 中 GameGroup 共享数据为准，
// 热路径只做本地换算；偏移由轮询/PubSub 从 Redis 同步。
func CurrentTime() time.Time {
	if data := snapshotOffset(); data != nil {
		return applyVirtualOffset(data)
	}
	return time.Now()
}

// RealTime 返回**真实**当前时间，永不受虚拟时间偏移影响。
//
// 用途边界（2026-11-05 全链路虚拟时间改造定稿）：一切**基础设施**计时必须用它
// 或 time.Now()，包括 context deadline/timeout 比较、ticker/timer 周期、重试退避、
// 缓存与告警去抖的「上次时刻」、连接读写 deadline、计时轮、用时间做种子的 ID/nonce。
// 把这些换成 CurrentTime()/CalculateVirtualTime()，在运营回拨或前跳虚拟时间时
// 会让 deadline 永不触发、计时轮停摆或空转、time.Since 变负、随机源重复。
//
// 业务时间（写库时间戳、日窗/周期/过期判定、对外展示）才走
// CurrentTime()/CalculateVirtualTime()。
func RealTime() time.Time { return time.Now() }

// RealMS 是 RealTime 的毫秒形式，供计时轮等基础设施使用。
func RealMS() int64 { return time.Now().UnixMilli() }

// ResetVirtualTime 删除组内 Redis 偏移，同组节点随后回到真实时间。
func ResetVirtualTime(ctx context.Context) error {
	if vtBackend == nil {
		return errors.New("redis client not initialized")
	}
	if err := vtBackend.Del(ctx, virtualTimeKey()); err != nil {
		return err
	}
	storeSnapshot(nil, false)
	vtLastRefresh.Store(time.Now().UnixNano())
	vtPublish(ctx, "reset")
	return nil
}

// SetVirtualTime 把「此刻」映射到目标虚拟时刻 at，是设置虚拟时间的**唯一推荐入口**。
//
// 内部用 time.Now().UnixNano() 与 at.UnixNano() 组装 SetVirtualTimeData 的两个
// 纳秒参数，调用方不需要也不应该碰 int64 单位 —— 本文件同时存在毫秒辅助函数
// （StringToUnixTime/MSToTimeString/CurrentMS），手工拼 int64 极易把毫秒喂给纳秒
// 实现，偏移直接放大 1e6 倍（虚拟时间跳到公元数千年后）。
//
// 语义：at 是「设置生效瞬间应显示的时间」，offset = at - now；之后每过 1 秒真实
// 时间虚拟时间也前进 1 秒（不冻结）。允许 at 早于 now（回拨），业务护栏由调用方
// 负责（后台接口做范围校验与二次确认）。
func SetVirtualTime(ctx context.Context, at time.Time) error {
	return SetVirtualTimeData(ctx, time.Now().UnixNano(), at.UnixNano())
}

// VirtualTimeState 是「当前虚拟时间配置」的只读视图，供后台回显与健康检查。
// 所有时间字段都是 time.Time，调用方不需要再做纳秒/毫秒换算。
type VirtualTimeState struct {
	Enabled    bool      // Redis 中是否存在该组的虚拟时间配置
	Anchor     time.Time // 设置时记录的虚拟时刻（Enabled=false 时为零值）
	AnchorReal time.Time // 设置时记录的真实时刻（Enabled=false 时为零值）
	RedisKey   string    // 实际使用的 key（含 GameGroup），排障与启动对拍用
}

// GetVirtualTime 读取该组的虚拟时间配置视图。与 GetVirtualTimeData 的区别：
// 这里把纳秒 int64 还原成 time.Time 并附带 Redis key，避免调用方各自换算单位。
// 未配置时返回 (Enabled:false, nil) —— 「未配置」是配置态而非错误。
func GetVirtualTime(ctx context.Context) (VirtualTimeState, error) {
	state := VirtualTimeState{RedisKey: VirtualTimeRedisKey()}
	data, err := GetVirtualTimeData(ctx)
	if err != nil {
		if errors.Is(err, ErrVirtualTimeNotSet) {
			return state, nil
		}
		return state, err
	}
	state.Enabled = true
	state.Anchor = time.Unix(0, data.VirtualTime)
	state.AnchorReal = time.Unix(0, data.RealTime)
	return state, nil
}

// InitVirtualTime 绑定 Redis 共享节点，并启动组内偏移同步。
func InitVirtualTime(client redis.Cmdable) {
	redisClient = client
	if client == nil {
		useVirtualTimeBackend(nil)
		return
	}
	vtBackend = redisVTBackend{c: client}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	RefreshVirtualTime(ctx)
	cancel()
	startVirtualTimeSync(client)
	vars.Info("虚拟时间模块使用 Redis 共享节点 key=%s", virtualTimeKey())
}

// StopVirtualTime 停止组内 Redis 同步。
func StopVirtualTime() {
	stopVirtualTimeSync()
	storeSnapshot(nil, false)
	vtLastRefresh.Store(0)
	vtBackend = nil
	redisClient = nil
}

// 时间工具函数部分保持不变（已优化命名和错误处理）
// CurrentMS 返回当前毫秒时间戳
func CurrentMS() int64 {
	return CurrentTime().UnixMilli()
}

// MSToTimeString 毫秒转时间字符串
func MSToTimeString(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// SecondToTimeString 秒转时间字符串
func SecondToTimeString(sec int64) string {
	return time.Unix(sec, 0).Format("2006-01-02 15:04:05")
}

// TimeToMidnight 获取时间的午夜时间
func TimeToMidnight(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

// MillisecondToMidnight 毫秒时间戳转午夜时间
func MillisecondToMidnight(ms int64) time.Time {
	return TimeToMidnight(time.UnixMilli(ms))
}

// StringToUnixTime 字符串转时间戳时间格式一定是2006-01-02 15:04:05
func StringToUnixTime(value string) (int64, error) {
	//时间格式是2006-01-02 15:04:05或者2006/01/02 15:04:05
	re := regexp.MustCompile(`^(\d{4})[-/](\d{2})[-/](\d{2}) (\d{2}):(\d{2}):(\d{2})$`)
	matches := re.FindStringSubmatch(value)
	if matches == nil || len(matches) != 7 {
		return 0, errors.New("invalid time format, expected: 2006-01-02 15:04:05")
	}

	year, _ := strconv.Atoi(matches[1])
	month, _ := strconv.Atoi(matches[2])
	day, _ := strconv.Atoi(matches[3])
	hour, _ := strconv.Atoi(matches[4])
	min, _ := strconv.Atoi(matches[5])
	sec, _ := strconv.Atoi(matches[6])

	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	return t.UnixMilli(), nil
}

// NextMidnight 获取下一个午夜时间
func NextMidnight(ms int64) int64 {
	return TimeToMidnight(time.UnixMilli(ms)).Add(24 * time.Hour).UnixMilli()
}

// NextHour 获取下一个整点时间
func NextHour(ms int64) int64 {
	t := time.UnixMilli(ms)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location()).UnixMilli()
}

// IsSameWeek 判断是否在同一周
func IsSameWeek(ms1, ms2 int64) bool {
	if ms1 == 0 || ms2 == 0 {
		return false
	}
	y1, w1 := time.UnixMilli(ms1).ISOWeek()
	y2, w2 := time.UnixMilli(ms2).ISOWeek()
	return y1 == y2 && w1 == w2
}

// IsSameMonth 判断是否在同一月
func IsSameMonth(ms1, ms2 int64) bool {
	if ms1 == 0 || ms2 == 0 {
		return false
	}
	y1, m1, _ := time.UnixMilli(ms1).Date()
	y2, m2, _ := time.UnixMilli(ms2).Date()
	return y1 == y2 && m1 == m2
}

// IsSameDay 判断是否在同一天
func IsSameDay(ms1, ms2 int64) bool {
	t1 := time.UnixMilli(ms1)
	t2 := time.UnixMilli(ms2)
	return t1.Year() == t2.Year() && t1.Month() == t2.Month() && t1.Day() == t2.Day()
}

// 获取时间对应的周几
func GetWeekDay(ms int64) int {
	t := time.UnixMilli(ms)
	return int(t.Weekday())
}

// FormatDuration 将持续时间格式化为人类可读的字符串
//
// Deprecated: 仓内零调用，仅为兼容既有外部用法保留。
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}
