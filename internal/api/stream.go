package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"agent_rebuild/internal/sse"
)

// handleStream 是断线补齐入口：GET /api/sessions/{id}/stream?last_event_id=N
//
// 行为（对应 docs/02-api-and-sse.md §3.4）：
//  1. 立即回 200 + text/event-stream；
//  2. 重放缓冲中 seq > last_event_id 的事件；
//  3. 若差距过大（缓冲已裁剪）→ 先发 resync + 全量消息快照，再转实时；
//  4. 重放后若会话仍在生成，转为实时推送订阅。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	ctx := r.Context()

	// 先确认会话存在，避免对不存在的会话建立长连接。
	if _, err := s.store.Get(ctx, sessionID); err != nil {
		s.respondStoreError(w, r, err)
		return
	}

	var lastID uint64
	if raw := r.URL.Query().Get("last_event_id"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, "last_event_id 必须是非负整数")
			return
		}
		lastID = n
	}

	sw, err := sse.NewWriter(w)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, CodeSSEUnsupported, err.Error())
		return
	}
	defer sw.Close()

	if err := sw.WriteEvent(sse.Event{Retry: sseRetryMillis}); err != nil {
		return
	}

	replay, live, cancelSub, ok := s.hub.Subscribe(sessionID, lastID)
	defer cancelSub()

	if !ok {
		// 缓冲已裁剪：发全量快照让前端彻底重建状态。
		msgs, msgErr := s.store.Messages(ctx, sessionID)
		if msgErr != nil {
			s.respondStoreError(w, r, msgErr)
			return
		}
		if err := sw.WriteEvent(sse.Event{
			ID:   s.hub.LastID(sessionID),
			Name: sse.EventResync,
			Data: sse.ResyncData{
				SessionID:   sessionID,
				LastEventID: s.hub.LastID(sessionID),
				Messages:    msgs,
			},
		}); err != nil {
			return
		}
	} else {
		for _, ev := range replay {
			// message.start 是「本次生成握手」事件：重连时不该重放，
			// 前端此时已知道 message_id（历史详情里能查到）。
			if ev.Name == sse.EventMessageStart {
				continue
			}
			if err := sw.WriteEvent(ev); err != nil {
				return
			}
		}
	}

	// 无运行中生成时，重放完即可收尾（前端会自行轮询或再次订阅）。
	if !s.locks.IsBusy(sessionID) {
		_ = sw.WriteComment("idle")
		return
	}

	ticker := time.NewTicker(s.cfg.SSEPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "订阅者断开", "session_id", sessionID)
			return

		case <-ticker.C:
			if err := sw.Ping(sse.PingData{TS: time.Now().UTC()}); err != nil {
				return
			}

		case ev, ok := <-live:
			if !ok {
				return
			}
			if err := sw.WriteEvent(ev); err != nil {
				return
			}
			// 终止类事件：推送后结束本次订阅。
			if ev.Name == sse.EventMessageDone ||
				ev.Name == sse.EventMessageError ||
				ev.Name == sse.EventMessageInterrupted {
				return
			}
		}
	}
}
