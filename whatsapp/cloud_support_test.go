package whatsapp

import (
	"context"
	"testing"
	"time"
)

func TestWithinWindow(t *testing.T) {
	now := time.Now()
	if !cloudWithinWindow(now.Add(-time.Hour).Unix(), now) {
		t.Fatal("1 小时前应在窗口内")
	}
	if cloudWithinWindow(now.Add(-25*time.Hour).Unix(), now) {
		t.Fatal("25 小时前应在窗口外")
	}
	if cloudWithinWindow(0, now) {
		t.Fatal("零时间戳应判窗口外")
	}
}

func TestSupportReplyWindowGuard(t *testing.T) {
	srv, _ := newFakeGraph(t)
	startWith(t, cloudTestCfg(t, srv.URL))
	c := WhatsappCloud()
	// 窗口外会在发请求前拦截
	if _, err := c.SupportReply(context.Background(), "86138", "hi", false); err == nil {
		t.Fatal("窗口外回复应被拒绝")
	}
	if _, err := c.SupportReply(context.Background(), "", "hi", true); err == nil {
		t.Fatal("空接收号应被拒绝")
	}
}
