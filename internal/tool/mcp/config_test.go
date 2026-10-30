package mcp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent_rebuild/internal/tool"
)

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadFilesParsesAndExpands(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "mcp.json", `{
	  "mcpServers": {
	    "gh": {
	      "command": "npx",
	      "args": ["-y", "pkg", "--price=$5", "${MISSING_OK:-dflt}"],
	      "env": {"TOKEN": "${GH_TOKEN}"},
	      "trust_annotations": true,
	      "tool_risk": {"search": "safe", "*": "moderate"},
	      "startup_timeout_sec": 5,
	      "someOtherClientField": 123
	    }
	  }
	}`)
	env := fakeEnv(map[string]string{
		"GH_TOKEN":       "secret",
		"PATH":           "/usr/bin",
		"OPENAI_API_KEY": "must-not-leak",
	})
	servers, warns := LoadFiles([]string{p, filepath.Join(dir, "missing.json")}, env)
	if len(warns) != 0 {
		t.Fatalf("不存在的文件不应告警：%v", warns)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %+v", servers)
	}
	sc := servers[0]
	if sc.Err != nil {
		t.Fatalf("配置应有效：%v", sc.Err)
	}
	// 裸 $5 不展开，${VAR:-default} 用默认值。
	if !slices.Equal(sc.Args, []string{"-y", "pkg", "--price=$5", "dflt"}) {
		t.Fatalf("args = %v", sc.Args)
	}
	if !slices.Contains(sc.Env, "TOKEN=secret") || !slices.Contains(sc.Env, "PATH=/usr/bin") {
		t.Fatalf("env = %v", sc.Env)
	}
	for _, kv := range sc.Env {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Fatal("白名单外的环境变量泄露给了 server")
		}
	}
	if !sc.TrustAnnotations || sc.ToolRisk["search"] != tool.RiskSafe || sc.ToolRisk["*"] != tool.RiskModerate {
		t.Fatalf("扩展字段解析错误：%+v", sc)
	}
	if sc.StartupTimeout.Seconds() != 5 {
		t.Fatalf("startup_timeout = %v", sc.StartupTimeout)
	}
	if sc.Source != p {
		t.Fatalf("source = %q", sc.Source)
	}
}

func TestLoadFilesPerServerErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "mcp.json", `{"mcpServers": {
	  "ok":       {"command": "x"},
	  "nocmd":    {"args": ["a"]},
	  "a.b":      {"command": "x"},
	  "needvar":  {"command": "x", "env": {"K": "${NOT_SET}"}},
	  "remote":   {"url": "https://example.com/mcp"},
	  "badrisk":  {"command": "x", "tool_risk": {"t": "low"}},
	  "badtype":  {"type": "websocket", "command": "x"}
	}}`)
	servers, warns := LoadFiles([]string{p}, fakeEnv(nil))
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	errs := map[string]string{}
	for _, sc := range servers {
		if sc.Err != nil {
			errs[sc.Name] = sc.Err.Error()
		} else {
			errs[sc.Name] = ""
		}
	}
	want := map[string]string{
		"ok":      "",
		"nocmd":   "缺少 command",
		"a.b":     "不合法",
		"needvar": "NOT_SET",
		"remote":  "HTTP transport",
		"badrisk": "不合法的风险等级",
		"badtype": "未知的 transport",
	}
	for name, sub := range want {
		got, ok := errs[name]
		if !ok {
			t.Fatalf("缺少 server %q", name)
		}
		if sub == "" && got != "" || sub != "" && !strings.Contains(got, sub) {
			t.Fatalf("%s: err = %q，期望包含 %q", name, got, sub)
		}
	}
	// 结果按名字排序（工具顺序稳定）。
	names := make([]string, 0, len(servers))
	for _, sc := range servers {
		names = append(names, sc.Name)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("未排序：%v", names)
	}
}

func TestLoadFilesLaterOverridesAndBadJSON(t *testing.T) {
	dir := t.TempDir()
	user := writeConfig(t, dir, "user.json", `{"mcpServers": {"s": {"command": "user-cmd"}, "u": {"command": "u"}}}`)
	broken := writeConfig(t, dir, "broken.json", `{not json`)
	proj := writeConfig(t, dir, "proj.json", `{"mcpServers": {"s": {"command": "proj-cmd"}}}`)

	servers, warns := LoadFiles([]string{user, broken, proj}, fakeEnv(nil))
	if len(warns) != 1 || !strings.Contains(warns[0].Error(), "broken.json") {
		t.Fatalf("应只对非法 JSON 告警一次：%v", warns)
	}
	if len(servers) != 2 || servers[0].Name != "s" || servers[0].Command != "proj-cmd" {
		t.Fatalf("后读的应覆盖先读的：%+v", servers)
	}
}

func TestDefaultConfigPaths(t *testing.T) {
	got := DefaultConfigPaths(nil, "/data", "/ws", false)
	if !slices.Equal(got, []string{filepath.Join("/data", "mcp.json")}) {
		t.Fatalf("默认不应读项目级配置：%v", got)
	}
	got = DefaultConfigPaths(nil, "/data", "/ws", true)
	if len(got) != 2 || got[1] != filepath.Join("/ws", ".mcp.json") {
		t.Fatalf("开启后应追加项目级配置：%v", got)
	}
	abs, _ := filepath.Abs("x.json")
	got = DefaultConfigPaths([]string{"x.json"}, "/data", "/ws", true)
	if !slices.Equal(got, []string{abs}) {
		t.Fatalf("显式指定时只读指定文件：%v", got)
	}
}
