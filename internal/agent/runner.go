// Package agent 是执行层：把会话历史交给模型，边跑边吐增量事件。
//
// 该包不感知 HTTP / SSE / 存储实现，只依赖 session 的领域类型与 tool 的元信息，
// 因此换执行引擎（自研 tool loop、多模型路由）不影响协议层。
package agent

import (
	"context"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/tool"
)

// EventType 执行层事件类型。
type EventType string

const (
	// TypeDelta 文本增量。
	TypeDelta EventType = "delta"
	// TypeDone 生成正常结束（携带完整文本与用量）。
	TypeDone EventType = "done"
	// TypeError 生成失败。
	TypeError EventType = "error"
	// TypeToolCall 模型请求调用工具。
	TypeToolCall EventType = "tool.call"
	// TypeToolResult 工具执行完成（结果将回喂给模型）。
	TypeToolResult EventType = "tool.result"
	// TypeToolApproval 工具需要人工审批（危险操作）。
	TypeToolApproval EventType = "tool.approval"
)

// Usage token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ToolCall 一次工具调用请求。
type ToolCall struct {
	// ID 由模型供应商返回（OpenAI 的 tool_call_id / Anthropic 的 tool_use id）。
	ID string
	// Name 工具名。
	Name string
	// Args 参数的 JSON 字符串（原样保留）。
	Args string
	// Risk 该工具的危险等级（供上层决定是否要审批）。
	Risk tool.Risk
	// RequiresApproval 是否需要人工确认后才执行。
	RequiresApproval bool
}

// ToolResult 工具执行结果。
type ToolResult struct {
	CallID string
	// Name 工具名（便于日志与 UI 展示）。
	Name string
	// Output 返回给模型的文本。
	Output string
	// IsError 表示工具执行失败。
	IsError bool
	// Rejected 表示用户拒绝了该操作（不是错误，而是"未执行"）。
	Rejected bool
	// DurationMs 执行耗时（毫秒）。
	DurationMs int64
}

// Event 执行层事件。
type Event struct {
	Type    EventType
	Delta   string // TypeDelta
	Content string // TypeDone 时的完整文本
	Usage   *Usage // TypeDone

	ToolCall   *ToolCall   // TypeToolCall / TypeToolApproval
	ToolResult *ToolResult // TypeToolResult

	Err error // TypeError
}

// Request 是一次模型调用的输入。
type Request struct {
	// History 到当前步为止的完整对话（含 tool 消息）。
	History []session.Message
	// Tools 本轮可用的工具（nil 表示不启用工具）。
	Tools []tool.Spec
	// SystemPrompt 系统提示（部分协议需要单独传）。
	SystemPrompt string
}

// Runner 是「单次模型调用」的边界。
//
// 职责边界：Runner 只负责一次请求-响应（含流式增量），
// 多轮工具循环由 loop 负责。这样协议层不需要知道工具怎么执行，
// 工具层不需要知道用什么模型。
type Runner interface {
	Run(ctx context.Context, req Request) <-chan Event
}

// Named 便于日志与 /api/health 展示。
type Named interface {
	Name() string
}
