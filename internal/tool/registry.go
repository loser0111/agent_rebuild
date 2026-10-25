package tool

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Options 是默认工具集的配置。
type Options struct {
	// Workspace 工作区根目录（所有文件与命令都限制在此）。
	Workspace string
	// EnableCommand 是否启用命令执行工具（危险，默认关闭）。
	EnableCommand bool
	// ReadOnly 只读模式：只注册读取类工具。
	ReadOnly bool
}

// DefaultRegistry 按配置装配默认工具集。
//
// 参考 Codex / Claude Code 的取舍：
//   - 默认提供 read_file / list_dir / search（只读，零风险）
//   - 写操作（write_file / edit_file）默认启用，但标为中风险
//   - 命令执行默认**不启用**，需显式开启（最危险）
func DefaultRegistry(opts Options) *Registry {
	root, err := filepath.Abs(opts.Workspace)
	if err != nil {
		slog.Warn("工作目录解析失败，使用当前目录", "err", err)
		root = opts.Workspace
	}
	// 确保目录存在，避免工具链在首次调用时集体失败。
	if err := os.MkdirAll(root, 0o755); err != nil {
		slog.Warn("创建工具工作目录失败", "dir", root, "err", err)
	}

	reg := NewRegistry()

	// 只读工具：始终注册。
	reg.Register(
		NewReadFileTool(root),
		NewListDirTool(root),
		NewSearchTool(root),
	)

	if opts.ReadOnly {
		slog.Info("工具集为只读模式", "workspace", root, "count", reg.Len())
		return reg
	}

	// 写工具：中风险（需要审批，但默认启用）。
	reg.Register(
		NewWriteFileTool(root),
		NewEditFileTool(root),
	)

	// 命令执行：高风险，默认不启用。
	if opts.EnableCommand {
		reg.Register(NewRunCommandTool(root))
	}

	slog.Info("工具集已装配",
		"workspace", root, "count", reg.Len(), "tools", strings.Join(reg.Names(), ","),
		"command_enabled", opts.EnableCommand)
	return reg
}

// ---- 协议适配：把 Spec 转成各模型 API 需要的格式 ----

// OpenAITool 是 OpenAI 协议的 tools 数组元素。
type OpenAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ToOpenAITools 转换为 OpenAI 兼容格式。
func ToOpenAITools(specs []Spec) []OpenAITool {
	out := make([]OpenAITool, 0, len(specs))
	for _, s := range specs {
		var t OpenAITool
		t.Type = "function"
		t.Function.Name = s.Name
		t.Function.Description = s.Description
		t.Function.Parameters = s.Parameters
		out = append(out, t)
	}
	return out
}

// AnthropicTool 是 Anthropic Messages API 的 tools 数组元素。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToAnthropicTools 转换为 Anthropic 格式。
func ToAnthropicTools(specs []Spec) []AnthropicTool {
	out := make([]AnthropicTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, AnthropicTool{
			Name:        s.Name,
			Description: s.Description,
			InputSchema: s.Parameters,
		})
	}
	return out
}

// Summarize 生成工具列表的文本摘要（用于系统提示或调试日志）。
func Summarize(specs []Spec) string {
	var sb strings.Builder
	for _, s := range specs {
		risk := ""
		if s.Risk != RiskSafe {
			risk = " [" + s.Risk.String() + "]"
		}
		sb.WriteString("- ")
		sb.WriteString(s.Name)
		sb.WriteString(risk)
		sb.WriteString(": ")
		sb.WriteString(s.Description)
		sb.WriteString("\n")
	}
	return sb.String()
}
