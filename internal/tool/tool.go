// Package tool 提供 agent 可调用的工具抽象与内置工具集。
//
// 设计参考 Codex 与 Claude Code：
//
//   - 工具集精简（10 个以内），覆盖「读、写、搜、跑」四类核心能力
//   - 参数用 JSON Schema 描述，由模型填充、由工具自行校验
//   - 输出强制截断（防大文件/大输出撑爆上下文）
//   - 每个工具标注危险等级，为人工审批（HITL）提供依据
//
// 工具不感知 HTTP / SSE / 会话，只依赖标准库与自身配置。
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Risk 是工具的危险等级，决定是否需要人工确认后才执行。
type Risk int

const (
	// RiskSafe 只读操作，无副作用（读文件、搜索）。
	RiskSafe Risk = iota
	// RiskModerate 有副作用但影响可控（写文件、创建目录）。
	RiskModerate
	// RiskDangerous 可能造成较大影响（执行任意命令、删除）。
	RiskDangerous
)

func (r Risk) String() string {
	switch r {
	case RiskSafe:
		return "safe"
	case RiskModerate:
		return "moderate"
	case RiskDangerous:
		return "dangerous"
	default:
		return "unknown"
	}
}

// RequiresApproval 判断该等级是否需要人工确认。
//
// 策略：Moderate 与 Dangerous 都需要确认。
// 可通过配置放宽（例如信任当前工作目录内的写操作）。
func (r Risk) RequiresApproval() bool {
	return r >= RiskModerate
}

// Spec 是工具的静态描述，用于生成模型可见的工具列表。
type Spec struct {
	// Name 工具名（模型调用时使用，必须唯一且稳定）。
	Name string `json:"name"`
	// Description 何时使用该工具（写给模型看，越具体越好）。
	Description string `json:"description"`
	// Parameters 参数的 JSON Schema。
	Parameters json.RawMessage `json:"parameters"`
	// Risk 危险等级。
	Risk Risk `json:"-"`
}

// Result 是工具执行结果。
type Result struct {
	// Output 返回给模型的文本内容。
	Output string
	// IsError 表示执行失败（模型应据此调整策略）。
	IsError bool
	// Truncated 表示输出被截断（提示模型不要假设拿到全部内容）。
	Truncated bool
}

// Tool 是一个可调用的工具。
type Tool interface {
	// Spec 返回工具描述。
	Spec() Spec
	// Execute 执行工具。args 是模型给出的 JSON 参数。
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Registry 是工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string // 保持注册顺序，让工具列表稳定
	root  string   // 工作区根目录（仅供观测与诊断，工具自身已捕获该值）
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// SetWorkspaceRoot 记录工作区根目录。
func (r *Registry) SetWorkspaceRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.root = root
}

// WorkspaceRoot 返回工具实际使用的工作区根目录。
//
// 单独暴露是为了排查「文件操作被限制在意外目录」这类问题：
// 日志里能直接看到根目录，而不用逐个工具去反推。
func (r *Registry) WorkspaceRoot() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.root
}

// Register 注册工具。名字重复会 panic（属于编程错误，应在启动时暴露）。
func (r *Registry) Register(tools ...Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		name := t.Spec().Name
		if name == "" {
			panic("tool: 工具名不能为空")
		}
		if _, exists := r.tools[name]; exists {
			panic("tool: 重复注册工具 " + name)
		}
		r.tools[name] = t
		r.order = append(r.order, name)
	}
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Specs 返回全部工具描述（顺序稳定）。
func (r *Registry) Specs() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Spec, 0, len(r.order))
	for _, name := range r.order {
		if t, ok := r.tools[name]; ok {
			out = append(out, t.Spec())
		}
	}
	return out
}

// Len 返回工具数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Names 返回全部工具名（排序后，便于日志与测试断言）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Invoke 校验并执行工具。
//
// 统一处理：工具不存在、参数非法、参数未知字段、执行超时。
// 返回的 error 表示「框架级失败」，Result.IsError 表示「工具业务失败」。
func (r *Registry) Invoke(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	t, ok := r.Get(name)
	if !ok {
		return Result{
			Output:  fmt.Sprintf("工具 %q 不存在。可用工具：%s", name, strings.Join(r.Names(), ", ")),
			IsError: true,
		}, nil
	}

	// 参数必须是合法 JSON 对象（允许空对象）。
	if len(args) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(args, &probe); err != nil {
			return Result{
				Output:  fmt.Sprintf("参数不是合法的 JSON 对象：%v", err),
				IsError: true,
			}, nil
		}
	}

	res, err := t.Execute(ctx, args)
	if err != nil {
		// 工具自身的执行错误也回给模型，让它能自我纠正。
		return Result{
			Output:  fmt.Sprintf("工具 %q 执行失败：%v", name, err),
			IsError: true,
		}, nil
	}
	return res, nil
}

// ---- 参数解析辅助 ----

// DecodeArgs 把 JSON 参数解码到 dst，并对未知字段报错。
//
// 严格模式很重要：模型经常臆造参数名，静默忽略会让它反复失败却不知原因。
func DecodeArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("参数格式错误: %w", err)
	}
	return nil
}

// Truncate 按字符截断输出，并标记是否截断。
//
// 用 rune 计数避免切断中文；保留头尾以便模型理解上下文。
func Truncate(s string, maxRunes int) (string, bool) {
	if maxRunes <= 0 {
		return s, false
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s, false
	}
	// 保留头 2/3、尾 1/3：开头通常是关键信息（如文件头部），
	// 结尾常有错误堆栈或汇总，中间省略。
	headLen := maxRunes * 2 / 3
	tailLen := maxRunes - headLen
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	omitted := len(runes) - maxRunes
	return fmt.Sprintf("%s\n\n... [省略 %d 字符] ...\n\n%s", head, omitted, tail), true
}
