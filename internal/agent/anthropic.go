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

// anthropicVersion 是 Anthropic API 要求的版本头。
// 官方会按此做向后兼容，升级需关注 changelog。
const anthropicVersion = "2023-06-01"

// AnthropicRunner 调用 Anthropic Messages API（POST /v1/messages，stream=true）。
//
// 与 OpenAI 兼容协议的关键差异（这也是需要单独实现 Runner 的原因）：
//
//	维度        OpenAI                          Anthropic
//	路径        /chat/completions               /messages
//	鉴权        Authorization: Bearer <key>      x-api-key: <key>
//	版本头      —                               anthropic-version: 2023-06-01
//	系统提示    messages[0].role=system         顶层 system 字段
//	流式负载    choices[].delta.content         content_block_delta.delta.text
//	结束标志    data: [DONE]                    message_stop 事件
//	用量        usage.prompt_tokens             usage.input_tokens
type AnthropicRunner struct {
	BaseURL      string
	APIKey       string
	Model        string
	SystemPrompt string
	MaxTokens    int
	ExtraHeaders map[string]string
	HTTPClient   *http.Client
}

// NewAnthropicRunner 构造 Anthropic 执行器。
//
// baseURL 允许省略版本段（/v1）：拼接请求地址时由 normalizeBaseURL 统一补全。
func NewAnthropicRunner(baseURL, apiKey, model, systemPrompt string) *AnthropicRunner {
	return &AnthropicRunner{
		BaseURL:      baseURL,
		APIKey:       apiKey,
		Model:        model,
		SystemPrompt: systemPrompt,
		// Messages API 的 max_tokens 是必填项；给一个足够大的默认值。
		MaxTokens: 4096,
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

func (a *AnthropicRunner) Name() string { return "anthropic:" + a.Model }

// ---- 请求体 ----

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
	// Tools 工具声明（Anthropic 用 input_schema）。
	Tools []tool.AnthropicTool `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role string `json:"role"` // 只能是 user / assistant
	// Content 可以是纯字符串，也可以是内容块数组。
	// 工具调用/结果需要数组形式，故用 any。
	Content any `json:"content"`
}

// anthropicContentBlock 是消息内容块（支持 text / tool_use / tool_result）。
type anthropicContentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ---- 流式事件 ----

// anthropicEvent 是所有事件的公共信封。
type anthropicEvent struct {
	Type string `json:"type"`

	// message_start
	Message *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`

	// content_block_start（工具调用从这里开始）
	Index        *int                   `json:"index"`
	ContentBlock *anthropicContentBlock `json:"content_block"`

	// content_block_delta
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// tool_use 的增量参数（input_json_delta.partial_json）
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`

	// message_delta（携带最终 output_tokens）
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	// error
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *AnthropicRunner) Run(ctx context.Context, req Request) <-chan Event {
	out := make(chan Event, 8)
	go func() {
		defer close(out)
		if err := a.stream(ctx, req, out); err != nil {
			select {
			case out <- Event{Type: TypeError, Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return out
}

func (a *AnthropicRunner) stream(ctx context.Context, rreq Request, out chan<- Event) error {
	system := rreq.SystemPrompt
	if system == "" {
		system = a.SystemPrompt
	}
	body := anthropicRequest{
		Model:     a.Model,
		MaxTokens: a.MaxTokens,
		System:    system,
		Messages:  a.toMessages(rreq.History),
		Stream:    true,
	}
	// 工具声明（Anthropic 用 input_schema 而非 parameters）。
	if len(rreq.Tools) > 0 {
		body.Tools = tool.ToAnthropicTools(rreq.Tools)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("构建请求体失败: %w", err)
	}

	url := endpointURL(a.BaseURL, "messages")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Anthropic 用 x-api-key 而非 Bearer。
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	for k, v := range a.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("调用模型失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("Anthropic 返回状态 %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var sb strings.Builder
	inputTokens, outputTokens := 0, 0
	finishReason := ""

	// 工具调用累积：block 索引 → 工具 id/name/参数。
	type pendingTool struct {
		id   string
		name string
		args strings.Builder
		// gotDelta 记录是否收到过 input_json_delta。
		//
		// 为什么需要它：Anthropic 官方流式协议里 content_block_start 的
		// input 固定是空对象 {}（占位），真实参数全部通过 input_json_delta
		// 传输。若把占位的 {} 也写入缓冲，再拼上 delta 就得到
		// `{"path":...}` 或 `{"}{...}`，下游 json.Unmarshal 报
		// "invalid character '{' after top-level value"。
		gotDelta bool
	}
	toolBlocks := make(map[int]*pendingTool)
	var toolOrder []int

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
		// Anthropic 也是 SSE：event: xxx / data: {...}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // 单帧解析失败不应中断整体流
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				inputTokens = ev.Message.Usage.InputTokens
			}

		case "content_block_start":
			// 工具调用从这里开始：记录 block 索引 → 工具 id/name，
			// 后续 input_json_delta 增量参数按索引累积。
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				idx := 0
				if ev.Index != nil {
					idx = *ev.Index
				}
				toolBlocks[idx] = &pendingTool{
					id:   ev.ContentBlock.ID,
					name: ev.ContentBlock.Name,
				}
				toolOrder = append(toolOrder, idx)
				// 这里的 input 有两种可能：
				//   a) 占位符 {}（官方流式格式），真实参数在后面的 delta 里
				//   b) 完整参数（非流式风格的网关），后面没有 delta
				// 先写进缓冲；一旦收到 delta，说明它是占位符，会被整体丢弃。
				if in := ev.ContentBlock.Input; len(in) > 0 && string(in) != "null" {
					toolBlocks[idx].args.Write(in)
				}
			}

		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			// 工具参数增量
			if ev.Delta.Type == "input_json_delta" && ev.Delta.PartialJSON != "" {
				idx := 0
				if ev.Index != nil {
					idx = *ev.Index
				}
				if pt, exists := toolBlocks[idx]; exists {
					// 首个 delta 说明 content_block_start 里的 input 只是占位符
					// （官方固定发 {}），必须丢弃，否则会拼出 `{"path":...}`。
					if !pt.gotDelta {
						pt.args.Reset()
						pt.gotDelta = true
					}
					// 后续 delta 正常情况下是增量片段；但部分网关每帧发送
					// 完整 JSON，交由 appendArgs 判重，避免 `{...}{...}`。
					appendArgs(&pt.args, ev.Delta.PartialJSON)
				}
				continue
			}
			// 正文增量；thinking_delta 等暂不展示。
			if ev.Delta.Text == "" {
				continue
			}
			sb.WriteString(ev.Delta.Text)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- Event{Type: TypeDelta, Delta: ev.Delta.Text}:
			}

		case "message_delta":
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					inputTokens = ev.Usage.InputTokens
				}
				outputTokens = ev.Usage.OutputTokens
			}

		case "message_stop":
			finishReason = "stop"

		case "error":
			if ev.Error != nil {
				return fmt.Errorf("Anthropic 流错误（%s）: %s", ev.Error.Type, ev.Error.Message)
			}
			return errors.New("Anthropic 流返回未知错误")

		case "content_block_stop", "ping":
			// 无需处理。
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("读取 Anthropic 流失败: %w", err)
	}

	// 输出累积的工具调用（在流结束后统一发出，保证参数完整）。
	var calls []ToolCall
	for _, idx := range toolOrder {
		pt, exists := toolBlocks[idx]
		if !exists || pt.name == "" {
			continue
		}
		id := pt.id
		if id == "" {
			id = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), idx)
		}
		args := pt.args.String()
		if args == "" {
			args = "{}"
		}
		call := ToolCall{ID: id, Name: pt.name, Args: args}
		calls = append(calls, call)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- Event{Type: TypeToolCall, ToolCall: &call}:
		}
	}

	// 有工具调用时不发终局 done（由 loop 继续下一轮），
	// 但要上报本轮用量（token 确实消耗了）。
	if len(calls) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- Event{Type: TypeDone, Usage: &Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		}}:
		}
		return nil
	}

	if sb.Len() == 0 {
		return errors.New("模型未返回任何内容")
	}

	// Anthropic 不总在流里给完整 usage，缺失时按字符数粗估。
	if outputTokens == 0 {
		outputTokens = len([]rune(sb.String()))
	}
	_ = finishReason

	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- Event{
		Type:    TypeDone,
		Content: sb.String(),
		Usage: &Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}:
	}
	return nil
}

// toMessages 转换为 Anthropic 的消息数组。
//
// 与 OpenAI 的关键差异：
//   - system 不能放在 messages 里，必须走顶层 system 字段
//   - 工具调用/结果用内容块数组表达，且必须是 user 角色（tool_result）
func (a *AnthropicRunner) toMessages(history []session.Message) []anthropicMessage {
	// 第一遍：收集所有 assistant 声明过的 tool_use id。
	// 孤儿 tool_result 会被 Anthropic 拒绝（tool_use_id 必须对应前置的 tool_use）。
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
	// 第二遍：收集已有结果应答的 tool_use id。
	// 未应答的 tool_use 同样非法（Anthropic 要求每个 tool_use 都有 tool_result），
	// ask_user 挂起后就会留下这种，必须在发送前剔除。
	answered := make(map[string]bool)
	for _, m := range history {
		if m.Role == session.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}

	out := make([]anthropicMessage, 0, len(history))
	for _, m := range history {
		// system 已在顶层。
		if m.Role == session.RoleSystem {
			continue
		}
		// 中断的回答不进上下文（与 OpenAI 实现保持一致）。
		if m.Role == session.RoleAssistant && m.Status == session.StatusInterrupted {
			continue
		}

		switch m.Role {
		case session.RoleTool:
			// 丢弃找不到前置 tool_use 的孤儿结果。
			if m.ToolCallID == "" || !declared[m.ToolCallID] {
				continue
			}
			// 工具结果在 Anthropic 中是 user 消息里的 tool_result 块。
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []anthropicContentBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
					IsError:   m.Status == session.StatusError,
				}},
			})

		case session.RoleAssistant:
			// assistant 可能同时含文本与工具调用。
			if len(m.ToolCalls) == 0 {
				if m.Content == "" {
					continue
				}
				out = append(out, anthropicMessage{Role: "assistant", Content: m.Content})
				continue
			}
			blocks := make([]anthropicContentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				// 只回传已有 tool_result 的调用，否则 Anthropic 会拒绝整个请求。
				if tc.ID != "" && !answered[tc.ID] {
					continue
				}
				input := json.RawMessage(tc.Args)
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			// 剔除后若只剩空块，这条消息没有意义，跳过（否则会发出空 content）。
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})

		default:
			// user
			if m.Content == "" {
				continue
			}
			out = append(out, anthropicMessage{Role: string(m.Role), Content: m.Content})
		}
	}
	return out
}
