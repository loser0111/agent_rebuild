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
	DefaultMaxSteps = 12
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

	// 工作副本：工具调用的请求与结果都要追加进上下文。
	working := make([]session.Message, len(history), len(history)+l.cfg.MaxSteps*2)
	copy(working, history)

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

		for _, call := range toolCalls {
			result := l.executeOne(ctx, call)
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

	// 达到最大步数：把已有文本作为最终回答，避免用户拿不到任何结果。
	res.Content = "（已达到最大工具调用轮数，回答可能不完整）"
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
	if _, ok := l.cfg.Registry.Get(call.Name); !ok {
		result.Output = fmt.Sprintf("工具 %q 不存在。可用工具：%s",
			call.Name, strings.Join(l.cfg.Registry.Names(), ", "))
		result.IsError = true
		result.DurationMs = time.Since(start).Milliseconds()
		return result
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

// riskOf 查询工具的危险等级。
//
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
