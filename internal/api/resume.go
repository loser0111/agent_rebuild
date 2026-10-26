package api

// 会话恢复（resume）：把用户对 ask_user 的回答补成一条 tool 结果，
// 然后重新进入工具循环，让 agent 继续往下跑。
//
// 设计要点：
//
//  1. **不唤醒原任务**。进程重启后内存里的 Task 早没了，
//     RecoverStaleRunning 也把 running 复位过了。resume 一定是一次
//     全新的执行：从 messages 表重建历史 + 把答复作为 tool 结果追加。
//
//  2. **call_id 是幂等键**。客户端必须回传提问时拿到的 call_id，
//     与 checkpoint.Pending.CallID 比对。不一致说明这条提问已经被答过了
//     （或会话已经被新一轮覆盖），直接返回当前状态而不是重复追加，
//     否则历史里会出现两条同名 tool 结果，模型会困惑。
//
//  3. **先补 tool 结果，再复用发送流程**。补完之后历史就是「完整的一轮」，
//     和正常对话没有区别，因此可以直接复用 exec.Start + pipeTaskToSSE，
//     不需要为 resume 单独写一套事件管道。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"log/slog"

	"agent_rebuild/internal/executor"
	"agent_rebuild/internal/session"
)

// resumeRequest 是 POST /api/sessions/{id}/resume 的请求体。
type resumeRequest struct {
	// CallID 提问时返回的 tool_call id，用于幂等校验（必填）。
	CallID string `json:"call_id"`
	// Answer 用户对 ask_user 的回答。
	Answer string `json:"answer"`
	// Answers 批量回答（同轮多个 ask_user 时使用）。
	//
	// 与 CallID/Answer 两种形式都支持：单问题用前者（旧客户端兼容），
	// 多问题用后者一次提交，避免反复建连。
	Answers map[string]string `json:"answers"`
	// Stream 是否以 SSE 返回后续生成（默认 true）。
	Stream *bool `json:"stream"`
}

// handleResume 处理「回答挂起中的提问并继续」。
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	ctx := r.Context()

	var req resumeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	req.CallID = strings.TrimSpace(req.CallID)
	req.Answer = strings.TrimSpace(req.Answer)

	// 归一化成 answers 映射：单问题（旧格式）与批量（新格式）统一处理。
	//
	// 键可以是 "call_id" 或 "call_id::qid"（同一调用内多个问题）。
	answers := make(map[string]string, len(req.Answers)+1)
	for k, v := range req.Answers {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		answers[k] = strings.TrimSpace(v)
	}
	if req.CallID != "" && req.Answer != "" {
		answers[req.CallID] = req.Answer
	}
	if len(answers) == 0 {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"需要提供 call_id + answer，或 answers 映射")
		return
	}

	// 没有 checkpoint 存储 → 这个部署形态不支持挂起，自然也无法 resume。
	if s.checkpoints == nil {
		respondError(w, r, http.StatusNotImplemented, CodeInternal,
			"当前部署未启用会话恢复能力")
		return
	}

	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}

	// 读 checkpoint：它是「谁在等什么」的唯一真相源。
	cp, err := s.checkpoints.LoadCheckpoint(ctx, sessionID)
	if err != nil {
		if errors.Is(err, session.ErrCheckpointNotFound) {
			respondError(w, r, http.StatusConflict, CodeConflict,
				"该会话当前没有等待回答的问题（可能已回答或已重新开始）")
			return
		}
		slog.ErrorContext(ctx, "读取 checkpoint 失败", "err", err, "session_id", sessionID)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "读取会话状态失败")
		return
	}

	// 只在挂起状态才接受 resume。
	// running 说明任务还在跑，done/failed 说明这一轮已结束。
	if !cp.IsSuspended() {
		respondError(w, r, http.StatusConflict, CodeConflict,
			"该会话没有等待回答的问题（当前状态："+string(cp.State)+"）")
		return
	}

	// 幂等/一致性校验：提交的答案必须都能对上当前挂起项。
	//
	// 支持两种键：
	//   - "call_id"           单问题调的简写
	//   - "call_id::qid"      同一调用内多问题的精确写法
	//
	// 只要有一个对不上就整体拒绝并说明原因，而不是部分接受——
	// 部分接受会让 checkpoint 处于「一半已回答」的中间态，
	// 而模型拿到残缺的 tool 结果会做出奇怪的推断。
	var unknown []string
	for key := range answers {
		if _, ok := findByAnswerKey(cp.Pendings, key); !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		respondError(w, r, http.StatusConflict, CodeConflict,
			"以下问题已被回答或已被新问题取代："+strings.Join(unknown, ", ")+
				"；请刷新会话查看最新状态")
		return
	}
	// 数量校验：每个 Pending 都必须有答案。
	missing := make([]string, 0)
	for _, p := range cp.Pendings {
		if _, ok := answerFor(answers, p); !ok {
			missing = append(missing, answerKeyOf(p))
		}
	}
	if len(missing) > 0 {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"还有 "+itoa(len(missing))+" 个问题未回答，请一并提交："+
				strings.Join(missing, ", "))
		return
	}

	// 串行保护：同会话已有任务在跑时不允许 resume。
	unlock, ok := s.locks.TryLock(sessionID)
	if !ok {
		respondError(w, r, http.StatusConflict, CodeSessionBusy, "该会话正在生成中，请稍候")
		return
	}

	// ---- 关键一步：把答复写成 tool 结果 ----
	//
	// 挂起时落库的每条 assistant(tool_calls) 都必须有且只有一条配对结果，
	// 历史才是合法的一轮。这里按 call_id 分组：
	//
	//   - 一个 ask_user 调用里含多个问题时，它们的 CallID 相同、QID 不同，
	//     必须**合并成一条** tool 结果（模型期望的是「一个调用一个结果」）。
	//     把多个问题拆成多条 tool 消息会让 OpenAI 直接 400。
	//   - 多个 call_id 时各自一条。
	//
	// 按 Pendings 的原顺序处理：OpenAI 要求 tool 消息的排列对应
	// tool_calls 的顺序，打乱会让部分模型产生混淆。
	byCall := make(map[string][]session.Pending, len(cp.Pendings))
	callOrder := make([]string, 0, len(cp.Pendings))
	for _, p := range cp.Pendings {
		if _, ok := byCall[p.CallID]; !ok {
			callOrder = append(callOrder, p.CallID)
		}
		byCall[p.CallID] = append(byCall[p.CallID], p)
	}

	for _, callID := range callOrder {
		group := byCall[callID]
		content := composeAnswer(group, answers)
		toolMsg := &session.Message{
			ID:         session.NewMessageID(),
			SessionID:  sessionID,
			Role:       session.RoleTool,
			Content:    content,
			Status:     session.StatusComplete,
			ToolCallID: callID,
		}
		if err := s.store.AppendMessage(ctx, toolMsg); err != nil {
			unlock()
			s.respondStoreError(w, r, err)
			return
		}
	}

	// 清掉 checkpoint：这个提问已经处理完了。
	// 即使后面的生成失败也不该「复活」这条提问——用户已经答过，
	// 再让他答一次是明显的 bug。
	if err := s.checkpoints.DeleteCheckpoint(ctx, sessionID); err != nil {
		// 删不掉不阻塞主流程：它最多导致一次重复提示，
		// 而 call_id 校验能拦住重复提交。
		slog.WarnContext(ctx, "清理 checkpoint 失败", "err", err, "session_id", sessionID)
	}

	// 新建 assistant 占位消息，承载本轮的后续生成。
	placeholder := session.NewAssistantPlaceholder(sessionID)
	if err := s.store.AppendMessage(ctx, placeholder); err != nil {
		unlock()
		s.respondStoreError(w, r, err)
		return
	}

	// 会话状态回到 running（之前是 suspended）。
	if _, err := s.store.Update(ctx, sessionID, func(sc *session.Session) error {
		sc.Status = session.SessionRunning
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "更新会话状态失败", "err", err, "session_id", sessionID)
	}

	// 重建历史：此时包含刚补的 tool 结果。
	history, err := s.store.Messages(ctx, sessionID)
	if err != nil {
		unlock()
		s.respondStoreError(w, r, err)
		return
	}
	// resume 这一步已经把答案补成了 tool 结果，这里只需要再纠正一下
	// 可能滞留 streaming 的历史消息状态（不处理悬空 tool_calls，
	// 原因见 session.RepairHistory 的函数注释——resume 本身走的
	// 正是 ask_user 挂起这个合法场景，不能被误判成"中断"）。
	history = session.RepairHistory(history)

	// 模型沿用会话上次的选择。
	modelConfigID := sess.ModelConfigID
	model := sess.Model

	// stream=false：聚合返回，便于 curl 调试。
	if req.Stream != nil && !*req.Stream {
		defer unlock()
		s.sendMessageAggregated(w, r, sessionID, placeholder, history, model, modelConfigID)
		return
	}

	task, startErr := s.exec.Start(ctx, executor.Request{
		SessionID:     sessionID,
		MessageID:     placeholder.ID,
		History:       history,
		ModelConfigID: modelConfigID,
		Model:         model,
		RunTimeout:    s.cfg.RunTimeout,
	})
	if startErr != nil {
		unlock()
		if errors.Is(startErr, executor.ErrBusy) {
			respondError(w, r, http.StatusConflict, CodeSessionBusy, "该会话正在生成中")
			return
		}
		slog.ErrorContext(ctx, "恢复执行失败", "err", startErr, "session_id", sessionID)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "无法继续生成")
		return
	}
	go func() {
		<-task.Done()
		s.releaseRun(sessionID)
		unlock()
	}()

	s.pipeTaskToSSE(w, r, task, sessionID, placeholder, model)
}

// handleGetCheckpoint 查询会话当前的挂起状态（前端刷新后据此恢复问答卡片）。
//
// 为什么需要：awaiting.input 事件只存在于事件流里，页面刷新后就丢了。
// 前端必须在打开会话时能问一次「有没有在等我的回答」。
func (s *Server) handleGetCheckpoint(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	ctx := r.Context()

	if s.checkpoints == nil {
		respondJSON(w, r, http.StatusOK, map[string]any{"suspended": false})
		return
	}

	cp, err := s.checkpoints.LoadCheckpoint(ctx, sessionID)
	if err != nil {
		if errors.Is(err, session.ErrCheckpointNotFound) {
			respondJSON(w, r, http.StatusOK, map[string]any{"suspended": false})
			return
		}
		slog.ErrorContext(ctx, "读取 checkpoint 失败", "err", err, "session_id", sessionID)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "读取会话状态失败")
		return
	}

	if !cp.IsSuspended() {
		respondJSON(w, r, http.StatusOK, map[string]any{
			"suspended": false,
			"state":     string(cp.State),
		})
		return
	}

	// 同时返回 questions 列表与首个问题的扁平字段：
	//   - 新前端用 questions 一次渲染全部卡片；
	//   - 旧前端（只认 call_id/question）仍能工作。
	questions := make([]map[string]any, 0, len(cp.Pendings))
	for _, p := range cp.Pendings {
		questions = append(questions, map[string]any{
			"call_id":  p.CallID,
			"qid":      p.QID,
			"key":      answerKeyOf(p),
			"question": p.Question,
			"options":  p.Options,
			"multi":    p.Multi,
		})
	}
	first := cp.Pendings[0]
	respondJSON(w, r, http.StatusOK, map[string]any{
		"suspended":  true,
		"state":      string(cp.State),
		"questions":  questions,
		"call_id":    first.CallID,
		"question":   first.Question,
		"options":    first.Options,
		"step_index": cp.StepIndex,
		"updated_at": cp.UpdatedAt,
	})
}

// itoa 把 Atoi 与 Itoa 的重复转换收敛成一行。
func itoa(n int) string {
	return strconv.Itoa(n)
}

/* ---- 答案键：call_id 与 call_id::qid 双写法 ---- */

// answerKeyOf 返回某个问题在提交时的规范键。
//
// 单问题调用（没有 QID）用 call_id 即可，多问题必须带 qid，
// 否则同一调用的多个答案会挤在同一个键上互相覆盖。
func answerKeyOf(p session.Pending) string {
	if p.QID == "" {
		return p.CallID
	}
	return p.CallID + "::" + p.QID
}

// answerFor 取出某个问题的答案。
//
// 先按精确键（call_id::qid）查，再退回裸 call_id：
// 这样前端在「只有一个问题」时可以只传 call_id，不必关心 qid。
func answerFor(answers map[string]string, p session.Pending) (string, bool) {
	if p.QID != "" {
		if v, ok := answers[answerKeyOf(p)]; ok {
			return v, true
		}
	}
	// 裸 call_id 只在「该调用只有一个问题」时才应使用，
	// 但这里无法判断调用里有几个问题，交给出题方保证；
	// 若同一调用有多个问题，裸键会命中第一条，属于调用方误用。
	v, ok := answers[p.CallID]
	return v, ok
}

// findByAnswerKey 按提交键反查挂起项（用于校验键是否合法）。
func findByAnswerKey(pendings []session.Pending, key string) (session.Pending, bool) {
	for _, p := range pendings {
		if answerKeyOf(p) == key || p.CallID == key {
			return p, true
		}
	}
	return session.Pending{}, false
}

// composeAnswer 把一个 ask_user 调用内所有问题的答案拼成一条 tool 结果。
//
// 为什么必须合并：模型发起的是**一次**工具调用（一个 call_id），
// 期望拿回**一条**结果。若按问题拆成多条 tool 消息，
// OpenAI 会直接 400（tool 消息数量与 tool_calls 不匹配）。
//
// 输出格式刻意做成「问题 → 回答」对：
//   - 单个问题时只给答案本身（最常见场景，保持简洁）；
//   - 多个问题时带编号与问题原文，模型才能对上哪个答案属于哪一问。
func composeAnswer(group []session.Pending, answers map[string]string) string {
	if len(group) == 1 {
		if v, ok := answerFor(answers, group[0]); ok {
			return v
		}
		return ""
	}
	var sb strings.Builder
	for i, p := range group {
		if i > 0 {
			sb.WriteString("\n")
		}
		v, _ := answerFor(answers, p)
		if p.QID != "" {
			sb.WriteString(p.QID)
			sb.WriteString(". ")
		}
		sb.WriteString(p.Question)
		sb.WriteString("\n→ ")
		sb.WriteString(v)
	}
	return sb.String()
}
