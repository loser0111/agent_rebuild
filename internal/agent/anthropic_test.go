package agent

// AnthropicRunner 测试：用 httptest 模拟真实 Messages API 的流式响应，
// 验证路径、鉴权头、system 处理、事件解析与用量。

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

// fakeAnthropic 模拟 Anthropic Messages API。
func fakeAnthropic(t *testing.T, textChunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 校验路径与鉴权方式（这是与 OpenAI 最大的差异）
		// 传进来的是假服务的裸地址（不含 /v1），因此路径会被自动补全版本段。
		if r.URL.Path != "/v1/messages" {
			t.Errorf("路径 = %q，期望 /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-test" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q，期望 2023-06-01", got)
		}
		// Anthropic 不使用 Bearer
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("不应发送 Authorization 头，得到 %q", got)
		}

		// 2. 校验请求体结构
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    string `json:"system"`
			Stream    bool   `json:"stream"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		if !req.Stream {
			t.Error("应请求 stream=true")
		}
		if req.MaxTokens <= 0 {
			t.Error("max_tokens 是必填项，必须 > 0")
		}
		if req.Model == "" {
			t.Error("model 不能为空")
		}
		// system 必须走顶层字段，不能出现在 messages 里
		for _, m := range req.Messages {
			if m.Role == "system" {
				t.Error("system 消息不应出现在 messages 数组中")
			}
		}

		// 3. 返回 Anthropic 风格的 SSE 流
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		writeEvent := func(name string, data any) {
			raw, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw)
			flush()
		}

		writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"usage": map[string]int{"input_tokens": 42, "output_tokens": 0},
			},
		})
		writeEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		for _, chunk := range textChunks {
			writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]string{"type": "text_delta", "text": chunk},
			})
		}
		writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]string{"stop_reason": "end_turn"},
			"usage": map[string]int{"input_tokens": 42, "output_tokens": 17},
		})
		writeEvent("message_stop", map[string]any{"type": "message_stop"})
	}))
}

func TestAnthropicRunnerStreams(t *testing.T) {
	srv := fakeAnthropic(t, []string{"你好", "，", "世界"})
	defer srv.Close()

	r := NewAnthropicRunner(srv.URL, "sk-ant-test", "claude-3-5-sonnet-latest", "你是助手")
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

	if got := strings.Join(deltas, ""); got != "你好，世界" {
		t.Fatalf("增量拼接 = %q", got)
	}
	if done == nil {
		t.Fatal("未收到 done")
	}
	if done.Content != "你好，世界" {
		t.Fatalf("done.Content = %q", done.Content)
	}
	// Anthropic 的 usage 是 input_tokens / output_tokens，需正确映射。
	if done.Usage == nil {
		t.Fatal("缺少 usage")
	}
	if done.Usage.PromptTokens != 42 {
		t.Fatalf("PromptTokens = %d，期望 42", done.Usage.PromptTokens)
	}
	if done.Usage.CompletionTokens != 17 {
		t.Fatalf("CompletionTokens = %d，期望 17", done.Usage.CompletionTokens)
	}
	if done.Usage.TotalTokens != 59 {
		t.Fatalf("TotalTokens = %d，期望 59", done.Usage.TotalTokens)
	}
}

func TestAnthropicRunnerName(t *testing.T) {
	r := NewAnthropicRunner("https://api.anthropic.com/v1", "k", "claude-3-opus", "")
	if got := r.Name(); got != "anthropic:claude-3-opus" {
		t.Fatalf("Name() = %q", got)
	}
	// BaseURL 原样保存；尾斜杠与版本段的归一化在拼接请求地址时处理，
	// 避免构造函数对入参做隐式改写（用户填什么，回显就是什么）。
	if r.BaseURL != "https://api.anthropic.com/v1" {
		t.Fatalf("BaseURL = %q", r.BaseURL)
	}
}

// system prompt 必须走顶层字段。
func TestAnthropicRunnerSystemPromptTopLevel(t *testing.T) {
	var captured struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	r := NewAnthropicRunner(srv.URL, "k", "m", "系统提示")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: historyWith("用户提问")}) {
	}

	if captured.System != "系统提示" {
		t.Fatalf("顶层 system = %q，期望「系统提示」", captured.System)
	}
	if len(captured.Messages) != 1 || captured.Messages[0].Role != "user" {
		t.Fatalf("messages 应只含 user，实际: %+v", captured.Messages)
	}
}

// 历史中的 system 消息应被过滤（不能进 messages 数组）。
func TestAnthropicRunnerFiltersSystemHistory(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	history := []session.Message{
		{ID: "1", Role: session.RoleSystem, Content: "旧的系统提示", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleUser, Content: "问题", Status: session.StatusComplete},
	}

	r := NewAnthropicRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range r.Run(ctx, Request{History: history}) {
	}

	for _, m := range captured.Messages {
		if m.Role == "system" {
			t.Fatal("system 消息不应出现在 messages 数组中")
		}
	}
}

// 上游错误应转为 TypeError 并带上状态码。
func TestAnthropicRunnerUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	r := NewAnthropicRunner(srv.URL, "bad", "m", "")
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
		t.Fatalf("错误信息应含状态码: %v", gotErr)
	}
}

// 流内 error 事件应被识别。
func TestAnthropicRunnerStreamErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"服务过载\"}}\n\n")
	}))
	defer srv.Close()

	r := NewAnthropicRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotErr error
	for ev := range r.Run(ctx, Request{History: historyWith("x")}) {
		if ev.Type == TypeError {
			gotErr = ev.Err
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "服务过载") {
		t.Fatalf("应上报流内错误信息，实际: %v", gotErr)
	}
}

// 空响应应报错，避免前端显示空气泡。
func TestAnthropicRunnerEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	r := NewAnthropicRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotErr error
	for ev := range r.Run(ctx, Request{History: historyWith("x")}) {
		if ev.Type == TypeError {
			gotErr = ev.Err
		}
	}
	if gotErr == nil {
		t.Fatal("空响应应报错")
	}
}

// ctx 取消应中断上游请求。
func TestAnthropicRunnerCancel(t *testing.T) {
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

	r := NewAnthropicRunner(srv.URL, "k", "m", "")
	ctx, cancel := context.WithCancel(context.Background())

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for range r.Run(ctx, Request{History: historyWith("x")}) {
		}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后未退出")
	}
}

// 两个 Runner 都必须满足接口契约。
func TestBothRunnersImplementInterface(t *testing.T) {
	var _ Runner = NewLLMRunner("http://x/v1", "k", "m", "")
	var _ Runner = NewAnthropicRunner("http://x/v1", "k", "m", "")
	var _ Named = NewLLMRunner("http://x/v1", "k", "m", "")
	var _ Named = NewAnthropicRunner("http://x/v1", "k", "m", "")
}
