package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/sse"
)

// 断线后按 last_event_id 补齐：重放缓冲事件并接上实时流。
func TestStreamReplayFromLastEventID(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	// 先跑完一次生成，事件进入 hub 缓冲。
	resp := env.startStream(t, sess.ID, map[string]any{"content": "第一轮"})
	frames := readSSE(t, resp.Body)
	resp.Body.Close()
	if len(frames) < 3 {
		t.Fatalf("首轮事件过少: %d", len(frames))
	}

	// 从第 1 号事件之后补齐：应重放 2..N。
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/sessions/%s/stream?last_event_id=1", env.url, sess.ID), nil)
	req.Header.Set("Accept", "text/event-stream")
	sub, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer sub.Body.Close()

	if sub.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，期望 200", sub.StatusCode)
	}
	if ct := sub.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	replayed, err := readSSEFrames(sub.Body, -1)
	if err != nil {
		t.Fatalf("读取重放流: %v", err)
	}

	// 重放的事件 id 必须严格大于 1，且与首轮一致。
	first := frames[0].ID
	if first != 1 {
		t.Fatalf("首轮首个事件 id = %d，期望 1", first)
	}
	if len(replayed) == 0 {
		t.Fatal("没有重放到任何事件")
	}
	for _, f := range replayed {
		if f.ID <= 1 {
			t.Fatalf("重放了不该重放的事件 id=%d", f.ID)
		}
	}
	if replayed[len(replayed)-1].Name != sse.EventMessageDone {
		t.Fatalf("重放末事件 = %q，期望 done", replayed[len(replayed)-1].Name)
	}
}

// 缓冲已裁剪时（last_event_id 过旧）应改为发 resync + 全量快照。
func TestStreamResyncWhenTrimmed(t *testing.T) {
	// 缓冲压到很小以制造裁剪（必须在构造前指定，executor 与 hub 要共用同一实例）。
	env := newTestEnvWithHub(t, 2)
	sess := env.createSession(t, "")

	resp := env.startStream(t, sess.ID, map[string]any{"content": "产生较多事件"})
	readSSE(t, resp.Body)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/sessions/%s/stream?last_event_id=0", env.url, sess.ID), nil)
	sub, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer sub.Body.Close()

	frames, err := readSSEFrames(sub.Body, 1)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("没有收到任何帧")
	}
	if frames[0].Name != sse.EventResync {
		t.Fatalf("首帧 = %q，期望 resync（缓冲已裁剪）", frames[0].Name)
	}
	if !strings.Contains(frames[0].Data, "messages") {
		t.Fatalf("resync 负载应含 messages 快照: %s", frames[0].Data)
	}
}

// last_event_id 非法应 400。
func TestStreamInvalidLastEventID(t *testing.T) {
	env := newTestEnv(t)
	sess := env.createSession(t, "")

	var body ErrorBody
	env.getJSON(t, "/api/sessions/"+sess.ID+"/stream?last_event_id=abc", http.StatusBadRequest, &body)
	if body.Error.Code != string(CodeInvalidArgument) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

func TestStreamUnknownSession(t *testing.T) {
	env := newTestEnv(t)
	var body ErrorBody
	env.getJSON(t, "/api/sessions/sess_missing/stream", http.StatusNotFound, &body)
	if body.Error.Code != string(CodeSessionNotFound) {
		t.Fatalf("error.code = %q", body.Error.Code)
	}
}

// 生成中订阅：能实时收到后续 delta 与 done。
func TestStreamLiveWhileGenerating(t *testing.T) {
	env := newTestEnv(t)
	env.server.cfg.SSEPingInterval = time.Hour
	sess := env.createSession(t, "")

	// 先在后台发起生成（用较慢节奏，给订阅留出窗口）。
	started := make(chan struct{})
	var once sync.Once
	env.server.runner = newSlowRunner(started)

	go func() {
		resp := env.startStream(t, sess.ID, map[string]any{"content": "实时订阅"})
		defer resp.Body.Close()
		readSSE(t, resp.Body)
	}()

	<-started // 等生成开始（start 事件已发布）

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/sessions/%s/stream?last_event_id=0", env.url, sess.ID), nil)
	sub, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer sub.Body.Close()

	frames, err := readSSEFrames(sub.Body, -1)
	if err != nil {
		t.Fatalf("读取实时流: %v", err)
	}
	names := frameNames(frames)
	if len(names) == 0 {
		t.Fatal("订阅未收到任何事件")
	}
	hasDone := false
	for _, n := range names {
		if n == sse.EventMessageDone {
			hasDone = true
		}
	}
	if !hasDone {
		t.Fatalf("实时订阅未收到 done，收到: %v", names)
	}
	once.Do(func() {})
}

// newSlowRunner 是包装 MockRunner 的可控执行器：首个 delta 前关闭 started 信号。
func newSlowRunner(started chan<- struct{}) *notifyingRunner {
	return &notifyingRunner{started: started, delay: 30 * time.Millisecond}
}

type notifyingRunner struct {
	started chan<- struct{}
	delay   time.Duration
}

func (r *notifyingRunner) Run(ctx context.Context, req agent.Request) <-chan agent.Event {
	out := make(chan agent.Event, 16)
	go func() {
		defer close(out)
		select {
		case <-ctx.Done():
			return
		case out <- agent.Event{Type: agent.TypeDelta, Delta: "第一块"}:
		}
		r.started <- struct{}{}

		for i := 0; i < 3; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.delay):
			}
			select {
			case <-ctx.Done():
				return
			case out <- agent.Event{Type: agent.TypeDelta, Delta: "后续块"}:
			}
		}
		select {
		case <-ctx.Done():
		case out <- agent.Event{Type: agent.TypeDone, Content: "第一块后续块后续块后续块",
			Usage: &agent.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}:
		}
	}()
	return out
}
