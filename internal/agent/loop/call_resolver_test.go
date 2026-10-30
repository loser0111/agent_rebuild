package loop_test

import (
	"context"
	"encoding/json"
	"testing"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/tool"
)

// proxyTool 模拟代理工具 search_router：自身 Spec().Risk 为 safe，
// 但 execute 的风险取决于参数里的目标（见 docs/17-mcp-client.md 12.5）。
type proxyTool struct{ executed []string }

func (p *proxyTool) Spec() tool.Spec {
	return tool.Spec{Name: "search_router", Parameters: json.RawMessage(`{"type":"object"}`), Risk: tool.RiskSafe}
}

func (p *proxyTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct{ Action, Tool string }
	_ = json.Unmarshal(args, &a)
	p.executed = append(p.executed, a.Action+":"+a.Tool)
	return tool.Result{Output: "ok"}, nil
}

func (p *proxyTool) ResolveCall(args json.RawMessage) (string, tool.Risk) {
	var a struct{ Action, Tool string }
	_ = json.Unmarshal(args, &a)
	if a.Action != "execute" {
		return "", tool.RiskSafe
	}
	return a.Tool, tool.RiskDangerous
}

// 代理工具按参数定级：list 直接执行；execute 一个高危目标必须经过审批（这里被拒绝），
// 且事件里带上真实目标供审批展示。
func TestLoopRiskResolvedFromArgs(t *testing.T) {
	proxy := &proxyTool{}
	reg := tool.NewRegistry()
	reg.Register(proxy)

	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{
			{ID: "c1", Name: "search_router", Args: `{"action":"list"}`},
			{ID: "c2", Name: "search_router", Args: `{"action":"execute","tool":"github/delete_repo"}`},
		}},
		{text: "完成"},
	}}
	lp := loop.New(runner, loop.Config{Registry: reg, Approver: denyAll{}})
	_, events, err := runLoop(t, lp, userHistory("清理仓库"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := map[string]agent.ToolCall{}
	for _, ev := range events {
		if ev.Type == agent.TypeToolCall && ev.ToolCall != nil {
			calls[ev.ToolCall.ID] = *ev.ToolCall
		}
	}
	if c := calls["c1"]; c.Risk != tool.RiskSafe || c.RequiresApproval || c.Target != "" {
		t.Fatalf("list 应为 safe 且无目标：%+v", c)
	}
	if c := calls["c2"]; c.Risk != tool.RiskDangerous || !c.RequiresApproval || c.Target != "github/delete_repo" {
		t.Fatalf("execute 应按目标定级为 dangerous 并带目标：%+v", c)
	}
	// 被拒绝的 execute 不应执行，list 应执行。
	if len(proxy.executed) != 1 || proxy.executed[0] != "list:" {
		t.Fatalf("实际执行 = %v，期望只有 list", proxy.executed)
	}
}
