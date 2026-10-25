// Package modelconfig 管理用户的模型配置（多模型支持）。
//
// 设计要点：
//   - 一个用户可以保存多套模型配置（不同服务商 / 不同模型）
//   - 恰好一个配置是默认（is_default）
//   - API Key 加密落盘，不以明文存储
//   - 配置文件放在数据库里，而非 .env：支持运行时增删改，无需重启
package modelconfig

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"agent_rebuild/internal/config"
)

// 领域错误。
var (
	ErrNotFound  = errors.New("modelconfig: 配置不存在")
	ErrConflict  = errors.New("modelconfig: 名称已存在")
	ErrInvalid   = errors.New("modelconfig: 参数不合法")
	ErrNoDefault = errors.New("modelconfig: 没有默认配置")
)

// Protocol 与 config.ProtocolXxx 保持一致，但此处独立定义以保持包边界清晰。
const (
	ProtocolOpenAI    = config.ProtocolOpenAI
	ProtocolAnthropic = config.ProtocolAnthropic
	ProtocolMock      = config.ProtocolMock
)

// Config 是一套模型配置。
type Config struct {
	ID string `json:"id"`
	// Name 用户自定义的显示名（唯一），如"工作用 DeepSeek"。
	Name string `json:"name"`
	// Protocol 决定用哪个 Runner。
	Protocol string `json:"protocol"`
	// Provider 决定预设端点与默认模型（可为空表示自定义）。
	Provider string `json:"provider"`
	// BaseURL 留空则用 provider 预设。
	BaseURL string `json:"base_url"`
	// Model 模型名。
	Model string `json:"model"`
	// SystemPrompt 该配置的系统提示。
	SystemPrompt string `json:"system_prompt"`
	// Temperature 采样温度（nil 表示用服务商默认）。
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens 最大生成 token（0 表示默认）。
	MaxTokens int `json:"max_tokens"`
	// IsDefault 是否为默认配置（全局唯一）。
	IsDefault bool `json:"is_default"`
	// HasAPIKey 表示是否已配置密钥（不返回密钥本身）。
	HasAPIKey bool      `json:"has_api_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// APIKey 仅在写入时使用，读取时不填充（避免泄漏）。
	APIKey string `json:"-"`
}

// Input 是创建/更新时的入参。
type Input struct {
	Name         string
	Protocol     string
	Provider     string
	BaseURL      string
	Model        string
	SystemPrompt string
	Temperature  *float64
	MaxTokens    int
	// APIKey 为空表示"不改动已有密钥"（更新场景）；
	// 创建时为空则从环境变量兜底。
	APIKey    string
	IsDefault bool
}

// Store 是模型配置的持久化接口。
type Store interface {
	Create(ctx context.Context, in Input) (*Config, error)
	Update(ctx context.Context, id string, in Input) (*Config, error)
	// Get 返回配置（不含明文 API Key，但 APIKey 字段会被填充供内部使用）。
	Get(ctx context.Context, id string) (*Config, error)
	// GetDefault 返回默认配置。
	GetDefault(ctx context.Context) (*Config, error)
	List(ctx context.Context) ([]*Config, error)
	Delete(ctx context.Context, id string) error
	// SetDefault 将指定配置设为默认（同时取消其他配置的默认标记）。
	SetDefault(ctx context.Context, id string) error
}

// Validate 校验入参。
func (in *Input) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Protocol = strings.ToLower(strings.TrimSpace(in.Protocol))
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.Model = strings.TrimSpace(in.Model)

	if in.Name == "" {
		return fmt.Errorf("%w: 名称不能为空", ErrInvalid)
	}
	if len([]rune(in.Name)) > 60 {
		return fmt.Errorf("%w: 名称过长（最多 60 字）", ErrInvalid)
	}
	switch in.Protocol {
	case ProtocolMock:
		// mock 不需要模型名与地址。
	case ProtocolOpenAI, ProtocolAnthropic:
		if in.Model == "" {
			return fmt.Errorf("%w: 需要指定模型名", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: 不支持的协议 %q（可选 openai / anthropic / mock）", ErrInvalid, in.Protocol)
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
		return fmt.Errorf("%w: temperature 应在 0~2 之间", ErrInvalid)
	}
	if in.MaxTokens < 0 {
		return fmt.Errorf("%w: max_tokens 不能为负", ErrInvalid)
	}
	return nil
}

// Cipher 负责 API Key 的加解密。
//
// 用 AES-GCM，密钥由「机器标识 + 用户主目录」派生。
// 这不是密钥管理的终极方案（真正的方案是 OS Keychain），
// 但足以避免「数据库文件被拷走后密钥明文泄漏」。
// 后续可替换为 macOS Keychain / Windows Credential Manager 实现。
type Cipher struct {
	key []byte
}

// NewCipher 派生加密密钥。
//
// secret 应包含机器与用户维度信息（如 userData 目录绝对路径），
// 这样数据库文件被拷到另一台机器后无法解密。
func NewCipher(secret string) *Cipher {
	sum := sha256.Sum256([]byte("agent_rebuild:" + secret))
	return &Cipher{key: sum[:]}
}

// Encrypt 加密明文。空串返回空串。
func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("modelconfig: 创建 cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("modelconfig: 创建 GCM 失败: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("modelconfig: 生成 nonce 失败: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密密文。空串返回空串。
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("modelconfig: 解码失败: %w", err)
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("modelconfig: 创建 cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("modelconfig: 创建 GCM 失败: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("modelconfig: 密文长度非法")
	}
	plain, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		// 解密失败常见原因：数据库被拷到另一台机器（派生密钥不同）。
		return "", fmt.Errorf("modelconfig: 解密失败（密钥可能来自其他设备）: %w", err)
	}
	return string(plain), nil
}

// maskKey 生成用于日志的安全表示。
func maskKey(key string) string {
	if key == "" {
		return "未设置"
	}
	if len(key) <= 8 {
		return "已设置"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

var _ = slog.Info
