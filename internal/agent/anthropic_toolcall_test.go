package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 复现线上故障（Anthropic 协议）：
// content_block_start 的 input 是空对象 {} 占位，真实参数走 input_json_delta，
// 两者被拼在一起成为 `{"}{"path":...}`。

// anthropicSSE 渲染一帧 Anthropic 风格 SSE。
func anthropicSSE(t *testing.T, w http.ResponseWriter, event string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestAnthropicStreamToolInputEmptyObjectThenDeltas 复现核心 bug：
// 严格照 Anthropic 官方流式格式——input 为 {}，后续 partial_json 拼出真实参数。
func TestAnthropicStreamToolInputEmptyObjectThenDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		anthropicSSE(t, w, "message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"usage": map[string]int{"input_tokens": 10}},
		})

		// 官方格式：content_block_start 带 input: {}
		anthropicSSE(t, w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    "toolu_01",
				"name":  "list_dir",
				"input": map[string]any{},
			},
		})

		// 真实参数走 input_json_delta 分片
		for _, frag := range []string{`{"path": "/tmp",`, ` "recursive": true}`} {
			anthropicSSE(t, w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
			})
		}

		anthropicSSE(t, w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 0,
		})
		anthropicSSE(t, w, "message_stop", map[string]any{"type": "message_stop"})
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewAnthropicRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	if calls[0].Name != "list_dir" {
		t.Fatalf("工具名 = %q", calls[0].Name)
	}
	assertValidJSONObject(t, calls[0].Args)
	if calls[0].Args != `{"path": "/tmp", "recursive": true}` {
		t.Fatalf("参数 = %q", calls[0].Args)
	}
}

// TestAnthropicStreamToolInputEmptyObjectNoArgs 无参数工具：
// input 为 {}，且没有任何 input_json_delta。
func TestAnthropicStreamToolInputEmptyObjectNoArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		anthropicSSE(t, w, "message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"usage": map[string]int{"input_tokens": 5}},
		})
		anthropicSSE(t, w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use", "id": "toolu_02", "name": "get_time",
				"input": map[string]any{},
			},
		})
		anthropicSSE(t, w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 0,
		})
		anthropicSSE(t, w, "message_stop", map[string]any{"type": "message_stop"})
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewAnthropicRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	assertValidJSONObject(t, calls[0].Args)
	if calls[0].Args != "{}" {
		t.Fatalf("无参工具参数应为 {}，实际 %q", calls[0].Args)
	}
}

// TestAnthropicStreamToolInputFullInStart 非流式风格的网关：
// 直接在 content_block_start 里给完整 input，没有任何 delta。
func TestAnthropicStreamToolInputFullInStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		anthropicSSE(t, w, "message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"usage": map[string]int{"input_tokens": 5}},
		})
		anthropicSSE(t, w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use", "id": "toolu_03", "name": "search",
				"input": map[string]any{"pattern": "foo"},
			},
		})
		anthropicSSE(t, w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 0,
		})
		anthropicSSE(t, w, "message_stop", map[string]any{"type": "message_stop"})
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewAnthropicRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	assertValidJSONObject(t, calls[0].Args)

	var got map[string]any
	if err := json.Unmarshal([]byte(calls[0].Args), &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got["pattern"] != "foo" {
		t.Fatalf("参数内容不对: %v", got)
	}
}

// TestAnthropicStreamToolInputEmptyObjectThenFullDelta 部分网关：
// input 为 {}，但 delta 每帧发送完整 JSON。
func TestAnthropicStreamToolInputEmptyObjectThenFullDelta(t *testing.T) {
	full := `{"path":"/tmp"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		anthropicSSE(t, w, "message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"usage": map[string]int{"input_tokens": 5}},
		})
		anthropicSSE(t, w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use", "id": "toolu_04", "name": "list_dir",
				"input": map[string]any{},
			},
		})
		// 重复发送完整参数两次
		for i := 0; i < 2; i++ {
			anthropicSSE(t, w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": full},
			})
		}
		anthropicSSE(t, w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 0,
		})
		anthropicSSE(t, w, "message_stop", map[string]any{"type": "message_stop"})
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewAnthropicRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	assertValidJSONObject(t, calls[0].Args)
	if calls[0].Args != full {
		t.Fatalf("参数 = %q，期望 %q", calls[0].Args, full)
	}
}

// TestAnthropicStreamMultipleToolBlocks 多个 tool_use 块互不干扰。
func TestAnthropicStreamMultipleToolBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		anthropicSSE(t, w, "message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"usage": map[string]int{"input_tokens": 5}},
		})

		for i, name := range []string{"list_dir", "search"} {
			anthropicSSE(t, w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": i,
				"content_block": map[string]any{
					"type": "tool_use", "id": fmt.Sprintf("toolu_%d", i), "name": name,
					"input": map[string]any{},
				},
			})
			anthropicSSE(t, w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]any{
					"type": "input_json_delta", "partial_json": `{"q":"x"}`,
				},
			})
			anthropicSSE(t, w, "content_block_stop", map[string]any{
				"type": "content_block_stop", "index": i,
			})
		}
		anthropicSSE(t, w, "message_stop", map[string]any{"type": "message_stop"})
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewAnthropicRunner(srv.URL, "k", "m", ""))
	if len(calls) != 2 {
		t.Fatalf("应有 2 个调用，实际 %d", len(calls))
	}
	if calls[0].Name != "list_dir" || calls[1].Name != "search" {
		t.Fatalf("工具名不对: %q %q", calls[0].Name, calls[1].Name)
	}
	for _, c := range calls {
		assertValidJSONObject(t, c.Args)
	}
}
