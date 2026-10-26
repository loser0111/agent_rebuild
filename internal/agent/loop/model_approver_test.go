package loop_test

// ModelApprover 的裁决解析、FailClosed/FailOpen、超时行为测试。

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/tool"
)

func TestModelApproverParsesVerdictApprove(t *testing.T) {
	judge := &scriptedRunner{steps: []scriptedStep{
		{text: `这是我的裁决：{"allowed": true, "reason": "只是读文件，安全"}`},
	}}
	a := loop.ModelApprover{Judge: judge, FailClosed: true}

	decision, reason := a.Approve(context.Background(), agent.ToolCall{
		Name: "write_file", Risk: tool.RiskModerate, Args: `{"path":"a.txt"}`,
	})
	if decision != loop.Approved {
		t.Fatalf("decision = %v，期望 Approved", decision)
	}
	if reason != "只是读文件，安全" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestModelApproverParsesVerdictDeny(t *testing.T) {
	judge := &scriptedRunner{steps: []scriptedStep{
		{text: `{"allowed": false, "reason": "路径越界"}`},
	}}
	a := loop.ModelApprover{Judge: judge, FailClosed: true}

	decision, reason := a.Approve(context.Background(), agent.ToolCall{
		Name: "write_file", Risk: tool.RiskModerate, Args: `{"path":"../etc/passwd"}`,
	})
	if decision != loop.Denied {
		t.Fatalf("decision = %v，期望 Denied", decision)
	}
	if reason != "路径越界" {
		t.Fatalf("reason = %q", reason)
	}
}

// Judge 调用本身失败（协议错误/网络错误）时，FailClosed=true 应拒绝。
func TestModelApproverFailClosedOnJudgeError(t *testing.T) {
	judge := &scriptedRunner{steps: []scriptedStep{
		{err: errors.New("网络错误")},
	}}
	a := loop.ModelApprover{Judge: judge, FailClosed: true}

	decision, reason := a.Approve(context.Background(), agent.ToolCall{
		Name: "run_command", Risk: tool.RiskDangerous, Args: "{}",
	})
	if decision != loop.Denied {
		t.Fatalf("decision = %v，期望 Denied（FailClosed）", decision)
	}
	if reason == "" {
		t.Fatal("应带失败原因")
	}
}

// Judge 返回的内容无法解析成 JSON 时，同样按 FailClosed 处理。
func TestModelApproverFailClosedOnUnparsableOutput(t *testing.T) {
	judge := &scriptedRunner{steps: []scriptedStep{
		{text: "我拒绝回答这个问题"},
	}}
	a := loop.ModelApprover{Judge: judge, FailClosed: true}

	decision, _ := a.Approve(context.Background(), agent.ToolCall{
		Name: "write_file", Risk: tool.RiskModerate, Args: "{}",
	})
	if decision != loop.Denied {
		t.Fatalf("decision = %v，期望 Denied（解析失败且 FailClosed）", decision)
	}
}

// FailClosed=false 时，失败场景应放行（显式配置的"宽松"行为）。
func TestModelApproverFailOpenWhenConfigured(t *testing.T) {
	judge := &scriptedRunner{steps: []scriptedStep{
		{err: errors.New("网络错误")},
	}}
	a := loop.ModelApprover{Judge: judge, FailClosed: false}

	decision, _ := a.Approve(context.Background(), agent.ToolCall{
		Name: "write_file", Risk: tool.RiskModerate, Args: "{}",
	})
	if decision != loop.Approved {
		t.Fatalf("decision = %v，期望 Approved（FailClosed=false）", decision)
	}
}

// Judge 为 nil（配置缺失）时不应 panic，应按 FailClosed 处理。
func TestModelApproverNilJudge(t *testing.T) {
	a := loop.ModelApprover{Judge: nil, FailClosed: true}
	decision, reason := a.Approve(context.Background(), agent.ToolCall{
		Name: "write_file", Risk: tool.RiskModerate, Args: "{}",
	})
	if decision != loop.Denied {
		t.Fatalf("decision = %v，期望 Denied", decision)
	}
	if reason == "" {
		t.Fatal("应说明未配置 Judge")
	}
}

// blockingRunner 永远不发任何事件，直到 ctx 被取消，用于模拟 judge 模型超时。
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ agent.Request) <-chan agent.Event {
	out := make(chan agent.Event)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out
}

func (blockingRunner) Name() string { return "blocking" }

// Judge 超时且 FailClosed=true 时应拒绝，且 Approve 本身不应超过配置的超时太多。
func TestModelApproverFailClosedOnTimeout(t *testing.T) {
	a := loop.ModelApprover{Judge: blockingRunner{}, Timeout: 50 * time.Millisecond, FailClosed: true}

	start := time.Now()
	decision, reason := a.Approve(context.Background(), agent.ToolCall{
		Name: "run_command", Risk: tool.RiskDangerous, Args: "{}",
	})
	elapsed := time.Since(start)

	if decision != loop.Denied {
		t.Fatalf("decision = %v，期望 Denied（超时后 FailClosed）", decision)
	}
	if reason == "" {
		t.Fatal("应带失败原因")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Approve 耗时 %v，应在超时配置附近返回，而不是一直阻塞", elapsed)
	}
}
