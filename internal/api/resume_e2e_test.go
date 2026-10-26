package api

// resume 端到端测试：提问 → 挂起 → （模拟重启）→ 恢复 → 继续回答。
//
// 「模拟重启」的做法：挂起后直接丢弃当前的 Server 与 executor，
// 用同一个数据库文件重建一套全新的服务实例。这是最接近真实的验证——
// 内存里的 Task、Loop、Hub 全部消失，只剩 SQLite 里的 messages
// 与 checkpoints，恢复能力是否真的成立一目了然。

import (
	"context"
	"encoding/json"
	"errors"
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

// resumeGateway 首轮 ask_user；此后（历史里有 tool 结果）给出最终答案。
type resumeGateway struct {
	question string
	options  []string
}

func (g *resumeGateway) handler(t *testing.T) http.HandlerFunc {
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

		// 历史里出现 tool 结果 → 用户已回答 → 给最终结论。
		for _, m := range body.Messages {
			if m["role"] == "tool" {
				fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"好，就用 SQLite 建表。"}}]}`+"\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				flush()
				return
			}
		}

		args, _ := json.Marshal(map[string]any{
			"question": g.question,
			"options":  g.options,
		})
		fmt.Fprintf(w,
			`data: {"choices":[{"delta":{"content":"先确认一下："}}]}`+"\n\n"+
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ask_1","type":"function","function":{"name":"ask_user","arguments":%s}}]}}]}`+"\n\n"+
				"data: [DONE]\n\n",
			jsonString(string(args)))
		flush()
	}
}

// resumeFixture 是一套可重建的服务实例（模拟进程重启）。
type resumeFixture struct {
	ts    *httptest.Server
	cps   *sqlite.CheckpointStore
	store *sqlite.SessionStore
	db    *sqlite.DB
}

// newResumeFixture 用指定数据库路径组装服务；dbPath 相同即「同一个库」。
func newResumeFixture(t *testing.T, dbPath, gwURL string) *resumeFixture {
	t.Helper()

	db, err := sqlite.Open(dbPath)
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

	runner := agent.NewLLMRunner(gwURL, "k", "m", "")
	s := NewServer(cfg, store, runner, agent.NewLocks())
	s.SetTools(tool.DefaultRegistry(tool.Options{Workspace: t.TempDir()}), "", 20, 10*time.Second)
	s.SetCheckpointer(cps)

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	return &resumeFixture{ts: ts, cps: cps, store: store, db: db}
}

// createSession 建一个会话，返回 ID。
func createSession(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/sessions", "application/json",
		strings.NewReader(`{"title":"resume"}`))
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
	if created.Session.ID == "" {
		t.Fatal("会话 ID 为空")
	}
	return created.Session.ID
}

// postJSON 发一个 JSON POST，返回状态码与响应体。
func postJSON(t *testing.T, url string, payload any) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, buf
}

// 核心场景：挂起 → 重建服务（模拟重启）→ resume → 得到最终回答。
func TestResumeAfterRestart(t *testing.T) {
	gw := &resumeGateway{question: "用哪种数据库？", options: []string{"SQLite", "Postgres"}}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "persist.db")
	ctx := context.Background()

	// ---- 第一次启动 ----
	first := newResumeFixture(t, dbPath, gwSrv.URL)
	sid := createSession(t, first.ts)
	sendAndCollect(t, first.ts, sid, "帮我建个用户表")

	// 确认已挂起且 checkpoint 带 sessionId。
	cp, err := first.cps.LoadCheckpoint(ctx, sid)
	if err != nil {
		t.Fatalf("应已挂起: %v", err)
	}
	if cp.SessionID != sid {
		t.Fatalf("checkpoint.session_id = %q，期望 %q", cp.SessionID, sid)
	}
	callID := cp.Pendings[0].CallID

	// ---- 模拟进程重启：关掉整套服务，用同一个库重建 ----
	first.ts.Close()
	if err := first.db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}

	second := newResumeFixture(t, dbPath, gwSrv.URL)

	// 重启后仍能读到挂起状态（这是跨进程恢复的前提）。
	cp2, err := second.cps.LoadCheckpoint(ctx, sid)
	if err != nil {
		t.Fatalf("重启后应仍能读到 checkpoint: %v", err)
	}
	if len(cp2.Pendings) == 0 || cp2.Pendings[0].CallID != callID {
		t.Fatalf("重启后 Pendings 丢失: %+v", cp2.Pendings)
	}

	// 会话状态应为 suspended，而不是被 RecoverStaleRunning 清成 idle。
	sess, err := second.store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("读取会话: %v", err)
	}
	if sess.Status != session.SessionSuspended {
		t.Fatalf("重启后会话状态 = %q，期望 suspended", sess.Status)
	}

	// ---- resume：提交回答 ----
	events := postResumeAndCollect(t, second.ts, sid, callID, "用 SQLite")

	var sawDone bool
	for _, e := range events {
		if e == "message.done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("resume 后应产出最终回答，实际事件: %v", events)
	}

	// checkpoint 应被清理（问题已回答）。
	if _, err := second.cps.LoadCheckpoint(ctx, sid); !errors.Is(err, session.ErrCheckpointNotFound) {
		t.Fatalf("resume 后 checkpoint 应被删除，实际: %v", err)
	}

	// 历史应包含：user → assistant(tool_calls) → tool(回答) → assistant(最终回答)
	msgs, err := second.store.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("读取消息: %v", err)
	}
	var hasToolAnswer, hasFinalAnswer bool
	for _, m := range msgs {
		if m.Role == session.RoleTool && m.ToolCallID == callID && m.Content == "用 SQLite" {
			hasToolAnswer = true
		}
		if m.Role == session.RoleAssistant && strings.Contains(m.Content, "SQLite 建表") {
			hasFinalAnswer = true
		}
	}
	if !hasToolAnswer {
		t.Fatalf("历史里应有用户的回答（tool 消息），实际: %s", dumpMsgs(msgs))
	}
	if !hasFinalAnswer {
		t.Fatalf("历史里应有最终回答，实际: %s", dumpMsgs(msgs))
	}
}

// postResumeAndCollect 发 resume 请求并读完 SSE。
func postResumeAndCollect(t *testing.T, ts *httptest.Server, sid, callID, answer string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/sessions/"+sid+"/resume",
		strings.NewReader(fmt.Sprintf(`{"call_id":%s,"answer":%s}`,
			jsonString(callID), jsonString(answer))))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("resume 请求失败: %v", err)
	}
	defer resp.Body.Close()

	var events []string
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(string(buf), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, strings.TrimSpace(name))
		}
	}
	return events
}

func dumpMsgs(msgs []session.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		tc := ""
		for _, c := range m.ToolCalls {
			tc += c.ID
		}
		fmt.Fprintf(&sb, "[%s id=%s tc=%s tcid=%s content=%q] ",
			m.Role, m.ID, tc, m.ToolCallID, m.Content)
	}
	return sb.String()
}

// call_id 不匹配必须拒绝（幂等保护，防止重复提交）。
func TestResumeRejectsMismatchedCallID(t *testing.T) {
	gw := &resumeGateway{question: "问题？"}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	fx := newResumeFixture(t, filepath.Join(t.TempDir(), "x.db"), gwSrv.URL)
	sid := createSession(t, fx.ts)
	sendAndCollect(t, fx.ts, sid, "开始")

	code, body := postJSON(t, fx.ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"call_id": "call_wrong", "answer": "答案",
	})
	if code != http.StatusConflict {
		t.Fatalf("call_id 不匹配应返回 409，实际 %d: %s", code, body)
	}
	// checkpoint 不能被破坏，用户应能拿正确 call_id 重试。
	cp, err := fx.cps.LoadCheckpoint(context.Background(), sid)
	if err != nil || len(cp.Pendings) == 0 {
		t.Fatalf("失败的 resume 不应破坏 checkpoint: %v", err)
	}
}

// 没有挂起项时 resume 应返回 409，而不是 500。
func TestResumeWithoutSuspension(t *testing.T) {
	gw := &resumeGateway{question: "问题？"}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	fx := newResumeFixture(t, filepath.Join(t.TempDir(), "y.db"), gwSrv.URL)
	sid := createSession(t, fx.ts)

	code, body := postJSON(t, fx.ts.URL+"/api/sessions/"+sid+"/resume", map[string]any{
		"call_id": "call_x", "answer": "答案",
	})
	if code != http.StatusConflict {
		t.Fatalf("无挂起项应返回 409，实际 %d: %s", code, body)
	}
}

// 空 answer / 空 call_id 应被 400 拦住。
func TestResumeValidatesInput(t *testing.T) {
	gw := &resumeGateway{question: "问题？"}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	fx := newResumeFixture(t, filepath.Join(t.TempDir(), "z.db"), gwSrv.URL)
	sid := createSession(t, fx.ts)

	cases := []map[string]any{
		{"call_id": "", "answer": "x"},
		{"call_id": "c", "answer": ""},
		{"call_id": "c", "answer": "   "},
	}
	for _, c := range cases {
		code, body := postJSON(t, fx.ts.URL+"/api/sessions/"+sid+"/resume", c)
		if code != http.StatusBadRequest {
			t.Fatalf("非法入参应返回 400，实际 %d: %s（入参 %+v）", code, body, c)
		}
	}
}

// checkpoint 查询接口：刷新页面后能恢复问答卡片。
func TestGetCheckpointExposesQuestion(t *testing.T) {
	gw := &resumeGateway{question: "选哪个？", options: []string{"A", "B"}}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	fx := newResumeFixture(t, filepath.Join(t.TempDir(), "c.db"), gwSrv.URL)
	sid := createSession(t, fx.ts)
	sendAndCollect(t, fx.ts, sid, "开始")

	resp, err := http.Get(fx.ts.URL + "/api/sessions/" + sid + "/checkpoint")
	if err != nil {
		t.Fatalf("查询 checkpoint: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Suspended bool     `json:"suspended"`
		CallID    string   `json:"call_id"`
		Question  string   `json:"question"`
		Options   []string `json:"options"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if !body.Suspended {
		t.Fatal("应报告 suspended=true")
	}
	if body.CallID == "" {
		t.Fatal("应返回 call_id（前端 resume 时需要）")
	}
	if body.Question != "选哪个？" {
		t.Fatalf("question = %q", body.Question)
	}
	if len(body.Options) != 2 {
		t.Fatalf("options = %v", body.Options)
	}
}

// 没有挂起项时 checkpoint 接口应返回 suspended=false（而不是 404）。
func TestGetCheckpointWhenIdle(t *testing.T) {
	gw := &resumeGateway{question: "问题？"}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()

	fx := newResumeFixture(t, filepath.Join(t.TempDir(), "i.db"), gwSrv.URL)
	sid := createSession(t, fx.ts)

	resp, err := http.Get(fx.ts.URL + "/api/sessions/" + sid + "/checkpoint")
	if err != nil {
		t.Fatalf("查询 checkpoint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", resp.StatusCode)
	}
	var body struct {
		Suspended bool `json:"suspended"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Suspended {
		t.Fatal("空闲会话不应报告 suspended")
	}
}
