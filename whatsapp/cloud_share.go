package whatsapp

// 分享深链：把文本或「指定号码 + 文本」拼成 wa.me 链接。纯函数，无外部依赖。

import (
	"net/url"
	"strings"
)

// cloudShareText 生成「不带指定号码、仅带文本」的分享深链。
//
// 点击后打开用户的 WhatsApp 并预填文本，由用户选择发给谁。文本会做 URL 编码。
// wa.me 本身没有硬长度上限，但过长的预填文本在部分客户端会被静默丢弃，调用方应自行控制。
func cloudShareText(text string) string {
	return "https://wa.me/?text=" + url.QueryEscape(text)
}

// cloudShareTo 生成「直接发给某个号码」的分享深链。
//
// phone 接受任意格式（空格、+、括号、连字符），统一清洗成纯数字；
// 号码应以国家码开头（如 8613800138000），不带 + 也可。
func cloudShareTo(phone, text string) string {
	cleaned := cloudNormalizePhone(phone)
	if text == "" {
		return "https://wa.me/" + cleaned
	}
	return "https://wa.me/" + cleaned + "?text=" + url.QueryEscape(text)
}

// cloudNormalizePhone 清洗并导出手机号，供外部（如测试服务）复用同一口径。
func cloudNormalizePhone(p string) string { return cloudCleanPhone(p) }

// cloudCleanPhone 去掉所有非数字字符，得到 E.164 纯数字形态。
func cloudCleanPhone(phone string) string {
	var b strings.Builder
	b.Grow(len(phone))
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// WhatsappCloudShareText 包级门面：生成纯文本分享深链。
func WhatsappCloudShareText(text string) string { return cloudShareText(text) }

// WhatsappCloudShareTo 包级门面：生成指定号码分享深链。
func WhatsappCloudShareTo(phone, text string) string { return cloudShareTo(phone, text) }

// WhatsappCloudNormalizePhone 包级门面：清洗并导出手机号。
func WhatsappCloudNormalizePhone(p string) string { return cloudNormalizePhone(p) }
