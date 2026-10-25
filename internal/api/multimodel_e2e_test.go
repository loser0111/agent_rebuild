package api

import (
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
	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
)

// multiModelEnv 用 SQLite 装配一套支持多模型的完整服务。
//
// 为什么必须用 SQLite：模型配置只有 SQL 实现（memory 驱动会返回 501），
// 而「会话选模型」的链路正好依赖它。
type multiModelEnv struct {
	url     string
	http    *httptest.Server
	store   *sqlite.SessionStore
	models  modelconfig.Store
	cleanup func()
}

// builtRunners 记录 resolver 实际构造了哪些模型配置，
// 用于断言「确实换了模型」而不是碰巧回退到默认。
func newMultiModelEnv(t *testing.T) (*multiModelEnv, *[]string) {
	t.Helper()

	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}

	cfg := &config.Config{
		Port:                  "0",
		Env:                   "test",
		CORSOrigins:           []string{"http://localhost:5173"},
		MaxMessagesPerSession: 200,
		SSEBufferSize:         200,
		SSEPingInterval:       time.Hour,
		RunTimeout:            30 * time.Second,
		LLMProvider:           "mock",
		LLMModel:              "mock-echo",
	}

	store := sqlite.NewSessionStore(db, cfg.MaxMessagesPerSession)
	models := modelconfig.NewSQLStore(db.SQL(), filepath.Join(dir, "key"))

	runner := agent.NewMockRunner(cfg.LLMModel)
	runner.Chunk = time.Millisecond

	s := NewServer(cfg, store, runner, agent.NewLocks())

	// 记录每次解析到的配置，方便断言。
	built := &[]string{}
	s.SetModelConfigStore(models, func(mc *modelconfig.Config) (agent.Runner, error) {
		*built = append(*built, mc.Name)
		m := agent.NewMockRunner(mc.Model)
		m.Chunk = time.Millisecond
		return m, nil
	})

	ts := httptest.NewServer(s.Handler())
	env := &multiModelEnv{
		url:    ts.URL,
		http:   ts,
		store:  store,
		models: models,
		cleanup: func() {
			ts.Close()
			_ = db.Close()
		},
	}
	t.Cleanup(env.cleanup)
	return env, built
}

func (e *multiModelEnv) req(t *testing.T, method, path string, body any, wantStatus int, dst any) {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, e.url+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		var buf strings.Builder
		_, _ = fmt.Fprint(&buf, "")
		t.Fatalf("%s %s status = %d，期望 %d", method, path, resp.StatusCode, wantStatus)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
}

// TestMultiModelSelectionSwitchesRunner 验证核心诉求：
// 同一会话切换模型配置后，后续生成确实使用了新配置。
func TestMultiModelSelectionSwitchesRunner(t *testing.T) {
	env, built := newMultiModelEnv(t)

	// 1) 建两个模型配置。
	var a, b modelconfig.Config
	env.req(t, http.MethodPost, "/api/models", map[string]any{
		"name": "配置A", "protocol": "mock", "model": "model-alpha", "is_default": true,
	}, http.StatusCreated, &a)
	env.req(t, http.MethodPost, "/api/models", map[string]any{
		"name": "配置B", "protocol": "mock", "model": "model-beta",
	}, http.StatusCreated, &b)

	if a.ID == "" || b.ID == "" {
		t.Fatalf("模型配置创建失败: a=%q b=%q", a.ID, b.ID)
	}

	// 2) 建会话并切到配置 A。
	var created createSessionResponse
	env.req(t, http.MethodPost, "/api/sessions", map[string]any{"title": "多模型"}, http.StatusCreated, &created)
	sid := created.Session.ID

	var patched createSessionResponse
	env.req(t, http.MethodPatch, "/api/sessions/"+sid,
		map[string]any{"model_config_id": a.ID}, http.StatusOK, &patched)

	if patched.Session.ModelConfigID != a.ID {
		t.Fatalf("会话模型未持久化：期望 %s，实际 %s", a.ID, patched.Session.ModelConfigID)
	}

	// 3) 发消息（非流式，便于断言），确认用的是 A。
	*built = nil
	var r1 sendMessageResponse
	f := false
	env.req(t, http.MethodPost, "/api/sessions/"+sid+"/messages",
		map[string]any{"content": "你好", "stream": f}, http.StatusOK, &r1)

	if len(*built) == 0 || (*built)[0] != "配置A" {
		t.Fatalf("期望执行配置A，实际构造的配置: %v", *built)
	}

	// 4) 切到配置 B，再次发消息，确认换成了 B。
	env.req(t, http.MethodPatch, "/api/sessions/"+sid,
		map[string]any{"model_config_id": b.ID}, http.StatusOK, nil)

	*built = nil
	var r2 sendMessageResponse
	env.req(t, http.MethodPost, "/api/sessions/"+sid+"/messages",
		map[string]any{"content": "再说一次", "stream": f}, http.StatusOK, &r2)

	if len(*built) == 0 || (*built)[0] != "配置B" {
		t.Fatalf("切换后期望执行配置B，实际构造的配置: %v", *built)
	}
}

// TestSendMessageWithModelConfigIDOverride 验证请求级指定优先于会话记录。
func TestSendMessageWithModelConfigIDOverride(t *testing.T) {
	env, built := newMultiModelEnv(t)

	var a, b modelconfig.Config
	env.req(t, http.MethodPost, "/api/models", map[string]any{
		"name": "A", "protocol": "mock", "model": "model-alpha",
	}, http.StatusCreated, &a)
	env.req(t, http.MethodPost, "/api/models", map[string]any{
		"name": "B", "protocol": "mock", "model": "model-beta",
	}, http.StatusCreated, &b)

	var created createSessionResponse
	env.req(t, http.MethodPost, "/api/sessions", map[string]any{}, http.StatusCreated, &created)
	sid := created.Session.ID

	// 会话上存 A，但请求里显式指定 B。
	env.req(t, http.MethodPatch, "/api/sessions/"+sid,
		map[string]any{"model_config_id": a.ID}, http.StatusOK, nil)

	*built = nil
	f := false
	env.req(t, http.MethodPost, "/api/sessions/"+sid+"/messages",
		map[string]any{"content": "用 B", "stream": f, "model_config_id": b.ID},
		http.StatusOK, nil)

	if len(*built) == 0 || (*built)[0] != "B" {
		t.Fatalf("请求级指定应优先，实际: %v", *built)
	}

	// 请求级指定也应被记住，下次不带则沿用 B。
	var detail sessionDetailResponse
	env.req(t, http.MethodGet, "/api/sessions/"+sid, nil, http.StatusOK, &detail)
	if detail.Session.ModelConfigID != b.ID {
		t.Fatalf("请求级模型选择未持久化：期望 %s，实际 %s", b.ID, detail.Session.ModelConfigID)
	}
}

// TestPatchSessionRejectsUnknownModelConfig 验证指向不存在的配置会明确报错。
func TestPatchSessionRejectsUnknownModelConfig(t *testing.T) {
	env, _ := newMultiModelEnv(t)

	var created createSessionResponse
	env.req(t, http.MethodPost, "/api/sessions", map[string]any{}, http.StatusCreated, &created)

	env.req(t, http.MethodPatch, "/api/sessions/"+created.Session.ID,
		map[string]any{"model_config_id": "no-such-config"}, http.StatusNotFound, nil)
}

// TestSendMessageFailsWhenModelConfigDeleted 验证配置被删后不会静默降级到默认。
func TestSendMessageFailsWhenModelConfigDeleted(t *testing.T) {
	env, _ := newMultiModelEnv(t)

	var a modelconfig.Config
	env.req(t, http.MethodPost, "/api/models", map[string]any{
		"name": "临时", "protocol": "mock", "model": "model-alpha",
	}, http.StatusCreated, &a)

	var created createSessionResponse
	env.req(t, http.MethodPost, "/api/sessions", map[string]any{}, http.StatusCreated, &created)

	// 删除配置后直接用它的 id 发消息。
	env.req(t, http.MethodDelete, "/api/models/"+a.ID, nil, http.StatusNoContent, nil)

	f := false
	// 期望非 200：模型配置不可用。
	raw, _ := json.Marshal(map[string]any{
		"content": "还在吗", "stream": f, "model_config_id": a.ID,
	})
	req, _ := http.NewRequest(http.MethodPost, env.url+"/api/sessions/"+created.Session.ID+"/messages",
		strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("配置已删除却返回 200，说明静默降级了")
	}
}

// TestSessionModelConfigPersistsAcrossStoreReopen 验证会话的模型选择落盘。
func TestSessionModelConfigPersistsAcrossStoreReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")

	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	store := sqlite.NewSessionStore(db, 100)

	ctx := context.Background()

	// 直接建一个带 model_config_id 的会话。
	s := &session.Session{
		ID:            "sess-persist",
		Title:         "持久化验证",
		Status:        session.SessionIdle,
		Model:         "mock-echo",
		ModelConfigID: "cfg-123",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := store.Create(ctx, s); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	_ = db.Close()

	// 重开数据库，确认字段还在。
	db2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("重开数据库失败: %v", err)
	}
	defer db2.Close()
	store2 := sqlite.NewSessionStore(db2, 100)

	got, err := store2.Get(ctx, "sess-persist")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if got.ModelConfigID != "cfg-123" {
		t.Fatalf("model_config_id 未持久化：%q", got.ModelConfigID)
	}
}
