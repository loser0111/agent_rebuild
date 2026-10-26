package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidProvider 表示 LLM_PROVIDER 无法识别。
var ErrInvalidProvider = errors.New("配置错误")

// ErrInvalidProtocol 表示 LLM_PROTOCOL 无法识别。
var ErrInvalidProtocol = errors.New("配置错误")

// ApprovalMode 决定工具调用（写文件/执行命令等需审批操作）由谁、怎么批准。
//
// 三档参考 Claude Code / Codex CLI 的权限模式设计，但收敛成产品侧更直观的
// 三个选择（见 docs/12-approval-modes.md）：
type ApprovalMode string

const (
	// ApprovalManual 一切需要审批的操作都挂起等人工在前端点击批准/拒绝。
	// 最安全，也是未显式配置时的默认值——新功能默认选最保守的一档，
	// 这是 Claude Code（default 模式）与 Codex CLI（Suggest 模式）的共同做法。
	ApprovalManual ApprovalMode = "manual"
	// ApprovalModel 由一次独立的模型调用（judge）当场裁决，不挂起、无需人工介入。
	ApprovalModel ApprovalMode = "model"
	// ApprovalAuto 完全自动：按 ENABLE_COMMAND_TOOL 决定命令执行开关，
	// 其余一律放行，不做任何形式的审批。
	ApprovalAuto ApprovalMode = "auto"
)

// normalizeApprovalMode 校验并归一化 APPROVAL_MODE 的取值。
// 无法识别的值会被拒绝（快速失败），而不是静默回退到某个默认值——
// 审批模式配错是安全相关的问题，不该被"拼写错误也能跑起来"掩盖。
func normalizeApprovalMode(raw string) (ApprovalMode, error) {
	switch ApprovalMode(strings.ToLower(strings.TrimSpace(raw))) {
	case ApprovalManual:
		return ApprovalManual, nil
	case ApprovalModel:
		return ApprovalModel, nil
	case ApprovalAuto:
		return ApprovalAuto, nil
	default:
		return "", fmt.Errorf("APPROVAL_MODE=%q 无法识别，可选值：manual | model | auto", raw)
	}
}

// Config 汇总全部运行时配置，全部来自环境变量（12-factor）。
// 默认值保证「零配置即可启动 Demo」：LLM_PROVIDER=mock。
type Config struct {
	// HTTP
	Port        string        // PORT
	Env         string        // APP_ENV: dev | prod
	CORSOrigins []string      // CORS_ORIGINS，逗号分隔
	ReadTimeout time.Duration // HTTP_READ_TIMEOUT

	// 存储
	StoreDriver           string // STORE_DRIVER: memory | sqlite | redis
	MaxMessagesPerSession int    // MAX_MESSAGES_PER_SESSION

	// SSE
	SSEPingInterval time.Duration // SSE_PING_INTERVAL
	SSEBufferSize   int           // SSE_BUFFER_SIZE

	// 生成
	RunTimeout time.Duration // RUN_TIMEOUT
	LLMTimeout time.Duration // LLM_TIMEOUT

	// LLM
	LLMProtocol     string // LLM_PROTOCOL: openai|anthropic|mock（留空按 provider 推断）
	LLMProvider     string // LLM_PROVIDER: openai|deepseek|...（见 providers.go）
	LLMProviderHint string // 该 provider 的使用提示（运行时有值，来自预设）
	LLMBaseURL      string // LLM_BASE_URL
	LLMModel        string // LLM_MODEL
	LLMAPIKey       string // OPENAI_API_KEY
	SystemPrompt    string // SYSTEM_PROMPT

	// 观测
	LogLevel string // LOG_LEVEL

	// 前端静态托管（单端口部署）
	WebDir string // WEB_DIR，如 web/dist；为空则不托管

	// 数据目录（SQLite 数据库与配置文件所在位置）
	DataDir string // DATA_DIR；为空时按平台默认

	// 工具系统
	ToolWorkspace     string        // TOOL_WORKSPACE；工具可访问的根目录，默认当前目录
	EnableCommandTool bool          // ENABLE_COMMAND_TOOL；是否启用命令执行（危险，默认关）
	MaxToolSteps      int           // MAX_TOOL_STEPS；单次对话最多几轮工具调用
	ToolTimeout       time.Duration // TOOL_TIMEOUT；单个工具执行超时

	// 审批
	ApprovalMode  ApprovalMode  // APPROVAL_MODE：manual | model | auto，默认 manual
	JudgeTimeout  time.Duration // JUDGE_TIMEOUT；model 模式下单次裁决超时
	JudgeFailOpen bool          // JUDGE_FAIL_OPEN；model 模式下 judge 调用失败时是否放行，默认 false（拒绝）
}

// Load 读取环境变量并校验。校验失败立即返回错误（快速失败）。
func Load() (*Config, error) {
	c := &Config{
		Port:                  envString("PORT", "8080"),
		Env:                   envString("APP_ENV", "dev"),
		CORSOrigins:           envList("CORS_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173"),
		ReadTimeout:           envDuration("HTTP_READ_TIMEOUT", 30*time.Second),
		StoreDriver:           envString("STORE_DRIVER", "memory"),
		MaxMessagesPerSession: envInt("MAX_MESSAGES_PER_SESSION", 200),
		SSEPingInterval:       envDuration("SSE_PING_INTERVAL", 15*time.Second),
		SSEBufferSize:         envInt("SSE_BUFFER_SIZE", 1000),
		RunTimeout:            envDuration("RUN_TIMEOUT", 180*time.Second),
		LLMTimeout:            envDuration("LLM_TIMEOUT", 120*time.Second),
		LLMProvider:           strings.TrimSpace(envString("LLM_PROVIDER", "mock")),
		// BaseURL / Model 初始为空：完全由 provider 预设决定，
		// 用户显式指定时才覆盖（见下方 explicit* 判断）。
		LLMBaseURL:   strings.TrimSpace(os.Getenv("LLM_BASE_URL")),
		LLMModel:     strings.TrimSpace(os.Getenv("LLM_MODEL")),
		LLMAPIKey:    strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		SystemPrompt: defaultSystemPrompt(),
		LogLevel:     envString("LOG_LEVEL", "info"),
		WebDir:       strings.TrimSpace(os.Getenv("WEB_DIR")),
		DataDir:      strings.TrimSpace(os.Getenv("DATA_DIR")),

		ToolWorkspace:     envString("TOOL_WORKSPACE", ""),
		EnableCommandTool: envBool("ENABLE_COMMAND_TOOL", true), // 默认支持系统配置路径，
		MaxToolSteps:      envInt("MAX_TOOL_STEPS", 200),
		ToolTimeout:       envDuration("TOOL_TIMEOUT", 120*time.Second),

		JudgeTimeout:  envDuration("JUDGE_TIMEOUT", 10*time.Second),
		JudgeFailOpen: envBool("JUDGE_FAIL_OPEN", false),
	}

	// 审批模式单独解析校验：配错值要快速失败，而不是静默退到某个默认值
	// （审批模式是安全相关的开关，拼写错误不该被悄悄吞掉）。
	approvalMode, err := normalizeApprovalMode(envString("APPROVAL_MODE", string(ApprovalManual)))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.ApprovalMode = approvalMode

	// 解析 provider 别名，套用内置预设（BaseURL / 默认模型）。
	// 用户显式给出的 LLM_BASE_URL / LLM_MODEL 始终优先，便于接自定义网关。
	spec, err := NormalizeProvider(c.LLMProvider)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.LLMProvider = spec.Name
	c.LLMProviderHint = spec.Hint

	// 协议解析：显式指定优先；留空则按 provider 预设推断（向后兼容旧配置）。
	if explicitProtocol := strings.TrimSpace(os.Getenv("LLM_PROTOCOL")); explicitProtocol != "" {
		proto, perr := NormalizeProtocol(explicitProtocol)
		if perr != nil {
			return nil, fmt.Errorf("config: %w", perr)
		}
		c.LLMProtocol = proto
	} else {
		c.LLMProtocol = spec.Protocol
	}

	// 协议决定 Runner，provider 只决定「连哪里、用什么模型」，两者解耦。
	// 这样允许 ProtocolOpenAI + 任意厂商，也支持将来「同厂商多协议」。
	if err := validateProtocolProviderPair(c.LLMProtocol, c.LLMProvider); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// 记录用户是否显式指定了地址/模型（用于决定是否套用预设默认值）。
	explicitBaseURL := envWasSet("LLM_BASE_URL")
	explicitModel := envWasSet("LLM_MODEL")

	if !explicitBaseURL && spec.BaseURL != "" {
		c.LLMBaseURL = spec.BaseURL
	}
	if !explicitModel && spec.DefaultModel != "" {
		c.LLMModel = spec.DefaultModel
	}

	switch c.LLMProtocol {
	case ProtocolMock:
		// Mock 不校验 Key，模型名固定回退到 mock-echo（除非用户显式指定）。
		if !explicitModel {
			c.LLMModel = "mock-echo"
		}
		c.LLMBaseURL = "" // mock 不发起网络请求，清空避免误导
	default:
		// 云端服务商要求 Key；本地部署允许缺省（但建议填占位值）。
		if spec.RequiresKey && c.LLMAPIKey == "" {
			return nil, fmt.Errorf(
				"config: LLM_PROVIDER=%s 需要设置 OPENAI_API_KEY\n提示: %s",
				c.LLMProvider, hintOrDefault(spec.Hint, "在服务商控制台创建 API 密钥"),
			)
		}
		if c.LLMModel == "" {
			return nil, fmt.Errorf(
				"config: LLM_PROVIDER=%s 需要显式指定 LLM_MODEL\n提示: %s",
				c.LLMProvider, hintOrDefault(spec.Hint, "查询服务商文档获取可用模型名"),
			)
		}
		if c.LLMBaseURL == "" {
			return nil, fmt.Errorf(
				"config: LLM_PROVIDER=%s 需要 LLM_BASE_URL（该 provider 无预设地址）",
				c.LLMProvider,
			)
		}
		// 本地部署且未填 Key 时给个占位值，避免部分客户端拒绝空鉴权头。
		if !spec.RequiresKey && c.LLMAPIKey == "" {
			c.LLMAPIKey = "local-no-key"
		}
	}

	if c.Port == "" {
		return nil, fmt.Errorf("config: PORT 不能为空")
	}
	if c.MaxMessagesPerSession <= 0 {
		return nil, fmt.Errorf("config: MAX_MESSAGES_PER_SESSION 必须为正数")
	}
	if c.SSEBufferSize <= 0 {
		return nil, fmt.Errorf("config: SSE_BUFFER_SIZE 必须为正数")
	}
	return c, nil
}

// IsDev 用于放开 CORS / 详细日志。
func (c *Config) IsDev() bool { return c.Env != "prod" }

func defaultSystemPrompt() string {
	if v := strings.TrimSpace(os.Getenv("SYSTEM_PROMPT")); v != "" {
		return v
	}
	return "你是一个严谨、务实的中文助手。回答简洁，代码块务必标注语言。"
}

// validateProtocolProviderPair 校验「协议 + 厂商」组合是否自洽。
//
// 允许的组合规则：
//   - ProtocolMock 只能配 provider=mock（反之亦然），避免「以为在调真模型却是假数据」
//   - ProtocolAnthropic 只应用于 anthropic 系（因为目前只有它提供该协议）
//   - ProtocolOpenAI 可配任意 OpenAI 兼容厂商
//
// 这里刻意不做「协议必须等于厂商预设协议」的强校验：用户完全可能用
// openai 协议去连一个第三方 Anthropic 兼容网关（或反之），只要地址对就行。
func validateProtocolProviderPair(protocol, provider string) error {
	if protocol == ProtocolMock && provider != "mock" {
		return fmt.Errorf(
			"LLM_PROTOCOL=mock 只能与 LLM_PROVIDER=mock 搭配（当前 provider=%s）"+
				"；若想用真实模型请去掉 LLM_PROTOCOL 或设为 openai/anthropic", provider)
	}
	if provider == "mock" && protocol != ProtocolMock {
		return fmt.Errorf(
			"LLM_PROVIDER=mock 必须使用 LLM_PROTOCOL=mock（当前 protocol=%s）", protocol)
	}
	return nil
}

// envWasSet 判断某个环境变量是否被显式设置且非空。
// 用于区分「用户明确指定」与「默认值」，从而决定是否套用 provider 预设。
func envWasSet(key string) bool {
	v, ok := os.LookupEnv(key)
	return ok && strings.TrimSpace(v) != ""
}

// hintOrDefault 返回 hint，为空时用 fallback。
func hintOrDefault(hint, fallback string) string {
	if strings.TrimSpace(hint) != "" {
		return hint
	}
	return fallback
}

// DisplayName 返回带提示的一行描述，用于启动日志。
func (c *Config) DisplayName() string {
	if c.LLMProtocol == ProtocolMock {
		return "Mock 模式（不调用外部服务）"
	}
	return fmt.Sprintf("%s/%s · %s · %s", c.LLMProvider, c.LLMProtocol, c.LLMModel, c.LLMBaseURL)
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// envBool 解析布尔型环境变量。
func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}

func envList(key, def string) []string {
	raw := envString(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"http://localhost:5173"}
	}
	return out
}
