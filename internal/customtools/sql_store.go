package customtools

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"agent_rebuild/internal/tool"
)

// SQLQuerier 抽象 *sql.DB 与 *sql.Tx 的公共查询能力
// （与 workspace / modelconfig 包同名接口一致，刻意不共享类型，避免包之间互相依赖）。
type SQLQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SQLStore 是 Store 的 SQLite 实现（表 tool_overrides，schema v4）。
type SQLStore struct {
	db  SQLQuerier
	now func() time.Time
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore 创建 SQLite 版覆盖存储。
func NewSQLStore(db SQLQuerier) *SQLStore {
	return &SQLStore{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// List 实现 Store。
func (s *SQLStore) List(ctx context.Context) (map[string]Override, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tool, risk, enabled FROM tool_overrides`)
	if err != nil {
		return nil, fmt.Errorf("customtools: 查询覆盖失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]Override{}
	for rows.Next() {
		var (
			name, risk string
			enabled    sql.NullInt64
		)
		if err := rows.Scan(&name, &risk, &enabled); err != nil {
			return nil, fmt.Errorf("customtools: 读取覆盖失败: %w", err)
		}
		var o Override
		if risk != "" {
			// 库里出现无法识别的值（被手工改过 / 将来降级）：忽略这个字段，而不是让整张表读不出来。
			if r, err := ParseRisk(risk); err == nil {
				o.Risk = &r
			}
		}
		if enabled.Valid {
			v := enabled.Int64 != 0
			o.Enabled = &v
		}
		if !o.IsEmpty() {
			out[name] = o
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("customtools: 遍历覆盖失败: %w", err)
	}
	return out, nil
}

// Set 实现 Store。
func (s *SQLStore) Set(ctx context.Context, name string, o Override) error {
	if o.IsEmpty() {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM tool_overrides WHERE tool = ?`, name); err != nil {
			return fmt.Errorf("customtools: 删除覆盖失败: %w", err)
		}
		return nil
	}
	risk := ""
	if o.Risk != nil {
		risk = riskString(*o.Risk)
	}
	var enabled any // nil → NULL
	if o.Enabled != nil {
		if *o.Enabled {
			enabled = 1
		} else {
			enabled = 0
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_overrides (tool, risk, enabled, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(tool) DO UPDATE SET
			risk = excluded.risk, enabled = excluded.enabled, updated_at = excluded.updated_at`,
		name, risk, enabled, s.now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("customtools: 保存覆盖失败: %w", err)
	}
	return nil
}

// riskString 与 tool.Risk.String() 相同，但对未知值返回空串（不把 "unknown" 写进库）。
func riskString(r tool.Risk) string {
	switch r {
	case tool.RiskSafe, tool.RiskModerate, tool.RiskDangerous:
		return r.String()
	default:
		return ""
	}
}
