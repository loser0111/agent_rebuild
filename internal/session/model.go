package session

import "time"

// Role 消息角色。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	// RoleTool 是工具执行结果（OpenAI/Anthropic 的 tool 消息）。
	// 追加，不改变现有值，旧数据与前端不受影响。
	RoleTool Role = "tool"
)

// MessageStatus 消息状态机：pending → streaming → complete | interrupted | error
type MessageStatus string

const (
	StatusPending     MessageStatus = "pending"
	StatusStreaming   MessageStatus = "streaming"
	StatusComplete    MessageStatus = "complete"
	StatusInterrupted MessageStatus = "interrupted"
	StatusError       MessageStatus = "error"
)

// SessionStatus 会话状态。
type SessionStatus string

const (
	SessionIdle    SessionStatus = "idle"
	SessionRunning SessionStatus = "running"
	SessionError   SessionStatus = "error"
)

// Usage token 用量（assistant 消息 complete 时填充）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// MessageError 消息级错误，避免把内部实现细节泄漏到协议层。
type MessageError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Session 会话。
type Session struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	MessageCount int           `json:"message_count"`
	Status       SessionStatus `json:"status"`
	Model        string        `json:"model"`
	// ModelConfigID 该会话选用的模型配置 ID（对应 model_configs.id）。
	//
	// 为空表示使用全局默认配置。会话级选择的好处：
	// 同一会话里切换模型后，历史消息与所选模型一起持久化，
	// 重新打开会话时自动复用上次的选择。
	ModelConfigID string            `json:"model_config_id,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
}

// Message 一条消息。
type Message struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Role      Role          `json:"role"`
	Content   string        `json:"content"`
	Status    MessageStatus `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Usage     *Usage        `json:"usage,omitempty"`
	Error     *MessageError `json:"error,omitempty"`

	// ToolCalls 是 assistant 发起的工具调用（可多个）。
	// 带 omitempty，旧数据与旧前端不受影响。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 是 tool 角色消息关联的调用 ID（幂等重放的主键）。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall 一次工具调用。
type ToolCall struct {
	// ID 由模型供应商返回（OpenAI 的 tool_call_id / Anthropic 的 tool_use id），
	// 是幂等重放的关联键。
	ID string `json:"id"`
	// Name 工具名。
	Name string `json:"name"`
	// Args 参数的 JSON 字符串（保持原样，不解析以避免精度问题）。
	Args string `json:"args"`
}
