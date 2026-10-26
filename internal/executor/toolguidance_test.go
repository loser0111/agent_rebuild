package executor

import (
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/tool"
)

// TestToolGuidanceInjected 工具使用准则必须注入系统提示。
//
// 为什么重要：模型拿到工具却没有行为约束时，会反复 list_dir/read_file
// 试探，几轮就把步数耗尽，最后给不出结论。
func TestToolGuidanceInjected(t *testing.T) {
	e := New(func() agent.Runner { return nil }, nil, nil)
	reg := tool.DefaultRegistry(tool.Options{Workspace: t.TempDir()})

	e.SetTools(reg, "你是助手", 12, 10*time.Second, loop.AutoApprover{})

	p := e.systemPrompt
	if !strings.Contains(p, "工具使用准则") {
		t.Fatalf("系统提示缺少工具准则: %q", p)
	}
	if !strings.Contains(p, "你是助手") {
		t.Fatalf("用户自定义系统提示被覆盖: %q", p)
	}
	// 关键约束条目
	for _, want := range []string{"不要重复读取同一个文件", "先规划再动手"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺少约束: %q", want)
		}
	}
}

// TestToolGuidanceWorksWithoutUserPrompt 用户没配系统提示时也应生效。
func TestToolGuidanceWorksWithoutUserPrompt(t *testing.T) {
	e := New(func() agent.Runner { return nil }, nil, nil)
	reg := tool.DefaultRegistry(tool.Options{Workspace: t.TempDir()})

	e.SetTools(reg, "", 12, 10*time.Second, loop.AutoApprover{})

	if !strings.Contains(e.systemPrompt, "工具使用准则") {
		t.Fatalf("空系统提示时准则未注入: %q", e.systemPrompt)
	}
}
