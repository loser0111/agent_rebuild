package api

// 回归测试：run_command（RiskDangerous）的调用必须和其他工具一样推 tool.call，
// 不能被推成 tool.approval。
//
// 背景：loop.riskOf 会把 RiskDangerous 映射成 RequiresApproval=true，
// executor 原来据此把事件名改成 tool.approval；但前端 chatStore.tsx 的
// handleEvent 从来没实现过 tool.approval 这个 case（审批 UI 还没做），
// 落进 default 分支被静默忽略——用户在界面上完全看不到 run_command
// 执行的过程，后续 tool.result 到达时也因为找不到对应卡片变成
// message_id 为空的孤儿记录。
//
// 第一轮修复（不考虑权限问题）：统一按 tool.call 推送，Risk /
// RequiresApproval 字段原样透传供前端展示徽章，但不再影响事件路由。
// 本测试钉住这个行为，防止以后又分叉回 tool.approval。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// fakeRunCommandModel 第一轮请求调用 run_command，第二轮基于结果给出最终回答。
type fakeRunCommandModel struct {
	callCount int32
	srv       *httptest.Server
}

func newFakeRunCommandModel(t *testing.T) *fakeRunCommandModel {
	t.Helper()
	m := &fakeRunCommandModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
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

		if n == 1 {
			chunk := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_cmd_1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"echo hello-from-run-command\"}"}}]}}]}`
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}

		var toolContent string
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				toolContent = msg.Content
			}
		}
		escaped, _ := json.Marshal("命令输出：" + strings.TrimSpace(toolContent))
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", escaped)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// newRunCommandEnv 构造启用了 run_command 的测试环境。
func newRunCommandEnv(t *testing.T, modelURL string) *testEnv {
	t.Helper()
	root := t.TempDir()

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
		EnableCommandTool:     true,
		MaxToolSteps:          5,
		ToolTimeout:           30 * time.Second,
	}
	store := session.NewMemoryStore(cfg.MaxMessagesPerSession)
	runner := agent.NewLLMRunner(modelURL, "sk-test", "fake-model", "")

	s := NewServer(cfg, store, runner, agent.NewLocks())
	registry := tool.DefaultRegistry(tool.Options{Workspace: root, EnableCommand: true})
	s.SetTools(registry, "", 5, 30*time.Second)

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{server: s, store: store, url: ts.URL, http: ts}
}

// TestRunCommandEmitsToolCallNotApproval 是本次修复的核心断言。
func TestRunCommandEmitsToolCallNotApproval(t *testing.T) {
	model := newFakeRunCommandModel(t)
	env := newRunCommandEnv(t, model.srv.URL)

	sess := env.createSession(t, "")
	resp := env.startStream(t, sess.ID, map[string]any{"content": "跑一下 echo"})
	defer resp.Body.Close()

	frames := readSSE(t, resp.Body)
	names := frameNames(frames)

	// 不应再出现 tool.approval：这是回归的核心断言。
	for _, n := range names {
		if n == sse.EventToolApproval {
			t.Fatalf("不应再发出 tool.approval 事件，实际序列: %v", names)
		}
	}

	var sawToolCall, sawToolResult, sawDone bool
	var callData sse.ToolCallData
	var resultData sse.ToolResultData
	for _, f := range frames {
		switch f.Name {
		case sse.EventToolCall:
			sawToolCall = true
			if err := json.Unmarshal([]byte(f.Data), &callData); err != nil {
				t.Fatalf("解析 tool.call: %v", err)
			}
		case sse.EventToolResult:
			sawToolResult = true
			if err := json.Unmarshal([]byte(f.Data), &resultData); err != nil {
				t.Fatalf("解析 tool.result: %v", err)
			}
		case sse.EventMessageDone:
			sawDone = true
		}
	}

	if !sawToolCall {
		t.Fatalf("应发出 tool.call 事件（run_command 也走这条），实际序列: %v", names)
	}
	if !sawToolResult {
		t.Fatalf("应发出 tool.result 事件，实际序列: %v", names)
	}
	if !sawDone {
		t.Fatalf("应发出 message.done 事件，实际序列: %v", names)
	}

	// Risk/RequiresApproval 元信息仍要原样透传（前端用来渲染徽章），
	// 只是不再影响事件名本身。
	if callData.Name != "run_command" {
		t.Fatalf("工具名 = %q，期望 run_command", callData.Name)
	}
	if callData.Risk != "dangerous" {
		t.Fatalf("run_command 的 risk 应为 dangerous，实际 %q", callData.Risk)
	}
	if !callData.RequiresApproval {
		t.Fatal("run_command 的 RequiresApproval 字段应仍为 true（供前端展示徽章）")
	}
	if callData.CallID == "" {
		t.Fatal("tool.call 应带 call_id")
	}
	if resultData.CallID != callData.CallID {
		t.Fatalf("tool.result 的 call_id 应与 tool.call 一致，got %q want %q",
			resultData.CallID, callData.CallID)
	}
	if resultData.IsError {
		t.Fatalf("命令执行不应失败: %s", resultData.Output)
	}
	if !strings.Contains(resultData.Output, "hello-from-run-command") {
		t.Fatalf("工具结果应包含命令输出，实际: %q", resultData.Output)
	}
}
