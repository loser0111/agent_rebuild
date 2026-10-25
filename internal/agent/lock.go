package agent

import "sync"

// Locks 是会话级串行锁：同一会话同时只允许一个生成在跑。
// 用 TryLock 而非阻塞等待——失败立即回 409，用户能得到明确反馈。
type Locks struct {
	m sync.Map // sessionID -> *sync.Mutex
}

// NewLocks 创建锁表。
func NewLocks() *Locks { return &Locks{} }

// TryLock 尝试占用会话锁；ok=false 表示已有生成在跑。
// 成功时必须调用 Unlock，否则该会话永久忙。
func (l *Locks) TryLock(sessionID string) (unlock func(), ok bool) {
	mu := l.mutex(sessionID)
	if !mu.TryLock() {
		return func() {}, false
	}
	return func() { mu.Unlock() }, true
}

// IsBusy 查询会话是否正在生成。
func (l *Locks) IsBusy(sessionID string) bool {
	v, ok := l.m.Load(sessionID)
	if !ok {
		return false
	}
	mu := v.(*sync.Mutex)
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

// Forget 在会话删除时清理锁条目，避免 map 无限增长。
func (l *Locks) Forget(sessionID string) { l.m.Delete(sessionID) }

func (l *Locks) mutex(sessionID string) *sync.Mutex {
	v, _ := l.m.LoadOrStore(sessionID, &sync.Mutex{})
	return v.(*sync.Mutex)
}
