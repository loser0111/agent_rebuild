package mcp

// 测试用 MCP server：测试二进制自身。
//
// TestMain 看到环境变量 MCP_FIXTURE_MODE 就不跑测试，而是进入 server 模式，
// 在 stdin/stdout 上讲 MCP。这样测试不依赖 Python / Node / 网络，
// 而且走的是真实的子进程 + 管道 + 进程组路径，和生产完全一致。
//
// 模式：
//   - normal：正常 server（工具见 fixtureTools）
//   - noisy ：先往 stdout 打几行非 JSON 日志，再正常工作
//   - crash ：往 stderr 打一行 "boom" 后 exit 3

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

const fixtureEnv = "MCP_FIXTURE_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fixtureEnv); mode != "" {
		runFixture(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fixtureServerConfig 返回一个指向 fixture server 的配置。
func fixtureServerConfig(t *testing.T, name, mode string) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return ServerConfig{
		Name:           name,
		Transport:      "stdio",
		Command:        exe,
		Env:            []string{fixtureEnv + "=" + mode},
		StartupTimeout: 10 * time.Second,
	}
}

type fixtureMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func runFixture(mode string) {
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	}
	if mode == "noisy" {
		fmt.Println("starting fixture server...")
		fmt.Println("{not json")
	}
	f := &fixtureServer{pingReply: make(chan struct{}, 1)}
	br := bufio.NewReader(os.Stdin)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var msg fixtureMsg
			if json.Unmarshal(line, &msg) == nil {
				f.handle(msg)
			}
		}
		if err != nil {
			return // stdin 关闭 = 规范约定的退出信号
		}
	}
}

type fixtureServer struct {
	writeMu sync.Mutex

	mu            sync.Mutex
	extraTool     bool
	lastCancelled string

	pingReply chan struct{}
}

func (f *fixtureServer) write(v any) {
	b, _ := json.Marshal(v)
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	os.Stdout.Write(append(b, '\n'))
}

func (f *fixtureServer) reply(id json.RawMessage, result any) {
	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func textResult(s string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": s}},
		"isError": isErr,
	}
}

func (f *fixtureServer) handle(msg fixtureMsg) {
	// 对我们发出的反向 ping 的响应（无 method，有 id）。
	if msg.Method == "" {
		select {
		case f.pingReply <- struct{}{}:
		default:
		}
		return
	}
	switch msg.Method {
	case "initialize":
		f.reply(msg.ID, map[string]any{
			"protocolVersion": "2025-03-26", // 故意与客户端请求的不同，验证协商兼容
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "fixture", "version": "9.9.9"},
			"instructions":    "测试用 fixture 服务。第二句不应出现在目录里",
		})
	case "notifications/initialized":
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		f.mu.Lock()
		f.lastCancelled = string(p.RequestID)
		f.mu.Unlock()
	case "tools/list":
		f.listTools(msg)
	case "tools/call":
		// 异步处理：slow 调用期间仍要能收到 cancelled 通知与其他请求。
		go f.callTool(msg)
	default:
		f.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": -32601, "message": "not found"}})
	}
}

// listTools 分两页返回，验证客户端翻页。
func (f *fixtureServer) listTools(msg fixtureMsg) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	tool := func(name string, ann map[string]any) map[string]any {
		t := map[string]any{"name": name, "description": "fixture " + name,
			"inputSchema": map[string]any{"type": "object"}}
		if ann != nil {
			t["annotations"] = ann
		}
		return t
	}
	if p.Cursor == "" {
		f.reply(msg.ID, map[string]any{
			"tools": []any{
				tool("echo", map[string]any{"readOnlyHint": true}),
				tool("fail", nil),
				tool("ping_client", nil),
			},
			"nextCursor": "page2",
		})
		return
	}
	tools := []any{
		tool("slow", map[string]any{"destructiveHint": false}),
		tool("change", nil),
		tool("exit", nil),
		tool("last_cancelled", nil),
	}
	f.mu.Lock()
	if f.extraTool {
		tools = append(tools, tool("added", nil))
	}
	f.mu.Unlock()
	f.reply(msg.ID, map[string]any{"tools": tools})
}

func (f *fixtureServer) callTool(msg fixtureMsg) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	switch p.Name {
	case "echo":
		f.reply(msg.ID, textResult(fmt.Sprint(p.Arguments["text"]), false))
	case "fail":
		f.reply(msg.ID, textResult("故意失败", true))
	case "slow":
		time.Sleep(3 * time.Second)
		f.reply(msg.ID, textResult("slow done", false))
	case "change":
		f.mu.Lock()
		f.extraTool = true
		f.mu.Unlock()
		f.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		f.reply(msg.ID, textResult("changed", false))
	case "exit":
		os.Exit(0)
	case "ping_client":
		// 反向请求：server 在处理调用期间 ping 客户端，客户端必须应答。
		f.write(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
		select {
		case <-f.pingReply:
			f.reply(msg.ID, textResult("pong-ok", false))
		case <-time.After(2 * time.Second):
			f.reply(msg.ID, textResult("pong-timeout", true))
		}
	case "last_cancelled":
		f.mu.Lock()
		v := f.lastCancelled
		f.mu.Unlock()
		f.reply(msg.ID, textResult(v, false))
	default:
		f.reply(msg.ID, textResult("unknown tool "+p.Name, true))
	}
}
