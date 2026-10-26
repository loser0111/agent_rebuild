package session

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 存储层错误，api 层据此映射 HTTP 状态码。
var (
	// ErrNotFound 资源不存在（会话或消息）。
	ErrNotFound = errors.New("session: not found")
	// ErrConflict 唯一键冲突（重复创建同一 ID）。
	ErrConflict = errors.New("session: conflict")
	// ErrInvalidCursor 列表游标无法解析或已失效。
	ErrInvalidCursor = errors.New("session: invalid cursor")
	// ErrInvalidArgument 入参不满足领域约束（空 ID、非法角色等）。
	ErrInvalidArgument = errors.New("session: invalid argument")

	// ErrCheckpointInvalid checkpoint 结构不合法（状态缺失、挂起却无 Pending 等）。
	ErrCheckpointInvalid = errors.New("session: invalid checkpoint")
	// ErrCheckpointNoSession checkpoint 缺少 session_id。
	//
	// 单独一个错误：这是最容易犯的错（调用方忘了传会话 ID），
	// 而与「结构非法」混在一起时很难定位。
	ErrCheckpointNoSession = errors.New("session: checkpoint without session_id")
	// ErrCheckpointNotFound 会话没有 checkpoint（通常是已经完成或被清理）。
	ErrCheckpointNotFound = errors.New("session: checkpoint not found")
)

// ListOptions 列表查询参数。
type ListOptions struct {
	// Limit 每页条数，<=0 时取 50，上限 200。
	Limit int
	// Cursor 上一页返回的 next_cursor（即上一页最后一条的 session ID）。
	Cursor string
}

// Store 是会话持久化的可插拔边界。
//
// 实现约定（所有实现都必须满足，contract_test.go 会逐条校验）：
//  1. 并发安全：可被多个 goroutine 同时调用；
//  2. 深拷贝：返回值不与内部状态共享任何可变内存（含 Meta map、Usage/Error 指针）；
//  3. 读改写原子化：Update / UpdateMessage 的 fn 在锁（或事务）内执行；
//  4. 不吞 ctx：ctx 已取消时立即返回 ctx.Err()。
//
// 换 SQLite / Redis 只需提供新的实现并改 STORE_DRIVER，业务代码零改动。
type Store interface {
	// Create 写入一个新会话；ID 重复返回 ErrConflict。
	Create(ctx context.Context, s *Session) error
	// Get 读取会话元信息（不含消息）。
	Get(ctx context.Context, id string) (*Session, error)
	// List 按 updated_at 倒序分页，返回 (当前页, 下一页游标, error)。
	List(ctx context.Context, opts ListOptions) ([]*Session, string, error)
	// Update 在锁内执行 fn 修改会话，返回更新后的副本。
	Update(ctx context.Context, id string, fn func(*Session) error) (*Session, error)
	// Delete 删除会话及其全部消息。
	Delete(ctx context.Context, id string) error

	// AppendMessage 追加一条消息，并同步维护会话的 message_count / updated_at。
	AppendMessage(ctx context.Context, m *Message) error
	// UpdateMessage 在锁内执行 fn 修改指定消息（如把 streaming 改为 complete）。
	UpdateMessage(ctx context.Context, sessionID, msgID string, fn func(*Message) error) error
	// Messages 返回会话的全部消息（时间正序副本）。
	Messages(ctx context.Context, sessionID string) ([]Message, error)
	// RecoverStaleRunning 把卡在 running 的会话复位为 idle，返回处理条数。
	//
	// 为什么要单独一个方法：running 状态只应存在于「进程内有任务在跑」期间。
	// 进程崩溃 / 被强杀 / 容器重启后，数据库里会留下永久的 running，
	// 前端据此提示「仍在生成中」，用户再怎么刷新都等不到结果。
	// 启动时调用一次即可自愈。
	RecoverStaleRunning(ctx context.Context) (int, error)
}

// Snapshot 是会话数据的全量可序列化快照。
// 它是持久化扩展点：任何实现都可以借助它把状态落到文件/数据库。
type Snapshot struct {
	Version  int                  `json:"version"`
	TakenAt  time.Time            `json:"taken_at"`
	Sessions []Session            `json:"sessions"`
	Messages map[string][]Message `json:"messages"`
}

// SnapshotVersion 当前快照格式版本，便于后续迁移。
const SnapshotVersion = 1

// Snapshotter 是可选扩展接口：实现它的 Store 可导出/导入全量状态。
//
// 用途：JSON 文件持久化、SQLite 冷启动回填、测试夹具、备份恢复。
// 业务层只依赖 Store，不需要感知该接口。
type Snapshotter interface {
	Snapshot(ctx context.Context) (*Snapshot, error)
	Restore(ctx context.Context, snap *Snapshot) error
}

// CheckpointStore 是断点持久化的可插拔边界。
//
// 为什么独立于 Store 而不是加进 Store 接口：
//   - checkpoint 只有 SQLite 驱动需要（内存驱动的进程重启即丢，无断点可言）；
//     合进 Store 会强迫所有实现（含测试假实现）都补 4 个方法。
//   - 语义上也不同：Store 管「会话与消息的事实」，CheckpointStore 管
//     「执行进度」。混在一起会让 Store 越来越臃肿。
//
// 实现约定与 Store 一致：并发安全、深拷贝、原子读改写、不吞 ctx。
type CheckpointStore interface {
	// SaveCheckpoint 写入或覆盖会话的 checkpoint（一个会话只有一条）。
	//
	// 覆盖语义（而非报冲突）是刻意的：新一轮执行会正常覆盖上一轮
	// 已完成的断点，否则每轮都要先显式删除。
	SaveCheckpoint(ctx context.Context, cp *Checkpoint) error
	// LoadCheckpoint 读取会话当前的 checkpoint。
	// 不存在时返回 ErrCheckpointNotFound。
	LoadCheckpoint(ctx context.Context, sessionID string) (*Checkpoint, error)
	// DeleteCheckpoint 删除会话的 checkpoint（执行正常结束后清理）。
	// 不存在时返回 nil（幂等），便于在 defer 里无脑调用。
	DeleteCheckpoint(ctx context.Context, sessionID string) error
}

// normalizeListOptions 归一化分页参数，避免各实现重复写边界判断。
func normalizeListOptions(opts ListOptions) (limit int, err error) {
	limit = opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if len(opts.Cursor) > 128 {
		return 0, fmt.Errorf("%w: 游标过长", ErrInvalidCursor)
	}
	return limit, nil
}
