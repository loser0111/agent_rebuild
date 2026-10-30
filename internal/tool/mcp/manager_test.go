package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_rebuild/internal/tool"
)

// testManager 记录每次 OnChange 时目录里可用工具的全名，便于断言。
type testManager struct {
	*Manager
	mu      sync.Mutex
	changes [][]string
}

func newTestManager(t *testing.T, servers ...ServerConfig) *testManager {
	t.Helper()
	tm := &testManager{}
	tm.Manager = NewManager(Options{
		Servers:     servers,
		Workspace:   t.TempDir(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BackoffBase: 20 * time.Millisecond,
		BackoffMax:  50 * time.Millisecond,
		MaxFailures: 3,
		OnChange: func() {
			// 回调里取 Catalog()：这正是 customtools.Rebuild 的用法。
			var names []string
			for _, cs := range tm.Catalog() {
				for _, ct := range cs.Tools {
					names = append(names, ct.FullName())
				}
			}
			tm.mu.Lock()
			tm.changes = append(tm.changes, names)
			tm.mu.Unlock()
		},
	})
	tm.Start(context.Background())
	t.Cleanup(tm.Close)
	return tm
}

// waitFor 轮询直到 cond 为真，超时则失败并打印当前状态。
func (tm *testManager) waitFor(t *testing.T, what string, cond func([]ServerStatus) bool) []ServerStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := tm.Status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(st, "", "  ")
			t.Fatalf("等待超时：%s\n当前状态：%s", what, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func statusIs(want Status) func([]ServerStatus) bool {
	return func(st []ServerStatus) bool { return len(st) > 0 && st[0].Status == want }
}

func (tm *testManager) lastChange() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.changes) == 0 {
		return nil
	}
	return tm.changes[len(tm.changes)-1]
}

// findTool 在当前目录里按全名找工具。
func findTool(m *Manager, full string) (CatalogTool, bool) {
	for _, cs := range m.Catalog() {
		for _, ct := range cs.Tools {
			if ct.FullName() == full {
				return ct, true
			}
		}
	}
	return CatalogTool{}, false
}

func callTool(t *testing.T, m *Manager, full string, args string) (tool.Result, error) {
	t.Helper()
	ct, ok := findTool(m, full)
	if !ok {
		t.Fatalf("找不到工具 %s", full)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ct.Call(ctx, json.RawMessage(args))
}

func TestManagerConnectsAndExposesTools(t *testing.T) {
	want := []string{"fx/echo", "fx/fail", "fx/ping_client",
		"fx/slow", "fx/change", "fx/exit", "fx/last_cancelled"}
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	// ready 与 OnChange 之间有一个短窗口（先 setStatus 再 publish），两个条件一起等。
	st := tm.waitFor(t, "ready 且已通知", func(st []ServerStatus) bool {
		return statusIs(StatusReady)(st) && slices.Equal(tm.lastChange(), want)
	})[0]

	if st.ServerName != "fixture" || st.ServerVersion != "9.9.9" {
		t.Fatalf("serverInfo 未记录：%+v", st)
	}
	if st.ProtocolVersion != "2025-03-26" {
		t.Fatalf("应接受 server 协商的协议版本，实际 %q", st.ProtocolVersion)
	}
	if st.ToolCount != len(want) {
		t.Fatalf("tool_count = %d，期望 %d", st.ToolCount, len(want))
	}

	cat := tm.Catalog()
	if len(cat) != 1 || !cat[0].Available {
		t.Fatalf("目录 = %+v", cat)
	}
	// 没写 description 时用 instructions 的第一句。
	if cat[0].Description != "测试用 fixture 服务" {
		t.Fatalf("server 说明 = %q", cat[0].Description)
	}
	// 两页工具都应拿到（验证翻页）。
	got := make([]string, 0, len(cat[0].Tools))
	for _, ct := range cat[0].Tools {
		got = append(got, ct.FullName())
		// 未开 trust_annotations：即使 server 自报只读，基础风险也必须是 dangerous。
		if ct.BaseRisk != tool.RiskDangerous || !ct.BaseEnabled {
			t.Fatalf("%s 基础值 = %v/%v，期望 dangerous/启用", ct.FullName(), ct.BaseRisk, ct.BaseEnabled)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("工具 = %v，期望 %v", got, want)
	}
}

func TestManagerTrustAnnotations(t *testing.T) {
	sc := fixtureServerConfig(t, "fx", "normal")
	sc.TrustAnnotations = true
	sc.Description = "自定义说明"
	sc.EnabledTools = []string{"echo", "slow", "fail"}
	tm := newTestManager(t, sc)
	tm.waitFor(t, "ready", statusIs(StatusReady))

	cat := tm.Catalog()[0]
	if cat.Description != "自定义说明" {
		t.Fatalf("mcp.json 的 description 应优先于 instructions：%q", cat.Description)
	}
	risks, enabled := map[string]tool.Risk{}, 0
	for _, ct := range cat.Tools {
		risks[ct.Name] = ct.BaseRisk
		if ct.BaseEnabled {
			enabled++
		}
	}
	// 白名单外的工具仍在目录里（BaseEnabled=false），面板才能重新打开它们。
	if len(risks) != 7 || enabled != 3 {
		t.Fatalf("工具 %d 个、启用 %d 个，期望 7 / 3", len(risks), enabled)
	}
	want := map[string]tool.Risk{"echo": tool.RiskSafe, "slow": tool.RiskModerate, "fail": tool.RiskDangerous}
	for k, v := range want {
		if risks[k] != v {
			t.Fatalf("%s 风险 = %v，期望 %v", k, risks[k], v)
		}
	}
}

func TestManagerCallTool(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))

	res, err := callTool(t, tm.Manager, "fx/echo", `{"text":"你好"}`)
	if err != nil || res.IsError || res.Output != "你好" {
		t.Fatalf("echo: res=%+v err=%v", res, err)
	}

	res, err = callTool(t, tm.Manager, "fx/fail", `{}`)
	if err != nil || !res.IsError || !strings.Contains(res.Output, "故意失败") {
		t.Fatalf("isError 应映射为 Result.IsError：res=%+v err=%v", res, err)
	}

	// 空参数应被补成 {}，server 能正常处理。
	res, err = callTool(t, tm.Manager, "fx/echo", ``)
	if err != nil || res.IsError {
		t.Fatalf("空参数调用失败：res=%+v err=%v", res, err)
	}
}

// server 在处理调用期间反向 ping 客户端：客户端读循环必须能应答，不能被挂起的调用堵住。
func TestManagerAnswersServerPing(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))

	res, err := callTool(t, tm.Manager, "fx/ping_client", `{}`)
	if err != nil || res.Output != "pong-ok" {
		t.Fatalf("反向 ping 未被应答：res=%+v err=%v", res, err)
	}
}

func TestManagerListChangedRefreshes(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))

	if _, err := callTool(t, tm.Manager, "fx/change", `{}`); err != nil {
		t.Fatalf("change: %v", err)
	}
	// 状态里出现新工具早于 OnChange（先 refreshTools 再 publish），所以等的是 OnChange 本身。
	tm.waitFor(t, "OnChange 收到新工具", func([]ServerStatus) bool {
		return slices.Contains(tm.lastChange(), "fx/added")
	})
}

func TestManagerReconnectsAfterCrash(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))

	// exit 工具让 server 直接退出：这次调用拿到连接关闭错误（回喂模型的那条路径）。
	if _, err := callTool(t, tm.Manager, "fx/exit", `{}`); err == nil {
		t.Fatal("server 退出时在途调用应返回错误")
	}
	tm.waitFor(t, "重连成功", func(st []ServerStatus) bool {
		return st[0].Status == StatusReady && st[0].Restarts >= 1
	})
	// 重连后工具可用。
	if res, err := callTool(t, tm.Manager, "fx/echo", `{"text":"again"}`); err != nil || res.Output != "again" {
		t.Fatalf("重连后调用失败：res=%+v err=%v", res, err)
	}
	// 中间应有一次「工具被撤掉」的通知。
	tm.mu.Lock()
	sawEmpty := false
	for _, c := range tm.changes {
		if len(c) == 0 {
			sawEmpty = true
		}
	}
	tm.mu.Unlock()
	if !sawEmpty {
		t.Fatal("server 断开时应先撤掉它的工具")
	}
}

func TestManagerGivesUpAfterMaxFailures(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "crash"))
	st := tm.waitFor(t, "failed", statusIs(StatusFailed))[0]

	if !strings.Contains(st.LastError, "boom") {
		t.Fatalf("last_error 应包含 stderr 尾部，实际：%q", st.LastError)
	}
	if !strings.Contains(st.LastError, "exit status 3") {
		t.Fatalf("last_error 应包含退出状态，实际：%q", st.LastError)
	}
	cat := tm.Catalog()
	if len(cat) != 1 || cat[0].Available || len(cat[0].Tools) != 0 {
		t.Fatalf("failed 的 server 应在目录里标为不可用且无工具：%+v", cat)
	}
	// 给模型看的原因只取第一行，不带 stderr 尾部。
	if cat[0].Reason == "" || strings.Contains(cat[0].Reason, "\n") {
		t.Fatalf("不可用原因 = %q", cat[0].Reason)
	}
	// 放弃后不再自动重连：restarts 不再增长。
	before := st.Restarts
	time.Sleep(200 * time.Millisecond)
	if after := tm.Status()[0].Restarts; after != before {
		t.Fatalf("failed 后仍在重连：restarts %d → %d", before, after)
	}
}

func TestManagerRestartFromReady(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))

	if err := tm.Restart("fx"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	tm.waitFor(t, "重启后 ready", func(st []ServerStatus) bool {
		return st[0].Status == StatusReady && st[0].Restarts == 1
	})
	if err := tm.Restart("nope"); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("未知 server 应返回 ErrUnknownServer，实际 %v", err)
	}
}

// 超时取消时应向 server 发 notifications/cancelled。
func TestCallToolCancelSendsNotification(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))

	slow, ok := findTool(tm.Manager, "fx/slow")
	if !ok {
		t.Fatal("找不到 fx/slow")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := slow.Call(ctx, json.RawMessage(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回超时错误，实际 %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("超时后应立即返回，而不是等 server 回复")
	}

	// server 应记录到被取消的请求 id（非空即可：id 由客户端分配）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := callTool(t, tm.Manager, "fx/last_cancelled", `{}`)
		if err == nil && res.Output != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server 未收到 notifications/cancelled：res=%+v err=%v", res, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 往 stdout 打日志的不守规矩 server 不应导致连接失败。
func TestManagerSkipsNoisyStdout(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "noisy"))
	tm.waitFor(t, "ready", statusIs(StatusReady))
	if res, err := callTool(t, tm.Manager, "fx/echo", `{"text":"ok"}`); err != nil || res.Output != "ok" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// 配置无效 / disabled 的 server 不启动，状态如实展示，且不能被「重启」。
func TestManagerInvalidAndDisabledServers(t *testing.T) {
	bad := ServerConfig{Name: "bad", Err: errors.New("stdio server 缺少 command")}
	off := fixtureServerConfig(t, "off", "normal")
	off.Disabled = true
	tm := newTestManager(t, bad, off)

	st := tm.Status()
	if st[0].Status != StatusFailed || !strings.Contains(st[0].LastError, "缺少 command") {
		t.Fatalf("无效配置状态错误：%+v", st[0])
	}
	if st[1].Status != StatusDisabled {
		t.Fatalf("disabled 状态错误：%+v", st[1])
	}
	for _, name := range []string{"bad", "off"} {
		if err := tm.Restart(name); !errors.Is(err, ErrNotRestartable) {
			t.Fatalf("Restart(%s) 应返回 ErrNotRestartable，实际 %v", name, err)
		}
	}
}

// Close 后 server 进程必须退出，工具不再可调用。
func TestManagerCloseStopsServers(t *testing.T) {
	tm := newTestManager(t, fixtureServerConfig(t, "fx", "normal"))
	tm.waitFor(t, "ready", statusIs(StatusReady))
	echo, ok := findTool(tm.Manager, "fx/echo")
	if !ok {
		t.Fatal("找不到 fx/echo")
	}

	done := make(chan struct{})
	go func() { tm.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close 超时")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 拿着 Close 之前的目录快照调用（模拟一轮对话里的旧 router）：必须明确失败而不是挂住。
	if _, err := echo.Call(ctx, json.RawMessage(`{"text":"x"}`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后调用应返回 ErrClosed，实际 %v", err)
	}
}
