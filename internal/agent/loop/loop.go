// Package loop 实现 agent 的多轮工具循环（ReAct 模式）。
//
// 职责边界：
//   - Runner 负责「一次模型调用」（可能产出文本或工具调用请求）
//   - Loop 负责「模型 → 工具 → 模型」的迭代、终止条件、工具执行
//
// 之所以分成两层：协议适配不需要知道工具怎么执行，
// 工具也不需要知道用哪个模型，各自可独立替换与测试。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/tool"
)

// 默认限制。
const (
	// DefaultMaxSteps 单次对话最多几轮「模型→工具」迭代。
	// 防止模型陷入死循环（反复调用同一工具）。
	DefaultMaxSteps = 200
	// DefaultToolTimeout 单个工具执行超时。
	DefaultToolTimeout = 120 * time.Second
)

// Config 是 Loop 的配置。
type Config struct {
	// Registry 工具注册表（nil 表示不启用工具）。
	Registry *tool.Registry
	// MaxSteps 最大迭代轮数（<=0 用默认值）。
	MaxSteps int
	// ToolTimeout 单工具超时（<=0 用默认值）。
	ToolTimeout time.Duration
	// SystemPrompt 系统提示。
	SystemPrompt string
	// Approver 审批回调：返回 true 表示允许执行。
	//
	// 为 nil 时按「工具危险等级」自动决定：Safe 直接执行，
	// Moderate/Dangerous 一律拒绝（安全默认）。
	Approver Approver

	// SessionID 当前会话 ID，checkpoint 以它为键落库。
	//
	// 为空时无法挂起：交互型工具（ask_user）会退化为返回一条
	// 「无法提问」的工具结果，让模型自己继续，而不是整个请求失败。
	SessionID string
	// Checkpointer 挂起点持久化（nil 表示不支持跨进程恢复）。
	//
	// 为 nil 时交互型工具同样退化处理。这样可以让单轮 Runner、
	// 无状态测试、以及未接存储的调用方都不用关心挂起机制。
	Checkpointer session.CheckpointStore
}

// Approver 决定某个工具调用是否需要用户确认，以及是否已获批准。
//
// 返回值 approved=false 时，该调用不会执行，而是把「被拒绝」
// 作为工具结果回喂给模型（模型应据此换一种方式）。
type Approver interface {
	// Approve 在工具执行前调用。allowed=false 表示拒绝。
	Approve(ctx context.Context, call agent.ToolCall) (allowed bool, reason string)
}

// AutoApprover 是基于危险等级的默认审批器。
//
// 策略（安全默认）：
//   - RiskSafe：直接执行（只读，无副作用）
//   - 其他：拒绝，并提示用户需显式授权
type AutoApprover struct {
	// AllowModerate 是否允许中风险工具（写文件）。
	// 生产环境建议开启，因为写文件是 agent 的核心能力。
	AllowModerate bool
	// AllowDangerous 是否允许危险工具（执行命令）。
	AllowDangerous bool
}

// Approve 实现 Approver。
func (a AutoApprover) Approve(_ context.Context, call agent.ToolCall) (bool, string) {
	switch call.Risk {
	case tool.RiskSafe:
		return true, ""
	case tool.RiskModerate:
		if a.AllowModerate {
			return true, ""
		}
		return false, "该操作会修改文件，需要用户确认"
	default:
		if a.AllowDangerous {
			return true, ""
		}
		return false, "该操作会执行命令，需要用户确认"
	}
}

// Loop 执行完整的「模型 + 工具」对话。
type Loop struct {
	runner agent.Runner
	cfg    Config

	// runID 标识本次执行，写进 checkpoint 便于排查「哪次执行留下的断点」。
	runID string
	// stepIndex 当前循环步号（0 基），挂起时写入 checkpoint。
	stepIndex int
	// completed 本轮已产出结果的 call_id（幂等重放用）。
	completed []string
	// suspended 非 nil 表示本轮因等待用户回答而挂起。
	//
	// 用它而不是「返回值 + 错误」来传递挂起状态：挂起是**正常**的
	// 业务结果，不是错误；用 error 表达会让上层把所有挂起都当失败处理，
	// 进而把消息标成 error 而不是 interrupted。
	suspended *session.Checkpoint
}

// New 创建 Loop。
func New(runner agent.Runner, cfg Config) *Loop {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultMaxSteps
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = DefaultToolTimeout
	}
	if cfg.Approver == nil {
		// 默认：允许写文件，不允许执行命令。
		cfg.Approver = AutoApprover{AllowModerate: true, AllowDangerous: false}
	}
	return &Loop{runner: runner, cfg: cfg}
}

// Suspended 返回挂起信息（nil 表示本轮正常结束）。
//
// 调用方据此区分三种收尾：正常完成 / 挂起等待 / 出错。
func (l *Loop) Suspended() *session.Checkpoint {
	return l.suspended.Clone()
}

// Result 是一次完整循环的结果。
type Result struct {
	// Content 最终文本回答。
	Content string
	// Usage 累计用量。
	Usage *agent.Usage
	// Steps 实际迭代轮数。
	Steps int
	// ToolResults 所有工具执行记录（便于持久化与 UI 展示）。
	ToolResults []agent.ToolResult
	// ToolCalls assistant 发起的全部工具调用（按序）。
	ToolCalls []agent.ToolCall
	// AssistantMessages 带 tool_calls 的 assistant 消息（必须与 ToolResults 成对落库）。
	//
	// 只落 tool 结果而不落这些 assistant 消息，会让历史出现「孤儿 tool 消息」，
	// 下次请求会被 OpenAI 以 400 拒绝。
	AssistantMessages []session.Message
}

// Run 执行循环并把事件逐个发给 out。
//
// 事件序列可能是：
//
//	delta... → tool.call → tool.result → delta... → done
//
// 调用方（executor）负责把这些事件转成 SSE。
func (l *Loop) Run(ctx context.Context, history []session.Message, out chan<- agent.Event) (*Result, error) {
	res := &Result{}
	specs := l.toolSpecs()

	// 每次 Run 都是一次独立的执行：重置挂起标记与完成列表，
	// 避免复用同一个 Loop 实例时（测试、或将来做重试）状态串台。
	l.suspended = nil
	l.completed = nil
	l.stepIndex = 0
	if l.runID == "" {
		l.runID = newMessageID()
	}

	// 执行开始：写一条 running checkpoint。
	//
	// 目的不是「记录进度」而是**兜底识别**：进程被杀后，库里若留下
	// suspended 的 checkpoint，说明有会话在等用户回答，启动时应提示用户；
	// 留下 running 的则说明是执行中断，可以直接清理。
	l.saveRunningCheckpoint(ctx)

	// 收尾：本轮若不是挂起结束，就清理 running 记录。
	//
	// 为什么必须清理：resume 的判定依据是「checkpoint 存在且为 suspended」。
	// 如果正常回答完还留着一条 running，前端一刷新就会以为「有个问题在等我」，
	// 而实际上什么都没有——这种假提示比不提示更糟。
	defer func() {
		if l.suspended == nil {
			l.clearCheckpoint()
		}
	}()

	// 工作副本：工具调用的请求与结果都要追加进上下文。
	working := make([]session.Message, len(history), len(history)+l.cfg.MaxSteps*2)
	copy(working, history)

	// 记录最后一次模型产出的文本：达到上限时作为兜底回答。
	// 小模型常常在最后一轮才给出结论，直接丢弃会让用户什么都拿不到。
	var lastText string

	for step := 0; step < l.cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Steps = step + 1

		// ---- 一次模型调用 ----
		var (
			text      strings.Builder
			toolCalls []agent.ToolCall
			stepUsage *agent.Usage
			runErr    error
		)

		for ev := range l.runner.Run(ctx, agent.Request{
			History:      working,
			Tools:        specs,
			SystemPrompt: l.cfg.SystemPrompt,
		}) {
			switch ev.Type {
			case agent.TypeDelta:
				if ev.Delta == "" {
					continue
				}
				text.WriteString(ev.Delta)
				if !emit(ctx, out, agent.Event{Type: agent.TypeDelta, Delta: ev.Delta}) {
					return res, ctx.Err()
				}

			case agent.TypeToolCall:
				if ev.ToolCall != nil {
					call := *ev.ToolCall
					// 补充风险信息供审批与 UI 使用。
					call.Risk, call.RequiresApproval = l.riskOf(call.Name)
					// 未知工具直接给出明确错误，避免被审批器误判为"危险操作"。
					if l.cfg.Registry != nil {
						if _, ok := l.cfg.Registry.Get(call.Name); !ok {
							call.RequiresApproval = false
						}
					}
					toolCalls = append(toolCalls, call)
					// 转发给前端：用户应能看到模型请求了什么工具、什么参数。
					if !emit(ctx, out, agent.Event{Type: agent.TypeToolCall, ToolCall: &call}) {
						return res, ctx.Err()
					}
				}

			case agent.TypeDone:
				// 注意：有工具调用的轮次也可能发 done（仅用于上报用量，
				// Content 为空）。因此这里不能无条件覆盖文本。
				if ev.Content != "" {
					text.Reset()
					text.WriteString(ev.Content)
				}
				if ev.Usage != nil {
					stepUsage = ev.Usage
				}

			case agent.TypeError:
				runErr = ev.Err
			}
		}
		if runErr != nil {
			return res, runErr
		}

		// 保留本轮文本，供达到上限时兜底。
		if text.Len() > 0 {
			lastText = text.String()
		}

		// 累计用量（工具调用轮也应有用量，否则统计会漏掉）。
		if stepUsage != nil {
			if res.Usage == nil {
				res.Usage = &agent.Usage{}
			}
			res.Usage.PromptTokens += stepUsage.PromptTokens
			res.Usage.CompletionTokens += stepUsage.CompletionTokens
			res.Usage.TotalTokens += stepUsage.TotalTokens
		}

		// ---- 无工具调用：对话结束 ----
		if len(toolCalls) == 0 {
			res.Content = text.String()
			if res.Content == "" {
				return res, errors.New("模型未返回任何内容")
			}
			if !emit(ctx, out, agent.Event{Type: agent.TypeDone, Content: res.Content, Usage: res.Usage}) {
				return res, ctx.Err()
			}
			return res, nil
		}

		// ---- 有工具调用：记录 assistant 消息，逐个执行 ----
		assistant := session.Message{
			ID:        newMessageID(),
			Role:      session.RoleAssistant,
			Content:   text.String(),
			Status:    session.StatusComplete,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
			ToolCalls: toSessionToolCalls(toolCalls),
		}
		working = append(working, assistant)
		res.ToolCalls = append(res.ToolCalls, toolCalls...)
		// 关键：assistant 的 tool_calls 消息必须落库。
		// 只落 tool 结果而不落发起方，会让历史里出现「孤儿 tool 消息」，
		// 下次请求会被 OpenAI 拒绝（tool 必须是对 tool_calls 的响应）。
		res.AssistantMessages = append(res.AssistantMessages, assistant)

		// ---- 先处理同轮的所有交互型调用 ----
		//
		// 为什么必须先统一扫描再执行：模型经常一口气抛多个问题
		// （「你到哪一步了？」「要不要我改代码？」）。早期实现在
		// executeOne 里遇到第一个交互调用就挂起并 return，同轮后面的
		// 问题既不执行、也不落 checkpoint——用户完全看不到它们，
		// 表现为「问题静默消失」。
		//
		// 现在改成：把同轮所有交互型调用收集成一次挂起，
		// 一条 checkpoint 里存全部 Pendings，前端一次展示多个问题。
		if l.suspendAll(ctx, toolCalls, out, step) {
			return res, nil
		}

		for _, call := range toolCalls {
			// 记录当前步号，挂起时写进 checkpoint。
			l.stepIndex = step

			result := l.executeOne(ctx, call)

			// 兜底：executeOne 里若因为降级失败而返回交互型调用，
			// 上面 suspendAll 已经处理过，正常不会走到这里。
			if l.suspended != nil {
				slog.Info("本轮因等待用户回答而挂起",
					"session_id", l.cfg.SessionID, "call_id", call.ID, "step", step)
				return res, nil
			}

			l.completed = append(l.completed, call.ID)
			res.ToolResults = append(res.ToolResults, result)

			// 发结果事件（UI 可展示「已执行」）。
			if !emit(ctx, out, agent.Event{Type: agent.TypeToolResult, ToolResult: &result}) {
				return res, ctx.Err()
			}

			// 回喂给模型：以 tool 角色的消息追加。
			working = append(working, session.Message{
				ID:         newMessageID(),
				Role:       session.RoleTool,
				Content:    result.Output,
				Status:     statusFor(result),
				ToolCallID: result.CallID,
				CreatedAt:  time.Now().UTC(),
				UpdatedAt:  time.Now().UTC(),
			})
		}
	}

	// 达到最大步数：保留模型已经产出的文本，再追加收尾说明。
	//
	// 早期实现直接丢弃 text（只有最后一句固定提示），用户拿不到任何
	// 实质内容——即使模型在前几轮已经写出了有用分析。这里改为保留。
	done := "（已达到最大工具调用轮数，回答可能不完整）"
	res.Content = done
	if lastText != "" {
		res.Content = lastText + "\n\n" + done
	}
	if !emit(ctx, out, agent.Event{Type: agent.TypeDone, Content: res.Content, Usage: res.Usage}) {
		return res, ctx.Err()
	}
	slog.Warn("工具循环达到最大步数", "steps", l.cfg.MaxSteps, "tools", len(res.ToolCalls))
	return res, nil
}

// executeOne 执行单个工具调用（含审批与超时）。
func (l *Loop) executeOne(ctx context.Context, call agent.ToolCall) agent.ToolResult {
	start := time.Now()
	result := agent.ToolResult{CallID: call.ID, Name: call.Name}

	if l.cfg.Registry == nil {
		result.Output = "工具系统未启用"
		result.IsError = true
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}

	// 先验证工具存在：未知工具直接报错，不进审批流程。
	// （否则用户会看到「需要确认执行命令」这种与实际情况无关的提示。）
	t, ok := l.cfg.Registry.Get(call.Name)
	if !ok {
		result.Output = fmt.Sprintf("工具 %q 不存在。可用工具：%s",
			call.Name, strings.Join(l.cfg.Registry.Names(), ", "))
		result.IsError = true
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}

	// 交互型工具（ask_user）：正常路径已在 suspendAll 里统一处理，
	// 走到这里说明「无法挂起」（缺 sessionId / 无存储 / 参数非法），
	// 此时返回一条可读的错误结果，让模型自己继续而不是卡死。
	if t.Spec().Interactive {
		return l.suspend(ctx, call, start)
	}

	// 审批。
	allowed, reason := l.cfg.Approver.Approve(ctx, call)
	if !allowed {
		if reason == "" {
			reason = "用户未授权此操作"
		}
		result.Output = "操作未执行：" + reason
		// Rejected 与 IsError 分开：模型应知道「是人不让做」而非「工具坏了」。
		result.Rejected = true
		result.IsError = true
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}

	toolCtx, cancel := context.WithTimeout(ctx, l.cfg.ToolTimeout)
	defer cancel()

	res, err := l.cfg.Registry.Invoke(toolCtx, call.Name, json.RawMessage(call.Args))
	result.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		result.Output = "工具执行失败：" + err.Error()
		result.IsError = true
		return result
	}
	result.Output = res.Output
	result.IsError = res.IsError
	return result
}

// clearCheckpoint 清理本会话的 checkpoint（正常收尾时调用）。
//
// 用独立 ctx：调用时 runCtx 可能正在取消，但清理必须生效，
// 否则会留下一条「永远在等回答」的假记录。
func (l *Loop) clearCheckpoint() {
	if l.cfg.Checkpointer == nil || strings.TrimSpace(l.cfg.SessionID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.cfg.Checkpointer.DeleteCheckpoint(ctx, l.cfg.SessionID); err != nil {
		slog.Warn("清理 checkpoint 失败", "err", err, "session_id", l.cfg.SessionID)
	}
}

// saveRunningCheckpoint 在执行开始时写一条 running checkpoint。
//
// 失败只记日志：挂起能力是增强功能，不能因为写入失败就让整轮对话失败。
func (l *Loop) saveRunningCheckpoint(ctx context.Context) {
	if l.cfg.Checkpointer == nil || strings.TrimSpace(l.cfg.SessionID) == "" {
		return
	}
	cp := &session.Checkpoint{
		SessionID: l.cfg.SessionID,
		Version:   1,
		RunID:     l.runID,
		StepIndex: 0,
		State:     session.CheckpointRunning,
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.cfg.Checkpointer.SaveCheckpoint(saveCtx, cp); err != nil {
		slog.Warn("写入 running checkpoint 失败", "err", err, "session_id", l.cfg.SessionID)
	}
}

// suspendAll 收集本轮所有交互型工具调用，写一条包含全部待回答项的 checkpoint。
//
// 返回 true 表示本轮已挂起，调用方应立刻返回（用**自己的** res，
// 不能返回这里构造的对象——挂起是「本轮暂止」，不是「产出了结果」）。
//
// 与早期实现的关键差异：不再「遇到第一个就挂起」。同一轮里的多个
// ask_user 必须一次性全部呈现给用户，否则后面的问题会静默消失——
// 它们的 tool_calls 已经落库，却永远不会有人回答。
func (l *Loop) suspendAll(
	ctx context.Context,
	calls []agent.ToolCall,
	out chan<- agent.Event,
	step int,
) bool {
	// 找出本轮所有交互型调用。
	var interactive []agent.ToolCall
	for _, call := range calls {
		if l.cfg.Registry == nil {
			break
		}
		t, ok := l.cfg.Registry.Get(call.Name)
		if !ok {
			continue
		}
		if t.Spec().Interactive {
			interactive = append(interactive, call)
		}
	}
	if len(interactive) == 0 {
		return false
	}

	// ---- 安全分支：无法挂起时降级 ----
	//
	// 缺 sessionId 或没有存储时，不能假装挂起（用户永远等不到下文）。
	// 退化为每个交互调用一条错误结果，让模型自己继续。
	reason := ""
	switch {
	case strings.TrimSpace(l.cfg.SessionID) == "":
		reason = "当前会话未配置 ID，无法保存待回答的问题。请基于现有信息继续，" +
			"或直接在回答中说明需要用户补充什么。"
	case l.cfg.Checkpointer == nil:
		reason = "当前环境不支持保存待回答的问题。请基于现有信息继续，" +
			"或直接在回答中说明需要用户补充什么。"
	}
	if reason != "" {
		slog.Warn("交互型工具被调用但无法挂起，降级为直接返回错误",
			"count", len(interactive), "session_id", l.cfg.SessionID, "reason", reason)
		return false // 交给 executeOne 逐条产出错误结果
	}

	// 参数不合法的问题不能进入 checkpoint：存下一个空白问题，
	// 用户面对的是一张无从回答的卡片。这类调用就地失败，其余照常挂起。
	//
	// 注意：一个 ask_user 调用里可能含**多个**问题（questions 数组），
	// 这里要把它们全部展开成独立的 Pending，否则用户只能看到第一个。
	pendings := make([]session.Pending, 0, len(interactive))
	var invalid []agent.ToolCall
	invalidCalls := make(map[string]bool, len(interactive))
	for _, call := range interactive {
		questions, err := tool.ParseAskUserQuestions(json.RawMessage(call.Args))
		if err != nil {
			slog.Warn("ask_user 参数不合法", "err", err, "call_id", call.ID)
			invalid = append(invalid, call)
			invalidCalls[call.ID] = true
			continue
		}
		for _, q := range questions {
			pendings = append(pendings, session.Pending{
				Kind:     session.PendingInput,
				CallID:   call.ID,
				QID:      q.ID,
				Name:     call.Name,
				Args:     call.Args,
				Question: q.Question,
				Options:  q.Options,
				Multi:    q.Multi,
			})
		}
	}
	// 全部非法：没有可挂起的内容，让 executeOne 逐条报错。
	if len(pendings) == 0 {
		return false
	}

	cp := &session.Checkpoint{
		SessionID: l.cfg.SessionID,
		Version:   1,
		RunID:     l.runID,
		StepIndex: step,
		State:     session.CheckpointSuspended,
		Pendings:  pendings,
		Completed: l.completed,
	}

	// 落库用独立 ctx：runCtx 可能正在取消（用户点停止），
	// 但 checkpoint 必须写成功，否则这些提问就彻底丢了。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.cfg.Checkpointer.SaveCheckpoint(saveCtx, cp); err != nil {
		slog.Error("保存 checkpoint 失败", "err", err,
			"session_id", l.cfg.SessionID, "count", len(pendings))
		return false
	}

	slog.Info("已挂起等待用户回答",
		"session_id", l.cfg.SessionID, "count", len(pendings),
		"step", step, "invalid", len(invalid))

	l.suspended = cp
	l.stepIndex = step
	return true
}

// suspend 处理单个交互型工具（旧的单条路径）。
//
// 现在主流程走 suspendAll（支持同轮多问题），这个函数保留给
// executeOne 在「只有一条且未预扫描」的场景下使用，
// 保证行为与 suspendAll 一致。
func (l *Loop) suspend(ctx context.Context, call agent.ToolCall, start time.Time) agent.ToolResult {
	result := agent.ToolResult{CallID: call.ID, Name: call.Name}
	fail := func(reason string) agent.ToolResult {
		result.Output = reason
		result.IsError = true
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}

	// ---- 安全分支 1：没有 sessionId ----
	//
	// 这是最容易踩的坑：checkpoint 以 session_id 为主键，缺失时
	// 要么外键失败、要么落进空主键。与其让用户看到一张 resume 不了
	// 的空白卡片，不如退化为一条工具结果，让模型自己继续。
	if strings.TrimSpace(l.cfg.SessionID) == "" {
		slog.Warn("交互型工具被调用但未配置 SessionID，降级为直接执行",
			"tool", call.Name, "call_id", call.ID)
		return fail("当前会话未配置 ID，无法保存待回答的问题。请基于现有信息继续，" +
			"或直接在回答中说明需要用户补充什么。")
	}

	// ---- 安全分支 2：没有存储 ----
	if l.cfg.Checkpointer == nil {
		slog.Debug("交互型工具被调用但未配置 Checkpointer，降级为直接执行",
			"tool", call.Name, "call_id", call.ID)
		return fail("当前环境不支持保存待回答的问题。请基于现有信息继续，" +
			"或直接在回答中说明需要用户补充什么。")
	}

	// ---- 校验参数 ----
	//
	// 挂起前就必须确认 question 非空：否则会存下一个空白 checkpoint，
	// 用户 resume 时面对的是一张没有问题的卡片，且无从判断该填什么。
	questions, err := tool.ParseAskUserQuestions(json.RawMessage(call.Args))
	if err != nil {
		slog.Warn("ask_user 参数不合法", "err", err, "call_id", call.ID)
		return fail(fmt.Sprintf("提问参数不合法：%v。请修正参数后重试。", err))
	}

	// ---- 写 checkpoint（核心：sessionId 在此落库）----
	pendings := make([]session.Pending, 0, len(questions))
	for _, q := range questions {
		pendings = append(pendings, session.Pending{
			Kind:     session.PendingInput,
			CallID:   call.ID,
			QID:      q.ID,
			Name:     call.Name,
			Args:     call.Args,
			Question: q.Question,
			Options:  q.Options,
			Multi:    q.Multi,
		})
	}
	cp := &session.Checkpoint{
		SessionID: l.cfg.SessionID,
		Version:   1,
		RunID:     l.runID,
		StepIndex: l.stepIndex,
		State:     session.CheckpointSuspended,
		Pendings:  pendings,
		Completed: l.completed,
	}

	// 落库用独立 ctx：runCtx 可能正在取消（用户点停止），
	// 但 checkpoint 必须写成功，否则这次提问就彻底丢了。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := l.cfg.Checkpointer.SaveCheckpoint(saveCtx, cp); err != nil {
		slog.Error("保存 checkpoint 失败", "err", err,
			"session_id", l.cfg.SessionID, "call_id", call.ID)
		return fail("保存待回答的问题失败：" + err.Error())
	}

	slog.Info("已挂起等待用户回答",
		"session_id", l.cfg.SessionID, "call_id", call.ID,
		"questions", len(questions))

	// 记录挂起标记，供 Run 在循环里识别并优雅退出。
	l.suspended = cp
	result.Output = "已向用户提问，等待回答。"
	result.DurationMs = time.Since(start).Milliseconds()
	return result
}

// riskOf 查询工具的危险等级。
// 未知工具返回 (RiskDangerous, false)：虽然按危险等级归类，
// 但**不需要审批**——它会直接执行失败并返回「工具不存在」，
// 否则用户会看到莫名其妙的「需要确认执行命令」提示。
func (l *Loop) riskOf(name string) (tool.Risk, bool) {
	if l.cfg.Registry == nil {
		return tool.RiskSafe, false
	}
	t, ok := l.cfg.Registry.Get(name)
	if !ok {
		return tool.RiskDangerous, false
	}
	risk := t.Spec().Risk
	return risk, risk.RequiresApproval()
}

func (l *Loop) toolSpecs() []tool.Spec {
	if l.cfg.Registry == nil || l.cfg.Registry.Len() == 0 {
		return nil
	}
	return l.cfg.Registry.Specs()
}

// emit 尝试发送事件，ctx 取消时返回 false。
func emit(ctx context.Context, out chan<- agent.Event, ev agent.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- ev:
		return true
	}
}

func toSessionToolCalls(calls []agent.ToolCall) []session.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]session.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, session.ToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
	}
	return out
}

func statusFor(r agent.ToolResult) session.MessageStatus {
	if r.IsError {
		return session.StatusError
	}
	return session.StatusComplete
}

func newMessageID() string {
	return "msg_" + randHex()
}

var (
	randMu   sync.Mutex
	randSeed uint64
)

// randHex 生成 8 字节随机十六进制串（避免引入 crypto/rand 依赖到热路径）。
func randHex() string {
	randMu.Lock()
	randSeed = randSeed*6364136223846793005 + 1442695040888963407
	v := randSeed
	randMu.Unlock()
	const digits = "0123456789abcdef"
	var out [16]byte
	for i := 15; i >= 0; i-- {
		out[i] = digits[v&0xf]
		v >>= 4
	}
	return string(out[:])
}
