package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/executor"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/sse"
)

// sseRetryMillis 建议客户端重连间隔（毫秒）。
const sseRetryMillis = 3000

type sendMessageRequest struct {
	Content         string `json:"content"`
	ClientMessageID string `json:"client_message_id"`
	Stream          *bool  `json:"stream"`
	Model           string `json:"model"`
	// ModelConfigID 指定本次使用的模型配置；为空则沿用会话上次的选择。
	ModelConfigID string `json:"model_config_id"`
}

type sendMessageResponse struct {
	UserMessage      *session.Message `json:"user_message"`
	AssistantMessage *session.Message `json:"assistant_message"`
}

// handleSendMessage 是 SSE 主流程。
//
// 关键设计：**执行生命周期与 HTTP 连接解耦**。
//
// 旧实现把 runCtx 派生自 r.Context()，客户端断开会立即中止生成。
// 引入 checkpoint / HITL 后这是致命缺陷（等审批时连接必然断开，
// 用户刷新页面任务就死了）。现在改为：
//
//  1. 落库消息 → 交给 executor 启动任务（生命周期独立）；
//  2. 本 handler 只做「观察者」：订阅任务事件并写 SSE；
//  3. 连接断开 → 仅取消订阅，任务继续跑；
//  4. 重连通过 GET /stream 续订阅（重放 + 实时）。
//
// stream=false 时退化为普通 JSON 聚合响应，便于 curl 调试。
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	ctx := r.Context()

	var req sendMessageRequest
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, "content 不能为空")
		return
	}

	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}

	// 同一会话串行：TryLock 失败立即返回 409，不阻塞等待。
	unlock, ok := s.locks.TryLock(sessionID)
	if !ok {
		respondError(w, r, http.StatusConflict, CodeSessionBusy, "该会话正在生成中，请等待完成或先取消")
		return
	}
	// 注意：不再 defer unlock —— 锁的生命周期跟随「任务」而非请求。
	// 解锁在任务结束时由 executor 回调完成（见下方 releaseOn）。

	// 落库：用户消息 + 助手占位。先落库，再回响应头，
	// 保证「前端看到的 ⊆ 服务端持久化的」。
	userMsg := session.NewUserMessage(sessionID, content)
	if err := s.store.AppendMessage(ctx, userMsg); err != nil {
		unlock()
		s.respondStoreError(w, r, err)
		return
	}
	placeholder := session.NewAssistantPlaceholder(sessionID)
	if err := s.store.AppendMessage(ctx, placeholder); err != nil {
		unlock()
		s.respondStoreError(w, r, err)
		return
	}

	// 标题与状态更新。
	if session.IsDefaultTitle(sess.Title) {
		if _, err := s.store.Update(ctx, sessionID, func(sc *session.Session) error {
			sc.Title = session.DeriveTitle(content, time.Now().UTC())
			sc.Status = session.SessionRunning
			return nil
		}); err != nil {
			slog.WarnContext(ctx, "更新会话标题失败", "session_id", sessionID, "err", err)
		}
	} else if _, err := s.store.Update(ctx, sessionID, func(sc *session.Session) error {
		sc.Status = session.SessionRunning
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "更新会话状态失败", "session_id", sessionID, "err", err)
	}

	// 决定本次使用哪个模型：
	//   1. 请求显式指定 model_config_id → 用它（并记住到会话上）
	//   2. 否则用会话上次的选择
	//   3. 都没有 → 留空，执行层回退到全局默认
	modelConfigID := strings.TrimSpace(req.ModelConfigID)
	if modelConfigID == "" {
		modelConfigID = sess.ModelConfigID
	}
	// 请求显式切换模型时，持久化到会话，下次打开自动沿用。
	if modelConfigID != "" && modelConfigID != sess.ModelConfigID {
		if _, err := s.store.Update(ctx, sessionID, func(sc *session.Session) error {
			sc.ModelConfigID = modelConfigID
			return nil
		}); err != nil {
			slog.WarnContext(ctx, "记录会话模型选择失败", "session_id", sessionID, "err", err)
		}
	}

	model := req.Model
	if model == "" {
		model = sess.Model
	}
	history, err := s.store.Messages(ctx, sessionID)
	if err != nil {
		unlock()
		s.respondStoreError(w, r, err)
		return
	}
	// 修复可能因上一次中断（落库超时 / 进程崩溃）留下的不一致历史：
	// 滞留 streaming 状态的消息修正为 complete/interrupted。
	// 纯内存操作，不产生任何额外的数据库写入。
	// 注意：不处理悬空 tool_calls——那交给 agent/llm.go、
	// agent/anthropic.go 在构建请求时按 declared/answered 校验剔除，
	// 否则会误伤 ask_user 挂起期间天然存在的"声明未应答"状态
	// （见 internal/session/repair.go 顶部注释的详细说明）。
	history = session.RepairHistory(history)

	// stream=false：聚合为非流式 JSON（同步执行，不进 executor）。
	if req.Stream != nil && !*req.Stream {
		defer unlock()
		s.sendMessageAggregated(w, r, sessionID, placeholder, history, model, modelConfigID)
		return
	}

	// 启动执行任务（生命周期独立于本请求）。
	task, startErr := s.exec.Start(ctx, executor.Request{
		SessionID:     sessionID,
		MessageID:     placeholder.ID,
		History:       history,
		ModelConfigID: modelConfigID,
		Model:         model,
		ApprovalMode:  sess.ApprovalMode,
		RunTimeout:    s.cfg.RunTimeout,
	})
	if startErr != nil {
		unlock()
		if errors.Is(startErr, executor.ErrBusy) {
			respondError(w, r, http.StatusConflict, CodeSessionBusy, "该会话正在生成中")
			return
		}
		slog.ErrorContext(ctx, "启动执行任务失败", "err", startErr, "session_id", sessionID)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "无法启动生成")
		return
	}
	// 任务结束（无论成败）才释放会话锁与 cancel 表。
	go func() {
		<-task.Done()
		s.releaseRun(sessionID)
		unlock()
	}()

	// 从这里开始是 SSE：错误只能以 event: error 表达。
	s.pipeTaskToSSE(w, r, task, sessionID, placeholder, model)
}

// pipeTaskToSSE 订阅任务事件并写入 SSE 响应。
//
// 与旧实现的核心差异：ctx.Done()（客户端断开）只取消订阅，不 cancel 任务。
func (s *Server) pipeTaskToSSE(
	w http.ResponseWriter,
	r *http.Request,
	task *executor.Task,
	sessionID string,
	placeholder *session.Message,
	model string,
) {
	ctx := r.Context()

	sw, err := sse.NewWriter(w)
	if err != nil {
		// 无法建立 SSE：任务仍在跑，但本次请求失败。
		respondError(w, r, http.StatusInternalServerError, CodeSSEUnsupported, err.Error())
		return
	}
	defer sw.Close()

	// 首个事件前声明重连间隔 + 一条注释做即时探活。
	if err := sw.WriteEvent(sse.Event{Name: "", Retry: sseRetryMillis, Data: nil}); err != nil {
		return
	}
	if err := sw.WriteComment("connected"); err != nil {
		return
	}

	// message.start：告知前端 message_id，后续 delta 才能归位。
	//
	// 此时执行体还停在 Task.gate 上（等首次订阅放行），
	// 因此 start 一定是本会话的第一个业务事件，顺序确定。
	startData := sse.MessageStartData{
		SessionID: sessionID,
		MessageID: placeholder.ID,
		Model:     model,
		CreatedAt: placeholder.CreatedAt,
	}
	startEv := task.Publish(sse.EventMessageStart, func(seq uint64) any {
		startData.Seq = seq
		return startData
	})
	if err := sw.WriteEvent(startEv); err != nil {
		return
	}

	// 标记 streaming。
	if err := s.store.UpdateMessage(ctx, sessionID, placeholder.ID, func(m *session.Message) error {
		m.Status = session.StatusStreaming
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "标记 streaming 失败", "err", err, "message_id", placeholder.ID)
	}

	// 订阅任务事件。首次订阅会放行执行体（Task.gate），
	// 因此「start 发布 → 建立订阅 → 执行体开跑」顺序确定，不会乱序。
	replay, live, cancelSub, ok := task.Subscribe(startEv.ID)
	defer cancelSub()
	if !ok {
		_ = sw.WriteComment("resync-required")
	}
	for _, ev := range replay {
		if err := sw.WriteEvent(ev); err != nil {
			return
		}
		if isTerminalEvent(ev.Name) {
			return
		}
	}

	ticker := time.NewTicker(s.cfg.SSEPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// ★ 客户端断开：只退出订阅，任务继续在后台跑。
			// 用户重连后可通过 GET /stream?last_event_id=N 补齐。
			slog.InfoContext(ctx, "客户端断开，任务继续执行",
				"session_id", sessionID, "message_id", placeholder.ID, "last_seq", task.LastSeq())
			return

		case <-ticker.C:
			if err := sw.Ping(sse.PingData{TS: time.Now().UTC()}); err != nil {
				return
			}

		case <-task.Done():
			// 任务已结束：把缓冲区里剩余事件写完再收尾。
			for {
				select {
				case ev := <-live:
					if err := sw.WriteEvent(ev); err != nil {
						return
					}
				default:
					return
				}
			}

		case ev, ok := <-live:
			if !ok {
				return
			}
			if err := sw.WriteEvent(ev); err != nil {
				return
			}
			if isTerminalEvent(ev.Name) {
				return
			}
		}
	}
}

// isTerminalEvent 判断是否为终止类事件。
func isTerminalEvent(name string) bool {
	return name == sse.EventMessageDone ||
		name == sse.EventMessageError ||
		name == sse.EventMessageInterrupted
}

// sendMessageAggregated 把流聚合成单个 JSON 响应（stream=false）。
func (s *Server) sendMessageAggregated(
	w http.ResponseWriter,
	r *http.Request,
	sessionID string,
	placeholder *session.Message,
	history []session.Message,
	model string,
	modelConfigID string,
) {
	ctx := r.Context()

	// 非流式路径也必须遵守「会话所选模型」，否则会出现
	// 「界面上选了模型，但非流式请求实际用了默认模型」的静默降级。
	runner, resolveErr := s.exec.ResolveRunner(modelConfigID)
	if resolveErr != nil {
		s.finishMessage(sessionID, placeholder.ID, "", session.StatusError,
			&session.MessageError{Code: string(CodeInvalidArgument), Message: resolveErr.Error()}, nil)
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, resolveErr.Error())
		return
	}
	if runner == nil {
		respondError(w, r, http.StatusBadGateway, CodeProviderUnavail, "执行器未配置")
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.RunTimeout)
	defer cancel()

	var acc strings.Builder
	var usage *session.Usage
	var runErr error

	for ev := range runner.Run(runCtx, agent.Request{History: history}) {
		switch ev.Type {
		case agent.TypeDelta:
			acc.WriteString(ev.Delta)
		case agent.TypeDone:
			if ev.Content != "" {
				acc.Reset()
				acc.WriteString(ev.Content)
			}
			if ev.Usage != nil {
				usage = &session.Usage{
					PromptTokens:     ev.Usage.PromptTokens,
					CompletionTokens: ev.Usage.CompletionTokens,
					TotalTokens:      ev.Usage.TotalTokens,
				}
			}
		case agent.TypeError:
			runErr = ev.Err
		}
	}

	if runErr != nil {
		s.finishMessage(sessionID, placeholder.ID, acc.String(), session.StatusError,
			&session.MessageError{Code: string(CodeProviderUnavail), Message: runErr.Error()}, nil)
		respondError(w, r, http.StatusBadGateway, CodeProviderUnavail, runErr.Error())
		return
	}

	s.finishMessage(sessionID, placeholder.ID, acc.String(), session.StatusComplete, nil, usage)
	msgs, _ := s.store.Messages(ctx, sessionID)
	// 修一下可能滞留 streaming 的历史消息状态，返回给调用方的内容才自洽
	// （不处理悬空 tool_calls，原因见 session.RepairHistory 的函数注释）。
	msgs = session.RepairHistory(msgs)
	var userMsg, assistantMsg *session.Message
	for i := range msgs {
		if msgs[i].ID == placeholder.ID {
			m := msgs[i]
			assistantMsg = &m
		} else if msgs[i].Role == session.RoleUser && userMsg == nil {
			m := msgs[i]
			userMsg = &m
		}
	}
	_ = model
	respondJSON(w, r, http.StatusOK, sendMessageResponse{
		UserMessage:      userMsg,
		AssistantMessage: assistantMsg,
	})
}

// finishMessage 落库最终状态（非流式路径使用）。
func (s *Server) finishMessage(sessionID, msgID, content string, status session.MessageStatus, msgErr *session.MessageError, usage *session.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.store.UpdateMessage(ctx, sessionID, msgID, func(m *session.Message) error {
		m.Content = content
		m.Status = status
		m.Error = msgErr
		if usage != nil {
			m.Usage = usage
		}
		return nil
	})
	if err != nil {
		slog.Error("最终状态落库失败", "err", err, "session_id", sessionID, "message_id", msgID)
		return
	}

	sessionStatus := session.SessionIdle
	if status == session.StatusError {
		sessionStatus = session.SessionError
	}
	if _, err := s.store.Update(ctx, sessionID, func(sc *session.Session) error {
		sc.Status = sessionStatus
		return nil
	}); err != nil && !errors.Is(err, session.ErrNotFound) {
		slog.Warn("会话状态落库失败", "err", err, "session_id", sessionID)
	}
}

// handleCancel 取消会话中运行中的生成。
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	if _, err := s.store.Get(r.Context(), sessionID); err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	if s.exec.Stop(sessionID) {
		respondJSON(w, r, http.StatusAccepted, map[string]bool{"canceled": true})
		return
	}

	// 没有运行中的任务，但会话可能残留 running 状态（进程异常退出导致）。
	// 此时用户点"停止"的真实意图是"让我能继续用这个会话"，因此顺手复位，
	// 否则前端会一直显示"生成中"且无法再发消息。
	reset, err := s.store.Update(r.Context(), sessionID, func(sc *session.Session) error {
		if sc.Status == session.SessionRunning {
			sc.Status = session.SessionIdle
		}
		return nil
	})
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	respondJSON(w, r, http.StatusOK, map[string]bool{
		"canceled":     false,
		"status_reset": reset.Status == session.SessionIdle,
	})
}
