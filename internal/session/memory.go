package session

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore 是进程内实现：map + RWMutex，重启即丢数据（Demo 可接受）。
//
// 并发模型：
//   - 读操作（Get / List / Messages）持读锁，可并行；
//   - 写操作（Create / Update / Delete / AppendMessage / UpdateMessage）持写锁；
//   - 会话元信息与消息列表在同一把锁保护下，保证 message_count 与真实消息数不漂移。
//
// 它同时实现了 Snapshotter，可作为后续 SQLite/文件持久化的冷启动来源。
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	messages map[string][]Message
	// cursorIndex 记录每个会话在排序列表中的位置无关，这里仅用于分页稳定性：
	// 游标即会话 ID，无需额外索引，保持实现简单。
	maxMsgs int
	// now 便于测试注入固定时钟（时钟漂移会让「updated_at 倒序」断言不稳定）。
	now func() time.Time
}

// NewMemoryStore 创建内存存储。maxMsgs 为单会话消息上限，<=0 表示不限制。
func NewMemoryStore(maxMsgs int) *MemoryStore {
	return &MemoryStore{
		sessions: make(map[string]*Session),
		messages: make(map[string][]Message),
		maxMsgs:  maxMsgs,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

var (
	_ Store       = (*MemoryStore)(nil)
	_ Snapshotter = (*MemoryStore)(nil)
)

// SetClock 注入时钟（测试用）。传 nil 恢复系统时钟。
func (s *MemoryStore) SetClock(clock func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if clock == nil {
		s.now = func() time.Time { return time.Now().UTC() }
		return
	}
	s.now = clock
}

func (s *MemoryStore) Create(ctx context.Context, sess *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("%w: 会话不能为空", ErrInvalidArgument)
	}
	if err := sess.Validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sess.ID]; ok {
		return ErrConflict
	}
	now := s.now()
	cp := sess.Clone()
	// 统一时间戳：调用方未填时补全，避免出现零值时间。
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	if cp.Status == "" {
		cp.Status = SessionIdle
	}
	if cp.MessageCount != 0 {
		// 新建会话不应自带计数，防止与真实消息列表不一致。
		cp.MessageCount = 0
	}
	s.sessions[cp.ID] = cp
	s.messages[cp.ID] = nil
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return sess.Clone(), nil
}

func (s *MemoryStore) List(ctx context.Context, opts ListOptions) ([]*Session, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit, err := normalizeListOptions(opts)
	if err != nil {
		return nil, "", err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	all := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		all = append(all, sess.Clone())
	}
	// updated_at 倒序；同一时刻按 ID 倒序，保证分页结果稳定可复现。
	sort.Slice(all, func(i, j int) bool {
		if all[i].UpdatedAt.Equal(all[j].UpdatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})

	start := 0
	if opts.Cursor != "" {
		found := false
		for i, sess := range all {
			if sess.ID == opts.Cursor {
				start = i + 1
				found = true
				break
			}
		}
		// 游标指向的会话已被删除时，从头部重新开始，避免用户看到空列表。
		if !found {
			start = 0
		}
	}

	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	if start > len(all) {
		start = len(all)
	}
	page := all[start:end]

	next := ""
	if end < len(all) && len(page) > 0 {
		next = page[len(page)-1].ID
	}
	return page, next, nil
}

func (s *MemoryStore) Update(ctx context.Context, id string, fn func(*Session) error) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("%w: Update 需要非空 fn", ErrInvalidArgument)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	// 在副本上执行 fn 并校验，失败则完全不改变内部状态（天然回滚）。
	work := sess.Clone()
	if err := fn(work); err != nil {
		return nil, err
	}
	if err := work.Validate(); err != nil {
		return nil, err
	}
	// ID 不可通过 Update 篡改，否则消息会失去归属。
	work.ID = sess.ID
	work.CreatedAt = sess.CreatedAt
	work.MessageCount = len(s.messages[id])
	work.UpdatedAt = s.now()
	s.sessions[id] = work
	return work.Clone(), nil
}

func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(s.sessions, id)
	delete(s.messages, id)
	return nil
}

func (s *MemoryStore) AppendMessage(ctx context.Context, m *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: 消息不能为空", ErrInvalidArgument)
	}
	if err := m.Validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[m.SessionID]
	if !ok {
		return ErrNotFound
	}
	now := s.now()
	cp := m.Clone()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now

	s.messages[m.SessionID] = append(s.messages[m.SessionID], cp)

	// 超出上限时裁剪最旧的非 system 消息；system 消息始终保留。
	if s.maxMsgs > 0 && len(s.messages[m.SessionID]) > s.maxMsgs {
		s.messages[m.SessionID] = trimOldest(s.messages[m.SessionID], s.maxMsgs)
	}
	sess.MessageCount = len(s.messages[m.SessionID])
	sess.UpdatedAt = now
	return nil
}

func (s *MemoryStore) UpdateMessage(ctx context.Context, sessionID, msgID string, fn func(*Message) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("%w: UpdateMessage 需要非空 fn", ErrInvalidArgument)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	msgs, ok := s.messages[sessionID]
	if !ok {
		return ErrNotFound
	}
	for i := range msgs {
		if msgs[i].ID != msgID {
			continue
		}
		// 同样在副本上修改：fn 报错时不污染已存数据。
		work := msgs[i].Clone()
		if err := fn(&work); err != nil {
			return err
		}
		if err := work.Validate(); err != nil {
			return err
		}
		// 归属与创建时间不可变。
		work.ID = msgs[i].ID
		work.SessionID = msgs[i].SessionID
		work.CreatedAt = msgs[i].CreatedAt
		work.UpdatedAt = s.now()
		msgs[i] = work
		s.messages[sessionID] = msgs

		if sess, ok := s.sessions[sessionID]; ok {
			sess.UpdatedAt = work.UpdatedAt
		}
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	src := s.messages[sessionID]
	out := make([]Message, len(src))
	for i := range src {
		out[i] = src[i].Clone()
	}
	return out, nil
}

// RecoverStaleRunning 把卡在 running 的会话复位为 idle。
//
// 内存存储在进程重启后本就什么都不剩，这里主要为了满足接口契约
// （contract_test 对所有实现一视同仁）。同进程内仍可能有需要复位的
// 场景：任务 goroutine 异常退出但没走到 finish。
func (s *MemoryStore) RecoverStaleRunning(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	now := s.now()
	for id, sess := range s.sessions {
		if sess.Status != SessionRunning {
			continue
		}
		sess.Status = SessionIdle
		sess.UpdatedAt = now
		s.sessions[id] = sess
		n++
	}
	return n, nil
}

// Snapshot 导出一份深拷贝的全量状态，可用于落盘或迁移到其他实现。
func (s *MemoryStore) Snapshot(ctx context.Context) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := &Snapshot{
		Version:  SnapshotVersion,
		TakenAt:  s.now(),
		Sessions: make([]Session, 0, len(s.sessions)),
		Messages: make(map[string][]Message, len(s.messages)),
	}
	for _, sess := range s.sessions {
		snap.Sessions = append(snap.Sessions, *sess.Clone())
	}
	sort.Slice(snap.Sessions, func(i, j int) bool {
		return snap.Sessions[i].CreatedAt.Before(snap.Sessions[j].CreatedAt)
	})
	for id, msgs := range s.messages {
		cloned := make([]Message, len(msgs))
		for i := range msgs {
			cloned[i] = msgs[i].Clone()
		}
		snap.Messages[id] = cloned
	}
	return snap, nil
}

// Restore 用快照整体替换当前状态（先校验版本，不做部分合并）。
func (s *MemoryStore) Restore(ctx context.Context, snap *Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snap == nil {
		return fmt.Errorf("%w: 快照不能为空", ErrInvalidArgument)
	}
	if snap.Version != SnapshotVersion {
		return fmt.Errorf("%w: 快照版本 %d 不受支持（期望 %d）", ErrInvalidArgument, snap.Version, SnapshotVersion)
	}

	sessions := make(map[string]*Session, len(snap.Sessions))
	messages := make(map[string][]Message, len(snap.Messages))
	for i := range snap.Sessions {
		sess := snap.Sessions[i].Clone()
		if err := sess.Validate(); err != nil {
			return err
		}
		if _, dup := sessions[sess.ID]; dup {
			return fmt.Errorf("%w: 快照中存在重复会话 %s", ErrConflict, sess.ID)
		}
		sessions[sess.ID] = sess

		src := snap.Messages[sess.ID]
		cloned := make([]Message, len(src))
		for j := range src {
			cloned[j] = src[j].Clone()
		}
		messages[sess.ID] = cloned
		// 计数以真实消息为准，修掉快照里可能过期的值。
		sess.MessageCount = len(cloned)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = sessions
	s.messages = messages
	return nil
}

// Len 返回当前会话数量（调试/测试用）。
func (s *MemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// trimOldest 保留全部 system 消息与最近的非 system 消息，总数不超过 keep。
//
// 边界：keep <= 0 视为不限制；system 消息数量本身超过 keep 时，
// 保留全部 system 消息（它们是上下文契约，宁可超限也不能丢），不会 panic。
func trimOldest(msgs []Message, keep int) []Message {
	if keep <= 0 || len(msgs) <= keep {
		return msgs
	}
	out := make([]Message, 0, keep)
	nonSystem := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleSystem {
			out = append(out, m)
			continue
		}
		nonSystem = append(nonSystem, m)
	}

	// system 已占用全部配额时，直接返回（保留全部 system + 不丢弃更多信息）。
	remaining := keep - len(out)
	if remaining <= 0 {
		res := make([]Message, 0, len(out)+len(nonSystem))
		res = append(res, out...)
		res = append(res, nonSystem...)
		return res
	}
	if len(nonSystem) <= remaining {
		out = append(out, nonSystem...)
		return out
	}
	out = append(out, nonSystem[len(nonSystem)-remaining:]...)
	return out
}
