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

// capturePath 启动一个记录「实际请求路径」的服务，并返回一个最小的成功响应。
//
// 这样验证的是真正发出去的 URL，而不是拼接函数的返回值——
// 补全逻辑写对了但没被调用到，是很容易犯的错。
func capturePath(t *testing.T, anthropic bool) (*httptest.Server, *string) {
	t.Helper()
	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		write := func(name string, data any) {
			raw, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw)
			flush()
		}

		if anthropic {
			write("message_start", map[string]any{
				"type":    "message_start",
				"message": map[string]any{"usage": map[string]int{"input_tokens": 1}},
			})
			write("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]string{"type": "text_delta", "text": "ok"},
			})
			write("message_stop", map[string]any{"type": "message_stop"})
			return
		}

		// OpenAI 风格
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// drain 把 Runner 的事件流跑完，确保请求真的发出去了。
func drain(t *testing.T, r Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: historyWith("hi")}) {
	}
}

// TestAnthropicRunnerCompletesV1Segment 验证：接口地址不含 /v1 时，
// 实际请求会落到 /v1/messages（这正是线上 404 的场景）。
func TestAnthropicRunnerCompletesV1Segment(t *testing.T) {
	srv, gotPath := capturePath(t, true)

	// 模拟用户填写的网关地址：有路径前缀但没有版本段。
	r := NewAnthropicRunner(srv.URL+"/coding-plan/ccdesktop", "k", "m", "")
	drain(t, r)

	want := "/coding-plan/ccdesktop/v1/messages"
	if *gotPath != want {
		t.Fatalf("实际请求路径 = %q，期望 %q", *gotPath, want)
	}
}

// TestAnthropicRunnerKeepsExistingV1 验证：已含 /v1 时不重复添加。
func TestAnthropicRunnerKeepsExistingV1(t *testing.T) {
	srv, gotPath := capturePath(t, true)

	r := NewAnthropicRunner(srv.URL+"/v1", "k", "m", "")
	drain(t, r)

	want := "/v1/messages"
	if *gotPath != want {
		t.Fatalf("实际请求路径 = %q，期望 %q（不应重复添加 v1）", *gotPath, want)
	}
}

// TestLLMRunnerCompletesV1Segment 验证 OpenAI 协议同样会补 /v1。
func TestLLMRunnerCompletesV1Segment(t *testing.T) {
	srv, gotPath := capturePath(t, false)

	r := NewLLMRunner(srv.URL+"/coding-plan/ccdesktop", "k", "m", "")
	drain(t, r)

	want := "/coding-plan/ccdesktop/v1/chat/completions"
	if *gotPath != want {
		t.Fatalf("实际请求路径 = %q，期望 %q", *gotPath, want)
	}
}

// TestLLMRunnerKeepsExistingV1 验证 OpenAI 协议不重复补版本段。
func TestLLMRunnerKeepsExistingV1(t *testing.T) {
	srv, gotPath := capturePath(t, false)

	r := NewLLMRunner(srv.URL+"/v1", "k", "m", "")
	drain(t, r)

	want := "/v1/chat/completions"
	if *gotPath != want {
		t.Fatalf("实际请求路径 = %q，期望 %q", *gotPath, want)
	}
}
