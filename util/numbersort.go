package util

import (
	"strconv"
	"strings"
)

// 定义数值类型约束（包含所有整型和浮点型）
type Numeric interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// 数字类排序，从小到大
type NumberSortLess[T Numeric] []T

func (this NumberSortLess[T]) Len() int {
	return len(this)
}
func (this NumberSortLess[T]) Less(i, j int) bool {
	return this[i] < this[j] // 直接比较数值
}
func (this NumberSortLess[T]) Swap(i, j int) {
	this[i], this[j] = this[j], this[i]
}

// 数字类排序，从大到小
type NumberSortDesc[T Numeric] []T

func (this NumberSortDesc[T]) Len() int {
	return len(this)
}
func (this NumberSortDesc[T]) Less(i, j int) bool {
	return this[i] > this[j] // 直接比较数值
}
func (this NumberSortDesc[T]) Swap(i, j int) {
	this[i], this[j] = this[j], this[i]
}

// getNumber 将字符串转换为数值类型（优化：使用类型断言替代反射，避免 reflect 开销）
func getNumber[T any](v string) T {
	var d T
	switch any(&d).(type) {
	case *uint:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint) = uint(num)
	case *uint8:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint8) = uint8(num)
	case *uint16:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint16) = uint16(num)
	case *uint32:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint32) = uint32(num)
	case *uint64:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint64) = num
	case *int:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int) = int(num)
	case *int8:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int8) = int8(num)
	case *int16:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int16) = int16(num)
	case *int32:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int32) = int32(num)
	case *int64:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int64) = num
	case *float32:
		num, _ := strconv.ParseFloat(v, 32)
		*any(&d).(*float32) = float32(num)
	case *float64:
		num, _ := strconv.ParseFloat(v, 64)
		*any(&d).(*float64) = num
	}
	return d
}

// 字符串转数字数组
func String2NumberArray[T any](str string, sep string) []T {
	strs := strings.Split(str, sep)
	ret := make([]T, 0)
	if len(strs) > 0 {
		for _, str := range strs {
			ret = append(ret, getNumber[T](str))
		}
	}
	return ret
}
