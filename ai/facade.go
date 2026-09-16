package ai

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"touchgocore/corectx"
	"touchgocore/vars"
)

var (
	facadeMu    sync.RWMutex
	clients     map[string]*Client
	defaultName string
	strategy    = StrategyDefault
	priceSrc    *PriceSource // 在线价格源；nil = 未启用（仅本地配置价格）
	pressure    = NewPressureTracker() // 调用压力跟踪（并发在途 + RPM 滑动窗口）
)

// Run 启动模型API服务，初始化所有已配置的提供方。
// 配置为 nil、enable=off 或 providers 为空时不启动。
func Run(ctx context.Context) error {
	cfg := corectx.CfgFrom(ctx)
	if cfg == nil || cfg.ModelAPI == nil {
		vars.Info("model_api 未配置，模型API服务不启动")
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(cfg.ModelAPI.Enable), "off") {
		vars.Info("model_api enable=off，模型API服务不启动")
		return nil
	}
	if len(cfg.ModelAPI.Providers) == 0 {
		vars.Info("model_api providers 为空，模型API服务不启动")
		return nil
	}

	m := make(map[string]*Client, len(cfg.ModelAPI.Providers))
	for name, pc := range cfg.ModelAPI.Providers {
		if pc == nil {
			continue
		}
		c, err := NewClient(name, pc)
		if err != nil {
			return fmt.Errorf("初始化模型提供方[%s]失败: %w", name, err)
		}
		m[name] = c
	}
	if len(m) == 0 {
		vars.Info("model_api 无有效提供方，模型API服务不启动")
		return nil
	}

	def := strings.TrimSpace(cfg.ModelAPI.Default)
	if def == "" {
		for k := range m {
			def = k // 仅一个提供方时以其为默认
		}
	}
	if _, ok := m[def]; !ok {
		return fmt.Errorf("model_api.default=%s 不存在于 providers", def)
	}
	st := ParseStrategy(cfg.ModelAPI.Strategy)

	// cheapest 策略且配置了在线价格源时，异步预热价格表（不阻塞启动）
	var src *PriceSource
	if st == StrategyCheapest {
		if s := NewPriceSource(cfg.ModelAPI.PriceSource); s != nil {
			src = s
			s.Prefetch()
		}
	}

	facadeMu.Lock()
	clients = m
	defaultName = def
	strategy = st
	priceSrc = src
	facadeMu.Unlock()

	vars.Info("模型API服务已启动，共 %d 个提供方，默认: %s，策略: %s", len(m), def, st)
	return nil
}

// Stop 停止模型API服务
func Stop(ctx context.Context) {
	facadeMu.Lock()
	clients = nil
	defaultName = ""
	strategy = StrategyDefault
	priceSrc = nil
	facadeMu.Unlock()
	pressure.Reset() // 清空压力统计，避免残留状态影响下次启动
	vars.Info("模型API服务已停止")
}

// GetClient 按提供方名获取客户端；name 为空时返回默认提供方。
func GetClient(name string) *Client {
	facadeMu.RLock()
	defer facadeMu.RUnlock()
	if name == "" {
		name = defaultName
	}
	return clients[name]
}

// Chat 便捷入口：同步调用 chat/completions。
// 路由优先级：指定提供方（name）→ 指定模型（req.Model 跨提供方反查）→ cheapest 策略 → 默认提供方。
// name 与 req.Model 均为空时才走策略自动选择，显式指定始终优先于策略。
func Chat(ctx context.Context, name string, req *ChatRequest) (*ChatResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}

	facadeMu.RLock()
	c, sel, err := route(name, req)
	facadeMu.RUnlock()
	if err != nil {
		return nil, err
	}

	if sel != nil {
		vars.Debug("model_api 自动选择模型: %s", sel)
		out := *req // 浅拷贝，避免把所选模型回写到调用方请求
		out.Model = sel.Model
		release := pressure.Acquire(sel.Provider, sel.Model)
		resp, err := c.Chat(ctx, &out)
		release()
		return resp, err
	}

	// 显式路径：不受压力阈值限制（调用方自负责），但仍计数以保证压力统计准确
	m := strings.TrimSpace(req.Model)
	if m == "" {
		m = c.DefaultModel()
	}
	release := pressure.Acquire(c.Name(), m)
	resp, err := c.Chat(ctx, req)
	release()
	return resp, err
}

// SelectModel 只做模型选择、不发起调用，便于调用方预览本次请求将使用的提供方、模型与预估成本。
// 语义与 Chat 的自动路由一致（即在未指定提供方、未指定模型的前提下做选择）。
func SelectModel(req *ChatRequest) (*Selection, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}
	facadeMu.RLock()
	defer facadeMu.RUnlock()
	if len(clients) == 0 {
		return nil, fmt.Errorf("model_api 未配置或 enable=off")
	}
	c, sel, err := route("", req)
	if err != nil {
		return nil, err
	}
	if sel != nil {
		return sel, nil
	}
	return resolveSelection(c, req, priceSrc), nil
}

// CurrentStrategy 返回当前生效的模型选择策略。
// （策略类型已占用 Strategy 名称，故访问器取名为 CurrentStrategy。）
func CurrentStrategy() Strategy {
	facadeMu.RLock()
	defer facadeMu.RUnlock()
	return strategy
}

// ListProviders 返回可用提供方名列表（已排序）
func ListProviders() []string {
	facadeMu.RLock()
	defer facadeMu.RUnlock()
	return providerNames()
}

// providerNames 返回提供方名列表（已排序；调用方需持有锁）
func providerNames() []string {
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// route 路由决策，返回选中的客户端与（走策略时的）选择结果。调用方需持有读锁。
func route(name string, req *ChatRequest) (*Client, *Selection, error) {
	// 1. 显式指定提供方
	if n := strings.TrimSpace(name); n != "" {
		c := clients[n]
		if c == nil {
			return nil, nil, fmt.Errorf("模型提供方[%s]不存在（可用: %s）", n, strings.Join(providerNames(), ", "))
		}
		return c, nil, nil
	}

	// 2. 显式指定模型：跨提供方反查拥有该模型的提供方
	if m := strings.TrimSpace(req.Model); m != "" {
		if c := findByModel(m); c != nil {
			return c, nil, nil
		}
		// 未在任何提供方 models 中登记：沿用旧行为，交给默认提供方透传该模型
		if c := clients[defaultName]; c != nil {
			return c, nil, nil
		}
		return nil, nil, fmt.Errorf("模型提供方[%s]不可用（model_api 未配置或 enable=off）", defaultName)
	}

	// 3. cheapest 策略：按预估消费自动选择（价格来自本地配置与在线价格源缓存，
	//    并考虑调用压力：最优达到阈值时溢出到下一个候选）
	if strategy == StrategyCheapest {
		sel := selectCheapest(clients, defaultName, req, priceSrc, pressure)
		if sel == nil {
			return nil, nil, fmt.Errorf("model_api 无可用模型（未配置或 enable=off）")
		}
		c := clients[sel.Provider]
		if c == nil {
			return nil, nil, fmt.Errorf("模型提供方[%s]不可用", sel.Provider)
		}
		return c, sel, nil
	}

	// 4. 默认提供方
	c := clients[defaultName]
	if c == nil {
		return nil, nil, fmt.Errorf("模型提供方[%s]不可用（model_api 未配置或 enable=off）", defaultName)
	}
	return c, nil, nil
}

// findByModel 按模型名跨提供方反查；多个提供方都配置了同名模型时取提供方名最小者（保证确定性）。
// 调用方需持有读锁。
func findByModel(model string) *Client {
	for _, n := range providerNames() {
		if clients[n].HasModel(model) {
			return clients[n]
		}
	}
	return nil
}
