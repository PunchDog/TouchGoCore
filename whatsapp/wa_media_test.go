package whatsapp

// 图文混排发送面的测试：图片下载闸（协议/状态码/体积/类型）、缩略图生成、
// 以及「带 ImageURL 的消息走 SendImage 分支、正文当说明文字」这条主线。
//
// 不测真实上传：Upload 是 whatsmeow 的对外能力，本包只负责把它的四段材料抄进
// ImageMessage（wa_media.go 的 SendImage）。真机验证属于验收档，不塞进单测。

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/util"
)

// waTestPNG 造一张 w×h 的纯色 PNG。
func waTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("造图失败: %v", err)
	}
	return buf.Bytes()
}

// TestWaFetchImageGuards 图片下载的三道闸：协议白名单、状态码、内容为空。
//
// 三条都是「配置错了要响亮失败」的场景：后台把 file:// 路径或 404 链接存进文案库，
// 只有在这里拒绝，运营才能在保存时看到原因，而不是发出去一条永远转圈的消息。
func TestWaFetchImageGuards(t *testing.T) {
	ctx := context.Background()

	if _, _, err := waFetchImage(ctx, "file:///etc/passwd"); err == nil ||
		!strings.Contains(err.Error(), "http/https") {
		t.Fatalf("非 http(s) 直链必须被拒: %v", err)
	}
	if _, _, err := waFetchImage(ctx, "   "); err == nil {
		t.Fatal("空直链必须被拒")
	}

	pngData := waTestPNG(t, 8, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "text/plain") // 故意配错：MIME 必须按内容嗅探
			_, _ = w.Write(pngData)
		case "/empty.png":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	data, mime, err := waFetchImage(ctx, srv.URL+"/ok.png")
	if err != nil {
		t.Fatalf("正常图片应当取到: %v", err)
	}
	if !bytes.Equal(data, pngData) {
		t.Fatalf("下载内容与源不一致: got %d bytes want %d", len(data), len(pngData))
	}
	if mime != "image/png" {
		t.Fatalf("MIME 必须按内容嗅探为 image/png，实得 %q", mime)
	}

	if _, _, err := waFetchImage(ctx, srv.URL+"/missing.png"); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("非 200 必须被拒且带状态码: %v", err)
	}
	if _, _, err := waFetchImage(ctx, srv.URL+"/empty.png"); err == nil ||
		!strings.Contains(err.Error(), "0 字节") {
		t.Fatalf("0 字节必须被拒: %v", err)
	}
	if got := waSniffImageMime([]byte("GIF89a")); got != "" {
		t.Fatalf("不认识的格式应当返回空 MIME，实得 %q", got)
	}
}

// TestWaMakeThumbnail 缩略图：等比缩到最长边 72、产出可解码的 JPEG、并回带原图尺寸。
func TestWaMakeThumbnail(t *testing.T) {
	src := waTestPNG(t, 200, 100)
	thumb, w, h := waMakeThumbnail(src)
	if w != 200 || h != 100 {
		t.Fatalf("原图尺寸应回带 200x100，实得 %dx%d", w, h)
	}
	if len(thumb) == 0 {
		t.Fatal("横图应当能生成缩略图")
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("缩略图必须是可解码的 JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != waImageThumbMaxSide || b.Dy() != waImageThumbMaxSide/2 {
		t.Fatalf("等比缩放结果应为 72x36，实得 %dx%d", b.Dx(), b.Dy())
	}

	// 小图不放大：本来就是 40x20，缩略图保持原尺寸。
	small := waTestPNG(t, 40, 20)
	thumb2, w2, h2 := waMakeThumbnail(small)
	if w2 != 40 || h2 != 20 || len(thumb2) == 0 {
		t.Fatalf("小图应当原尺寸出缩略图: %dx%d len=%d", w2, h2, len(thumb2))
	}
}

// TestWhatsappSendImageUsesImageBranch 图文消息走 SendImage 分支：
// 正文进入 Caption（不是 Conversation），目标归一成会话 JID，且不落进文本账。
func TestWhatsappSendImageUsesImageBranch(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	const imgURL = "https://admin.example.com/static/chatimg_abc.jpg"
	id, err := WhatsappSendImage(nil, "8613800000000", "+86 139-0000-0000", imgURL, " 你好，欢迎加入 ")
	if err != nil || id != "MSG-1" {
		t.Fatalf("图文发送: id=%q err=%v", id, err)
	}
	s.mu.Lock()
	imgs := append([]fakeImage(nil), s.sentImages...)
	texts := append([]fakeText(nil), s.sent...)
	s.mu.Unlock()
	if len(texts) != 0 {
		t.Fatalf("图文消息不得落进纯文本账: %+v", texts)
	}
	if len(imgs) != 1 {
		t.Fatalf("应当恰好发出一条图片消息，实得 %d", len(imgs))
	}
	if imgs[0].to != "8613900000000@s.whatsapp.net" {
		t.Fatalf("收件人未归一: %q", imgs[0].to)
	}
	if imgs[0].url != imgURL {
		t.Fatalf("图片直链不符: %q", imgs[0].url)
	}
	// 说明文字原样下传（trim 在 waConn.SendImage 组装 protobuf 时做）：
	// 框架层不做静默清洗，否则「发出去的文案」与「库里存的文案」会悄悄不一致。
	if imgs[0].caption != " 你好，欢迎加入 " {
		t.Fatalf("说明文字应当原样传递: %q", imgs[0].caption)
	}

	// 图与文都空：整条拒绝（不是「发了个空气」）。
	if id, err := WhatsappSendImage(nil, "8613800000000", "8613900000000", "", ""); err == nil || id != "" {
		t.Fatalf("图与文都空必须被拒: id=%q err=%v", id, err)
	}

	// 只有图没有说明是合法的（Caption 为空）。
	if _, err := WhatsappSendImage(nil, "8613800000000", "8613900000000", imgURL, ""); err != nil {
		t.Fatalf("纯图片消息应当被允许: %v", err)
	}
}

// TestWaBatchImageContent 批内每条按自己的 ImageURL 决定形态：
// 同一批里既有纯文本又有图文，两条必须落到各自的分支（自动发送逐条随机文案时，
// 有的文案带图、有的不带，这条闸就是那个混合场景）。
func TestWaBatchImageContent(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	// 速率闸放到 2 条：默认 send_rate 是 1|1，第二条会被闸拒而根本走不到发送分支，
	// 那样测出来的就不是「图文分支」而是「速率闸」了。
	cfg := &config.WhatsappSessionsConfig{SendRate: "1|2", AntibanEnabled: waBool(false)}
	m, _, get := startFakeWA(t, cfg, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(_ time.Duration) {} // 批内间隔对本用例无意义，别真睡
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	var results []*SendResult
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(r *SendResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{
		{To: "8613900000000", Body: "纯文本"},
		{To: "8613900000001", Body: "带图说明", ImageURL: "https://x/y.jpg"},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "两条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	})

	s.mu.Lock()
	texts := append([]fakeText(nil), s.sent...)
	imgs := append([]fakeImage(nil), s.sentImages...)
	s.mu.Unlock()
	if len(texts) != 1 || texts[0].body != "纯文本" {
		t.Fatalf("纯文本条目应走文本分支: %+v", texts)
	}
	if len(imgs) != 1 || imgs[0].caption != "带图说明" || imgs[0].url != "https://x/y.jpg" {
		t.Fatalf("带图条目应走图片分支: %+v", imgs)
	}
}
