package whatsapp

import (
	"os"
	"testing"

	"touchgocore/config"
)

// 单测默认关掉 send_rate 闸（Window/Max 都为 0 = 不限）。
//
// 【为什么需要】2026-11-02 起单条发送也进速率账（awaitSendSlot），缺省速率是
// 60 秒 1 条。既有用例里大量「同一账号连发两三条」的场景（发信后清空、防封闸、
// 自动登录、JID 归一）本意都不是测节奏 —— 留着闸会让它们每条白等一个窗口，
// 测试从秒级变成分钟级，而它们要断言的东西与节奏毫无关系。
//
// 节奏本身由 wa_sendslot_test.go 专测：那里显式注入速率（withSendRate），
// 断言的正是「第二条要等满窗口」。
//
// 用 override 而不是改配置：override 是业务侧真实的注入通道
// （whatsapp.SetSendRateOverride），测的就是它。
func TestMain(m *testing.M) {
	SetSendRateOverride(&config.WaSendRate{})
	code := m.Run()
	SetSendRateOverride(nil)
	os.Exit(code)
}
