package zalo

// 本文件钉住「上游登录链 panic 不能带走进程」这道闸。
//
// 现场：发送触发懒登录/重登时，zcago v0.3.0 的 auth.login 对 nil response 取
// .Status（网络不可达/超时都会走到），panic 发生在登录 goroutine，未收口就是整个
// 进程退出。zaRealSDK.Login/LoginQR 必须把它折算成 errZALoginUpstreamPanic。
//
// 用假句柄注入 panic，不连网、不打真库。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amrakk/zcago"
)

// panicZALogin 模拟 zcago 登录链对 nil response 的 nil 解引用。
type panicZALogin struct{}

var _ zcago.Zalo = panicZALogin{}

func (panicZALogin) Login(context.Context, zcago.Credentials) (zcago.API, error) {
	panic("runtime error: invalid memory address or nil pointer dereference")
}

func (panicZALogin) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zcago.API, error) {
	panic("runtime error: invalid memory address or nil pointer dereference")
}

// installPanicZALogin 把句柄工厂换成 panic 假件，测试结束还原。
func installPanicZALogin(t *testing.T) {
	t.Helper()
	prev := zaNewZaloHandle
	zaNewZaloHandle = func() zcago.Zalo { return panicZALogin{} }
	t.Cleanup(func() { zaNewZaloHandle = prev })
}

func TestZALoginRecoversUpstreamPanic(t *testing.T) {
	installPanicZALogin(t)
	sdk := newZASDK()

	conn, err := sdk.Login(context.Background(), zcago.Credentials{})
	if conn != nil {
		t.Fatalf("panic 收口后不应交出 conn，实得 %#v", conn)
	}
	if !errors.Is(err, errZALoginUpstreamPanic) {
		t.Fatalf("Login err=%v，期望 errZALoginUpstreamPanic", err)
	}
	// 文案要能指导下步排查：点名端点与网络/代理，而不是只给 panic 值。
	if !strings.Contains(err.Error(), "getServerInfo") {
		t.Fatalf("错误文案缺少端点提示: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "nil pointer dereference") {
		t.Fatalf("错误文案缺少 panic 值: %q", err.Error())
	}

	conn, err = sdk.LoginQR(context.Background(), nil, nil)
	if conn != nil {
		t.Fatalf("LoginQR panic 收口后不应交出 conn，实得 %#v", conn)
	}
	if !errors.Is(err, errZALoginUpstreamPanic) {
		t.Fatalf("LoginQR err=%v，期望 errZALoginUpstreamPanic", err)
	}
}

// TestZALoginNormalErrorUntouched 确认收口不吞普通错误：非 panic 的错误原样返回。
func TestZALoginNormalErrorUntouched(t *testing.T) {
	prev := zaNewZaloHandle
	zaNewZaloHandle = func() zcago.Zalo { return errZALogin{} }
	t.Cleanup(func() { zaNewZaloHandle = prev })

	_, err := newZASDK().Login(context.Background(), zcago.Credentials{})
	if !errors.Is(err, errFacBoom) {
		t.Fatalf("普通错误 err=%v，期望原样返回 errFacBoom", err)
	}
	if errors.Is(err, errZALoginUpstreamPanic) {
		t.Fatalf("普通错误被误判成上游 panic: %v", err)
	}
}

// errZALogin 模拟登录返回普通错误（如 cookie 失效）。
type errZALogin struct{}

var _ zcago.Zalo = errZALogin{}

func (errZALogin) Login(context.Context, zcago.Credentials) (zcago.API, error) {
	return nil, errFacBoom
}

func (errZALogin) LoginQR(context.Context, *zcago.LoginQROption, zcago.LoginQRCallback) (zcago.API, error) {
	return nil, errFacBoom
}
