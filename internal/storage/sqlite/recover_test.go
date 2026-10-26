package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent_rebuild/internal/session"
)

// TestRecoverStaleRunningSQLite 复现线上问题：
// 进程崩溃后数据库残留 running，前端一直提示"仍在生成中"。
func TestRecoverStaleRunningSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recover.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	store := NewSessionStore(db, 100)
	ctx := context.Background()

	// 手动写入三条不同状态的会话（模拟崩溃现场）。
	insert := func(id string, st session.SessionStatus) {
		s := &session.Session{
			ID: id, Title: "t-" + id, Status: st, Model: "m",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("创建会话 %s 失败: %v", id, err)
		}
	}
	insert("idle-1", session.SessionIdle)
	insert("run-1", session.SessionRunning)
	insert("run-2", session.SessionRunning)

	n, err := store.RecoverStaleRunning(ctx)
	if err != nil {
		t.Fatalf("RecoverStaleRunning 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应复位 2 条，实际 %d", n)
	}

	for _, id := range []string{"idle-1", "run-1", "run-2"} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", id, err)
		}
		if got.Status != session.SessionIdle {
			t.Errorf("%s 状态应为 idle，实际 %q", id, got.Status)
		}
	}
}

// TestRecoverStaleRunningPersisted 复位结果必须落盘，重开数据库仍是 idle。
func TestRecoverStaleRunningPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	store := NewSessionStore(db, 100)
	ctx := context.Background()

	if err := store.Create(ctx, &session.Session{
		ID: "s1", Title: "t", Status: session.SessionRunning, Model: "m",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := store.RecoverStaleRunning(ctx); err != nil {
		t.Fatalf("复位失败: %v", err)
	}
	_ = db.Close()

	// 重开（模拟下次启动）
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer db2.Close()
	store2 := NewSessionStore(db2, 100)

	got, err := store2.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Status != session.SessionIdle {
		t.Fatalf("复位未落盘，状态仍为 %q", got.Status)
	}
}

// TestRecoverStaleRunningNoop 没有 running 时返回 0 且不报错。
func TestRecoverStaleRunningNoop(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "noop.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer db.Close()
	store := NewSessionStore(db, 100)
	ctx := context.Background()

	if err := store.Create(ctx, &session.Session{
		ID: "s1", Title: "t", Status: session.SessionIdle,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	n, err := store.RecoverStaleRunning(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("应复位 0 条，实际 %d", n)
	}
}
