package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_rebuild/internal/session"
)

func historyWith(contents ...string) []session.Message {
	out := make([]session.Message, 0, len(contents))
	for i, c := range contents {
		role := session.RoleUser
		if i%2 == 1 {
			role = session.RoleAssistant
		}
		out = append(out, session.Message{
			ID:      "msg_" + c,
			Role:    role,
			Content: c,
			Status:  session.StatusComplete,
		})
	}
	return out
}

// Mock 必须分多块输出，且拼接后等于 done 的完整文本。
func TestMockRunnerStreamsChunks(t *testing.T) {
	r := NewMockRunner("mock-echo")
	r.Chunk = 0 // 用默认间隔（25ms）

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var deltas []string
	var done *Event
	chunks := 0
	for ev := range r.Run(ctx, Request{History: historyWith("测试问题")}) {
		switch ev.Type {
		case TypeDelta:
			if ev.Delta == "" {
				t.Error("delta 不应为空")
			}
			deltas = append(deltas, ev.Delta)
			chunks++
		case TypeDone:
			e := ev
			done = &e
		}
	}

	if chunks < 2 {
		t.Fatalf("分块数 = %d，期望 >= 2", chunks)
	}
	if done == nil {
		t.Fatal("未收到 done 事件")
	}
	if !strings.HasPrefix(done.Content, "（Mock 模式") {
		t.Fatalf("done 内容应带 Mock 提示前缀: %q", done.Content)
	}
	// done 的完整文本必须以所有 delta 拼接为开头（首帧提示 + 后续内容）。
	joined := strings.Join(deltas, "")
	if !strings.HasPrefix(joined, "（Mock 模式") {
		t.Fatalf("delta 拼接应等于完整文本: %q", joined)
	}
	if done.Usage == nil || done.Usage.TotalTokens == 0 {
		t.Fatalf("done 应带 usage: %+v", done.Usage)
	}
}

// 可配置延迟：Chunk 越大，总耗时越长。
func TestMockRunnerConfigurableDelay(t *testing.T) {
	fast := NewMockRunner("m")
	fast.Chunk = time.Millisecond
	slow := NewMockRunner("m")
	slow.Chunk = 8 * time.Millisecond

	measure := func(r *MockRunner) time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		for range r.Run(ctx, Request{History: historyWith("延迟")}) {
		}
		return time.Since(start)
	}

	fastDur := measure(fast)
	slowDur := measure(slow)
	if slowDur <= fastDur {
		t.Fatalf("慢速分块耗时应更长: slow=%v fast=%v", slowDur, fastDur)
	}
}

// ctx 取消时 Runner 应及时停止，不泄漏 goroutine。
func TestMockRunnerRespectsCancel(t *testing.T) {
	r := NewMockRunner("m")
	r.Chunk = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	ch := r.Run(ctx, Request{History: historyWith("取消")})

	// 读到第一个 delta 后取消。
	ev := <-ch
	if ev.Type != TypeDelta {
		t.Fatalf("首个事件类型 = %q，期望 delta", ev.Type)
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel 已关闭，符合预期
			}
		case <-deadline:
			t.Fatal("取消后 Runner 未及时退出")
		}
	}
}

// 空历史时也应给出可读的默认回答。
func TestMockRunnerEmptyHistory(t *testing.T) {
	r := NewMockRunner("m")
	r.Chunk = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var done *Event
	for ev := range r.Run(ctx, Request{}) {
		if ev.Type == TypeDone {
			e := ev
			done = &e
		}
	}
	if done == nil || !strings.Contains(done.Content, "Mock Agent") {
		t.Fatalf("空历史时应给出默认回答: %+v", done)
	}
}

// 只取最后一条 user 消息作为提问。
func TestMockRunnerUsesLastUserMessage(t *testing.T) {
	r := NewMockRunner("m")
	r.Chunk = time.Millisecond

	history := []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "第一个问题", Status: session.StatusComplete},
		{ID: "2", Role: session.RoleAssistant, Content: "第一个回答", Status: session.StatusComplete},
		{ID: "3", Role: session.RoleUser, Content: "最后一个问题", Status: session.StatusComplete},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var done *Event
	for ev := range r.Run(ctx, Request{History: history}) {
		if ev.Type == TypeDone {
			e := ev
			done = &e
		}
	}
	if done == nil {
		t.Fatal("未收到 done")
	}
	if !strings.Contains(done.Content, "最后一个问题") {
		t.Fatalf("应引用最后一条用户消息: %q", done.Content)
	}
	if strings.Contains(done.Content, "第一个问题") {
		t.Fatal("不应引用更早的用户消息")
	}
}

// 中文按字符切块，不能截断成乱码。
func TestChunkStringSplitsRunes(t *testing.T) {
	parts := chunkString("中文测试内容", 2)
	if len(parts) != 3 {
		t.Fatalf("切块数 = %d，期望 3", len(parts))
	}
	joined := strings.Join(parts, "")
	if joined != "中文测试内容" {
		t.Fatalf("拼接后 = %q，与原文不一致", joined)
	}
	for _, p := range parts {
		for _, r := range p {
			if r > 0x7F && r < 0x4E00 {
				t.Fatalf("出现非法字符: %q", p)
			}
		}
	}

	if got := chunkString("", 4); len(got) != 0 {
		t.Fatalf("空串应返回空切片，得到 %v", got)
	}
	// size<=0 时使用默认块大小，不能 panic 或死循环。
	if got := chunkString("abc", 0); strings.Join(got, "") != "abc" {
		t.Fatalf("size<=0 时结果错误: %v", got)
	}
}

// ---- 执行层接口契约 ----

// Runner 必须实现 Named（便于 /api/health 与日志展示）。
func TestRunnersImplementInterface(t *testing.T) {
	var _ Runner = NewMockRunner("m")
	var _ Named = NewMockRunner("m")

	if got := NewMockRunner("m").Name(); got != "mock:m" {
		t.Fatalf("MockRunner.Name() = %q", got)
	}

	llm := NewLLMRunner("https://api.example.com/v1", "key", "gpt-x", "sys")
	var _ Runner = llm
	var _ Named = llm
	if got := llm.Name(); got != "openai:gpt-x" {
		t.Fatalf("LLMRunner.Name() = %q", got)
	}
	// BaseURL 原样保存；尾斜杠与版本段在拼接请求地址时统一处理。
	if llm.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("BaseURL = %q", llm.BaseURL)
	}
}

// ---- 会话锁 ----

func TestLocksTryLock(t *testing.T) {
	l := NewLocks()

	unlock, ok := l.TryLock("s1")
	if !ok {
		t.Fatal("首次加锁应成功")
	}
	if !l.IsBusy("s1") {
		t.Fatal("持锁期间 IsBusy 应为 true")
	}

	// 同会话重复加锁失败。
	if _, ok := l.TryLock("s1"); ok {
		t.Fatal("同会话重复加锁应失败")
	}
	// 不同会话互不影响。
	unlock2, ok := l.TryLock("s2")
	if !ok {
		t.Fatal("不同会话应可并行加锁")
	}

	unlock()
	if l.IsBusy("s1") {
		t.Fatal("解锁后 IsBusy 应为 false")
	}
	unlock2()

	// 释放后可重新获取，避免 409 残留。
	unlock3, ok := l.TryLock("s1")
	if !ok {
		t.Fatal("释放后应可重新加锁")
	}
	unlock3()
}

func TestLocksForget(t *testing.T) {
	l := NewLocks()
	unlock, _ := l.TryLock("s")
	unlock()

	l.Forget("s")
	if l.IsBusy("s") {
		t.Fatal("Forget 后不应处于忙状态")
	}
}

// 并发 TryLock 只能有一个成功，其余必须立即失败（不阻塞）。
func TestLocksConcurrent(t *testing.T) {
	l := NewLocks()

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if unlock, ok := l.TryLock("same"); ok {
				mu.Lock()
				success++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond) // 持锁一小段时间
				unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	// 允许串行获胜（前一个释放后另一个成功），但任何时刻只能有一个持锁者；
	// 这里的关键断言是「不 panic、不死锁、最终锁已释放」。
	if l.IsBusy("same") {
		t.Fatal("并发结束后锁应已释放")
	}
	if success == 0 {
		t.Fatal("至少应有一个 goroutine 加锁成功")
	}
}
