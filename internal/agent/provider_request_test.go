package agent_test

// 用本地假网关验证：不同 provider 别名下，LLMRunner 实际发出的请求
// 路径、鉴权头、模型名是否正确——这是「配了别名连不上」类问题的关键验证。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/session"
)

// fakeGateway 记录收到的请求并返回一个最小可用的 SSE 流。
type fakeGateway struct {
	srv      *httptest.Server
	gotPath  string
	gotAuth  string
	gotModel string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.gotPath = r.URL.Path
		g.gotAuth = r.Header.Get("Authorization")

		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		g.gotModel = req.Model

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// 验证：各 provider 预设的 BaseURL/Model 会被如实用于请求。
func TestProviderPresetsDriveActualRequest(t *testing.T) {
	gw := newFakeGateway(t)

	cases := []struct {
		provider      string
		envBaseURL    string // 为空表示不覆盖，但需改指向假网关
		wantModel     string
		wantPath      string
		wantAuthValue string
	}{
		// 所有别名都走 /chat/completions，差别只在模型名与地址。
		// 注意：这里把 BaseURL 指向假网关的裸地址（不含 /v1），
		// 因此期望路径会带上自动补全的版本段。
		{provider: "deepseek", wantModel: "deepseek-chat", wantPath: "/v1/chat/completions"},
		{provider: "moonshot", wantModel: "moonshot-v1-8k", wantPath: "/v1/chat/completions"},
		{provider: "qwen", wantModel: "qwen-plus", wantPath: "/v1/chat/completions"},
		{provider: "zhipu", wantModel: "glm-4-flash", wantPath: "/v1/chat/completions"},
		{provider: "ollama", wantModel: "qwen2.5:7b", wantPath: "/v1/chat/completions"},
	}

	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			// 重置环境，加载该 provider 的预设。
			for _, k := range []string{"LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "OPENAI_API_KEY"} {
				t.Setenv(k, "")
			}
			t.Setenv("LLM_PROVIDER", c.provider)
			t.Setenv("OPENAI_API_KEY", "sk-test")

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load(%s): %v", c.provider, err)
			}
			if cfg.LLMModel != c.wantModel {
				t.Fatalf("预设模型 = %q，期望 %q", cfg.LLMModel, c.wantModel)
			}

			// 把 BaseURL 指向假网关（保留路径结构验证）。
			runner := agent.NewLLMRunner(gw.srv.URL, cfg.LLMAPIKey, cfg.LLMModel, "")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			history := []session.Message{
				{ID: "1", Role: session.RoleUser, Content: "hi", Status: session.StatusComplete},
			}
			var gotDone bool
			for ev := range runner.Run(ctx, agent.Request{History: history}) {
				if ev.Type == agent.TypeError {
					t.Fatalf("意外错误: %v", ev.Err)
				}
				if ev.Type == agent.TypeDone {
					gotDone = true
				}
			}
			if !gotDone {
				t.Fatal("未收到 done")
			}

			if gw.gotPath != c.wantPath {
				t.Fatalf("请求路径 = %q，期望 %q", gw.gotPath, c.wantPath)
			}
			if gw.gotModel != c.wantModel {
				t.Fatalf("请求中的 model = %q，期望 %q（预设未传到请求）", gw.gotModel, c.wantModel)
			}
			if gw.gotAuth == "" {
				t.Fatal("缺少 Authorization 头")
			}
		})
	}
}

// 验证：用户显式覆盖 LLM_BASE_URL / LLM_MODEL 时，请求使用用户值。
func TestExplicitOverrideDrivesRequest(t *testing.T) {
	gw := newFakeGateway(t)

	for _, k := range []string{"LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("LLM_PROVIDER", "deepseek")
	t.Setenv("LLM_BASE_URL", "https://custom.invalid/v1") // 会被替换为假网关
	t.Setenv("LLM_MODEL", "custom-model")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMBaseURL != "https://custom.invalid/v1" {
		t.Fatalf("BaseURL = %q，用户覆盖应生效", cfg.LLMBaseURL)
	}
	if cfg.LLMModel != "custom-model" {
		t.Fatalf("Model = %q，用户覆盖应生效", cfg.LLMModel)
	}

	runner := agent.NewLLMRunner(gw.srv.URL, cfg.LLMAPIKey, cfg.LLMModel, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range runner.Run(ctx, agent.Request{History: []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "hi", Status: session.StatusComplete},
	}}) {
	}

	if gw.gotModel != "custom-model" {
		t.Fatalf("请求 model = %q，期望 custom-model", gw.gotModel)
	}
}
