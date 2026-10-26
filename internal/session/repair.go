package session

import "strings"

// RepairHistory 修复因中断（落库超时 / 进程崩溃 / SSE 断连）而产生的、
// 不满足语义自洽性的历史消息，返回修复后的副本。
//
// 纯函数：只读入参、不做任何 I/O，因此可以在每次读取历史时无代价地调用，
// **不会给存储层增加任何写入压力**——只是对已经加载在内存里的消息切片
// 做一次 O(n) 的线性扫描（n 是会话消息数，通常几十到几百条）。
//
// # 只做一件事：修正滞留在 streaming 的消息状态
//
// 成因：assistant 消息的正文（content）与状态（status）本应在同一次
// UpdateMessage 调用里原子更新为 complete，但如果那次调用因为超时失败，
// 消息会永久停留在 status=streaming——即使 content 其实已经写入成功
// （SSE 也已经把完整内容推给了前端）。修复方式：content（或 tool_calls）
// 非空则判定为实际已完成，修正为 complete；都为空则说明连文本都没收到，
// 判定为 interrupted（与用户主动取消的语义一致）。
//
// # 刻意不做的事：不处理"悬空 tool_call"（assistant 声明了调用、却没有
// 对应 tool 结果）
//
// 这是从 local-agent 项目移植这个函数时踩的一个坑，记录下来避免未来重踩：
//
// 悬空 tool_call 有两种成因，而且**无法仅从消息列表本身区分**：
//
//  1. 真的中断了（进程崩溃/落库超时）——此时确实需要某种补救；
//  2. ask_user 工具调用本身就在"等用户回答"，assistant(tool_calls) 落库后
//     故意不落 tool 结果，可能悬空几分钟到几天，这是**正常状态**，
//     由 session.Checkpoint 的 Pending 记录"谁在等什么"。
//
// 这两种情况天然长一个样子（有 tool_calls、无 tool 结果），要分清楚
// 必须去查有没有一条匹配 call_id 的活跃 Checkpoint.Pending——而 Checkpoint
// 是单独一张表/另一个 Store，不在 []Message 里，这个纯函数拿不到。
//
// 早期实现（对标 local-agent 的 repairDanglingToolCalls）试图直接给悬空
// 调用补一条"未产生结果"的占位 tool 消息，结果在 ask_user 挂起场景下
// 造成了两处真实回归：
//
//   - 送给模型的历史里，原本"未应答的调用"会被 agent/llm.go 与
//     agent/anthropic.go 的 toMessages 逻辑**整条声明一起丢弃**
//     （见那两个文件里 `declared` / `answered` 的两遍扫描，专门写了注释
//     说明这是为 ask_user 挂起场景设计的）。占位结果一插，这条调用就从
//     "声明+未应答，被丢弃"变成"声明+已应答"，于是模型会把我们瞎编的
//     占位文本当成用户对 ask_user 问题的真实回答，彻底答非所问。
//   - 前端 `web/src/state/toolRestore.ts` 的 rebuildToolCalls 用
//     "有没有 tool 结果" + "call_id 是否在当前 Checkpoint.Pending 里"
//     两个信号区分"真丢失"（渲染成 error）与"正在等回答"（渲染成
//     running/等待回答卡片）。占位结果一插，这条调用就从"无结果"
//     变成"有结果且状态 complete"，直接被渲成 success，等待回答的
//     交互卡片消失，用户看到的是一个凭空"成功"的工具调用。
//
// 换句话说：agent_rebuild 已经在"LLM 请求构建"和"前端展示"两处各自有
// 专门的、能区分"挂起"与"真丢失"的处理（并且比"插一条占位消息"更正确：
// 挂起时不该让模型看到任何答案，真丢失时也不需要编一条假结果），
// 在这之上再插一条通用占位消息纯属画蛇添足，而且会直接撞车。
// 所以这里不处理这一类问题，交给已有机制。
func RepairHistory(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}

	out := make([]Message, len(messages))
	copy(out, messages)

	fixStreamingStatus(out)
	return out
}

// fixStreamingStatus 原地修正滞留在 streaming 的 assistant 消息状态。
func fixStreamingStatus(messages []Message) {
	for i := range messages {
		m := &messages[i]
		if m.Role != RoleAssistant || m.Status != StatusStreaming {
			continue
		}
		if strings.TrimSpace(m.Content) != "" || len(m.ToolCalls) > 0 {
			// 内容（或工具调用）已经落库成功，只是收尾状态没来得及写入。
			//
			// 注意：ask_user 挂起时也会落一条 status=streaming、
			// content=模型提问前说的话 的消息（见 executor.go 的
			// TypeAwaitingInput 分支），这里把它一并修成 complete 是安全的——
			// 这条消息之后永远不会再被写入（resume 会新建一条占位消息承载
			// 后续生成，不会复用它），且没有任何下游逻辑依赖它必须保持
			// streaming 才能正确展示（前端的"等待回答"提示由
			// Checkpoint.Pending 驱动，不看这条消息的 status）。
			m.Status = StatusComplete
		} else {
			// 连正文都没收到：这一轮确实没跑完。
			m.Status = StatusInterrupted
		}
	}
}
