package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/sse"
)

// failingRunner 是必然失败的执行器，用于验证 message.error 路径。
type failingRunner struct{}

func (failingRunner) Run(ctx context.Context, _ agent.Request) <-chan agent.Event {
	out := make(chan agent.Event, 1)
	go func() {
		defer close(out)
		select {
		case <-ctx.Done():
		case out <- agent.Event{Type: agent.TypeError, Err: fmt.Errorf("上游模型不可用（测试注入）")}:
		}
	}()
	return out
}

// sseFrame 是测试侧解析出的单帧。
type sseFrame struct {
	ID    uint64
	Name  string
	Data  string
	Retry int
}

// readSSE 同步读完一个 SSE 响应，返回全部事件帧（纯 retry 帧不计入）。
func readSSE(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	frames, err := readSSEFrames(body, -1)
	if err != nil {
		t.Fatalf("读取 SSE 失败: %v", err)
	}
	return frames
}

// readSSERaw 与 readSSE 相同，但同时返回原始响应文本，
// 用于断言 retry 指令这类「非事件字段」。
func readSSERaw(t *testing.T, body io.Reader) ([]sseFrame, string) {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("读取 SSE 失败: %v", err)
	}
	frames, err := readSSEFrames(strings.NewReader(string(raw)), -1)
	if err != nil {
		t.Fatalf("解析 SSE 失败: %v", err)
	}
	return frames, string(raw)
}

// readSSEFrames 读取最多 n 帧（n<0 表示读到底）。
//
// 严格按 WHATWG 规范解析：空行分帧，忽略注释行，处理 "field: value"。
func readSSEFrames(body io.Reader, n int) ([]sseFrame, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		frames  []sseFrame
		current sseFrame
		data    []string
	)
	flush := func() bool {
		// 纯 retry 帧（只有 retry: N，没有 event/data/id）不是业务事件，不作为帧返回；
		// retry 语义由 TestSSERetryDirective 单独断言。
		if current.Name == "" && len(data) == 0 {
			current = sseFrame{}
			data = nil
			return false
		}
		current.Data = strings.Join(data, "\n")
		frames = append(frames, current)
		current = sseFrame{}
		data = nil
		return true
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if flush() && n > 0 && len(frames) >= n {
				return frames, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // 注释行（探活/心跳注释）
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			fmt.Sscanf(value, "%d", &current.ID)
		case "event":
			current.Name = value
		case "data":
			data = append(data, value)
		case "retry":
			fmt.Sscanf(value, "%d", &current.Retry)
		}
	}
	if err := scanner.Err(); err != nil {
		return frames, err
	}
	flush()
	return frames, nil
}

// frameNames 提取帧名序列，便于断言顺序。
func frameNames(frames []sseFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if f.Name != "" {
			out = append(out, f.Name)
		}
	}
	return out
}

// startStream 发起 POST /messages 并返回响应（不读取 body）。
func (e *testEnv) startStream(t *testing.T, sessionID string, payload map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, e.url+"/api/sessions/"+sessionID+"/messages", bytesReader(raw))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST messages: %v", err)
	}
	return resp
}

// ---- 核心：事件序列 ----

func TestSendMessageSSEEventSequence(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "你好，做个自我介绍"})
	defer resp.Body.Close()

	// 1) 响应头必须是 SSE，且带齐反代理/缓存相关头。
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("Cache-Control = %q，期望包含 no-cache", cc)
	}
	if cn := resp.Header.Get("Connection"); cn != "keep-alive" {
		t.Fatalf("Connection = %q，期望 keep-alive", cn)
	}
	if ab := resp.Header.Get("X-Accel-Buffering"); ab != "no" {
		t.Fatalf("X-Accel-Buffering = %q，期望 no", ab)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("SSE 响应缺少 X-Request-Id")
	}

	// rawBody 需要保留原始文本才能验证 retry 指令（纯 retry 帧不是事件帧）。
	frames, rawBody := readSSERaw(t, resp.Body)
	names := frameNames(frames)
	if len(names) < 3 {
		t.Fatalf("事件过少: %v", names)
	}
	if names[0] != sse.EventMessageStart {
		t.Fatalf("首个事件 = %q，期望 %q", names[0], sse.EventMessageStart)
	}
	if names[len(names)-1] != sse.EventMessageDone {
		t.Fatalf("末个事件 = %q，期望 %q", names[len(names)-1], sse.EventMessageDone)
	}
	deltaCount := 0
	for i, n := range names {
		if n == sse.EventMessageDelta {
			deltaCount++
			if i == 0 || i == len(names)-1 {
				t.Fatalf("delta 不应出现在首尾: %v", names)
			}
		}
	}
	if deltaCount < 2 {
		t.Fatalf("delta 数量 = %d，期望 >= 2（Mock 分块输出）", deltaCount)
	}
	if strings.Contains(strings.Join(names, ","), sse.EventMessageError) {
		t.Fatalf("正常流程不应出现 error 事件: %v", names)
	}

	// 3) retry 必须在首个事件前声明。
	retryLine := fmt.Sprintf("retry: %d", sseRetryMillis)
	if !strings.Contains(rawBody, retryLine) {
		t.Fatalf("缺少 %q 指令\n原始响应:\n%s", retryLine, rawBody)
	}
	if idx := strings.Index(rawBody, retryLine); idx > strings.Index(rawBody, "event:") {
		t.Fatalf("retry 应出现在首个事件之前\n原始响应:\n%s", rawBody)
	}

	// 4) id 必须单调递增，且与负载里的 seq 完全一致。
	var prev uint64
	startMsgID := ""
	var streamed strings.Builder
	var done sse.MessageDoneData
	for _, f := range frames {
		if f.ID <= prev && f.Name != "" {
			t.Fatalf("事件 id 未单调递增: %d <= %d（event=%s）", f.ID, prev, f.Name)
		}
		prev = f.ID

		switch f.Name {
		case sse.EventMessageStart:
			var d sse.MessageStartData
			if err := json.Unmarshal([]byte(f.Data), &d); err != nil {
				t.Fatalf("解析 start: %v", err)
			}
			if d.Seq != f.ID {
				t.Fatalf("start 负载 seq=%d 与帧 id=%d 不一致", d.Seq, f.ID)
			}
			if d.MessageID == "" || d.SessionID != sess.ID {
				t.Fatalf("start 负载字段缺失: %+v", d)
			}
			startMsgID = d.MessageID
		case sse.EventMessageDelta:
			var d sse.MessageDeltaData
			if err := json.Unmarshal([]byte(f.Data), &d); err != nil {
				t.Fatalf("解析 delta: %v", err)
			}
			if d.Seq != f.ID {
				t.Fatalf("delta 负载 seq=%d 与帧 id=%d 不一致", d.Seq, f.ID)
			}
			if d.MessageID != startMsgID {
				t.Fatalf("delta 的 message_id=%q 与 start 的 %q 不一致", d.MessageID, startMsgID)
			}
			if d.Delta == "" {
				t.Fatal("delta 内容不应为空")
			}
			streamed.WriteString(d.Delta)
		case sse.EventMessageDone:
			if err := json.Unmarshal([]byte(f.Data), &done); err != nil {
				t.Fatalf("解析 done: %v", err)
			}
			if done.Seq != f.ID {
				t.Fatalf("done 负载 seq=%d 与帧 id=%d 不一致", done.Seq, f.ID)
			}
		}
	}

	// 5) done 的完整文本必须等于所有 delta 拼接（前端增量渲染的一致性前提）。
	if done.Content != streamed.String() {
		t.Fatalf("done.content 与 delta 拼接不一致\ndone: %q\ndeltas: %q", done.Content, streamed.String())
	}
	if done.MessageID != startMsgID {
		t.Fatalf("done 的 message_id 不匹配")
	}
	if done.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q，期望 stop", done.FinishReason)
	}
	if done.Usage == nil || done.Usage.TotalTokens == 0 {
		t.Fatalf("done 缺少 usage: %+v", done.Usage)
	}
}

// ---- 落库一致性 ----

func TestSendMessagePersistsMessages(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "持久化测试"})
	frames := readSSE(t, resp.Body)
	resp.Body.Close()
	if len(frames) == 0 {
		t.Fatal("没有收到任何事件")
	}

	var detail sessionDetailResponse
	env.getJSON(t, "/api/sessions/"+sess.ID, http.StatusOK, &detail)

	if len(detail.Messages) != 2 {
		t.Fatalf("消息数 = %d，期望 2（user + assistant）", len(detail.Messages))
	}
	if detail.Messages[0].Role != "user" || detail.Messages[0].Content != "持久化测试" {
		t.Fatalf("用户消息错误: %+v", detail.Messages[0])
	}
	assistant := detail.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("第二条应为 assistant: %+v", assistant)
	}
	if assistant.Status != "complete" {
		t.Fatalf("assistant 状态 = %q，期望 complete", assistant.Status)
	}
	if assistant.Content == "" {
		t.Fatal("assistant 内容不应为空")
	}
	if assistant.Usage == nil || assistant.Usage.TotalTokens == 0 {
		t.Fatalf("assistant 缺少 usage: %+v", assistant.Usage)
	}
	if detail.Session.MessageCount != 2 {
		t.Fatalf("MessageCount = %d，期望 2", detail.Session.MessageCount)
	}
	if detail.Session.Status != "idle" {
		t.Fatalf("会话状态 = %q，生成结束后应为 idle", detail.Session.Status)
	}
	// last_event_id 应等于事件流的最大 id。
	if detail.LastEventID != frames[len(frames)-1].ID {
		t.Fatalf("last_event_id = %d，期望 %d", detail.LastEventID, frames[len(frames)-1].ID)
	}
}

// 首条提问应把默认标题替换为提问内容。
func TestFirstMessageDerivesTitle(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "帮我设计一个 SSE 协议"})
	readSSE(t, resp.Body)
	resp.Body.Close()

	var detail sessionDetailResponse
	env.getJSON(t, "/api/sessions/"+sess.ID, http.StatusOK, &detail)
	if detail.Session.Title != "帮我设计一个 SSE 协议" {
		t.Fatalf("标题 = %q，期望由首条提问推导", detail.Session.Title)
	}
}

// ---- 入参校验 ----

func TestSendMessageValidation(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	var body ErrorBody
	env.postJSON(t, "/api/sessions/"+sess.ID+"/messages",
		map[string]any{"content": "   "}, http.StatusBadRequest, &body)
	if body.Error.Code != string(CodeInvalidArgument) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestSendMessageUnknownSession(t *testing.T) {
	env := newTestEnv(t)
	var body ErrorBody
	env.postJSON(t, "/api/sessions/sess_missing/messages",
		map[string]any{"content": "hi"}, http.StatusNotFound, &body)
	if body.Error.Code != string(CodeSessionNotFound) {
		t.Fatalf("error.code = %q，期望 session_not_found", body.Error.Code)
	}
}

// ---- 非流式退化 ----

func TestSendMessageNonStream(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	var out sendMessageResponse
	env.postJSON(t, "/api/sessions/"+sess.ID+"/messages",
		map[string]any{"content": "非流式", "stream": false}, http.StatusOK, &out)

	if out.AssistantMessage == nil || out.UserMessage == nil {
		t.Fatalf("响应缺少消息: %+v", out)
	}
	if out.AssistantMessage.Content == "" || out.AssistantMessage.Status != "complete" {
		t.Fatalf("assistant 结果错误: %+v", out.AssistantMessage)
	}
}

// ---- 并发保护 ----

// 同一会话正在生成时再次发送应返回 409，而不是排队阻塞。
func TestSendMessageConcurrentConflict(t *testing.T) {
	env := newTestEnvWithRunner(t, func(r *agent.MockRunner) {
		r.Chunk = 20 * time.Millisecond // 拉长生成时间，制造并发窗口
	})
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "第一次"})
	defer resp.Body.Close()

	// 等到流建立（start 事件已到）再并发发送。
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("读取首行失败: %v", err)
	}

	var body ErrorBody
	env.postJSON(t, "/api/sessions/"+sess.ID+"/messages",
		map[string]any{"content": "第二次"}, http.StatusConflict, &body)
	if body.Error.Code != string(CodeSessionBusy) {
		t.Fatalf("error.code = %q，期望 session_busy", body.Error.Code)
	}

	// 第一次仍应正常结束。
	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), "event: "+sse.EventMessageDone) {
		t.Fatalf("第一次生成未正常完成: %s", string(rest))
	}
	_ = reader
}

// ---- 客户端断开 ----

// 客户端中途关闭连接：上游被取消，已生成内容以 interrupted 落库。
func TestClientDisconnectMarksInterrupted(t *testing.T) {
	env := newTestEnvWithRunner(t, func(r *agent.MockRunner) {
		r.Chunk = 15 * time.Millisecond
	})
	sess := env.createSession(t, "")

	req, _ := http.NewRequest(http.MethodPost, env.url+"/api/sessions/"+sess.ID+"/messages",
		bytesReader(mustJSON(t, map[string]any{"content": "断开测试，请输出较长内容"})))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	// 只读前几帧就断开。
	frames, _ := readSSEFrames(resp.Body, 4)
	if len(frames) < 3 {
		t.Fatalf("断开前应至少收到 3 帧，实际 %d", len(frames))
	}
	_ = resp.Body.Close()

	// ★ 新架构的关键行为：客户端断开**不会**中止执行。
	// 任务在后台继续跑完，最终落库为 complete（而不是 interrupted）。
	deadline := time.Now().Add(10 * time.Second)
	var status session.MessageStatus
	var content string
	for time.Now().Before(deadline) {
		msgs, msgErr := env.store.Messages(context.Background(), sess.ID)
		if msgErr == nil && len(msgs) >= 2 {
			status = msgs[len(msgs)-1].Status
			content = msgs[len(msgs)-1].Content
			// 终态：complete（跑完）/ interrupted / error
			if status == session.StatusComplete || status == session.StatusInterrupted ||
				status == session.StatusError {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if status != session.StatusComplete {
		t.Fatalf("断开后 assistant 状态 = %q，期望 complete（任务应在后台跑完）", status)
	}
	if content == "" {
		t.Fatal("断开后任务继续执行，应产出完整内容")
	}

	// 关键验证：断开后写入的内容，可通过 /stream 补齐（断线重连语义）。
	streamResp, err := http.Get(env.url + "/api/sessions/" + sess.ID + "/stream?last_event_id=0")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer streamResp.Body.Close()
	replayed, _ := readSSEFrames(streamResp.Body, -1)
	var replayedContent strings.Builder
	for _, f := range replayed {
		if f.Name == sse.EventMessageDelta {
			var d sse.MessageDeltaData
			if json.Unmarshal([]byte(f.Data), &d) == nil {
				replayedContent.WriteString(d.Delta)
			}
		}
	}
	if replayedContent.Len() == 0 {
		t.Fatal("断线重连应能补齐断开期间产生的事件")
	}

	// 锁必须已释放：可以再次发送。
	resp2 := env.startStream(t, sess.ID, map[string]any{"content": "再来一次"})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("断开后重新发送 status = %d，期望 200（锁已释放）", resp2.StatusCode)
	}
	readSSE(t, resp2.Body)
}

// 显式取消仍应立即中断（与「连接断开」区分开）。
func TestCancelStillInterrupts(t *testing.T) {
	env := newTestEnvWithRunner(t, func(r *agent.MockRunner) {
		r.Chunk = 20 * time.Millisecond
	})
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "取消测试，输出长内容"})
	defer resp.Body.Close()

	// 等首帧后取消。
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("读取首行: %v", err)
	}

	var cancelResp map[string]bool
	env.postJSON(t, "/api/sessions/"+sess.ID+"/cancel", nil, http.StatusAccepted, &cancelResp)
	if !cancelResp["canceled"] {
		t.Fatal("canceled 应为 true")
	}

	// 显式取消必须真的中断，落库为 interrupted。
	deadline := time.Now().Add(5 * time.Second)
	var status session.MessageStatus
	for time.Now().Before(deadline) {
		msgs, err := env.store.Messages(context.Background(), sess.ID)
		if err == nil && len(msgs) >= 2 {
			status = msgs[len(msgs)-1].Status
			if status == session.StatusInterrupted || status == session.StatusComplete {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != session.StatusInterrupted {
		t.Fatalf("显式取消后状态 = %q，期望 interrupted", status)
	}
}

// ---- 心跳 ----

// SSE_PING_INTERVAL 配置足够小时，长生成过程中应能观察到 ping 事件。
func TestStreamPingHeartbeat(t *testing.T) {
	env := newTestEnvWithRunner(t, func(r *agent.MockRunner) {
		r.Chunk = 25 * time.Millisecond
	})
	env.server.cfg.SSEPingInterval = 10 * time.Millisecond

	sess := env.createSession(t, "")
	resp := env.startStream(t, sess.ID, map[string]any{"content": "触发心跳"})
	defer resp.Body.Close()

	frames := readSSE(t, resp.Body)
	names := frameNames(frames)

	pings := 0
	for _, n := range names {
		if n == sse.EventPing {
			pings++
		}
	}
	if pings == 0 {
		t.Fatalf("未观察到 ping 心跳，事件序列: %v", names)
	}
	// ping 不应打断主序列。
	if names[0] != sse.EventMessageStart || names[len(names)-1] != sse.EventMessageDone {
		t.Fatalf("心跳后主序列异常: %v", names)
	}
}

// ---- 执行层出错 ----

// 注入一个必然失败的 Runner，验证 message.error 事件与落库状态。
func TestSendMessageRunnerError(t *testing.T) {
	env := newTestEnv(t)
	env.server.runner = failingRunner{}
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "会失败"})
	defer resp.Body.Close()

	frames := readSSE(t, resp.Body)
	names := frameNames(frames)
	if len(names) == 0 || names[0] != sse.EventMessageStart {
		t.Fatalf("事件序列异常: %v", names)
	}
	if names[len(names)-1] != sse.EventMessageError {
		t.Fatalf("末事件 = %q，期望 message.error（序列 %v）", names[len(names)-1], names)
	}

	var errData sse.MessageErrorData
	for _, f := range frames {
		if f.Name == sse.EventMessageError {
			if err := json.Unmarshal([]byte(f.Data), &errData); err != nil {
				t.Fatalf("解析 error 负载: %v", err)
			}
		}
	}
	if errData.Error == nil || errData.Error.Message == "" {
		t.Fatalf("error 负载缺少信息: %+v", errData)
	}

	// 落库：消息为 error，会话为 error。
	msgs, err := env.store.Messages(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	last := msgs[len(msgs)-1]
	if last.Status != "error" {
		t.Fatalf("消息状态 = %q，期望 error", last.Status)
	}
	if last.Error == nil {
		t.Fatal("消息应带 error 详情")
	}
	got, _ := env.store.Get(context.Background(), sess.ID)
	if got.Status != "error" {
		t.Fatalf("会话状态 = %q，期望 error", got.Status)
	}
}

// ---- 取消接口 ----

func TestCancelGeneratingMessage(t *testing.T) {
	env := newTestEnvWithRunner(t, func(r *agent.MockRunner) {
		r.Chunk = 25 * time.Millisecond
	})
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "取消测试，请输出很长很长内容"})
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("读取首行: %v", err)
	}

	var cancelResp map[string]bool
	env.postJSON(t, "/api/sessions/"+sess.ID+"/cancel", nil, http.StatusAccepted, &cancelResp)
	if !cancelResp["canceled"] {
		t.Fatalf("canceled = %v，期望 true", cancelResp["canceled"])
	}

	// 流应以 interrupted 结束，而不是 done。
	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), "event: "+sse.EventMessageInterrupted) {
		t.Fatalf("流中未出现 message.interrupted: %s", string(rest))
	}

	// 重复取消是幂等的。
	var second map[string]bool
	env.postJSON(t, "/api/sessions/"+sess.ID+"/cancel", nil, http.StatusOK, &second)
	if second["canceled"] {
		t.Fatal("无运行中生成时应返回 canceled=false")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}
