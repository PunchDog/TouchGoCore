package ai

import "encoding/json"

// Role 消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 对话消息
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时必填
}

// FunctionCall 函数调用
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// ToolCall 工具调用
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionDef 函数定义
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema，RawMessage 避免二次转义
}

// Tool 工具定义
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// ModelInfo 模型元信息（用于"最省消费"策略比价与能力判断）
type ModelInfo struct {
	Name          string  // 模型名
	InputPrice    float64 // 输入价格（美元/百万 token），0 表示未配置
	OutputPrice   float64 // 输出价格（美元/百万 token），0 表示未配置
	SupportsTools bool    // 是否支持 Function Calling
}

// Priced 是否配置了价格（未配置价格的模型不参与"最省消费"比价）
func (m ModelInfo) Priced() bool { return m.InputPrice > 0 || m.OutputPrice > 0 }

// ChatRequest 对话补全请求
type ChatRequest struct {
	Model       string    `json:"model,omitempty"` // 为空时由策略或提供方默认模型决定
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

// Usage token 用量
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice 候选结果
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatResponse 对话补全响应
type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// APIError OpenAI 标准错误体
type APIError struct {
	Message string      `json:"message"`
	Type    string      `json:"type"`
	Code    interface{} `json:"code"`
}

// Error 实现 error 接口
func (e *APIError) Error() string { return e.Message }

// errorBody 错误响应体
type errorBody struct {
	Error APIError `json:"error"`
}
