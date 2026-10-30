package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agent_rebuild/internal/tool"
)

// maxOutputRunes 工具输出上限，与 read_file 的 maxReadChars 一致：
// 外部工具没有理由比内置工具更能撑爆上下文。
const maxOutputRunes = 30000

// CatalogTool 是一个自定义工具的描述 + 执行入口（见 docs/17 第 12 节）。
//
// 为什么不再是 tool.Tool：自定义工具不原生暴露给模型，不进 Registry，
// 只经代理工具 search_router 发现与执行。search_router 需要的是比 tool.Spec
// 更丰富的元信息（所属 server、原始名、基础风险/开关），而执行入口只需要一个函数。
//
// BaseRisk / BaseEnabled 是 mcp.json 与 annotations 算出的「基础值」；
// 前端设置（customtools 包）在此之上覆盖，最终生效值由 customtools 计算。
type CatalogTool struct {
	Server      string
	Name        string // 远端原始工具名
	Title       string
	Description string
	// InputSchema 已规整为顶层 {"type":"object",...}。
	InputSchema json.RawMessage
	BaseRisk    tool.Risk
	BaseEnabled bool
	// Call 执行一次 tools/call。args 原样透传：schema 由 server 定义也由 server 校验
	// （router 只做最常见错误的预校验，见 docs/17 12.4）。
	Call func(ctx context.Context, args json.RawMessage) (tool.Result, error)
}

// FullName 返回 "server/原始名"。
func (t CatalogTool) FullName() string { return FullName(t.Server, t.Name) }

// buildCatalog 把 tools/list 的结果转成 CatalogTool 列表：算基础风险与开关，绑定执行入口。
//
// 与早期 buildTools 的区别：被 mcp.json 关掉的工具**不再丢弃**，而是 BaseEnabled=false
// 保留下来——前端面板要能看到它们并重新打开。是否给模型用由 customtools 根据生效值决定。
func buildCatalog(sc ServerConfig, client *Client, remote []RemoteTool, warn func(msg string, args ...any)) []CatalogTool {
	out := make([]CatalogTool, 0, len(remote))
	seen := make(map[string]bool, len(remote))
	for _, rt := range remote {
		if rt.Name == "" {
			continue
		}
		if seen[rt.Name] {
			warn("MCP server 返回了重名工具，已跳过", "server", sc.Name, "tool", rt.Name)
			continue
		}
		seen[rt.Name] = true
		out = append(out, CatalogTool{
			Server:      sc.Name,
			Name:        rt.Name,
			Title:       toolTitle(rt),
			Description: strings.TrimSpace(rt.Description),
			InputSchema: normalizeSchema(rt.InputSchema),
			BaseRisk:    resolveRisk(sc, rt),
			BaseEnabled: toolEnabled(sc, rt.Name),
			Call:        callFunc(sc.Name, rt.Name, client),
		})
	}
	return out
}

// callFunc 绑定一次远端调用：渲染结果、截断，错误带上 server 名。
func callFunc(server, name string, client *Client) func(context.Context, json.RawMessage) (tool.Result, error) {
	return func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
		res, err := client.CallTool(ctx, name, args)
		if err != nil {
			// 返回 error → 上层包装成 IsError 回喂模型，模型可以自己决定是否重试。
			return tool.Result{}, fmt.Errorf("MCP server %q: %w", server, err)
		}
		out, truncated := tool.Truncate(renderResult(res), maxOutputRunes)
		return tool.Result{Output: out, IsError: res.IsError, Truncated: truncated}, nil
	}
}

func toolTitle(rt RemoteTool) string {
	if t := strings.TrimSpace(rt.Title); t != "" {
		return t
	}
	if rt.Annotations != nil {
		return strings.TrimSpace(rt.Annotations.Title)
	}
	return ""
}

// toolEnabled 按原始工具名应用 mcp.json 的白/黑名单。白名单非空时只放行名单内工具。
func toolEnabled(sc ServerConfig, name string) bool {
	if len(sc.EnabledTools) > 0 && !slices.Contains(sc.EnabledTools, name) {
		return false
	}
	return !slices.Contains(sc.DisabledTools, name)
}

// resolveRisk 决定远端工具的**基础**风险等级（见 docs/17 第 5 节；前端设置在此之上覆盖）。
//
// 默认 RiskDangerous：外部 server 跑的是任意代码，与 run_command 同级，
// 不因为包了一层「工具」的壳就被信任。只有用户在配置里**显式**放宽才降级。
func resolveRisk(sc ServerConfig, rt RemoteTool) tool.Risk {
	if r, ok := sc.ToolRisk[rt.Name]; ok {
		return r
	}
	if sc.TrustAnnotations && rt.Annotations != nil {
		a := rt.Annotations
		if a.ReadOnlyHint != nil && *a.ReadOnlyHint {
			return tool.RiskSafe
		}
		// destructiveHint 规范默认是 true，只有明确说了 false 才降到 moderate。
		if a.DestructiveHint != nil && !*a.DestructiveHint {
			return tool.RiskModerate
		}
	}
	if r, ok := sc.ToolRisk["*"]; ok {
		return r
	}
	return tool.RiskDangerous
}

// normalizeSchema 保证参数 schema 是两家 API 都接受的形状。
//
// OpenAI 与 Anthropic 都要求顶层是 {"type":"object", ...}；个别 server 对无参
// 工具返回空 schema 或缺 properties，原样透传会让整次模型请求 400
// （不只是这一个工具不可用）。这里只补缺，不改写 server 给出的其他内容。
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	empty := json.RawMessage(`{"type":"object","properties":{}}`)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return empty
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return empty
	}
	changed := false
	if _, ok := m["type"]; !ok {
		m["type"] = json.RawMessage(`"object"`)
		changed = true
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = json.RawMessage(`{}`)
		changed = true
	}
	if !changed {
		return trimmed
	}
	b, err := json.Marshal(m)
	if err != nil {
		return empty
	}
	return b
}

// renderResult 把 CallToolResult 渲染成给模型看的纯文本（见 docs/17 第 6 节）。
//
// 当前 tool.Result 只有文本，图片/音频无法传给模型，用占位说明代替，
// 至少让模型知道「拿到了一张图，但看不到」，而不是以为工具什么都没返回。
func renderResult(res *CallResult) string {
	parts := make([]string, 0, len(res.Content))
	hasText := false
	for _, c := range res.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
			hasText = true
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s: %s, 约 %s，当前客户端不支持把%s传给模型]",
				c.Type, orUnknown(c.MimeType), approxDecodedSize(c.Data), mediaWord(c.Type)))
		case "resource":
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
				hasText = true
			} else if c.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource: %s (%s)]", c.Resource.URI, orUnknown(c.Resource.MimeType)))
			}
		case "resource_link":
			parts = append(parts, fmt.Sprintf("[resource_link: %s %s]", c.Name, c.URI))
		default:
			parts = append(parts, fmt.Sprintf("[未知内容类型 %q]", c.Type))
		}
	}
	// 规范要求返回 structuredContent 的 server 同时给一份 text 序列化；
	// 没给的时候才用它兜底，避免同一份数据在输出里出现两遍。
	if !hasText && len(bytes.TrimSpace(res.StructuredContent)) > 0 {
		parts = append(parts, string(res.StructuredContent))
	}
	if len(parts) == 0 {
		if res.IsError {
			return "（工具报告失败，但没有返回错误信息）"
		}
		return "（工具执行成功，无输出）"
	}
	return strings.Join(parts, "\n\n")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func mediaWord(typ string) string {
	if typ == "audio" {
		return "音频"
	}
	return "图片"
}

// approxDecodedSize 估算 base64 解码后的大小（不真的解码，只看长度）。
func approxDecodedSize(b64 string) string {
	n := len(b64) * 3 / 4
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dKB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
