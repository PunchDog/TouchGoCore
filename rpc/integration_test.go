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

func TestRPCRoundTripRequestID(t *testing.T) {
	cfg := &config.Cfg{
		Rpc: &config.RpcConfig{
			Auth: &config.RpcAuthConfig{Mode: "none"},
		},
	}
	prev := config.Cfg_
	prevCtx := rpcRunCtx
	config.Cfg_ = cfg
	rpcRunCtx = corectx.WithCfg(context.Background(), cfg)
	UseRegistry(syncmap.NewMap[string, *RpcServer](), syncmap.NewMap[string, *RpcClient]())
	t.Cleanup(func() {
		config.Cfg_ = prev
		rpcRunCtx = prevCtx
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := StartGrpcServer("itest", port, false); err != nil {
		t.Fatal(err)
	}
	srv := GetRpcServer("itest")
	if srv == nil {
		t.Fatal("server missing")
	}
	t.Cleanup(func() { srv.Stop(context.Background()) })

	util.RegisterProtocolType(3, 1, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, 3, 1)
	handler := func(_ context.Context, _ *msginfo) proto.Message {
		return wrapperspb.String("pong")
	}
	util.DefaultCallFunc.Register(key, handler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, handler) })

	client := NewRpcClient("itest-client", "127.0.0.1", port)
	if client == nil {
		t.Fatal("client nil")
	}
	client.timeout = 3 * time.Second

	done := make(chan proto.Message, 1)
	client.SetCallbacks(&ClientCallbacks{
		OnMessageReceived: func(_ string, _, _ int32, resp proto.Message) {
			select {
			case done <- resp:
			default:
			}
		},
	})
	client.SendMsg(3, 1, wrapperspb.String("ping"), nil)
	select {
	case got := <-done:
		sv, ok := got.(*wrapperspb.StringValue)
		if !ok || sv.GetValue() != "pong" {
			t.Fatalf("got %#v", got)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("rpc roundtrip timeout")
	}
}
