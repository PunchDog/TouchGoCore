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

func WithCfg(ctx context.Context, cfg *config.Cfg) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return ctx
	}
	return context.WithValue(ctx, cfgKey{}, cfg)
}

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

func AppViewFrom(ctx context.Context) *AppView {
	if ctx != nil {
		if view, ok := ctx.Value(viewKey{}).(*AppView); ok {
			return view
		}
	}
	if config.Cfg_ == nil {
		return nil
	}
	return &AppView{ServerName: config.ServerName_, Cfg: config.Cfg_}
}
