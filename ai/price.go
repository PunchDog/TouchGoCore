package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/vars"
)

// 默认价格源地址
const (
	defaultLiteLLMURL    = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	defaultOpenRouterURL = "https://openrouter.ai/api/v1/models"
)

const (
	defaultPriceCacheTTL = 24 * time.Hour
	defaultPriceTimeout  = 10 * time.Second
	// maxPriceBodyBytes 价格表响应体上限（LiteLLM 全量 JSON 约 2~3MB，留足余量）
	maxPriceBodyBytes = 32 << 20
	// failBackoff 拉取失败后的最小重试间隔，防止请求风暴
	failBackoff = time.Minute
)

// PriceQuote 单个模型价格（美元/百万 token）
type PriceQuote struct {
	Input  float64
	Output float64
}

// PriceSource 在线价格源：整表拉取 + TTL 缓存 + 单飞刷新。
// 两个价格源都是"一次请求返回全部模型价格"，按整表缓存远优于按模型逐个请求。
// 刷新在互斥锁内同步进行（单飞）：并发查询只触发一次网络请求，其余等待后读表。
type PriceSource struct {
	provider string // litellm / openrouter
	url      string
	ttl      time.Duration
	timeout  time.Duration
	http     *http.Client

	mu        sync.Mutex
	table     map[string]PriceQuote // 键含原始名与小写名，便于大小写不敏感匹配
	fetchedAt time.Time
	failAt    time.Time // 上次失败时间；failBackoff 内不再发起请求
}

// NewPriceSource 按配置创建价格源；未配置或 provider=off/非法时返回 nil（不获取在线价格）。
func NewPriceSource(cfg *config.PriceSourceConfig) *PriceSource {
	provider := cfg.EffectiveProvider()
	if provider == "" {
		return nil
	}
	ttl := time.Duration(cfg.CacheTTL) * time.Second
	if ttl <= 0 {
		ttl = defaultPriceCacheTTL
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultPriceTimeout
	}
	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		if provider == config.PriceSourceLiteLLM {
			url = defaultLiteLLMURL
		} else {
			url = defaultOpenRouterURL
		}
	}
	return &PriceSource{
		provider: provider,
		url:      url,
		ttl:      ttl,
		timeout:  timeout,
		http:     &http.Client{},
	}
}

// Lookup 查询模型价格（美元/百万 token）。三级名称匹配：精确 → 小写 → 提供方名前缀。
// 缓存过期或为空时单飞刷新整表（表新但未命中不刷新：整表已含全市场价格）；
// 失败时告警并进入短退避，返回 ok=false（不致命）。
func (p *PriceSource) Lookup(providerName, model string) (PriceQuote, bool) {
	if p == nil {
		return PriceQuote{}, false
	}
	m := strings.TrimSpace(model)
	if m == "" {
		return PriceQuote{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// 表非空且未过期：直接读表（命中与否都不再请求网络）
	if len(p.table) > 0 && time.Since(p.fetchedAt) < p.ttl {
		return p.getLocked(providerName, m)
	}
	// 表为空（从未成功拉取）或已过期 → 单飞刷新；失败退避期内不重复请求
	if p.failAt.IsZero() || time.Since(p.failAt) >= failBackoff {
		p.fetchLocked()
	}
	return p.getLocked(providerName, m)
}

// Prefetch 异步预热价格表（不阻塞调用方；持锁拉取，期间并发查询会等待后直接读表）
func (p *PriceSource) Prefetch() {
	if p == nil {
		return
	}
	go func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.table) > 0 && time.Since(p.fetchedAt) < p.ttl {
			return
		}
		p.fetchLocked()
	}()
}

// getLocked 从缓存表查价格。调用方需持有 p.mu。
func (p *PriceSource) getLocked(providerName, model string) (PriceQuote, bool) {
	if len(p.table) == 0 {
		return PriceQuote{}, false
	}
	keys := []string{
		model,
		strings.ToLower(model),
		providerName + "/" + model,
		strings.ToLower(providerName) + "/" + strings.ToLower(model),
	}
	for _, k := range keys {
		if q, ok := p.table[k]; ok {
			return q, true
		}
	}
	return PriceQuote{}, false
}

// fetchLocked 拉取并解析整张价格表。调用方需持有 p.mu。
func (p *PriceSource) fetchLocked() {
	table, err := p.fetchTable()
	if err != nil {
		p.failAt = time.Now()
		vars.Warning("model_api 价格源[%s]拉取失败: %v（%v 内不再重试）", p.provider, err, failBackoff)
		return
	}
	p.table = table
	p.fetchedAt = time.Now()
	p.failAt = time.Time{} // 成功后清除失败退避
	vars.Info("model_api 价格源[%s]已加载 %d 个模型价格", p.provider, len(table))
}

// fetchTable 请求并解析价格源
func (p *PriceSource) fetchTable() (map[string]PriceQuote, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPriceBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取价格表失败: %w", err)
	}
	switch p.provider {
	case config.PriceSourceLiteLLM:
		return parseLiteLLM(data)
	case config.PriceSourceOpenRouter:
		return parseOpenRouter(data)
	default:
		return nil, fmt.Errorf("未知价格源: %s", p.provider)
	}
}

// litellmEntry LiteLLM 价格 JSON 条目（指针字段容忍缺省；计价单位为美元/token）
type litellmEntry struct {
	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`
}

// parseLiteLLM 解析 LiteLLM model_prices JSON，换算为美元/百万 token
func parseLiteLLM(data []byte) (map[string]PriceQuote, error) {
	var raw map[string]litellmEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 litellm 价格表失败: %w", err)
	}
	out := make(map[string]PriceQuote, len(raw))
	for name, e := range raw {
		q := PriceQuote{}
		valid := false
		if e.InputCostPerToken != nil && *e.InputCostPerToken >= 0 {
			q.Input = *e.InputCostPerToken * 1e6
			valid = true
		}
		if e.OutputCostPerToken != nil && *e.OutputCostPerToken >= 0 {
			q.Output = *e.OutputCostPerToken * 1e6
			valid = true
		}
		if !valid {
			continue // 价格缺省或非法（如负数）的条目跳过
		}
		addPriceKey(out, name, q)
	}
	return out, nil
}

// openRouterModel OpenRouter models 条目（pricing 为字符串，美元/token；免费模型为 "0"）
type openRouterModel struct {
	ID      string `json:"id"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

// parseOpenRouter 解析 OpenRouter /api/v1/models，换算为美元/百万 token；
// 非数值（如 "-1"）按缺省处理，两个价格都非法的条目跳过。
func parseOpenRouter(data []byte) (map[string]PriceQuote, error) {
	var raw struct {
		Data []openRouterModel `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 openrouter 价格表失败: %w", err)
	}
	out := make(map[string]PriceQuote, len(raw.Data))
	for _, m := range raw.Data {
		q := PriceQuote{}
		valid := false
		if v, err := parsePriceValue(m.Pricing.Prompt); err == nil {
			q.Input = v
			valid = true
		}
		if v, err := parsePriceValue(m.Pricing.Completion); err == nil {
			q.Output = v
			valid = true
		}
		if !valid {
			continue // 价格全非法（如 "-1"）的条目跳过
		}
		addPriceKey(out, m.ID, q)
	}
	return out, nil
}

// parsePriceValue 解析 OpenRouter 价格字符串（美元/token）；非数值或负数视为非法
func parsePriceValue(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("非法价格: %q", s)
	}
	return v * 1e6, nil
}

// addPriceKey 写入原始键与小写键，便于大小写不敏感匹配
func addPriceKey(table map[string]PriceQuote, name string, q PriceQuote) {
	table[name] = q
	if low := strings.ToLower(name); low != name {
		if _, exists := table[low]; !exists {
			table[low] = q
		}
	}
}
