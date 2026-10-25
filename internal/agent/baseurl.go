package agent

import "strings"

// 版本段常量。
//
// 为什么需要它：Anthropic 的 Messages API 与 OpenAI 的 Chat Completions API
// 都把版本号写死在路径里（/v1/messages、/v1/chat/completions），没有
// 「无版本」的入口。但用户在「接口地址」里填的东西五花八门：
//
//	https://api.openai.com            ← 只填域名
//	https://api.openai.com/v1         ← 官方文档写法
//	https://api.openai.com/v1/         ← 带尾斜杠
//	https://api.openai.com/v1beta     ← 少数兼容实现
//
// 如果程序只做字符串拼接，前两种填法会得到不同结果，其中一种必然 404。
// normalizeBaseURL 负责把这些写法统一到「以版本段结尾、无尾斜杠」的形式。
const defaultAPIVersion = "v1"

// knownVersionSegments 是「看起来已经是版本段」的路径尾段。
//
// 覆盖 v1 / v2 / v1beta / v4 这类形式。识别到就不再补 v1，
// 否则会把 .../v1beta 变成 .../v1beta/v1。
func looksLikeVersionSegment(seg string) bool {
	s := strings.ToLower(strings.TrimSpace(seg))
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	// v 后面必须是数字，允许再跟字母后缀（beta / alpha 等）。
	rest := s[1:]
	if rest[0] < '0' || rest[0] > '9' {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		default:
			return false
		}
	}
	return true
}

// normalizeBaseURL 把用户填写的接口地址归一化，返回可直接拼接端点名的前缀。
//
// 规则：
//  1. 去空白、去尾部斜杠
//  2. 末尾已是版本段（v1 / v2 / v1beta…）→ 原样返回
//  3. 否则追加 /v1
//
// 空串直接返回空串：让调用方自行决定默认值，避免凭空造出一个地址。
func normalizeBaseURL(raw string) string {
	base := strings.TrimSpace(raw)
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}

	// 取最后一段判断是否已是版本段。
	idx := strings.LastIndex(base, "/")
	last := base
	if idx >= 0 {
		last = base[idx+1:]
	}
	if looksLikeVersionSegment(last) {
		return base
	}
	return base + "/" + defaultAPIVersion
}

// endpointURL 拼出完整请求地址。
//
// baseURL 为空的场景（例如只配了 provider 预设）交给 Orchestrator 兜底，
// 这里保持简单：空则只返回端点路径。
func endpointURL(baseURL, path string) string {
	base := normalizeBaseURL(baseURL)
	path = strings.TrimPrefix(path, "/")
	if base == "" {
		return path
	}
	return base + "/" + path
}
