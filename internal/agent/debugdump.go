package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// dumpRequest 在 LOG_LEVEL=debug 下打印即将发出的模型请求。
//
// 默认只打「元信息」（协议、地址、模型名、请求体字节数与消息条数），
// 因为 body 里含完整对话历史与工具输出——本项目这些内容常达数十 KB，
// 每次都全量打到日志里会淹没真正有用的行。
//
// 需要看完整参数时置 LLM_DUMP_BODY=1（见 debugDumpBodyEnabled）。
//
// 这里刻意不接收 http.Header：密钥在 llm.go 的 Authorization: Bearer
// 与 anthropic.go 的 x-api-key 里，一旦跟着 body 一起打出来就等于明文
// 落盘。要看 headers 请用 HTTP 代理抓包（Transport 已设 ProxyFromEnvironment）。
func dumpRequest(ctx context.Context, protocol, url, model string, raw []byte, msgCount int) {
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}

	attrs := []any{
		"protocol", protocol,
		"url", url,
		"model", model,
		"body_bytes", len(raw),
		"messages", msgCount,
	}
	if debugDumpBodyEnabled() {
		attrs = append(attrs, "body", string(raw))
	}
	slog.DebugContext(ctx, "模型请求", attrs...)
}

// debugDumpBodyEnabled 判断是否输出完整请求体。
//
// 关掉它只需要不设置（或置 0）：默认关闭是刻意的，避免误把
// 用户对话内容与工具输出长期留在日志文件里。
func debugDumpBodyEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LLM_DUMP_BODY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
