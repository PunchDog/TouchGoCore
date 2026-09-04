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
)

func init() {
	vtPollInterval.Store(int64(defaultVTPoll))
}

// VirtualTimeData 虚拟时间数据结构
type VirtualTimeData struct {
	RealTime    int64 // 记录时的真实时间（Unix 毫秒）
	VirtualTime int64 // 记录时的虚拟时间（Unix 毫秒）
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
func RefreshVirtualTime(ctx context.Context) {
	if vtBackend == nil {
		storeSnapshot(nil, false)
		return
	}
	data, err := GetVirtualTimeData(ctx)
	if err != nil {
		if errors.Is(err, ErrVirtualTimeNotSet) {
			storeSnapshot(nil, false)
			return
		}
		// Redis 瞬时失败时保留上一份快照，避免同组一部分节点跳回真实时间。
		return
	}
	storeSnapshot(data, true)
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
func CalculateVirtualTime(ctx context.Context) (time.Time, error) {
	RefreshVirtualTime(ctx)
	if data := snapshotOffset(); data != nil {
		return applyVirtualOffset(data), nil
	}
	if vtBackend == nil {
		return time.Now(), errors.New("redis client not initialized")
	}
	return time.Now(), ErrVirtualTimeNotSet
}

// CurrentTime 返回当前时间。偏移以 Redis 中 GameGroup 共享数据为准，
// 热路径只做本地换算；偏移由轮询/PubSub 从 Redis 同步。
func CurrentTime() time.Time {
	if data := snapshotOffset(); data != nil {
		return applyVirtualOffset(data)
	}
	return time.Now()
}

// ResetVirtualTime 删除组内 Redis 偏移，同组节点随后回到真实时间。
func ResetVirtualTime(ctx context.Context) error {
	if vtBackend == nil {
		return errors.New("redis client not initialized")
	}
	if err := vtBackend.Del(ctx, virtualTimeKey()); err != nil {
		return err
	}
	storeSnapshot(nil, false)
	vtPublish(ctx, "reset")
	return nil
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
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}
