package api

// 一个 ask_user 调用内包含多个问题（questions 数组）的端到端验证。
//
// 这是「一次问多个问题」真正要解决的问题：
//
//	模型之前的 schema 是 { question, options }，一次只能问一个。
//	想收集 5 个信息就要挂起 5 次 —— 用户要等 5 轮，体验极差。
//
// 现在 schema 是 { questions: [{question, options}, ...] }，
// 一次调用装下全部问题，一条 checkpoint、一次回答、一条 tool 结果。
//
// 本测试守住：
//  1. 一个 call_id 下的多个问题都进 checkpoint（QID 各不相同）；
//  2. 前端收到多个 awaiting.input，但 call_id 相同、qid 不同；
//  3. 批量提交后**只写一条** tool 结果（多条会让 OpenAI 400）；
//  4. 这条结果里包含全部问答对，模型能对上号。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

// oneCallMultiQuestionGateway 首轮用**一次** ask_user 问 3 个问题。
type oneCallMultiQuestionGateway struct{}

func (g *oneCallMultiQuestionGateway) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		hasToolResult := false
		for _, m := range body.Messages {
			if m["role"] == "tool" {
				hasToolResult = true
			}
		}
		if hasToolResult {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"收到，全部按你说的做。"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}

		// 一次调用里装 3 个问题。
		args := `{
			"questions":[
				{"id":"db","question":"用哪种数据库？","options":["SQLite","Postgres"]},
				{"id":"scope","question":"需要改哪些文件？"},
				{"question":"要不要补测试？","options":["要","不要"]}
			]
		}`
		fmt.Fprintf(w,
			`data: {"choices":[{"delta":{"content":"先确认几个问题："}}]}`+"\n\n"+
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_multi",`+
				`"type":"function","function":{"name":"ask_user","arguments":%s}}]}}]}`+"\n\n"+
				"data: [DONE]\n\n",
			jsonString(args))
		flush()
	}
}

func newOneCallEnv(t *testing.T) (*httptest.Server, *sqlite.CheckpointStore, *sqlite.SessionStore, string) {
	t.Helper()

	gw := &oneCallMultiQuestionGateway{}
	gwSrv := httptest.NewServer(gw.handler(t))
	t.Cleanup(gwSrv.Close)

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "one.db"))
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
		strings.NewReader(`{"title":"one-call"}`))
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

// collectAwaitingFull 解析 awaiting.input 事件，返回 (call_id, qid) 列表。
func collectAwaitingFull(t *testing.T, ts *httptest.Server, sid string) []struct{ CallID, QID string } {
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

	var out []struct{ CallID, QID string }
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	cur := ""
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			cur = strings.TrimSpace(name)
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && cur == "awaiting.input" {
			var d struct {
				CallID string `json:"call_id"`
				QID    string `json:"qid"`
			}
			if json.Unmarshal([]byte(data), &d) == nil {
				out = append(out, struct{ CallID, QID string }{d.CallID, d.QID})
			}
			cur = ""
		}
	}
	return out
}

// 核心：一次调用里的 3 个问题都要挂起、且共享同一个 call_id。
func TestOneCallMultiQuestionSuspendsAll(t *testing.T) {
	ts, cps, _, sid := newOneCallEnv(t)

	events := collectAwaitingFull(t, ts, sid)
	if len(events) != 3 {
		t.Fatalf("应收到 3 个 awaiting.input，实际 %d：%+v", len(events), events)
	}
	for i, ev := range events {
		if ev.CallID != "call_multi" {
			t.Fatalf("第 %d 个问题的 call_id 应为 call_multi，实际 %q", i+1, ev.CallID)
		}
	}
	// qid 必须各不相同，否则前端无法区分。
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.QID == "" {
			t.Fatal("qid 不能为空")
		}
		if seen[ev.QID] {
			t.Fatalf("qid 重复: %q", ev.QID)
		}
		seen[ev.QID] = true
	}
	if !seen["db"] || !seen["scope"] {
		t.Fatalf("显式 id 应保留，实际: %+v", seen)
	}

	cp, err := cps.LoadCheckpoint(context.Background(), sid)
	if err != nil {
		t.Fatalf("读取 checkpoint: %v", err)
	}
	if len(cp.Pendings) != 3 {
		t.Fatalf("checkpoint 应有 3 个问题，实际 %d", len(cp.Pendings))
	}
	if cp.Pendings[0].Question != "用哪种数据库？" {
		t.Fatalf("问题顺序错误: %+v", cp.Pendings)
	}
}

// 批量提交：只写**一条** tool 结果（多条会让 OpenAI 400）。
func TestOneCallMultiQuestionSingleToolResult(t *testing.T) {
	ts, cps, store, sid := newOneCallEnv(t)
	collectAwaitingFull(t, ts, sid)

	cp, _ := cps.LoadCheckpoint(context.Background(), sid)
	answers := map[string]string{}
	for _, p := range cp.Pendings {
		answers[p.CallID+"::"+p.QID] = "答-" + p.QID
	}

	code, body := postJSON(t, ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"answers": answers,
		"stream":  false,
	})
	if code != http.StatusOK {
		t.Fatalf("批量提交应成功，实际 %d: %s", code, body)
	}

	msgs, err := store.Messages(context.Background(), sid)
	if err != nil {
		t.Fatalf("读取消息: %v", err)
	}
	var toolMsgs []session.Message
	for _, m := range msgs {
		if m.Role == session.RoleTool {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("应只写 1 条 tool 结果（多条会让 OpenAI 400），实际 %d 条：%s",
			len(toolMsgs), dumpMsgs(msgs))
	}
	if toolMsgs[0].ToolCallID != "call_multi" {
		t.Fatalf("tool 结果的 call_id 错误: %q", toolMsgs[0].ToolCallID)
	}

	// 内容应包含全部问答对，模型才能对上号。
	content := toolMsgs[0].Content
	for _, want := range []string{"用哪种数据库？", "答-db", "需要改哪些文件？", "答-scope", "要不要补测试？"} {
		if !strings.Contains(content, want) {
			t.Fatalf("tool 结果缺少 %q，实际内容:\n%s", want, content)
		}
	}
}

// 漏答一个应被拒绝，且不破坏 checkpoint。
func TestOneCallMultiQuestionPartialRejected(t *testing.T) {
	ts, cps, _, sid := newOneCallEnv(t)
	collectAwaitingFull(t, ts, sid)

	code, body := postJSON(t, ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"answers": map[string]string{"call_multi::db": "SQLite"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("漏答应返回 400，实际 %d: %s", code, body)
	}

	cp, err := cps.LoadCheckpoint(context.Background(), sid)
	if err != nil {
		t.Fatalf("失败的提交不应删除 checkpoint: %v", err)
	}
	if len(cp.Pendings) != 3 {
		t.Fatalf("checkpoint 应保持 3 个问题，实际 %d", len(cp.Pendings))
	}
}

// 旧格式兼容：只给 call_id + answer 时，单问题调用仍能工作。
func TestOneCallLegacySingleAnswerStillWorks(t *testing.T) {
	// 用单问题网关（旧 schema 的调用形态）。
	ts, cps, store, sid := newSingleQuestionEnv(t)
	collectAwaitingFull(t, ts, sid)

	code, body := postJSON(t, ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"call_id": "call_single",
		"answer":  "SQLite",
		"stream":  false,
	})
	if code != http.StatusOK {
		t.Fatalf("旧格式应仍可用，实际 %d: %s", code, body)
	}

	msgs, _ := store.Messages(context.Background(), sid)
	var toolContent string
	for _, m := range msgs {
		if m.Role == session.RoleTool {
			toolContent = m.Content
		}
	}
	// 单个问题时只给答案本身，不带「问题 → 答案」样板。
	if toolContent != "SQLite" {
		t.Fatalf("单个问题的 tool 结果应只含答案，实际 %q", toolContent)
	}
	_ = cps
}

// newSingleQuestionEnv 是「旧 schema」形态的单问题网关。
func newSingleQuestionEnv(t *testing.T) (*httptest.Server, *sqlite.CheckpointStore, *sqlite.SessionStore, string) {
	t.Helper()

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m["role"] == "tool" {
				fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"好。"}}]}`+"\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				flush()
				return
			}
		}
		args := `{"question":"用哪种库？","options":["SQLite","Postgres"]}`
		fmt.Fprintf(w,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_single",`+
				`"type":"function","function":{"name":"ask_user","arguments":%s}}]}}]}`+"\n\n"+
				"data: [DONE]\n\n",
			jsonString(args))
		flush()
	}))
	t.Cleanup(gwSrv.Close)

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "single.db"))
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
		strings.NewReader(`{"title":"single"}`))
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
