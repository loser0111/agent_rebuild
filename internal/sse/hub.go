package sse

import "sync"

// Hub 是会话（key）维度的事件分发中心：
//   - 为每个会话分配单调递增的序号（seq），写入环形缓冲；
//   - 把事件 fan-out 给该会话的所有实时订阅者（GET /stream）；
//   - 支持按 Last-Event-ID 重放已缓冲事件。
//
// 序号由 Hub 统一分配，是「事件 ID 全局单调」的唯一来源，
// 避免 handler 与订阅者各自编号导致 last_event_id 语义错乱。
type Hub struct {
	mu                    sync.Mutex
	size                  int
	subscriberBufferSize  int
	maxMessageDeltaChunks int
	chans                 map[string]*hubChannel
}

type hubChannel struct {
	buf     *Buffer
	subs    map[int64]chan Event
	nextSub int64
	nextID  uint64
}

// NewHub 创建分发中心。
// size 为每会话保留的事件条数（<=0 取默认 10000）。
// subscriberBufferSize 为每个订阅者的事件队列缓冲大小（<=0 取默认 4096）。
// maxMessageDeltaChunks 为单条消息的 delta 事件最大缓冲数（<=0 取默认 50000）。
func NewHub(size int, subscriberBufferSize int, maxMessageDeltaChunks int) *Hub {
	if size <= 0 {
		size = 10000
	}
	if subscriberBufferSize <= 0 {
		subscriberBufferSize = 4096
	}
	if maxMessageDeltaChunks <= 0 {
		maxMessageDeltaChunks = 50000
	}
	return &Hub{
		size:                  size,
		subscriberBufferSize:  subscriberBufferSize,
		maxMessageDeltaChunks: maxMessageDeltaChunks,
		chans:                 make(map[string]*hubChannel),
	}
}

// Publish 为事件分配序号并广播，返回实际写入（含序号）的事件。
//
// build 接收已分配的 seq，用于把 seq 填进业务负载（如 MessageDeltaData.Seq），
// 这样负载里的 seq 与 SSE 帧的 id 永远一致。
func (h *Hub) Publish(key string, build func(seq uint64) Event) Event {
	return h.publish(key, build, false)
}

// PublishEphemeral 发布一个「不参与重放」的事件。
//
// 用于 message.start 这类「连接握手」事件：它需要有 seq 供前端对齐，
// 但重连时**不应**被重放（否则每次重连都会收到重复的 start）。
// 序号仍从同一计数器分配，保证全局单调。
func (h *Hub) PublishEphemeral(key string, build func(seq uint64) Event) Event {
	return h.publish(key, build, true)
}

func (h *Hub) publish(key string, build func(seq uint64) Event, ephemeral bool) Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.channel(key)
	st.nextID++
	ev := build(st.nextID)
	ev.ID = st.nextID
	if !ephemeral {
		st.buf.Push(ev)
	}

	// 慢订阅者处理：缓冲满时采用**防守型缓冲策略**而非直接丢弃。
	// 第一次溢出发送警告事件，让前端主动重连拉取全量数据。
	// 这样绝不会无声地丢失任何消息内容。
	for _, ch := range st.subs {
		select {
		case ch <- ev:
		default:
			// 缓冲满：发送警告事件，要求前端重连
			warningEv := Event{
				ID:   st.nextID + 1,
				Name: EventBufferOverflow,
				Data: map[string]any{
					"session_id": key,
					"reason":     "订阅缓冲溢出，请重新连接以同步完整状态",
				},
			}
			select {
			case ch <- warningEv:
			default:
				// 即使警告也放不进去，就算了（已经很满了）
			}
		}
	}
	return ev
}

// Subscribe 订阅某会话的事件。
//
// after 为客户端已收到的最后一个事件 ID：
//   - 返回 replay：ID 严格大于 after 的缓冲事件；
//   - ok=false 表示 after 早于缓冲下界（差距过大），调用方应改发 resync；
//   - live 为后续实时事件通道，调用方必须调用 cancel 释放。
func (h *Hub) Subscribe(key string, after uint64) (replay []Event, live <-chan Event, cancel func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.channel(key)
	replay, ok = st.buf.Since(after)

	st.nextSub++
	id := st.nextSub
	ch := make(chan Event, h.subscriberBufferSize)
	st.subs[id] = ch

	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur, exists := h.chans[key]; exists {
			if sub, exists := cur.subs[id]; exists {
				delete(cur.subs, id)
				// 关闭前必须先从 map 移除，且 Publish 持同一把锁，故不会向已关闭通道发送。
				close(sub)
			}
			// 无订阅者且缓冲为空时回收，避免 map 随会话数无限增长。
			if len(cur.subs) == 0 && cur.buf.LastID() == 0 {
				delete(h.chans, key)
			}
		}
	}
	return replay, ch, cancel, ok
}

// LastID 返回会话当前最大事件 ID（无事件时为 0）。
func (h *Hub) LastID(key string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.chans[key]; ok {
		return st.buf.LastID()
	}
	return 0
}

// Drop 在会话删除时清理该会话的缓冲与订阅。
func (h *Hub) Drop(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.chans[key]; ok {
		for id, ch := range st.subs {
			delete(st.subs, id)
			close(ch)
		}
		delete(h.chans, key)
	}
}

// ActiveSubscribers 返回当前订阅者数量（调试/测试用）。
func (h *Hub) ActiveSubscribers(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.chans[key]; ok {
		return len(st.subs)
	}
	return 0
}

// channel 惰性创建会话状态。调用方必须持 h.mu。
func (h *Hub) channel(key string) *hubChannel {
	st, ok := h.chans[key]
	if !ok {
		st = &hubChannel{
			buf:  NewBuffer(h.size),
			subs: make(map[int64]chan Event),
		}
		h.chans[key] = st
	}
	return st
}
