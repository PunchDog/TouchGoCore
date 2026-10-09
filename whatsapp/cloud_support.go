package whatsapp

// 客服与帮助：接收用户入站消息，在 24 小时客服窗口内自由回复；窗口外只能发模板。
// 窗口以最后一次用户消息的时间戳为起点。

import (
	"context"
	"errors"
	"time"
)

// cloudSupportAgent 处理「客服与帮助」。
type cloudSupportAgent struct {
	client *cloudClient
}

// NewCloudSupportAgent 构造客服代理（可选，供测试注入客户端）。
func NewCloudSupportAgent(client *cloudClient) *cloudSupportAgent {
	return &cloudSupportAgent{client: client}
}

// cloudCustomerServiceWindow 是客服窗口长度：最后一次用户消息起 24 小时内可发非模板消息。
const cloudCustomerServiceWindow = 24 * time.Hour

// cloudWithinWindow 判断给定用户消息时间戳（Unix 秒）是否仍在 24h 客服窗口内。
func cloudWithinWindow(msgTimestampUnix int64, now time.Time) bool {
	if msgTimestampUnix <= 0 {
		return false
	}
	return now.Sub(time.Unix(msgTimestampUnix, 0)) <= cloudCustomerServiceWindow
}

// Reply 在窗口内回一条文本；窗口外返回明确错误（此时应改走 SendTemplate）。
func (s *cloudSupportAgent) Reply(ctx context.Context, to, body string, withinWindow bool) (string, error) {
	if to == "" || body == "" {
		return "", errors.New("客服回复失败: 接收号或内容为空")
	}
	if !withinWindow {
		return "", errors.New("客服回复失败: 已超出 24h 客服窗口，请改用模板消息")
	}
	return s.client.SendText(ctx, to, body)
}

// WhatsappCloudSupportReply 经 Cloud 能力在窗口内回复；未启动返回明确错误。
func WhatsappCloudSupportReply(to, body string, withinWindow bool) (string, error) {
	c, err := currentCloud()
	if err != nil {
		return "", err
	}
	return c.support.Reply(context.Background(), to, body, withinWindow)
}

// WhatsappCloudWithinWindow 判断时间戳是否仍在 24h 客服窗口内。
func WhatsappCloudWithinWindow(msgTimestampUnix int64, now time.Time) bool {
	return cloudWithinWindow(msgTimestampUnix, now)
}
