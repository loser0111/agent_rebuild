package agent

// 回归测试：ask_user 挂起后留下的「未应答 tool_calls」不能让后续请求被拒。
//
// 真实场景：
//   1. 模型调用 ask_user → assistant(tool_calls) 立刻落库（executor.go 的
//      appendToolCallMessage），但 tool 结果要等用户回答后才补；
//   2. 用户没回答，直接问下一个问题；
//   3. 此时历史里有「声明了 tool_calls 却没有 tool 结果」的消息。
//
// OpenAI 会返回 400：
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//
// Anthropic 同理（每个 tool_use 都要有 tool_result）。
// 这类错误对用户完全不可解释——他只是问了句话，却整个会话都发不出去了。
//
// 处理策略：发送前把「未应答的 tool_calls」从 assistant 消息里剔除。
// 这比丢弃整条 assistant 消息更好：模型说过的分析文本仍然保留。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_rebuild/internal/session"
)

// 未应答的 tool_calls 必须被剔除（OpenAI 侧）。
func TestOpenAIDropsUnansweredToolCalls(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		got = req.Messages

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// 复刻挂起后的历史：assistant 有 tool_calls，但没有配对 tool 结果。
	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "帮我建表", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleAssistant, Content: "需要先确认一下。",
			Status:    session.StatusComplete,
			ToolCalls: []session.ToolCall{{ID: "call_ask", Name: "ask_user", Args: `{"question":"用哪个？"}`}}},
		// 注意：没有 role=tool 的应答消息。
		{ID: "3", Role: session.RoleUser, Content: "算了先看别的", Status: session.StatusComplete},
	}

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: history}) {
	}

	// 1. 任何消息都不能带 tool_calls（那个调用没人应答）。
	for _, m := range got {
		if tc, ok := m["tool_calls"]; ok && tc != nil {
			t.Fatalf("未应答的 tool_calls 不应发送，实际消息: %+v", m)
		}
	}
	// 2. 但 assistant 的文本必须保留——用户仍需看到模型说了什么。
	var foundText bool
	for _, m := range got {
		if m["role"] == "assistant" && m["content"] == "需要先确认一下。" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("assistant 的文本应保留，实际消息: %+v", got)
	}
	// 3. 后续 user 消息也要在，否则模型看不到用户的新问题。
	var foundFollowUp bool
	for _, m := range got {
		if m["role"] == "user" && m["content"] == "算了先看别的" {
			foundFollowUp = true
		}
	}
	if !foundFollowUp {
		t.Fatalf("后续 user 消息不应丢失，实际消息: %+v", got)
	}
}

// 已应答的调用必须原样保留（不能因为清洗而误删正常工具链）。
func TestOpenAIKeepsAnsweredToolCalls(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		got = req.Messages

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "看目录", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleAssistant, Content: "", Status: session.StatusComplete,
			ToolCalls: []session.ToolCall{{ID: "call_ls", Name: "list_dir", Args: `{"path":"."}`}}},
		{ID: "3", Role: session.RoleTool, Content: "a.go", Status: session.StatusComplete,
			ToolCallID: "call_ls"},
		{ID: "4", Role: session.RoleAssistant, Content: "共 1 个文件", Status: session.StatusComplete},
	}

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: history}) {
	}

	var sawToolCall, sawToolResult bool
	for _, m := range got {
		if tc, ok := m["tool_calls"]; ok && tc != nil {
			sawToolCall = true
		}
		if m["role"] == "tool" && m["tool_call_id"] == "call_ls" {
			sawToolResult = true
		}
	}
	if !sawToolCall {
		t.Fatalf("已应答的 tool_calls 应保留，实际: %+v", got)
	}
	if !sawToolResult {
		t.Fatalf("对应的 tool 结果应保留，实际: %+v", got)
	}
}

// Anthropic 侧同样的清洗（未应答的 tool_use 必须剔除）。
func TestAnthropicDropsUnansweredToolUse(t *testing.T) {
	var captured []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		captured = req.Messages

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "建表", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleAssistant, Content: "先确认一下",
			Status:    session.StatusComplete,
			ToolCalls: []session.ToolCall{{ID: "tu_1", Name: "ask_user", Args: `{"question":"?"}`}}},
		{ID: "3", Role: session.RoleUser, Content: "先看别的", Status: session.StatusComplete},
	}

	r := NewAnthropicRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: history}) {
	}

	for _, m := range captured {
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_use" || block["type"] == "tool_result" {
				t.Fatalf("未应答的 tool_use 不应发送: %+v", captured)
			}
		}
	}
}

// 混合场景：一条消息里既有已应答也有未应答的调用，只剔除未应答的那个。
func TestMixedAnsweredAndUnansweredToolCalls(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		got = req.Messages

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "q", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleAssistant, Content: "", Status: session.StatusComplete,
			ToolCalls: []session.ToolCall{
				{ID: "call_done", Name: "list_dir", Args: `{"path":"."}`},
				{ID: "call_pending", Name: "ask_user", Args: `{"question":"?"}`},
			}},
		{ID: "3", Role: session.RoleTool, Content: "ok", Status: session.StatusComplete,
			ToolCallID: "call_done"},
	}

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: history}) {
	}

	for _, m := range got {
		tcs, ok := m["tool_calls"].([]any)
		if !ok {
			continue
		}
		if len(tcs) != 1 {
			t.Fatalf("应只保留 1 个已应答的调用，实际 %d 个: %+v", len(tcs), tcs)
		}
		fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "list_dir" {
			t.Fatalf("保留的应是已应答的 list_dir，实际: %+v", fn)
		}
	}
}
