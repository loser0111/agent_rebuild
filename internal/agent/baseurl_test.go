package agent

import "testing"

// TestNormalizeBaseURL 覆盖用户可能填写的各种接口地址写法。
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// 只填域名：应自动补 /v1（这是 404 问题的核心场景）
		{"纯域名", "https://api.openai.com", "https://api.openai.com/v1"},
		{"带路径前缀无版本", "https://ada-cli-go.ctripcorp.com/coding-plan/ccdesktop",
			"https://ada-cli-go.ctripcorp.com/coding-plan/ccdesktop/v1"},

		// 已含版本段：不应重复添加
		{"官方写法", "https://api.anthropic.com/v1", "https://api.anthropic.com/v1"},
		{"已含版本带尾斜杠", "https://api.deepseek.com/v1/", "https://api.deepseek.com/v1"},
		{"v1beta", "https://example.com/v1beta", "https://example.com/v1beta"},
		{"v4", "https://example.com/api/v4", "https://example.com/api/v4"},

		// 尾部多余斜杠
		{"纯域名带尾斜杠", "https://api.openai.com/", "https://api.openai.com/v1"},

		// 空白处理
		{"前后空白", "  https://api.openai.com/v1  ", "https://api.openai.com/v1"},

		// 空串不臆造地址
		{"空串", "", ""},
		{"纯空白", "   ", ""},

		// 本地地址
		{"本地端口", "http://localhost:11434", "http://localhost:11434/v1"},

		// 版本号看起来像但不是（路径最后一段是数字字母混合的模型名）
		{"末段为普通词", "https://example.com/api", "https://example.com/api/v1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeBaseURL(c.in)
			if got != c.want {
				t.Fatalf("normalizeBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// TestLooksLikeVersionSegment 验证版本段识别边界。
func TestLooksLikeVersionSegment(t *testing.T) {
	yes := []string{"v1", "v2", "v10", "v1beta", "V1", "v3alpha"}
	no := []string{"", "v", "1", "chat", "ccdesktop", "coding-plan", "vx", "v1-", "v1beta2-"}

	for _, s := range yes {
		if !looksLikeVersionSegment(s) {
			t.Errorf("looksLikeVersionSegment(%q) 应为 true", s)
		}
	}
	for _, s := range no {
		if looksLikeVersionSegment(s) {
			t.Errorf("looksLikeVersionSegment(%q) 应为 false", s)
		}
	}
}

// TestEndpointURL 验证完整端点拼接。
func TestEndpointURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{"openai 官方", "https://api.openai.com/v1", "chat/completions",
			"https://api.openai.com/v1/chat/completions"},
		{"openai 只填域名", "https://api.openai.com", "chat/completions",
			"https://api.openai.com/v1/chat/completions"},
		{"anthropic 官方", "https://api.anthropic.com/v1", "messages",
			"https://api.anthropic.com/v1/messages"},
		{"anthropic 网关无版本", "https://ada-cli-go.ctripcorp.com/coding-plan/ccdesktop",
			"messages", "https://ada-cli-go.ctripcorp.com/coding-plan/ccdesktop/v1/messages"},
		{"path 带前导斜杠", "https://api.openai.com/v1", "/chat/completions",
			"https://api.openai.com/v1/chat/completions"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := endpointURL(c.base, c.path)
			if got != c.want {
				t.Fatalf("endpointURL(%q, %q) = %q，期望 %q", c.base, c.path, got, c.want)
			}
		})
	}
}
