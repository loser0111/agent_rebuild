package api

// 同轮多个 ask_user 的端到端验证。
//
// 真实故障：模型一次抛两个问题（「你到哪一步了？」「要不要我改代码？」），
// 早期实现遇到第一个就挂起并 return，第二个问题的 tool_calls 已经落库，
// 却永远不会有人回答——界面上表现为「问题静默消失」，
// 而历史里留下一条永远等不到的孤儿 tool_calls。
//
// 本测试守住：
//  1. 两个问题都进 checkpoint；
//  2. 前端收到两个 awaiting.input 事件；
//  3. 批量提交后两条 tool 结果都写入，顺序与提问一致；
//  4. 只答一个会被拒绝（避免模型拿到残缺上下文）。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
	"agent_rebuild/internal/tool"
)

// multiAskGateway 首轮一次抛两个问题；历史里出现 tool 结果后给最终答案。
type multiAskGateway struct{}

func (g *multiAskGateway) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := r.Context().Done(), false; ok {
				_ = f
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		toolCount := 0
		for _, m := range body.Messages {
			if m["role"] == "tool" {
				toolCount++
			}
		}
		// 两个问题都有结果了 → 收尾。
		if toolCount >= 2 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"明白了，按你说的做。"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}

		q1 := `{"question":"做到哪一步了？","options":["阶段1","阶段3"]}`
		q2 := `{"question":"要不要我改代码？","options":["要","不要"]}`
		// 同一条 assistant 消息里带两个 tool_calls。
		// arguments 是「JSON 字符串」，所以要用 jsonString 再包一层引号。
		fmt.Fprintf(w,
			`data: {"choices":[{"delta":{"content":"先确认两件事："}}]}`+"\n\n"+
				`data: {"choices":[{"delta":{"tool_calls":[`+
				`{"index":0,"id":"call_q1","type":"function","function":{"name":"ask_user","arguments":%s}},`+
				`{"index":1,"id":"call_q2","type":"function","function":{"name":"ask_user","arguments":%s}}`+
				`]}}]}`+"\n\n"+
				"data: [DONE]\n\n",
			jsonString(q1), jsonString(q2))
		flush()
	}
}

func newMultiAskEnv(t *testing.T) (*httptest.Server, *sqlite.CheckpointStore, *sqlite.SessionStore, string) {
	t.Helper()

	gw := &multiAskGateway{}
	gwSrv := httptest.NewServer(gw.handler(t))
	t.Cleanup(gwSrv.Close)

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "multi.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		Port: "0", Env: "test",
		CORSOrigins:           []string{"http://localhost:5173"},
		MaxMessagesPerSession: 200, SSEBufferSize: 200,
		SSEPingInterval: time.Hour, RunTimeout: 60 * time.Second,
		LLMProvider: "mock", LLMModel: "mock-echo",
	}
	store := sqlite.NewSessionStore(db, cfg.MaxMessagesPerSession)
	cps := sqlite.NewCheckpointStore(db)

	runner := agent.NewLLMRunner(gwSrv.URL, "k", "m", "")
	s := NewServer(cfg, store, runner, agent.NewLocks())
	s.SetTools(tool.DefaultRegistry(tool.Options{Workspace: t.TempDir()}), "", 20, 10*time.Second)
	s.SetCheckpointer(cps)

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/sessions", "application/json",
		strings.NewReader(`{"title":"multi"}`))
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	defer resp.Body.Close()
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	return ts, cps, store, created.Session.ID
}

// collectAwaiting 发消息并解析出所有 awaiting.input 的 call_id。
func collectAwaiting(t *testing.T, ts *httptest.Server, sid string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sessions/"+sid+"/messages",
		strings.NewReader(`{"content":"开始"}`))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("发消息: %v", err)
	}
	defer resp.Body.Close()

	var callIDs []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var curEvent string
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			curEvent = strings.TrimSpace(name)
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && curEvent == "awaiting.input" {
			var d struct {
				CallID string `json:"call_id"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.CallID != "" {
				callIDs = append(callIDs, d.CallID)
			}
			curEvent = ""
		}
	}
	return callIDs
}

// 核心：同轮两个 ask_user 都必须挂起并被前端看到。
func TestMultiAskUserBothSuspended(t *testing.T) {
	ts, cps, _, sid := newMultiAskEnv(t)

	callIDs := collectAwaiting(t, ts, sid)
	if len(callIDs) != 2 {
		t.Fatalf("应收到 2 个 awaiting.input 事件，实际 %d：%v", len(callIDs), callIDs)
	}
	if callIDs[0] != "call_q1" || callIDs[1] != "call_q2" {
		t.Fatalf("call_id 顺序应与模型提问顺序一致，实际: %v", callIDs)
	}

	cp, err := cps.LoadCheckpoint(context.Background(), sid)
	if err != nil {
		t.Fatalf("读取 checkpoint: %v", err)
	}
	if len(cp.Pendings) != 2 {
		t.Fatalf("checkpoint 应存 2 个待回答项，实际 %d：%+v", len(cp.Pendings), cp.Pendings)
	}
	if cp.Pendings[0].Question != "做到哪一步了？" {
		t.Fatalf("第一个问题内容错误: %q", cp.Pendings[0].Question)
	}
	if cp.Pendings[1].Question != "要不要我改代码？" {
		t.Fatalf("第二个问题内容错误: %q", cp.Pendings[1].Question)
	}
}

// 只答一个应被拒绝：残缺上下文会让模型做出奇怪推断。
func TestMultiAskRejectsPartialAnswers(t *testing.T) {
	ts, _, _, sid := newMultiAskEnv(t)
	collectAwaiting(t, ts, sid)

	code, body := postJSON(t, ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"answers": map[string]string{"call_q1": "阶段1"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("漏答应返回 400，实际 %d: %s", code, body)
	}
	if !strings.Contains(string(body), "未回答") {
		t.Fatalf("错误信息应说明还有未回答的问题，实际: %s", body)
	}
}

// 批量提交：两条 tool 结果都写入，顺序与提问一致。
func TestMultiAskBatchResume(t *testing.T) {
	ts, cps, store, sid := newMultiAskEnv(t)
	collectAwaiting(t, ts, sid)

	code, body := postJSON(t, ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"answers": map[string]string{
			"call_q1": "阶段3",
			"call_q2": "要",
		},
		"stream": false,
	})
	if code != http.StatusOK {
		t.Fatalf("批量 resume 应成功，实际 %d: %s", code, body)
	}

	// checkpoint 应清理干净。
	if _, err := cps.LoadCheckpoint(context.Background(), sid); err == nil {
		t.Fatal("resume 后 checkpoint 应被删除")
	}

	msgs, err := store.Messages(context.Background(), sid)
	if err != nil {
		t.Fatalf("读取消息: %v", err)
	}
	// 按写入顺序收集 tool 结果。
	var got []string
	for _, m := range msgs {
		if m.Role == session.RoleTool {
			got = append(got, m.ToolCallID+"="+m.Content)
		}
	}
	// 最终生成那一轮可能又触发一次 ask_user（假网关在 toolCount<2 时才提问，
	// 这里已有 2 条，所以不会再问）。
	if len(got) < 2 {
		t.Fatalf("应至少写入 2 条 tool 结果，实际 %d：%v", len(got), got)
	}
	if got[0] != "call_q1=阶段3" || got[1] != "call_q2=要" {
		t.Fatalf("tool 结果顺序或内容错误：%v", got)
	}
}

// 未回答问题不应破坏 checkpoint（用户可改完再提交）。
func TestMultiAskPartialFailureKeepsCheckpoint(t *testing.T) {
	ts, cps, _, sid := newMultiAskEnv(t)
	collectAwaiting(t, ts, sid)

	postJSON(t, ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"answers": map[string]string{"call_q1": "阶段1"},
	})

	cp, err := cps.LoadCheckpoint(context.Background(), sid)
	if err != nil {
		t.Fatalf("失败的提交不应删除 checkpoint: %v", err)
	}
	if len(cp.Pendings) != 2 {
		t.Fatalf("checkpoint 应保持完整，实际 %d 项", len(cp.Pendings))
	}
}

// checkpoint 查询接口应返回全部问题。
func TestGetCheckpointReturnsAllQuestions(t *testing.T) {
	ts, _, _, sid := newMultiAskEnv(t)
	collectAwaiting(t, ts, sid)

	resp, err := http.Get(ts.URL + "/api/sessions/" + sid + "/checkpoint")
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var body struct {
		Suspended bool `json:"suspended"`
		Questions []struct {
			CallID   string   `json:"call_id"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("解析失败: %v（原文 %s）", err, raw)
	}
	if !body.Suspended {
		t.Fatal("应报告 suspended")
	}
	if len(body.Questions) != 2 {
		t.Fatalf("应返回 2 个问题，实际 %d：%s", len(body.Questions), raw)
	}
	if body.Questions[1].CallID != "call_q2" {
		t.Fatalf("第二个问题 call_id 错误: %q", body.Questions[1].CallID)
	}
	if len(body.Questions[1].Options) != 2 {
		t.Fatalf("第二个问题选项丢失: %v", body.Questions[1].Options)
	}
}
