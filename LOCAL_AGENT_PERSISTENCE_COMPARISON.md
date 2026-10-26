# local-agent 项目的消息持久化与中断恢复机制

## 🎯 核心结论（先说答案）

**local-agent 能做到"网络/进程中断后继续"，但它的做法和我之前给 agent_rebuild 的建议完全不同——它不是靠"流式落库每个字符"，而是靠"按步骤粒度同步落库 + 崩溃后自动修复协议缺口"。**

这是一个桌面应用（Wails，Go + Vue），没有 HTTP 服务器中间层，消息持久化是**本地磁盘文件写入**，不是远程数据库写入。这个部署形态差异，直接决定了它完全不需要考虑"数据库超时"这个问题——这正是 agent_rebuild 的根源问题。

---

## 📐 架构对比

| 维度 | agent_rebuild | local-agent |
|------|--------------|-------------|
| 部署形态 | Go HTTP 服务器 + 浏览器（SSE） | Wails 桌面单进程 |
| 持久化目标 | 远程/本地 SQLite（经 DB 驱动） | 本地 JSON 文件（直接文件 I/O） |
| 持久化延迟风险 | 可能超时（网络/DB负载） | 几乎不会超时（本地磁盘） |
| "中断"的含义 | HTTP 连接断开 / DB 写入超时 | 进程被杀 / 应用强退 / 网络请求失败 |
| 恢复方式 | （我建议的）Checkpoint | 逐步落库 + 悬空调用修复 + Todo 注入 |

---

## 🔑 机制一：按"步骤"同步落库，而不是按"字符"或按"整轮"

### 实际代码逻辑（`internal/app/chat.go` 第 989-1160 行）

```go
for {  // ReAct 循环，每一轮：
    resp, err = llm.CallLLMForModel(ctx, model, req)  // ① 调用 LLM（流式在内部聚合成完整 resp）
    
    if 需要工具调用 {
        executedToolCalls := executeToolCalls(...)        // ② 执行工具（同步跑完）
        
        savedAssistant := Store.AppendMessage(assistantMsg) // ③ 立即落库 assistant(tool_calls)
        
        for 每个工具结果 {
            savedTool := Store.AppendMessage(toolMsg)       // ④ 立即落库 tool 结果
        }
        
        continue  // ⑤ 回到循环顶部，再调用一次 LLM
    }
    
    // finish_reason == stop
    savedFinal := Store.AppendMessage(finalMsg)  // ⑥ 落库最终回复
    return result
}
```

### 关键设计：**每一次 LLM 调用是一个"原子单元"**

- LLM 的流式输出（`CallLLMStreamForModel`）在内部把所有 delta 聚合成完整的 `resp` 才返回
- **流式只用来推送 SSE 事件给前端看字（打字机效果），不用来分片落库**
- 落库只发生在"这一轮 LLM 调用彻底返回"之后——要么整轮成功落库，要么这一轮完全不落库

### 这解决了你问的"网络中断"场景

```
场景：第 3 轮工具调用后，第 4 轮 LLM 生成内容时网络断了

t=0   ├─ 轮1：assistant(tool_calls) ✅ 已落库
t=1   ├─ 轮1：tool 结果            ✅ 已落库
t=2   ├─ 轮2：assistant(tool_calls) ✅ 已落库
t=3   ├─ 轮2：tool 结果            ✅ 已落库
t=4   ├─ 轮3：assistant(tool_calls) ✅ 已落库
t=5   ├─ 轮3：tool 结果            ✅ 已落库
t=6   ├─ 轮4：LLM 调用中... 网络中断 ❌
t=7   └─ CallLLMForModel 返回 error，这一轮完全不落库

结果：
✅ 前 3 轮的工具调用与结果都在（没有丢失，不用重跑）
❌ 只有第 4 轮（最后这次生成）没有产生任何持久化内容
✅ 用户重新发消息/点继续时，模型从「轮3 的工具结果」接着往下生成
   —— 相当于只重跑了"最后一次 LLM 调用"，不是整个任务
```

**这正是你问题里想要的效果**：不需要从头重新运行整个任务，只是最后一个"原子步骤"要重做。

---

## 🔑 机制二：崩溃后"悬空 tool_call"的自动修复

即便做到了"按步骤落库"，仍有一个**更窄的中间状态**可能被打断：

```go
savedAssistant := Store.AppendMessage(assistantMsg)  // ← 刚落库完 assistant(tool_calls)
                                                       // ↑ 如果这里应用被强杀/断电……
for 每个工具结果 {
    savedTool := Store.AppendMessage(toolMsg)         // ← 工具结果还没来得及落库
}
```

### 后果（如果不处理）

```
会话历史里留下：
  assistant: { tool_calls: [{id: "call_1", name: "read_file"}] }
  （没有对应的 tool 消息）

下次重启后用户发消息，history 送给模型：
  OpenAI/Anthropic 协议要求：每个 tool_call 必须有配对的 tool 消息
  → API 直接 400 拒绝请求
  → 表现为"重启后这条会话一开口就报错"（代码注释里写的真实线上现象）
```

### 解决方案：`repairDanglingToolCalls`（`internal/app/recovery.go`）

```go
// 每次构建发给模型的消息列表时，自动扫描并修补
func repairDanglingToolCalls(messages []llm.LLMMessage) []llm.LLMMessage {
    // 1. 找出所有已经有配对 tool 结果的 call_id
    answered := map[string]bool{...}
    
    // 2. 扫描所有 assistant(tool_calls)，找出没有配对结果的
    // 3. 给每个悬空的 tool_call 补一条占位结果：
    out = append(out, llm.LLMMessage{
        Role:       store.RoleTool,
        Content:    "[系统] 该工具调用在上一次运行中断时未产生结果（应用被关闭或强制中断），此处以空结果占位。",
        ToolCallID: tc.ID,
    })
}
```

**效果**：
- 不需要用户干预
- 不需要重新执行那个工具
- 模型看到"这个工具调用的结果是空的"，会自己判断要不要重试这个工具
- 会话协议永远合法，不会 400

**调用时机**：每次 `buildRunMessages` 组装发给模型的历史时自动调用（`chat.go:531`），对用户完全透明。

---

## 🔑 机制三：Todo 上下文重新注入（软续接）

```go
// internal/app/recovery.go
func withTodoContext(systemPrompt string, session *store.Session) string {
    // 把会话里"未完成"的 todo 项目，重新拼回 system prompt
    // 让模型在新的一轮对话里"记起"自己之前在做什么
}
```

**场景**：长任务被打断后，用户说"继续"——模型不能靠自己的记忆（新进程没有内存状态），只能靠**系统提示词里重新注入的 todo 清单**知道接下来要做什么。

这弥补了"消息历史完整"但"任务目标容易丢"的问题——协议层的完整性不等于语义层的连续性。

---

## 🔑 机制四：原子文件写入（防止"写一半"的文件损坏）

```go
// internal/store/sessions.go: saveSession
// 1. 写到临时文件
tmp := os.CreateTemp(s.dir, session.ID+".*.tmp")
tmp.Write(data)
// 2. fsync 确保落盘（不只是进了页缓存）
tmp.Sync()
tmp.Close()
// 3. rename 是同分区上的原子操作——要么是旧文件，要么是完整新文件，没有中间态
os.Rename(tmpPath, target)
```

**这解决了另一种"中断"**：如果写文件的过程中断电/崩溃，不会留下一个损坏的半截 JSON——而是老文件原样保留。

**这和 agent_rebuild 用 SQLite 的思路不同**：SQLite 本身也有 WAL 日志保证原子性，但 local-agent 用的是最朴素的"临时文件 + rename"，因为它的存储单元是整个会话 JSON 文件（不是数据库表行）。

---

## 📊 四个机制如何配合解决你的问题

你问的场景："中间某个消息因为网络原因断掉，能否从断点继续，而不是从头重跑？"

| 中断发生的位置 | 恢复方式 | 是否需要重跑 |
|--------------|---------|------------|
| 工具调用执行中（本地执行，几乎不受网络影响） | 工具跑完才会进入落库环节 | 不涉及网络中断 |
| LLM 生成过程中（网络请求失败） | 本轮 LLM 调用整体失败，之前轮次已落库 | 只重跑**这一轮**，不是整个任务 |
| 落库到 tool_calls 和 tool 结果之间（进程崩溃） | `repairDanglingToolCalls` 自动补占位结果 | 不重跑，模型自行判断是否重试该工具 |
| 写文件过程中断电 | 原子 rename，老文件完好 | 丢失的是"这一次"没保存成功的追加，不是历史 |
| 多步骤长任务整体被打断 | `withTodoContext` 重新注入未完成清单 | 从 todo 清单接续，不是从头 |

---

## 🆚 与我之前给 agent_rebuild 建议的对比

### 我之前建议的"Checkpoint 恢复机制"

```
TypeDone 时先保存 checkpoint（内容+状态）
再尝试落库
如果落库失败，从 checkpoint 恢复
```

这本质上是在"模拟"local-agent 这里天然具备的能力，但做法更复杂：

- local-agent：**天然就是一步一存**（工具调用后立即存），不需要额外的 checkpoint 层
- agent_rebuild：由于用了"整轮内容最后一次性落库"的模式，所以必须额外引入 checkpoint 作为补丁

### 关键差异：agent_rebuild 的"整轮"粒度 vs local-agent 的"步骤"粒度

```
agent_rebuild 现在的粒度（有问题）：
  工具调用1 ✅落库 → 工具调用2 ✅落库 → ... → 最终文本 ⚠️一次性落库（大块内容）

local-agent 的粒度（更优）：
  每一轮 LLM 调用（可能产出 tool_calls 或最终文本）都是一个原子单元
  每个单元完成后立即落库，不管这个单元产出的是"中间工具调用"还是"最终回复"
```

**对 agent_rebuild 的直接启发**：

你的 `persistMessage`（落最终回复）和 `appendToolCallMessage`/`appendToolResultMessage`（落工具消息）其实已经是分离的调用。问题不在于"要不要流式落字符"，而在于：

1. ✅ **保留现状**：工具调用/结果已经是步骤级落库（这部分做得对，不用改）
2. ✅ **最终文本也不需要逐字落库**：因为它和 local-agent 一样，是"一轮 LLM 调用"的原子产出——只要这一轮内部不要超时失败就行
3. 🎯 **真正要做的**：像 local-agent 的 `repairDanglingToolCalls` 一样，在**下次加载历史时自动检测并修复不一致状态**，而不是用复杂的 Checkpoint 机制

---

## ✅ 给 agent_rebuild 的具体建议（借鉴 local-agent 的思路）

### 1. 加一个"修复悬空消息"的函数（类似 `repairDanglingToolCalls`）

```go
// internal/executor/repair.go（新文件）

// repairIncompleteMessages 修复因为中断而留在 "streaming" 状态的消息。
// 对标 local-agent 的 repairDanglingToolCalls 思路：
// 不是"恢复原始内容"，而是"让历史恢复协议自洽"。
func repairIncompleteMessages(messages []session.Message) []session.Message {
    for i, m := range messages {
        if m.Role == session.RoleAssistant && m.Status == session.StatusStreaming {
            // 这条消息因为中断没有被正常收尾
            // 方案 A：如果 content 不为空，直接标记为 complete（内容其实是完整的，只是状态没来得及更新）
            if m.Content != "" {
                messages[i].Status = session.StatusComplete
            } else {
                // 方案 B：content 为空，说明连内容都没收到，标记为 interrupted
                messages[i].Status = session.StatusInterrupted
            }
        }
    }
    return messages
}
```

### 2. 检测"assistant(tool_calls) 无配对 tool 结果"（和 local-agent 完全一样的问题）

你的 `appendToolCallMessage` 和 `appendToolResultMessage` 是分两次调用落库的，中间也可能被打断（例如工具执行超时、进程重启）。这和 local-agent 的"悬空 tool_call"问题**一模一样**，直接搬运它的检测逻辑即可。

### 3. 不必引入额外的 Checkpoint/Redis

local-agent 证明了：只要持久化操作本身足够快（本地文件 I/O 几乎不超时），"整轮原子落库 + 下次加载时修复"就足够解决你担心的问题，不需要我之前建议的复杂方案。

**但注意**：agent_rebuild 用的是 SQLite（可能是远程或者有锁竞争），这才是真正导致超时的原因。所以**仍然建议**：
- 优化超时配置（上一轮讨论的内容仍然有效）
- 但不需要"流式落每个字符"或"额外 Checkpoint 存储"
- 只需要补一个"修复函数"，在加载历史时把不一致的状态修正过来

---

## 📌 总结：三句话回答你的问题

1. **local-agent 能做到中断后继续，靠的是"步骤级原子落库"（每轮 LLM 调用/每个工具调用是一个单元），不是"字符级流式落库"**——流式只用于前端打字机效果，不涉及持久化。

2. **真正的"断点"只会发生在"工具调用已存、结果未存"这个窄窗口**，local-agent 用 `repairDanglingToolCalls` 在下次加载时自动补占位结果，完全不需要重跑。

3. **对 agent_rebuild 的正确启发**：不需要流式落库文本内容，也不需要复杂的 Checkpoint/Redis 机制；只需要（a）解决 SQLite 的超时问题（本地文件 I/O 几乎没有这个问题，这是两个项目的本质差异），（b）加一个类似 `repairDanglingToolCalls` 的"历史自愈"函数即可。

