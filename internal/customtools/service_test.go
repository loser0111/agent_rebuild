package customtools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent_rebuild/internal/storage/sqlite"
	"agent_rebuild/internal/tool"
	"agent_rebuild/internal/tool/mcp"
	"agent_rebuild/internal/tool/router"
)

type fakeSource struct {
	mu  sync.Mutex
	cat []mcp.CatalogServer
}

func (f *fakeSource) Catalog() []mcp.CatalogServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cat
}

func newSQLStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ct.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLStore(db.SQL())
}

type harness struct {
	svc     *Service
	src     *fakeSource
	applied *tool.Registry
}

func newHarness(t *testing.T, store Store) *harness {
	t.Helper()
	builtin := tool.NewRegistry()
	h := &harness{src: &fakeSource{cat: []mcp.CatalogServer{{
		Name: "gh", Description: "GitHub", Available: true,
		Tools: []mcp.CatalogTool{
			{Server: "gh", Name: "search", Description: "搜索", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				BaseRisk: tool.RiskSafe, BaseEnabled: true,
				Call: func(context.Context, json.RawMessage) (tool.Result, error) {
					return tool.Result{Output: "searched"}, nil
				}},
			{Server: "gh", Name: "delete", Description: "删除", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				BaseRisk: tool.RiskDangerous, BaseEnabled: false,
				Call: func(context.Context, json.RawMessage) (tool.Result, error) {
					return tool.Result{Output: "deleted"}, nil
				}},
		},
	}}}}
	h.svc = New(Options{Builtin: builtin, Source: h.src, Store: store, Apply: func(r *tool.Registry) { h.applied = r }})
	h.svc.Rebuild()
	return h
}

func (h *harness) resolve(t *testing.T, toolName string) (string, tool.Risk) {
	t.Helper()
	tl, ok := h.applied.Get(router.Name)
	if !ok {
		t.Fatal("未注册 search_router")
	}
	return tl.(tool.CallResolver).ResolveCall(json.RawMessage(`{"action":"execute","tool":"` + toolName + `"}`))
}

func boolPtr(b bool) *bool { return &b }

func TestRebuildRegistersRouterWithEffectiveValues(t *testing.T) {
	h := newHarness(t, newSQLStore(t))
	if _, risk := h.resolve(t, "gh/search"); risk != tool.RiskSafe {
		t.Fatalf("基础值应生效：%v", risk)
	}
	// mcp.json 关掉的 delete：不会执行，按 safe 定级。
	if _, risk := h.resolve(t, "gh/delete"); risk != tool.RiskSafe {
		t.Fatalf("禁用工具应为 safe：%v", risk)
	}
}

// 用户设置优先于基础值；恢复默认后又跟随基础值；设置持久化后重新载入仍生效。
func TestOverridePrecedenceAndReset(t *testing.T) {
	store := newSQLStore(t)
	h := newHarness(t, store)
	ctx := context.Background()

	// 打开 mcp.json 关掉的工具，并把只读工具调成高危。
	if err := h.svc.SetOverride(ctx, Input{Tool: "gh/delete", Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SetOverride(ctx, Input{Tool: "gh/search", Risk: "dangerous"}); err != nil {
		t.Fatal(err)
	}
	if target, risk := h.resolve(t, "gh/delete"); target != "gh/delete" || risk != tool.RiskDangerous {
		t.Fatalf("打开后应按基础风险 dangerous 定级：%q %v", target, risk)
	}
	if _, risk := h.resolve(t, "gh/search"); risk != tool.RiskDangerous {
		t.Fatalf("风险覆盖未生效：%v", risk)
	}

	views := h.svc.ToolViews()["gh"]
	if len(views) != 2 || views[0].OverrideRisk != "dangerous" || views[0].BaseRisk != "safe" || views[0].Risk != "dangerous" {
		t.Fatalf("views = %+v", views)
	}
	if views[1].OverrideEnabled == nil || !*views[1].OverrideEnabled || views[1].BaseEnabled || !views[1].Enabled {
		t.Fatalf("views[1] = %+v", views[1])
	}

	// 重新载入（模拟重启）：设置仍在。
	h2 := &harness{src: h.src}
	h2.svc = New(Options{Builtin: tool.NewRegistry(), Source: h.src, Store: store, Apply: func(r *tool.Registry) { h2.applied = r }})
	h2.svc.Rebuild()
	if _, risk := h2.resolve(t, "gh/search"); risk != tool.RiskDangerous {
		t.Fatalf("重启后设置丢失：%v", risk)
	}

	// 恢复默认：两个字段都跟随 → 删除覆盖。
	if err := h.svc.SetOverride(ctx, Input{Tool: "gh/search"}); err != nil {
		t.Fatal(err)
	}
	if _, risk := h.resolve(t, "gh/search"); risk != tool.RiskSafe {
		t.Fatalf("恢复默认后应回到基础值：%v", risk)
	}
	if m, _ := store.List(ctx); len(m) != 1 {
		t.Fatalf("恢复默认应删除该行，剩余 %v", m)
	}
}

func TestSetOverrideValidationAndUnsupported(t *testing.T) {
	h := newHarness(t, newSQLStore(t))
	ctx := context.Background()
	for _, in := range []Input{{Tool: "noslash"}, {Tool: "/x"}, {Tool: "gh/search", Risk: "low"}} {
		if err := h.svc.SetOverride(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v 应返回 ErrInvalid，实际 %v", in, err)
		}
	}
	// 覆盖可以指向当前不存在的工具（server 还没连上）。
	if err := h.svc.SetOverride(ctx, Input{Tool: "later/tool", Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("应允许覆盖未知工具：%v", err)
	}

	mem := newHarness(t, nil)
	if mem.svc.OverridesSupported() {
		t.Fatal("没有 Store 时应不支持保存")
	}
	if err := mem.svc.SetOverride(ctx, Input{Tool: "gh/search", Risk: "safe"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("应返回 ErrUnsupported，实际 %v", err)
	}
}

// 没有配置任何外部 server 时不注册 search_router；server 都不可用时照样注册并在描述里说明。
func TestRouterRegistrationRules(t *testing.T) {
	h := newHarness(t, nil)
	h.src.mu.Lock()
	h.src.cat = nil
	h.src.mu.Unlock()
	h.svc.Rebuild()
	if _, ok := h.applied.Get(router.Name); ok {
		t.Fatal("没有外部 server 时不应注册 search_router")
	}

	h.src.mu.Lock()
	h.src.cat = []mcp.CatalogServer{{Name: "pg", Available: false, Reason: "启动超时"}}
	h.src.mu.Unlock()
	h.svc.Rebuild()
	tl, ok := h.applied.Get(router.Name)
	if !ok {
		t.Fatal("server 不可用时也应注册 search_router")
	}
	if d := tl.Spec().Description; !strings.Contains(d, "pg：暂不可用（启动超时）") {
		t.Fatalf("描述应说明不可用：\n%s", d)
	}
}
