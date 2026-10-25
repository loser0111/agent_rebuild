package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/tool"
)

// LLMRunner 调用任意 OpenAI 兼容接口（/chat/completions, stream=true）。
// 不引 SDK，避免版本耦合；换网关只需改 base_url / model。
type LLMRunner struct {
	BaseURL      string
	APIKey       string
	Model        string
	SystemPrompt string
	ExtraHeaders map[string]string
	HTTPClient   *http.Client
}

// NewLLMRunner 构造真实模型执行器。httpClient 为空时使用带超时的默认 client。
//
// baseURL 允许省略版本段（/v1）：拼接请求地址时由 normalizeBaseURL 统一补全。
func NewLLMRunner(baseURL, apiKey, model, systemPrompt string) *LLMRunner {
	return &LLMRunner{
		BaseURL:      baseURL,
		APIKey:       apiKey,
		Model:        model,
		SystemPrompt: systemPrompt,
		HTTPClient: &http.Client{
			Timeout: 0, // 由 ctx 控制超时，避免与流式响应冲突
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (l *LLMRunner) Name() string { return "openai:" + l.Model }

// ---- 请求体 ----

type chatRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []chatMessage     `json:"messages"`
	Tools    []tool.OpenAITool `json:"tools,omitempty"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content 在 assistant 带 tool_calls 时可为空，故用 omitempty。
	Content string `json:"content,omitempty"`
	// ToolCalls assistant 发起的工具调用。
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 role=tool 时使用，关联被应答的调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name 仅 role=tool 时使用（部分网关要求）。
	Name string `json:"name,omitempty"`
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (l *LLMRunner) Run(ctx context.Context, req Request) <-chan Event {
	out := make(chan Event, 8)
	go func() {
		defer close(out)
		if err := l.stream(ctx, req, out); err != nil {
			select {
			case out <- Event{Type: TypeError, Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return out
}

func (l *LLMRunner) stream(ctx context.Context, rreq Request, out chan<- Event) error {
	body := chatRequest{
		Model:    l.Model,
		Stream:   true,
		Messages: l.toMessages(rreq.History, rreq.SystemPrompt),
	}
	if len(rreq.Tools) > 0 {
		body.Tools = tool.ToOpenAITools(rreq.Tools)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("构建请求体失败: %w", err)
	}

	url := endpointURL(l.BaseURL, "chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+l.APIKey)
	for k, v := range l.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("调用模型失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("模型返回状态 %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var sb strings.Builder
	var usage *Usage
	finish := ""

	// 流式 tool_calls 的累积：OpenAI 按 index 分片返回，
	// 首片带 id/name，后续片只带 arguments 片段，需要按 index 拼接。
	type pendingCall struct {
		id      string
		name    string
		argsBuf strings.Builder
	}
	pending := make(map[int]*pendingCall)
	var pendingOrder []int

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}

		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // 单帧解析失败不应中断整体流
		}
		if chunk.Error != nil {
			return errors.New(chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = &Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
			// 文本增量
			if ch.Delta.Content != "" {
				sb.WriteString(ch.Delta.Content)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case out <- Event{Type: TypeDelta, Delta: ch.Delta.Content}:
				}
			}
			// 工具调用增量
			for i, tc := range ch.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				pc, exists := pending[idx]
				if !exists {
					pc = &pendingCall{}
					pending[idx] = pc
					pendingOrder = append(pendingOrder, idx)
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pc.argsBuf.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("读取模型流失败: %w", err)
	}

	// 输出累积的工具调用。
	var calls []ToolCall
	for _, idx := range pendingOrder {
		pc := pending[idx]
		if pc.name == "" {
			continue
		}
		id := pc.id
		if id == "" {
			// 部分网关不返回 id，自行生成以保证可关联。
			id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), idx)
		}
		call := ToolCall{
			ID:   id,
			Name: pc.name,
			Args: pc.argsBuf.String(),
		}
		calls = append(calls, call)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- Event{Type: TypeToolCall, ToolCall: &call}:
		}
	}

	// 有工具调用时不发完整的 done（由 loop 继续下一轮），
	// 但要单独上报用量：这一轮的 token 确实消耗了，漏掉会让统计偏少。
	if len(calls) > 0 {
		if usage != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- Event{Type: TypeDone, Usage: usage}:
			}
		}
		return nil
	}

	if sb.Len() == 0 {
		return errors.New("模型未返回任何内容")
	}
	if usage == nil {
		// 部分网关不在流式响应里给 usage，按字符数粗估避免前端显示空。
		usage = &Usage{CompletionTokens: len([]rune(sb.String()))}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	_ = finish

	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- Event{Type: TypeDone, Content: sb.String(), Usage: usage}:
	}
	return nil
}

// toMessages 把会话历史转为 OpenAI messages。
//
// 关键约束：role=tool 的消息必须紧跟在其 tool_calls 的发起方之后。
// 若历史中存在「孤儿 tool 消息」（例如早期版本只落结果未落发起方），
// 直接发送会被 OpenAI 以 400 拒绝：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 因此这里做一次防御性清洗：收集所有有效的 tool_call_id，
// 丢弃那些找不到前置的发起的 tool 消息。
func (l *LLMRunner) toMessages(history []session.Message, systemPrompt string) []chatMessage {
	if systemPrompt == "" {
		systemPrompt = l.SystemPrompt
	}

	// 第一遍：收集所有 assistant 声明过的 tool_call id。
	declared := make(map[string]bool)
	for _, m := range history {
		if m.Role == session.RoleAssistant {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					declared[tc.ID] = true
				}
			}
		}
	}

	msgs := make([]chatMessage, 0, len(history)+1)
	if systemPrompt != "" {
		msgs = append(msgs, chatMessage{Role: string(session.RoleSystem), Content: systemPrompt})
	}
	for _, m := range history {
		// 中断的 assistant 回答不进上下文：
		// 半截内容会被模型当成「已完成的推理」继续引用，容易产生错误结论。
		if m.Role == session.RoleAssistant && m.Status == session.StatusInterrupted {
			continue
		}

		// 孤儿 tool 消息必须丢弃（否则整个请求被拒）。
		if m.Role == session.RoleTool {
			if m.ToolCallID == "" || !declared[m.ToolCallID] {
				continue
			}
		}

		cm := chatMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		// assistant 的工具调用请求必须原样回传，否则模型无法理解上下文。
		for _, tc := range m.ToolCalls {
			var ctc chatToolCall
			ctc.ID = tc.ID
			ctc.Type = "function"
			ctc.Function.Name = tc.Name
			ctc.Function.Arguments = tc.Args
			cm.ToolCalls = append(cm.ToolCalls, ctc)
		}
		// 空内容的 assistant 消息（只有 tool_calls）需保留，否则工具链断裂。
		if m.Content == "" && len(m.ToolCalls) == 0 && m.ToolCallID == "" {
			continue
		}
		msgs = append(msgs, cm)
	}
	return msgs
}
