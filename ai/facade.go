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

	facadeMu.Lock()
	clients = m
	defaultName = def
	facadeMu.Unlock()

	vars.Info("模型API服务已启动，共 %d 个提供方，默认: %s", len(m), def)
	return nil
}

// Stop 停止模型API服务
func Stop(ctx context.Context) {
	facadeMu.Lock()
	clients = nil
	defaultName = ""
	facadeMu.Unlock()
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

// Chat 便捷入口：同步调用 chat/completions。name 为提供方名，空 = 默认提供方。
func Chat(ctx context.Context, name string, req *ChatRequest) (*ChatResponse, error) {
	facadeMu.RLock()
	n := name
	if n == "" {
		n = defaultName
	}
	c := clients[n]
	facadeMu.RUnlock()
	if c == nil {
		return nil, fmt.Errorf("模型提供方[%s]不可用（model_api 未配置或 enable=off）", n)
	}
	return c.Chat(ctx, req)
}

// ListProviders 返回可用提供方名列表（已排序）
func ListProviders() []string {
	facadeMu.RLock()
	defer facadeMu.RUnlock()
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
