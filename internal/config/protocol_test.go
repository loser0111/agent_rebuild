package config

import (
	"errors"
	"strings"
	"testing"
)

// 清空协议相关环境变量。
func clearProtoEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LLM_PROTOCOL", "LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

// 不写 LLM_PROTOCOL 时，应按 provider 预设推断（向后兼容）。
func TestProtocolInferredFromProvider(t *testing.T) {
	cases := map[string]string{
		"mock":      ProtocolMock,
		"openai":    ProtocolOpenAI,
		"deepseek":  ProtocolOpenAI,
		"ollama":    ProtocolOpenAI,
		"anthropic": ProtocolAnthropic,
	}
	for provider, wantProto := range cases {
		t.Run(provider, func(t *testing.T) {
			clearProtoEnv(t)
			t.Setenv("LLM_PROVIDER", provider)
			if provider != "mock" && wantProto != ProtocolAnthropic {
				t.Setenv("OPENAI_API_KEY", "sk-test")
			}
			if wantProto == ProtocolAnthropic {
				t.Setenv("OPENAI_API_KEY", "sk-ant-test")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(%s): %v", provider, err)
			}
			if cfg.LLMProtocol != wantProto {
				t.Fatalf("Protocol = %q，期望 %q", cfg.LLMProtocol, wantProto)
			}
		})
	}
}

// 显式指定 LLM_PROTOCOL 时以它为准（可覆盖预设）。
func TestExplicitProtocolOverridesInference(t *testing.T) {
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_PROTOCOL", "anthropic") // 用 openai 前缀 + anthropic 协议
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("LLM_BASE_URL", "https://my-anthropic-proxy/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMProtocol != ProtocolAnthropic {
		t.Fatalf("Protocol = %q，显式值应优先", cfg.LLMProtocol)
	}
}

func TestProtocolAliases(t *testing.T) {
	cases := map[string]string{
		"openai":        ProtocolOpenAI,
		"OpenAI":        ProtocolOpenAI,
		"openai-compat": ProtocolOpenAI,
		"anthropic":     ProtocolAnthropic,
		"claude":        ProtocolAnthropic,
		"Anthropic-API": ProtocolAnthropic,
		"messages":      ProtocolAnthropic,
		"mock":          ProtocolMock,
		"  OpenAI  ":    ProtocolOpenAI,
	}
	for input, want := range cases {
		got, err := NormalizeProtocol(input)
		if err != nil {
			t.Fatalf("NormalizeProtocol(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeProtocol(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestEmptyProtocolMeansInfer(t *testing.T) {
	got, err := NormalizeProtocol("")
	if err != nil {
		t.Fatalf("空协议不应报错: %v", err)
	}
	if got != "" {
		t.Fatalf("空协议应返回空字符串（表示推断），得到 %q", got)
	}
}

func TestUnknownProtocol(t *testing.T) {
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_PROTOCOL", "not-a-protocol")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	_, err := Load()
	if err == nil {
		t.Fatal("未知协议应报错")
	}
	if !errors.Is(err, ErrInvalidProtocol) {
		t.Fatalf("err = %v，期望包裹 ErrInvalidProtocol", err)
	}
	msg := err.Error()
	for _, want := range []string{"openai", "anthropic", "mock"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应列出协议 %q，实际: %s", want, msg)
		}
	}
}

// anthropic 预设应正确带出 BaseURL 与默认模型。
func TestAnthropicPreset(t *testing.T) {
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-ant-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMProtocol != ProtocolAnthropic {
		t.Fatalf("Protocol = %q", cfg.LLMProtocol)
	}
	if cfg.LLMBaseURL != "https://api.anthropic.com/v1" {
		t.Fatalf("BaseURL = %q", cfg.LLMBaseURL)
	}
	if cfg.LLMModel != "claude-3-5-sonnet-latest" {
		t.Fatalf("Model = %q", cfg.LLMModel)
	}
}

// claude 是 anthropic 的别名。
func TestClaudeAlias(t *testing.T) {
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "claude")
	t.Setenv("OPENAI_API_KEY", "sk-ant-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMProvider != "anthropic" {
		t.Fatalf("Provider = %q，claude 应归一化为 anthropic", cfg.LLMProvider)
	}
}

// 协议与厂商的自洽性校验。
func TestProtocolProviderMismatch(t *testing.T) {
	// mock 协议配非 mock 厂商 → 报错（避免「以为在调真模型其实是假数据」）
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_PROTOCOL", "mock")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if _, err := Load(); err == nil {
		t.Fatal("LLM_PROTOCOL=mock + provider=openai 应报错")
	}

	// mock 厂商配真协议 → 报错
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LLM_PROTOCOL", "openai")
	if _, err := Load(); err == nil {
		t.Fatal("provider=mock + protocol=openai 应报错")
	}
}

// 用 openai 协议接第三方 Anthropic 兼容网关应当允许（只改 BaseURL）。
func TestOpenAIProtocolWithCustomBaseURL(t *testing.T) {
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("LLM_BASE_URL", "https://my-gateway/v1")
	t.Setenv("LLM_MODEL", "my-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMProtocol != ProtocolOpenAI {
		t.Fatalf("Protocol = %q", cfg.LLMProtocol)
	}
	if cfg.LLMBaseURL != "https://my-gateway/v1" {
		t.Fatalf("BaseURL = %q", cfg.LLMBaseURL)
	}
}

func TestProtocolNames(t *testing.T) {
	names := ProtocolNames()
	if len(names) != 3 {
		t.Fatalf("协议数量 = %d，期望 3（mock/openai/anthropic）", len(names))
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"mock", "openai", "anthropic"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少协议 %q，实际: %s", want, joined)
		}
	}
}

// DisplayName 应同时体现厂商与协议，避免语义含糊。
func TestDisplayNameShowsProtocol(t *testing.T) {
	clearProtoEnv(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-ant-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dn := cfg.DisplayName()
	if !strings.Contains(dn, "anthropic") {
		t.Fatalf("DisplayName 应含 provider/protocol: %q", dn)
	}
	if !strings.Contains(dn, "claude") {
		t.Fatalf("DisplayName 应含模型名: %q", dn)
	}
}
