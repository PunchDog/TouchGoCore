package touchgocore

import (
	"testing"

	"touchgocore/rpc"
	"touchgocore/syncmap"
	"touchgocore/websocket"
)

func TestAppRegistryFallback(t *testing.T) {
	app := &App{
		rpcClients: syncmap.NewMap[string, *rpc.RpcClient](),
		rpcServers: syncmap.NewMap[string, *rpc.RpcServer](),
		wsClients:  syncmap.NewMap[int64, *websocket.Client](),
	}
	if app.GetRpcClient("missing") != nil {
		t.Fatal("expected nil rpc client")
	}
	if app.GetRpcServer("missing") != nil {
		t.Fatal("expected nil rpc server")
	}
	if app.GetWSClient(1) != nil {
		t.Fatal("expected nil ws client")
	}
}
