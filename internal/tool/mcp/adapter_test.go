package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"agent_rebuild/internal/tool"
)

func boolPtr(b bool) *bool { return &b }

func TestResolveRisk(t *testing.T) {
	ro := RemoteTool{Name: "r", Annotations: &ToolAnnotations{ReadOnlyHint: boolPtr(true)}}
	nd := RemoteTool{Name: "n", Annotations: &ToolAnnotations{DestructiveHint: boolPtr(false)}}
	plain := RemoteTool{Name: "p"}

	cases := []struct {
		name string
		sc   ServerConfig
		rt   RemoteTool
		want tool.Risk
	}{
		{"默认 dangerous，即使自报只读", ServerConfig{}, ro, tool.RiskDangerous},
		{"信任 readOnlyHint", ServerConfig{TrustAnnotations: true}, ro, tool.RiskSafe},
		{"信任 destructiveHint=false", ServerConfig{TrustAnnotations: true}, nd, tool.RiskModerate},
		{"没有 annotations 仍 dangerous", ServerConfig{TrustAnnotations: true}, plain, tool.RiskDangerous},
		{"按工具覆盖优先于 annotations", ServerConfig{TrustAnnotations: true,
			ToolRisk: map[string]tool.Risk{"r": tool.RiskDangerous}}, ro, tool.RiskDangerous},
		{"* 作为 server 默认", ServerConfig{ToolRisk: map[string]tool.Risk{"*": tool.RiskModerate}}, plain, tool.RiskModerate},
	}
	for _, c := range cases {
		if got := resolveRisk(c.sc, c.rt); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeSchema(t *testing.T) {
	cases := []struct{ in, want string }{
		{``, `{"type":"object","properties":{}}`},
		{`null`, `{"type":"object","properties":{}}`},
		{`{"type":"object"}`, `{"properties":{},"type":"object"}`},
		{`{"properties":{"a":{"type":"string"}},"type":"object"}`, `{"properties":{"a":{"type":"string"}},"type":"object"}`},
	}
	for _, c := range cases {
		if got := string(normalizeSchema(json.RawMessage(c.in))); got != c.want {
			t.Errorf("normalizeSchema(%q) = %s，期望 %s", c.in, got, c.want)
		}
	}
}

func TestRenderResult(t *testing.T) {
	res := &CallResult{Content: []ContentBlock{
		{Type: "text", Text: "第一段"},
		{Type: "image", MimeType: "image/png", Data: strings.Repeat("A", 4096)},
		{Type: "resource", Resource: &EmbeddedResource{URI: "file:///a", Text: "资源正文"}},
		{Type: "resource_link", Name: "b", URI: "file:///b"},
	}, StructuredContent: json.RawMessage(`{"x":1}`)}
	out := renderResult(res)
	for _, want := range []string{"第一段", "[image: image/png, 约 3KB", "资源正文", "[resource_link: b file:///b]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, `{"x":1}`) {
		t.Fatal("已有 text 时不应重复输出 structuredContent")
	}

	onlyStructured := renderResult(&CallResult{StructuredContent: json.RawMessage(`{"x":1}`)})
	if onlyStructured != `{"x":1}` {
		t.Fatalf("没有 text 时应以 structuredContent 兜底：%q", onlyStructured)
	}
	if got := renderResult(&CallResult{IsError: true}); !strings.Contains(got, "失败") {
		t.Fatalf("空错误结果应有说明：%q", got)
	}
}

// mcp.json 关掉的工具保留在目录里（BaseEnabled=false），前端面板才能重新打开它；空名与重名跳过。
func TestBuildCatalogKeepsDisabledTools(t *testing.T) {
	sc := ServerConfig{Name: "s", DisabledTools: []string{"b"}, ToolRisk: map[string]tool.Risk{"a": tool.RiskSafe}}
	cat := buildCatalog(sc, nil, []RemoteTool{
		{Name: "a", Description: " 做 A "}, {Name: "b"}, {Name: ""}, {Name: "a"},
	}, func(string, ...any) {})
	if len(cat) != 2 {
		t.Fatalf("目录 = %+v", cat)
	}
	if cat[0].FullName() != "s/a" || cat[0].Description != "做 A" || !cat[0].BaseEnabled || cat[0].BaseRisk != tool.RiskSafe {
		t.Fatalf("a = %+v", cat[0])
	}
	if cat[1].Name != "b" || cat[1].BaseEnabled || cat[1].BaseRisk != tool.RiskDangerous {
		t.Fatalf("b 应保留且 BaseEnabled=false、默认 dangerous：%+v", cat[1])
	}
	if string(cat[1].InputSchema) != `{"type":"object","properties":{}}` {
		t.Fatalf("schema 未规整：%s", cat[1].InputSchema)
	}
}

// ---- client 协议细节（不起子进程，用 io.Pipe 驱动）----

func TestReadLineSkipsOversizedLine(t *testing.T) {
	in := strings.Repeat("x", 100) + "\n" + `{"ok":1}` + "\n"
	br := bufio.NewReaderSize(strings.NewReader(in), 16)
	if _, err := readLine(br, 50); !errors.Is(err, errLineTooLong) {
		t.Fatalf("超长行应返回 errLineTooLong，实际 %v", err)
	}
	line, err := readLine(br, 50)
	if err != nil || string(line) != `{"ok":1}` {
		t.Fatalf("超长行之后应能继续读下一行：%q %v", line, err)
	}
}

// server 把 id 回成字符串、或用 batch 回复时，客户端都应正确配对。
func TestClientAcceptsStringIDAndBatch(t *testing.T) {
	srvR, cliW := io.Pipe() // 客户端 → server
	cliR, srvW := io.Pipe() // server → 客户端
	c := NewClient(cliR, cliW, nil, nil)
	t.Cleanup(func() { _ = srvW.Close(); _ = cliW.Close() })

	go func() {
		br := bufio.NewReader(srvR)
		for i := 0; ; i++ {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal(line, &m)
			if len(m.ID) == 0 {
				continue // 通知
			}
			var out string
			if i%2 == 0 {
				out = `{"jsonrpc":"2.0","id":"` + strings.Trim(string(m.ID), `"`) + `","result":{"tools":[]}}`
			} else {
				out = `[{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"tools":[{"name":"t"}]}}]`
			}
			_, _ = srvW.Write([]byte(out + "\n"))
		}
	}()

	ctx := t.Context()
	for i := 0; i < 2; i++ {
		done := make(chan error, 1)
		go func() { _, err := c.ListTools(ctx); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("第 %d 次 ListTools: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 次 ListTools 未收到响应（id 配对失败）", i)
		}
	}
}
