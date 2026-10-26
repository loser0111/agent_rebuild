package loop_test

// Loop 的端到端测试：用脚本化的假 Runner 模拟模型的工具调用决策，
// 验证完整的「模型 → 工具 → 模型」循环。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/tool"
)

// scriptedRunner 按预设脚本产生事件序列。
//
// 每次 Run 取脚本中的下一项，用来模拟模型的多轮决策。
type scriptedRunner struct {
	steps []scriptedStep
	// calls 记录每次 Run 收到的请求（用于断言上下文累积）。
	requests []agent.Request
	idx      int
}

type scriptedStep struct {
	text  string
	tools []agent.ToolCall
	err   error
}

func (r *scriptedRunner) Run(ctx context.Context, req agent.Request) <-chan agent.Event {
	out := make(chan agent.Event, 16)
	// 深拷贝请求，避免测试断言时被后续修改影响。
	cp := agent.Request{
		History:      append([]session.Message(nil), req.History...),
		Tools:        req.Tools,
		SystemPrompt: req.SystemPrompt,
	}
	r.requests = append(r.requests, cp)

	var step scriptedStep
	if r.idx < len(r.steps) {
		step = r.steps[r.idx]
	}
	r.idx++

	go func() {
		defer close(out)
		if step.err != nil {
			out <- agent.Event{Type: agent.TypeError, Err: step.err}
			return
		}
		if step.text != "" {
			// 分块发送，模拟流式。
			for _, part := range splitText(step.text, 4) {
				select {
				case <-ctx.Done():
					return
				case out <- agent.Event{Type: agent.TypeDelta, Delta: part}:
				}
			}
		}
		for i := range step.tools {
			call := step.tools[i]
			select {
			case <-ctx.Done():
				return
			case out <- agent.Event{Type: agent.TypeToolCall, ToolCall: &call}:
			}
		}
		// 有工具调用时也上报一轮用量（真实协议确实消耗了 token），
		// 但 Content 为空，表示这不是终局回答。
		if len(step.tools) == 0 {
			select {
			case <-ctx.Done():
			case out <- agent.Event{Type: agent.TypeDone, Content: step.text,
				Usage: &agent.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}:
			}
		} else {
			select {
			case <-ctx.Done():
			case out <- agent.Event{Type: agent.TypeDone,
				Usage: &agent.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}:
			}
		}
	}()
	return out
}

func (r *scriptedRunner) Name() string { return "scripted" }

func splitText(s string, n int) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var out []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// runLoop 执行 Loop 并收集所有事件。
func runLoop(t *testing.T, lp *loop.Loop, history []session.Message) (*loop.Result, []agent.Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out := make(chan agent.Event, 64)
	var events []agent.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range out {
			events = append(events, ev)
		}
	}()

	res, err := lp.Run(ctx, history, out)
	close(out)
	<-done
	return res, events, err
}

func userHistory(content string) []session.Message {
	return []session.Message{{
		ID: "u1", Role: session.RoleUser, Content: content,
		Status: session.StatusComplete,
	}}
}

// ---- 无工具路径 ----

func TestLoopNoToolsDirectAnswer(t *testing.T) {
	runner := &scriptedRunner{steps: []scriptedStep{{text: "直接回答"}}}
	lp := loop.New(runner, loop.Config{})

	res, events, err := runLoop(t, lp, userHistory("你好"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Content != "直接回答" {
		t.Fatalf("Content = %q", res.Content)
	}
	if res.Steps != 1 {
		t.Fatalf("Steps = %d，期望 1", res.Steps)
	}
	// 应发出 delta(s) + done。
	var gotDone bool
	for _, ev := range events {
		if ev.Type == agent.TypeDone {
			gotDone = true
			if ev.Content != "直接回答" {
				t.Fatalf("done.Content = %q", ev.Content)
			}
		}
	}
	if !gotDone {
		t.Fatal("应发出 done 事件")
	}
}

// ---- 单工具调用 ----

func TestLoopSingleToolCall(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("文件内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	args, _ := json.Marshal(map[string]any{"path": "hello.txt"})
	runner := &scriptedRunner{steps: []scriptedStep{
		// 第一轮：模型请求读文件。
		{tools: []agent.ToolCall{{ID: "call_1", Name: "read_file", Args: string(args)}}},
		// 第二轮：拿到结果后给出回答。
		{text: "文件内容是「文件内容」"},
	}}

	lp := loop.New(runner, loop.Config{Registry: reg})
	res, events, err := runLoop(t, lp, userHistory("看看 hello.txt"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 应执行 2 轮（工具轮 + 回答轮）。
	if res.Steps != 2 {
		t.Fatalf("Steps = %d，期望 2", res.Steps)
	}
	if res.Content != "文件内容是「文件内容」" {
		t.Fatalf("Content = %q", res.Content)
	}
	if len(res.ToolResults) != 1 {
		t.Fatalf("ToolResults = %d，期望 1", len(res.ToolResults))
	}
	if res.ToolResults[0].IsError {
		t.Fatalf("工具执行失败: %s", res.ToolResults[0].Output)
	}
	if !strings.Contains(res.ToolResults[0].Output, "文件内容") {
		t.Fatalf("工具输出不含文件内容: %q", res.ToolResults[0].Output)
	}

	// 事件序列应含 tool.call 与 tool.result。
	var sawCall, sawResult bool
	for _, ev := range events {
		switch ev.Type {
		case agent.TypeToolCall:
			sawCall = true
		case agent.TypeToolResult:
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("应发出 tool.call 与 tool.result 事件：call=%v result=%v", sawCall, sawResult)
	}

	// 第二轮请求的上下文应包含 assistant 的工具调用与 tool 结果消息。
	secondReq := runner.requests[1]
	var hasAssistantToolCall, hasToolResult bool
	for _, m := range secondReq.History {
		if m.Role == session.RoleAssistant && len(m.ToolCalls) > 0 {
			hasAssistantToolCall = true
		}
		if m.Role == session.RoleTool && m.ToolCallID == "call_1" {
			hasToolResult = true
		}
	}
	if !hasAssistantToolCall {
		t.Fatal("第二轮上下文应包含 assistant 的工具调用消息")
	}
	if !hasToolResult {
		t.Fatal("第二轮上下文应包含 tool 角色结果消息")
	}
}

// ---- 多工具调用 ----

func TestLoopMultipleToolCalls(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	argsA, _ := json.Marshal(map[string]any{"path": "a.txt"})
	argsB, _ := json.Marshal(map[string]any{"path": "b.txt"})
	runner := &scriptedRunner{steps: []scriptedStep{
		// 一轮内请求两个工具。
		{tools: []agent.ToolCall{
			{ID: "c1", Name: "read_file", Args: string(argsA)},
			{ID: "c2", Name: "read_file", Args: string(argsB)},
		}},
		{text: "都读到了"},
	}}

	lp := loop.New(runner, loop.Config{Registry: reg})
	res, _, err := runLoop(t, lp, userHistory("读两个文件"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolResults) != 2 {
		t.Fatalf("ToolResults = %d，期望 2", len(res.ToolResults))
	}
	if !strings.Contains(res.ToolResults[0].Output, "AAA") {
		t.Fatalf("第一个结果错误: %q", res.ToolResults[0].Output)
	}
	if !strings.Contains(res.ToolResults[1].Output, "BBB") {
		t.Fatalf("第二个结果错误: %q", res.ToolResults[1].Output)
	}
}

// ---- 工具失败 ----

func TestLoopToolFailureFedBackToModel(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	args, _ := json.Marshal(map[string]any{"path": "nonexistent.txt"})
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{{ID: "c1", Name: "read_file", Args: string(args)}}},
		{text: "文件不存在，我换个方式"},
	}}

	lp := loop.New(runner, loop.Config{Registry: reg})
	res, _, err := runLoop(t, lp, userHistory("读不存在的文件"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.ToolResults) != 1 || !res.ToolResults[0].IsError {
		t.Fatalf("工具失败应标记 IsError: %+v", res.ToolResults)
	}
	// 失败信息必须回喂给模型（否则模型无法自我纠正）。
	secondReq := runner.requests[1]
	var foundErrMsg bool
	for _, m := range secondReq.History {
		if m.Role == session.RoleTool && strings.Contains(m.Content, "不存在") {
			foundErrMsg = true
		}
	}
	if !foundErrMsg {
		t.Fatal("工具失败信息应回喂给模型")
	}
	// 循环仍应正常结束。
	if res.Content == "" {
		t.Fatal("应产出最终回答")
	}
}

// ---- 未知工具 ----

func TestLoopUnknownTool(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{{ID: "c1", Name: "no_such_tool", Args: "{}"}}},
		{text: "工具不存在"},
	}}

	lp := loop.New(runner, loop.Config{Registry: reg})
	res, _, err := runLoop(t, lp, userHistory("x"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolResults) != 1 || !res.ToolResults[0].IsError {
		t.Fatalf("未知工具应报错: %+v", res.ToolResults)
	}
	if !strings.Contains(res.ToolResults[0].Output, "不存在") {
		t.Fatalf("应说明工具不存在: %q", res.ToolResults[0].Output)
	}
}

// ---- 审批 ----

// denyAll 拒绝一切中高风险工具。
type denyAll struct{}

func (denyAll) Approve(_ context.Context, call agent.ToolCall) (loop.ApprovalDecision, string) {
	if call.Risk == tool.RiskSafe {
		return loop.Approved, ""
	}
	return loop.Denied, "测试拒绝"
}

// 被拒绝的工具不应执行，但模型应收到「被拒绝」的反馈。
func TestLoopToolRejectedByApprover(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	args, _ := json.Marshal(map[string]any{"path": "new.txt", "content": "x"})
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{{ID: "c1", Name: "write_file", Args: string(args)}}},
		{text: "好的，我改为说明方案"},
	}}

	lp := loop.New(runner, loop.Config{Registry: reg, Approver: denyAll{}})
	res, _, err := runLoop(t, lp, userHistory("写个文件"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.ToolResults) != 1 {
		t.Fatalf("ToolResults = %d", len(res.ToolResults))
	}
	if !res.ToolResults[0].Rejected {
		t.Fatal("应标记为被拒绝")
	}
	// 文件不应被创建。
	if _, statErr := os.Stat(filepath.Join(root, "new.txt")); statErr == nil {
		t.Fatal("被拒绝的工具不应产生副作用")
	}
	// 拒绝理由应回喂给模型。
	secondReq := runner.requests[1]
	var reasonFedBack bool
	for _, m := range secondReq.History {
		if m.Role == session.RoleTool && strings.Contains(m.Content, "未执行") {
			reasonFedBack = true
		}
	}
	if !reasonFedBack {
		t.Fatal("拒绝理由应回喂给模型（让模型换方式）")
	}
	// 循环应继续并正常结束。
	if res.Content != "好的，我改为说明方案" {
		t.Fatalf("Content = %q", res.Content)
	}
}

// 默认审批器：允许只读与写文件，拒绝命令执行。
func TestAutoApproverDefaults(t *testing.T) {
	a := loop.AutoApprover{AllowModerate: true, AllowDangerous: false}
	ctx := context.Background()

	if decision, _ := a.Approve(ctx, agent.ToolCall{Risk: tool.RiskSafe}); decision != loop.Approved {
		t.Fatal("只读工具应放行")
	}
	if decision, _ := a.Approve(ctx, agent.ToolCall{Risk: tool.RiskModerate}); decision != loop.Approved {
		t.Fatal("写文件应放行（AllowModerate=true）")
	}
	if decision, reason := a.Approve(ctx, agent.ToolCall{Risk: tool.RiskDangerous}); decision == loop.Approved {
		t.Fatal("命令执行应被拒绝")
	} else if reason == "" {
		t.Fatal("拒绝应带理由")
	}

	// 未开启 AllowModerate 时应拒绝写文件。
	strict := loop.AutoApprover{}
	if decision, _ := strict.Approve(ctx, agent.ToolCall{Risk: tool.RiskModerate}); decision == loop.Approved {
		t.Fatal("严格模式下写文件应被拒绝")
	}
	// AutoApprover 永远能当场决定，不应返回 AwaitingHuman。
	if decision, _ := strict.Approve(ctx, agent.ToolCall{Risk: tool.RiskDangerous}); decision == loop.AwaitingHuman {
		t.Fatal("AutoApprover 不应返回 AwaitingHuman")
	}
}

// ManualApprover 对任何需要审批的调用都应返回 AwaitingHuman，Safe 工具不在
// 其职责范围内（evaluateApprovals 不会为 Safe 工具调用 Approve，见下方
// TestLoopManualApprovalSuspendsWriteOperation 的分工说明）。
func TestManualApproverAlwaysAwaits(t *testing.T) {
	a := loop.ManualApprover{}
	ctx := context.Background()

	for _, risk := range []tool.Risk{tool.RiskModerate, tool.RiskDangerous} {
		decision, reason := a.Approve(ctx, agent.ToolCall{Risk: risk})
		if decision != loop.AwaitingHuman {
			t.Fatalf("risk=%s: decision = %v，期望 AwaitingHuman", risk, decision)
		}
		if reason == "" {
			t.Fatalf("risk=%s: 应带理由说明等待人工", risk)
		}
	}
}

// 人工审批模式下，写操作应挂起等待人工决定，而不是直接执行或拒绝。
//
// 关键断言：
//  1. 文件确实没有被创建（挂起 ≠ 已执行、也 ≠ 已拒绝，是"还没决定"）；
//  2. checkpoint 里的 Pending.Kind 是 PendingApproval（不是 PendingInput）；
//  3. Risk/Reason 被正确带出，供前端渲染审批卡片。
func TestLoopManualApprovalSuspendsWriteOperation(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})
	cp := &fakeCheckpointer{}

	args, _ := json.Marshal(map[string]any{"path": "new.txt", "content": "x"})
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{{ID: "c1", Name: "write_file", Args: string(args)}}},
	}}

	lp := loop.New(runner, loop.Config{
		Registry: reg, Approver: loop.ManualApprover{},
		SessionID: "sess_manual", Checkpointer: cp,
	})
	res, _, err := runLoop(t, lp, userHistory("写个文件"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 挂起时不应产出工具结果或最终回答。
	if len(res.ToolResults) != 0 {
		t.Fatalf("挂起时不应有工具结果: %+v", res.ToolResults)
	}
	if res.Content != "" {
		t.Fatalf("挂起时不应有最终回答: %q", res.Content)
	}

	// 文件不应被创建：这是挂起而不是"静默放行"的关键证据。
	if _, statErr := os.Stat(filepath.Join(root, "new.txt")); statErr == nil {
		t.Fatal("挂起中的写操作不应产生副作用")
	}

	saved := cp.suspendedOf()
	if saved == nil {
		t.Fatal("应保存一条 suspended checkpoint")
	}
	if len(saved.Pendings) != 1 {
		t.Fatalf("Pendings = %d，期望 1", len(saved.Pendings))
	}
	p := saved.Pendings[0]
	if p.Kind != session.PendingApproval {
		t.Fatalf("Kind = %q，期望 %q", p.Kind, session.PendingApproval)
	}
	if p.CallID != "c1" || p.Name != "write_file" {
		t.Fatalf("CallID/Name 未正确带出: %+v", p)
	}
	if p.Risk != tool.RiskModerate.String() {
		t.Fatalf("Risk = %q，期望 %q", p.Risk, tool.RiskModerate.String())
	}
	if p.Reason == "" {
		t.Fatal("Reason 应带出 ManualApprover 给出的说明")
	}
}

// 同一轮里既有 ask_user 又有待审批写操作时，必须合并进同一条 checkpoint，
// 否则会出现"只挂起了一半"的半成品状态，另一半调用静默消失。
func TestLoopManualApprovalMergesWithAskUser(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})
	cp := &fakeCheckpointer{}

	writeArgs, _ := json.Marshal(map[string]any{"path": "new.txt", "content": "x"})
	askArgs, _ := json.Marshal(map[string]any{"question": "这个值应该填多少？"})
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{
			{ID: "c1", Name: "write_file", Args: string(writeArgs)},
			{ID: "c2", Name: "ask_user", Args: string(askArgs)},
		}},
	}}

	lp := loop.New(runner, loop.Config{
		Registry: reg, Approver: loop.ManualApprover{},
		SessionID: "sess_mixed", Checkpointer: cp,
	})
	if _, _, err := runLoop(t, lp, userHistory("你帮我处理一下")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	saved := cp.suspendedOf()
	if saved == nil {
		t.Fatal("应保存一条 suspended checkpoint")
	}
	if len(saved.Pendings) != 2 {
		t.Fatalf("Pendings = %d，期望 2（一个问题 + 一个待批操作）", len(saved.Pendings))
	}
	var gotApproval, gotInput bool
	for _, p := range saved.Pendings {
		switch p.Kind {
		case session.PendingApproval:
			gotApproval = true
		case session.PendingInput:
			gotInput = true
		}
	}
	if !gotApproval || !gotInput {
		t.Fatalf("两类挂起应同时存在: %+v", saved.Pendings)
	}
}

// ---- ExecuteApproved：人工批准后才真正执行工具 ----

// 批准后必须真正落盘，不能只是把"已批准"文字当结果喂给模型。
func TestExecuteApprovedRunsTool(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})
	args, _ := json.Marshal(map[string]any{"path": "approved.txt", "content": "hello"})

	res := loop.ExecuteApproved(context.Background(), reg, 5*time.Second, "c1", "write_file", string(args))
	if res.IsError {
		t.Fatalf("执行应成功: %s", res.Output)
	}
	data, err := os.ReadFile(filepath.Join(root, "approved.txt"))
	if err != nil {
		t.Fatalf("文件应已写入: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("内容 = %q", string(data))
	}
}

// 工具不存在时应给出明确错误，而不是 panic 或空结果。
func TestExecuteApprovedUnknownTool(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	res := loop.ExecuteApproved(context.Background(), reg, 5*time.Second, "c1", "no_such_tool", "{}")
	if !res.IsError {
		t.Fatal("未知工具应报错")
	}
	if !strings.Contains(res.Output, "不存在") {
		t.Fatalf("应说明工具不存在: %q", res.Output)
	}
}

// registry 为 nil（工具系统未启用）时应安全返回错误，而不是 panic。
func TestExecuteApprovedNilRegistry(t *testing.T) {
	res := loop.ExecuteApproved(context.Background(), nil, 5*time.Second, "c1", "write_file", "{}")
	if !res.IsError {
		t.Fatal("registry 为 nil 时应报错")
	}
}

// ---- 步数上限 ----

// 模型无限请求工具时，应在上限处停止，不无限循环。
func TestLoopMaxSteps(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	args, _ := json.Marshal(map[string]any{"path": "."})
	steps := make([]scriptedStep, 20)
	for i := range steps {
		steps[i] = scriptedStep{tools: []agent.ToolCall{
			{ID: "c" + string(rune('0'+i%10)), Name: "list_dir", Args: string(args)},
		}}
	}
	runner := &scriptedRunner{steps: steps}

	lp := loop.New(runner, loop.Config{Registry: reg, MaxSteps: 3})
	res, _, err := runLoop(t, lp, userHistory("循环调用"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps > 3 {
		t.Fatalf("Steps = %d，不应超过 MaxSteps=3", res.Steps)
	}
	// 达到上限也应有收尾（避免用户拿不到任何输出）。
	if res.Content == "" {
		t.Fatal("达到上限时应给出收尾说明")
	}
	if !strings.Contains(res.Content, "最大") {
		t.Fatalf("应提示达到最大轮数: %q", res.Content)
	}
}

// ---- 错误传播 ----

func TestLoopRunnerError(t *testing.T) {
	runner := &scriptedRunner{steps: []scriptedStep{{err: errors.New("模型不可用")}}}
	lp := loop.New(runner, loop.Config{})

	_, _, err := runLoop(t, lp, userHistory("x"))
	if err == nil {
		t.Fatal("Runner 报错应向上传播")
	}
	if !strings.Contains(err.Error(), "模型不可用") {
		t.Fatalf("err = %v", err)
	}
}

// ---- 无注册表 ----

func TestLoopWithoutRegistry(t *testing.T) {
	runner := &scriptedRunner{steps: []scriptedStep{{text: "纯文本回答"}}}
	lp := loop.New(runner, loop.Config{}) // Registry 为 nil

	res, _, err := runLoop(t, lp, userHistory("x"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Content != "纯文本回答" {
		t.Fatalf("Content = %q", res.Content)
	}
	// 无工具时不应给 Runner 传工具列表。
	if len(runner.requests[0].Tools) != 0 {
		t.Fatalf("无注册表时不应传工具: %d", len(runner.requests[0].Tools))
	}
}

// 有注册表时，Runner 应收到工具列表。
func TestLoopPassesToolSpecs(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})
	runner := &scriptedRunner{steps: []scriptedStep{{text: "ok"}}}

	lp := loop.New(runner, loop.Config{Registry: reg})
	if _, _, err := runLoop(t, lp, userHistory("x")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	specs := runner.requests[0].Tools
	if len(specs) != reg.Len() {
		t.Fatalf("工具数 = %d，期望 %d", len(specs), reg.Len())
	}
	// 每个工具都应有名字与描述（模型据此决定何时调用）。
	for _, sp := range specs {
		if sp.Name == "" || sp.Description == "" {
			t.Fatalf("工具描述不完整: %+v", sp)
		}
	}
}

// 用量应跨轮累计。
func TestLoopUsageAccumulated(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})
	args, _ := json.Marshal(map[string]any{"path": "."})

	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{{ID: "c1", Name: "list_dir", Args: string(args)}}},
		{text: "完成"},
	}}
	lp := loop.New(runner, loop.Config{Registry: reg})

	res, _, err := runLoop(t, lp, userHistory("x"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 两轮各自的 usage 应累加（脚本每轮给 10/5/15）。
	if res.Usage == nil {
		t.Fatal("应有累计用量")
	}
	if res.Usage.TotalTokens != 30 {
		t.Fatalf("TotalTokens = %d，期望 30（两轮各 15）", res.Usage.TotalTokens)
	}
}
