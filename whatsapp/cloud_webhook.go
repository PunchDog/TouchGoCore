package whatsapp

// 入站 Webhook：验证挑战握手、对载荷做 HMAC-SHA256 验签、解析信封并摊平消息/状态。
//
// 注意：Webhook 信封与消息类型在此导出（CloudWebhookEnvelope / CloudInboundMessage 等），
// 以便外部包（如测试脚手架）在 Webhook 处理路径里遍历消息并读取字段。校验/签名/解析的
// 纯函数仍保持包内小写形态，供同包测试直接调用。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// cloudVerifyWebhookChallenge 处理 Webhook 的 GET 验证握手：
// Meta 带 hub.mode=subscribe、hub.verify_token、hub.challenge，
// 验证 token 一致后原样回 challenge。
func cloudVerifyWebhookChallenge(mode, token, challenge, wantToken string) (string, error) {
	if mode != "subscribe" {
		return "", errors.New("webhook 验证失败: hub.mode 不是 subscribe")
	}
	if token != wantToken {
		return "", errors.New("webhook 验证失败: verify_token 不匹配")
	}
	return challenge, nil
}

// cloudComputeSignature 用 appSecret 对原始 body 算 HMAC-SHA256（十六进制小写），
// 与 Meta 在 X-Hub-Signature-256 头里给的 "sha256=..." 比对。
func cloudComputeSignature(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// cloudVerifySignature 校验入站载荷签名；timing-safe 比较，避免长度泄漏。
func cloudVerifySignature(appSecret string, body []byte, signatureHeader string) error {
	if appSecret == "" {
		return errors.New("未配置 app_secret，无法校验 webhook 签名")
	}
	want := cloudComputeSignature(appSecret, body)
	if !hmac.Equal([]byte(want), []byte(signatureHeader)) {
		return errors.New("webhook 签名校验失败：载荷可能被篡改")
	}
	return nil
}

// CloudWebhookEnvelope 是入站 webhook 信封。
type CloudWebhookEnvelope struct {
	Object string              `json:"object"`
	Entry  []CloudWebhookEntry `json:"entry"`
}

// CloudWebhookEntry 是信封里的一条 change 入口。
type CloudWebhookEntry struct {
	ID      string               `json:"id"`
	Changes []CloudWebhookChange `json:"changes"`
}

// CloudWebhookChange 是入口里的一次变更（字段 + 值）。
type CloudWebhookChange struct {
	Field string            `json:"field"`
	Value CloudWebhookValue `json:"value"`
}

// CloudWebhookValue 是一次变更携带的业务值（联系人/消息/状态）。
type CloudWebhookValue struct {
	MessagingProduct string `json:"messaging_product"`
	Metadata         struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		PhoneNumberID      string `json:"phone_number_id"`
	} `json:"metadata"`
	Contacts []CloudWebhookContact `json:"contacts"`
	Messages []CloudInboundMessage `json:"messages"`
	Statuses []CloudStatusUpdate   `json:"statuses"`
}

// CloudWebhookContact 是消息来源联系人。
type CloudWebhookContact struct {
	Profile struct {
		Name string `json:"name"`
	} `json:"profile"`
	WaID string `json:"wa_id"`
}

// CloudInboundMessage 是一条入站消息。Text 与 Interactive 至少其一非空。
type CloudInboundMessage struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"` // text / interactive / ...
	Text      *struct {
		Body string `json:"body"`
	} `json:"text,omitempty"`
	Interactive *struct {
		Type        string `json:"type"` // button_reply
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply,omitempty"`
	} `json:"interactive,omitempty"`
}

// CloudStatusUpdate 是消息状态回执（sent / delivered / read / failed）。
type CloudStatusUpdate struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Timestamp   string `json:"timestamp"`
	RecipientID string `json:"recipient_id"`
}

// cloudParseWebhook 把原始 body 解析成信封；不校验签名（签名由 cloudVerifySignature 单独做）。
func cloudParseWebhook(body []byte) (*CloudWebhookEnvelope, error) {
	var env CloudWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("webhook 载荷不可解析: %w", err)
	}
	return &env, nil
}

// CollectMessages 把信封里所有 messages 摊平，便于业务处理。
func (e *CloudWebhookEnvelope) CollectMessages() []CloudInboundMessage {
	var out []CloudInboundMessage
	for _, entry := range e.Entry {
		for _, change := range entry.Changes {
			out = append(out, change.Value.Messages...)
		}
	}
	return out
}

// CollectStatuses 把信封里所有 statuses 摊平。
func (e *CloudWebhookEnvelope) CollectStatuses() []CloudStatusUpdate {
	var out []CloudStatusUpdate
	for _, entry := range e.Entry {
		for _, change := range entry.Changes {
			out = append(out, change.Value.Statuses...)
		}
	}
	return out
}

// WhatsappCloudVerifyWebhook 经 Cloud 能力处理 Webhook 验证握手；未启动返回明确错误。
func WhatsappCloudVerifyWebhook(mode, token, challenge string) (string, error) {
	c, err := currentCloud()
	if err != nil {
		return "", err
	}
	return cloudVerifyWebhookChallenge(mode, token, challenge, c.cfg.VerifyToken)
}

// WhatsappCloudVerifyWebhookSignature 经 Cloud 能力校验入站签名；未启动返回明确错误。
func WhatsappCloudVerifyWebhookSignature(body []byte, sigHeader string) error {
	c, err := currentCloud()
	if err != nil {
		return err
	}
	return cloudVerifySignature(c.cfg.AppSecret, body, sigHeader)
}

// WhatsappCloudParseWebhook 解析 Webhook 载荷为信封（不校验签名）。
func WhatsappCloudParseWebhook(body []byte) (*CloudWebhookEnvelope, error) {
	return cloudParseWebhook(body)
}

// WhatsappCloudCollectMessages 从信封摊平所有消息。
func WhatsappCloudCollectMessages(env *CloudWebhookEnvelope) []CloudInboundMessage {
	if env == nil {
		return nil
	}
	return env.CollectMessages()
}

// WhatsappCloudCollectStatuses 从信封摊平所有状态回执。
func WhatsappCloudCollectStatuses(env *CloudWebhookEnvelope) []CloudStatusUpdate {
	if env == nil {
		return nil
	}
	return env.CollectStatuses()
}
