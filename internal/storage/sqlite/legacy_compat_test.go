package sqlite_test

// 旧格式兼容：老库里 payload 写的是单个 "pending"，
// 新代码必须能读出来并升级成 pendings 列表，否则升级后那些
// 挂起会变成「有状态但没问题」的死卡片。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
)

func TestLegacySinglePendingUpgrades(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	store := sqlite.NewSessionStore(db, 0)
	sess := session.NewSessionAt("旧数据", "m", time.Now().UTC())
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("建会话: %v", err)
	}

	// 手工写入旧格式 payload（模拟早期版本留下的数据）。
	legacy := `{"run_id":"r1","pending":{"kind":"input","call_id":"call_old","name":"ask_user","args":"{}","question":"老问题？","options":["A"]},"completed":["c0"]}`
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO checkpoints (session_id, version, step_index, state, payload, updated_at)
		 VALUES (?, 1, 2, 'suspended', ?, ?)`,
		sess.ID, legacy, time.Now().UnixMilli()); err != nil {
		t.Fatalf("写入旧格式: %v", err)
	}

	cps := sqlite.NewCheckpointStore(db)
	cp, err := cps.LoadCheckpoint(ctx, sess.ID)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(cp.Pendings) != 1 {
		t.Fatalf("旧格式应升级为 1 个待回答项，实际 %d", len(cp.Pendings))
	}
	if cp.Pendings[0].CallID != "call_old" {
		t.Fatalf("call_id = %q", cp.Pendings[0].CallID)
	}
	if cp.Pendings[0].Question != "老问题？" {
		t.Fatalf("question = %q", cp.Pendings[0].Question)
	}
	if !cp.IsSuspended() {
		t.Fatal("升级后应仍判定为挂起（否则用户无法回答）")
	}
}
