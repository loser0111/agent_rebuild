package config

import (
	"fmt"
	"strings"
)

// Protocol 表示服务端使用的 API 协议族。
//
// 之所以把「协议」与「厂商」分开建模：
//   - 同一个协议可以对接不同厂商（OpenAI 兼容市场极大，DeepSeek/Ollama/vLLM 都是）
//   - 同一个厂商也可能提供多种协议（现在少见，但未来会变）
//   - 只用一个字段会导致「LLM_PROVIDER=openai 但实际连 Ollama」这类语义混乱
const (
	// ProtocolOpenAI 是 OpenAI 兼容协议：POST /chat/completions。
	ProtocolOpenAI = "openai"
	// ProtocolAnthropic 是 Anthropic Messages API：POST /messages。
	ProtocolAnthropic = "anthropic"
	// ProtocolMock 是本地假执行器，不发起网络请求。
	ProtocolMock = "mock"
)

// ProviderSpec 描述一个服务商的预设。
//
// BaseURL / DefaultModel 让用户少写配置；Protocol 决定用哪个 Runner。
type ProviderSpec struct {
	// Name 是规范化后的 provider 名（小写）。
	Name string
	// Protocol 是该服务商使用的 API 协议族。
	Protocol string
	// BaseURL 是端点前缀（含 /v1 之类的版本号）。
	BaseURL string
	// DefaultModel 是该服务商推荐的默认模型。
	DefaultModel string
	// RequiresKey 表示是否必须提供 API Key。
	RequiresKey bool
	// Hint 用于错误信息与日志。
	Hint string
}

// providers 是内置别名表。
//
// 新增条目时必须明确 Protocol：只有实现了对应 Runner 的协议才能出现在这里，
// 否则配置能通过但请求必然失败。
var providers = map[string]ProviderSpec{
	// ---- 官方 / 云端 ----
	"openai": {
		Name:         "openai",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://api.openai.com/v1",
		DefaultModel: "gpt-4o-mini",
		RequiresKey:  true,
	},
	"anthropic": {
		Name:         "anthropic",
		Protocol:     ProtocolAnthropic,
		BaseURL:      "https://api.anthropic.com/v1",
		DefaultModel: "claude-3-5-sonnet-latest",
		RequiresKey:  true,
		Hint:         "Anthropic Messages API（非 OpenAI 兼容），使用 x-api-key 鉴权",
	},
	"deepseek": {
		Name:         "deepseek",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://api.deepseek.com/v1",
		DefaultModel: "deepseek-chat",
		RequiresKey:  true,
		Hint:         "推理模型可用 deepseek-reasoner",
	},
	"moonshot": {
		Name:         "moonshot",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://api.moonshot.cn/v1",
		DefaultModel: "moonshot-v1-8k",
		RequiresKey:  true,
		Hint:         "Kimi 开放平台",
	},
	"zhipu": {
		Name:         "zhipu",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://open.bigmodel.cn/api/paas/v4",
		DefaultModel: "glm-4-flash",
		RequiresKey:  true,
		Hint:         "智谱 GLM，注意版本前缀是 /v4",
	},
	"qwen": {
		Name:         "qwen",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://dashscope.aliyuncs.com/compatible-mode/v1",
		DefaultModel: "qwen-plus",
		RequiresKey:  true,
		Hint:         "阿里云百炼 DashScope 的 OpenAI 兼容模式",
	},
	"siliconflow": {
		Name:         "siliconflow",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://api.siliconflow.cn/v1",
		DefaultModel: "Qwen/Qwen2.5-7B-Instruct",
		RequiresKey:  true,
	},
	"openrouter": {
		Name:         "openrouter",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://openrouter.ai/api/v1",
		DefaultModel: "openai/gpt-4o-mini",
		RequiresKey:  true,
	},
	"groq": {
		Name:         "groq",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://api.groq.com/openai/v1",
		DefaultModel: "llama-3.3-70b-versatile",
		RequiresKey:  true,
	},

	// ---- 本地部署（无需真实 Key，但多数客户端要求非空，填任意占位值）----
	"ollama": {
		Name:         "ollama",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "http://localhost:11434/v1",
		DefaultModel: "qwen2.5:7b",
		RequiresKey:  false,
		Hint:         "本地部署：需先 ollama serve，OPENAI_API_KEY 填任意值（如 ollama）",
	},
	"vllm": {
		Name:         "vllm",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "http://localhost:8000/v1",
		DefaultModel: "",
		RequiresKey:  false,
		Hint:         "本地部署：LLM_MODEL 必须显式指定为 vLLM 加载的模型名",
	},
	"lmstudio": {
		Name:         "lmstudio",
		Protocol:     ProtocolOpenAI,
		BaseURL:      "http://localhost:1234/v1",
		DefaultModel: "",
		RequiresKey:  false,
		Hint:         "本地部署：LLM_MODEL 需填写 LM Studio 中已加载的模型",
	},

	// ---- 特殊值 ----
	"mock": {
		Name:         "mock",
		Protocol:     ProtocolMock,
		BaseURL:      "",
		DefaultModel: "mock-echo",
		RequiresKey:  false,
		Hint:         "本地演示模式，不调用任何外部服务",
	},
}

// aliases 把常见拼写/旧称映射到规范名。
var aliases = map[string]string{
	"open-ai":       "openai",
	"azure":         "openai", // Azure 需自行填 LLM_BASE_URL
	"azure-openai":  "openai",
	"claude":        "anthropic",
	"anthropic-api": "anthropic",
	"kimi":          "moonshot",
	"glm":           "zhipu",
	"bigmodel":      "zhipu",
	"dashscope":     "qwen",
	"alibaba":       "qwen",
	"tongyi":        "qwen",
	"silicon":       "siliconflow",
	"silicon-flow":  "siliconflow",
	"lm-studio":     "lmstudio",
	"lm_studio":     "lmstudio",
	"local":         "ollama",
	"o-l-l-a-m-a":   "ollama",
	"":              "mock",
}

// protocolAliases 把协议名归一化（用户可能写 Anthropic / claude / open-ai）。
var protocolAliases = map[string]string{
	"openai":            ProtocolOpenAI,
	"open-ai":           ProtocolOpenAI,
	"openai-compat":     ProtocolOpenAI,
	"openai-compatible": ProtocolOpenAI,
	"anthropic":         ProtocolAnthropic,
	"anthropic-api":     ProtocolAnthropic,
	"claude":            ProtocolAnthropic,
	"messages":          ProtocolAnthropic,
	"mock":              ProtocolMock,
	"":                  "", // 空表示「由 provider 预设推断」
}

// NormalizeProtocol 归一化协议名。空字符串返回 ("", nil)，表示交给预设推断。
func NormalizeProtocol(raw string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	p, ok := protocolAliases[key]
	if !ok {
		return "", fmt.Errorf(
			"%w: 不支持的 LLM_PROTOCOL=%q\n可选值: %s\n（留空则按 LLM_PROVIDER 的预设自动推断）",
			ErrInvalidProtocol, raw, strings.Join(ProtocolNames(), ", "),
		)
	}
	return p, nil
}

// ProtocolNames 返回全部已实现的协议名。
func ProtocolNames() []string {
	return []string{ProtocolMock, ProtocolOpenAI, ProtocolAnthropic}
}

// NormalizeProvider 把用户输入规范化为内置 provider 名，并返回其预设。
//
// 容错：忽略大小写与首尾空白；支持别名；空值视为 mock。
// 未知值返回错误并列出全部可用项，避免用户对着「不支持的 provider」猜。
func NormalizeProvider(raw string) (ProviderSpec, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if canonical, ok := aliases[key]; ok {
		key = canonical
	}
	spec, ok := providers[key]
	if !ok {
		return ProviderSpec{}, fmt.Errorf(
			"%w: 不支持的 LLM_PROVIDER=%q\n可选值: %s\n也可以随便填一个名字并手动指定 LLM_BASE_URL",
			ErrInvalidProvider, raw, strings.Join(ProviderNames(), ", "),
		)
	}
	return spec, nil
}

// ProviderNames 返回全部内置 provider 名（排序稳定，便于文档与错误信息）。
func ProviderNames() []string {
	// 保持固定顺序，让错误提示可读性一致（不依赖 map 迭代顺序）。
	order := []string{
		"mock", "openai", "anthropic", "deepseek", "moonshot", "zhipu", "qwen",
		"siliconflow", "openrouter", "groq", "ollama", "vllm", "lmstudio",
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		if _, ok := providers[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// ProviderHint 返回该 provider 的使用提示（可能为空）。
func ProviderHint(name string) string {
	if spec, ok := providers[name]; ok {
		return spec.Hint
	}
	return ""
}
