package sse

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriterEncodesFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}

	if err := w.WriteEvent(Event{
		ID:    7,
		Name:  EventMessageDelta,
		Data:  MessageDeltaData{Seq: 7, Delta: "你好\n世界"},
		Retry: 3000,
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	w.Close()

	body := rec.Body.String()
	// JSON 会把正文中的换行转义成 \n，因此 data 始终是单行。
	want := "retry: 3000\n" +
		"id: 7\n" +
		"event: message.delta\n" +
		"data: {\"seq\":7,\"session_id\":\"\",\"message_id\":\"\",\"delta\":\"你好\\n世界\"}\n\n"
	if body != want {
		t.Fatalf("帧内容不匹配\n得到: %q\n期望: %q", body, want)
	}
	if strings.Count(body, "data: ") != 1 {
		t.Fatalf("data 必须是单行，实际帧: %q", body)
	}
}

func TestBufferSinceAndTrim(t *testing.T) {
	b := NewBuffer(3)
	for i := uint64(1); i <= 5; i++ {
		b.Push(Event{ID: i, Name: EventPing})
	}
	if got := b.LastID(); got != 5 {
		t.Fatalf("LastID = %d, 期望 5", got)
	}

	events, ok := b.Since(4)
	if !ok || len(events) != 1 || events[0].ID != 5 {
		t.Fatalf("Since(4) = %v, ok=%v", events, ok)
	}

	// 缓冲只保留 3 条（3,4,5），请求 0 之后的全部应触发 resync 判断
	if _, ok := b.Since(0); ok {
		t.Fatal("Since(0) 期望 ok=false（已裁剪）")
	}
}
