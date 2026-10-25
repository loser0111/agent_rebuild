package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 复现线上故障：工具调用的 arguments 被拼接成 `{}{"path":...}`，
// 导致 json.Unmarshal 报 "invalid character '{' after top-level value"。

// sseChunk 渲染一帧 OpenAI 风格 SSE。
func sseChunk(t *testing.T, w http.ResponseWriter, delta map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": delta}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fmt.Fprintf(w, "data: %s\n\n", body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// collectToolCalls 跑一次 Runner 并返回所有 tool.call 事件。
func collectToolCalls(t *testing.T, r Runner) []ToolCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var calls []ToolCall
	for ev := range r.Run(ctx, Request{History: historyWith("hi")}) {
		if ev.Type == TypeToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	return calls
}

// assertValidJSONObject 断言参数是合法 JSON 对象。
func assertValidJSONObject(t *testing.T, raw string) {
	t.Helper()
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("参数不是合法 JSON 对象: %v\n原始内容: %q", err, raw)
	}
}

// TestStreamToolCallArgumentsFragmented 标准 OpenAI 行为：
// 首帧给 id/name + 第一个参数片段，后续帧只追加剩余片段。
func TestStreamToolCallArgumentsFragmented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "list_dir", "arguments": `{"path":`},
			}},
		})
		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    0,
				"function": map[string]any{"arguments": `"/tmp","recursive":true}`},
			}},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewLLMRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	if calls[0].Name != "list_dir" {
		t.Fatalf("工具名 = %q", calls[0].Name)
	}
	assertValidJSONObject(t, calls[0].Args)
	if calls[0].Args != `{"path":"/tmp","recursive":true}` {
		t.Fatalf("参数 = %q", calls[0].Args)
	}
}

// TestStreamToolCallArgumentsRepeatedFull 复现线上网关行为：
// 每帧都带**完整**的 arguments（而不是增量片段）。
//
// 这正是截图里 `{}{"path":...}` 的来源：无条件字符串拼接把
// 第一帧的完整对象与第二帧的完整对象连在了一起。
func TestStreamToolCallArgumentsRepeatedFull(t *testing.T) {
	full := `{"path":"/Users/temptrip/external/agent_rebuild","recursive":true}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 第一帧：带完整参数
		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "list_dir", "arguments": full},
			}},
		})
		// 第二帧：重复同一个完整参数
		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    0,
				"function": map[string]any{"arguments": full},
			}},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewLLMRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	// 关键断言：不能变成 {}{...}
	assertValidJSONObject(t, calls[0].Args)
	if calls[0].Args != full {
		t.Fatalf("参数 = %q，期望 %q", calls[0].Args, full)
	}
}

// TestStreamToolCallEmptyArgumentsObject 某些网关在无参数时发送 "{}"。
func TestStreamToolCallEmptyArgumentsObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "list_dir", "arguments": "{}"},
			}},
		})
		// 再来一帧空的 {}
		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    0,
				"function": map[string]any{"arguments": "{}"},
			}},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewLLMRunner(srv.URL, "k", "m", ""))
	if len(calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(calls))
	}
	assertValidJSONObject(t, calls[0].Args)
}

// TestStreamToolCallMultipleCalls 两个独立调用（不同 index）不能被混在一起。
func TestStreamToolCallMultipleCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		sseChunk(t, w, map[string]any{
			"tool_calls": []map[string]any{
				{"index": 0, "id": "c0", "type": "function",
					"function": map[string]any{"name": "list_dir", "arguments": `{"path":"/a"}`}},
				{"index": 1, "id": "c1", "type": "function",
					"function": map[string]any{"name": "search", "arguments": `{"pattern":"x"}`}},
			},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	calls := collectToolCalls(t, NewLLMRunner(srv.URL, "k", "m", ""))
	if len(calls) != 2 {
		t.Fatalf("应有 2 个调用，实际 %d", len(calls))
	}
	if calls[0].Name != "list_dir" || calls[1].Name != "search" {
		t.Fatalf("工具名不对: %q %q", calls[0].Name, calls[1].Name)
	}
	assertValidJSONObject(t, calls[0].Args)
	assertValidJSONObject(t, calls[1].Args)
}
