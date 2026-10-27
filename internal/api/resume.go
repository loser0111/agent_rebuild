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
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"log/slog"

	"agent_rebuild/internal/agent/loop"
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
	// Decisions 人工审批的决定：call_id -> "approve" | "deny"。
	//
	// 与 Answers 分开是刻意的：两者语义完全不同——Answers 的值直接就是
	// tool 结果（ask_user），而 Decisions 的值只是"批不批"，批准后还要
	// 真正执行工具才能产出 tool 结果（见 loop.ExecuteApproved）。混在一个
	// 字段里会让"回答文字"和"审批决定"的处理逻辑互相污染。
	Decisions map[string]string `json:"decisions"`
	// Reason 拒绝审批时的理由（可选），会作为"操作未执行"的说明回喂给模型。
	Reason string `json:"reason"`
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

	// 归一化 decisions 映射：键是 call_id，值是 "approve" / "deny"（大小写容错）。
	//
	// 与 answers 分开校验/处理：两者对应 Pending.Kind 的两个分支，详见下方。
	decisions := make(map[string]string, len(req.Decisions))
	for k, v := range req.Decisions {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		decisions[k] = strings.ToLower(strings.TrimSpace(v))
	}

	if len(answers) == 0 && len(decisions) == 0 {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"需要提供 call_id + answer、answers 映射，或 decisions 映射")
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

	// 幂等/一致性校验：提交的答案/决定必须都能对上当前挂起项。
	//
	// answers 的键支持两种形式：
	//   - "call_id"           单问题调的简写
	//   - "call_id::qid"      同一调用内多问题的精确写法
	// decisions 的键是 call_id，值必须是 "approve" 或 "deny"。
	//
	// 只要有一个对不上就整体拒绝并说明原因，而不是部分接受——
	// 部分接受会让 checkpoint 处于「一半已处理」的中间态，
	// 而模型拿到残缺的 tool 结果会做出奇怪的推断。
	approvalPendings := make(map[string]session.Pending, len(cp.Pendings))
	for _, p := range cp.Pendings {
		if p.Kind == session.PendingApproval {
			approvalPendings[p.CallID] = p
		}
	}

	var unknown []string
	for key := range answers {
		if _, ok := findByAnswerKey(cp.Pendings, key); !ok {
			unknown = append(unknown, key)
		}
	}
	for callID := range decisions {
		if _, ok := approvalPendings[callID]; !ok {
			unknown = append(unknown, callID)
		}
	}
	if len(unknown) > 0 {
		respondError(w, r, http.StatusConflict, CodeConflict,
			"以下问题/操作已被处理或已被新一轮取代："+strings.Join(unknown, ", ")+
				"；请刷新会话查看最新状态")
		return
	}

	// 数量与取值校验：每个 Pending 都必须被覆盖，审批决定只能是 approve/deny。
	missing := make([]string, 0)
	var invalidDecisions []string
	for _, p := range cp.Pendings {
		if p.Kind == session.PendingApproval {
			d, ok := decisions[p.CallID]
			if !ok {
				missing = append(missing, p.CallID)
				continue
			}
			if d != "approve" && d != "deny" {
				invalidDecisions = append(invalidDecisions, p.CallID+"="+d)
			}
			continue
		}
		if _, ok := answerFor(answers, p); !ok {
			missing = append(missing, answerKeyOf(p))
		}
	}
	if len(invalidDecisions) > 0 {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"decisions 的值只能是 approve 或 deny，以下不合法："+strings.Join(invalidDecisions, ", "))
		return
	}
	if len(missing) > 0 {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"还有 "+itoa(len(missing))+" 项未处理，请一并提交："+
				strings.Join(missing, ", "))
		return
	}

	// 串行保护：同会话已有任务在跑时不允许 resume。
	unlock, ok := s.locks.TryLock(sessionID)
	if !ok {
		respondError(w, r, http.StatusConflict, CodeSessionBusy, "该会话正在生成中，请稍候")
		return
	}

	// ---- 关键一步：把答复/审批决定写成 tool 结果 ----
	//
	// 挂起时落库的每条 assistant(tool_calls) 都必须有且只有一条配对结果，
	// 历史才是合法的一轮。这里按 call_id 分组：
	//
	//   - 一个 ask_user 调用里含多个问题时，它们的 CallID 相同、QID 不同，
	//     必须**合并成一条** tool 结果（模型期望的是「一个调用一个结果」）。
	//     把多个问题拆成多条 tool 消息会让 OpenAI 直接 400。
	//   - PendingApproval 每个 call_id 只会有一条 Pending（没有 QID 细分），
	//     但处理方式和 PendingInput 完全不同：decision=approve 时要先真正
	//     执行工具（见 loop.ExecuteApproved），不能把"已批准"当成结果；
	//     decision=deny 时产出"操作未执行"，工具从未被调用。
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
		kind := group[0].Kind // 同一 call_id 的 Pendings Kind 必须一致（由 suspendAll 保证）。

		var content string
		status := session.StatusComplete

		switch kind {
		case session.PendingApproval:
			p := group[0]
			switch decisions[callID] {
			case "deny":
				reason := strings.TrimSpace(req.Reason)
				if reason == "" {
					reason = "用户拒绝了该操作"
				}
				content = "操作未执行：" + reason
				status = session.StatusError

			case "approve":
				// 这里才是工具第一次真正被执行：挂起期间它只是"待批准"，
				// 从未调用 registry.Invoke。批准之后必须真正执行并把
				// 真实结果喂给模型，不能用"已批准"这几个字充数，
				// 否则模型会以为文件已经写好，但实际上什么都没发生。
				result := loop.ExecuteApproved(ctx, s.tools, s.cfg.ToolTimeout, callID, p.Name, p.Args)
				content = result.Output
				if result.IsError {
					status = session.StatusError
				}
			}

		default: // session.PendingInput
			content = composeAnswer(group, answers)
		}

		toolMsg := &session.Message{
			ID:         session.NewMessageID(),
			SessionID:  sessionID,
			Role:       session.RoleTool,
			Content:    content,
			Status:     status,
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

	// 顺手清理挂起期间留下的"僵尸" streaming 占位消息。
	//
	// 成因：上一次任务挂起时，当前这条 assistant 占位消息被落成
	// status=streaming（见 executor.go 的 TypeAwaitingInput 分支），之后
	// 那个任务就结束了——resume 会新建一条全新的占位消息承载后续生成，
	// 不会再碰旧的这条。RepairHistory 只在**读**历史时在内存里把它纠正成
	// complete/interrupted，从不回写数据库，于是库里会一直堆积这类用不上
	// 但永远留着的消息。这里趁着已经有一份最新 history，顺手按
	// RepairHistory 同样的判定标准回写一次，避免越积越多——
	// 判定逻辑必须跟 fixStreamingStatus 完全一致，否则两处会在边界情况上
	// 打架，所以特意加了交叉引用注释。失败只记日志：这是清理性质的操作，
	// 不能因为它失败就卡住本次 resume 的主流程。
	reconcileStreamingMessages(ctx, s.store, sessionID, history)

	// resume 这一步已经把答案补成了 tool 结果，这里只需要再纠正一下
	// 可能滞留 streaming 的历史消息状态（不处理悬空 tool_calls，
	// 原因见 session.RepairHistory 的函数注释——resume 本身走的
	// 正是 ask_user 挂起这个合法场景，不能被误判成"中断"）。
	history = session.RepairHistory(history)

	// 兜底检测：警告"孤儿 tool_calls"（有调用请求但历史里找不到配对结果）。
	//
	// 正常情况下不应该出现——loop.go 的 Run() 已经改成"同轮里不需要
	// 挂起的调用先执行完、只把真正需要挂起的子集交给 suspendAll"，
	// cp.Pendings 之外的调用理应都已经有配对的 tool 结果。这里只是
	// 一道低成本的事后检测（纯日志，不做任何补偿执行）：如果未来又
	// 出现类似"某个调用既不在 Pendings 里、也没有结果"的回归，能在
	// 日志里第一时间发现，而不是等用户反馈"工具卡住了"才去翻代码。
	warnOrphanedToolCalls(ctx, sessionID, history)

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
		ApprovalMode:  sess.ApprovalMode,
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
	//   - 新前端用 questions 一次渲染全部卡片（按 kind 区分问答卡片/审批卡片）；
	//   - 旧前端（只认 call_id/question）仍能工作——旧字段对 PendingApproval
	//     项没有意义（Question 为空），但不影响旧前端已有的问答场景。
	questions := make([]map[string]any, 0, len(cp.Pendings))
	for _, p := range cp.Pendings {
		questions = append(questions, map[string]any{
			"kind":     string(p.Kind),
			"call_id":  p.CallID,
			"qid":      p.QID,
			"key":      answerKeyOf(p),
			"question": p.Question,
			"options":  p.Options,
			"multi":    p.Multi,
			// 以下三个仅 kind=approval 时有值。
			"name":   p.Name,
			"args":   p.Args,
			"risk":   p.Risk,
			"reason": p.Reason,
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

// warnOrphanedToolCalls 扫描历史里所有 assistant 的 tool_calls，
// 找出没有配对 tool 结果的 call_id 并记一条警告日志。
//
// 纯诊断用途：不做任何补偿执行，也不阻塞 resume 流程——发现孤儿调用时
// 唯一能做的安全操作就是让模型知道"这条调用没有结果"，而这正是
// session.RepairHistory 已经在做的事（见该函数注释）。这里只是在
// RepairHistory 跑完之后再确认一遍"真的一个孤儿都没剩"，方便在日志里
// 尽早发现类似 loop.go 挂起顺序这类回归，而不是等用户反馈才去排查。
func warnOrphanedToolCalls(ctx context.Context, sessionID string, history []session.Message) {
	resultOf := make(map[string]bool)
	for _, m := range history {
		if m.Role == session.RoleTool && m.ToolCallID != "" {
			resultOf[m.ToolCallID] = true
		}
	}
	for _, m := range history {
		if m.Role != session.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !resultOf[tc.ID] {
				slog.WarnContext(ctx, "发现孤儿 tool_call（无配对结果，resume 前未能补齐）",
					"session_id", sessionID, "call_id", tc.ID, "name", tc.Name)
			}
		}
	}
}

// reconcileStreamingMessages 把历史里滞留在 streaming 的 assistant 消息
// 回写成 complete/interrupted，落到数据库里（而不是只在内存里纠正）。
//
// 这些消息是上一次任务挂起时留下的占位：那次任务已经结束，不会再有
// 任何代码更新它们；resume 会新建一条全新的占位消息承载后续生成，
// 不会复用它们（见 session.RepairHistory 顶部注释）。RepairHistory 只在
// **读**历史时做内存修正，不回写——于是库里会一直堆积这类用不上但
// 永远留着的消息（纯数据卫生问题，不影响功能，RepairHistory 已经保证
// 读到的历史是对的）。既然 resume 这一步已经要查一次最新 history，
// 顺手把它们落库改正，避免越积越多。
//
// 判定标准必须跟 fixStreamingStatus（session/repair.go）完全一致：
// 内容或 tool_calls 非空 → complete，否则 → interrupted。
//
// 失败只记日志：这是清理性质的操作，不能因为它失败就卡住本次 resume
// 的主流程（主流程已经完成了真正要紧的事——补答案/执行已批准的操作）。
func reconcileStreamingMessages(ctx context.Context, store session.Store, sessionID string, history []session.Message) {
	for _, m := range history {
		if m.Role != session.RoleAssistant || m.Status != session.StatusStreaming {
			continue
		}
		msgID := m.ID
		err := store.UpdateMessage(ctx, sessionID, msgID, func(mm *session.Message) error {
			// double-check：UpdateMessage 在锁内重新读取一次最新状态，
			// 避免并发场景下把已经被别处纠正过的消息又处理一遍。
			if mm.Status != session.StatusStreaming {
				return nil
			}
			if strings.TrimSpace(mm.Content) != "" || len(mm.ToolCalls) > 0 {
				mm.Status = session.StatusComplete
			} else {
				mm.Status = session.StatusInterrupted
			}
			return nil
		})
		if err != nil {
			slog.WarnContext(ctx, "回写挂起占位消息状态失败（不影响本次 resume，下次读取仍会被 RepairHistory 内存纠正）",
				"err", err, "session_id", sessionID, "message_id", msgID)
		}
	}
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
