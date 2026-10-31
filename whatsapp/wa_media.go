package whatsapp

// 本文件是「图文混排」发送面：一条图片消息 = 图片本体 + 说明文字（caption）。
//
// 为什么单独成文：发文本只走 Conversation 一种 protobuf 形态，发图片要走
// 拉取 → 加密上传 → 组装 ImageMessage → SendMessage 四步，且每一步都有自己的
// 失败面（源站不可达 / 体积超限 / 类型不认识 / 上传被拒 / 服务端不回消息 id）。
// 把这些塞进 wa_client.go 会把发信门面里「一条文本」的主线埋掉。
//
// 口径（与发文本逐条对齐，改动前先读）：
//   - 契约同为「空 id ⇔ 非 nil error」：拿不到消息 id 就是没受理。
//   - 空正文**不是**错误：图文消息允许只有图（Caption 为空），但图与文同时为空
//     仍然是错误（那是「发了个空气」，下游无法解释）。
//   - 图片由本包自己下载：业务方只给直链（后台静态目录），不传二进制。
//     直链只接受 http/https，且下载有超时与体积上限 —— 上传前必须整份读进内存
//     算 SHA256，流式读没有意义，所以上限是硬闸而不是建议值。
//   - 缩略图（JPEGThumbnail）尽量给：没有缩略图时对方客户端要等整图下载完才显示
//     预览，图文广告的第一眼观感差别很大。给不出来（webp / 解码失败）不阻断发送，
//     只记 Warning —— 图片本体是必须的，缩略图是体验。
//   - 防封闸照旧：闸看的是说明文字（与发文本同一口径），闸拦下就整条不发，
//     不会出现「图发出去了、文没跟上」的半条消息。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // 注册 png 解码器（image.Decode 靠它）
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"touchgocore/vars"
)

// OutboundImage 一条待发的图片消息。
//
// URL 是图片直链（http/https），由本包下载后上传给 WhatsApp；
// Caption 是图片下方的说明文字（可空，图文混排时就是那段长文案）。
// MimeType 留空时按内容嗅探，填了也只认 image/jpeg、image/png、image/webp 三档。
type OutboundImage struct {
	URL      string
	Caption  string
	MimeType string
}

const (
	// waImageMaxBytes 单图上限 16MB：WhatsApp 图片消息的服务端上限是 16MB，
	// 超过必然被拒；提前拦下能给出「源图太大」这种可操作的错误，而不是上传到
	// 一半收到一个没有上下文的 413。
	waImageMaxBytes int64 = 16 << 20
	// waImageThumbMaxSide 缩略图最长边（像素）。WhatsApp 客户端预览用的是几十像素级
	// 的小图，72 与上游惯例一致；再大只是白白多占消息体积。
	waImageThumbMaxSide = 72
	// waImageFetchTimeout 单图下载预算。后台静态目录是同机房/同域名的直链，
	// 正常是毫秒级；30s 只兜「反代卡住」这类异常。
	waImageFetchTimeout = 30 * time.Second
	// waImageUserAgent 是下载图片时显式声明的 UA。
	//
	// 【为什么必须设（2026-10-30 真机踩坑）】不设时 Go 发的是 "Go-http-client/1.1"，
	// 挂在 Cloudflare 后面的源站会把它当爬虫直接回 403（实测：同一个 URL，
	// 浏览器 UA 拿 404、Go UA 拿 403，且 403 的响应体是 Cloudflare 的
	// "Attention Required!" 拦截页）。这个错误只出现在消息服日志里，后台完全无感，
	// 表现就是「内网发图正常、外网发图 403」—— 内网直连源站不过 CDN，所以看不见。
	//
	// 用常见桌面浏览器 UA 是最小侵入：源站是自家静态目录，没有伪装意图，
	// 只是别让 WAF 把「不是浏览器」当成「是爬虫」。真正的治理仍在 CDN 侧
	// （WAF 放行 /static/ 或按源站 IP 白名单），这一条保证不配也能用。
	waImageUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// 两个注入点：测试整体替换（照 waSessionFactory / waBatchSleep 的换-还原范式）。
var (
	// waImageHTTP 下载图片直链用的客户端。nil Transport ⇒ 默认 Transport，只读
	// HTTP(S)_PROXY 环境变量；显式代理（WhatsappSetProxy / sessions.proxy_url）只管
	// WhatsApp 协议出口（WS + 媒体上传下载），不覆盖这条直链下载——直链通常是业务
	// 自己的静态目录，绕代理反而可能不可达。
	waImageHTTP = &http.Client{Timeout: waImageFetchTimeout}
	// waImageThumb 生成缩略图与像素尺寸；返回 nil 缩略图表示「这张给不出来」。
	waImageThumb = waMakeThumbnail
)

// errWaImageEmpty 是「图和文都是空」的固定文案。
var errWaImageEmpty = errors.New("消息内容为空")

// waFetchImage 下载一张图片，返回字节与嗅探出的 MIME。
//
// 只接受 http/https：后台配的是静态目录直链，出现 file:// 或裸路径说明配置错了，
// 在这里拒绝比让 http.Client 抛一个难懂的错误更清楚。
func waFetchImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, "", errors.New("图片直链为空")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("图片直链不可解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("图片直链只支持 http/https，收到 %q", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("构造图片请求失败: %w", err)
	}
	// UA 与 Accept 都要给：CDN/WAF 常常只要缺一项就当非浏览器处理（见 waImageUserAgent）。
	req.Header.Set("User-Agent", waImageUserAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,*/*;q=0.8")
	resp, err := waImageHTTP.Do(req)
	if err != nil {
		// 错误串带 URL：403/404 的排查第一个问题永远是「它到底在下载哪个地址」。
		return nil, "", fmt.Errorf("下载图片失败 %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 带上 URL 与状态码，并把「可能是 CDN 拦的」写在文案里：这条错误串是
		// 运维唯一能看到的线索，不该只给一个裸的 403。
		return nil, "", fmt.Errorf("下载图片失败: 源站返回 %d（%s）%s",
			resp.StatusCode, rawURL, waFetchImageHint(resp))
	}
	// 多读 1 字节用于判超限：Content-Length 可能缺失或撒谎，只有读出来的字节数算数。
	data, err := io.ReadAll(io.LimitReader(resp.Body, waImageMaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取图片失败: %w", err)
	}
	if int64(len(data)) > waImageMaxBytes {
		return nil, "", fmt.Errorf("图片超过 %d MB 上限", waImageMaxBytes>>20)
	}
	if len(data) == 0 {
		return nil, "", errors.New("图片内容为空（源站返回 0 字节）")
	}
	mime := waSniffImageMime(data)
	if mime == "" {
		return nil, "", errors.New("图片格式不认识（只支持 jpeg / png / webp）")
	}
	return data, mime, nil
}

// waFetchImageHint 给非 200 响应补一句「这个状态码大概率意味着什么」。
//
// 存在理由：图片直链的失败面全在反代/CDN 侧，而这条错误串是运维唯一能看到的东西。
// 一个裸的 "源站返回 403" 会让人先去翻代码，而实际要改的是 WAF 规则。
// 下面两类都是 2026-10-30 真机实证过的：
//   - 403 + Server: cloudflare → WAF 把非浏览器请求当爬虫拦了（本项目已靠 UA 绕过，
//     但换成其它出口/其它 CDN 仍会复发）；
//   - 404 → /static/ 被反代到了不服务静态目录的进程（本项目 8090 的 static 是 null）。
func waFetchImageHint(resp *http.Response) string {
	switch resp.StatusCode {
	case http.StatusForbidden:
		if server := strings.ToLower(resp.Header.Get("Server")); strings.Contains(server, "cloudflare") {
			return "（源站是 Cloudflare，403 通常是 WAF 拦截非浏览器请求，需放行 /static/）"
		}
		return "（源站拒绝访问，检查反代 allow/deny 与防盗链）"
	case http.StatusNotFound:
		return "（源站没有这个文件：检查直链域名是否正确、/static/ 是否反代到服务静态目录的进程）"
	case http.StatusUnauthorized, http.StatusProxyAuthRequired:
		return "（源站要求认证，直链可能不是公开可读的）"
	}
	return ""
}

// waSniffImageMime 按文件头判类型。不信任 Content-Type：静态目录的 MIME 由
// 反代决定，配错时 WhatsApp 客户端会拿到一个打不开的附件。
func waSniffImageMime(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// waMakeThumbnail 生成缩略图与原始像素尺寸。
//
// 返回 (nil, 0, 0) 表示这张图给不出缩略图（webp 没有标准库解码器、或数据损坏）。
// 调用方据此只记 Warning，不阻断发送。
func waMakeThumbnail(data []byte) ([]byte, uint32, uint32) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0
	}
	// 等比缩到最长边 ≤ 72：横图压宽、竖图压高，比例不变形。
	tw, th := w, h
	if tw > waImageThumbMaxSide || th > waImageThumbMaxSide {
		if tw >= th {
			th = th * waImageThumbMaxSide / tw
			tw = waImageThumbMaxSide
		} else {
			tw = tw * waImageThumbMaxSide / th
			th = waImageThumbMaxSide
		}
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	// 最近邻取样：缩略图只用于预览，不值得引入双线性插值的复杂度与依赖。
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		sy := b.Min.Y + y*h/th
		for x := 0; x < tw; x++ {
			sx := b.Min.X + x*w/tw
			small.Set(x, y, img.At(sx, sy))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 70}); err != nil {
		return nil, 0, 0
	}
	return buf.Bytes(), uint32(w), uint32(h)
}

// upload 是带读锁的媒体上传：whatsmeow 的 Upload 读 mediaHTTP.Transport，
// 运行中切代理（waConn.SetProxy）写的正是同一字段，不互斥就是数据竞争。
// defer 兜住 panic，避免锁泄漏把之后的切代理永久堵死。
func (c *waConn) upload(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cli.Upload(ctx, data, kind)
}

// SendImage 是 waSession 的图片分支真实现：下载 → 上传 → 组装 ImageMessage → 发送。
//
// 上传用 cli.Upload（whatsmeow/upload.go:69）：它负责生成 mediaKey、加密、算
// FileSHA256/FileEncSHA256 并落到 WhatsApp 的 CDN，返回的四段材料必须逐字抄进
// ImageMessage，少一段对方就解不开（表现为「图片一直转圈」）。
func (c *waConn) SendImage(ctx context.Context, to string, img OutboundImage) (string, time.Time, error) {
	jid, err := types.ParseJID(to)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("收件人 JID 不可解析: %w", err)
	}
	data, mime, err := waFetchImage(ctx, img.URL)
	if err != nil {
		return "", time.Time{}, err
	}
	if want := strings.TrimSpace(img.MimeType); want != "" && want != mime {
		// 配置的类型与内容不符时以内容为准，只提示：拒发会让运营卡在一个
		// 「明明能打开」的文件上。
		vars.Warning("WA 图片类型声明与实际不符（声明=%s 实际=%s），按实际类型发送", want, mime)
	}
	thumb, w, h := waImageThumb(data)
	if thumb == nil {
		vars.Warning("WA 图片缩略图生成失败（webp 或数据损坏），本条按无预览发送")
	}
	// upload 读 mediaHTTP.Transport：与运行中切换代理（waConn.SetProxy）写同一字段，
	// 必须互斥（实现见 upload 方法）。
	up, err := c.upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("图片上传失败: %w", err)
	}
	msg := &waE2E.ImageMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		Mimetype:      proto.String(mime),
		FileSHA256:    up.FileSHA256,
		FileEncSHA256: up.FileEncSHA256,
		MediaKey:      up.MediaKey,
		FileLength:    proto.Uint64(up.FileLength),
	}
	if caption := strings.TrimSpace(img.Caption); caption != "" {
		msg.Caption = proto.String(caption)
	}
	if thumb != nil {
		msg.JPEGThumbnail = thumb
		msg.Width = proto.Uint32(w)
		msg.Height = proto.Uint32(h)
	}
	resp, err := c.cli.SendMessage(ctx, jid, &waE2E.Message{ImageMessage: msg})
	return string(resp.ID), resp.Timestamp, err
}

// WhatsappSendImage 用 fromJID 指定的在跑账号向 to 发一条图文消息（图片 + 说明），
// 返回服务器给的消息 id。
//
// 契约与 WhatsappSendMessage 完全一致（空 id ⇔ 非 nil error；连接态自愈；
// 发送成功后清空该会话）。差别只在内容形态：图片由本包下载并上传，
// 说明文字为空时就是一条纯图片消息。
func WhatsappSendImage(ctx context.Context, fromJID, to, imageURL, caption string) (string, error) {
	m, err := currentWA()
	if err != nil {
		return "", err
	}
	w, err := m.ensureLoggedIn(ctx, fromJID)
	if err != nil {
		return "", waLoginErr("发送图片消息", err)
	}
	msgID, _, err := deliverOutbound(ctx, w, to, OutboundMessage{
		To: to, Body: caption, ImageURL: imageURL,
	}, "发送图片消息", false)
	return msgID, err
}
