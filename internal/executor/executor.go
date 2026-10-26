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
	"strings"
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
	// checkpointer 断点存储（nil 表示不支持挂起等待用户回答）。
	checkpointer session.CheckpointStore

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
	// 追加工具使用准则：模型（尤其小模型）拿到工具后极易反复试探同一个
	// 文件、把 12 轮步数耗尽，最后什么结论都给不出。显式约束能显著减少
	// 无效往返。
	e.systemPrompt = withToolGuidance(systemPrompt)
	e.maxSteps = maxSteps
	e.toolTimeout = timeout
	e.approver = approver
}

// SetCheckpointer 注入断点存储，启用「等待用户回答」能力。
//
// 单独一个 setter 而不是塞进 SetTools：不是所有部署形态都需要挂起
// （例如内存存储的单进程 demo），把它做成可选依赖更贴合实际。
func (e *Executor) SetCheckpointer(cp session.CheckpointStore) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkpointer = cp
}

// SetApprover 运行时热替换审批策略，不影响已注册的工具/系统提示/步数限制。
//
// 单独一个 setter 而不是复用 SetTools：切换审批模式（manual/model/auto）
// 时没有理由连 registry、systemPrompt、maxSteps、toolTimeout 一起重设——
// 那些要么没变，要么重设它们本身就有风险（比如把 systemPrompt 的工具
// 使用准则重复追加一遍）。调用方（Server.SetApprovalMode）只需要换掉
// Approver 这一个字段。
//
// 并发安全：已经在跑的任务会在下一次 streamEvents 读取快照时拿到新值——
// 当前实现里 streamEvents 对 e.approver 没有做快照读取（见下方 run 时机），
// 而是 loop.New 时直接读 e.approver 字段，所以严格来说只影响"尚未进入
// loop.New 构造"的那一刻。对运行中的单次工具调用没有影响：
// evaluateApprovals 在同一轮 Run() 内只会读一次 Approver。
func (e *Executor) SetApprover(approver loop.Approver) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.approver = approver
}

// withToolGuidance 在用户系统提示后追加工具使用准则。
//
// 为什么放在代码里而不是只写文档：这是「不照做就明显变差」的行为约束，
// 应该在所有部署形态下默认生效，而不是依赖用户手动配置。
// 用户仍可通过 SYSTEM_PROMPT 覆盖自己的部分，但准则会保留。
func withToolGuidance(base string) string {
	guidance := strings.Join([]string{
		"## 工具使用准则",
		"",
		"1. 先规划再动手：先用一次 list_dir 或 search 定位，不要逐个文件盲试。",
		"2. 不要重复读取同一个文件。已经读过的内容就在上文，直接使用；",
		"   需要该文件的其他部分时，用 offset/limit 精准读取，不要整篇重读。",
		"3. 优先用 search 一次定位多处，再用 read_file 精读关键片段。",
		"4. 工具结果足以回答时立刻给出答案，不要为了「再确认一下」继续调用。",
		"5. 单个问题通常 3-5 次工具调用内就能回答；接近上限仍无结论时，",
		"   应基于已有信息作答并说明还缺什么。",
		"6. 工具报错时先读错误信息调整参数，不要用相同参数重试。",
	}, "\n")

	base = strings.TrimSpace(base)
	if base == "" {
		return guidance
	}
	return base + "\n\n" + guidance
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
	//
	// 注意：这里**不能**用 defer cancel，原因见下方注释。
	//
	// 关键：这里**不设整体超时**。早期实现给 persistCtx 设过 30s 固定超时，
	// 问题是 runCtx（整轮执行）可以跑到几分钟甚至更久，而工具调用可能发生在
	// 执行的任意时刻——一旦某次工具调用落库发生在 30s 之后，会直接超时失败，
	// 与"任务跑多久"无关，只与"工具调用发生得多晚"有关，是一个与实际执行时长
	// 脱节的超时设计（线上出现过大量 context deadline exceeded 正是此因）。
	//
	// 现在改为：persistCtx 本身不设超时，每一次实际的数据库操作
	// （appendToolCallMessage / appendToolResultMessage / persistMessage）
	// 各自套一个较短的独立超时（见对应函数），既能防止单次数据库操作真正
	// 卡死，又不会因为"整体跑得久"而误伤本该成功的落库。
	persistCtx := context.Background()

	finish := func(status Status, content string, msgStatus session.MessageStatus, msgErr *session.MessageError, usage *sse.Usage) {
		task.mu.Lock()
		task.status = status
		task.mu.Unlock()
		e.persistMessage(persistCtx, req.SessionID, req.MessageID, content, msgStatus, msgErr, usage)
	}

	var acc []byte
	// 优先用 Loop（支持工具循环）；未配置时退化为单轮 Runner。
	// done 信号用于等待内部 goroutine（含工具落库）真正结束，
	// 确保 run() 返回前，所有工具调用/结果都已经尝试落库完毕。
	events, eventsDone := e.streamEvents(runCtx, runner, req, persistCtx)
	defer func() {
		if eventsDone != nil {
			select {
			case <-eventsDone:
			case <-time.After(10 * time.Second):
				slog.Warn("等待工具落库超时", "session_id", req.SessionID)
			}
		}
	}()

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

				// 落库顺序：工具消息在事件到达时已经同步落库（见下方
				// TypeToolCall / TypeToolResult 分支），这里只需落最终回答。
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

				// 立即落库 assistant 的 tool_calls 消息。
				//
				// 为什么不等 Loop 收尾统一落库：前端收到 done 后可能立刻
				// 刷新或断开，异步落库会被取消；而 tool 消息缺失会让模型
				// 下次以为"没调过工具"，从 list_dir 重来（线上现象）。
				// 在这里落库能与事件到达严格同步，且顺序天然正确
				// （OpenAI 要求 tool 结果紧跟其 tool_calls）。
				e.appendToolCallMessage(persistCtx, req.SessionID, call)

				// 统一发 tool.call，不区分「稍后可能需要人工审批」的调用。
				//
				// 原因：此刻（模型刚流出这个 tool_call）审批决策还没算出来——
				// Loop.Run 里 evaluateApprovals/suspendAll 发生在收集完
				// 本轮全部 toolCalls **之后**，这个事件早在那之前就已经发出。
				// 真正区分「会挂起等人」与「直接执行」的时机是下面的
				// TypeAwaitingInput 分支：按 Pending.Kind 决定发 awaiting.input
				// 还是 tool.approval。Risk / RequiresApproval 两个字段仍然
				// 原样透传给前端（ToolCallCard 会据此渲染"需确认"徽章，纯展示），
				// 为审批卡片保留足够的上下文。
				task.Publish(sse.EventToolCall, func(seq uint64) any {
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

				// 立即落库工具结果（紧跟在对应 tool_calls 之后）。
				e.appendToolResultMessage(persistCtx, req.SessionID, r)

				task.Publish(sse.EventToolResult, func(seq uint64) any {
					return sse.ToolResultData{
						Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
						CallID: r.CallID, Name: r.Name, Output: r.Output,
						IsError: r.IsError, Rejected: r.Rejected, DurationMs: r.DurationMs,
					}
				})

			case agent.TypeAwaitingInput:
				// 本轮挂起等待用户回答：不发 done、不落最终回答。
				//
				// 之前的文本增量（模型在提问前说的话）要保留下来，
				// 否则用户看到一片空白，不知道模型为什么停下来。
				if ev.Suspended == nil || len(ev.Suspended.Pendings) == 0 {
					continue
				}
				cp := ev.Suspended

				// 把已产出的文本落库成 assistant 消息（状态为 streaming），
				// 让「模型说了什么 + 在等什么」一起呈现。
				e.persistMessage(persistCtx, req.SessionID, req.MessageID, string(acc),
					session.StatusStreaming, nil, nil)
				e.markSessionAwaitingInput(persistCtx, req.SessionID)

				task.mu.Lock()
				task.status = StatusSucceeded
				task.mu.Unlock()

				// 按挂起原因分流成两种事件：
				//   - PendingInput（ask_user）  → awaiting.input，前端渲染问答卡片；
				//   - PendingApproval（人工审批）→ tool.approval，前端渲染批准/拒绝卡片。
				//
				// 两者可能出现在同一条 checkpoint 里（模型同轮既问问题又想写文件），
				// 必须按各自的 Kind 分别发对应事件，否则其中一类会在前端
				// 找不到渲染分支（旧版本只认 awaiting.input，tool.approval
				// 事件虽已定义多时但从未被 Publish 过，见 09-tools.md 已知限制）。
				//
				// qid 必须带上：多个问题可能属于**同一个** call_id
				// （ask_user 一次调用里的 questions 数组），
				// 前端只有靠 qid 才能把答案对应回具体问题。
				for _, p := range cp.Pendings {
					pending := p
					if pending.Kind == session.PendingApproval {
						task.Publish(sse.EventToolApproval, func(seq uint64) any {
							return sse.ToolApprovalData{
								Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
								CallID: pending.CallID, Name: pending.Name, Args: pending.Args,
								Risk: pending.Risk, Reason: pending.Reason,
							}
						})
						continue
					}
					task.Publish(sse.EventAwaitingInput, func(seq uint64) any {
						return sse.AwaitingInputData{
							Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
							CallID: pending.CallID, QID: pending.QID,
							Question: pending.Question, Options: pending.Options,
							Multi: pending.Multi, StepIndex: cp.StepIndex,
						}
					})
				}
				return

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

// streamEvents 返回执行事件流与「内部 goroutine 已全部收尾」的信号。
//
// 有工具注册表时走 Loop（ReAct 多轮循环），否则退化为单轮 Runner。
// 两条路径都产出统一的 agent.Event 序列，下游无感。
//
// done 通道的意义：工具调用落库发生在内部 goroutine 里，调用方必须
// 等它关闭后再释放 persistCtx，否则写入会因 ctx 取消而静默失败。
func (e *Executor) streamEvents(
	runCtx context.Context,
	runner agent.Runner,
	req Request,
	persistCtx context.Context,
) (<-chan agent.Event, <-chan struct{}) {
	out := make(chan agent.Event, 16)
	done := make(chan struct{})

	if e.registry == nil || e.registry.Len() == 0 {
		// 无工具：直接转发 Runner 事件。
		go func() {
			defer close(done)
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
		return out, done
	}

	go func() {
		defer close(done)
		defer close(out)

		// 读一次快照：配置可能在 Start 之后被 SetXXX 改动。
		e.mu.Lock()
		checkpointer := e.checkpointer
		e.mu.Unlock()

		lp := loop.New(runner, loop.Config{
			Registry:     e.registry,
			SystemPrompt: e.systemPrompt,
			MaxSteps:     e.maxSteps,
			ToolTimeout:  e.toolTimeout,
			Approver:     e.approver,
			// 关键：把 sessionID 传给 loop，checkpoint 以它为键落库。
			SessionID:    req.SessionID,
			Checkpointer: checkpointer,
		})
		_, err := lp.Run(runCtx, req.History, out)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			select {
			case <-runCtx.Done():
			case out <- agent.Event{Type: agent.TypeError, Err: err}:
			}
			return
		}
		// 本轮因等待用户回答而挂起：发一个专门的事件，
		// 让前端切到「等待输入」状态，而不是把空内容当完成或中断。
		if cp := lp.Suspended(); cp != nil {
			select {
			case <-runCtx.Done():
			case out <- agent.Event{Type: agent.TypeAwaitingInput, Suspended: cp}:
			}
			return
		}
		// 注意：工具消息的落库已在 TypeToolCall / TypeToolResult 事件
		// 到达时同步完成，这里不再重复落库（重复会造成历史里出现
		// 成对的重复 tool_calls，模型会看到"调用了两次"）。
	}()
	return out, done
}

// persistOpTimeout 是单次数据库写操作的超时上限。
//
// 这个超时只保护"单次 SQL 执行"本身（给 SQLite 的 busy_timeout 之外再加一层
// 应用层兜底），不代表"整个任务的持久化阶段"——后者不应该有总时限，详见
// run() 里 persistCtx 的注释。15s 对本地 SQLite 单条 INSERT/UPDATE 是很宽松
// 的上限：正常情况下这类写入是毫秒级的，只有真正卡死（锁等待、磁盘异常）才
// 会触发。
const persistOpTimeout = 15 * time.Second

// appendToolCallMessage 立即落库一条 assistant(tool_calls) 消息。
//
// 每条 tool_call 落一条独立 assistant 消息，与随后的 tool 结果严格配对。
func (e *Executor) appendToolCallMessage(ctx context.Context, sessionID string, call agent.ToolCall) {
	if e.store == nil {
		return
	}
	// 单操作超时：与 persistCtx 本身是否有整体时限无关，见 persistOpTimeout。
	opCtx, cancel := context.WithTimeout(ctx, persistOpTimeout)
	defer cancel()

	now := time.Now().UTC()
	m := &session.Message{
		ID:        session.NewMessageID(),
		SessionID: sessionID,
		Role:      session.RoleAssistant,
		Content:   "",
		Status:    session.StatusComplete,
		ToolCalls: []session.ToolCall{{
			ID: call.ID, Name: call.Name, Args: call.Args,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := e.store.AppendMessage(opCtx, m); err != nil {
		slog.Warn("工具调用消息落库失败", "err", err,
			"session_id", sessionID, "call_id", call.ID)
		// 注意：这里不重试、不 panic。万一这次落库失败，这个 call_id 会
		// 在历史里"悬空"（无 assistant 声明却可能有 tool 结果，或反之）。
		// agent/llm.go、agent/anthropic.go 构建请求时本来就会校验
		// declared/answered 是否成对，不成对的一律剔除——不会导致
		// 下一次请求被 API 拒绝，只是这次工具调用对模型"看起来没发生过"。
	}
}

// appendToolResultMessage 立即落库对应工具结果（role=tool）。
func (e *Executor) appendToolResultMessage(ctx context.Context, sessionID string, r agent.ToolResult) {
	if e.store == nil {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, persistOpTimeout)
	defer cancel()

	status := session.StatusComplete
	if r.IsError {
		status = session.StatusError
	}
	now := time.Now().UTC()
	m := &session.Message{
		ID:         session.NewMessageID(),
		SessionID:  sessionID,
		Role:       session.RoleTool,
		Content:    r.Output,
		Status:     status,
		ToolCallID: r.CallID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := e.store.AppendMessage(opCtx, m); err != nil {
		slog.Warn("工具结果落库失败", "err", err,
			"session_id", sessionID, "call_id", r.CallID)
		// 同上：这个 call_id 的结果没写进去，下次构建请求时
		// agent/llm.go、agent/anthropic.go 的 declared/answered 校验
		// 会发现它"声明了但没应答"，直接把这条调用声明一起丢弃。
	}
}

// markSessionAwaitingInput 把会话标记为「等待用户输入」。
//
// 为什么需要独立的会话状态而不是复用 running/idle：
//   - 复用 running：前端会一直显示「生成中」，用户不知道是在等自己；
//   - 复用 idle：重启后 RecoverStaleRunning 会把它当僵尸清掉，
//     「有会话在等你回答」这个信息就丢了。
//
// 因此加一个 suspended，让恢复逻辑能区分对待。
func (e *Executor) markSessionAwaitingInput(ctx context.Context, sessionID string) {
	if e.store == nil {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, persistOpTimeout)
	defer cancel()
	if _, err := e.store.Update(opCtx, sessionID, func(sc *session.Session) error {
		sc.Status = session.SessionSuspended
		return nil
	}); err != nil && !errors.Is(err, session.ErrNotFound) {
		slog.Warn("标记会话为等待输入失败", "err", err, "session_id", sessionID)
	}
}

// persistMessage 落库最终回答并复位会话状态。
func (e *Executor) persistMessage(
	ctx context.Context, sessionID, msgID, content string,
	msgStatus session.MessageStatus, msgErr *session.MessageError, usage *sse.Usage,
) {
	if e.store == nil {
		return
	}
	// 单操作超时（而非整体 persistCtx 超时），理由同 appendToolCallMessage。
	opCtx, cancel := context.WithTimeout(ctx, persistOpTimeout)
	defer cancel()

	err := e.store.UpdateMessage(opCtx, sessionID, msgID, func(m *session.Message) error {
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
		// 这里失败不代表内容丢失：content 已经通过 SSE 推给了前端，
		// 只是这条消息在数据库里会停留在 streaming 状态。下次读取历史时
		// session.RepairHistory 会据此 content 非空，判定为 complete，
		// 不需要用户重新运行这条消息。
		return
	}

	sessStatus := session.SessionIdle
	if msgStatus == session.StatusError {
		sessStatus = session.SessionError
	}
	// 会话状态复位同样只给单次操作超时，不借用上面可能已耗尽的 opCtx
	// （UpdateMessage 失败时已经 return，这里的 opCtx 仍未取消，可以复用）。
	if _, err := e.store.Update(opCtx, sessionID, func(sc *session.Session) error {
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
