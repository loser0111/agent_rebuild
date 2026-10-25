package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent_rebuild/internal/session"
)

// MockRunner 是无 Key 也能跑通全链路的确定性执行器：
// 按固定节奏把一段中文文本按 2~4 字切块吐出，用于 Demo 与单测。
type MockRunner struct {
	Model  string
	Chunk  time.Duration // 每块间隔，默认 25ms
	Prefix string
}

// NewMockRunner 创建 Mock 执行器。
func NewMockRunner(model string) *MockRunner {
	return &MockRunner{Model: model, Chunk: 25 * time.Millisecond}
}

func (m *MockRunner) Name() string { return "mock:" + m.Model }

func (m *MockRunner) Run(ctx context.Context, req Request) <-chan Event {
	out := make(chan Event, 8)
	chunk := m.Chunk
	if chunk <= 0 {
		chunk = 25 * time.Millisecond
	}

	go func() {
		defer close(out)

		history := req.History
		last := lastUserContent(history)
		answer := m.buildAnswer(last)

		// 首帧立即送达，保证首字延迟极低。
		if !emit(ctx, out, Event{Type: TypeDelta, Delta: "（Mock 模式，未配置模型 Key）\n\n"}) {
			return
		}

		for _, part := range chunkString(answer, 4) {
			select {
			case <-ctx.Done():
				emitInterrupted(ctx, out)
				return
			case <-time.After(chunk):
			}
			if !emit(ctx, out, Event{Type: TypeDelta, Delta: part}) {
				return
			}
		}

		emit(ctx, out, Event{
			Type:    TypeDone,
			Content: "（Mock 模式，未配置模型 Key）\n\n" + answer,
			Usage: &Usage{
				PromptTokens:     len([]rune(last)),
				CompletionTokens: len([]rune(answer)),
				TotalTokens:      len([]rune(last)) + len([]rune(answer)),
			},
		})
	}()

	return out
}

func (m *MockRunner) buildAnswer(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "你好，我是本地 Mock Agent。配置 `LLM_PROVIDER=openai` 与 `OPENAI_API_KEY` 后即可接入真实模型。"
	}
	return fmt.Sprintf("收到你的消息：**%s**\n\n"+
		"这是一条由本地 Mock 执行器按 25ms 节奏逐块生成的回复，用于验证以下链路：\n\n"+
		"1. `POST /api/sessions/{id}/messages` 建立 SSE 流\n"+
		"2. `message.start` → `message.delta` × N → `message.done`\n"+
		"3. 服务端每 15s 的 `ping` 心跳\n"+
		"4. 生成结束后的会话落库与历史回放\n\n"+
		"要接真实模型，只需改环境变量，无需改代码。", truncateRunes(prompt, 80))
}

// emit 尝试发送事件；ctx 已取消时返回 false。
func emit(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- ev:
		return true
	}
}

// emitInterrupted 在取消时尽力发出中断事件（不阻塞）。
func emitInterrupted(ctx context.Context, out chan<- Event) {
	select {
	case out <- Event{Type: TypeError, Err: context.Canceled}:
	default:
	}
}

// lastUserContent 取最后一条 user 消息内容。
func lastUserContent(history []session.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == session.RoleUser {
			return history[i].Content
		}
	}
	return ""
}

// chunkString 按 rune 切块，避免中文被截断成乱码。
func chunkString(s string, size int) []string {
	if size <= 0 {
		size = 4
	}
	runes := []rune(s)
	out := make([]string, 0, len(runes)/size+1)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
