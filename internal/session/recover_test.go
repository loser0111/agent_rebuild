package session

import (
	"context"
	"testing"
	"time"
)

// TestRecoverStaleRunningMemory 把卡住的 running 复位为 idle。
func TestRecoverStaleRunningMemory(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(100)

	// 造三条：idle / running / running
	ids := make(map[string]string) // 标记 → 实际 ID
	mk := func(mark string, st SessionStatus) {
		sess := NewSessionAt("s-"+mark, "mock", time.Now().UTC())
		sess.Status = st
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}
		ids[mark] = sess.ID
	}
	mk("a", SessionIdle)
	mk("b", SessionRunning)
	mk("c", SessionRunning)

	n, err := s.RecoverStaleRunning(ctx)
	if err != nil {
		t.Fatalf("RecoverStaleRunning 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应复位 2 条，实际 %d", n)
	}

	for mark, id := range ids {
		got, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", mark, err)
		}
		if got.Status != SessionIdle {
			t.Errorf("会话 %s 状态应为 idle，实际 %q", mark, got.Status)
		}
	}
}

// TestRecoverStaleRunningIdempotent 重复调用不应有副作用。
func TestRecoverStaleRunningIdempotent(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(100)

	sess := NewSessionAt("t", "mock", time.Now().UTC())
	sess.Status = SessionRunning
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	if n, _ := s.RecoverStaleRunning(ctx); n != 1 {
		t.Fatalf("第一次应复位 1 条，实际 %d", n)
	}
	if n, _ := s.RecoverStaleRunning(ctx); n != 0 {
		t.Fatalf("第二次应复位 0 条，实际 %d", n)
	}
}

// TestRecoverStaleRunningRespectsCancel ctx 取消时应返回错误。
func TestRecoverStaleRunningRespectsCancel(t *testing.T) {
	s := NewMemoryStore(100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.RecoverStaleRunning(ctx); err == nil {
		t.Fatal("ctx 已取消应返回错误")
	}
}
