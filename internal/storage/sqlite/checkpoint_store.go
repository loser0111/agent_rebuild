package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent_rebuild/internal/session"
)

// CheckpointStore 是 session.CheckpointStore 的 SQLite 实现。
//
// 表结构见 db.go 的 schemaV1（checkpoints），关键点：
//   - session_id 是主键 → 一个会话只有一条，天然满足「覆盖语义」
//   - 外键 ON DELETE CASCADE → 删会话时断点自动清理，无需应用层操心
//   - payload 存 JSON（Pending / Completed 这类结构化数据不适合拆列）
type CheckpointStore struct {
	db  *DB
	now func() time.Time
}

var _ session.CheckpointStore = (*CheckpointStore)(nil)

// NewCheckpointStore 创建 SQLite 断点存储。
func NewCheckpointStore(db *DB) *CheckpointStore {
	return &CheckpointStore{
		db:  db,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// SetClock 注入时钟（测试用）。
func (s *CheckpointStore) SetClock(clock func() time.Time) {
	if clock == nil {
		s.now = func() time.Time { return time.Now().UTC() }
		return
	}
	s.now = clock
}

// checkpointPayload 是 checkpoints.payload 列的 JSON 结构。
//
// session_id / version / step_index / state 都有独立列（要参与查询与排查），
// 只有「挂起详情 + 已完成列表」放这里。
type checkpointPayload struct {
	RunID string `json:"run_id,omitempty"`
	// Pendings 当前格式：同一轮可以有多个待回答项。
	Pendings []session.Pending `json:"pendings,omitempty"`
	// LegacyPending 旧格式兼容：早期版本只存单个 pending。
	//
	// 保留这个字段纯粹为了读取老数据——老库里写的是 "pending"，
	// 若不认它，升级后那些挂起会变成「有状态但没问题」的死卡片。
	LegacyPending *session.Pending `json:"pending,omitempty"`
	Completed     []string         `json:"completed,omitempty"`
}

// apply 把 payload 内容填进领域对象（含旧格式升级）。
func (p checkpointPayload) apply(cp *session.Checkpoint) {
	cp.RunID = p.RunID
	cp.Completed = p.Completed
	cp.Pendings = p.Pendings
	// 旧格式升级：单 pending 视作只有一个待回答项。
	if len(cp.Pendings) == 0 && p.LegacyPending != nil {
		cp.Pendings = []session.Pending{*p.LegacyPending}
	}
	cp.Normalize()
}

// SaveCheckpoint 写入或覆盖会话的 checkpoint。
func (s *CheckpointStore) SaveCheckpoint(ctx context.Context, cp *session.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 领域校验会拦住「session_id 为空」——这是必须的第一道闸门：
	// 空 session_id 会触发外键失败，错误信息对调用方毫无指导意义。
	if err := cp.Validate(); err != nil {
		return err
	}

	version := cp.Version
	if version <= 0 {
		version = 1
	}
	payload, err := json.Marshal(checkpointPayload{
		RunID:     cp.RunID,
		Pendings:  cp.Pendings,
		Completed: cp.Completed,
	})
	if err != nil {
		return fmt.Errorf("sqlite: 序列化 checkpoint 失败: %w", err)
	}

	now := s.now()
	if !cp.UpdatedAt.IsZero() {
		now = cp.UpdatedAt
	}

	// ON CONFLICT DO UPDATE：一个会话一条，重复写入即覆盖。
	_, err = s.db.SQL().ExecContext(ctx, `
		INSERT INTO checkpoints (session_id, version, step_index, state, payload, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			version    = excluded.version,
			step_index = excluded.step_index,
			state      = excluded.state,
			payload    = excluded.payload,
			updated_at = excluded.updated_at`,
		cp.SessionID, version, cp.StepIndex, string(cp.State), string(payload), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlite: 保存 checkpoint 失败: %w", err)
	}
	return nil
}

// LoadCheckpoint 读取会话当前的 checkpoint。
func (s *CheckpointStore) LoadCheckpoint(ctx context.Context, sessionID string) (*session.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, session.ErrCheckpointNoSession
	}

	var (
		version   int
		stepIndex int
		state     string
		payload   string
		updatedAt int64
	)
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT version, step_index, state, payload, updated_at
		FROM checkpoints WHERE session_id = ?`, sessionID,
	).Scan(&version, &stepIndex, &state, &payload, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrCheckpointNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: 读取 checkpoint 失败: %w", err)
	}

	cp := &session.Checkpoint{
		SessionID: sessionID,
		Version:   version,
		StepIndex: stepIndex,
		State:     session.CheckpointState(state),
		UpdatedAt: time.UnixMilli(updatedAt).UTC(),
	}
	// payload 为空视作「没有附加状态」，不当作损坏——
	// running 状态的 checkpoint 本来就可能没有待回答项。
	if payload != "" {
		var p checkpointPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, fmt.Errorf("sqlite: 解析 checkpoint payload 失败: %w", err)
		}
		p.apply(cp)
	}
	return cp, nil
}

// DeleteCheckpoint 删除会话的 checkpoint（幂等）。
func (s *CheckpointStore) DeleteCheckpoint(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" {
		return session.ErrCheckpointNoSession
	}
	if _, err := s.db.SQL().ExecContext(ctx,
		`DELETE FROM checkpoints WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("sqlite: 删除 checkpoint 失败: %w", err)
	}
	return nil
}

// ListSuspended 列出所有处于挂起状态的会话（按更新时间倒序）。
//
// 用于启动时提示「有 N 个会话在等你回答」，以及 /api/sessions 标记状态。
// 之所以单独一个方法：state 是独立列，可以直接走索引，不需要把
// 所有 checkpoint 读进内存再过滤。
func (s *CheckpointStore) ListSuspended(ctx context.Context) ([]*session.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT session_id, version, step_index, state, payload, updated_at
		FROM checkpoints
		WHERE state = ?
		ORDER BY updated_at DESC`, string(session.CheckpointSuspended))
	if err != nil {
		return nil, fmt.Errorf("sqlite: 查询挂起会话失败: %w", err)
	}
	defer rows.Close()

	out := make([]*session.Checkpoint, 0, 8)
	for rows.Next() {
		var (
			sessionID string
			version   int
			stepIndex int
			state     string
			payload   string
			updatedAt int64
		)
		if err := rows.Scan(&sessionID, &version, &stepIndex, &state, &payload, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: 扫描挂起会话失败: %w", err)
		}
		cp := &session.Checkpoint{
			SessionID: sessionID,
			Version:   version,
			StepIndex: stepIndex,
			State:     session.CheckpointState(state),
			UpdatedAt: time.UnixMilli(updatedAt).UTC(),
		}
		if payload != "" {
			var p checkpointPayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return nil, fmt.Errorf("sqlite: 解析 checkpoint payload 失败: %w", err)
			}
			p.apply(cp)
		}
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: 遍历挂起会话失败: %w", err)
	}
	return out, nil
}
