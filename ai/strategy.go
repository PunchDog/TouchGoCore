package ai

import (
	"fmt"
	"sort"
	"strings"

	"touchgocore/config"
	"touchgocore/vars"
)

// Strategy 模型选择策略
type Strategy string

const (
	// StrategyDefault 使用默认提供方 + 其默认模型（默认行为）
	StrategyDefault Strategy = "default"
	// StrategyCheapest 按本次请求的预估消费自动选择最省钱的模型
	StrategyCheapest Strategy = "cheapest"
)

// defaultEstimateOutputTokens 请求未设置 max_tokens 时，用于成本估算的输出 token 数
const defaultEstimateOutputTokens = 256

// ParseStrategy 解析配置中的策略字符串；空值或未知取值都回退为 default（未知取值由 config.Validate 拦截）。
func ParseStrategy(s string) Strategy {
	if strings.EqualFold(strings.TrimSpace(s), config.ModelStrategyCheapest) {
		return StrategyCheapest
	}
	return StrategyDefault
}

// Selection 模型选择结果
type Selection struct {
	Provider   string  // 提供方名
	Model      string  // 模型名
	EstCost    float64 // 预估成本（美元）
	PriceKnown bool    // 所选模型是否配置了价格
	Fallback   bool    // 是否因无可用候选而回退到默认提供方默认模型
}

// String 便于日志与调试
func (s *Selection) String() string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("provider=%s model=%s est_cost=%.6fUSD price_known=%t fallback=%t",
		s.Provider, s.Model, s.EstCost, s.PriceKnown, s.Fallback)
}

// candidate 参与比价的候选（提供方 + 模型元信息）
type candidate struct {
	provider string
	info     ModelInfo
}

// EstimatePromptTokens 估算输入 token 数：消息内容、工具调用与工具定义的总字节数 / 2。
// 中文 UTF-8 约 3 字节/字、英文约 1 字节/字，除以 2 兼顾中英文；不引入分词依赖，
// 量级正确且排序稳定（价格差是选型主导因素，估算误差不影响结论）。
func EstimatePromptTokens(req *ChatRequest) int {
	if req == nil {
		return 1
	}
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.Name) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	if n <= 0 {
		return 1
	}
	return n/2 + 1
}

// EstimateCost 估算单次调用成本（美元）。
// 输出 token 取 req.MaxTokens，未设置时按 defaultEstimateOutputTokens 估计。
func EstimateCost(m ModelInfo, req *ChatRequest) float64 {
	in := EstimatePromptTokens(req)
	out := defaultEstimateOutputTokens
	if req != nil && req.MaxTokens > 0 {
		out = req.MaxTokens
	}
	return (float64(in)*m.InputPrice + float64(out)*m.OutputPrice) / 1e6
}

// hasTools 请求是否带工具定义
func hasTools(req *ChatRequest) bool { return req != nil && len(req.Tools) > 0 }

// pickCheapest 在候选中确定性地选出预估成本最低者。
// candidates 已按提供方名排序、同提供方内按配置顺序排列；仅当成本严格更小时才替换，
// 从而成本相同时结果可复现。无候选时返回 nil。
func pickCheapest(req *ChatRequest, cands []candidate) *Selection {
	var best *candidate
	bestCost := 0.0
	for i := range cands {
		c := &cands[i]
		cost := EstimateCost(c.info, req)
		if best == nil || cost < bestCost {
			best, bestCost = c, cost
		}
	}
	if best == nil {
		return nil
	}
	return &Selection{
		Provider:   best.provider,
		Model:      best.info.Name,
		EstCost:    bestCost,
		PriceKnown: true,
	}
}

// selectCheapest 在所有提供方的模型中选择预估成本最低者（调用方需持有读锁）。
//
// 两级筛选：
//  1. 先只保留配置了价格的模型（未配置价格无法比价）；
//  2. 请求带 tools 时，优先收敛到支持 Function Calling 的子集（该子集非空时才收敛）。
//
// 无可用候选时回退到默认提供方默认模型（告警 + Fallback 标记），
// 避免策略配置不全导致整体不可用，退化为旧行为。
func selectCheapest(clients map[string]*Client, defaultName string, req *ChatRequest) *Selection {
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names) // 按提供方名排序，保证结果确定性

	cands := make([]candidate, 0, len(names))
	for _, n := range names {
		for _, m := range clients[n].Models() {
			if !m.Priced() {
				continue
			}
			cands = append(cands, candidate{provider: n, info: m})
		}
	}
	if len(cands) == 0 {
		vars.Warning("model_api 策略[%s]无已配置价格的模型，回退到默认提供方[%s]", StrategyCheapest, defaultName)
		return fallbackSelection(clients, defaultName, req)
	}
	if hasTools(req) {
		toolCands := make([]candidate, 0, len(cands))
		for _, c := range cands {
			if c.info.SupportsTools {
				toolCands = append(toolCands, c)
			}
		}
		if len(toolCands) > 0 {
			cands = toolCands
		}
	}
	if sel := pickCheapest(req, cands); sel != nil {
		return sel
	}
	return fallbackSelection(clients, defaultName, req)
}

// fallbackSelection 回退到默认提供方的默认模型
func fallbackSelection(clients map[string]*Client, defaultName string, req *ChatRequest) *Selection {
	c := clients[defaultName]
	if c == nil {
		return nil
	}
	sel := &Selection{Provider: c.Name(), Model: c.DefaultModel(), Fallback: true}
	for _, m := range c.Models() {
		if m.Name == sel.Model && m.Priced() {
			sel.EstCost = EstimateCost(m, req)
			sel.PriceKnown = true
			break
		}
	}
	return sel
}

// resolveSelection 解析"非策略路径"最终使用的提供方与模型（含价格信息，仅供预览）
func resolveSelection(c *Client, req *ChatRequest) *Selection {
	m := ""
	if req != nil {
		m = strings.TrimSpace(req.Model)
	}
	if m == "" {
		m = c.DefaultModel()
	}
	sel := &Selection{Provider: c.Name(), Model: m}
	for _, mi := range c.Models() {
		if mi.Name == m && mi.Priced() {
			sel.EstCost = EstimateCost(mi, req)
			sel.PriceKnown = true
			break
		}
	}
	return sel
}
