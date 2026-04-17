package util

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
	"touchgocore/vars"

	"github.com/redis/go-redis/v9"
)

const (
	// VirtualTimeKey Redis 中存储虚拟时间配置的 Key
	VirtualTimeKey = "game:virtual_time"
	// FieldRealTime    Redis Hash 中的真实时间字段名
	FieldRealTime = "real_time"
	// FieldVirtualTime Redis Hash 中的虚拟时间字段名
	FieldVirtualTime = "virtual_time"
)

// virtualTimeHolder 保存 Redis 客户端的全局变量
var (
	redisClient redis.Cmdable
)

// VirtualTimeData 虚拟时间数据结构
type VirtualTimeData struct {
	RealTime    int64 // 记录时的真实时间（Unix 毫秒）
	VirtualTime int64 // 记录时的虚拟时间（Unix 毫秒）
}

func virtualTimeKey() string {
	return fmt.Sprintf("%s:%s", VirtualTimeKey, GameGroup)
}

// GetVirtualTimeData 获取 Redis 中存储的虚拟时间数据
// 返回 nil 表示未找到或出错，使用真实时间
func GetVirtualTimeData(ctx context.Context) (*VirtualTimeData, error) {
	if redisClient == nil {
		return nil, errors.New("redis client not initialized")
	}

	// 使用 HGETALL 获取所有字段
	result, err := redisClient.HGetAll(ctx, virtualTimeKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("get virtual time from redis: %w", err)
	}

	// 如果 Hash 不存在，返回 nil
	if len(result) == 0 {
		return nil, fmt.Errorf("get virtual time result from redis is 0")
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
// realTime: 当前的真实时间（Unix 毫秒）
// virtualTime: 虚拟时间（Unix 毫秒）
func SetVirtualTimeData(ctx context.Context, realTime, virtualTime int64) error {
	if redisClient == nil {
		return errors.New("redis client not initialized")
	}

	err := redisClient.HSet(ctx, virtualTimeKey(), map[string]any{
		FieldRealTime:    realTime,
		FieldVirtualTime: virtualTime,
	}).Err()
	if err != nil {
		return fmt.Errorf("set virtual time to redis: %w", err)
	}

	return nil
}

// CalculateVirtualTime 根据 Redis 中的虚拟时间数据计算当前虚拟时间
// 公式：虚拟时间 = 记录虚拟时间 + (当前真实时间 - 记录真实时间)
// 如果 Redis 中没有虚拟时间配置，返回当前真实时间
func CalculateVirtualTime(ctx context.Context) (time.Time, error) {
	// 获取当前真实时间
	nowReal := time.Now().UnixMilli()

	// 获取 Redis 中的虚拟时间数据
	data, err := GetVirtualTimeData(ctx)
	if err != nil {
		return time.Now(), err
	}

	// 计算偏移量
	offset := nowReal - data.RealTime

	// 计算虚拟时间
	virtualMs := data.VirtualTime + offset

	return time.UnixMilli(virtualMs), nil
}

// CurrentTime 返回当前时间，支持虚拟时间
// 如果 Redis 中配置了虚拟时间，则返回虚拟时间；否则返回真实时间
func CurrentTime() time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	virtualTime, err := CalculateVirtualTime(ctx)
	if err != nil {
		// 如果出错或返回零值，使用真实时间
		return time.Now()
	}
	return virtualTime
}

// ResetVirtualTime 重置虚拟时间为真实时间
func ResetVirtualTime(ctx context.Context) error {
	if redisClient == nil {
		return errors.New("redis client not initialized")
	}

	return redisClient.Del(ctx, VirtualTimeKey).Err()
}

// InitVirtualTime 初始化虚拟时间模块（应在应用启动时调用）
// 从 App 实例获取 Redis 客户端并设置到 util 包中
func InitVirtualTime(client redis.Cmdable) {
	redisClient = client
	vars.Info("虚拟时间模块初始化redis")
}

// 时间工具函数部分保持不变（已优化命名和错误处理）
// CurrentMS 返回当前毫秒时间戳
func CurrentMS() int64 {
	return CurrentTime().UnixMilli()
}

// CurrentS 返回当前秒时间戳
func CurrentS() int64 {
	return CurrentTime().Unix()
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
