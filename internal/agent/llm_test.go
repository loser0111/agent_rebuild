package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/session"
)

// fakeOpenAI 模拟 OpenAI 兼容网关的 /chat/completions 流式响应。
func fakeOpenAI(t *testing.T, chunks []string, opts ...func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 传进来的是假服务的裸地址（不含 /v1），路径会被自动补全版本段。
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("请求路径 = %q，期望 /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}

		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		if !req.Stream {
			t.Error("应请求 stream=true")
		}
		if req.Model == "" {
			t.Error("model 不能为空")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		for _, c := range chunks {
			payload := map[string]any{
				"choices": []map[string]any{
					{"delta": map[string]string{"content": c}},
				},
			}
			raw, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// 末帧带 usage + finish_reason
		final := map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]string{}, "finish_reason": "stop"},
			},
			"usage": map[string]int{
				"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30,
			},
		}
		raw, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		if len(opts) > 0 {
			opts[0](w)
		}
	}))
}

func TestLLMRunnerStreamsUpstreamSSE(t *testing.T) {
	srv := fakeOpenAI(t, []string{"你", "好", "，", "世界"})
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "test-key", "gpt-x", "你是助手")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var deltas []string
	var done *Event
	for ev := range r.Run(ctx, Request{History: historyWith("问个问题")}) {
		switch ev.Type {
		case TypeDelta:
			deltas = append(deltas, ev.Delta)
		case TypeDone:
			e := ev
			done = &e
		case TypeError:
			t.Fatalf("意外错误: %v", ev.Err)
		}
	}

	if strings.Join(deltas, "") != "你好，世界" {
		t.Fatalf("增量拼接 = %q", strings.Join(deltas, ""))
	}
	if done == nil {
		t.Fatal("未收到 done")
	}
	if done.Content != "你好，世界" {
		t.Fatalf("done.Content = %q", done.Content)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 30 {
		t.Fatalf("usage 解析错误: %+v", done.Usage)
	}
}

// system prompt 应作为第一条 message 发送，且历史按 role 转换。
func TestLLMRunnerSendsSystemPromptAndHistory(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "k", "m", "系统提示")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: historyWith("用户提问")}) {
	}

	if len(captured.Messages) < 2 {
		t.Fatalf("应包含 system + user，实际 %d 条", len(captured.Messages))
	}
	if captured.Messages[0].Role != "system" || captured.Messages[0].Content != "系统提示" {
		t.Fatalf("首条应为 system prompt: %+v", captured.Messages[0])
	}
	if captured.Messages[1].Role != "user" || captured.Messages[1].Content != "用户提问" {
		t.Fatalf("第二条应为 user 消息: %+v", captured.Messages[1])
	}
}

// 上游返回非 200 时应发 TypeError。
func TestLLMRunnerUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "bad", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotErr error
	for ev := range r.Run(ctx, Request{History: historyWith("x")}) {
		if ev.Type == TypeError {
			gotErr = ev.Err
		}
		if ev.Type == TypeDone {
			t.Fatal("上游失败时不应产生 done")
		}
	}
	if gotErr == nil {
		t.Fatal("应收到错误事件")
	}
	if !strings.Contains(gotErr.Error(), "401") {
		t.Fatalf("错误信息应包含状态码: %v", gotErr)
	}
}

// 上游返回空内容时不应假装成功。
func TestLLMRunnerEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotErr error
	for ev := range r.Run(ctx, Request{History: historyWith("x")}) {
		if ev.Type == TypeError {
			gotErr = ev.Err
		}
	}
	if gotErr == nil {
		t.Fatal("空响应应报错，避免前端显示空气泡")
	}
}

// ctx 取消应中断上游请求。
func TestLLMRunnerCancelStops(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range r.Run(ctx, Request{History: historyWith("x")}) {
		}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 LLMRunner 未退出")
	}
}

// 中断的 assistant 消息不应进入上下文（避免把半截回答当事实继续推理）。
func TestLLMRunnerSkipsInterruptedMessages(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "问题", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleAssistant, Content: "半截回答", Status: session.StatusInterrupted},
	}

	r := NewLLMRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: history}) {
	}

	for _, m := range captured.Messages {
		if strings.Contains(m.Content, "半截回答") {
			t.Fatalf("中断的半截回答不应进入上下文: %+v", captured.Messages)
		}
	}
}
