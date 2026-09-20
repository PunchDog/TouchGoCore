package corectx

import (
	"context"

	"touchgocore/config"
)

type cfgKey struct{}
type viewKey struct{}

// AppView 是 App 在各模块中的只读视图，避免模块反向依赖 touchgocore 包。
type AppView struct {
	ServerName string
	Cfg        *config.Cfg
}

// WithCfg 把配置挂到 ctx 上。
//
// cfg 为 nil 时按「不注入」处理，原样返回 ctx：下游一律回落到全局配置，
// 这与注入一份空 Cfg 的含义不同（空 Cfg 表示各段均为 nil）。
// 这里不改成哨兵值，是因为现存调用方（含 App 装配路径）大量传入可能为 nil 的
// 配置，把它们集体降级成「CfgFrom 返回 nil」会让各模块的判空路径同时改道。
func WithCfg(ctx context.Context, cfg *config.Cfg) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return ctx
	}
	return context.WithValue(ctx, cfgKey{}, cfg)
}

// CfgFrom 从 ctx 取出配置。
//
// 取值顺序：显式注入的 cfg → AppView.Cfg → 全局 config.Cfg_。
// 最后一级是为兼容「不经过 App」的旧调用方保留的兜底：它读的是包级变量，
// 与 App 的装配并发时属于已废弃路径，新代码应始终从 App.ctx 派生上下文。
func CfgFrom(ctx context.Context) *config.Cfg {
	if ctx != nil {
		if cfg, ok := ctx.Value(cfgKey{}).(*config.Cfg); ok && cfg != nil {
			return cfg
		}
		if view, ok := ctx.Value(viewKey{}).(*AppView); ok && view != nil && view.Cfg != nil {
			return view.Cfg
		}
	}
	return config.Cfg_
}

func WithAppView(ctx context.Context, view *AppView) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if view == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, viewKey{}, view)
	if view.Cfg != nil {
		ctx = WithCfg(ctx, view.Cfg)
	}
	return ctx
}

// AppViewFrom 取出 App 视图；ctx 未注入时按全局配置合成一份兜底视图。
//
// 两个分支都返回副本：ctx 里存的是全 App 共享的同一个 *AppView，
// 直接把它交给调用方等于允许任何一方改 ServerName / Cfg 指针被所有链路看见。
// 需要写回的场景请走 App 自身的装配路径，不要改这里的返回值。
func AppViewFrom(ctx context.Context) *AppView {
	if ctx != nil {
		if view, ok := ctx.Value(viewKey{}).(*AppView); ok && view != nil {
			copied := *view
			return &copied
		}
	}
	if config.Cfg_ == nil {
		return nil
	}
	return &AppView{ServerName: config.ServerName_, Cfg: config.Cfg_}
}
