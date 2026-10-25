package modelconfig

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLQuerier 抽象 *sql.DB 与 *sql.Tx 的公共查询能力。
type SQLQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLTx 在 SQLQuerier 之上补充事务控制。
type SQLTx interface {
	SQLQuerier
	Commit() error
	Rollback() error
}

// SQLStore 是 Store 的 SQLite 实现。
type SQLStore struct {
	db     SQLQuerier
	cipher *Cipher
	now    func() time.Time
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore 创建 SQLite 版模型配置存储。
func NewSQLStore(db SQLQuerier, keySource string) *SQLStore {
	return &SQLStore{
		db:     db,
		cipher: NewCipher(keySource),
		now:    func() time.Time { return time.Now().UTC() },
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

// Create 新建配置。
//
// 若这是第一条配置，自动设为默认（保证总有默认可用）。
func (s *SQLStore) Create(ctx context.Context, in Input) (*Config, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_configs`).Scan(&total); err != nil {
		return nil, fmt.Errorf("modelconfig: 统计配置失败: %w", err)
	}
	isDefault := in.IsDefault || total == 0

	encKey, err := s.cipher.Encrypt(in.APIKey)
	if err != nil {
		return nil, err
	}

	now := s.now()
	id := newID()

	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if isDefault {
		if _, err := tx.ExecContext(ctx, `UPDATE model_configs SET is_default = 0`); err != nil {
			return nil, fmt.Errorf("modelconfig: 清除默认标记失败: %w", err)
		}
	}

	var temp any
	if in.Temperature != nil {
		temp = *in.Temperature
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_configs
			(id, name, protocol, provider, base_url, model, api_key_enc, system_prompt,
			 temperature, max_tokens, is_default, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.Protocol, in.Provider, in.BaseURL, in.Model, encKey, in.SystemPrompt,
		temp, in.MaxTokens, boolToInt(isDefault), toMillis(now), toMillis(now),
	); err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q", ErrConflict, in.Name)
		}
		return nil, fmt.Errorf("modelconfig: 插入配置失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("modelconfig: 提交失败: %w", err)
	}

	return s.Get(ctx, id)
}

// Update 修改配置。
//
// APIKey 为空表示「保留原密钥」（前端不应拿到明文，也无法回填）。
func (s *SQLStore) Update(ctx context.Context, id string, in Input) (*Config, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	existing, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// 空 APIKey = 不改动。
	encKey := ""
	if strings.TrimSpace(in.APIKey) != "" {
		encKey, err = s.cipher.Encrypt(in.APIKey)
		if err != nil {
			return nil, err
		}
	}

	now := s.now()
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if in.IsDefault && !existing.IsDefault {
		if _, err := tx.ExecContext(ctx, `UPDATE model_configs SET is_default = 0`); err != nil {
			return nil, fmt.Errorf("modelconfig: 清除默认标记失败: %w", err)
		}
	}

	var temp any
	if in.Temperature != nil {
		temp = *in.Temperature
	}

	if encKey != "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE model_configs
			SET name = ?, protocol = ?, provider = ?, base_url = ?, model = ?,
			    api_key_enc = ?, system_prompt = ?, temperature = ?, max_tokens = ?,
			    is_default = ?, updated_at = ?
			WHERE id = ?`,
			in.Name, in.Protocol, in.Provider, in.BaseURL, in.Model, encKey, in.SystemPrompt,
			temp, in.MaxTokens, boolToInt(in.IsDefault), toMillis(now), id)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE model_configs
			SET name = ?, protocol = ?, provider = ?, base_url = ?, model = ?,
			    system_prompt = ?, temperature = ?, max_tokens = ?,
			    is_default = ?, updated_at = ?
			WHERE id = ?`,
			in.Name, in.Protocol, in.Provider, in.BaseURL, in.Model, in.SystemPrompt,
			temp, in.MaxTokens, boolToInt(in.IsDefault), toMillis(now), id)
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q", ErrConflict, in.Name)
		}
		return nil, fmt.Errorf("modelconfig: 更新配置失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("modelconfig: 提交失败: %w", err)
	}

	// 保证至少有一个默认：若把唯一默认改成了非默认，则把最早的设为默认。
	if !in.IsDefault && existing.IsDefault {
		if err := s.ensureDefault(ctx); err != nil {
			return nil, err
		}
	}

	return s.Get(ctx, id)
}

// Get 读取配置（含解密后的 APIKey，仅供服务端内部使用）。
func (s *SQLStore) Get(ctx context.Context, id string) (*Config, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, protocol, provider, base_url, model, api_key_enc, system_prompt,
		       temperature, max_tokens, is_default, created_at, updated_at
		FROM model_configs WHERE id = ?`, id)

	cfg, encKey, err := s.scan(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if encKey != "" {
		plain, decErr := s.cipher.Decrypt(encKey)
		if decErr != nil {
			// 解密失败不应让整个配置不可用：标记为无密钥并记录，让用户重新填。
			cfg.HasAPIKey = false
			cfg.APIKey = ""
		} else {
			cfg.HasAPIKey = true
			cfg.APIKey = plain
		}
	}
	return cfg, nil
}

// GetDefault 返回默认配置。
func (s *SQLStore) GetDefault(ctx context.Context) (*Config, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM model_configs WHERE is_default = 1 LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoDefault
	}
	if err != nil {
		return nil, fmt.Errorf("modelconfig: 查询默认配置失败: %w", err)
	}
	return s.Get(ctx, id)
}

// List 返回全部配置（按默认优先、更新时间倒序）。不含明文密钥。
func (s *SQLStore) List(ctx context.Context) ([]*Config, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, protocol, provider, base_url, model, api_key_enc, system_prompt,
		       temperature, max_tokens, is_default, created_at, updated_at
		FROM model_configs
		ORDER BY is_default DESC, updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("modelconfig: 查询配置失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Config, 0, 8)
	for rows.Next() {
		cfg, encKey, scanErr := s.scan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		// 列表不返回明文，只标记是否存在。
		cfg.HasAPIKey = encKey != ""
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("modelconfig: 遍历配置失败: %w", err)
	}
	return out, nil
}

// Delete 删除配置。删除默认配置后自动指定新的默认。
func (s *SQLStore) Delete(ctx context.Context, id string) error {
	existing, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM model_configs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("modelconfig: 删除配置失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if existing.IsDefault {
		if err := s.ensureDefault(ctx); err != nil {
			return err
		}
	}
	return nil
}

// SetDefault 指定默认配置。
func (s *SQLStore) SetDefault(ctx context.Context, id string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM model_configs WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("modelconfig: 查询配置失败: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE model_configs SET is_default = 0`); err != nil {
		return fmt.Errorf("modelconfig: 清除默认标记失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE model_configs SET is_default = 1, updated_at = ? WHERE id = ?`,
		toMillis(s.now()), id); err != nil {
		return fmt.Errorf("modelconfig: 设置默认失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("modelconfig: 提交失败: %w", err)
	}
	return nil
}

// ensureDefault 在没有默认配置时，把最早创建的一条设为默认。
func (s *SQLStore) ensureDefault(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_configs WHERE is_default = 1`).Scan(&count); err != nil {
		return fmt.Errorf("modelconfig: 统计默认配置失败: %w", err)
	}
	if count > 0 {
		return nil
	}

	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM model_configs ORDER BY created_at ASC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 一条配置都没有，无需处理
	}
	if err != nil {
		return fmt.Errorf("modelconfig: 查询候选默认失败: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE model_configs SET is_default = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("modelconfig: 设置兜底默认失败: %w", err)
	}
	return nil
}

// ---- 内部辅助 ----

type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// beginTx 在支持事务的前提下开启事务；否则退化为直接用底层连接
// （仅内存 fake 场景会出现，生产环境总是 SQLite）。
func (s *SQLStore) beginTx(ctx context.Context) (SQLTx, error) {
	if b, ok := s.db.(txBeginner); ok {
		tx, err := b.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("modelconfig: 开启事务失败: %w", err)
		}
		return tx, nil
	}
	return noopTx{SQLQuerier: s.db}, nil
}

// noopTx 是事务的退化实现：Commit/Rollback 均为空操作。
type noopTx struct {
	SQLQuerier
}

func (noopTx) Commit() error   { return nil }
func (noopTx) Rollback() error { return nil }

func (s *SQLStore) scan(sc interface{ Scan(...any) error }) (*Config, string, error) {
	var (
		cfg       Config
		temp      sql.NullFloat64
		maxTokens int
		isDefault int
		created   int64
		updated   int64
		encKey    string
	)
	if err := sc.Scan(&cfg.ID, &cfg.Name, &cfg.Protocol, &cfg.Provider, &cfg.BaseURL,
		&cfg.Model, &encKey, &cfg.SystemPrompt, &temp, &maxTokens, &isDefault,
		&created, &updated); err != nil {
		return nil, "", err
	}
	if temp.Valid {
		v := temp.Float64
		cfg.Temperature = &v
	}
	cfg.MaxTokens = maxTokens
	cfg.IsDefault = isDefault == 1
	cfg.CreatedAt = fromMillis(created)
	cfg.UpdatedAt = fromMillis(updated)
	return &cfg, encKey, nil
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("mc_%d", time.Now().UnixNano())
	}
	return "mc_" + hex.EncodeToString(b[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func toMillis(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
