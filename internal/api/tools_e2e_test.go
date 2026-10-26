package api

// 工具调用的端到端集成测试：从 HTTP 请求到 SSE 事件，覆盖完整工具链路。
//
// 用假模型服务（httptest）模拟 OpenAI 的工具调用响应，
// 验证：模型请求工具 → 执行 → 结果回喂 → 最终回答。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/sse"
	"agent_rebuild/internal/tool"
)

// fakeToolCallingModel 模拟支持工具调用的 OpenAI 兼容服务。
//
// 第一次请求：返回 tool_call（要求读文件）
// 第二次请求：收到工具结果后返回最终文本
type fakeToolCallingModel struct {
	callCount int32
	srv       *httptest.Server
}

func newFakeToolCallingModel(t *testing.T) *fakeToolCallingModel {
	t.Helper()
	m := &fakeToolCallingModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)

		n := atomic.AddInt32(&m.callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		// 断言：请求里应带上工具声明。
		if len(req.Tools) == 0 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"没有收到工具声明"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}

		if n == 1 {
			// 第一轮：请求调用 read_file。
			chunk := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"target.txt\"}"}}]}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}

		// 第二轮：检查是否收到了工具结果。
		var toolContent string
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				toolContent = msg.Content
			}
		}
		if toolContent == "" {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"工具结果为空"}}]}`+"\n\n")
		} else {
			// 把文件内容回显给用户（证明工具真的执行了）。
			escaped, _ := json.Marshal("文件里写着：" + strings.TrimSpace(toolContent))
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", escaped)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// newToolEnv 构造启用工具系统的测试环境。
func newToolEnv(t *testing.T, modelURL string) (*testEnv, string) {
	t.Helper()
	root := t.TempDir()
	if err := writeFileForTest(root, "target.txt", "这是文件内容"); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}

	cfg := &config.Config{
		Port:                  "0",
		Env:                   "test",
		MaxMessagesPerSession: 200,
		SSEBufferSize:         200,
		SSEPingInterval:       time.Hour,
		RunTimeout:            30 * time.Second,
		LLMProvider:           "openai",
		LLMProtocol:           config.ProtocolOpenAI,
		LLMModel:              "fake-model",
		LLMBaseURL:            modelURL,
		LLMAPIKey:             "sk-test",
		EnableCommandTool:     false,
		MaxToolSteps:          5,
		ToolTimeout:           30 * time.Second,
	}
	store := session.NewMemoryStore(cfg.MaxMessagesPerSession)
	runner := agent.NewLLMRunner(modelURL, "sk-test", "fake-model", "")

	s := NewServer(cfg, store, runner, agent.NewLocks())
	// 启用工具（工作区指向临时目录）。
	registry := tool.DefaultRegistry(tool.Options{Workspace: root})
	s.SetTools(registry, "", 5, 30*time.Second)

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{server: s, store: store, url: ts.URL, http: ts}, root
}

func writeFileForTest(root, name, content string) error {
	return os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
}

// TestToolCallEndToEnd 验证完整的工具调用链路。
func TestToolCallEndToEnd(t *testing.T) {
	model := newFakeToolCallingModel(t)
	env, _ := newToolEnv(t, model.srv.URL)

	sess := env.createSession(t, "")
	resp := env.startStream(t, sess.ID, map[string]any{"content": "读一下 target.txt"})
	defer resp.Body.Close()

	frames := readSSE(t, resp.Body)
	names := frameNames(frames)

	// 事件序列应包含工具调用与结果。
	var sawToolCall, sawToolResult, sawDone bool
	for _, n := range names {
		switch n {
		case sse.EventToolCall:
			sawToolCall = true
		case sse.EventToolResult:
			sawToolResult = true
		case sse.EventMessageDone:
			sawDone = true
		}
	}
	if !sawToolCall {
		t.Fatalf("应发出 tool.call 事件，实际序列: %v", names)
	}
	if !sawToolResult {
		t.Fatalf("应发出 tool.result 事件，实际序列: %v", names)
	}
	if !sawDone {
		t.Fatalf("应发出 message.done 事件，实际序列: %v", names)
	}

	// tool.call 负载应含工具名与参数（供 UI 展示）。
	for _, f := range frames {
		if f.Name == sse.EventToolCall {
			var d sse.ToolCallData
			if err := json.Unmarshal([]byte(f.Data), &d); err != nil {
				t.Fatalf("解析 tool.call: %v", err)
			}
			if d.Name != "read_file" {
				t.Fatalf("工具名 = %q，期望 read_file", d.Name)
			}
			if d.CallID == "" {
				t.Fatal("tool.call 应带 call_id")
			}
			if d.Risk != "safe" {
				t.Fatalf("read_file 的 risk 应为 safe，实际 %q", d.Risk)
			}
			if d.RequiresApproval {
				t.Fatal("只读工具不应需要审批")
			}
		}
		if f.Name == sse.EventToolResult {
			var d sse.ToolResultData
			if err := json.Unmarshal([]byte(f.Data), &d); err != nil {
				t.Fatalf("解析 tool.result: %v", err)
			}
			if d.IsError {
				t.Fatalf("工具执行失败: %s", d.Output)
			}
			if !strings.Contains(d.Output, "这是文件内容") {
				t.Fatalf("工具结果不含文件内容: %q", d.Output)
			}
		}
	}

	// 最终回答应引用工具结果。
	var finalContent string
	for _, f := range frames {
		if f.Name == sse.EventMessageDone {
			var d sse.MessageDoneData
			if err := json.Unmarshal([]byte(f.Data), &d); err != nil {
				t.Fatalf("解析 done: %v", err)
			}
			finalContent = d.Content
		}
	}
	if !strings.Contains(finalContent, "这是文件内容") {
		t.Fatalf("最终回答应基于工具结果，实际: %q", finalContent)
	}

	// 模型应被调用两轮（工具轮 + 回答轮）。
	if got := atomic.LoadInt32(&model.callCount); got != 2 {
		t.Fatalf("模型调用次数 = %d，期望 2", got)
	}
}

// 工具调用落库必须保持「assistant(tool_calls) → tool(结果)」的配对顺序。
func TestToolMessagesPersistedInPairs(t *testing.T) {
	model := newFakeToolCallingModel(t)
	env, _ := newToolEnv(t, model.srv.URL)

	sess := env.createSession(t, "")
	resp := env.startStream(t, sess.ID, map[string]any{"content": "读一下 target.txt"})
	readSSE(t, resp.Body)
	resp.Body.Close()

	// 等待工具消息落库（落库发生在流结束后）。
	var msgs []session.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ = env.store.Messages(context.Background(), sess.ID)
		var hasTool bool
		for _, m := range msgs {
			if m.Role == session.RoleTool {
				hasTool = true
			}
		}
		if hasTool {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 校验：每条 tool 消息前面必须有一条声明了该 callID 的 assistant 消息。
	declared := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == session.RoleAssistant {
			for _, tc := range m.ToolCalls {
				declared[tc.ID] = true
			}
		}
		if m.Role == session.RoleTool {
			if m.ToolCallID == "" {
				t.Fatal("tool 消息缺少 tool_call_id")
			}
			if !declared[m.ToolCallID] {
				t.Fatalf("发现孤儿 tool 消息（call_id=%q 无前置 tool_calls）", m.ToolCallID)
			}
		}
	}

	// 至少应有一条 assistant 消息带 tool_calls。
	var hasToolCallAssistant bool
	for _, m := range msgs {
		if m.Role == session.RoleAssistant && len(m.ToolCalls) > 0 {
			hasToolCallAssistant = true
		}
	}
	if !hasToolCallAssistant {
		t.Fatalf("assistant 的 tool_calls 未落库，消息列表: %+v", summarizeMessages(msgs))
	}
}

// summarizeMessages 生成便于阅读的消息摘要（避免断言失败时刷屏）。
func summarizeMessages(msgs []session.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		s := string(m.Role)
		if len(m.ToolCalls) > 0 {
			s += fmt.Sprintf("(calls=%d)", len(m.ToolCalls))
		}
		if m.ToolCallID != "" {
			s += fmt.Sprintf("(call_id=%s)", m.ToolCallID)
		}
		out = append(out, s)
	}
	return out
}

// 二次对话时，历史里不应含孤儿 tool 消息（否则真实网关会 400）。
func TestSecondTurnHistoryIsValid(t *testing.T) {
	model := newFakeToolCallingModel(t)
	env, _ := newToolEnv(t, model.srv.URL)

	sess := env.createSession(t, "")

	// 第一轮：触发工具调用。
	resp := env.startStream(t, sess.ID, map[string]any{"content": "读文件"})
	readSSE(t, resp.Body)
	resp.Body.Close()

	// 等落库完成。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := env.store.Messages(context.Background(), sess.ID)
		var hasTool bool
		for _, m := range msgs {
			if m.Role == session.RoleTool {
				hasTool = true
			}
		}
		if hasTool {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 第二轮：这一轮会带着完整历史再请求模型。
	// 如果历史里有孤儿 tool 消息，假网关不会拦（它不校验），
	// 但我们可以直接断言历史结构。
	resp2 := env.startStream(t, sess.ID, map[string]any{"content": "再说一次"})
	readSSE(t, resp2.Body)
	resp2.Body.Close()

	msgs, err := env.store.Messages(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	declared := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == session.RoleAssistant {
			for _, tc := range m.ToolCalls {
				declared[tc.ID] = true
			}
		}
	}
	for _, m := range msgs {
		if m.Role == session.RoleTool && !declared[m.ToolCallID] {
			t.Fatalf("第二轮历史含孤儿 tool 消息: %v", summarizeMessages(msgs))
		}
	}
}
func TestListToolsEndpoint(t *testing.T) {
	model := newFakeToolCallingModel(t)
	env, _ := newToolEnv(t, model.srv.URL)

	var body struct {
		Tools []struct {
			Name             string `json:"name"`
			Description      string `json:"description"`
			Risk             string `json:"risk"`
			RequiresApproval bool   `json:"requires_approval"`
		} `json:"tools"`
		Count int `json:"count"`
	}
	env.getJSON(t, "/api/tools", http.StatusOK, &body)

	if body.Count != 6 {
		t.Fatalf("工具数 = %d，期望 6", body.Count)
	}
	names := make(map[string]bool)
	for _, tl := range body.Tools {
		names[tl.Name] = true
		if tl.Description == "" {
			t.Fatalf("工具 %q 缺少描述", tl.Name)
		}
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "search", "ask_user"} {
		if !names[want] {
			t.Fatalf("缺少工具 %q，实际: %v", want, names)
		}
	}
}

// 未启用工具时 /api/tools 应返回空列表（而非报错）。
func TestListToolsWhenDisabled(t *testing.T) {
	env := newTestEnv(t)
	var body struct {
		Tools []any `json:"tools"`
		Count int   `json:"count"`
	}
	env.getJSON(t, "/api/tools", http.StatusOK, &body)
	if body.Count != 0 {
		t.Fatalf("未启用工具时应返回 0，实际 %d", body.Count)
	}
}
