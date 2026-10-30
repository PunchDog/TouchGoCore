package rpc

// SendMsgCtx 的错误传播测试。
//
// 存在意义：SendMsg 在服务端失败时静默（只打日志），调用方只能靠自己超时感知。
// SendMsgCtx 把每类失败变成可 errors.Is 的 error —— 这几条测试钉住这个契约，
// 防止将来有人"顺手"把 SendMsgCtx 改回静默。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"touchgocore/util"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestSendMsgCtxWaitTimeout 客户端自身超时必须返回 ErrWaitTimeout，
// 而不是挂到服务端 12s 预算。
func TestSendMsgCtxWaitTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		slowP1, slowP2 = int32(61), int32(1)
	)
	_, client := eventTestEnv(t, "sctx-client")

	util.RegisterProtocolType(slowP1, slowP2, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, slowP1, slowP2)
	slow := func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(3 * time.Second) // 远大于下面的 client.timeout
		return wrapperspb.String("late")
	}
	util.DefaultCallFunc.Register(key, slow)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, slow) })

	// 客户端预算压到 300ms：服务端还在 sleep，客户端必须先放弃。
	client.timeout = 300 * time.Millisecond

	err := client.SendMsgCtx(context.Background(), slowP1, slowP2,
		wrapperspb.String("hi"), func(proto.Message) {})
	if err == nil {
		t.Fatal("服务端慢响应时 SendMsgCtx 应返回 error")
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("期望 ErrWaitTimeout，实际: %v", err)
	}
}

// TestSendMsgCtxCtxCancel 调用方取消必须原样返回 ctx.Err()，不能伪装成超时。
func TestSendMsgCtxCtxCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		slowP1, slowP2 = int32(62), int32(1)
	)
	_, client := eventTestEnv(t, "sctx-client2")

	util.RegisterProtocolType(slowP1, slowP2, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, slowP1, slowP2)
	slow := func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(3 * time.Second)
		return wrapperspb.String("late")
	}
	util.DefaultCallFunc.Register(key, slow)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, slow) })

	client.timeout = 5 * time.Second // 客户端预算够长，让 ctx 先到

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := client.SendMsgCtx(ctx, slowP1, slowP2, wrapperspb.String("hi"), func(proto.Message) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 context.DeadlineExceeded，实际: %v", err)
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Fatal("调用方取消不应被报告成 ErrWaitTimeout")
	}
}

// TestSendMsgCtxClosed 客户端关闭后调用必须立刻返回 ErrClientClosed。
func TestSendMsgCtxClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	_, client := eventTestEnv(t, "sctx-client3")
	_ = client.Close()

	err := client.SendMsgCtx(context.Background(), 63, 1, wrapperspb.String("hi"), func(proto.Message) {})
	if !errors.Is(err, ErrClientClosed) {
		t.Fatalf("期望 ErrClientClosed，实际: %v", err)
	}
}

// TestSendMsgCtxRecoversAfterTimeout 钉住"超时后作废流"这个修复。
//
// 修复前的行为（2026-10-28 实测）：一次超时会让这条流进入半死状态，
// **此后所有调用都等满超时且拿不到响应**，直到进程重启。
// 本测试先制造一次超时，再调一个本该秒回的协议 —— 必须成功。
func TestSendMsgCtxRecoversAfterTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		slowP1, slowP2 = int32(71), int32(1)
		fastP1, fastP2 = int32(72), int32(1)
	)
	_, client := eventTestEnv(t, "sctx-recover")

	util.RegisterProtocolType(slowP1, slowP2, wrapperspb.String(""))
	util.RegisterProtocolType(fastP1, fastP2, wrapperspb.String(""))

	slowKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, slowP1, slowP2)
	slow := func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(2 * time.Second)
		return wrapperspb.String("slow")
	}
	util.DefaultCallFunc.Register(slowKey, slow)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(slowKey, slow) })

	fastKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, fastP1, fastP2)
	fast := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("fast")
	}
	util.DefaultCallFunc.Register(fastKey, fast)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(fastKey, fast) })

	// 先把预算压到 300ms，制造一次必然的超时。
	client.timeout = 300 * time.Millisecond
	err := client.SendMsgCtx(context.Background(), slowP1, slowP2,
		wrapperspb.String("hi"), func(proto.Message) {})
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("第一次调用应超时，实际: %v", err)
	}

	// 恢复正常预算，紧接着调秒回的协议：必须成功。
	client.timeout = 5 * time.Second
	var got proto.Message
	err = client.SendMsgCtx(context.Background(), fastP1, fastP2,
		wrapperspb.String("hi"), func(m proto.Message) { got = m })
	if err != nil {
		t.Fatalf("超时之后的调用不应失败（流未重建？）: %v", err)
	}
	if got == nil {
		t.Fatal("超时之后的调用拿不到响应")
	}
	if s, ok := got.(*wrapperspb.StringValue); !ok || s.GetValue() != "fast" {
		t.Fatalf("响应内容不对: %v", got)
	}
}
