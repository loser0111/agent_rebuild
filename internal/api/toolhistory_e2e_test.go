package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
	"agent_rebuild/internal/tool"
)

// 验证：第二轮对话时，发给模型的历史里**必须包含第一轮的工具调用与结果**，
// 否则模型不知道已经调用过，会重复调用同一个工具。

type capturedRequest struct {
	Messages []map[string]any `json:"messages"`
	Tools    []any            `json:"tools"`
}

type captureGateway struct {
	mu       sync.Mutex
	requests []capturedRequest
	// callCount 控制第几次请求返回工具调用还是最终回答。
	callCount int
}

func (g *captureGateway) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req capturedRequest
		_ = json.Unmarshal(body, &req)

		g.mu.Lock()
		g.requests = append(g.requests, req)
		n := len(g.requests)
		g.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		// 第 1 次请求：返回一个工具调用（list_dir）。
		if n == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}

		// 第 2 次及以后：直接给最终回答。
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"已完成"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}
}

// TestSecondTurnHistoryIncludesToolResults 核心断言：
// 第二次用户发言时，历史里必须有 role=tool 的消息与其 tool_call_id。
func TestSecondTurnHistoryIncludesToolResults(t *testing.T) {
	gw := &captureGateway{}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	cfg := &config.Config{
		Port: "0", Env: "test",
		CORSOrigins:           []string{"http://localhost:5173"},
		MaxMessagesPerSession: 200, SSEBufferSize: 200,
		SSEPingInterval: time.Hour, RunTimeout: 30 * time.Second,
		LLMProvider: "mock", LLMModel: "mock-echo",
	}
	store := sqlite.NewSessionStore(db, cfg.MaxMessagesPerSession)
	models := modelconfig.NewSQLStore(db.SQL(), dir)

	runner := agent.NewLLMRunner(gwSrv.URL, "k", "test-model", "")
	s := NewServer(cfg, store, runner, agent.NewLocks())

	root := t.TempDir()
	s.SetTools(tool.DefaultRegistry(tool.Options{Workspace: root}), "", 5, 10*time.Second)
	s.SetModelConfigStore(models, func(mc *modelconfig.Config) (agent.Runner, error) {
		return agent.NewLLMRunner(gwSrv.URL, "k", "test-model", ""), nil
	})

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	env := &multiModelEnv{url: ts.URL, http: ts, store: store, models: models}

	// 建会话。
	var created createSessionResponse
	env.req(t, http.MethodPost, "/api/sessions", map[string]any{"title": "t"}, http.StatusCreated, &created)
	sid := created.Session.ID

	// ---- 第一轮：触发工具调用 ----
	// 注意必须走流式路径：非流式（stream=false）会绕过 executor/loop，
	// 也就不会产生工具调用记录。这里用 stream=true 复现真实交互。
	env.postSSE(t, "/api/sessions/"+sid+"/messages",
		map[string]any{"content": "看看当前目录"})

	// 落库检查：应有 assistant(tool_calls) + tool(结果)
	msgs, err := store.Messages(t.Context(), sid)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}

	var hasToolCall, hasToolResult bool
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call_a" {
			hasToolCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "call_a" {
			hasToolResult = true
			t.Logf("落库的 tool 结果内容(前80字): %.80s", m.Content)
		}
	}
	if !hasToolCall {
		t.Fatalf("落库里没有 assistant 的 tool_calls，消息: %+v", summarize(msgs))
	}
	if !hasToolResult {
		t.Fatalf("落库里没有 tool 结果消息，消息: %+v", summarize(msgs))
	}

	// ---- 第二轮：用户再发一句话 ----
	env.postSSE(t, "/api/sessions/"+sid+"/messages",
		map[string]any{"content": "继续"})

	// 检查最后一次请求（第二轮）发给模型的 messages。
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.requests) < 2 {
		t.Fatalf("应有至少 2 次模型请求，实际 %d", len(gw.requests))
	}
	// 第二轮可能包含多步（loop），取最后一次。
	last := gw.requests[len(gw.requests)-1]

	var roles []string
	var hasToolMsg bool
	var hasToolCallsInAssistant bool
	for _, m := range last.Messages {
		role, _ := m["role"].(string)
		roles = append(roles, role)
		if role == "tool" {
			hasToolMsg = true
			if id, _ := m["tool_call_id"].(string); id != "call_a" {
				t.Errorf("tool 消息的 tool_call_id 应为 call_a，实际 %q", id)
			}
		}
		if role == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				hasToolCallsInAssistant = true
			}
		}
	}

	t.Logf("第二轮请求的消息角色序列: %v", roles)

	if !hasToolCallsInAssistant {
		t.Errorf("第二轮历史里缺少 assistant 的 tool_calls —— 模型无法知道调过工具")
	}
	if !hasToolMsg {
		t.Errorf("第二轮历史里缺少 role=tool 的结果消息 —— 模型会重复调用工具")
	}
}

// postSSE 发起流式请求并读完整个事件流。
//
// 流式路径才会经过 executor + loop（工具调用在那里产生），
// 非流式走 sendMessageAggregated，不产生工具调用记录。
func (e *multiModelEnv) postSSE(t *testing.T, path string, body any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, e.url+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status=%d body=%s", path, resp.StatusCode, b)
	}
	// 读完整个流，确保后台执行完成、落库结束。
	_, _ = io.Copy(io.Discard, resp.Body)
}

func summarize(msgs []session.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, fmt.Sprintf("%s(%s,tool_call_id=%s,tool_calls=%d)",
			m.Role, m.Status, m.ToolCallID, len(m.ToolCalls)))
	}
	return out
}

var _ = strings.TrimSpace
