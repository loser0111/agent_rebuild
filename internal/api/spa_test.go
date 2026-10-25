package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWebDir 造一个最小的前端产物目录。
func newWebDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "<html><body>APP SHELL</body></html>")
	mustWrite(t, filepath.Join(dir, "assets", "app-abc123.js"), "console.log(1)")
	mustWrite(t, filepath.Join(dir, "favicon.svg"), "<svg/>")
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func getBody(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n]), resp.Header
}

func TestServeWebIndex(t *testing.T) {
	env := newTestEnvWithWeb(t, newWebDir(t))

	code, body, _ := getBody(t, env.url+"/")
	if code != http.StatusOK {
		t.Fatalf("GET / status = %d，期望 200", code)
	}
	if !strings.Contains(body, "APP SHELL") {
		t.Fatalf("未返回 index.html: %q", body)
	}
}

// 前端路由（如 /sessions/xxx）应回退到 index.html。
func TestServeWebSPAFallback(t *testing.T) {
	env := newTestEnvWithWeb(t, newWebDir(t))

	for _, p := range []string{"/sessions/abc", "/settings", "/deep/nested/route"} {
		code, body, _ := getBody(t, env.url+p)
		if code != http.StatusOK {
			t.Fatalf("GET %s status = %d，期望 200（SPA 回退）", p, code)
		}
		if !strings.Contains(body, "APP SHELL") {
			t.Fatalf("GET %s 未回退到 index.html: %q", p, body)
		}
	}
}

// 静态资源直接命中，且 assets 下带长缓存头。
func TestServeWebAssets(t *testing.T) {
	env := newTestEnvWithWeb(t, newWebDir(t))

	code, body, header := getBody(t, env.url+"/assets/app-abc123.js")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "console.log") {
		t.Fatalf("资源内容错误: %q", body)
	}
	if cc := header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("assets 应长缓存，得到 Cache-Control = %q", cc)
	}

	// index.html 必须禁缓存，保证发版即时生效。
	_, _, idxHeader := getBody(t, env.url+"/")
	if cc := idxHeader.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("index.html 应禁缓存，得到 Cache-Control = %q", cc)
	}
}

// 缺失的静态资源应 404，不能伪装成 HTML（否则前端会拿到 index.html 当 JS 执行）。
func TestServeWebMissingAssetIs404(t *testing.T) {
	env := newTestEnvWithWeb(t, newWebDir(t))

	code, body, _ := getBody(t, env.url+"/assets/missing-xyz.js")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（避免把 index.html 当 JS 返回）", code)
	}
	if strings.Contains(body, "APP SHELL") {
		t.Fatal("缺失资源不应回退到 index.html")
	}
}

// /api 下未匹配路径必须仍是 JSON 404，不能被 SPA 回退吞掉。
func TestServeWebAPINotFoundStaysJSON(t *testing.T) {
	env := newTestEnvWithWeb(t, newWebDir(t))

	code, body, header := getBody(t, env.url+"/api/nope")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", code)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q，期望 JSON", header.Get("Content-Type"))
	}

	var errBody ErrorBody
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("响应不是 JSON: %q", body)
	}
	if errBody.Error.Code != string(CodeNotFound) {
		t.Fatalf("error.code = %q", errBody.Error.Code)
	}
}

// 路径穿越（../）不得读到 dist 之外的文件。
func TestServeWebPreventsTraversal(t *testing.T) {
	dir := newWebDir(t)
	// 在 dist 的父目录放一个机密文件。
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	mustWrite(t, secret, "TOP SECRET")
	t.Cleanup(func() { _ = os.Remove(secret) })

	env := newTestEnvWithWeb(t, dir)

	req, _ := http.NewRequest(http.MethodGet, env.url+"/", nil)
	// 用原始路径尝试穿越（Go 客户端会规范化 URL，因此手工设置 RequestURI）。
	req.URL.Path = "/../secret.txt"
	req.URL.RawPath = "/../secret.txt"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if strings.Contains(string(buf[:n]), "TOP SECRET") {
		t.Fatal("路径穿越成功读取到 dist 之外的文件")
	}
}

// 目录不存在时返回 503 + 构建提示，而不是静默 404。
func TestServeWebMissingDir(t *testing.T) {
	env := newTestEnvWithWeb(t, filepath.Join(t.TempDir(), "not-built"))

	code, body, _ := getBody(t, env.url+"/")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503", code)
	}
	if !strings.Contains(body, "make build-web") {
		t.Fatalf("应提示如何构建前端: %q", body)
	}
}

// 未启用静态托管时，非 /api 路径仍返回 JSON 404（保持原有行为）。
func TestServeWebDisabled(t *testing.T) {
	env := newTestEnv(t)
	code, body, header := getBody(t, env.url+"/")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", code)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", header.Get("Content-Type"))
	}
	if !strings.Contains(body, "not_found") {
		t.Fatalf("body = %q", body)
	}
}

// 静态资源不接受写方法。
func TestServeWebRejectsWriteMethods(t *testing.T) {
	env := newTestEnvWithWeb(t, newWebDir(t))

	req, _ := http.NewRequest(http.MethodPost, env.url+"/index.html", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d，期望 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}
}
