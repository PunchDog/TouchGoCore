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
	EstCost    float64 // 预估成本（美元）；无价格模型为 0
	NoPrice    bool    // 无价格标签：本地与在线均无价格（视为免费，如同层免费模型优先选中）
	Fallback   bool    // 是否因无可用候选而回退到默认提供方默认模型
	Overloaded bool    // 所有候选均达到调用压力阈值时的降级标记（仍选了排序最优者）
}

// String 便于日志与调试
func (s *Selection) String() string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("provider=%s model=%s est_cost=%.6fUSD no_price=%t fallback=%t overloaded=%t",
		s.Provider, s.Model, s.EstCost, s.NoPrice, s.Fallback, s.Overloaded)
}

// candidate 参与比价的候选（提供方 + 模型元信息 + 解析后的价格）
type candidate struct {
	provider   string
	info       ModelInfo
	priced     bool    // 价格是否可知（本地配置或在线获取）
	cost       float64 // 预估成本（美元）；priced=false 时无意义
	overloaded bool    // 是否已达到调用压力阈值（并发或 RPM 任一超限）
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

// selectCheapest 分层筛选最省消费的模型（调用方需持有读锁）。
//
// 流程：
//  1. 收集候选：价格解析按"本地配置价格 → 在线价格源 → 无价格标签"三级进行，
//     并按模型/提供方级限额判断调用压力（并发在途数、RPM 滑动窗口任一超限即超载）；
//  2. 请求带 tools 时优先收敛到支持 Function Calling 的子集（该子集非空才收敛）；
//  3. 全序排序：priority 升序分层；同层内无价格模型优先（视为免费，成本 0），
//     其余按预估成本升序（稳定排序保证确定性）；
//  4. 压力溢出：依序取第一个未达到压力阈值的候选；最优模型达到阈值时自动溢出到下一个；
//  5. 全部候选均超限时软降级：仍选排序最优者并告警（Overloaded 标记），保证业务不阻断；
//  6. 无任何候选时回退默认提供方默认模型（Fallback 标记）。
func selectCheapest(clients map[string]*Client, defaultName string, req *ChatRequest, src *PriceSource, tracker *PressureTracker) *Selection {
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names) // 按提供方名排序，保证结果确定性

	cands := make([]candidate, 0, len(names))
	for _, n := range names {
		for _, m := range clients[n].Models() {
			c := candidate{provider: n, info: m}
			if m.Priced() {
				c.priced = true
				c.cost = EstimateCost(m, req)
			} else if q, ok := src.Lookup(n, m.Name); ok {
				// 本地未配置价格，使用在线价格
				c.priced = true
				c.cost = EstimateCost(ModelInfo{InputPrice: q.Input, OutputPrice: q.Output}, req)
			}
			if mc, rpm := clients[n].LimitsFor(m.Name); tracker.Overloaded(n, m.Name, mc, rpm) {
				c.overloaded = true
			}
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		vars.Warning("model_api 策略[%s]无可用模型候选，回退到默认提供方[%s]", StrategyCheapest, defaultName)
		return fallbackSelection(clients, defaultName, req, src)
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

	// 全序排序：优先级升序；同层内无价格优先，其余按预估成本升序（稳定排序保证确定性）
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].info.Priority != cands[j].info.Priority {
			return cands[i].info.Priority < cands[j].info.Priority
		}
		pi, pj := candidateRank(cands[i]), candidateRank(cands[j])
		if pi != pj {
			return pi < pj
		}
		return cands[i].cost < cands[j].cost
	})

	// 压力溢出：依序取第一个未超限的候选（最优达到阈值时自动看下一个）
	for i := range cands {
		if cands[i].overloaded {
			continue
		}
		return selectionOf(&cands[i])
	}
	// 全部超限：软降级仍选最优并告警
	vars.Warning("model_api 所有候选模型均达到调用压力阈值，降级选用最优模型[%s/%s]",
		cands[0].provider, cands[0].info.Name)
	return selectionOf(&cands[0], true)
}

// candidateRank 同层内排序权重：无价格模型（免费）为 0，有价格模型为 1
func candidateRank(c candidate) int {
	if c.priced {
		return 1
	}
	return 0
}

// selectionOf 由候选构造选择结果；degrade 表示全超限降级（最优本身超限）
func selectionOf(c *candidate, degrade ...bool) *Selection {
	sel := &Selection{Provider: c.provider, Model: c.info.Name, EstCost: c.cost}
	if !c.priced {
		sel.NoPrice = true
		sel.EstCost = 0
	}
	if len(degrade) > 0 && degrade[0] {
		sel.Overloaded = true
	}
	return sel
}

// fallbackSelection 回退到默认提供方的默认模型
func fallbackSelection(clients map[string]*Client, defaultName string, req *ChatRequest, src *PriceSource) *Selection {
	c := clients[defaultName]
	if c == nil {
		return nil
	}
	sel := &Selection{Provider: c.Name(), Model: c.DefaultModel(), Fallback: true}
	resolvePrice(c, sel.Model, req, src, sel)
	return sel
}

// resolvePrice 按三级解析填充选择结果的价格信息：本地配置价格 → 在线价格源 → 无价格标签。
func resolvePrice(c *Client, model string, req *ChatRequest, src *PriceSource, sel *Selection) {
	for _, m := range c.Models() {
		if m.Name == model && m.Priced() {
			sel.EstCost = EstimateCost(m, req)
			return
		}
	}
	if q, ok := src.Lookup(c.Name(), model); ok {
		sel.EstCost = EstimateCost(ModelInfo{InputPrice: q.Input, OutputPrice: q.Output}, req)
		return
	}
	sel.NoPrice = true
}

// resolveSelection 解析"非策略路径"最终使用的提供方与模型（含价格信息，仅供预览）
func resolveSelection(c *Client, req *ChatRequest, src *PriceSource) *Selection {
	m := ""
	if req != nil {
		m = strings.TrimSpace(req.Model)
	}
	if m == "" {
		m = c.DefaultModel()
	}
	sel := &Selection{Provider: c.Name(), Model: m}
	resolvePrice(c, m, req, src, sel)
	return sel
}
