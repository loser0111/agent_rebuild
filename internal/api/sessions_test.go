package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCreateSession(t *testing.T) {
	env := newTestEnv(t)

	var out createSessionResponse
	env.postJSON(t, "/api/sessions", map[string]any{"title": "我的会话"}, http.StatusCreated, &out)

	if out.Session == nil {
		t.Fatal("响应缺少 session")
	}
	if out.Session.ID == "" || !strings.HasPrefix(out.Session.ID, "sess_") {
		t.Fatalf("ID = %q，期望 sess_ 前缀", out.Session.ID)
	}
	if out.Session.Title != "我的会话" {
		t.Fatalf("Title = %q", out.Session.Title)
	}
	if out.Session.Model != "mock-echo" {
		t.Fatalf("Model = %q，期望回退到配置默认模型", out.Session.Model)
	}
	if out.Session.Status != "idle" {
		t.Fatalf("Status = %q", out.Session.Status)
	}
	if out.Session.CreatedAt.IsZero() || out.Session.UpdatedAt.IsZero() {
		t.Fatal("时间戳不应为零值")
	}
}

// 空 body 也应能创建会话（前端「新建」按钮直连）。
func TestCreateSessionEmptyBody(t *testing.T) {
	env := newTestEnv(t)

	resp, err := http.Post(env.url+"/api/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d，期望 201", resp.StatusCode)
	}

	var out createSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Session == nil || out.Session.Title == "" {
		t.Fatalf("应生成默认标题: %+v", out.Session)
	}
}

func TestCreateSessionInvalidJSON(t *testing.T) {
	env := newTestEnv(t)

	resp, err := http.Post(env.url+"/api/sessions", "application/json", strings.NewReader("{不是JSON"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", resp.StatusCode)
	}
	var body ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Code != string(CodeInvalidArgument) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestListSessions(t *testing.T) {
	env := newTestEnv(t)

	// 空列表：next_cursor 必须是 null。
	var empty listSessionsResponse
	env.getJSON(t, "/api/sessions", http.StatusOK, &empty)
	if len(empty.Sessions) != 0 || empty.NextCursor != nil {
		t.Fatalf("空列表响应错误: %+v", empty)
	}

	for i := 0; i < 3; i++ {
		env.createSession(t, "")
	}

	var list listSessionsResponse
	env.getJSON(t, "/api/sessions", http.StatusOK, &list)
	if len(list.Sessions) != 3 {
		t.Fatalf("会话数 = %d，期望 3", len(list.Sessions))
	}

	// 分页：limit=2 → 2 条 + 游标。
	var page1 listSessionsResponse
	env.getJSON(t, "/api/sessions?limit=2", http.StatusOK, &page1)
	if len(page1.Sessions) != 2 || page1.NextCursor == nil {
		t.Fatalf("第一页错误: %d 条, cursor=%v", len(page1.Sessions), page1.NextCursor)
	}

	var page2 listSessionsResponse
	env.getJSON(t, "/api/sessions?limit=2&cursor="+*page1.NextCursor, http.StatusOK, &page2)
	if len(page2.Sessions) != 1 || page2.NextCursor != nil {
		t.Fatalf("第二页错误: %d 条, cursor=%v", len(page2.Sessions), page2.NextCursor)
	}
}

func TestListSessionsInvalidLimit(t *testing.T) {
	env := newTestEnv(t)
	var body ErrorBody
	env.getJSON(t, "/api/sessions?limit=abc", http.StatusBadRequest, &body)
	if body.Error.Code != string(CodeInvalidArgument) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestGetSessionDetail(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "详情会话")

	var detail sessionDetailResponse
	env.getJSON(t, "/api/sessions/"+sess.ID, http.StatusOK, &detail)

	if detail.Session == nil || detail.Session.ID != sess.ID {
		t.Fatalf("session 字段错误: %+v", detail.Session)
	}
	if len(detail.Messages) != 0 {
		t.Fatalf("新会话不应有消息，得到 %d 条", len(detail.Messages))
	}
	if detail.LastEventID != 0 {
		t.Fatalf("last_event_id = %d，期望 0", detail.LastEventID)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	env := newTestEnv(t)
	var body ErrorBody
	env.getJSON(t, "/api/sessions/sess_missing", http.StatusNotFound, &body)
	if body.Error.Code != string(CodeSessionNotFound) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestPatchSession(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "旧标题")

	var out createSessionResponse
	env.patchJSON(t, "/api/sessions/"+sess.ID, map[string]any{"title": "  新标题  "}, http.StatusOK, &out)
	if out.Session.Title != "新标题" {
		t.Fatalf("Title = %q，期望被清洗并更新", out.Session.Title)
	}

	// 未提供任何字段 → 400。
	var body ErrorBody
	env.patchJSON(t, "/api/sessions/"+sess.ID, map[string]any{}, http.StatusBadRequest, &body)
	if body.Error.Code != string(CodeInvalidArgument) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestPatchSessionNotFound(t *testing.T) {
	env := newTestEnv(t)
	var body ErrorBody
	env.patchJSON(t, "/api/sessions/sess_missing", map[string]any{"title": "x"}, http.StatusNotFound, &body)
	if body.Error.Code != string(CodeSessionNotFound) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestDeleteSession(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "待删除")

	resp := env.do(t, http.MethodDelete, "/api/sessions/"+sess.ID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204", resp.StatusCode)
	}

	var body ErrorBody
	env.getJSON(t, "/api/sessions/"+sess.ID, http.StatusNotFound, &body)
	if body.Error.Code != string(CodeSessionNotFound) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}

	// 重复删除 → 404。
	resp2 := env.do(t, http.MethodDelete, "/api/sessions/"+sess.ID)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除 status = %d，期望 404", resp2.StatusCode)
	}
}

// 删除正在生成的会话：先取消生成，再删除，且不留下 409 残留。
func TestDeleteSessionWhileRunning(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "删除时仍在生成"})

	del := env.do(t, http.MethodDelete, "/api/sessions/"+sess.ID)
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("删除 status = %d，期望 204", del.StatusCode)
	}
	resp.Body.Close()

	var body ErrorBody
	env.getJSON(t, "/api/sessions/"+sess.ID, http.StatusNotFound, &body)
}

// 方法不匹配时应返回 405 + Allow 头。
func TestMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodPut, "/api/sessions")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d，期望 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow = %q，期望 \"GET, POST\"", got)
	}
	var body ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != string(CodeMethodNotAllowed) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

// 请求体超过 1MB 应被拒绝。
func TestPayloadTooLarge(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	huge := strings.Repeat("a", 2<<20)
	resp, err := http.Post(env.url+"/api/sessions/"+sess.ID+"/messages",
		"application/json", strings.NewReader(`{"content":"`+huge+`"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", resp.StatusCode)
	}
}
