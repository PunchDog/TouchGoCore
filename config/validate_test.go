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

func TestValidateModelAPIMultiModel(t *testing.T) {
	c := &Cfg{ModelAPI: &ModelAPIConfig{
		Enable:   "on",
		Default:  "a",
		Strategy: "cheapest",
		Providers: map[string]*ModelProviderConfig{
			"a": {
				BaseURL: "https://x/v1", APIKey: "k", Model: "m1",
				Models: []*ModelInfoConfig{
					{Name: "m1", InputPrice: 1, OutputPrice: 2, SupportsTools: true},
					{Name: "m2", InputPrice: 0.1, OutputPrice: 0.2},
				},
			},
			// 未配置 model，应取 models 首个作为默认模型
			"b": {
				BaseURL: "https://y/v1", APIKey: "k",
				Models: []*ModelInfoConfig{{Name: "n1", InputPrice: 0.01, OutputPrice: 0.02}},
			},
		},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("合法多模型配置应通过校验: %v", err)
	}
	if got := c.ModelAPI.StrategyOf(); got != ModelStrategyCheapest {
		t.Errorf("StrategyOf = %q, want %q", got, ModelStrategyCheapest)
	}
	if got := c.ModelAPI.Providers["b"].DefaultModel(); got != "n1" {
		t.Errorf("DefaultModel = %q, want n1", got)
	}
	// 未配置 strategy 时按 default 处理
	if got := (&ModelAPIConfig{}).StrategyOf(); got != ModelStrategyDefault {
		t.Errorf("StrategyOf(空) = %q, want %q", got, ModelStrategyDefault)
	}
	if !(&ModelAPIConfig{Strategy: " CHEAPEST "}).StrategyValid() {
		t.Error("策略取值应忽略大小写与空白")
	}
	if (&ModelAPIConfig{Strategy: "smart"}).StrategyValid() {
		t.Error("未知策略取值应判定为非法")
	}
}

func TestValidateModelAPIErrors(t *testing.T) {
	base := func(mut func(*ModelAPIConfig)) *Cfg {
		c := &Cfg{ModelAPI: &ModelAPIConfig{
			Enable:  "on",
			Default: "a",
			Providers: map[string]*ModelProviderConfig{
				"a": {
					BaseURL: "https://x/v1", APIKey: "k", Model: "m1",
					Models: []*ModelInfoConfig{{Name: "m1", InputPrice: 1, OutputPrice: 2}},
				},
			},
		}}
		mut(c.ModelAPI)
		return c
	}

	cases := []struct {
		name string
		mut  func(*ModelAPIConfig)
	}{
		{"策略非法", func(m *ModelAPIConfig) { m.Strategy = "smart" }},
		{"模型名为空", func(m *ModelAPIConfig) { m.Providers["a"].Models[0].Name = " " }},
		{"模型条目为空", func(m *ModelAPIConfig) { m.Providers["a"].Models = append(m.Providers["a"].Models, nil) }},
		{"价格为负", func(m *ModelAPIConfig) { m.Providers["a"].Models[0].InputPrice = -1 }},
		{"模型名重复", func(m *ModelAPIConfig) {
			m.Providers["a"].Models = append(m.Providers["a"].Models, &ModelInfoConfig{Name: "m1", InputPrice: 1})
		}},
		{"默认模型不在列表中", func(m *ModelAPIConfig) { m.Providers["a"].Model = "other" }},
		{"model 与 models 同为空", func(m *ModelAPIConfig) {
			m.Providers["a"].Model = ""
			m.Providers["a"].Models = nil
		}},
		{"cheapest 但无价格", func(m *ModelAPIConfig) {
			m.Strategy = "cheapest"
			m.Providers["a"].Model = "m1"
			m.Providers["a"].Models = []*ModelInfoConfig{{Name: "m1"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := base(tc.mut).Validate(); err == nil {
				t.Error("期望校验失败，但通过了")
			}
		})
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
