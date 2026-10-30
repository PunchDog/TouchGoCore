package whatsapp

// CTA 按钮消息（native_flow / cta_url）的发送面：
//   ① 带链接的条目走 SendInteractive（正文、图片 header、按钮字样/链接逐字下传）；
//   ② 按钮被服务端拒 ⇒ 降级为「正文 + 字样 + 链接」的普通文本再发一次，
//      整批不因这一条停摆（个人号对交互消息的支持不是官方契约）。

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"touchgocore/util"
)

// TestWaBatchLinkContent 带按钮的条目走 SendInteractive，不落进纯文本账。
func TestWaBatchLinkContent(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(_ time.Duration) {}
	waBatchDelay = func(min, _ time.Duration) time.Duration { return min }
	t.Cleanup(func() { waBatchSleep, waBatchDelay = oldSleep, oldDelay })

	var mu sync.Mutex
	results := 0
	id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+"SendResult", func(*SendResult) {
		mu.Lock()
		results++
		mu.Unlock()
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+"SendResult", id) })

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{{
		To: "8613900000000", Body: "说明文字",
		LinkText: "立即参与", LinkURL: "https://landing.example.com/act/11",
	}}); err != nil {
		t.Fatalf("批量受理失败: %v", err)
	}
	waitUntil(t, 3*time.Second, "一条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return results == 1
	})

	s.mu.Lock()
	links := append([]fakeInteractive(nil), s.sentLinks...)
	texts := append([]fakeText(nil), s.sent...)
	s.mu.Unlock()
	if len(texts) != 0 {
		t.Fatalf("按钮消息不该落进纯文本账: %+v", texts)
	}
	if len(links) != 1 {
		t.Fatalf("应当恰好发出一条按钮消息，实得 %d", len(links))
	}
	got := links[0]
	if got.to != "8613900000000@s.whatsapp.net" || got.body != "说明文字" ||
		got.buttonText != "立即参与" || got.buttonURL != "https://landing.example.com/act/11" {
		t.Fatalf("按钮消息落点不符: %+v", got)
	}
}

// TestWaLinkFallbackWhenInteractiveRejected 交互消息被拒 ⇒ 摊成文本重发，且正文含链接。
func TestWaLinkFallbackWhenInteractiveRejected(t *testing.T) {
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	m, _, get := startFakeWA(t, nil, nil, dev)
	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatal(err)
	}
	s := get(dev.ID.String())
	s.mu.Lock()
	s.linkErr = errors.New("interactive message not supported")
	s.mu.Unlock()

	oldSleep, oldDelay := waBatchSleep, waBatchDelay
	waBatchSleep = func(_ time.Duration) {}
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

	if err := WhatsappSendBatchAsync(nil, "8613800000000", []OutboundMessage{{
		To: "8613900000000", Body: "说明文字",
		LinkText: "立即参与", LinkURL: "https://landing.example.com/act/11",
	}}); err != nil {
		t.Fatalf("批量受理失败: %v", err)
	}
	waitUntil(t, 3*time.Second, "一条 SendResult", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 1
	})

	s.mu.Lock()
	texts := append([]fakeText(nil), s.sent...)
	s.mu.Unlock()
	if len(texts) != 1 {
		t.Fatalf("降级后应发出一条文本，实得 %d", len(texts))
	}
	want := "说明文字\n\n立即参与\nhttps://landing.example.com/act/11"
	if texts[0].body != want {
		t.Fatalf("降级正文=%q，期望 %q", texts[0].body, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("降级成功后结果应为成功: %+v", results)
	}
}

// TestWaLinkFallbackBody 纯函数：三种组合的拼接形态。
func TestWaLinkFallbackBody(t *testing.T) {
	cases := []struct{ body, text, url, want string }{
		{"正文", "按钮", "https://a.example.com/x", "正文\n\n按钮\nhttps://a.example.com/x"},
		{"正文", "", "https://a.example.com/x", "正文\nhttps://a.example.com/x"},
		{"", "按钮", "https://a.example.com/x", "按钮\nhttps://a.example.com/x"},
	}
	for _, c := range cases {
		if got := waLinkFallbackBody(c.body, c.text, c.url); got != c.want {
			t.Fatalf("waLinkFallbackBody(%q,%q,%q)=%q，期望 %q", c.body, c.text, c.url, got, c.want)
		}
	}
	if got := waInteractiveBizNodes(); len(got) != 1 || got[0].Tag != "biz" {
		t.Fatalf("按钮消息必须带 biz 节点: %+v", got)
	}
	if !strings.Contains(waCtaButtonName, "cta_url") {
		t.Fatalf("按钮 name 应为 cta_url，实得 %q", waCtaButtonName)
	}
}
