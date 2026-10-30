// Package customtools 管理「自定义工具」（MCP 等外部来源）的用户设置，
// 并把「外部工具目录 + 用户设置」组装成代理工具 search_router（见 docs/17-mcp-client.md 第 5、12 节）。
//
// 分层：
//   - mcp 包只管连接与基础值（mcp.json / annotations 算出的风险与开关），不知道用户设置；
//   - router 包只管 list / search / execute，不知道 MCP，也不知道设置从哪来；
//   - 本包在两者之间：取 mcp.Catalog()，叠加用户覆盖算出生效值，构建 Router，
//     再交给 apply（main.go 里是 api.Server.ReplaceTools）原子替换进 Registry。
package customtools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent_rebuild/internal/tool"
)

// 领域错误。
var (
	// ErrInvalid 入参不合法（工具名格式、风险取值）。
	ErrInvalid = errors.New("customtools: 参数不合法")
	// ErrUnsupported 当前存储不支持保存设置（STORE_DRIVER=memory）。
	ErrUnsupported = errors.New("customtools: 当前存储不支持保存工具设置（请用 STORE_DRIVER=sqlite 启动）")
)

// Override 是用户对一个自定义工具的覆盖设置。nil 字段表示「跟随基础值」。
type Override struct {
	Risk    *tool.Risk `json:"-"`
	Enabled *bool      `json:"-"`
}

// IsEmpty 两个字段都跟随基础值：等价于没有覆盖，存储层应删除这一行。
func (o Override) IsEmpty() bool { return o.Risk == nil && o.Enabled == nil }

// Effective 计算生效值：覆盖优先，否则用基础值。
func Effective(baseRisk tool.Risk, baseEnabled bool, o Override) (tool.Risk, bool) {
	risk, enabled := baseRisk, baseEnabled
	if o.Risk != nil {
		risk = *o.Risk
	}
	if o.Enabled != nil {
		enabled = *o.Enabled
	}
	return risk, enabled
}

// Input 是 API 层的入参：Risk 为空串表示恢复默认；Enabled 为 nil 表示恢复默认。
type Input struct {
	Tool    string `json:"tool"`
	Risk    string `json:"risk"`
	Enabled *bool  `json:"enabled"`
}

// parse 校验并转换为 (工具全名, Override)。
func (in Input) parse() (string, Override, error) {
	name := strings.TrimSpace(in.Tool)
	server, toolName, ok := strings.Cut(name, "/")
	if !ok || strings.TrimSpace(server) == "" || strings.TrimSpace(toolName) == "" {
		return "", Override{}, fmt.Errorf("%w：tool 必须是「服务名/工具名」，实际 %q", ErrInvalid, in.Tool)
	}
	var o Override
	if r := strings.TrimSpace(in.Risk); r != "" {
		risk, err := ParseRisk(r)
		if err != nil {
			return "", Override{}, err
		}
		o.Risk = &risk
	}
	if in.Enabled != nil {
		v := *in.Enabled
		o.Enabled = &v
	}
	return name, o, nil
}

// ParseRisk 解析 safe / moderate / dangerous。
func ParseRisk(s string) (tool.Risk, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "safe":
		return tool.RiskSafe, nil
	case "moderate":
		return tool.RiskModerate, nil
	case "dangerous":
		return tool.RiskDangerous, nil
	default:
		return 0, fmt.Errorf("%w：风险等级只能是 safe / moderate / dangerous，实际 %q", ErrInvalid, s)
	}
}

// Store 是覆盖设置的持久化接口。
type Store interface {
	// List 返回全部覆盖（键为 "server/原始名"）。
	List(ctx context.Context) (map[string]Override, error)
	// Set 保存一条覆盖；o.IsEmpty() 时删除该行。
	Set(ctx context.Context, tool string, o Override) error
}
