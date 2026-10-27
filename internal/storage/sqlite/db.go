// Package sqlite 提供基于 SQLite 的持久化实现。
//
// 驱动选择：modernc.org/sqlite（纯 Go 翻译版）。
//
//   - CGO_ENABLED=0 可编译，产物仍是静态单文件（桌面端"拖进即用"不退化）
//   - 用户机器无需安装 SQLite，也不需要 C 编译器
//   - 代价：二进制增加约 2-3 MB，首次编译变慢
//
// 不使用 mattn/go-sqlite3：它需要 CGO + 系统 SQLite 库，会破坏单文件分发。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动
)

// ErrSchemaTooNew 表示数据库 schema 版本高于当前程序支持的版本。
// 出现这种情况说明用户用旧版程序打开了新版数据库，必须拒绝以免损坏数据。
var ErrSchemaTooNew = errors.New("sqlite: 数据库 schema 版本高于当前程序，请升级应用")

// schemaVersion 是当前程序实现的 schema 版本。
// 每次改动表结构时 +1，并在 migrate 里追加迁移步骤。
const schemaVersion = 3

// Open 打开（或创建）数据库并完成初始化。
//
// path 为空时使用内存数据库（测试用）；":memory:" 亦等价。
func Open(path string) (*DB, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: 打开数据库失败: %w", err)
	}

	// SQLite 的并发模型：只允许一个写者。
	// 连接池设得太大会导致 SQLITE_BUSY 频繁，这里限制为 1 个写连接。
	// 读操作实际也在同一连接上串行——对本项目（单用户本地应用）不是瓶颈。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := applyPragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &DB{db: db, path: path}, nil
}

// DB 封装底层连接池，供各 Store 复用。
type DB struct {
	db   *sql.DB
	path string
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.db.Close() }

// Path 返回数据库文件路径（内存库返回 ":memory:"）。
func (d *DB) Path() string { return d.path }

// SQL 暴露底层 *sql.DB，供同包内的 Store 实现使用。
func (d *DB) SQL() *sql.DB { return d.db }

// buildDSN 构造 DSN。
//
// 关键参数说明：
//   - _pragma=busy_timeout(5000)：写锁冲突时等待而非立即报错
//   - _pragma=journal_mode(WAL)：写不阻塞读，崩溃恢复更好
//   - _pragma=foreign_keys(1)：开启外键约束（删除会话级联删消息）
func buildDSN(path string) (string, error) {
	if path == "" || path == ":memory:" {
		return "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", nil
	}

	// 确保父目录存在（桌面端首次启动时 userData 目录可能已存在，但保险起见）。
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("sqlite: 创建数据目录失败: %w", err)
		}
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("sqlite: 解析路径失败: %w", err)
	}
	// DSN 里的 Windows 路径反斜杠需要转成斜杠。
	abs = filepath.ToSlash(abs)
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		abs,
	), nil
}

// applyPragmas 设置连接级 pragma（DSN 里已设大部分，这里补运行期检查）。
func applyPragmas(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("sqlite: 读取 journal_mode 失败: %w", err)
	}
	// 内存库的 journal_mode 固定为 memory，属正常。
	if !strings.EqualFold(mode, "wal") && !strings.EqualFold(mode, "memory") {
		slog.Warn("SQLite 未启用 WAL，并发写入性能可能下降", "journal_mode", mode)
	}
	return nil
}

// migrate 执行 schema 迁移。
//
// 用 SQLite 的 user_version pragma 记录版本，简单可靠（无需额外元数据表）。
func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("sqlite: 读取 schema 版本失败: %w", err)
	}

	if current > schemaVersion {
		return fmt.Errorf("%w（数据库=%d，程序=%d）", ErrSchemaTooNew, current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}

	slog.Info("开始迁移数据库", "from", current, "to", schemaVersion)

	// 迁移步骤按版本递增追加，每步必须幂等或包在事务里。
	steps := []struct {
		version int
		sql     string
	}{
		{version: 1, sql: schemaV1},
		{version: 2, sql: schemaV2},
		{version: 3, sql: schemaV3},
	}

	for _, step := range steps {
		if step.version <= current {
			continue
		}
		if err := runMigration(ctx, db, step.version, step.sql); err != nil {
			return err
		}
		slog.Info("数据库迁移完成", "version", step.version)
	}
	return nil
}

func runMigration(ctx context.Context, db *sql.DB, version int, statements string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: 开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range splitStatements(statements) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: 执行迁移 v%d 失败: %w\nSQL: %s", version, err, stmt)
		}
	}
	// user_version 不支持参数占位，只能拼接（version 是内部常量，无注入风险）。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("sqlite: 写入 schema 版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: 提交迁移失败: %w", err)
	}
	return nil
}

// splitStatements 按分号切分 SQL 语句（简单实现，够用即可）。
func splitStatements(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// schemaV1 是初始 schema。
//
// 设计要点：
//   - sessions / messages：会话与消息（含工具调用字段预留）
//   - model_configs：多模型配置（API Key 加密存储）
//   - 时间统一存 Unix 毫秒（整数，跨时区无歧义，排序快）
//   - meta 用 JSON 文本存，避免为少量扩展字段建表
const schemaV1 = `
CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,
    title         TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'idle',
    message_count INTEGER NOT NULL DEFAULT 0,
    meta          TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_updated_at
    ON sessions(updated_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS messages (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'complete',
    usage_json   TEXT NOT NULL DEFAULT '',
    error_json   TEXT NOT NULL DEFAULT '',
    tool_calls   TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    seq          INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session_seq
    ON messages(session_id, seq);

CREATE TABLE IF NOT EXISTS model_configs (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    protocol      TEXT NOT NULL,
    provider      TEXT NOT NULL,
    base_url      TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL,
    api_key_enc   TEXT NOT NULL DEFAULT '',
    system_prompt TEXT NOT NULL DEFAULT '',
    temperature   REAL,
    max_tokens    INTEGER NOT NULL DEFAULT 0,
    is_default    INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_model_configs_name
    ON model_configs(name);

CREATE TABLE IF NOT EXISTS checkpoints (
    session_id  TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 1,
    step_index  INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'running',
    payload     TEXT NOT NULL,
    updated_at  INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
`

// schemaV2 增加「会话选用的模型配置」。
//
// 为什么单独列而不塞进 meta：这个字段要参与查询与校验（例如
// 「某配置被哪些会话使用」），放 JSON 里无法建索引。
//
// 注意：ADD COLUMN 不支持 IF NOT EXISTS，因此迁移必须保证只执行一次
// （由 user_version 控制）。
const schemaV2 = `
ALTER TABLE sessions ADD COLUMN model_config_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sessions_model_config
    ON sessions(model_config_id);
`

// schemaV3 增加「会话绑定的 workspace」与「会话级审批模式覆盖」。
//
// 设计见 docs/14-workspace-and-session-presets.md：
//   - workspaces 是独立实体（可被多个会话复用），folders 用 JSON 文本列存储，
//     不拆子表——folders 不参与任何查询条件，拆表只会多一次 JOIN 成本。
//   - sessions.workspace_id 刻意不加外键约束：删除 workspace 不应级联影响
//     引用它的会话，语义上与 model_config_id 失效时的处理方式一致（使用时
//     才报错，而不是删除那一刻去扫描清洗所有会话）。
//   - sessions.approval_mode 为空表示"跟随进程级全局审批模式"，与
//     model_config_id 为空回退全局默认模型是同一套既有约定。
const schemaV3 = `
CREATE TABLE IF NOT EXISTS workspaces (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    folders    TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

ALTER TABLE sessions ADD COLUMN workspace_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN approval_mode TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sessions_workspace
    ON sessions(workspace_id);
`
