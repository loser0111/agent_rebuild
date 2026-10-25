// Package sse 提供 SSE（text/event-stream）的事件定义、编码与写入能力。
//
// 设计要点：
//   - 只依赖标准库，手写编码器，精确控制 id/event/data 与心跳；
//   - 事件名以常量化形式暴露，前后端共享同一套命名（见 docs/02-api-and-sse.md）；
//   - 不做业务判断，不感知 session / agent。
package sse

import "time"

// 事件名常量。新增事件只允许追加，不允许改名（协议兼容约束）。
const (
	EventMessageStart       = "message.start"
	EventMessageDelta       = "message.delta"
	EventMessageDone        = "message.done"
	EventMessageError       = "message.error"
	EventMessageInterrupted = "message.interrupted"
	EventPing               = "ping"
	EventError              = "error"
	EventResync             = "resync"

	// 工具相关事件（参考 Codex / Claude Code 的展示方式）。
	EventToolCall     = "tool.call"     // 模型请求调用工具
	EventToolResult   = "tool.result"   // 工具执行完成
	EventToolApproval = "tool.approval" // 需要人工审批
)

// ToolCallData 工具调用事件负载。
type ToolCallData struct {
	Seq       uint64 `json:"seq"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	// CallID 工具调用 ID（结果事件用同一 ID 关联）。
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Args 参数的 JSON 字符串（前端可折叠展示）。
	Args string `json:"args"`
	// Risk 危险等级：safe / moderate / dangerous。
	Risk string `json:"risk"`
	// RequiresApproval 是否需要用户确认。
	RequiresApproval bool `json:"requires_approval"`
}

// ToolResultData 工具结果事件负载。
type ToolResultData struct {
	Seq       uint64 `json:"seq"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	// Output 工具输出（可能被截断）。
	Output string `json:"output"`
	// IsError 工具执行失败。
	IsError bool `json:"is_error"`
	// Rejected 用户拒绝了该操作。
	Rejected bool `json:"rejected"`
	// DurationMs 执行耗时。
	DurationMs int64 `json:"duration_ms"`
}

// ToolApprovalData 审批请求负载。
type ToolApprovalData struct {
	Seq       uint64 `json:"seq"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Args      string `json:"args"`
	Risk      string `json:"risk"`
	Reason    string `json:"reason,omitempty"`
}

// Event 是待发送的一帧。ID 为会话维度单调递增整数，用于 Last-Event-ID 重放。
type Event struct {
	ID    uint64
	Name  string
	Data  any // JSON 可序列化；序列化失败由 Writer 兜底为流级 error 事件
	Retry int // 毫秒，仅首个事件前发送一次，0 表示不发送
}

// PingData 心跳负载。
type PingData struct {
	TS time.Time `json:"ts"`
}

// MessageStartData 助手消息开始。
type MessageStartData struct {
	Seq       uint64    `json:"seq"`
	SessionID string    `json:"session_id"`
	MessageID string    `json:"message_id"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
}

// MessageDeltaData 增量文本。
type MessageDeltaData struct {
	Seq       uint64 `json:"seq"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Delta     string `json:"delta"`
}

// MessageDoneData 生成正常结束。
type MessageDoneData struct {
	Seq          uint64 `json:"seq"`
	SessionID    string `json:"session_id"`
	MessageID    string `json:"message_id"`
	Content      string `json:"content"`
	FinishReason string `json:"finish_reason"`
	Usage        *Usage `json:"usage,omitempty"`
}

// MessageInterruptedData 客户端取消或服务端超时。
type MessageInterruptedData struct {
	Seq       uint64 `json:"seq"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
}

// MessageErrorData 单条消息生成失败。
type MessageErrorData struct {
	Seq       uint64        `json:"seq"`
	SessionID string        `json:"session_id"`
	MessageID string        `json:"message_id"`
	Error     *ErrorPayload `json:"error"`
}

// ErrorData 流级错误（不属于某条消息）。
type ErrorData struct {
	Error     *ErrorPayload `json:"error"`
	RequestID string        `json:"request_id,omitempty"`
}

// ResyncData 事件缓冲已被裁剪，需以全量快照重建。
type ResyncData struct {
	SessionID   string `json:"session_id"`
	LastEventID uint64 `json:"last_event_id"`
	Messages    any    `json:"messages"`
}

// ErrorPayload 对外错误结构。
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Usage token 用量。放在 sse 包避免 sse → session 的反向依赖。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
