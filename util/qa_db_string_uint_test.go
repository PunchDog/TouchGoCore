package util

import (
	"math"
	"testing"
	"time"
)

// ==================== P3：to* 转换函数对无符号整型不得 panic ====================
//
// 事故形状：toBool/toInt/toFloat64/toString/toTime 把 uint 族与有符号整型
// 合并在同一 case 走 reflect.Value.Int()，而 reflect 对无符号 kind 调 .Int()
// 直接 panic（"reflect: call of reflect.Value.Int on uint Value"）。

func TestToBool_UintNoPanic(t *testing.T) {
	if !toBool(uint(1)) {
		t.Fatal("uint(1) 应为 true")
	}
	if toBool(uint(0)) {
		t.Fatal("uint(0) 应为 false")
	}
	if !toBool(uint64(math.MaxUint64)) {
		t.Fatal("uint64 大值应为 true")
	}
	if !toBool(uint8(2)) || !toBool(uint16(2)) || !toBool(uint32(2)) {
		t.Fatal("uint8/16/32 非零应为 true")
	}
}

func TestToInt_UintNoPanic(t *testing.T) {
	if got := toInt(uint(42)); got != 42 {
		t.Fatalf("uint(42)=%d", got)
	}
	if got := toInt(uint64(math.MaxInt64)); got != math.MaxInt64 {
		t.Fatalf("uint64(MaxInt64)=%d", got)
	}
	if got := toInt(uint8(255)); got != 255 {
		t.Fatalf("uint8(255)=%d", got)
	}
	// 有符号路径回归不变
	if got := toInt(int64(-7)); got != -7 {
		t.Fatalf("int64(-7)=%d", got)
	}
}

func TestToFloat64_UintNoPanic(t *testing.T) {
	if got := toFloat64(uint(3)); got != 3.0 {
		t.Fatalf("uint(3)=%v", got)
	}
	if got := toFloat64(uint64(1 << 40)); got != float64(uint64(1<<40)) {
		t.Fatalf("uint64(1<<40)=%v", got)
	}
}

func TestToString_UintNoPanic(t *testing.T) {
	if got := toString(uint(9)); got != "9" {
		t.Fatalf("uint(9)=%q", got)
	}
	if got := toString(uint64(math.MaxUint64)); got != "18446744073709551615" {
		t.Fatalf("MaxUint64=%q", got)
	}
	if got := toString(int16(-5)); got != "-5" {
		t.Fatalf("int16(-5)=%q", got)
	}
}

func TestToTime_UintNoPanic(t *testing.T) {
	if got := toTime(uint64(1000)); !got.Equal(time.UnixMilli(1000)) {
		t.Fatalf("uint64(1000)=%v", got)
	}
	if got := toTime(uint(0)); !got.Equal(time.UnixMilli(0)) {
		t.Fatalf("uint(0)=%v", got)
	}
}

// 经公开入口 ParseDbData 的端到端口径（recover 会吞 panic，但结果必须正确）。
func TestParseDbData_UintSource(t *testing.T) {
	var i int64
	ParseDbData(&i, uint(123))
	if i != 123 {
		t.Fatalf("ParseDbData(*int64, uint(123))=%d", i)
	}
	var s string
	ParseDbData(&s, uint64(math.MaxUint64))
	if s != "18446744073709551615" {
		t.Fatalf("ParseDbData(*string, MaxUint64)=%q", s)
	}
	var b bool
	ParseDbData(&b, uint(1))
	if !b {
		t.Fatal("ParseDbData(*bool, uint(1))=false")
	}
}
