package config

import "testing"

func TestValidateNil(t *testing.T) {
	var c *Cfg
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for nil cfg")
	}
}

func TestValidatePortConflict(t *testing.T) {
	c := &Cfg{
		Web:      &WebConfig{HTTPPort: 8000},
		Ws:       &WebsocketConfig{Port: []*WebsocketPort{{Port: 8000}}},
		LogLevel: "info",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected port conflict")
	}
}

func TestValidateOK(t *testing.T) {
	c := &Cfg{
		Web: &WebConfig{HTTPPort: 1000},
		Ws: &WebsocketConfig{
			Port:           []*WebsocketPort{{Port: 8000}},
			CheckOrigin:    true,
			AllowedOrigins: []string{"http://127.0.0.1:3000"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeRpcPortAlias(t *testing.T) {
	c := &Cfg{RpcPort: &RpcConfig{Server: []*RpcAddr{{Name: "s", Port: 7000}}}}
	c.Normalize()
	if c.Rpc == nil || c.Rpc != c.RpcPort {
		t.Fatal("expected RpcPort aliased to Rpc")
	}
	if c.RpcOf() != c.Rpc {
		t.Fatal("RpcOf")
	}
}

func TestQueueCapacity(t *testing.T) {
	var c *Cfg
	if c.QueueCapacity(4096) != 4096 {
		t.Fatal("nil cfg default")
	}
	c = &Cfg{Server: &ServerConfig{ReadBuffer: 128, WriteBuffer: 64, Backpressure: true}}
	if c.QueueCapacity(4096) != 128 || c.WriteQueueCapacity(4096) != 64 || !c.DropOnFull() {
		t.Fatal("server config not applied")
	}
}
