// Package workspace 管理用户的 workspace 配置：一个 workspace 绑定若干个
// 本地文件夹，供会话引用（见 docs/14-workspace-and-session-presets.md）。
//
// 设计要点：
//   - 一个 workspace 可以被多个会话复用（同一个仓库相关的多次对话），
//     因此做成独立实体（照 internal/modelconfig 的先例），而不是直接
//     把文件夹列表塞进 session 表。
//   - 本包只负责"绑定关系"的持久化与校验，**不参与任何工具调用时的路径
//     权限判断**——tool.Registry 目前仍然只认启动时配置的单一根目录。
//     把两者接起来是后续独立的一次改动（见文档第 6 节已知限制）。
package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// 领域错误。
var (
	// ErrNotFound workspace 不存在。
	ErrNotFound = errors.New("workspace: 不存在")
	// ErrInvalid 入参不满足领域约束。
	ErrInvalid = errors.New("workspace: 参数不合法")
)

// nameMaxRunes 是 Name 的最大字数上限（与 modelconfig.Input 的 Name 上限一致）。
const nameMaxRunes = 60

// Workspace 是一个「会话可绑定的文件夹集合」。
type Workspace struct {
	ID string `json:"id"`
	// Name 用户自定义的显示名，纯展示用，不要求全局唯一
	// （不同场景下用同一个名字描述不同的文件夹组合是合理的）。
	Name string `json:"name"`
	// Folders 绑定的本地文件夹绝对路径列表，至少一个，创建/更新时去重。
	Folders   []string  `json:"folders"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Input 是创建 / 更新时的入参。
type Input struct {
	Name    string
	Folders []string
}

// Store 是 workspace 的持久化接口。
type Store interface {
	Create(ctx context.Context, in Input) (*Workspace, error)
	Get(ctx context.Context, id string) (*Workspace, error)
	List(ctx context.Context) ([]*Workspace, error)
	Update(ctx context.Context, id string, in Input) (*Workspace, error)
	Delete(ctx context.Context, id string) error
}

// Validate 校验并归一化入参（原地修改 in.Name / in.Folders）。
//
// 归一化规则：
//   - Name 去首尾空白；
//   - Folders 逐个 TrimSpace、filepath.Clean，丢弃空字符串，按出现顺序去重；
//   - 要求至少保留 1 个文件夹，且每个必须是绝对路径——相对路径在"跨会话/
//     跨进程重新打开"时含义会漂移（相对于谁的 cwd？），直接拒绝更安全。
func (in *Input) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return fmt.Errorf("%w: 名称不能为空", ErrInvalid)
	}
	if len([]rune(in.Name)) > nameMaxRunes {
		return fmt.Errorf("%w: 名称过长（最多 %d 字）", ErrInvalid, nameMaxRunes)
	}

	seen := make(map[string]struct{}, len(in.Folders))
	cleaned := make([]string, 0, len(in.Folders))
	for _, raw := range in.Folders {
		f := strings.TrimSpace(raw)
		if f == "" {
			continue
		}
		f = filepath.Clean(f)
		if !filepath.IsAbs(f) {
			return fmt.Errorf("%w: 文件夹必须是绝对路径: %q", ErrInvalid, raw)
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		cleaned = append(cleaned, f)
	}
	if len(cleaned) == 0 {
		return fmt.Errorf("%w: 至少需要绑定一个文件夹", ErrInvalid)
	}
	in.Folders = cleaned
	return nil
}
