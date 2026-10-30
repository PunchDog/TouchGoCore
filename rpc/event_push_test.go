package rpc

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/syncmap"
	"touchgocore/util"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// 事件推送的集成测试：验证 requestID=0 的主动推帧能被 OnEvent 收到，
// 且不影响未声明为事件的协议号走原来的兼容路径。
//
// 背景（events.go 头注）：修复前这类帧要么被丢弃（无在途请求），要么被
// 错投成某个业务请求的响应（有在途请求）——后者会让业务拿到语义无关的消息。

// eventTestEnv 起一对真实的 server/client，返回两者与清理函数。
func eventTestEnv(t *testing.T, clientName string) (*RpcServer, *RpcClient) {
	t.Helper()

	cfg := &config.Cfg{
		Rpc: &config.RpcConfig{
			Auth: &config.RpcAuthConfig{Mode: "none"},
		},
	}
	prev := config.Cfg_
	prevCtx := runCtx()
	config.Cfg_ = cfg
	setRunCtx(corectx.WithCfg(context.Background(), cfg))
	UseRegistry(syncmap.NewMap[string, *RpcServer](), syncmap.NewMap[string, *RpcClient]())
	t.Cleanup(func() {
		config.Cfg_ = prev
		setRunCtx(prevCtx)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := StartGrpcServer("evtest", port, false); err != nil {
		t.Fatal(err)
	}
	srv := GetRpcServer("evtest")
	if srv == nil {
		t.Fatal("server missing")
	}
	t.Cleanup(func() { srv.Stop(context.Background()) })

	client := NewRpcClient(clientName, "127.0.0.1", port)
	if client == nil {
		t.Fatal("client nil")
	}
	client.timeout = 3 * time.Second
	t.Cleanup(func() { _ = client.Close() })

	return srv, client
}

// waitConnected 发一次请求把 client 的 bidi 流建起来（流是懒建的）。
func waitConnected(t *testing.T, client *RpcClient, p1, p2 int32) {
	t.Helper()
	got := make(chan proto.Message, 1)
	client.SendMsg(p1, p2, wrapperspb.String("hello"), func(resp proto.Message) {
		select {
		case got <- resp:
		default:
		}
	})
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("首次请求超时，流未建立")
	}
}

// TestEventPushDelivered 已声明为事件的协议号：主动推帧必须送达 OnEvent。
func TestEventPushDelivered(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		pingP1, pingP2   = int32(31), int32(1)
		eventP1, eventP2 = int32(91), int32(1)
	)
	srv, client := eventTestEnv(t, "ev-client")

	util.RegisterProtocolType(pingP1, pingP2, wrapperspb.String(""))
	util.RegisterProtocolType(eventP1, eventP2, wrapperspb.String(""))
	pingKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, pingP1, pingP2)
	pingHandler := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("pong")
	}
	util.DefaultCallFunc.Register(pingKey, pingHandler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(pingKey, pingHandler) })

	// 声明事件通道 —— 这是本次修复的核心开关。
	RegisterEventProtocol(eventP1, eventP2)
	t.Cleanup(func() { UnregisterEventProtocol(eventP1, eventP2) })

	received := make(chan proto.Message, 4)
	client.SetCallbacks(&ClientCallbacks{
		OnEvent: func(_ string, p1, p2 int32, evt proto.Message) {
			if p1 != eventP1 || p2 != eventP2 {
				t.Errorf("OnEvent 协议号不符: got %d:%d", p1, p2)
			}
			select {
			case received <- evt:
			default:
			}
		},
	})

	waitConnected(t, client, pingP1, pingP2)

	// 服务端主动推：RpcServer.Send 走 requestID=0。
	if err := srv.Send("ev-client", eventP1, eventP2, wrapperspb.String("qr-refresh")); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	select {
	case evt := <-received:
		if got := evt.(*wrapperspb.StringValue).GetValue(); got != "qr-refresh" {
			t.Fatalf("事件内容不符: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内未收到事件 —— 修复未生效（帧被丢弃或错投）")
	}
}

// TestEventPushIgnoredWhenUnregistered 未声明的协议号：行为必须保持原样
// （事件被丢弃但不 panic、不影响后续请求）。这条是兼容性回归闸。
func TestEventPushIgnoredWhenUnregistered(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		pingP1, pingP2   = int32(32), int32(1)
		eventP1, eventP2 = int32(92), int32(1)
	)
	srv, client := eventTestEnv(t, "ev-client2")

	util.RegisterProtocolType(pingP1, pingP2, wrapperspb.String(""))
	util.RegisterProtocolType(eventP1, eventP2, wrapperspb.String(""))
	pingKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, pingP1, pingP2)
	pingHandler := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("pong")
	}
	util.DefaultCallFunc.Register(pingKey, pingHandler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(pingKey, pingHandler) })

	// 故意不注册事件协议。
	eventHit := make(chan struct{}, 1)
	client.SetCallbacks(&ClientCallbacks{
		OnEvent: func(_ string, _, _ int32, _ proto.Message) {
			select {
			case eventHit <- struct{}{}:
			default:
			}
		},
	})

	waitConnected(t, client, pingP1, pingP2)
	_ = srv.Send("ev-client2", eventP1, eventP2, wrapperspb.String("noise"))

	select {
	case <-eventHit:
		t.Fatal("未声明为事件的协议号不应触发 OnEvent")
	case <-time.After(500 * time.Millisecond):
		// 预期：走原兼容路径，不进 OnEvent
	}

	// 关键：上面那条"垃圾帧"不能污染后续正常请求。
	got := make(chan proto.Message, 1)
	client.SendMsg(pingP1, pingP2, wrapperspb.String("again"), func(resp proto.Message) {
		select {
		case got <- resp:
		default:
		}
	})
	select {
	case resp := <-got:
		if resp == nil || resp.(*wrapperspb.StringValue).GetValue() != "pong" {
			t.Fatalf("后续请求被污染: %v", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("后续请求超时 —— 未声明的推送帧破坏了连接")
	}
}
