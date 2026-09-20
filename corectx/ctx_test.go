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

// TestWithCfgNilMeansNoInjection 回归（S60）：注入 nil 等于不注入，
// 下游继续按全局兜底；显式注入的空 Cfg 才是「各段均为 nil」。
func TestWithCfgNilMeansNoInjection(t *testing.T) {
	prev := config.Cfg_
	config.Cfg_ = &config.Cfg{LogLevel: "info"}
	t.Cleanup(func() { config.Cfg_ = prev })

	if got := CfgFrom(WithCfg(context.Background(), nil)); got != config.Cfg_ {
		t.Fatalf("✘ nil 注入改变了兜底语义: %v", got)
	}
	empty := &config.Cfg{}
	if got := CfgFrom(WithCfg(context.Background(), empty)); got != empty {
		t.Fatal("✘ 显式空 Cfg 被全局兜底覆盖")
	}
	// nil view 等价于不注入：既不返回 nil ctx，也不冲掉链上已有的视图
	ctx := WithAppView(context.Background(), &AppView{ServerName: "gate"})
	if got := AppViewFrom(WithAppView(ctx, nil)); got == nil || got.ServerName != "gate" {
		t.Fatalf("✘ nil view 覆盖了已有视图: %#v", got)
	}
}

// TestCfgFromPrecedence 回归（S60）：显式 cfg 必须压过视图里的 cfg，
// 否则同一 ctx 上两次注入的优先级不确定。
func TestCfgFromPrecedence(t *testing.T) {
	viewCfg := &config.Cfg{LogLevel: "warn"}
	injected := &config.Cfg{LogLevel: "debug"}
	ctx := WithAppView(context.Background(), &AppView{ServerName: "gate", Cfg: viewCfg})
	ctx = WithCfg(ctx, injected)
	if got := CfgFrom(ctx); got != injected {
		t.Fatalf("✘ 后注入的 cfg 未压过视图: %v", got)
	}

	onlyView := WithAppView(context.Background(), &AppView{ServerName: "gate", Cfg: viewCfg})
	if got := CfgFrom(onlyView); got != viewCfg {
		t.Fatal("✘ 视图兜底未生效")
	}
}

// TestAppViewFromReturnsCopy 回归（S60）：交出的是副本。修复前直接返回 ctx 中
// 共享的那个指针，任何一处改 view.ServerName 都会串到全 App 的所有链路。
func TestAppViewFromReturnsCopy(t *testing.T) {
	view := &AppView{ServerName: "gate", Cfg: &config.Cfg{LogLevel: "warn"}}
	ctx := WithAppView(context.Background(), view)

	got := AppViewFrom(ctx)
	if got == view {
		t.Fatal("✘ 返回了共享的视图指针，调用方可原地改写全链路")
	}
	got.ServerName = "tampered"
	if view.ServerName != "gate" {
		t.Fatalf("✘ 视图被调用方改写: %s", view.ServerName)
	}
	if again := AppViewFrom(ctx); again.ServerName != "gate" {
		t.Fatalf("✘ 后续取到的视图被污染: %s", again.ServerName)
	}
	if AppViewFrom(context.Background()) == nil && config.Cfg_ != nil {
		t.Fatal("✘ 未注入视图时应按全局兜底")
	}
}
