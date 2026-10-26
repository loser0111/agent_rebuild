package session

import "testing"

func msg(role Role, content string, status MessageStatus) Message {
	return Message{ID: NewMessageID(), SessionID: "s1", Role: role, Content: content, Status: status}
}

// TestRepairHistory_NoIssues 正常历史不应被改动（长度、顺序、状态都保持原样）。
func TestRepairHistory_NoIssues(t *testing.T) {
	in := []Message{
		msg(RoleUser, "你好", StatusComplete),
		msg(RoleAssistant, "你好，有什么可以帮你", StatusComplete),
	}
	out := RepairHistory(in)
	if len(out) != len(in) {
		t.Fatalf("长度应不变，got %d want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Status != in[i].Status || out[i].Content != in[i].Content {
			t.Fatalf("第 %d 条消息被意外修改: %+v", i, out[i])
		}
	}
}

// TestRepairHistory_StreamingWithContent 内容已完整但状态滞留 streaming，
// 应修正为 complete（而不是被当作"未完成"丢弃）。
func TestRepairHistory_StreamingWithContent(t *testing.T) {
	in := []Message{
		msg(RoleUser, "你好", StatusComplete),
		msg(RoleAssistant, "这是完整的回复内容", StatusStreaming),
	}
	out := RepairHistory(in)
	if out[1].Status != StatusComplete {
		t.Fatalf("应修正为 complete，got %s", out[1].Status)
	}
	if out[1].Content != "这是完整的回复内容" {
		t.Fatalf("内容不应被改动，got %q", out[1].Content)
	}
}

// TestRepairHistory_StreamingWithToolCallsOnly 没有正文但有 tool_calls 时，
// 同样应判定为已完成（工具调用本身就是"已落库的产出"）。
func TestRepairHistory_StreamingWithToolCallsOnly(t *testing.T) {
	assistant := msg(RoleAssistant, "", StatusStreaming)
	assistant.ToolCalls = []ToolCall{{ID: "call_1", Name: "list_dir"}}
	in := []Message{assistant}
	out := RepairHistory(in)
	if out[0].Status != StatusComplete {
		t.Fatalf("应修正为 complete，got %s", out[0].Status)
	}
}

// TestRepairHistory_StreamingEmpty 既没有正文也没有 tool_calls，
// 说明这一轮确实什么都没产出，应判定为 interrupted。
func TestRepairHistory_StreamingEmpty(t *testing.T) {
	in := []Message{msg(RoleAssistant, "", StatusStreaming)}
	out := RepairHistory(in)
	if out[0].Status != StatusInterrupted {
		t.Fatalf("应修正为 interrupted，got %s", out[0].Status)
	}
}

// TestRepairHistory_AskUserPendingIsNotTouched 这是本函数最重要的不变量：
// ask_user 挂起时留下的"assistant(tool_calls) 无配对 tool 结果"必须原样
// 保留，不能被当成"悬空调用"补一条占位结果。
//
// 背景：executor.go 的 TypeAwaitingInput 分支会把模型提问前说的话落库成
// status=streaming 的 assistant 消息，而触发挂起的那个 ask_user 调用本身
// （独立的一条 assistant(tool_calls) 消息，由 appendToolCallMessage 落库）
// 天然没有配对的 tool 结果——这个状态可能持续几分钟到几天，是正常状态，
// 不是中断留下的垃圾。RepairHistory 如果替它补一条占位 tool 结果，会让
// agent/llm.go、agent/anthropic.go 把这条"假结果"当成模型已经得到的答案
// 转发给 LLM，也会让前端把本该是"等待回答"的卡片渲染成"成功"。
//
// 因此 RepairHistory 必须完全不处理 tool_calls 配对问题——这类判断
// 依赖 Checkpoint（哪个 call_id 正被 Pending 持有），而 Checkpoint 不在
// []Message 里，本函数天然看不到也不该看。
func TestRepairHistory_AskUserPendingIsNotTouched(t *testing.T) {
	preamble := msg(RoleAssistant, "让我先确认一下你的需求。", StatusStreaming)
	askCall := msg(RoleAssistant, "", StatusComplete)
	askCall.ToolCalls = []ToolCall{{ID: "call_ask_1", Name: "ask_user", Args: `{"question":"用哪种数据库？"}`}}

	in := []Message{
		msg(RoleUser, "帮我设计一下存储方案", StatusComplete),
		preamble,
		askCall,
		// 注意：没有 role=tool、tool_call_id=call_ask_1 的消息——这是预期状态。
	}

	out := RepairHistory(in)

	if len(out) != len(in) {
		t.Fatalf("不应插入任何消息，长度 got %d want %d", len(out), len(in))
	}
	// preamble 的 streaming 状态按既有逻辑会被修正为 complete（这是安全的，
	// 见 fixStreamingStatus 的注释），但 askCall 必须保持原样：
	// 既不能多出配对的 tool 结果，也不能被动它的 Status/ToolCalls。
	if len(out[2].ToolCalls) != 1 || out[2].ToolCalls[0].ID != "call_ask_1" {
		t.Fatalf("ask_user 调用本身不应被改动，got %+v", out[2])
	}
	for _, m := range out {
		if m.Role == RoleTool && m.ToolCallID == "call_ask_1" {
			t.Fatalf("不应为挂起中的 ask_user 调用补占位 tool 结果，got %+v", m)
		}
	}
}

// TestRepairHistory_EmptyInput 空输入不应 panic，原样返回。
func TestRepairHistory_EmptyInput(t *testing.T) {
	out := RepairHistory(nil)
	if len(out) != 0 {
		t.Fatalf("空输入应返回空，got %d 条", len(out))
	}
}

// TestRepairHistory_DoesNotMutateInput 确认不修改调用方传入的原始切片
// （调用方可能还持有这份引用用于别的用途，例如日志/trace）。
func TestRepairHistory_DoesNotMutateInput(t *testing.T) {
	original := msg(RoleAssistant, "完整内容", StatusStreaming)
	in := []Message{original}

	_ = RepairHistory(in)

	if in[0].Status != StatusStreaming {
		t.Fatalf("不应修改入参，入参状态被改成了 %s", in[0].Status)
	}
}
