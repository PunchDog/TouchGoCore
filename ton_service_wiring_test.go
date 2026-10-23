package touchgocore

import (
	"context"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/ton"
)

// TestTonServiceRegistered TON 已从 telegramService 拆出，必须独立注册。
func TestTonServiceRegistered(t *testing.T) {
	app := &App{}
	app.registerServices()
	names := map[string]bool{}
	for _, s := range app.services {
		names[s.Name()] = true
	}
	if !names["ton"] {
		t.Fatal("启动服务列表里没有 ton")
	}
	if !names["telegram"] {
		t.Fatal("启动服务列表里丢了 telegram（Bot 仍在）")
	}
}

// TestTonServiceStartsWithoutConfig 没配 ton.native 时服务启动不报错、不绑通道。
func TestTonServiceStartsWithoutConfig(t *testing.T) {
	t.Cleanup(func() {
		ton.TonStop(context.Background())
		pay.Unbind(pay.KindTON)
	})
	svc := &tonService{}
	if svc.Name() != "ton" {
		t.Fatalf("服务名=%q，期望 ton", svc.Name())
	}
	ctx := corectx.WithCfg(context.Background(), &config.Cfg{})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("空配置启动 TON 服务不该报错: %v", err)
	}
	if _, err := pay.TransferFor(pay.KindTON); err == nil {
		t.Fatal("空配置把 TON 通道绑上了分派表")
	}
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("停止 TON 服务失败: %v", err)
	}
}
