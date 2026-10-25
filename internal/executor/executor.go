// Package executor 管理「执行任务」的生命周期，使其独立于 HTTP 请求。
//
// # 为什么需要这一层
//
// 早期实现里，执行上下文派生自 r.Context()：
//
//	runCtx, cancel := context.WithTimeout(r.Context(), RunTimeout)
//
// 这意味着「客户端断开 → 运行中止」。对纯聊天是合理优化（省 token），
// 但引入 checkpoint / HITL 后变成致命缺陷：
//
//   - 等待人工审批可能几分钟到几小时，连接必然断开；
//   - 用户刷新页面 / 关标签页，任务就死了；
//   - 长任务（RAG + 多工具）无法跨请求继续。
//
// 因此执行任务的生命周期必须由本层独立持有：
//   - 连接断开 → 仅 detach（停止推送），不 cancel；
//   - 只有显式 Stop / 超时 / 会话删除才 cancel；
//   - 任务事件通过 sse.Hub 广播，重连后可续订阅。
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/sse"
	"agent_rebuild/internal/tool"
)

// 执行层错误。
var (
	// ErrBusy 该会话已有运行中的任务。
	ErrBusy = errors.New("executor: 该会话已有运行中的任务")
	// ErrNoRunner Runner 未配置。
	ErrNoRunner = errors.New("executor: runner 未配置")
	// ErrModelConfigInvalid 指定的模型配置无法使用（不存在 / 已被删除 / 参数非法）。
	ErrModelConfigInvalid = errors.New("executor: 模型配置不可用")
)

// Request 是一次执行的输入。
type Request struct {
	// SessionID 所属会话。
	SessionID string
	// MessageID assistant 占位消息 ID（事件与落库都以此为准）。
	MessageID string
	// History 到本次执行前的完整历史（含刚落的 user 消息）。
	History []session.Message
	// Model 覆盖会话默认模型（可为空）。
	Model string
	// ModelConfigID 指定使用哪一套模型配置（对应 model_configs.id）。
	//
	// 为空时使用全局默认配置。这是「会话级/请求级选模型」的入口：
	// 同一会话可以本次用 A 模型、下次用 B 模型，无需重启。
	ModelConfigID string
	// RunTimeout 单次执行总时限；<=0 使用默认值。
	RunTimeout time.Duration
}

// RunnerResolver 按模型配置 ID 解析出可用的 Runner。
//
// 为什么用「解析器」而不是直接传 Runner：
//   - 每个模型配置的协议/地址/密钥都不同，需要现场构造
//   - 构造失败（如配置被删除）应转换成友好错误，而不是让请求 panic
//
// 返回的 Runner 由调用方使用，不要求幂等（每次执行都会重新解析，
// 保证用户改了密钥后立即生效）。
type RunnerResolver func(modelConfigID string) (agent.Runner, error)

// Status 任务状态。
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Task 是一次执行任务的句柄。可被多次订阅（重连、多标签页）。
type Task struct {
	ID        string
	SessionID string
	MessageID string
	StartedAt time.Time

	hub    *sse.Hub
	cancel context.CancelFunc
	done   chan struct{}
	// gate 关闭后才允许执行体开始跑 Runner。
	//
	// 原因：Start 返回与首次 Subscribe 之间若有窗口，Runner 可能已产生
	// （甚至产生完）事件，导致 message.start 与后续事件顺序错乱。
	// 由首个订阅方关闭 gate，保证「先有观察者，再有事件」。
	gate     chan struct{}
	gateOnce sync.Once

	mu     sync.RWMutex
	status Status
	err    error
}

// openGate 放行执行体。可重复调用。
func (t *Task) openGate() {
	t.gateOnce.Do(func() { close(t.gate) })
}

// Done 在任务终止（成功/失败/取消）时关闭。
func (t *Task) Done() <-chan struct{} { return t.done }

// Status 返回当前状态。
func (t *Task) Status() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}

// Err 返回失败原因（仅 StatusFailed 时非 nil）。
func (t *Task) Err() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.err
}

// Subscribe 订阅事件流。
//
// after 为客户端已知的最后一个事件序号（0 表示从当前开始）。
// 返回的 cancel 必须调用，用于释放订阅（关闭标签页等），
// 但**不会**终止任务本身——这正是与旧实现的关键区别。
//
// 首次订阅会放行执行体（见 Task.gate）：保证消息顺序从 start 开始。
func (t *Task) Subscribe(after uint64) (replay []sse.Event, live <-chan sse.Event, cancel func(), ok bool) {
	replay, live, cancel, ok = t.hub.Subscribe(t.SessionID, after)
	t.openGate()
	return replay, live, cancel, ok
}

// Publish 由执行体调用，广播一个事件（自动分配 seq）并写入环形缓冲。
func (t *Task) Publish(name string, build func(seq uint64) any) sse.Event {
	return t.hub.Publish(t.SessionID, func(seq uint64) sse.Event {
		return sse.Event{Name: name, Data: build(seq)}
	})
}

// PublishEphemeral 广播一个不参与重放的事件（如 message.start 握手）。
func (t *Task) PublishEphemeral(name string, build func(seq uint64) any) sse.Event {
	return t.hub.PublishEphemeral(t.SessionID, func(seq uint64) sse.Event {
		return sse.Event{Name: name, Data: build(seq)}
	})
}

// LastSeq 返回当前最大事件序号。
func (t *Task) LastSeq() uint64 { return t.hub.LastID(t.SessionID) }

// Executor 管理执行中的任务。
type Executor struct {
	hub   *sse.Hub
	store session.Store
	// runnerFn 延迟获取 Runner：便于测试替换与运行时热切换。
	runnerFn func() agent.Runner
	// resolver 按模型配置 ID 构造 Runner（nil 表示不支持多模型选择）。
	resolver RunnerResolver

	// 工具系统（nil 表示不启用）。
	registry     *tool.Registry
	systemPrompt string
	maxSteps     int
	toolTimeout  time.Duration
	approver     loop.Approver

	mu    sync.Mutex
	tasks map[string]*Task // sessionID -> Task

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
}

// New 创建执行器。
//
// runnerFn 延迟取 Runner（而非构造时固定），这样：
//   - 测试可以替换 Server 上的 runner 而无需重建 executor；
//   - 将来支持运行时热切换模型/协议。
func New(runnerFn func() agent.Runner, hub *sse.Hub, store session.Store) *Executor {
	return &Executor{
		runnerFn: runnerFn,
		hub:      hub,
		store:    store,
		tasks:    make(map[string]*Task),
		closed:   make(chan struct{}),
	}
}

// Start 启动一次执行。
//
// 关键：返回的 Task 生命周期与调用方无关。调用方（HTTP handler）断开
// 不会影响任务；只有 Stop / 超时 / 执行体自身结束才会终止。
func (e *Executor) Start(ctx context.Context, req Request) (*Task, error) {
	// 解析 Runner：优先用请求指定的模型配置，否则回退全局默认。
	runner, err := e.ResolveRunner(req.ModelConfigID)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, ErrNoRunner
	}

	e.mu.Lock()
	if _, exists := e.tasks[req.SessionID]; exists {
		e.mu.Unlock()
		return nil, ErrBusy
	}

	timeout := req.RunTimeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}

	// ★ 核心：从 ctx 剥离取消信号。
	// WithoutCancel 保留 ctx 的值（如 request id），但断开取消关联，
	// 因此客户端断开不会传导到执行上下文。
	base := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(base, timeout)

	task := &Task{
		ID:        "task_" + req.MessageID,
		SessionID: req.SessionID,
		MessageID: req.MessageID,
		StartedAt: time.Now().UTC(),
		hub:       e.hub,
		cancel:    cancel,
		done:      make(chan struct{}),
		gate:      make(chan struct{}),
		status:    StatusRunning,
	}
	e.tasks[req.SessionID] = task
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		// 等首个订阅者就绪再开跑：避免 start 与后续事件乱序。
		// 超时兜底，防止无人订阅时任务永久挂起（例如 stream=false 路径）。
		select {
		case <-task.gate:
		case <-runCtx.Done():
		case <-e.closed:
		}
		e.run(runCtx, task, req, cancel, runner)
	}()

	return task, nil
}

// Get 取回运行中的任务（用于重连后订阅）。
func (e *Executor) Get(sessionID string) (*Task, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[sessionID]
	return t, ok
}

// Stop 显式终止任务（对应 POST /cancel）。
func (e *Executor) Stop(sessionID string) bool {
	e.mu.Lock()
	t, ok := e.tasks[sessionID]
	e.mu.Unlock()
	if !ok {
		return false
	}
	t.cancel()
	return true
}

// Shutdown 取消所有任务并等待收尾（优雅退出用）。
//
// 顺序很重要：先 cancel 让执行体有机会把 interrupted 状态落库，再等待。
func (e *Executor) Shutdown(ctx context.Context) error {
	e.closeOnce.Do(func() { close(e.closed) })

	e.mu.Lock()
	for _, t := range e.tasks {
		t.cancel()
	}
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("executor: 等待任务收尾超时: %w", ctx.Err())
	}
}

// SetTools 启用工具系统。
//
// 必须在 Start 之前调用（并发安全：内部有锁）。
func (e *Executor) SetTools(registry *tool.Registry, systemPrompt string, maxSteps int, timeout time.Duration, approver loop.Approver) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.registry = registry
	e.systemPrompt = systemPrompt
	e.maxSteps = maxSteps
	e.toolTimeout = timeout
	e.approver = approver
}

// ResolveRunner 按模型配置 ID 解析 Runner，供流式与非流式两条路径共用。
//
// 解析顺序：
//  1. modelConfigID 非空 → 用 Resolver 按 ID 构造
//  2. 否则用全局默认（runnerFn）
//
// 这样既支持「会话级选模型」，也保留「不指定就用默认」的简单用法。
func (e *Executor) ResolveRunner(modelConfigID string) (agent.Runner, error) {
	if modelConfigID != "" && e.resolver != nil {
		runner, err := e.resolver(modelConfigID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrModelConfigInvalid, err)
		}
		if runner != nil {
			return runner, nil
		}
	}
	return e.runnerFn(), nil
}

// SetResolver 注入按模型配置解析 Runner 的能力。
//
// 未注入时，所有请求都回退到全局默认 Runner（向后兼容）。
func (e *Executor) SetResolver(resolver RunnerResolver) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resolver = resolver
}

// ActiveCount 返回运行中的任务数（调试用）。
func (e *Executor) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tasks)
}

// ---- 内部实现 ----

// run 是执行体：消费 Runner 事件、广播给订阅者、收尾落库。
func (e *Executor) run(runCtx context.Context, task *Task, req Request, cancel context.CancelFunc, runner agent.Runner) {
	defer cancel()
	defer close(task.done)
	defer func() {
		e.mu.Lock()
		delete(e.tasks, req.SessionID)
		e.mu.Unlock()
	}()

	// 兜底落库用独立 ctx：执行 ctx 可能已取消，不能用它写库。
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer persistCancel()

	finish := func(status Status, content string, msgStatus session.MessageStatus, msgErr *session.MessageError, usage *sse.Usage) {
		task.mu.Lock()
		task.status = status
		task.mu.Unlock()
		e.persistMessage(persistCtx, req.SessionID, req.MessageID, content, msgStatus, msgErr, usage)
	}

	var acc []byte
	// 优先用 Loop（支持工具循环）；未配置时退化为单轮 Runner。
	events := e.streamEvents(runCtx, runner, req, persistCtx)

	for {
		select {
		case <-runCtx.Done():
			// 超时或显式取消。
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				task.Publish(sse.EventMessageError, func(seq uint64) any {
					return sse.MessageErrorData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
						Error: &sse.ErrorPayload{Code: "internal_error", Message: "生成超时"},
					}
				})
				finish(StatusFailed, string(acc), session.StatusError,
					&session.MessageError{Code: "internal_error", Message: "生成超时"}, nil)
				return
			}
			// 显式取消：告知前端已中断。
			task.Publish(sse.EventMessageInterrupted, func(seq uint64) any {
				return sse.MessageInterruptedData{
					Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Content: string(acc),
				}
			})
			finish(StatusCanceled, string(acc), session.StatusInterrupted, nil, nil)
			return

		case ev, ok := <-events:
			if !ok {
				// Runner 关闭但未发 done：按中断处理，避免前端永久转圈。
				task.Publish(sse.EventMessageInterrupted, func(seq uint64) any {
					return sse.MessageInterruptedData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Content: string(acc),
					}
				})
				finish(StatusCanceled, string(acc), session.StatusInterrupted, nil, nil)
				return
			}

			switch ev.Type {
			case agent.TypeDelta:
				if ev.Delta == "" {
					continue
				}
				acc = append(acc, ev.Delta...)
				task.Publish(sse.EventMessageDelta, func(seq uint64) any {
					return sse.MessageDeltaData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Delta: ev.Delta,
					}
				})

			case agent.TypeDone:
				content := ev.Content
				if content == "" {
					content = string(acc)
				}
				var usage *session.Usage
				if ev.Usage != nil {
					usage = &session.Usage{
						PromptTokens:     ev.Usage.PromptTokens,
						CompletionTokens: ev.Usage.CompletionTokens,
						TotalTokens:      ev.Usage.TotalTokens,
					}
				}
				sseUsage := toSSEUsage(usage)
				// 先落库再广播：保证收到 done 时历史已可查。
				e.persistMessage(persistCtx, req.SessionID, req.MessageID, content,
					session.StatusComplete, nil, sseUsage)
				task.mu.Lock()
				task.status = StatusSucceeded
				task.mu.Unlock()
				task.Publish(sse.EventMessageDone, func(seq uint64) any {
					return sse.MessageDoneData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
						Content: content, FinishReason: "stop", Usage: sseUsage,
					}
				})
				return

			case agent.TypeToolCall:
				// 工具调用请求：推给前端展示（参数可供用户审查）。
				if ev.ToolCall == nil {
					continue
				}
				call := *ev.ToolCall
				eventName := sse.EventToolCall
				if call.RequiresApproval {
					eventName = sse.EventToolApproval
				}
				task.Publish(eventName, func(seq uint64) any {
					return sse.ToolCallData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
						CallID: call.ID, Name: call.Name, Args: call.Args,
						Risk: call.Risk.String(), RequiresApproval: call.RequiresApproval,
					}
				})

			case agent.TypeToolResult:
				if ev.ToolResult == nil {
					continue
				}
				r := *ev.ToolResult
				task.Publish(sse.EventToolResult, func(seq uint64) any {
					return sse.ToolResultData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
						CallID: r.CallID, Name: r.Name, Output: r.Output,
						IsError: r.IsError, Rejected: r.Rejected, DurationMs: r.DurationMs,
					}
				})

			case agent.TypeError:
				if errors.Is(ev.Err, context.Canceled) {
					task.Publish(sse.EventMessageInterrupted, func(seq uint64) any {
						return sse.MessageInterruptedData{
							Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Content: string(acc),
						}
					})
					finish(StatusCanceled, string(acc), session.StatusInterrupted, nil, nil)
					return
				}
				msg := "生成失败"
				if ev.Err != nil {
					msg = ev.Err.Error()
				}
				payload := &sse.ErrorPayload{Code: "provider_unavailable", Message: msg}
				task.Publish(sse.EventMessageError, func(seq uint64) any {
					return sse.MessageErrorData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Error: payload,
					}
				})
				finish(StatusFailed, string(acc), session.StatusError,
					&session.MessageError{Code: payload.Code, Message: msg}, nil)
				return

			default:
				slog.Debug("忽略未知执行层事件", "type", ev.Type, "session_id", req.SessionID)
			}
		}
	}
}

// streamEvents 返回执行事件流。
//
// 有工具注册表时走 Loop（ReAct 多轮循环），否则退化为单轮 Runner。
// 两条路径都产出统一的 agent.Event 序列，下游无感。
func (e *Executor) streamEvents(
	runCtx context.Context,
	runner agent.Runner,
	req Request,
	persistCtx context.Context,
) <-chan agent.Event {
	out := make(chan agent.Event, 16)

	if e.registry == nil || e.registry.Len() == 0 {
		// 无工具：直接转发 Runner 事件。
		go func() {
			defer close(out)
			for ev := range runner.Run(runCtx, agent.Request{
				History:      req.History,
				SystemPrompt: e.systemPrompt,
			}) {
				select {
				case <-runCtx.Done():
					return
				case out <- ev:
				}
			}
		}()
		return out
	}

	go func() {
		defer close(out)
		lp := loop.New(runner, loop.Config{
			Registry:     e.registry,
			SystemPrompt: e.systemPrompt,
			MaxSteps:     e.maxSteps,
			ToolTimeout:  e.toolTimeout,
			Approver:     e.approver,
		})
		res, err := lp.Run(runCtx, req.History, out)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			select {
			case <-runCtx.Done():
			case out <- agent.Event{Type: agent.TypeError, Err: err}:
			}
			return
		}
		// 工具调用记录落库。
		//
		// 顺序极其重要：必须先落 assistant 的 tool_calls 消息，
		// 再落对应的 tool 结果。否则 OpenAI 会以
		// "Messages with role 'tool' must be a response to a preceding
		// message with 'tool_calls'" 拒绝后续请求。
		if res != nil && len(res.ToolResults) > 0 {
			e.persistToolMessages(persistCtx, req.SessionID, res)
		}
	}()
	return out
}

// persistToolMessages 把 assistant 的 tool_calls 与对应 tool 结果成对落库。
//
// 历史回放必须保持「tool_calls → tool 结果」的配对关系，
// 因此这里按 assistant 消息的顺序，逐条落其发起的调用与结果。
func (e *Executor) persistToolMessages(ctx context.Context, sessionID string, res *loop.Result) {
	if e.store == nil || len(res.ToolResults) == 0 {
		return
	}

	// 建立 callID → 结果 的索引，便于按 assistant 消息分组。
	resultByCall := make(map[string]agent.ToolResult, len(res.ToolResults))
	for _, r := range res.ToolResults {
		resultByCall[r.CallID] = r
	}

	now := time.Now().UTC()
	for _, asst := range res.AssistantMessages {
		// 1) 先落 assistant（带 tool_calls），它是 tool 消息的前置。
		out := &session.Message{
			ID:        session.NewMessageID(),
			SessionID: sessionID,
			Role:      session.RoleAssistant,
			Content:   asst.Content,
			Status:    session.StatusComplete,
			ToolCalls: asst.ToolCalls,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := e.store.AppendMessage(ctx, out); err != nil {
			slog.Warn("assistant 工具调用落库失败", "err", err, "session_id", sessionID)
			// 落库失败时不能继续落 tool 消息，否则又会产生孤儿。
			continue
		}

		// 2) 再落该 assistant 消息里每个调用对应的结果。
		for _, tc := range asst.ToolCalls {
			r, ok := resultByCall[tc.ID]
			if !ok {
				// 没有结果（例如执行被中断）：不要凭空造一条 tool 消息。
				continue
			}
			status := session.StatusComplete
			if r.IsError {
				status = session.StatusError
			}
			toolMsg := &session.Message{
				ID:         session.NewMessageID(),
				SessionID:  sessionID,
				Role:       session.RoleTool,
				Content:    r.Output,
				Status:     status,
				ToolCallID: r.CallID,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if err := e.store.AppendMessage(ctx, toolMsg); err != nil {
				slog.Warn("工具结果落库失败", "err", err, "call_id", r.CallID)
			}
		}
	}
}
func (e *Executor) persistMessage(
	ctx context.Context, sessionID, msgID, content string,
	msgStatus session.MessageStatus, msgErr *session.MessageError, usage *sse.Usage,
) {
	if e.store == nil {
		return
	}
	err := e.store.UpdateMessage(ctx, sessionID, msgID, func(m *session.Message) error {
		m.Content = content
		m.Status = msgStatus
		m.Error = msgErr
		if usage != nil {
			m.Usage = &session.Usage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("落库消息最终状态失败", "err", err, "session_id", sessionID, "message_id", msgID)
		return
	}

	sessStatus := session.SessionIdle
	if msgStatus == session.StatusError {
		sessStatus = session.SessionError
	}
	if _, err := e.store.Update(ctx, sessionID, func(sc *session.Session) error {
		sc.Status = sessStatus
		return nil
	}); err != nil && !errors.Is(err, session.ErrNotFound) {
		slog.Warn("落库会话状态失败", "err", err, "session_id", sessionID)
	}
}

func toSSEUsage(u *session.Usage) *sse.Usage {
	if u == nil {
		return nil
	}
	return &sse.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}
