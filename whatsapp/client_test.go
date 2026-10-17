package whatsapp

// 原 client_test.go 测的是资金供应商链路的装配、报文、签名与重试，随实现一并删除；
// 这里只留下启动生命周期那条混合用例——它判的是消息侧 fail-closed 与停止幂等。

import (
	"context"
	"errors"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
)

// TestStartNilCfgDoesNotPanic 没有任何 whatsapp 配置时启动必须安静通过：
// 消息侧不装配、门面就地拒绝；资金门面本就未接入，与启没启动无关。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	WhatsappStop(nil)
	WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	// 传 nil 回落到包级运行快照：上一条刚把「没有配置」写进快照，这里才测得准「未启动」。
	WhatsappStart(nil)
	if _, err := WhatsappQuery(nil, "O1"); !errors.Is(err, ErrPayoutNotWired) {
		t.Fatalf("未接入的查询应返回哨兵而不是别的错误: %v", err)
	}
	if _, err := WhatsappSendMessage(nil, "86138", "86139", "hi"); err == nil {
		t.Fatal("消息侧未启动时发送应当被拒")
	}
	WhatsappStop(nil)
	WhatsappStop(nil) // 幂等
}
