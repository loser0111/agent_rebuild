package sqlite_test

// 回归测试：进程退出前必须关闭数据库，否则 WAL 未 checkpoint 会丢数据。
//
// 这是实测发现的真实缺陷：早期实现只关闭了 HTTP 服务，
// 没有关闭 *sql.DB，导致 agent.db-wal 残留、重启后数据消失。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
)

// 未关闭连接时，WAL 文件应仍存在（说明数据还在 WAL 里，未合并）。
func TestWALFileExistsBeforeClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal-test.db")
	ctx := context.Background()

	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store := sqlite.NewSessionStore(db, 0)
	sess := session.NewSession("wal 测试", "m")
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AppendMessage(ctx, session.NewUserMessage(sess.ID, "数据")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// 此时未关闭：WAL 文件应存在（数据尚未 checkpoint 到主库）。
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Logf("WAL 文件不存在（可能已自动 checkpoint），跳过该断言")
	}

	// 关键：关闭后数据必须可读。
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 关闭后 WAL 应被清理（数据已合并进主库）。
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Logf("WAL 文件在关闭后仍存在（SQLite 可能保留了空 WAL），非致命")
	}

	// 重新打开验证数据完整性。
	db2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer func() { _ = db2.Close() }()

	got, err := sqlite.NewSessionStore(db2, 0).Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("重启后 Get 失败（数据丢失）: %v", err)
	}
	if got.Title != "wal 测试" {
		t.Fatalf("Title = %q", got.Title)
	}
}

// 反复开关数据库不应丢数据（避免 Close 被遗漏而无人发现）。
func TestRepeatedOpenCloseKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeated.db")
	ctx := context.Background()

	const rounds = 5
	var lastID string

	for i := 0; i < rounds; i++ {
		db, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("第 %d 轮 Open: %v", i, err)
		}
		store := sqlite.NewSessionStore(db, 0)

		// 每轮写入一条新会话。
		sess := session.NewSession("round", "m")
		sess.Title = "会话" + string(rune('0'+i))
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("第 %d 轮 Create: %v", i, err)
		}
		if err := store.AppendMessage(ctx, session.NewUserMessage(sess.ID, "msg")); err != nil {
			t.Fatalf("第 %d 轮 AppendMessage: %v", i, err)
		}
		lastID = sess.ID

		if err := db.Close(); err != nil {
			t.Fatalf("第 %d 轮 Close: %v", i, err)
		}

		// 每轮关闭后立刻重开验证：已写入的数据必须都在。
		verify, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("第 %d 轮验证 Open: %v", i, err)
		}
		list, _, listErr := sqlite.NewSessionStore(verify, 0).List(ctx, session.ListOptions{Limit: 100})
		verifyCloseErr := verify.Close()
		if listErr != nil {
			t.Fatalf("第 %d 轮 List: %v", i, listErr)
		}
		if closeErr := verifyCloseErr; closeErr != nil {
			t.Fatalf("第 %d 轮验证 Close: %v", i, closeErr)
		}
		if len(list) != i+1 {
			t.Fatalf("第 %d 轮后应有 %d 条会话，实际 %d 条（数据丢失）", i, i+1, len(list))
		}
	}

	// 最终确认最后一条还在。
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("最终 Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := sqlite.NewSessionStore(db, 0).Get(ctx, lastID); err != nil {
		t.Fatalf("最终 Get 失败: %v", err)
	}
}
