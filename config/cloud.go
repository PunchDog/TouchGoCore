package config

import "time"

// 以下方法是 WhatsappCloudConfig 的「缺省值」口径：纯读取、不写日志，
// 与迁移前 whatsappbiz.Config 的 effective* 方法保持一致。所有方法都对 nil
// 接收者安全——外层 currentCloud 在未配置时返回 nil，调用方仍可调这些方法取缺省。

// EffectiveBaseURL 补默认 Graph API 基址。
func (c *WhatsappCloudConfig) EffectiveBaseURL() string {
	if c == nil || c.BaseURL == "" {
		return "https://graph.facebook.com"
	}
	return c.BaseURL
}

// EffectiveAPIVersion 补默认 API 版本。
func (c *WhatsappCloudConfig) EffectiveAPIVersion() string {
	if c == nil || c.APIVersion == "" {
		return "v23.0"
	}
	return c.APIVersion
}

// CodeTTL 补默认验证码有效期（CodeTTLSec<=0 → 5 分钟）。
func (c *WhatsappCloudConfig) CodeTTL() time.Duration {
	if c == nil || c.CodeTTLSec <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.CodeTTLSec) * time.Second
}

// BindTemplateName 补默认绑定验证码模板名。
func (c *WhatsappCloudConfig) BindTemplateName() string {
	if c == nil || c.BindTemplate == "" {
		return "bind_otp"
	}
	return c.BindTemplate
}

// OnboardSteps 返回引导步骤；未配置时回落内置默认两步。
func (c *WhatsappCloudConfig) OnboardSteps() []WhatsappOnboardStep {
	if c == nil || len(c.Onboarding) == 0 {
		return defaultOnboardSteps()
	}
	return c.Onboarding
}

// defaultOnboardSteps 是内置默认引导：欢迎选语言 → 是否开启通知。
// 内容与原 whatsappbiz 的 steps 变量一致。
func defaultOnboardSteps() []WhatsappOnboardStep {
	return []WhatsappOnboardStep{
		{
			Body:    "欢迎使用！点击下方按钮选择语言开始引导。",
			Buttons: []WhatsappOnboardButton{{ID: "lang_zh", Title: "中文"}, {ID: "lang_en", Title: "English"}},
		},
		{
			Body:    "已记录你的选择，是否开启消息通知？",
			Buttons: []WhatsappOnboardButton{{ID: "notify_on", Title: "开启"}, {ID: "notify_off", Title: "暂不"}},
		},
	}
}
