package sse

import "sync"

// Buffer 是会话维度的事件环形缓冲，用于 Last-Event-ID 重放。
// 超过容量时丢弃最旧事件；重放请求的 lastID 若已被裁剪，调用方应改发 resync。
type Buffer struct {
	mu         sync.RWMutex
	size       int
	buf        []Event // 按 ID 升序
	minDropped uint64  // 已被裁剪的最小 ID（用于判断是否需要 resync）
}

// NewBuffer 创建容量为 size 的缓冲（size <= 0 时默认 10000）。
func NewBuffer(size int) *Buffer {
	if size <= 0 {
		size = 10000
	}
	return &Buffer{size: size, buf: make([]Event, 0, size)}
}

// Push 追加事件，返回写入后的当前最大 ID。
// 当缓冲超过容量时，记录被丢弃的最旧事件 ID。
func (b *Buffer) Push(ev Event) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, ev)
	if len(b.buf) > b.size {
		// 记录被裁剪的最旧事件 ID
		if len(b.buf) > 0 {
			b.minDropped = b.buf[0].ID
		}
		b.buf = b.buf[len(b.buf)-b.size:]
	}
	return ev.ID
}

// LastID 返回当前最大事件 ID（无事件时为 0）。
func (b *Buffer) LastID() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.buf) == 0 {
		return 0
	}
	return b.buf[len(b.buf)-1].ID
}

// Since 返回 ID 严格大于 after 的事件副本。
//
// ok=false 表示无法保证连续性（after 早于缓冲下界，中间事件已被裁剪），
// 调用方应改发 resync + 全量消息快照。
func (b *Buffer) Since(after uint64) (events []Event, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// 无事件：只有 after==0 才是「本来就没有」的合法连续状态。
	if len(b.buf) == 0 {
		return nil, after == 0
	}
	oldest := b.buf[0].ID
	// 缓冲从 oldest 开始，只有 after >= oldest-1 才能无缝续上。
	if after+1 < oldest {
		return nil, false
	}

	out := make([]Event, 0, len(b.buf))
	for _, ev := range b.buf {
		if ev.ID > after {
			out = append(out, ev)
		}
	}
	return out, true
}
