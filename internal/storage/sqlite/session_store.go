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

// SessionStore 是 session.Store 的 SQLite 实现。
//
// 契约与内存实现完全一致（由 session.RunStoreContract 校验）：
//   - 并发安全：由 SQLite 事务 + 单连接串行保证
//   - 深拷贝：每次查询都从数据库重新读取，天然不共享内存
//   - 读改写原子化：Update / UpdateMessage 在事务内执行 fn
//   - 不吞 ctx：所有查询都带 ctx
type SessionStore struct {
	db     *DB
	maxMsg int
	now    func() time.Time
}

var _ session.Store = (*SessionStore)(nil)

// NewSessionStore 创建 SQLite 会话存储。
// maxMsg <= 0 表示不限制单会话消息数。
func NewSessionStore(db *DB, maxMsg int) *SessionStore {
	return &SessionStore{
		db:     db,
		maxMsg: maxMsg,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// SetClock 注入时钟（测试用）。
func (s *SessionStore) SetClock(clock func() time.Time) {
	if clock == nil {
		s.now = func() time.Time { return time.Now().UTC() }
		return
	}
	s.now = clock
}

// Create 写入新会话。
func (s *SessionStore) Create(ctx context.Context, sess *session.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("%w: 会话不能为空", session.ErrInvalidArgument)
	}
	if err := sess.Validate(); err != nil {
		return err
	}

	now := s.now()
	created := sess.CreatedAt
	if created.IsZero() {
		created = now
	}
	meta, err := encodeMeta(sess.Meta)
	if err != nil {
		return err
	}

	_, err = s.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions (id, title, model, model_config_id, workspace_id, approval_mode, status, message_count, meta, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		sess.ID, sess.Title, sess.Model, sess.ModelConfigID, sess.WorkspaceID, sess.ApprovalMode, string(sess.Status), meta,
		toMillis(created), toMillis(now),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return session.ErrConflict
		}
		return fmt.Errorf("sqlite: 创建会话失败: %w", err)
	}
	return nil
}

// Get 读取会话元信息。
func (s *SessionStore) Get(ctx context.Context, id string) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	row := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, title, model, model_config_id, workspace_id, approval_mode, status, message_count, meta, created_at, updated_at
		FROM sessions WHERE id = ?`, id)

	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}
	return sess, nil
}

// List 按 updated_at 倒序分页。
func (s *SessionStore) List(ctx context.Context, opts session.ListOptions) ([]*session.Session, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var (
		rows *sql.Rows
		err  error
	)
	if opts.Cursor == "" {
		rows, err = s.db.SQL().QueryContext(ctx, `
			SELECT id, title, model, model_config_id, workspace_id, approval_mode, status, message_count, meta, created_at, updated_at
			FROM sessions
			ORDER BY updated_at DESC, id DESC
			LIMIT ?`, limit)
	} else {
		// 游标分页：找到游标行的 (updated_at, id)，取严格小于它的记录。
		var curUpdated int64
		var curID string
		err = s.db.SQL().QueryRowContext(ctx,
			`SELECT updated_at, id FROM sessions WHERE id = ?`, opts.Cursor,
		).Scan(&curUpdated, &curID)
		if errors.Is(err, sql.ErrNoRows) {
			// 游标指向的会话已删除：退回第一页，避免用户看到空列表。
			return s.List(ctx, session.ListOptions{Limit: limit})
		}
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: 解析游标失败: %w", err)
		}
		rows, err = s.db.SQL().QueryContext(ctx, `
			SELECT id, title, model, model_config_id, workspace_id, approval_mode, status, message_count, meta, created_at, updated_at
			FROM sessions
			WHERE (updated_at < ?) OR (updated_at = ? AND id < ?)
			ORDER BY updated_at DESC, id DESC
			LIMIT ?`, curUpdated, curUpdated, curID, limit)
	}
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: 列出会话失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]*session.Session, 0, limit)
	for rows.Next() {
		sess, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, "", scanErr
		}
		items = append(items, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: 遍历会话失败: %w", err)
	}

	next := ""
	if len(items) == limit {
		// 还有更多时返回最后一个 id 作为游标。
		next = items[len(items)-1].ID
	}
	return items, next, nil
}

// Update 在事务内执行 fn 修改会话。
func (s *SessionStore) Update(ctx context.Context, id string, fn func(*session.Session) error) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("%w: Update 需要非空 fn", session.ErrInvalidArgument)
	}

	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT id, title, model, model_config_id, workspace_id, approval_mode, status, message_count, meta, created_at, updated_at
		FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}

	// 在副本上执行并校验：fn 报错或校验失败都不改动数据库（天然回滚）。
	if err := fn(sess); err != nil {
		return nil, err
	}
	if err := sess.Validate(); err != nil {
		return nil, err
	}

	// 不可变字段保护。
	sess.ID = id
	// message_count 以真实消息数为准，防止与 messages 表不一致。
	var realCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, id,
	).Scan(&realCount); err != nil {
		return nil, fmt.Errorf("sqlite: 统计消息数失败: %w", err)
	}
	sess.MessageCount = realCount
	sess.UpdatedAt = s.now()

	meta, err := encodeMeta(sess.Meta)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET title = ?, model = ?, model_config_id = ?, workspace_id = ?, approval_mode = ?, status = ?, message_count = ?, meta = ?, updated_at = ?
		WHERE id = ?`,
		sess.Title, sess.Model, sess.ModelConfigID, sess.WorkspaceID, sess.ApprovalMode, string(sess.Status), sess.MessageCount, meta,
		toMillis(sess.UpdatedAt), id,
	); err != nil {
		return nil, fmt.Errorf("sqlite: 更新会话失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: 提交事务失败: %w", err)
	}
	return sess, nil
}

// Delete 删除会话（消息由外键级联删除）。
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := s.db.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: 删除会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: 读取影响行数失败: %w", err)
	}
	if n == 0 {
		return session.ErrNotFound
	}
	return nil
}

// AppendMessage 追加消息并维护 session 计数与时间。
func (s *SessionStore) AppendMessage(ctx context.Context, m *session.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: 消息不能为空", session.ErrInvalidArgument)
	}
	if err := m.Validate(); err != nil {
		return err
	}

	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 会话必须存在。
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, m.SessionID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return session.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("sqlite: 查询会话失败: %w", err)
	}

	now := s.now()
	created := m.CreatedAt
	if created.IsZero() {
		created = now
	}

	// seq 用于保证同会话内消息的稳定顺序（created_at 精度可能相同）。
	var maxSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM messages WHERE session_id = ?`, m.SessionID,
	).Scan(&maxSeq); err != nil {
		return fmt.Errorf("sqlite: 计算消息序号失败: %w", err)
	}

	usageJSON, errorJSON, toolCalls, err := encodeMessageParts(m)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages
			(id, session_id, role, content, status, usage_json, error_json, tool_calls, tool_call_id, seq, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.SessionID, string(m.Role), m.Content, string(m.Status),
		usageJSON, errorJSON, toolCalls, m.ToolCallID, maxSeq+1,
		toMillis(created), toMillis(now),
	); err != nil {
		if isUniqueViolation(err) {
			return session.ErrConflict
		}
		return fmt.Errorf("sqlite: 插入消息失败: %w", err)
	}

	// 裁剪：超出上限时删除最旧的（保留 system 消息）。
	if s.maxMsg > 0 {
		if err := trimMessages(ctx, tx, m.SessionID, s.maxMsg); err != nil {
			return err
		}
	}

	// 同步会话计数与时间。
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET message_count = (SELECT COUNT(*) FROM messages WHERE session_id = ?),
		    updated_at = ?
		WHERE id = ?`, m.SessionID, toMillis(now), m.SessionID,
	); err != nil {
		return fmt.Errorf("sqlite: 更新会话统计失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: 提交事务失败: %w", err)
	}
	return nil
}

// UpdateMessage 在事务内执行 fn 修改消息。
func (s *SessionStore) UpdateMessage(ctx context.Context, sessionID, msgID string, fn func(*session.Message) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("%w: UpdateMessage 需要非空 fn", session.ErrInvalidArgument)
	}

	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT id, session_id, role, content, status, usage_json, error_json, tool_calls, tool_call_id, created_at, updated_at
		FROM messages WHERE session_id = ? AND id = ?`, sessionID, msgID)
	msg, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session.ErrNotFound
		}
		return err
	}

	if err := fn(msg); err != nil {
		return err
	}
	if err := msg.Validate(); err != nil {
		return err
	}

	// 不可变字段保护。
	msg.ID = msgID
	msg.SessionID = sessionID
	msg.UpdatedAt = s.now()

	usageJSON, errorJSON, toolCalls, err := encodeMessageParts(msg)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET role = ?, content = ?, status = ?, usage_json = ?, error_json = ?,
		    tool_calls = ?, tool_call_id = ?, updated_at = ?
		WHERE session_id = ? AND id = ?`,
		string(msg.Role), msg.Content, string(msg.Status), usageJSON, errorJSON,
		toolCalls, msg.ToolCallID, toMillis(msg.UpdatedAt), sessionID, msgID,
	); err != nil {
		return fmt.Errorf("sqlite: 更新消息失败: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, toMillis(msg.UpdatedAt), sessionID,
	); err != nil {
		return fmt.Errorf("sqlite: 更新会话时间失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: 提交事务失败: %w", err)
	}
	return nil
}

// Messages 返回会话全部消息（按 seq 正序）。
func (s *SessionStore) Messages(ctx context.Context, sessionID string) ([]session.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 先确认会话存在（与内存实现语义一致）。
	var exists int
	err := s.db.SQL().QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: 查询会话失败: %w", err)
	}

	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, session_id, role, content, status, usage_json, error_json, tool_calls, tool_call_id, created_at, updated_at
		FROM messages WHERE session_id = ? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: 查询消息失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]session.Message, 0, 16)
	for rows.Next() {
		msg, scanErr := scanMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: 遍历消息失败: %w", err)
	}
	return out, nil
}

// RecoverStaleRunning 把卡在 running 的会话复位为 idle。
//
// 场景：进程被强杀 / 崩溃 / 容器重启后，数据库里会残留 running 状态。
// 前端据此显示「仍在生成中」，而实际没有任何任务在跑，
// 用户重新打开会话只会看到一条永远不消失的提示。
//
// 在启动装配完成后调用一次即可自愈。
func (s *SessionStore) RecoverStaleRunning(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	res, err := s.db.SQL().ExecContext(ctx, `
		UPDATE sessions SET status = ?, updated_at = ?
		WHERE status = ?`,
		string(session.SessionIdle), toMillis(s.now()), string(session.SessionRunning),
	)
	if err != nil {
		return 0, fmt.Errorf("sqlite: 复位 running 会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // 计数不可得不算失败
	}
	return int(n), nil
}

// ---- 内部辅助 ----

type scanner interface {
	Scan(dest ...any) error
}

func scanSession(sc scanner) (*session.Session, error) {
	var (
		sess    session.Session
		status  string
		meta    string
		created int64
		updated int64
	)
	if err := sc.Scan(&sess.ID, &sess.Title, &sess.Model, &sess.ModelConfigID, &sess.WorkspaceID, &sess.ApprovalMode, &status,
		&sess.MessageCount, &meta, &created, &updated); err != nil {
		return nil, err
	}
	sess.Status = session.SessionStatus(status)
	sess.CreatedAt = fromMillis(created)
	sess.UpdatedAt = fromMillis(updated)
	sess.Meta = decodeMeta(meta)
	return &sess, nil
}

func scanMessage(sc scanner) (*session.Message, error) {
	var (
		msg       session.Message
		role      string
		status    string
		usageJSON string
		errorJSON string
		toolCalls string
		created   int64
		updated   int64
	)
	if err := sc.Scan(&msg.ID, &msg.SessionID, &role, &msg.Content, &status,
		&usageJSON, &errorJSON, &toolCalls, &msg.ToolCallID, &created, &updated); err != nil {
		return nil, err
	}
	msg.Role = session.Role(role)
	msg.Status = session.MessageStatus(status)
	msg.CreatedAt = fromMillis(created)
	msg.UpdatedAt = fromMillis(updated)

	if usageJSON != "" {
		var u session.Usage
		if err := json.Unmarshal([]byte(usageJSON), &u); err == nil {
			msg.Usage = &u
		}
	}
	if errorJSON != "" {
		var e session.MessageError
		if err := json.Unmarshal([]byte(errorJSON), &e); err == nil {
			msg.Error = &e
		}
	}
	// tool_calls 必须还原，否则历史里 assistant 消息看起来像"普通文本消息"，
	// 配对的 tool 结果会变成孤儿被 toMessages 丢弃，
	// 模型因此不知道工具已经调用过，每次对话都重复调用同一个工具。
	if toolCalls != "" {
		var tcs []session.ToolCall
		if err := json.Unmarshal([]byte(toolCalls), &tcs); err == nil {
			msg.ToolCalls = tcs
		}
	}
	return &msg, nil
}

func encodeMeta(meta map[string]string) (string, error) {
	if len(meta) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("sqlite: 序列化 meta 失败: %w", err)
	}
	return string(raw), nil
}

func decodeMeta(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil
	}
	return meta
}

func encodeMessageParts(m *session.Message) (usageJSON, errorJSON, toolCalls string, err error) {
	if m.Usage != nil {
		raw, marshalErr := json.Marshal(m.Usage)
		if marshalErr != nil {
			return "", "", "", fmt.Errorf("sqlite: 序列化 usage 失败: %w", marshalErr)
		}
		usageJSON = string(raw)
	}
	if m.Error != nil {
		raw, marshalErr := json.Marshal(m.Error)
		if marshalErr != nil {
			return "", "", "", fmt.Errorf("sqlite: 序列化 error 失败: %w", marshalErr)
		}
		errorJSON = string(raw)
	}
	if len(m.ToolCalls) > 0 {
		raw, marshalErr := json.Marshal(m.ToolCalls)
		if marshalErr != nil {
			return "", "", "", fmt.Errorf("sqlite: 序列化 tool_calls 失败: %w", marshalErr)
		}
		toolCalls = string(raw)
	}
	return usageJSON, errorJSON, toolCalls, nil
}

// trimMessages 裁剪超限消息，保留全部 system 消息与最近的非 system 消息。
func trimMessages(ctx context.Context, tx *sql.Tx, sessionID string, keep int) error {
	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&total); err != nil {
		return fmt.Errorf("sqlite: 统计消息数失败: %w", err)
	}
	if total <= keep {
		return nil
	}

	var systemCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'system'`, sessionID,
	).Scan(&systemCount); err != nil {
		return fmt.Errorf("sqlite: 统计 system 消息失败: %w", err)
	}
	// system 已占满配额：保留全部 system，不删（与内存实现语义一致）。
	remaining := keep - systemCount
	if remaining <= 0 {
		return nil
	}

	// 删除除「最近 remaining 条非 system 消息」之外的非 system 消息。
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE session_id = ?
		  AND role != 'system'
		  AND id NOT IN (
		      SELECT id FROM messages
		      WHERE session_id = ? AND role != 'system'
		      ORDER BY seq DESC
		      LIMIT ?
		  )`, sessionID, sessionID, remaining); err != nil {
		return fmt.Errorf("sqlite: 裁剪消息失败: %w", err)
	}
	return nil
}

func toMillis(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// isUniqueViolation 判断是否为唯一约束冲突（现代 sqlite 驱动的错误字符串匹配）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "UNIQUE constraint failed") || contains(msg, "constraint failed: UNIQUE")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
