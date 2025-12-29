package util

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// 时间工具函数部分保持不变（已优化命名和错误处理）
// CurrentMillisecond 返回当前毫秒时间戳
func CurrentMillisecond() int64 {
	return time.Now().UnixMilli()
}

// CurrentSecond 返回当前秒时间戳
func CurrentSecond() int64 {
	return time.Now().Unix()
}

// MillisecondToTimeString 毫秒转时间字符串
func MillisecondToTimeString(ms int64) string {
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

// StringToUnixTime 字符串转时间戳
func StringToUnixTime(value string) (int64, error) {
	re := regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})$`)
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
