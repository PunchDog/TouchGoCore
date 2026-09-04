package touchgocore

import (
	"context"
	"testing"
	"time"

	"touchgocore/rpc"
	"touchgocore/syncmap"
	"touchgocore/websocket"
)

type mockService struct {
	name   string
	starts int
	stops  int
}

func (m *mockService) Name() string { return m.name }
func (m *mockService) Start(context.Context) error {
	m.starts++
	return nil
}
func (m *mockService) Stop(context.Context) error {
	m.stops++
	return nil
}

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

func TestAppStartShutdownOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &mockService{name: "a"}
	b := &mockService{name: "b"}
	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		services: []Service{a, b},
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	if a.starts != 1 || b.starts != 1 {
		t.Fatalf("starts a=%d b=%d", a.starts, b.starts)
	}
	if err := app.Shutdown(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if a.stops != 1 || b.stops != 1 {
		t.Fatalf("stops a=%d b=%d", a.stops, b.stops)
	}
	if app.started {
		t.Fatal("expected started=false")
	}
}
