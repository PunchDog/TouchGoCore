package whatsapp

// 本文件聚合 WhatsApp Business Cloud API（Meta Graph API 直连）的框架侧能力：出向消息
// 客户端、Webhook 协议件、分享与客服窗口纯函数、引导步骤渲染。它们不依赖 pay_sdks，
// 凭证由 config.WhatsappCloudConfig 直接持有。生命周期与 fund/login 同口径：用 atomic.Pointer
// 存只读快照，未启动返回明确错误而不是 panic，便于整机在只配了 Cloud 或只配了资金链路时各起各的。
//
// 边界口径：本包不持有任何按手机号的可变状态。绑定账务（验证码存储与摘要、冷却、预算、
// 绑定态跃迁）和引导进度都归业务方 store；框架只发信、只解协议。需要演示这种状态怎么落，
// 看 cmd/whatsappbiz-test。

import (
	"context"
	"errors"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/vars"
)

// whatsappCloud 是 Cloud API 框架侧能力的聚合快照；WhatsappStart 写入，入口经 WhatsappCloud() 读取。
type whatsappCloud struct {
	cfg     *config.WhatsappCloudConfig
	client  *cloudClient
	support *cloudSupportAgent
}

// cloud 是 Cloud 能力的只读快照指针；nil 表示未启动。
var cloud atomic.Pointer[whatsappCloud]

// WhatsappCloud 返回当前 Cloud 能力快照；未启动时为 nil。
func WhatsappCloud() *whatsappCloud { return cloud.Load() }

// currentCloud 取 Cloud 能力快照；未启动时返回明确错误而不是 panic。
func currentCloud() (*whatsappCloud, error) {
	c := cloud.Load()
	if c == nil {
		return nil, errors.New("whatsapp Cloud 未启动（检查 whatsapp.cloud 段是否配置了 access_token / phone_number_id）")
	}
	return c, nil
}

// cloudReady 判断 Cloud 配置是否足以启动（AccessToken 与 PhoneNumberID 都不能空）。
func cloudReady(c *config.WhatsappCloudConfig) bool {
	return c != nil && c.AccessToken != "" && c.PhoneNumberID != ""
}

// startCloud 按配置装配 Cloud 能力。引用缺失只记日志返回，不阻断整机启动。
func startCloud(all *config.Cfg, wcfg *config.WhatsappCloudConfig) {
	if !cloudReady(wcfg) {
		vars.Info("不启动 WhatsApp Cloud")
		return
	}
	cli := newCloudClient(wcfg)
	support := NewCloudSupportAgent(cli)
	cloud.Store(&whatsappCloud{
		cfg:     wcfg,
		client:  cli,
		support: support,
	})
	vars.Info("WhatsApp Cloud 已就绪: phone_number_id=%s", wcfg.PhoneNumberID)
}

// stopCloud 摘掉 Cloud 能力快照；可重复调用。
func stopCloud() { cloud.Store(nil) }

// SupportReply 在 24h 客服窗口内回一条文本。
func (w *whatsappCloud) SupportReply(ctx context.Context, to, body string, withinWindow bool) (string, error) {
	return w.support.Reply(ctx, to, body, withinWindow)
}
