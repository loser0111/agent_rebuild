package api

import (
	"context"
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
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
	"agent_rebuild/internal/tool"
)

// 复现线上现象：一次对话里模型多次调用工具，但落库的 tool_calls 远少于实际。
//
// 关键怀疑点：persistCtx 只给 5 秒，而工具循环可能跑很久；
// 或者 res.AssistantMessages 与 ToolResults 不成对。

// multiToolGateway 前 N 次请求返回工具调用，最后返回最终答案。
type multiToolGateway struct {
	mu       sync.Mutex
	n        int
	toolRuns int // 要求调用几轮工具
}

func (g *multiToolGateway) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.n++
		cur := g.n
		g.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		if cur <= g.toolRuns {
			// 每轮调用一次 list_dir，call id 各不相同
			body := fmt.Sprintf(
				`data: {"choices":[{"delta":{"content":"第%d步分析。","tool_calls":[{"index":0,"id":"call_%d","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]}}]}`+"\n\n",
				cur, cur)
			fmt.Fprint(w, body)
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"最终答案"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}
}

// TestAllToolCallsPersisted 5 轮工具调用必须全部落库。
func TestAllToolCallsPersisted(t *testing.T) {
	const rounds = 5

	gw := &multiToolGateway{toolRuns: rounds}
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
		SSEPingInterval: time.Hour, RunTimeout: 60 * time.Second,
		LLMProvider: "mock", LLMModel: "mock-echo",
	}
	store := sqlite.NewSessionStore(db, cfg.MaxMessagesPerSession)

	runner := func() agent.Runner {
		return agent.NewLLMRunner(gwSrv.URL, "k", "m", "")
	}
	s := NewServer(cfg, store, runner(), agent.NewLocks())
	root := t.TempDir()
	s.SetTools(tool.DefaultRegistry(tool.Options{Workspace: root}), "", 20, 10*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 建会话
	resp, err := http.Post(ts.URL+"/api/sessions", "application/json",
		strings.NewReader(`{"title":"t"}`))
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sid := created.Session.ID

	// 发消息（流式）
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sessions/"+sid+"/messages",
		strings.NewReader(`{"content":"帮我看看项目"}`))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	sResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("发消息失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, sResp.Body)
	sResp.Body.Close()

	// 落库检查
	msgs, err := store.Messages(context.Background(), sid)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}

	var asstWithCalls, toolMsgs int
	var paired bool
	for i, m := range msgs {
		if m.Role == session.RoleAssistant && len(m.ToolCalls) > 0 {
			asstWithCalls += len(m.ToolCalls)
			// 检查下一条是否是对应的 tool 结果（OpenAI 要求严格相邻）
			if i+1 < len(msgs) && msgs[i+1].Role == session.RoleTool &&
				msgs[i+1].ToolCallID == m.ToolCalls[0].ID {
				paired = true
			}
		}
		if m.Role == session.RoleTool {
			toolMsgs++
		}
	}

	t.Logf("模型请求 %d 次，落库: assistant带tool_calls=%d, tool结果=%d", gw.n, asstWithCalls, toolMsgs)

	if asstWithCalls != rounds {
		t.Errorf("应有 %d 个 tool_calls 落库，实际 %d", rounds, asstWithCalls)
	}
	if toolMsgs != rounds {
		t.Errorf("应有 %d 条 tool 结果落库，实际 %d", rounds, toolMsgs)
	}
	if !paired {
		t.Error("tool 结果未紧跟在对应 tool_calls 之后（OpenAI 会拒绝后续请求）")
	}
}
