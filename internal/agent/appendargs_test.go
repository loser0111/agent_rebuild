package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// builder 包一层 strings.Builder，便于在测试里直接传参。
type builder struct{ b strings.Builder }

// jsonErr 返回 JSON 解析错误信息（合法时返回 nil）。
func jsonErr(s string) error {
	var v any
	return json.Unmarshal([]byte(s), &v)
}

// TestAppendArgsTokenByToken 复现线上故障：
// 逐 token 分片时，若某个分片本身是一个合法 JSON 值（如 "true"、"\"str\""），
// 旧的"判重"逻辑会误判为"重复发送完整参数"并 RESET，
// 导致缓冲区只剩尾部片段（截图里的 `true}`）。
func TestAppendArgsTokenByToken(t *testing.T) {
	// 这些分片逐 token 到达，拼起来才是完整参数。
	frags := []string{
		`{`, `"path"`, `:`, ` `, `"internal/tool"`,
		`,`, ` `, `"recursive"`, `:`, ` `, `true`, `}`,
	}

	var buf builder
	for _, f := range frags {
		appendArgs(&buf.b, f)
	}

	want := `{"path": "internal/tool", "recursive": true}`
	got := buf.b.String()

	if !json.Valid([]byte(got)) {
		t.Fatalf("逐 token 拼接结果非法: %v\n得到: %q\n期望: %q",
			jsonErr(got), got, want)
	}
	if got != want {
		t.Fatalf("拼接结果 = %q，期望 %q", got, want)
	}
}

// TestAppendArgsChunkedValues 分片边界落在合法 JSON 值上（"true" 独立成片）。
func TestAppendArgsChunkedValues(t *testing.T) {
	// 这类分片非常常见：模型按 token 流式输出，"true" 单独一片。
	frags := []string{
		`{"path": "internal/tool", "recursive":`,
		` `,
		`true`,
		`}`,
	}

	var buf builder
	for _, f := range frags {
		appendArgs(&buf.b, f)
	}

	want := `{"path": "internal/tool", "recursive": true}`
	got := buf.b.String()
	if !json.Valid([]byte(got)) {
		t.Fatalf("分段拼接结果非法: %v\n得到: %q", jsonErr(got), got)
	}
	if got != want {
		t.Fatalf("拼接结果 = %q，期望 %q", got, want)
	}
}

// TestAppendArgsRegressionNeverTruncates 回归防护：
// 逐 token 流式时绝不能出现"只剩尾部片段"的结果。
// 线上故障表现就是参数变成 `true}`（前面内容被误判为重复而丢弃）。
func TestAppendArgsRegressionNeverTruncates(t *testing.T) {
	// 覆盖各种会单独成片的合法 JSON 值。
	cases := []struct {
		name  string
		frags []string
		want  string
	}{
		{
			name:  "布尔 true 独立成片",
			frags: []string{`{"recursive":`, ` `, `true`, `}`},
			want:  `{"recursive": true}`,
		},
		{
			name:  "数字独立成片",
			frags: []string{`{"max":`, ` `, `123`, `}`},
			want:  `{"max": 123}`,
		},
		{
			name:  "字符串独立成片",
			frags: []string{`{"path":`, ` `, `"abc"`, `}`},
			want:  `{"path": "abc"}`,
		},
		{
			name:  "null 独立成片",
			frags: []string{`{"x":`, `null`, `}`},
			want:  `{"x":null}`,
		},
		{
			name:  "嵌套对象",
			frags: []string{`{"a":`, `{"b":`, `1`, `}`, `}`},
			want:  `{"a":{"b":1}}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			for _, f := range c.frags {
				appendArgs(&b, f)
			}
			got := b.String()
			if got != c.want {
				t.Fatalf("分片 %v\n得到: %q\n期望: %q", c.frags, got, c.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("结果不是合法 JSON: %q", got)
			}
		})
	}
}

// TestStreamToolCallTokenByToken 端到端：SSE 每帧一个 token 片段。
func TestStreamToolCallTokenByToken(t *testing.T) {
	frags := []string{
		`{`, `"path"`, `:`, ` `, `"internal/tool"`,
		`,`, ` `, `"recursive"`, `:`, ` `, `true`, `}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		for i, f := range frags {
			tc := map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": f},
			}
			if i == 0 {
				tc["id"] = "call_1"
				tc["type"] = "function"
				tc["function"] = map[string]any{"name": "list_dir", "arguments": f}
			}
			sseChunk(t, w, map[string]any{"tool_calls": []map[string]any{tc}})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewLLMRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	assertValidJSONObject(t, calls[0].Args)
	if calls[0].Args != `{"path": "internal/tool", "recursive": true}` {
		t.Fatalf("参数 = %q", calls[0].Args)
	}
}
