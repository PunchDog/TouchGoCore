package whatsapp

import (
	"context"
	"strings"
	"testing"
)

// TestOnboardingStepRender 验证框架只负责「按配置渲染第 idx 步并发送」：
// 步骤文案与按钮 id 落进出向报文，进度号由调用方决定。
func TestOnboardingStepRender(t *testing.T) {
	srv, bodies := newFakeGraph(t)
	startWith(t, cloudTestCfg(t, srv.URL))

	if _, err := WhatsappCloudSendOnboardingStep(context.Background(), "86138", 0); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("第 0 步应发出 1 条请求，实得 %d", len(*bodies))
	}
	if !strings.Contains((*bodies)[0], "欢迎使用") || !strings.Contains((*bodies)[0], "lang_zh") {
		t.Fatalf("第 0 步报文不符: %s", (*bodies)[0])
	}

	if _, err := WhatsappCloudSendOnboardingStep(context.Background(), "86138", 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*bodies)[1], "是否开启消息通知") || !strings.Contains((*bodies)[1], "notify_on") {
		t.Fatalf("第 1 步报文不符: %s", (*bodies)[1])
	}
}

// TestOnboardingStepOutOfRange 验证越界步号直接报错且不发出请求（内置默认两步）。
func TestOnboardingStepOutOfRange(t *testing.T) {
	srv, bodies := newFakeGraph(t)
	startWith(t, cloudTestCfg(t, srv.URL))

	for _, idx := range []int{-1, 2, 99} {
		if _, err := WhatsappCloudSendOnboardingStep(context.Background(), "86138", idx); err == nil {
			t.Fatalf("步号 %d 应越界报错", idx)
		}
	}
	if len(*bodies) != 0 {
		t.Fatalf("越界时不应发出请求，实得 %d 条", len(*bodies))
	}
}
