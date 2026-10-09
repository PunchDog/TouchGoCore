package whatsapp

import (
	"testing"
)

func TestVerifyWebhookChallenge(t *testing.T) {
	if _, err := cloudVerifyWebhookChallenge("subscribe", "tok", "123", "tok"); err != nil {
		t.Fatalf("应当通过: %v", err)
	}
	if _, err := cloudVerifyWebhookChallenge("other", "tok", "123", "tok"); err == nil {
		t.Fatal("mode 错误应通过")
	}
	if _, err := cloudVerifyWebhookChallenge("subscribe", "wrong", "123", "tok"); err == nil {
		t.Fatal("token 错误应通过")
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account"}`)
	secret := "appsecret"
	good := cloudComputeSignature(secret, body)
	if err := cloudVerifySignature(secret, body, good); err != nil {
		t.Fatalf("正确签名应过: %v", err)
	}
	if err := cloudVerifySignature(secret, body, "sha256=deadbeef"); err == nil {
		t.Fatal("错误签名应通过")
	}
	if err := cloudVerifySignature("", body, good); err == nil {
		t.Fatal("未配置密钥应通过")
	}
	// 篡改 body 后旧签名失效
	if err := cloudVerifySignature(secret, []byte(`{"object":"tampered"}`), good); err == nil {
		t.Fatal("篡改后仍过签")
	}
}

func TestParseWebhookCollects(t *testing.T) {
	raw := `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"PNID"},"messages":[{"from":"86138","id":"w1","type":"text","text":{"body":"hi"}}],"statuses":[{"id":"w1","status":"delivered"}]}}]}]}`
	env, err := cloudParseWebhook([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	msgs := env.CollectMessages()
	if len(msgs) != 1 || msgs[0].From != "86138" || msgs[0].Text == nil || msgs[0].Text.Body != "hi" {
		t.Fatalf("消息收集错误: %+v", msgs)
	}
	st := env.CollectStatuses()
	if len(st) != 1 || st[0].Status != "delivered" {
		t.Fatalf("状态收集错误: %+v", st)
	}
}

func TestParseWebhookButtonReply(t *testing.T) {
	raw := `{"object":"x","entry":[{"changes":[{"field":"messages","value":{"messages":[{"from":"86138","id":"i1","type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"lang_zh","title":"中文"}}}]}}]}]}`
	env, _ := cloudParseWebhook([]byte(raw))
	m := env.CollectMessages()[0]
	if m.Type != "interactive" || m.Interactive == nil || m.Interactive.ButtonReply == nil {
		t.Fatalf("按钮回流解析错误: %+v", m)
	}
	if m.Interactive.ButtonReply.ID != "lang_zh" {
		t.Fatalf("按钮 ID 错误: %s", m.Interactive.ButtonReply.ID)
	}
}
