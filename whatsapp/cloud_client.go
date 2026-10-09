package whatsapp

// 出向消息体：只定义 Cloud API /messages 端点实际需要的字段，不做多余抽象。
// 这些结构全部是 Graph API 请求与回执形态，对应迁移前 whatsappbiz 的 types.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"touchgocore/config"
)

// cloudTextPayload 是 /messages 端点的统一请求包络。
type cloudTextPayload struct {
	MessagingProduct string                `json:"messaging_product"`
	RecipientType    string                `json:"recipient_type"`
	To               string                `json:"to"`
	Type             string                `json:"type"`
	Text             *cloudTextBody        `json:"text,omitempty"`
	Template         *cloudTemplateBody    `json:"template,omitempty"`
	Interactive      *cloudInteractiveBody `json:"interactive,omitempty"`
}

type cloudTextBody struct {
	// PreviewURL 由配置段 Whatsapp.Cloud.preview_url 驱动（见 SendText），不是调用方可
	// 逐个指定的参数；false 时因 omitempty 整键不出现在报文里，等同 Meta 的缺省行为。
	PreviewURL bool   `json:"preview_url,omitempty"`
	Body       string `json:"body"`
}

type cloudTemplateBody struct {
	Name       string                   `json:"name"`
	Language   *cloudTemplateLanguage   `json:"language"`
	Components []cloudTemplateComponent `json:"components,omitempty"`
}

type cloudTemplateLanguage struct {
	Code string `json:"code"`
}

type cloudTemplateComponent struct {
	Type       string               `json:"type"`
	Parameters []cloudTemplateParam `json:"parameters,omitempty"`
}

type cloudTemplateParam struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type cloudInteractiveBody struct {
	Type   string                  `json:"type"` // "button"
	Body   *cloudInteractiveText   `json:"body"`
	Action *cloudInteractiveAction `json:"action"`
}

type cloudInteractiveText struct {
	Text string `json:"text"`
}

type cloudInteractiveAction struct {
	Buttons []cloudInteractiveButton `json:"buttons"`
}

type cloudInteractiveButton struct {
	Type  string            `json:"type"` // "reply"
	Reply *cloudButtonReply `json:"reply"`
}

type cloudButtonReply struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// cloudGraphResponse 是 /messages 端点的回执；Error 非空表示业务失败。
type cloudGraphResponse struct {
	MessagingProduct string `json:"messaging_product"`
	Contacts         []struct {
		WaID string `json:"wa_id"`
	} `json:"contacts"`
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error *cloudGraphError `json:"error,omitempty"`
}

type cloudGraphError struct {
	Message      string `json:"message"`
	Code         int    `json:"code"`
	ErrorSubcode int    `json:"error_subcode"`
}

// CloudButton 是 SendInteractiveButtons 的入参。
type CloudButton struct {
	ID    string
	Title string
}

// CloudAPIError 是 Cloud API 返回的错误（含业务码，便于上层按 code 处理，如 131009 参数错误）。
type CloudAPIError struct {
	StatusCode int
	Code       int
	Message    string
}

func (e *CloudAPIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("whatsapp api 错误 (http=%d, code=%d): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("whatsapp api 错误 (http=%d): %s", e.StatusCode, e.Message)
}

// cloudClient 是 WhatsApp Business Cloud API 的最小客户端。
//
// 只负责把请求送到 Graph API 并把回执读成 cloudGraphResponse，不碰业务状态——本包没有
// 任何按手机号的状态，绑定/引导进度归调用方。
type cloudClient struct {
	cfg        *config.WhatsappCloudConfig
	httpClient *http.Client
}

// cloudSendTimeout 是出站发送的兜底超时：调用方的 ctx 仍是主控制手段，这里只保证 ctx
// 没带 deadline 时不会无限挂住一个进程级共享 client。
const cloudSendTimeout = 15 * time.Second

// newCloudClient 用配置构造客户端；httpClient 自带兜底超时（测试用 httptest 真实 server
// 直接命中，无需注入）。
func newCloudClient(cfg *config.WhatsappCloudConfig) *cloudClient {
	return &cloudClient{cfg: cfg, httpClient: &http.Client{Timeout: cloudSendTimeout}}
}

// SendText 发送一条纯文本消息；to 为目标号码（E.164，纯数字）。返回 wamid。
//
// 是否带链接预览只读配置段 preview_url（cfg.PreviewURL），签名不带开关：Meta 把该参数
// 定为 per-message，但本包按「一处配置一处行为」收口，调用方要差异就分实例或后补参数。
func (c *cloudClient) SendText(ctx context.Context, to, body string) (string, error) {
	return c.send(ctx, &cloudTextPayload{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               to,
		Type:             "text",
		Text:             &cloudTextBody{Body: body, PreviewURL: c.cfg.PreviewURL},
	})
}

// SendTemplate 发送一条模板消息（用于 24h 窗口之外的主动触达，如绑定验证码）。
// lang 为模板语言码（如 en / zh_CN），params 依次填入模板的 {{1}} {{2}} ...
func (c *cloudClient) SendTemplate(ctx context.Context, to, name, lang string, params ...string) (string, error) {
	comp := cloudTemplateComponent{Type: "body"}
	if len(params) > 0 {
		comp.Parameters = make([]cloudTemplateParam, 0, len(params))
		for _, p := range params {
			comp.Parameters = append(comp.Parameters, cloudTemplateParam{Type: "text", Text: p})
		}
	}
	return c.send(ctx, &cloudTextPayload{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               to,
		Type:             "template",
		Template: &cloudTemplateBody{
			Name:       name,
			Language:   &cloudTemplateLanguage{Code: lang},
			Components: []cloudTemplateComponent{comp},
		},
	})
}

// SendInteractiveButtons 发送一条带「回复按钮」的交互式消息（用于引导流程）。
// 按钮的 id 与 title 受 Meta 限制（id <= 256 字节，title <= 20 字符），调用方自行保证。
func (c *cloudClient) SendInteractiveButtons(ctx context.Context, to, body string, buttons []CloudButton) (string, error) {
	if len(buttons) == 0 {
		return "", errors.New("交互式消息至少需要一个按钮")
	}
	if len(buttons) > 3 {
		return "", errors.New("交互式消息最多 3 个按钮")
	}
	act := &cloudInteractiveAction{Buttons: make([]cloudInteractiveButton, 0, len(buttons))}
	for _, b := range buttons {
		act.Buttons = append(act.Buttons, cloudInteractiveButton{
			Type:  "reply",
			Reply: &cloudButtonReply{ID: b.ID, Title: b.Title},
		})
	}
	return c.send(ctx, &cloudTextPayload{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               to,
		Type:             "interactive",
		Interactive: &cloudInteractiveBody{
			Type:   "button",
			Body:   &cloudInteractiveText{Text: body},
			Action: act,
		},
	})
}

// send 是底层发送：校验凭证 → 序列化 → POST Graph API → 解析回执 → 归一错误。
func (c *cloudClient) send(ctx context.Context, p *cloudTextPayload) (string, error) {
	if c.cfg.AccessToken == "" {
		return "", errors.New("未配置 access_token")
	}
	if c.cfg.PhoneNumberID == "" {
		return "", errors.New("未配置 phone_number_id")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("消息体序列化失败: %w", err)
	}
	endpoint := fmt.Sprintf("%s/%s/%s/messages", c.cfg.EffectiveBaseURL(), c.cfg.EffectiveAPIVersion(), c.cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("发送消息请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var gar cloudGraphResponse
	if err := json.Unmarshal(body, &gar); err != nil {
		return "", fmt.Errorf("回执不可解析 (status=%d): %w", resp.StatusCode, err)
	}
	if gar.Error != nil && gar.Error.Message != "" {
		return "", &CloudAPIError{StatusCode: resp.StatusCode, Code: gar.Error.Code, Message: gar.Error.Message}
	}
	if resp.StatusCode >= 300 {
		return "", &CloudAPIError{StatusCode: resp.StatusCode, Message: string(body)}
	}
	if len(gar.Messages) == 0 {
		return "", errors.New("发送成功但未返回消息 ID")
	}
	return gar.Messages[0].ID, nil
}

// WhatsappCloudSendText 经 Cloud 能力发送纯文本；未启动返回明确错误。
//
// 签名冻结，行为随配置：报文中是否带 preview_url 取决于启动时的 Whatsapp.Cloud.preview_url。
func WhatsappCloudSendText(ctx context.Context, to, body string) (string, error) {
	c, err := currentCloud()
	if err != nil {
		return "", err
	}
	return c.client.SendText(ctx, to, body)
}

// WhatsappCloudSendTemplate 经 Cloud 能力发送模板消息；未启动返回明确错误。
//
// 契约冻结：本签名与「返回空 message id 必伴随非 nil error」（见 send() 的各失败路径）是
// 下游 wa.Provider meta 实现的直读面，改名/改语义须先在边界交接档回话。to 由调用方归一化
// （见 WhatsappCloudNormalizePhone），本包不代为清洗。
func WhatsappCloudSendTemplate(ctx context.Context, to, name, lang string, params ...string) (string, error) {
	c, err := currentCloud()
	if err != nil {
		return "", err
	}
	return c.client.SendTemplate(ctx, to, name, lang, params...)
}

// WhatsappCloudSendInteractiveButtons 经 Cloud 能力发送交互式按钮消息；未启动返回明确错误。
func WhatsappCloudSendInteractiveButtons(ctx context.Context, to, body string, buttons []CloudButton) (string, error) {
	c, err := currentCloud()
	if err != nil {
		return "", err
	}
	return c.client.SendInteractiveButtons(ctx, to, body, buttons)
}
