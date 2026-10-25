package sqlite_test

// 验证 SQLite 实现的「持久化」这一内存实现不具备的能力。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
)

// 关掉数据库再重新打开，数据必须还在——这是选 SQLite 的根本理由。
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.db")
	ctx := context.Background()

	// 第一次打开：写入会话与消息。
	db1, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store1 := sqlite.NewSessionStore(db1, 0)

	sess := session.NewSession("持久化验证", "gpt-4o-mini")
	if err := store1.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	msg := session.NewUserMessage(sess.ID, "重启后我应该还在")
	if err := store1.AppendMessage(ctx, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	assistant := session.NewAssistantPlaceholder(sess.ID)
	if err := store1.AppendMessage(ctx, assistant); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := store1.UpdateMessage(ctx, sess.ID, assistant.ID, func(m *session.Message) error {
		m.Content = "回答内容"
		m.Status = session.StatusComplete
		m.Usage = &session.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
		return nil
	}); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	// 关键：先关闭连接（模拟进程退出）。
	if err := db1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 第二次打开：读取。
	db2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer func() { _ = db2.Close() }()
	store2 := sqlite.NewSessionStore(db2, 0)

	got, err := store2.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("重启后 Get: %v", err)
	}
	if got.Title != "持久化验证" {
		t.Fatalf("Title = %q，期望保留", got.Title)
	}
	if got.MessageCount != 2 {
		t.Fatalf("MessageCount = %d，期望 2", got.MessageCount)
	}

	msgs, err := store2.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2", len(msgs))
	}
	if msgs[0].Content != "重启后我应该还在" {
		t.Fatalf("消息内容丢失: %q", msgs[0].Content)
	}
	if msgs[1].Status != session.StatusComplete || msgs[1].Usage == nil {
		t.Fatalf("assistant 状态/用量丢失: %+v", msgs[1])
	}
	if msgs[1].Usage.TotalTokens != 30 {
		t.Fatalf("Usage 未持久化: %+v", msgs[1].Usage)
	}
}

// 消息顺序必须在重启后保持一致（靠 seq 而非时间戳）。
func TestMessageOrderStableAfterReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "order.db")
	ctx := context.Background()

	db1, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store1 := sqlite.NewSessionStore(db1, 0)
	sess := session.NewSession("顺序", "m")
	if err := store1.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 连续写入多条（同一毫秒内，时间戳可能相同）。
	const n = 20
	for i := 0; i < n; i++ {
		m := session.NewUserMessage(sess.ID, "msg")
		m.Content = string(rune('A' + i%26))
		if err := store1.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	_ = db1.Close()

	db2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer func() { _ = db2.Close() }()
	store2 := sqlite.NewSessionStore(db2, 0)

	msgs, err := store2.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("消息数 = %d，期望 %d", len(msgs), n)
	}
	for i := 0; i < n; i++ {
		want := string(rune('A' + i%26))
		if msgs[i].Content != want {
			t.Fatalf("第 %d 条顺序错乱: 得到 %q 期望 %q", i, msgs[i].Content, want)
		}
	}
}

// 删除会话应级联删除消息（外键 ON DELETE CASCADE）。
func TestCascadeDelete(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "cascade.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := sqlite.NewSessionStore(db, 0)
	ctx := context.Background()

	sess := session.NewSession("级联删除", "m")
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := store.AppendMessage(ctx, session.NewUserMessage(sess.ID, "x")); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// 直接查 messages 表，确认级联生效。
	var count int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sess.ID).Scan(&count); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if count != 0 {
		t.Fatalf("级联删除失效：仍有 %d 条消息", count)
	}
}

// schema 版本高于程序时应拒绝打开（防止旧版程序损坏新数据）。
func TestSchemaTooNewRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")

	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 手工把版本改高。
	if _, err := db.SQL().Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("设置版本: %v", err)
	}
	_ = db.Close()

	_, err = sqlite.Open(path)
	if err == nil {
		t.Fatal("schema 版本过新时应拒绝打开")
	}
	if !errors.Is(err, sqlite.ErrSchemaTooNew) {
		t.Fatalf("err = %v，期望 ErrSchemaTooNew", err)
	}
}

// WAL 模式应启用（并发读写性能）。
func TestWALEnabled(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "wal.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var mode string
	if err := db.SQL().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("查询 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q，期望 wal", mode)
	}
}
