package whatsapp

// 本文件是 QA 独立验证补充用例，覆盖迁移落地 Checklist 第 7 条里现有用例未触及的
// 边界/错误路径：Cloud 半残配置不启动、配置驱动的自定义引导步数、Webhook 空密钥验签。
// 不修改任何业务源码，仅新增测试。

import (
	"context"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
)

// TestCloudPartialConfigDoesNotStart 验证 fail-closed 的半残配置：
// 只配 PhoneNumberID 缺 AccessToken（或反之）时，cloudReady 应为 false，
// WhatsappStart 不启动 Cloud（cloud 快照保持 nil），门面返回非 nil error 而非 panic。
func TestCloudPartialConfigDoesNotStart(t *testing.T) {
	// 情况一：只有 PhoneNumberID，缺 AccessToken
	WhatsappStop(nil)
	cfg1 := &config.Cfg{Whatsapp: &config.WhatsappConfig{Cloud: &config.WhatsappCloudConfig{PhoneNumberID: "PNID"}}}
	WhatsappStart(corectx.WithCfg(context.Background(), cfg1))
	if c := WhatsappCloud(); c != nil {
		t.Fatalf("缺 AccessToken 时 WhatsappCloud 应为 nil，实得 %+v", c)
	}
	if _, err := WhatsappCloudSendText(context.Background(), "86138", "hi"); err == nil {
		t.Fatal("Cloud 未启动时发送文本应返回 error，而非 nil")
	}
	if cloudReady(cfg1.Whatsapp.Cloud) {
		t.Fatal("cloudReady 在缺 AccessToken 时应为 false")
	}

	// 情况二：只有 AccessToken，缺 PhoneNumberID（反之）
	WhatsappStop(nil)
	cfg2 := &config.Cfg{Whatsapp: &config.WhatsappConfig{Cloud: &config.WhatsappCloudConfig{AccessToken: "tok"}}}
	WhatsappStart(corectx.WithCfg(context.Background(), cfg2))
	if c := WhatsappCloud(); c != nil {
		t.Fatalf("缺 PhoneNumberID 时 WhatsappCloud 应为 nil，实得 %+v", c)
	}
	if _, err := WhatsappCloudSendTemplate(context.Background(), "86138", "bind_otp", "en_US", "123456", "5"); err == nil {
		t.Fatal("Cloud 未启动时发送模板应返回 error，而非 nil")
	}
	if _, err := WhatsappCloudSendOnboardingStep(context.Background(), "86138", 0); err == nil {
		t.Fatal("Cloud 未启动时发送引导步骤应返回 error，而非 nil")
	}
	if cloudReady(cfg2.Whatsapp.Cloud) {
		t.Fatal("cloudReady 在缺 PhoneNumberID 时应为 false")
	}
}

// TestOnboardingThreeStepsFromConfig 验证「配置统一进 JSON」真的生效：
// 当 WhatsappCloudConfig.Onboarding 配 3 步时，按下标 2 应发出第 3 步的文案与按钮，
// 下标 3 越界报错且不发请求（内置默认只有两步，见 TestOnboardingStepRender）。
func TestOnboardingThreeStepsFromConfig(t *testing.T) {
	srv, bodies := newFakeGraph(t)
	steps := []config.WhatsappOnboardStep{
		{Body: "步骤1：选择语言", Buttons: []config.WhatsappOnboardButton{{ID: "b1", Title: "中文"}}},
		{Body: "步骤2：开启通知", Buttons: []config.WhatsappOnboardButton{{ID: "b2", Title: "开启"}}},
		{Body: "步骤3：完成引导", Buttons: []config.WhatsappOnboardButton{{ID: "b3", Title: "完成"}}},
	}
	cfg := cloudTestCfg(t, srv.URL)
	cfg.Whatsapp.Cloud.Onboarding = steps
	startWith(t, cfg)

	if _, err := WhatsappCloudSendOnboardingStep(context.Background(), "86138", 2); err != nil {
		t.Fatalf("配置 3 步时下标 2 应可发送: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("应只发出 1 条请求，实得 %d", len(*bodies))
	}
	if !strings.Contains((*bodies)[0], "步骤3：完成引导") || !strings.Contains((*bodies)[0], `"id":"b3"`) {
		t.Fatalf("应按配置 JSON 渲染第 3 步: %s", (*bodies)[0])
	}

	if _, err := WhatsappCloudSendOnboardingStep(context.Background(), "86138", 3); err == nil {
		t.Fatal("配置 3 步时下标 3 应越界报错")
	}
	if len(*bodies) != 1 {
		t.Fatalf("越界时不应追加请求，实得 %d 条", len(*bodies))
	}
}

// TestCloudWebhookEmptySecretFails 验证门面层：AppSecret 为空时验签必须失败
// （不依赖纯函数，走 WhatsappCloudVerifyWebhookSignature 门面）。
func TestCloudWebhookEmptySecretFails(t *testing.T) {
	srv, _ := newFakeGraph(t)
	cfg := cloudTestCfg(t, srv.URL)
	cfg.Whatsapp.Cloud.AppSecret = "" // 清空密钥
	startWith(t, cfg)

	body := []byte(`{"object":"whatsapp_business_account"}`)
	if err := WhatsappCloudVerifyWebhookSignature(body, "sha256=deadbeef"); err == nil {
		t.Fatal("AppSecret 为空时门面验签应失败")
	}
}
