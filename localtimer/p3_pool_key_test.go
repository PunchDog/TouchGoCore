package localtimer_test

import (
	"testing"

	"touchgocore/localtimer"
	"touchgocore/localtimer/internal/dupname"
)

// PlainTimer 与 dupname.PlainTimer 短名相同、包不同，用于回归 S62：
// 池按短名分组时两者共用一个 sync.Pool，New 闭包捕获的是首个调用方的元素类型，
// 后到的类型会从池里拿到别包实例，NewTimer 断言回 T 时直接 panic。
type PlainTimer struct {
	localtimer.Timer
	Mark int
}

// Tick 实现 localtimer.TimerInterface。
func (p *PlainTimer) Tick() {}

func TestTimerPoolKeyedByTypeNotShortName(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("✘ 跨包同短名撞池，别包实例被发给本包类型: %v", r)
		}
	}()

	first, err := localtimer.NewTimer[*PlainTimer](1000, -1, nil)
	if err != nil {
		t.Fatalf("本包定时器创建失败: %v", err)
	}
	first.Mark = 7

	other, err := localtimer.NewTimer[*dupname.PlainTimer](1000, -1, nil)
	if err != nil {
		t.Fatalf("外部包定时器创建失败: %v", err)
	}
	other.Mark = "外部包"

	if first.Mark != 7 {
		t.Fatalf("✘ 本包实例字段被外部包类型覆盖: Mark=%d", first.Mark)
	}
	if other.Mark != "外部包" {
		t.Fatalf("✘ 外部包实例类型不符: %#v", other)
	}
}
