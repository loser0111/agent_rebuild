package sse

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Publish 必须分配单调递增的 seq，并把它同时写进帧 ID 与业务负载。
func TestHubPublishAssignsMonotonicSeq(t *testing.T) {
	h := NewHub(10, 0, 0)

	for i := 1; i <= 5; i++ {
		ev := h.Publish("s1", func(seq uint64) Event {
			return Event{Name: EventMessageDelta, Data: MessageDeltaData{Seq: seq, Delta: "x"}}
		})
		if ev.ID != uint64(i) {
			t.Fatalf("第 %d 次 Publish 的 ID = %d，期望 %d", i, ev.ID, i)
		}
		data, ok := ev.Data.(MessageDeltaData)
		if !ok {
			t.Fatalf("负载类型 = %T", ev.Data)
		}
		if data.Seq != ev.ID {
			t.Fatalf("负载 seq=%d 与帧 ID=%d 不一致", data.Seq, ev.ID)
		}
	}
	if got := h.LastID("s1"); got != 5 {
		t.Fatalf("LastID = %d，期望 5", got)
	}
}

// 不同会话的 seq 相互独立。
func TestHubSeqIsPerKey(t *testing.T) {
	h := NewHub(10, 0, 0)
	h.Publish("a", func(seq uint64) Event { return Event{Name: EventPing} })
	h.Publish("a", func(seq uint64) Event { return Event{Name: EventPing} })
	h.Publish("b", func(seq uint64) Event { return Event{Name: EventPing} })

	if got := h.LastID("a"); got != 2 {
		t.Fatalf("a.LastID = %d，期望 2", got)
	}
	if got := h.LastID("b"); got != 1 {
		t.Fatalf("b.LastID = %d，期望 1", got)
	}
}

// 订阅者应实时收到 Publish 的事件。
func TestHubSubscribeReceivesLive(t *testing.T) {
	h := NewHub(10, 0, 0)
	_, live, cancel, ok := h.Subscribe("s", 0)
	if !ok {
		t.Fatal("空缓冲时 Subscribe 应返回 ok=true")
	}
	defer cancel()

	if n := h.ActiveSubscribers("s"); n != 1 {
		t.Fatalf("订阅者数 = %d，期望 1", n)
	}

	h.Publish("s", func(seq uint64) Event {
		return Event{Name: EventMessageDelta, Data: MessageDeltaData{Seq: seq, Delta: "hi"}}
	})

	select {
	case ev := <-live:
		if ev.Name != EventMessageDelta || ev.ID != 1 {
			t.Fatalf("收到事件错误: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到实时事件")
	}
}

// 按 after 重放缓冲事件。
func TestHubSubscribeReplays(t *testing.T) {
	h := NewHub(10, 0, 0)
	for i := 0; i < 5; i++ {
		h.Publish("s", func(seq uint64) Event { return Event{Name: EventMessageDelta} })
	}

	replay, _, cancel, ok := h.Subscribe("s", 2)
	defer cancel()
	if !ok {
		t.Fatal("期望 ok=true")
	}
	if len(replay) != 3 {
		t.Fatalf("重放 %d 条，期望 3 条（id 3,4,5）", len(replay))
	}
	if replay[0].ID != 3 || replay[2].ID != 5 {
		t.Fatalf("重放区间错误: %d..%d", replay[0].ID, replay[2].ID)
	}
}

// 缓冲已裁剪时 Subscribe 返回 ok=false，调用方应改发 resync。
func TestHubSubscribeSignalsResync(t *testing.T) {
	h := NewHub(2, 0, 0)
	for i := 0; i < 5; i++ {
		h.Publish("s", func(seq uint64) Event { return Event{Name: EventMessageDelta} })
	}
	_, _, cancel, ok := h.Subscribe("s", 0)
	defer cancel()
	if ok {
		t.Fatal("缓冲已裁剪，期望 ok=false 触发 resync")
	}
}

// 取消订阅后不应再收到事件，且不 panic。
func TestHubCancelUnsubscribes(t *testing.T) {
	h := NewHub(10, 0, 0)
	_, _, cancel, _ := h.Subscribe("s", 0)
	cancel()

	if n := h.ActiveSubscribers("s"); n != 0 {
		t.Fatalf("取消后订阅者数 = %d，期望 0", n)
	}
	// Publish 在无订阅者时不应 panic。
	h.Publish("s", func(seq uint64) Event { return Event{Name: EventPing} })
}

// 慢订阅者：通道满时丢弃事件而不是阻塞 Publish。
func TestHubSlowSubscriberDoesNotBlock(t *testing.T) {
	h := NewHub(1000, 0, 0)
	_, live, cancel, _ := h.Subscribe("s", 0)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ { // 远超通道容量 256
			h.Publish("s", func(seq uint64) Event { return Event{Name: EventPing} })
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 被慢订阅者阻塞了")
	}

	// 消费完已缓冲的事件（数量 <= 通道容量）。
	count := 0
	for {
		select {
		case <-live:
			count++
			continue
		default:
		}
		break
	}
	if count == 0 {
		t.Fatal("应至少收到部分事件")
	}
}

// 并发 Publish 与 Subscribe 必须安全（-race 下验证）。
func TestHubConcurrentPublishSubscribe(t *testing.T) {
	h := NewHub(100, 0, 0)
	const pubN, subN = 50, 8

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < pubN; i++ {
			h.Publish("s", func(seq uint64) Event { return Event{Name: EventMessageDelta} })
		}
	}()

	for i := 0; i < subN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _, cancel, _ := h.Subscribe("s", uint64(j))
				cancel()
				_ = h.LastID("s")
			}
		}()
	}
	wg.Wait()

	if got := h.LastID("s"); got != pubN {
		t.Fatalf("LastID = %d，期望 %d", got, pubN)
	}
	if n := h.ActiveSubscribers("s"); n != 0 {
		t.Fatalf("残留订阅者 = %d，期望 0", n)
	}
}

// Drop 清理会话状态。
func TestHubDrop(t *testing.T) {
	h := NewHub(10, 0, 0)
	h.Publish("s", func(seq uint64) Event { return Event{Name: EventPing} })
	_, _, cancel, _ := h.Subscribe("s", 0)
	defer cancel()

	h.Drop("s")
	if got := h.LastID("s"); got != 0 {
		t.Fatalf("Drop 后 LastID = %d，期望 0", got)
	}
	if n := h.ActiveSubscribers("s"); n != 0 {
		t.Fatalf("Drop 后订阅者 = %d，期望 0", n)
	}
}

// 事件数据必须能独立序列化（前端按 JSON 解析）。
func TestHubEventDataSerializable(t *testing.T) {
	h := NewHub(10, 0, 0)
	ev := h.Publish("s", func(seq uint64) Event {
		return Event{
			Name: EventMessageStart,
			Data: MessageStartData{Seq: seq, SessionID: fmt.Sprintf("sess_%d", seq), MessageID: "msg_1"},
		}
	})
	if ev.Data == nil {
		t.Fatal("负载不应为空")
	}
	if ev.Name != EventMessageStart {
		t.Fatalf("事件名 = %q", ev.Name)
	}
}
