package session_test

// 用现有契约测试校验 SQLite 实现，确保行为与内存实现完全一致。

import (
	"path/filepath"
	"testing"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
)

// TestSQLiteStoreContract 让 SQLite 实现接受与内存实现完全相同的契约检查。
//
// 这是「换存储实现不破坏行为」的守门测试：只要契约测试通过，
// 上层（api / executor）无需任何改动。
func TestSQLiteStoreContract(t *testing.T) {
	runContract(t, false)
}

// 开启消息裁剪后同样必须满足全部契约。
func TestSQLiteStoreContractWithTrim(t *testing.T) {
	runContract(t, true)
}

func runContract(t *testing.T, withTrim bool) {
	t.Helper()

	maxMsgs := 0
	if withTrim {
		maxMsgs = 5
	}

	session.RunStoreContractHelper(t, func(t *testing.T) session.Store {
		t.Helper()
		db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatalf("打开数据库: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return sqlite.NewSessionStore(db, maxMsgs)
	})
}
