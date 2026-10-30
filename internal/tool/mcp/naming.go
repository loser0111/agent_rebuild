package mcp

import (
	"fmt"
	"regexp"
	"strings"
)

// 工具命名（见 docs/17-mcp-client.md 第 4 节）。
//
// 自定义工具不原生暴露给模型，名字只出现在 search_router 的参数和结果里，
// 形如 "server/原始名"（例如 github/search_issues）。它只是一个普通字符串，
// 不受 OpenAI function name [a-zA-Z0-9_-]{1,64} 的约束，所以原始名原样保留，
// 不清洗、不截断、不加哈希——模型读到的就是 server 自己起的名字。
//
// 早期为原生暴露设计的 mcp__<server>__<tool>（清洗 + 哈希后缀）已随原生暴露一起废弃。
const maxServerName = 32

// serverNameRe 约束 server 名：不含 "/"，保证全名按第一个 "/" 切分一定能还原出
// (server, tool)；MCP 规范的工具名也不含 "/"。
var serverNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateServerName 校验配置里的 server 名，不合法时返回可读原因。
func ValidateServerName(name string) error {
	if name == "" {
		return fmt.Errorf("server 名不能为空")
	}
	if len(name) > maxServerName {
		return fmt.Errorf("server 名 %q 过长（最多 %d 个字符）", name, maxServerName)
	}
	if !serverNameRe.MatchString(name) {
		return fmt.Errorf("server 名 %q 不合法：只允许字母、数字、下划线和连字符", name)
	}
	return nil
}

// FullName 返回自定义工具的全名 "server/原始名"。
func FullName(server, tool string) string { return server + "/" + tool }

// SplitFullName 把全名拆回 (server, tool)；没有 "/" 时 ok=false。
func SplitFullName(full string) (server, tool string, ok bool) {
	return strings.Cut(full, "/")
}
