package corectx

import (
	"context"
	"testing"

	"touchgocore/config"
)

func TestCfgFromPrefersContext(t *testing.T) {
	injected := &config.Cfg{LogLevel: "debug"}
	prev := config.Cfg_
	config.Cfg_ = &config.Cfg{LogLevel: "info"}
	t.Cleanup(func() { config.Cfg_ = prev })

	ctx := WithCfg(context.Background(), injected)
	got := CfgFrom(ctx)
	if got != injected {
		t.Fatal("expected injected cfg")
	}
	if CfgFrom(context.Background()) != config.Cfg_ {
		t.Fatal("expected fallback to Cfg_")
	}
}

func TestAppViewFrom(t *testing.T) {
	view := &AppView{ServerName: "gate", Cfg: &config.Cfg{LogLevel: "warn"}}
	ctx := WithAppView(context.Background(), view)
	got := AppViewFrom(ctx)
	if got == nil || got.ServerName != "gate" || CfgFrom(ctx) != view.Cfg {
		t.Fatalf("view=%#v", got)
	}
}
