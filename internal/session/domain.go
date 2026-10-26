package session

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// DefaultTitle 是新建会话的标题前缀，前端可直接展示。
const DefaultTitle = "新会话"

// titleMaxRunes 自动生成标题的最大字数（中文按 rune 计）。
const titleMaxRunes = 24

// NewSession 构造一个可入库的会话。
// 调用方只需给出标题与模型，ID/时间戳由领域层统一生成，避免各处手写。
func NewSession(title, model string) *Session {
	return NewSessionAt(title, model, time.Now().UTC())
}

// NewSessionAt 与 NewSession 相同，但由调用方指定时间戳。
// 与 Store 的可注入时钟配合使用，保证同一次写入内时间来源一致（也便于测试）。
func NewSessionAt(title, model string, now time.Time) *Session {
	now = now.UTC()
	if strings.TrimSpace(model) == "" {
		model = "mock-echo"
	}
	return &Session{
		ID:        NewSessionID(),
		Title:     NormalizeTitle(title, now),
		CreatedAt: now,
		UpdatedAt: now,
		Status:    SessionIdle,
		Model:     model,
	}
}

// NormalizeTitle 清洗标题：去空、折叠空白、超长截断；空标题回退为「新会话 · MM-DD HH:mm」。
func NormalizeTitle(raw string, now time.Time) string {
	t := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if t == "" {
		return DefaultTitle + " · " + now.Format("01-02 15:04")
	}
	return truncateRunes(t, titleMaxRunes)
}

// IsDefaultTitle 判断标题是否仍是自动生成的默认值。
// 首条用户消息落库时用它决定是否用提问内容替换标题。
func IsDefaultTitle(title string) bool {
	if title == "" {
		return true
	}
	return title == DefaultTitle || strings.HasPrefix(title, DefaultTitle+" · ")
}

// DeriveTitle 从用户首条消息推导标题（取首行、去 Markdown 标记、截断）。
func DeriveTitle(content string, now time.Time) string {
	line := content
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	// 去掉常见的 Markdown 前缀标记，避免标题出现 "### " / "> " / "- "。
	line = strings.TrimLeft(line, "#>-* \t")
	line = strings.TrimSpace(line)
	if line == "" {
		return NormalizeTitle("", now)
	}
	return truncateRunes(line, titleMaxRunes)
}

// NewUserMessage 构造一条用户消息。
func NewUserMessage(sessionID, content string) *Message {
	return NewUserMessageAt(sessionID, content, time.Now().UTC())
}

// NewUserMessageAt 由调用方指定时间戳（与 Store 时钟对齐，便于测试）。
func NewUserMessageAt(sessionID, content string, now time.Time) *Message {
	now = now.UTC()
	return &Message{
		ID:        NewMessageID(),
		SessionID: sessionID,
		Role:      RoleUser,
		Content:   content,
		Status:    StatusComplete,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// NewAssistantPlaceholder 构造助手占位消息（生成开始时先落库，状态 pending）。
// 这样即使流中途断开，会话详情里也能看到「已提问、回答未完成」的真实状态。
func NewAssistantPlaceholder(sessionID string) *Message {
	return NewAssistantPlaceholderAt(sessionID, time.Now().UTC())
}

// NewAssistantPlaceholderAt 由调用方指定时间戳。
func NewAssistantPlaceholderAt(sessionID string, now time.Time) *Message {
	now = now.UTC()
	return &Message{
		ID:        NewMessageID(),
		SessionID: sessionID,
		Role:      RoleAssistant,
		Content:   "",
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Validate 校验消息的领域约束，Store 实现可在写入前调用。
func (m *Message) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("%w: 消息 ID 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(m.SessionID) == "" {
		return fmt.Errorf("%w: session_id 不能为空", ErrInvalidArgument)
	}
	switch m.Role {
	case RoleUser, RoleAssistant, RoleSystem, RoleTool:
	default:
		return fmt.Errorf("%w: 未知角色 %q", ErrInvalidArgument, m.Role)
	}
	switch m.Status {
	case StatusPending, StatusStreaming, StatusComplete, StatusInterrupted, StatusError:
	default:
		return fmt.Errorf("%w: 未知消息状态 %q", ErrInvalidArgument, m.Status)
	}
	return nil
}

// Validate 校验会话的领域约束。
func (s *Session) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("%w: 会话 ID 不能为空", ErrInvalidArgument)
	}
	switch s.Status {
	case SessionIdle, SessionRunning, SessionError, SessionSuspended:
	default:
		return fmt.Errorf("%w: 未知会话状态 %q", ErrInvalidArgument, s.Status)
	}
	return nil
}

// Clone 深拷贝会话，避免调用方与 Store 内部共享 Meta map。
func (s *Session) Clone() *Session {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Meta != nil {
		cp.Meta = make(map[string]string, len(s.Meta))
		for k, v := range s.Meta {
			cp.Meta[k] = v
		}
	}
	return &cp
}

// Clone 深拷贝消息，避免共享 Usage / Error 指针。
func (m *Message) Clone() Message {
	cp := *m
	if m.Usage != nil {
		u := *m.Usage
		cp.Usage = &u
	}
	if m.Error != nil {
		e := *m.Error
		cp.Error = &e
	}
	return cp
}

// truncateRunes 按字符（而非字节）截断，避免中文被截成乱码。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return strings.TrimSpace(s[:i]) + "…"
		}
		count++
	}
	// 未超长：顺带归一化掉不可见的控制字符。
	return strings.Map(func(r rune) rune {
		if r == '\uFEFF' || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			return -1
		}
		return r
	}, s)
}
