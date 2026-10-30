package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"agent_rebuild/internal/customtools"
	"agent_rebuild/internal/tool"
	"agent_rebuild/internal/tool/mcp"
	"agent_rebuild/internal/tool/router"
)

// fakeExtTool 模拟一个运行时才出现的外部（MCP）工具。
type fakeExtTool struct{ name string }

func (f fakeExtTool) Spec() tool.Spec {
	return tool.Spec{Name: f.name, Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Risk: tool.RiskDangerous}
}

func (f fakeExtTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Output: f.name}, nil
}

// 对话进行中反复整体替换工具集，不应产生数据竞争（需 go test -race 才有意义）。
// 这是 docs/17 第 2 节修复的回归测试：改动前 Executor.registry / Server.tools 是无锁读。
func TestReplaceToolsConcurrentWithRuns(t *testing.T) {
	env := newTestEnv(t)
	builtin := tool.DefaultRegistry(tool.Options{Workspace: t.TempDir()})
	env.server.SetTools(builtin, "", 5, 5*time.Second)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				env.server.ReplaceTools(builtin.With(fakeExtTool{"mcp__x__a"}))
			} else {
				env.server.ReplaceTools(builtin)
			}
			_ = env.server.Tools().Len()
		}
	}()

	for i := 0; i < 5; i++ {
		sess := env.createSession(t, "")
		var out sendMessageResponse
		env.postJSON(t, "/api/sessions/"+sess.ID+"/messages",
			map[string]any{"content": "hi", "stream": false}, http.StatusOK, &out)
		var tools map[string]any
		env.getJSON(t, "/api/tools", http.StatusOK, &tools)
	}
	close(stop)
	wg.Wait()
}

// 工具系统未启用时，ReplaceTools 不应凭空启用它（否则会出现「有工具没审批器」）。
func TestReplaceToolsIgnoredWithoutSetTools(t *testing.T) {
	env := newTestEnv(t)
	env.server.ReplaceTools(tool.NewRegistry())
	if env.server.Tools() != nil {
		t.Fatal("未 SetTools 时 ReplaceTools 不应生效")
	}
}

func TestMCPServersEndpointDisabled(t *testing.T) {
	env := newTestEnv(t)
	var out mcpServersResponse
	env.getJSON(t, "/api/mcp/servers", http.StatusOK, &out)
	if out.Enabled || out.Servers == nil || len(out.Servers) != 0 {
		t.Fatalf("未启用时应返回 enabled=false 与空数组：%+v", out)
	}
	resp := env.do(t, http.MethodPost, "/api/mcp/servers/x/restart")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("未启用时重启应 409，实际 %d", resp.StatusCode)
	}
}

func TestMCPServersEndpointListsAndRestarts(t *testing.T) {
	env := newTestEnv(t)
	off := mcp.ServerConfig{Name: "off", Command: "x", Disabled: true}
	m := mcp.NewManager(mcp.Options{Servers: []mcp.ServerConfig{off}, ConfigPaths: []string{"/data/mcp.json"}})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	env.server.SetMCP(m, nil)

	var out mcpServersResponse
	env.getJSON(t, "/api/mcp/servers", http.StatusOK, &out)
	if !out.Enabled || len(out.Servers) != 1 || out.Servers[0].Status != mcp.StatusDisabled ||
		len(out.ConfigPaths) != 1 || out.Servers[0].Tools == nil || out.OverridesSupported {
		t.Fatalf("状态不符：%+v", out)
	}

	for path, want := range map[string]int{
		"/api/mcp/servers/off/restart":  http.StatusConflict,
		"/api/mcp/servers/nope/restart": http.StatusNotFound,
	} {
		resp := env.do(t, http.MethodPost, path)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("POST %s = %d，期望 %d", path, resp.StatusCode, want)
		}
	}
}

// fakeCatalog 是只有一个工具的外部目录。
type fakeCatalog struct{}

func (fakeCatalog) Catalog() []mcp.CatalogServer {
	return []mcp.CatalogServer{{Name: "gh", Available: true, Tools: []mcp.CatalogTool{{
		Server: "gh", Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		BaseRisk: tool.RiskDangerous, BaseEnabled: true,
	}}}}
}

// overrideStore 是内存版覆盖存储（API 测试不依赖 sqlite）。
type overrideStore struct {
	mu sync.Mutex
	m  map[string]customtools.Override
}

func (s *overrideStore) List(context.Context) (map[string]customtools.Override, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]customtools.Override{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func (s *overrideStore) Set(_ context.Context, name string, o customtools.Override) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.IsEmpty() {
		delete(s.m, name)
	} else {
		s.m[name] = o
	}
	return nil
}

func putJSON(t *testing.T, env *testEnv, path string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, env.url+path, bytesReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	return resp
}

// PUT /api/tool-overrides：改设置后 search_router 立即按新风险定级，面板数据带上覆盖与生效值。
func TestSetToolOverrideRebuildsRouter(t *testing.T) {
	env := newTestEnv(t)
	builtin := tool.DefaultRegistry(tool.Options{Workspace: t.TempDir()})
	env.server.SetTools(builtin, "", 5, 5*time.Second)
	m := mcp.NewManager(mcp.Options{})
	ct := customtools.New(customtools.Options{
		Builtin: builtin, Source: fakeCatalog{},
		Store: &overrideStore{m: map[string]customtools.Override{}}, Apply: env.server.ReplaceTools,
	})
	ct.Rebuild()
	env.server.SetMCP(m, ct)

	riskOf := func() tool.Risk {
		tl, ok := env.server.Tools().Get(router.Name)
		if !ok {
			t.Fatal("search_router 未注册")
		}
		_, r := tl.(tool.CallResolver).ResolveCall(json.RawMessage(`{"action":"execute","tool":"gh/search"}`))
		return r
	}
	if riskOf() != tool.RiskDangerous {
		t.Fatal("初始应为基础风险 dangerous")
	}

	resp := putJSON(t, env, "/api/tool-overrides", map[string]any{"tool": "gh/search", "risk": "safe"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
	var out mcpServersResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OverridesSupported {
		t.Fatal("有 Store 时应支持保存")
	}
	if riskOf() != tool.RiskSafe {
		t.Fatal("PUT 之后应立即按新风险定级")
	}

	for _, body := range []string{
		`{"tool":"noslash","risk":"safe"}`,
		`{"tool":"gh/search","risk":"low"}`,
	} {
		want := http.StatusBadRequest
		req, _ := http.NewRequest(http.MethodPut, env.url+"/api/tool-overrides", bytesReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("PUT %s = %d，期望 %d", body, r.StatusCode, want)
		}
	}
}

func TestSetToolOverrideWithoutMCP(t *testing.T) {
	env := newTestEnv(t)
	resp := putJSON(t, env, "/api/tool-overrides", map[string]any{"tool": "gh/search", "risk": "safe"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("MCP 未启用时应 409，实际 %d", resp.StatusCode)
	}
}
