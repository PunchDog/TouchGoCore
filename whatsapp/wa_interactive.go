package whatsapp

// 本文件是「CtaURL 按钮消息」发送面：正文（可选图片作 header）+ 一个点击跳网页的按钮。
//
// 【为什么单开一文件】WhatsApp 没有「后挂按钮」这种消息形态：带按钮的消息是
// InteractiveMessage（native_flow / cta_url），图文形态下图片是它的 header，
// 正文是它的 body。组装比一条 Conversation 复杂得多（图片还要先上传拿
// mediaKey/directPath），但失败面很小 —— 服务端要么受理要么拒，不会出现半条消息。
//
// 【biz 节点是必须的】whatsmeow 的 SendMessage 只负责把 protobuf 塞进 <message>，
// 它内置的 biz 节点只覆盖 ButtonsMessage/ListMessage 等旧形态（send.go 的
// getButtonTypeFromMessage）。native_flow 按钮要额外挂：
//
//	<biz><interactive type="native_flow" v="1"><native_flow v="9" name="mixed"/></interactive></biz>
//
// 缺它时服务端不接受这条消息（或对端渲染成「此版本不支持」）。这里通过
// SendRequestExtra.AdditionalNodes 追加，不魔改库。
//
// 【发不出去就降级，不整批失败】个人号（companion device）对交互消息的支持不是
// 官方契约，个别账号/版本会被服务端拒。调用方（wa_client.go 的 waSendContent）
// 捕获失败后把按钮内容摊成「正文 + 按钮字样 + 链接」的普通文本/图文再发一次：
// 收件人至少还能点到链接，而不是整批推送停在这一条上。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"touchgocore/vars"
)

// OutboundInteractive 一条待发的 CTA 按钮消息。
//
// Body 是正文（图文形态下是图片下方说明）；ImageURL 非空时作 header 图片，
// 由本包下载后上传；ButtonText 是按钮字样（WhatsApp 上限 20 字符），
// ButtonURL 是点击后打开的网页（http/https）。
type OutboundInteractive struct {
	Body       string
	ImageURL   string
	ButtonText string
	ButtonURL  string
}

// waCtaButtonName 是 native_flow 里「跳转网页」按钮的固定 name。
const waCtaButtonName = "cta_url"

// waLinkFallbackBody 把按钮摊成普通文本的最后一行：`正文 + 空行 + 字样 + 换行 + 链接`。
//
// 降级路径专用：WhatsApp 会把裸 URL 自动识别成可点链接，所以收件人仍能跳转，
// 只是少了按钮外观。字样为空时只追加链接（成对校验在业务侧，这里是兜底）。
func waLinkFallbackBody(body, text, url string) string {
	body = strings.TrimSpace(body)
	text = strings.TrimSpace(text)
	url = strings.TrimSpace(url)
	switch {
	case text == "":
		return strings.TrimSpace(body + "\n" + url)
	case body == "":
		return text + "\n" + url
	default:
		return body + "\n\n" + text + "\n" + url
	}
}

// waInteractiveBizNodes native_flow 按钮消息必须随消息一起发的 biz 节点。
func waInteractiveBizNodes() []waBinary.Node {
	return []waBinary.Node{{
		Tag: "biz",
		Content: []waBinary.Node{{
			Tag:   "interactive",
			Attrs: waBinary.Attrs{"type": "native_flow", "v": "1"},
			Content: []waBinary.Node{{
				Tag:   "native_flow",
				Attrs: waBinary.Attrs{"v": "9", "name": "mixed"},
			}},
		}},
	}}
}

// buildInteractiveMessage 组装 InteractiveMessage（图片 header + 正文 + cta_url 按钮）。
func (c *waConn) buildInteractiveMessage(ctx context.Context, m OutboundInteractive) (*waE2E.Message, error) {
	buttonURL := strings.TrimSpace(m.ButtonURL)
	buttonText := strings.TrimSpace(m.ButtonText)
	if buttonURL == "" || buttonText == "" {
		return nil, fmt.Errorf("按钮字样与跳转网页都必须非空")
	}
	// 纯文本按钮消息不带 header（空 header 对象会被对端当成「有头但没内容」）。
	var header *waE2E.InteractiveMessage_Header
	if imageURL := strings.TrimSpace(m.ImageURL); imageURL != "" {
		data, mime, err := waFetchImage(ctx, imageURL)
		if err != nil {
			return nil, err
		}
		up, err := c.cli.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return nil, fmt.Errorf("图片上传失败: %w", err)
		}
		img := &waE2E.ImageMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			Mimetype:      proto.String(mime),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			MediaKey:      up.MediaKey,
			FileLength:    proto.Uint64(up.FileLength),
		}
		if thumb, w, h := waImageThumb(data); thumb != nil {
			img.JPEGThumbnail = thumb
			img.Width = proto.Uint32(w)
			img.Height = proto.Uint32(h)
		} else {
			vars.Warning("WA 按钮消息的 header 图缩略图生成失败（webp 或数据损坏），本条按无预览发送")
		}
		header = &waE2E.InteractiveMessage_Header{
			HasMediaAttachment: proto.Bool(true),
			Media:              &waE2E.InteractiveMessage_Header_ImageMessage{ImageMessage: img},
		}
	}
	// buttonParamsJSON 是 WhatsApp 约定的紧凑 JSON 字符串，键名固定。
	params, err := json.Marshal(map[string]string{
		"display_text": buttonText,
		"url":          buttonURL,
		"merchant_url": buttonURL,
	})
	if err != nil {
		return nil, fmt.Errorf("按钮参数序列化失败: %w", err)
	}
	return &waE2E.Message{
		InteractiveMessage: &waE2E.InteractiveMessage{
			Header: header,
			Body:   &waE2E.InteractiveMessage_Body{Text: proto.String(m.Body)},
			InteractiveMessage: &waE2E.InteractiveMessage_NativeFlowMessage_{
				NativeFlowMessage: &waE2E.InteractiveMessage_NativeFlowMessage{
					Buttons: []*waE2E.InteractiveMessage_NativeFlowMessage_NativeFlowButton{{
						Name:             proto.String(waCtaButtonName),
						ButtonParamsJSON: proto.String(string(params)),
					}},
					MessageVersion: proto.Int32(1),
				},
			},
		},
	}, nil
}

// SendInteractive 发送一条 CTA 按钮消息（可选图片 header），返回消息 id 与服务器时间戳。
func (c *waConn) SendInteractive(ctx context.Context, to string, m OutboundInteractive) (string, time.Time, error) {
	jid, err := types.ParseJID(to)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("收件人 JID 不可解析: %w", err)
	}
	msg, err := c.buildInteractiveMessage(ctx, m)
	if err != nil {
		return "", time.Time{}, err
	}
	nodes := waInteractiveBizNodes()
	resp, err := c.cli.SendMessage(ctx, jid, msg, whatsmeow.SendRequestExtra{AdditionalNodes: &nodes})
	if err != nil {
		return "", time.Time{}, err
	}
	return string(resp.ID), resp.Timestamp, nil
}
