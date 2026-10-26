package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent_rebuild/internal/session"
)

// TestToolCallsRoundTrip 回归防护：tool_calls 与 tool_call_id 必须能完整往返。
//
// 线上故障：scanMessage 读出了 tool_calls 列却没有反序列化，
// 于是重新加载会话时 assistant 的 tool_calls 全部丢失，
// 配对的 tool 结果在 toMessages 里被判为孤儿丢弃，
// 模型误以为没调用过工具 → 每次对话都重复调用。
func TestToolCallsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toolcalls.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	store := NewSessionStore(db, 100)
	ctx := context.Background()

	sess := &session.Session{
		ID: "s1", Title: "t", Status: session.SessionIdle, Model: "m",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// assistant 消息带 tool_calls
	asst := &session.Message{
		ID: "m1", SessionID: "s1", Role: session.RoleAssistant,
		Content: "", Status: session.StatusComplete,
		ToolCalls: []session.ToolCall{
			{ID: "call_a", Name: "list_dir", Args: `{"path":"."}`},
			{ID: "call_b", Name: "read_file", Args: `{"path":"a.txt"}`},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.AppendMessage(ctx, asst); err != nil {
		t.Fatalf("写入 assistant 失败: %v", err)
	}

	// 配对的 tool 结果
	toolMsg := &session.Message{
		ID: "m2", SessionID: "s1", Role: session.RoleTool,
		Content: "文件列表", Status: session.StatusComplete,
		ToolCallID: "call_a",
		CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.AppendMessage(ctx, toolMsg); err != nil {
		t.Fatalf("写入 tool 失败: %v", err)
	}

	_ = db.Close()

	// 重开数据库，模拟「下次对话重新加载历史」。
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("重开数据库失败: %v", err)
	}
	defer db2.Close()
	store2 := NewSessionStore(db2, 100)

	msgs, err := store2.Messages(ctx, "s1")
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}

	var gotAsst, gotTool *session.Message
	for i := range msgs {
		switch msgs[i].Role {
		case session.RoleAssistant:
			gotAsst = &msgs[i]
		case session.RoleTool:
			gotTool = &msgs[i]
		}
	}

	if gotAsst == nil {
		t.Fatal("未找到 assistant 消息")
	}
	if len(gotAsst.ToolCalls) != 2 {
		t.Fatalf("tool_calls 丢失：期望 2 个，实际 %d 个", len(gotAsst.ToolCalls))
	}
	if gotAsst.ToolCalls[0].ID != "call_a" || gotAsst.ToolCalls[0].Name != "list_dir" {
		t.Fatalf("tool_calls[0] 内容不对: %+v", gotAsst.ToolCalls[0])
	}
	if gotAsst.ToolCalls[0].Args != `{"path":"."}` {
		t.Fatalf("tool_calls[0].Args 丢失: %q", gotAsst.ToolCalls[0].Args)
	}
	if gotAsst.ToolCalls[1].ID != "call_b" || gotAsst.ToolCalls[1].Name != "read_file" {
		t.Fatalf("tool_calls[1] 内容不对: %+v", gotAsst.ToolCalls[1])
	}

	if gotTool == nil {
		t.Fatal("未找到 tool 消息")
	}
	if gotTool.ToolCallID != "call_a" {
		t.Fatalf("tool_call_id 丢失: %q", gotTool.ToolCallID)
	}
}

// TestMessagesWithoutToolCalls 无工具调用的普通消息不受影响。
func TestMessagesWithoutToolCalls(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "plain.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	store := NewSessionStore(db, 100)
	ctx := context.Background()

	sess := &session.Session{
		ID: "s1", Title: "t", Status: session.SessionIdle,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	m := &session.Message{
		ID: "m1", SessionID: "s1", Role: session.RoleUser, Content: "你好",
		Status:    session.StatusComplete,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.AppendMessage(ctx, m); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	msgs, err := store.Messages(ctx, "s1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应有 1 条消息，实际 %d", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 0 {
		t.Fatalf("普通消息不应有 tool_calls: %+v", msgs[0].ToolCalls)
	}
	if msgs[0].Content != "你好" {
		t.Fatalf("内容不对: %q", msgs[0].Content)
	}
}
