package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/session"
)

// testEnv 是对外暴露的测试装置：既能发 HTTP 请求，也能直接读存储断言落库结果。
type testEnv struct {
	server *Server
	store  *session.MemoryStore
	url    string
	http   *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWithRunner(t, nil)
}

// newTestEnvWithRunner 允许注入自定义 Runner（如可控延迟/错误的假执行器）。
func newTestEnvWithRunner(t *testing.T, customize func(r *agent.MockRunner)) *testEnv {
	t.Helper()
	return buildTestEnv(t, customize, "")
}

// newTestEnvWithWeb 启用单端口静态托管，用于 SPA 相关测试。
func newTestEnvWithWeb(t *testing.T, webDir string) *testEnv {
	t.Helper()
	return buildTestEnv(t, nil, webDir)
}

// newTestEnvWithHub 允许指定 hub 缓冲大小（用于测试缓冲裁剪）。
func newTestEnvWithHub(t *testing.T, bufferSize int) *testEnv {
	t.Helper()
	return buildTestEnvWithHub(t, nil, "", bufferSize)
}

func buildTestEnv(t *testing.T, customize func(r *agent.MockRunner), webDir string) *testEnv {
	t.Helper()
	return buildTestEnvWithHub(t, customize, webDir, 200)
}

func buildTestEnvWithHub(t *testing.T, customize func(r *agent.MockRunner), webDir string, bufferSize int) *testEnv {
	t.Helper()
	cfg := &config.Config{
		Port:                  "0",
		Env:                   "test",
		CORSOrigins:           []string{"http://localhost:5173"},
		MaxMessagesPerSession: 200,
		SSEBufferSize:         bufferSize,
		SSEPingInterval:       time.Hour, // 默认不触发 ping，避免污染事件序列断言
		RunTimeout:            30 * time.Second,
		LLMProvider:           "mock",
		LLMModel:              "mock-echo",
	}
	store := session.NewMemoryStore(cfg.MaxMessagesPerSession)

	runner := agent.NewMockRunner(cfg.LLMModel)
	// 测试里把分块间隔压到极小，兼顾速度与「多块输出」。
	runner.Chunk = 1 * time.Millisecond
	if customize != nil {
		customize(runner)
	}

	s := NewServer(cfg, store, runner, agent.NewLocks())
	if webDir != "" {
		s.ServeWeb(webDir)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{server: s, store: store, url: ts.URL, http: ts}
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	return newTestEnv(t).server.Handler()
}

func (e *testEnv) createSession(t *testing.T, title string) *session.Session {
	t.Helper()
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}
	var out createSessionResponse
	e.postJSON(t, "/api/sessions", body, http.StatusCreated, &out)
	return out.Session
}

func (e *testEnv) postJSON(t *testing.T, path string, body any, wantStatus int, dst any) {
	t.Helper()
	var (
		resp *http.Response
		err  error
	)
	if body == nil {
		resp, err = http.Post(e.url+path, "application/json", nil)
	} else {
		raw, _ := json.Marshal(body)
		resp, err = http.Post(e.url+path, "application/json", bytesReader(raw))
	}
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s status = %d，期望 %d", path, resp.StatusCode, wantStatus)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func (e *testEnv) getJSON(t *testing.T, path string, wantStatus int, dst any) {
	t.Helper()
	resp, err := http.Get(e.url + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s status = %d，期望 %d", path, resp.StatusCode, wantStatus)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func (e *testEnv) patchJSON(t *testing.T, path string, body any, wantStatus int, dst any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPatch, e.url+path, bytesReader(raw))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("PATCH %s status = %d，期望 %d", path, resp.StatusCode, wantStatus)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func (e *testEnv) do(t *testing.T, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.url+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}
