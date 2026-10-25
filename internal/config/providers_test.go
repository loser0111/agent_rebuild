package config

import (
	"errors"
	"strings"
	"testing"
)

// 清空全部 LLM 相关环境变量，确保用例之间不互相污染。
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "OPENAI_API_KEY", "SYSTEM_PROMPT",
	} {
		t.Setenv(k, "")
	}
}

func TestProviderAliases(t *testing.T) {
	cases := []struct {
		provider     string
		wantName     string
		wantBaseURL  string
		wantModel    string
		wantNeedsKey bool
	}{
		{"openai", "openai", "https://api.openai.com/v1", "gpt-4o-mini", true},
		{"deepseek", "deepseek", "https://api.deepseek.com/v1", "deepseek-chat", true},
		{"moonshot", "moonshot", "https://api.moonshot.cn/v1", "moonshot-v1-8k", true},
		{"zhipu", "zhipu", "https://open.bigmodel.cn/api/paas/v4", "glm-4-flash", true},
		{"qwen", "qwen", "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-plus", true},
		{"siliconflow", "siliconflow", "https://api.siliconflow.cn/v1", "Qwen/Qwen2.5-7B-Instruct", true},
		{"openrouter", "openrouter", "https://openrouter.ai/api/v1", "openai/gpt-4o-mini", true},
		{"groq", "groq", "https://api.groq.com/openai/v1", "llama-3.3-70b-versatile", true},
		{"ollama", "ollama", "http://localhost:11434/v1", "qwen2.5:7b", false},
		{"vllm", "vllm", "http://localhost:8000/v1", "", false},
		{"lmstudio", "lmstudio", "http://localhost:1234/v1", "", false},
	}

	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			clearLLMEnv(t)
			t.Setenv("LLM_PROVIDER", c.provider)
			if c.wantNeedsKey {
				t.Setenv("OPENAI_API_KEY", "sk-test")
			}
			if c.wantModel == "" {
				// 本地部署要求显式模型名。
				t.Setenv("LLM_MODEL", "my-local-model")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LLMProvider != c.wantName {
				t.Fatalf("Provider = %q，期望 %q", cfg.LLMProvider, c.wantName)
			}
			if cfg.LLMBaseURL != c.wantBaseURL {
				t.Fatalf("BaseURL = %q，期望 %q", cfg.LLMBaseURL, c.wantBaseURL)
			}
			wantModel := c.wantModel
			if wantModel == "" {
				wantModel = "my-local-model"
			}
			if cfg.LLMModel != wantModel {
				t.Fatalf("Model = %q，期望 %q", cfg.LLMModel, wantModel)
			}
		})
	}
}

func TestProviderCommonAliases(t *testing.T) {
	// 常见别名/拼写应归一化到规范名。
	cases := map[string]string{
		"kimi":       "moonshot",
		"glm":        "zhipu",
		"bigmodel":   "zhipu",
		"dashscope":  "qwen",
		"alibaba":    "qwen",
		"tongyi":     "qwen",
		"lm-studio":  "lmstudio",
		"local":      "ollama",
		"open-ai":    "openai",
		"azure":      "openai",
		"DeepSeek":   "deepseek",
		"OLLAMA":     "ollama",
		"  Moonshot": "moonshot",
	}
	for input, want := range cases {
		spec, err := NormalizeProvider(input)
		if err != nil {
			t.Fatalf("NormalizeProvider(%q): %v", input, err)
		}
		if spec.Name != want {
			t.Fatalf("NormalizeProvider(%q) = %q，期望 %q", input, spec.Name, want)
		}
	}
}

func TestProviderCaseInsensitive(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "  DeepSeek  ")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMProvider != "deepseek" {
		t.Fatalf("Provider = %q，期望归一化为小写 deepseek", cfg.LLMProvider)
	}
}

func TestUnknownProvider(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "not-a-real-provider")

	_, err := Load()
	if err == nil {
		t.Fatal("未知 provider 应报错")
	}
	if !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("err = %v，期望包裹 ErrInvalidProvider", err)
	}
	// 错误信息应列出可选值，避免用户靠猜。
	msg := err.Error()
	for _, name := range []string{"openai", "deepseek", "ollama"} {
		if !strings.Contains(msg, name) {
			t.Fatalf("错误信息应列出可选值 %q，实际: %s", name, msg)
		}
	}
}

func TestProviderRequiresKey(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "deepseek")
	// 未设置 Key

	_, err := Load()
	if err == nil {
		t.Fatal("云端 provider 缺 Key 应报错")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("错误信息应提示需要 OPENAI_API_KEY: %v", err)
	}
}

func TestLocalProviderWithoutKey(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "ollama")
	// 本地部署不要求 Key

	cfg, err := Load()
	if err != nil {
		t.Fatalf("本地 provider 不应强制要求 Key: %v", err)
	}
	// 但应自动填占位值，避免部分客户端拒绝空 Authorization。
	if cfg.LLMAPIKey == "" {
		t.Fatal("本地 provider 应填充占位 Key")
	}
}

func TestLocalProviderRequiresModel(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "vllm")
	// vllm 没有默认模型，必须显式指定

	_, err := Load()
	if err == nil {
		t.Fatal("无默认模型的 provider 缺 LLM_MODEL 应报错")
	}
	if !strings.Contains(err.Error(), "LLM_MODEL") {
		t.Fatalf("错误信息应提示需要 LLM_MODEL: %v", err)
	}
}

func TestExplicitBaseURLOverridesPreset(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "deepseek")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("LLM_BASE_URL", "http://my-gateway:8000/v1")
	t.Setenv("LLM_MODEL", "my-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMBaseURL != "http://my-gateway:8000/v1" {
		t.Fatalf("BaseURL = %q，用户显式值应优先于预设", cfg.LLMBaseURL)
	}
	if cfg.LLMModel != "my-model" {
		t.Fatalf("Model = %q，用户显式值应优先于预设", cfg.LLMModel)
	}
}

func TestMockProviderClearsBaseURL(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "mock")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMModel != "mock-echo" {
		t.Fatalf("Model = %q，mock 模式应为 mock-echo", cfg.LLMModel)
	}
	if cfg.LLMBaseURL != "" {
		t.Fatalf("BaseURL = %q，mock 模式应清空避免误导", cfg.LLMBaseURL)
	}
}

func TestProviderNamesStable(t *testing.T) {
	names := ProviderNames()
	if len(names) < 10 {
		t.Fatalf("内置 provider 数量 = %d，期望 >= 10", len(names))
	}
	// 顺序必须稳定（供错误信息与文档使用）。
	first := ProviderNames()
	second := ProviderNames()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("ProviderNames 顺序不稳定: %v vs %v", first, second)
		}
	}
	if names[0] != "mock" {
		t.Fatalf("首个应为 mock，实际 %q", names[0])
	}
}

func TestEmptyProviderDefaultsToMock(t *testing.T) {
	clearLLMEnv(t)
	// 未设置 LLM_PROVIDER

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMProvider != "mock" {
		t.Fatalf("Provider = %q，空值应回退 mock", cfg.LLMProvider)
	}
}

func TestProviderHint(t *testing.T) {
	if h := ProviderHint("ollama"); !strings.Contains(h, "ollama serve") {
		t.Fatalf("ollama 提示应包含启动命令，实际: %q", h)
	}
	if h := ProviderHint("nonexistent"); h != "" {
		t.Fatalf("未知 provider 的提示应为空，实际: %q", h)
	}
}

func TestDisplayName(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "mock")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(cfg.DisplayName(), "Mock") {
		t.Fatalf("mock 模式的 DisplayName 应含 Mock: %q", cfg.DisplayName())
	}

	clearLLMEnv(t)
	t.Setenv("LLM_PROVIDER", "deepseek")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dn := cfg2.DisplayName()
	if !strings.Contains(dn, "deepseek") || !strings.Contains(dn, "deepseek-chat") {
		t.Fatalf("DisplayName 应含 provider 与模型: %q", dn)
	}
}
