package sqlite_test

// checkpoint 落库的端到端测试：用真实 SQLite 文件验证
// 「ask_user 挂起后，session_id 确实写进了 checkpoints 表」。
//
// 为什么单独写这个：单元测试用的是内存假实现，
// 真实实现涉及 ON CONFLICT、外键、payload 序列化等，
// 这些都必须对真库验证一遍，否则「保存了 sessionId」只是口头承诺。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
)

func newCheckpointFixture(t *testing.T) (*sqlite.DB, *sqlite.CheckpointStore, *sqlite.SessionStore, string) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sessions := sqlite.NewSessionStore(db, 0)
	ctx := context.Background()
	// checkpoints 有外键指向 sessions，必须先建会话。
	sess := session.NewSessionAt("测试会话", "m", time.Now().UTC())
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	return db, sqlite.NewCheckpointStore(db), sessions, sess.ID
}

// 核心：save → load 往返后 session_id 与挂起详情完整保留。
func TestCheckpointRoundTripPreservesSessionID(t *testing.T) {
	db, cps, _, sessionID := newCheckpointFixture(t)
	ctx := context.Background()

	cp := &session.Checkpoint{
		SessionID: sessionID,
		Version:   1,
		RunID:     "run_1",
		StepIndex: 3,
		State:     session.CheckpointSuspended,
		Pendings: []session.Pending{{
			Kind:     session.PendingInput,
			CallID:   "call_abc",
			Name:     "ask_user",
			Args:     `{"question":"用哪个？"}`,
			Question: "用哪个？",
			Options:  []string{"A", "B"},
		}},
		Completed: []string{"call_prev"},
	}
	if err := cps.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatalf("保存: %v", err)
	}

	// 直接查表，确认 session_id 落在真正的列上（而不是只存在 payload 里）。
	var (
		gotSessionID string
		gotState     string
		gotStep      int
	)
	err := db.SQL().QueryRowContext(ctx,
		`SELECT session_id, state, step_index FROM checkpoints`).
		Scan(&gotSessionID, &gotState, &gotStep)
	if err != nil {
		t.Fatalf("查询 checkpoints: %v", err)
	}
	if gotSessionID != sessionID {
		t.Fatalf("数据库里的 session_id = %q，期望 %q", gotSessionID, sessionID)
	}
	if gotState != "suspended" {
		t.Fatalf("state = %q，期望 suspended", gotState)
	}
	if gotStep != 3 {
		t.Fatalf("step_index = %d，期望 3", gotStep)
	}

	// 再走一遍 Load，验证 payload 反序列化。
	loaded, err := cps.LoadCheckpoint(ctx, sessionID)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if loaded.SessionID != sessionID {
		t.Fatalf("Load 返回的 session_id = %q", loaded.SessionID)
	}
	if len(loaded.Pendings) != 1 || loaded.Pendings[0].CallID != "call_abc" {
		t.Fatalf("Pendings 丢失: %+v", loaded.Pendings)
	}
	if loaded.Pendings[0].Question != "用哪个？" {
		t.Fatalf("Question = %q", loaded.Pendings[0].Question)
	}
	if len(loaded.Pendings[0].Options) != 2 {
		t.Fatalf("Options = %v", loaded.Pendings[0].Options)
	}
	if len(loaded.Completed) != 1 || loaded.Completed[0] != "call_prev" {
		t.Fatalf("Completed = %v", loaded.Completed)
	}
}

// 一个会话只有一条：重复保存必须覆盖，而不是报错或多行。
func TestCheckpointSaveOverwrites(t *testing.T) {
	db, cps, _, sessionID := newCheckpointFixture(t)
	ctx := context.Background()

	first := &session.Checkpoint{
		SessionID: sessionID, Version: 1, State: session.CheckpointRunning, StepIndex: 0,
	}
	if err := cps.SaveCheckpoint(ctx, first); err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	second := &session.Checkpoint{
		SessionID: sessionID, Version: 1, State: session.CheckpointSuspended, StepIndex: 2,
		Pendings: []session.Pending{{Kind: session.PendingInput, CallID: "c1", Question: "q"}},
	}
	if err := cps.SaveCheckpoint(ctx, second); err != nil {
		t.Fatalf("覆盖保存: %v", err)
	}

	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("计数: %v", err)
	}
	if n != 1 {
		t.Fatalf("checkpoint 行数 = %d，期望 1（一 session 一条）", n)
	}

	loaded, err := cps.LoadCheckpoint(ctx, sessionID)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if loaded.State != session.CheckpointSuspended {
		t.Fatalf("覆盖后 state = %q", loaded.State)
	}
	if loaded.StepIndex != 2 {
		t.Fatalf("覆盖后 step_index = %d", loaded.StepIndex)
	}
}

// session_id 为空必须在领域层就拒绝，不能靠外键报错。
func TestCheckpointSaveRejectsEmptySessionID(t *testing.T) {
	_, cps, _, _ := newCheckpointFixture(t)
	err := cps.SaveCheckpoint(context.Background(), &session.Checkpoint{
		Version: 1, State: session.CheckpointRunning,
	})
	if !errors.Is(err, session.ErrCheckpointNoSession) {
		t.Fatalf("期望 ErrCheckpointNoSession，实际: %v", err)
	}
}

// 挂起状态但没有 Pending 是非法的（resume 时无从下手）。
func TestCheckpointSaveRejectsSuspendedWithoutPending(t *testing.T) {
	_, cps, _, sessionID := newCheckpointFixture(t)
	err := cps.SaveCheckpoint(context.Background(), &session.Checkpoint{
		SessionID: sessionID, Version: 1, State: session.CheckpointSuspended,
	})
	if !errors.Is(err, session.ErrCheckpointInvalid) {
		t.Fatalf("期望 ErrCheckpointInvalid，实际: %v", err)
	}
}

// 读取不存在的 checkpoint 应返回专门的错误，便于上层区分「没有」与「出错」。
func TestCheckpointLoadNotFound(t *testing.T) {
	_, cps, _, _ := newCheckpointFixture(t)
	_, err := cps.LoadCheckpoint(context.Background(), "sess_none")
	if !errors.Is(err, session.ErrCheckpointNotFound) {
		t.Fatalf("期望 ErrCheckpointNotFound，实际: %v", err)
	}
}

// 删除幂等：不存在时也返回 nil，便于在 defer 里直接调用。
func TestCheckpointDeleteIdempotent(t *testing.T) {
	_, cps, _, sessionID := newCheckpointFixture(t)
	ctx := context.Background()
	if err := cps.DeleteCheckpoint(ctx, sessionID); err != nil {
		t.Fatalf("删除不存在的 checkpoint 应返回 nil: %v", err)
	}

	cp := &session.Checkpoint{
		SessionID: sessionID, Version: 1, State: session.CheckpointRunning,
	}
	if err := cps.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatalf("保存: %v", err)
	}
	if err := cps.DeleteCheckpoint(ctx, sessionID); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if _, err := cps.LoadCheckpoint(ctx, sessionID); !errors.Is(err, session.ErrCheckpointNotFound) {
		t.Fatalf("删除后应读不到，实际: %v", err)
	}
}

// 删除会话时 checkpoint 应被级联清理（外键 ON DELETE CASCADE）。
func TestCheckpointCascadesOnSessionDelete(t *testing.T) {
	db, cps, sessions, sessionID := newCheckpointFixture(t)
	ctx := context.Background()

	if err := cps.SaveCheckpoint(ctx, &session.Checkpoint{
		SessionID: sessionID, Version: 1, State: session.CheckpointSuspended,
		Pendings: []session.Pending{{Kind: session.PendingInput, CallID: "c", Question: "q"}},
	}); err != nil {
		t.Fatalf("保存: %v", err)
	}

	if err := sessions.Delete(ctx, sessionID); err != nil {
		t.Fatalf("删除会话: %v", err)
	}

	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("计数: %v", err)
	}
	if n != 0 {
		t.Fatalf("删除会话后 checkpoint 应被级联删除，实际剩 %d 行", n)
	}
}

// ListSuspended 只返回挂起的会话，且能直接拿到问题内容。
func TestCheckpointListSuspended(t *testing.T) {
	_, cps, sessions, firstID := newCheckpointFixture(t)
	ctx := context.Background()

	// 第二个会话。
	second := session.NewSessionAt("第二个", "m", time.Now().UTC())
	if err := sessions.Create(ctx, second); err != nil {
		t.Fatalf("创建第二个会话: %v", err)
	}

	// firstID：挂起；second.ID：运行中（不应出现在结果里）。
	if err := cps.SaveCheckpoint(ctx, &session.Checkpoint{
		SessionID: firstID, Version: 1, State: session.CheckpointSuspended,
		Pendings: []session.Pending{{Kind: session.PendingInput, CallID: "c1", Question: "在等什么？"}},
	}); err != nil {
		t.Fatalf("保存挂起: %v", err)
	}
	if err := cps.SaveCheckpoint(ctx, &session.Checkpoint{
		SessionID: second.ID, Version: 1, State: session.CheckpointRunning,
	}); err != nil {
		t.Fatalf("保存运行中: %v", err)
	}

	list, err := cps.ListSuspended(ctx)
	if err != nil {
		t.Fatalf("ListSuspended: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("挂起数 = %d，期望 1", len(list))
	}
	if list[0].SessionID != firstID {
		t.Fatalf("挂起的应当是 %q，实际 %q", firstID, list[0].SessionID)
	}
	if len(list[0].Pendings) == 0 || list[0].Pendings[0].Question != "在等什么？" {
		t.Fatalf("应能直接拿到问题，实际: %+v", list[0].Pendings)
	}
}
