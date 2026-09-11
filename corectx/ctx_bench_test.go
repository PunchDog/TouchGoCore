package corectx

import (
	"context"
	"testing"

	"touchgocore/config"
)

func BenchmarkWithCfg_From(b *testing.B) {
	cfg := &config.Cfg{}
	ctx := WithCfg(context.Background(), cfg)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CfgFrom(ctx)
	}
}

func BenchmarkWithAppView_From(b *testing.B) {
	ctx := WithAppView(context.Background(), &AppView{
		ServerName: "test",
		Cfg:        &config.Cfg{},
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = AppViewFrom(ctx)
	}
}
