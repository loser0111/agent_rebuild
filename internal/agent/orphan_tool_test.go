package agent

// 回归测试：历史中存在「孤儿 tool 消息」时不能把非法请求发给模型。
//
// 真实故障：早期版本只落库 role=tool 的结果，没落带 tool_calls 的 assistant 消息。
// 于是历史里出现「没有前置 tool_calls 的 tool 消息」，OpenAI 返回 400：
//
//	Messages with role 'tool' must be a response to a preceding message
//	with 'tool_calls'

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

// captureMessages 启动一个假网关，返回它收到的 messages 数组。
func captureMessages(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
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
	return srv, &got
}

// 孤儿 tool 消息（无前置 tool_calls）必须被丢弃。
func TestOpenAIDropsOrphanToolMessages(t *testing.T) {
	srv, got := captureMessages(t)
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 模拟损坏的历史：tool 消息存在，但没有任何 assistant 声明过该调用。
	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "读文件", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleTool, Content: "文件内容", Status: session.StatusComplete,
			ToolCallID: "orphan_call_1"},
	}

	for range r.Run(ctx, Request{History: history}) {
	}

	for _, m := range *got {
		if m["role"] == "tool" {
			t.Fatalf("孤儿 tool 消息不应发给模型，实际消息: %+v", *got)
		}
	}

	// user 消息必须保留。
	var hasUser bool
	for _, m := range *got {
		if m["role"] == "user" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Fatal("user 消息不应被丢弃")
	}
}

// 合法的 tool 链（assistant tool_calls + tool 结果）必须完整保留。
func TestOpenAIKeepsValidToolChain(t *testing.T) {
	srv, got := captureMessages(t)
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "读文件", Status: session.StatusComplete},
		{
			ID: "2", Role: session.RoleAssistant, Status: session.StatusComplete,
			ToolCalls: []session.ToolCall{{ID: "call_1", Name: "read_file", Args: `{"path":"a.txt"}`}},
		},
		{ID: "3", Role: session.RoleTool, Content: "文件内容", Status: session.StatusComplete,
			ToolCallID: "call_1"},
	}

	for range r.Run(ctx, Request{History: history}) {
	}

	var hasToolCall, hasToolResult bool
	for _, m := range *got {
		if m["role"] == "assistant" {
			if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
				hasToolCall = true
			}
		}
		if m["role"] == "tool" {
			hasToolResult = true
			// tool 消息必须带 tool_call_id。
			if m["tool_call_id"] != "call_1" {
				t.Fatalf("tool_call_id 丢失: %+v", m)
			}
		}
	}
	if !hasToolCall {
		t.Fatal("assistant 的 tool_calls 应保留")
	}
	if !hasToolResult {
		t.Fatal("合法的 tool 结果应保留")
	}
}

// 混合场景：部分合法、部分孤儿，只丢孤儿。
func TestOpenAIDropsOnlyOrphanToolMessages(t *testing.T) {
	srv, got := captureMessages(t)
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "第一问", Status: session.StatusComplete},
		// 合法的调用链
		{ID: "2", Role: session.RoleAssistant, Status: session.StatusComplete,
			ToolCalls: []session.ToolCall{{ID: "good_call", Name: "read_file", Args: "{}"}}},
		{ID: "3", Role: session.RoleTool, Content: "好结果", Status: session.StatusComplete,
			ToolCallID: "good_call"},
		// 孤儿：无前置 tool_calls
		{ID: "4", Role: session.RoleTool, Content: "坏结果", Status: session.StatusComplete,
			ToolCallID: "bad_call"},
		{ID: "5", Role: session.RoleUser, Content: "第二问", Status: session.StatusComplete},
	}

	for range r.Run(ctx, Request{History: history}) {
	}

	var toolMsgs []map[string]any
	for _, m := range *got {
		if m["role"] == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("应只保留 1 条合法 tool 消息，实际 %d 条: %+v", len(toolMsgs), toolMsgs)
	}
	if toolMsgs[0]["tool_call_id"] != "good_call" {
		t.Fatalf("保留的是错误的消息: %+v", toolMsgs[0])
	}
}

// tool 消息缺失 tool_call_id 时也应丢弃（无法与调用关联）。
func TestOpenAIDropsToolMessageWithoutID(t *testing.T) {
	srv, got := captureMessages(t)
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "问", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleTool, Content: "无 ID 的结果", Status: session.StatusComplete},
	}

	for range r.Run(ctx, Request{History: history}) {
	}

	for _, m := range *got {
		if m["role"] == "tool" {
			t.Fatalf("无 tool_call_id 的消息不应发送: %+v", *got)
		}
	}
}

// Anthropic 侧同样要丢弃孤儿 tool_result。
func TestAnthropicDropsOrphanToolMessages(t *testing.T) {
	var captured []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		captured = req.Messages

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	r := NewAnthropicRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "问", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleTool, Content: "孤儿结果", Status: session.StatusComplete,
			ToolCallID: "orphan_1"},
	}

	for range r.Run(ctx, Request{History: history}) {
	}

	// 不应出现含 tool_result 的消息。
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
			if block["type"] == "tool_result" {
				t.Fatalf("孤儿 tool_result 不应发送: %+v", captured)
			}
		}
	}
}
