// Package vars 是一个「包路径里带 vars 段」的第三方桩包，仅用于测试日志调用点定位：
// 它的函数名形如 touchgocore/testdata/vars.EnqueueFromHelper，包含子串 "/vars."，
// 早先按 Contains("/vars.") 判定「是否本包帧」的实现会把它当成 vars 内部帧跳过，
// 于是业务调用点被报成了测试文件那一行。
package vars

import (
	"log/slog"

	"touchgocore/vars"
)

// EnqueueFromHelper 从本包（即业务侧）发起一次入队，本应被识别为调用点。
func EnqueueFromHelper(ch *vars.AsyncLoggerChannel, level slog.Level, msg string) bool {
	return ch.Enqueue(level, msg, nil, nil)
}
