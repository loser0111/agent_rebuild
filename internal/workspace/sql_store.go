package workspace

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SQLQuerier 抽象 *sql.DB 与 *sql.Tx 的公共查询能力（与 modelconfig 包同名接口
// 保持一致的最小子集，两者刻意不共享类型，避免 workspace 包反向依赖 modelconfig）。
type SQLQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLStore 是 Store 的 SQLite 实现。
type SQLStore struct {
	db  SQLQuerier
	now func() time.Time
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore 创建 SQLite 版 workspace 存储。
func NewSQLStore(db SQLQuerier) *SQLStore {
	return &SQLStore{
		db:  db,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// SetClock 注入时钟（测试用）。
func (s *SQLStore) SetClock(clock func() time.Time) {
	if clock == nil {
		s.now = func() time.Time { return time.Now().UTC() }
		return
	}
	s.now = clock
}

// Create 新建 workspace。
func (s *SQLStore) Create(ctx context.Context, in Input) (*Workspace, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	folders, err := encodeFolders(in.Folders)
	if err != nil {
		return nil, err
	}

	now := s.now()
	id := newID()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, folders, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, in.Name, folders, toMillis(now), toMillis(now),
	); err != nil {
		return nil, fmt.Errorf("workspace: 插入失败: %w", err)
	}
	return s.Get(ctx, id)
}

// Get 读取 workspace。
func (s *SQLStore) Get(ctx context.Context, id string) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, folders, created_at, updated_at
		FROM workspaces WHERE id = ?`, id)

	ws, err := scanWorkspace(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return ws, nil
}

// List 返回全部 workspace（更新时间倒序）。
func (s *SQLStore) List(ctx context.Context) ([]*Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, folders, created_at, updated_at
		FROM workspaces
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("workspace: 查询失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Workspace, 0, 8)
	for rows.Next() {
		ws, scanErr := scanWorkspace(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, ws)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: 遍历失败: %w", err)
	}
	return out, nil
}

// Update 修改 workspace（整体覆盖 Name / Folders，语义与 modelconfig.Update 一致）。
func (s *SQLStore) Update(ctx context.Context, id string, in Input) (*Workspace, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}

	folders, err := encodeFolders(in.Folders)
	if err != nil {
		return nil, err
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE workspaces SET name = ?, folders = ?, updated_at = ?
		WHERE id = ?`,
		in.Name, folders, toMillis(s.now()), id,
	); err != nil {
		return nil, fmt.Errorf("workspace: 更新失败: %w", err)
	}
	return s.Get(ctx, id)
}

// Delete 删除 workspace。
//
// 刻意不级联处理引用它的会话：session.workspace_id 没有外键约束
// （见 docs/14-workspace-and-session-presets.md 第 2.2 节），删除后
// 那些会话的 workspace_id 会变成一个"查不到"的悬空引用，等到真正
// 使用时再报错——与 model_config_id 失效时的处理方式保持一致，
// 不在删除这一刻去扫描、清洗所有会话。
func (s *SQLStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("workspace: 删除失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workspace: 读取影响行数失败: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- 内部辅助 ----

func scanWorkspace(sc interface{ Scan(...any) error }) (*Workspace, error) {
	var (
		ws      Workspace
		folders string
		created int64
		updated int64
	)
	if err := sc.Scan(&ws.ID, &ws.Name, &folders, &created, &updated); err != nil {
		return nil, err
	}
	ws.Folders = decodeFolders(folders)
	ws.CreatedAt = fromMillis(created)
	ws.UpdatedAt = fromMillis(updated)
	return &ws, nil
}

// encodeFolders 序列化为 JSON 数组；入参已经过 Input.Validate() 保证非空。
func encodeFolders(folders []string) (string, error) {
	raw, err := json.Marshal(folders)
	if err != nil {
		return "", fmt.Errorf("workspace: 序列化文件夹列表失败: %w", err)
	}
	return string(raw), nil
}

// decodeFolders 反序列化失败时返回空列表而不是报错：
// 这是展示用的读路径，单条脏数据不应该让整个 List/Get 失败。
func decodeFolders(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ws_%d", time.Now().UnixNano())
	}
	return "ws_" + hex.EncodeToString(b[:])
}

func toMillis(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
