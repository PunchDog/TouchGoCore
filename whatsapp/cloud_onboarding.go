package whatsapp

// 引导流程的「渲染 + 发送」件：按 config.WhatsappCloudConfig.OnboardSteps() 的步骤定义，
// 把指定步骤发成一条带回复按钮的交互式消息。
//
// 边界口径：本包不持有「进行到第几步」。进度归属与持久化由业务方 store 持有——调用方自取
// cfg.OnboardSteps() 得总步数，自己记每个号码走到哪一步，再按步号调本件；按钮 id 的分支
// 语义同样归业务方。

import (
	"context"
	"fmt"

	"touchgocore/config"
)

// cloudSendOnboardingStep 发送第 idx 步（从 0 起）引导消息；to 需是调用方归一化后的号码。
func cloudSendOnboardingStep(ctx context.Context, c *cloudClient, cfg *config.WhatsappCloudConfig, to string, idx int) (string, error) {
	steps := cfg.OnboardSteps()
	if idx < 0 || idx >= len(steps) {
		return "", fmt.Errorf("引导步骤越界: %d（共 %d 步）", idx, len(steps))
	}
	s := steps[idx]
	btns := make([]CloudButton, 0, len(s.Buttons))
	for _, b := range s.Buttons {
		btns = append(btns, CloudButton{ID: b.ID, Title: b.Title})
	}
	return c.SendInteractiveButtons(ctx, to, s.Body, btns)
}

// WhatsappCloudSendOnboardingStep 经 Cloud 能力发送第 idx 步引导消息；未启动返回明确错误。
func WhatsappCloudSendOnboardingStep(ctx context.Context, to string, idx int) (string, error) {
	c, err := currentCloud()
	if err != nil {
		return "", err
	}
	return cloudSendOnboardingStep(ctx, c.client, c.cfg, to, idx)
}
