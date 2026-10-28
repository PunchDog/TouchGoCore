package zalo

// 本文件钉住传输层失败折 599 响应的兜底：它是「上游登录链 nil 解引用 panic」的
// 唯一防线（panic 发生在上游 goroutine 里，调用方 recover 接不住）。
//
// 用假 RoundTripper 注入失败，不连网。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// failTransport 模拟传输层失败（DNS/连接/超时）。
type failTransport struct{ err error }

func (t failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

func TestZAFallbackTransportTurnsErrorIntoResponse(t *testing.T) {
	tr := zaFallbackTransport{base: failTransport{err: errors.New("dial tcp: lookup wpa.chat.zalo.me: no such host")}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://wpa.chat.zalo.me/api/x", nil)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("兜底后不应再返回 err: %v", err)
	}
	if resp == nil {
		t.Fatal("兜底后必须给出一个非 nil response")
	}
	if resp.StatusCode != 599 {
		t.Fatalf("状态码=%d，期望 599", resp.StatusCode)
	}
	if resp.Body == nil {
		t.Fatal("Body 不能为 nil：上游 endpoint 会 defer resp.Body.Close()")
	}
	if resp.Header == nil {
		t.Fatal("Header 不能为 nil：上游会读 resp.Header.Get")
	}
	// 上游拿到的错误文案里不能再出现 URL（URL 带加密参数与会话标识）。
	if strings.Contains(resp.Status, "wpa.chat.zalo.me/api") {
		t.Fatalf("状态行回显了 URL: %q", resp.Status)
	}
	if !strings.Contains(resp.Status, "no such host") {
		t.Fatalf("状态行应保留可排查的原因: %q", resp.Status)
	}
}

// TestZAFallbackTransportPassesSuccess 断言成功响应原样透传，不被改写。
func TestZAFallbackTransportPassesSuccess(t *testing.T) {
	want := &http.Response{StatusCode: 200, Body: http.NoBody}
	tr := zaFallbackTransport{base: okTransport{resp: want}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://wpa.chat.zalo.me/api/x", nil)

	got, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != want {
		t.Fatalf("成功响应被改写: %+v", got)
	}
}

type okTransport struct{ resp *http.Response }

func (t okTransport) RoundTrip(*http.Request) (*http.Response, error) { return t.resp, nil }

// TestZATransportErrTextDropsURL 断言 url.Error 的 URL 不进文案。
func TestZATransportErrTextDropsURL(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://wpa.chat.zalo.me/api/login/getServerInfo?signkey=SECRET",
		Err: errors.New("dial tcp: i/o timeout"),
	}
	text := zaTransportErrText(err)
	if strings.Contains(text, "SECRET") || strings.Contains(text, "wpa.chat.zalo.me") {
		t.Fatalf("文案泄露了 URL/参数: %q", text)
	}
	if !strings.Contains(text, "i/o timeout") {
		t.Fatalf("文案丢了根因: %q", text)
	}
	if strings.ContainsAny(text, "\r\n") {
		t.Fatalf("文案必须单行: %q", text)
	}
}
