package api

import (
	"bytes"
	"net/http"
	"testing"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func TestHealth(t *testing.T) {
	env := newTestEnv(t)
	var body map[string]any
	env.getJSON(t, "/api/health", http.StatusOK, &body)

	if body["status"] != "ok" {
		t.Fatalf("status 字段 = %v", body["status"])
	}
	if body["provider"] != "mock" {
		t.Fatalf("provider = %v", body["provider"])
	}
}

func TestHealthHeaders(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodGet, "/api/health")
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-Id"); got == "" {
		t.Fatal("缺少 X-Request-Id 响应头")
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestNotFoundIsJSON(t *testing.T) {
	env := newTestEnv(t)
	var body ErrorBody
	env.getJSON(t, "/api/not-exist", http.StatusNotFound, &body)

	if body.Error.Code != string(CodeNotFound) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	env := newTestEnv(t)

	req, _ := http.NewRequest(http.MethodOptions, env.url+"/api/sessions", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("Allow-Origin = %q", got)
	}
}

// 白名单之外的来源不应被回写 CORS 头。
func TestCORSRejectsUnknownOrigin(t *testing.T) {
	env := newTestEnv(t)

	req, _ := http.NewRequest(http.MethodOptions, env.url+"/api/sessions", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("不应放行未知来源，得到 Allow-Origin = %q", got)
	}
}

// 保证 context 里的 requestID 能被中间件写入响应。
func TestRequestIDPropagated(t *testing.T) {
	env := newTestEnv(t)

	req, _ := http.NewRequest(http.MethodGet, env.url+"/api/health", nil)
	req.Header.Set("X-Request-Id", "req_fixed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-Id"); got != "req_fixed" {
		t.Fatalf("X-Request-Id = %q，期望沿用客户端值", got)
	}

	// 未传时应由服务端生成。
	resp2, err := http.Get(env.url + "/api/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("X-Request-Id"); got == "" || got == "req_fixed" {
		t.Fatalf("应生成新的 X-Request-Id，得到 %q", got)
	}
}
