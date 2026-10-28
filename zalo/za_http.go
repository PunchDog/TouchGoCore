package zalo

// 本文件是 Zalo 通道私有 HTTP client 的传输层兜底。
//
// 背景（2026-10-27 实测踩坑）：zcago v0.3.0 的 auth.login 在 httpx.Request 失败时
// 会对 **nil** response 取 .Status（session/auth/login.go:62/74/167/176），必然 panic。
// 而这个 panic 发生在 zcago 内部的 errgroup goroutine 里 —— 调用方的 recover
// （zaRecoverLoginPanic）**接不住**，整个进程会被带走：发送消息时账号登录不上，
// 表现是「一发送就崩」/「response 为空」。
//
// 兜底做法：在我们自己的私有 client 上包一层 Transport，把传输层失败（DNS/连接/
// TLS/超时/ctx 取消）折成一个合成的 **599** 响应，而不是 (nil, err)。上游随后走
// 「HTTP 状态非 2xx」的既有分支，登录失败变成一个干净的可判错误，进程不崩。
//
// 为什么不用 (nil, err)：zcago 里所有 endpoint 都会先判 err 再用 response，唯独
// auth 那四处漏判 —— 只有把 response 变成非 nil，这条路才走得通。

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// zaFallbackTransport 把传输层失败折成 599 合成响应。
type zaFallbackTransport struct {
	base http.RoundTripper
}

// RoundTrip 透传成功响应；失败时返回 599 合成响应（err 恒为 nil）。
func (t zaFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	return &http.Response{
		Status:     "599 " + zaTransportErrText(err),
		StatusCode: 599,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// zaTransportErrText 把传输层错误压成一行短文案。
//
// 只取 url.Error 的内层错误：外层 Error() 会带上完整 URL（Zalo 的 URL 里带加密
// 参数与会话标识），不能进日志/回显。超长截断，避免把整个错误树塞进 HTTP 状态行。
func zaTransportErrText(err error) string {
	if err == nil {
		return "transport error"
	}
	text := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		text = ue.Err.Error()
	}
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.TrimSpace(text)
	if len(text) > 160 {
		text = text[:160]
	}
	if text == "" {
		return "transport error"
	}
	return text
}
