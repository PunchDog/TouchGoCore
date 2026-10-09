package whatsapp

import (
	"net/url"
	"strings"
	"testing"
)

func TestShareText(t *testing.T) {
	got := cloudShareText("Hello World")
	if got != "https://wa.me/?text=Hello+World" {
		t.Fatalf("ShareText=%s", got)
	}
	got = cloudShareText("你好 & 世界")
	if !strings.HasPrefix(got, "https://wa.me/?text=") {
		t.Fatalf("缺少 text 参数: %s", got)
	}
	enc := strings.TrimPrefix(got, "https://wa.me/?text=")
	if _, err := url.QueryUnescape(enc); err != nil {
		t.Fatalf("文本未正确编码: %v", err)
	}
}

func TestShareTo(t *testing.T) {
	cases := []struct {
		phone, text, wantPrefix string
	}{
		{"+86 138 0013 8000", "", "https://wa.me/8613800138000"},
		{"(86)13800138000", "hi", "https://wa.me/8613800138000?text=hi"},
		{"8613800138000", "a b", "https://wa.me/8613800138000?text=a+b"},
	}
	for _, cs := range cases {
		got := cloudShareTo(cs.phone, cs.text)
		if !strings.HasPrefix(got, cs.wantPrefix) {
			t.Errorf("ShareTo(%q,%q)=%s，期望前缀 %s", cs.phone, cs.text, got, cs.wantPrefix)
		}
	}
}

func TestShareToCleansNonDigits(t *testing.T) {
	got := cloudShareTo("abc+86-138", "")
	if got != "https://wa.me/86138" {
		t.Fatalf("号码清洗错误: %s", got)
	}
}

func TestNormalizePhone(t *testing.T) {
	if cloudNormalizePhone(" +86 (138) ") != "86138" {
		t.Fatal("NormalizePhone 清洗不符")
	}
}
