// Package dupname 只为回归「定时器对象池按类型短名分组」而存在：
// 这里的 PlainTimer 与 localtimer 测试包里的同名类型短名一致、包不同。
package dupname

import "touchgocore/localtimer"

// PlainTimer 是外部包里的定时器类型。
type PlainTimer struct {
	localtimer.Timer
	Mark string
}

// Tick 实现 TimerInterface。
func (p *PlainTimer) Tick() {}
