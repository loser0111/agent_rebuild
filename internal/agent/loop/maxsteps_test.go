package loop

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/tool"
)

// multiStepRunner 每轮都请求调用工具，直到达到上限——模拟"模型反复探索"。
type multiStepRunner struct {
	calls int
}

func (r *multiStepRunner) Run(ctx context.Context, req agent.Request) <-chan agent.Event {
	out := make(chan agent.Event, 4)
	go func() {
		defer close(out)
		r.calls++
		// 每轮都给一句话 + 一个工具调用
		out <- agent.Event{Type: agent.TypeDelta, Delta: "第" + itoa(r.calls) + "轮分析..."}
		out <- agent.Event{Type: agent.TypeToolCall, ToolCall: &agent.ToolCall{
			ID: "call_" + itoa(r.calls), Name: "list_dir", Args: `{"path":"."}`,
		}}
	}()
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestMaxStepsKeepsModelText 达到上限时必须保留模型已产出的文本。
//
// 线上问题：直接丢弃 text，只回一句「已达到最大工具调用轮数」，
// 用户花掉 12 轮 token 却什么结论都拿不到。
func TestMaxStepsKeepsModelText(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	r := &multiStepRunner{}
	lp := New(r, Config{
		Registry:    reg,
		MaxSteps:    3,
		ToolTimeout: 2 * time.Second,
	})

	out := make(chan agent.Event, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := lp.Run(ctx, []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "帮我看看代码", Status: session.StatusComplete},
	}, out)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	if res.Steps != 3 {
		t.Fatalf("应跑满 3 轮，实际 %d", res.Steps)
	}

	// 关键断言：内容里必须有模型产出的分析文本
	if !strings.Contains(res.Content, "轮分析") {
		t.Fatalf("达到上限时丢失了模型文本，实际内容: %q", res.Content)
	}
	// 同时保留提示语
	if !strings.Contains(res.Content, "已达到最大工具调用轮数") {
		t.Fatalf("缺少上限提示，实际内容: %q", res.Content)
	}
}

// TestDoneEventCarriesPreservedText done 事件应带上保留下来的内容。
func TestDoneEventCarriesPreservedText(t *testing.T) {
	root := t.TempDir()
	reg := tool.DefaultRegistry(tool.Options{Workspace: root})

	lp := New(&multiStepRunner{}, Config{
		Registry: reg, MaxSteps: 2, ToolTimeout: 2 * time.Second,
	})

	out := make(chan agent.Event, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := lp.Run(ctx, []session.Message{
		{ID: "1", Role: session.RoleUser, Content: "hi", Status: session.StatusComplete},
	}, out); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	close(out)

	var doneContent string
	for ev := range out {
		if ev.Type == agent.TypeDone {
			doneContent = ev.Content
		}
	}
	if !strings.Contains(doneContent, "轮分析") {
		t.Fatalf("done 事件丢失模型文本: %q", doneContent)
	}
}
