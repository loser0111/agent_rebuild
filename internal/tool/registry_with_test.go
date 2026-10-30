package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// stubTool 是只有名字的最小工具，用于测试 Registry.With 的合并逻辑。
type stubTool struct{ name string }

func (s stubTool) Spec() Spec {
	return Spec{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`), Risk: RiskDangerous}
}

func (s stubTool) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{Output: s.name}, nil
}

// With 应保持内置工具在前、外部工具在后，且不修改原注册表。
func TestRegistryWithKeepsOrderAndDoesNotMutate(t *testing.T) {
	base := NewRegistry()
	base.SetWorkspaceRoot("/ws")
	base.Register(stubTool{"a"}, stubTool{"b"})

	merged := base.With(stubTool{"mcp__x__c"}, stubTool{"mcp__x__d"})

	specs := merged.Specs()
	got := make([]string, 0, len(specs))
	for _, s := range specs {
		got = append(got, s.Name)
	}
	want := []string{"a", "b", "mcp__x__c", "mcp__x__d"}
	if len(got) != len(want) {
		t.Fatalf("合并后工具 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("合并后顺序 = %v，期望 %v", got, want)
		}
	}
	if base.Len() != 2 {
		t.Fatalf("原注册表被修改：%v", base.Names())
	}
	if merged.WorkspaceRoot() != "/ws" {
		t.Fatalf("WorkspaceRoot 未继承：%q", merged.WorkspaceRoot())
	}
}

// 外部工具与内置工具重名、或彼此重名、或空名时，应跳过而不是 panic。
func TestRegistryWithSkipsDuplicatesWithoutPanic(t *testing.T) {
	base := NewRegistry()
	base.Register(stubTool{"read_file"})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("With 不应 panic：%v", r)
		}
	}()
	merged := base.With(stubTool{"read_file"}, stubTool{"x"}, stubTool{"x"}, stubTool{""})

	if merged.Len() != 2 {
		t.Fatalf("工具数 = %d，期望 2：%v", merged.Len(), merged.Names())
	}
	// 重名时保留的是原有的内置工具。
	res, _ := merged.Invoke(context.Background(), "read_file", nil)
	if res.Output != "read_file" {
		t.Fatalf("重名时应保留内置工具，实际输出 %q", res.Output)
	}
}
