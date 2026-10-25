# 09 · 工具系统

> 参考 Codex 与 Claude Code 的工具设计，为 agent 提供「读、写、搜、跑」四类核心能力。

---

## 1. 工具清单

| 工具 | 风险 | 需审批 | 说明 |
| --- | --- | --- | --- |
| `read_file` | safe | ❌ | 读文件，带行号；支持 offset/limit 分段 |
| `list_dir` | safe | ❌ | 列目录，目录在前文件在后，含大小；跳过噪音目录 |
| `search` | safe | ❌ | 正则搜索，返回 `文件:行号:内容`；支持 glob 过滤 |
| `write_file` | moderate | ✅ | 创建/整体覆盖文件，自动建父目录 |
| `edit_file` | moderate | ✅ | 精确字符串替换（比整体覆盖安全） |
| `run_command` | **dangerous** | ✅ | 执行 shell 命令（**默认关闭**） |

设计取舍参考两家：

- **工具集精简**（6 个），每个都覆盖高频需求，不让模型在几十个工具里选
- **读优先**：`read_file` 返回带行号内容，模型据此定位编辑位置
- **编辑优于覆盖**：`edit_file` 要求 `old_string` 完全一致，避免模型"我以为它长这样"
- **危险操作显式标注**，为人工审批（HITL）提供依据

---

## 2. 安全设计（重点）

### 2.1 路径隔离

所有文件工具都限制在 `TOOL_WORKSPACE` 内：

```go
// resolve 校验：解析后的绝对路径必须仍在 root 下
rel, err := filepath.Rel(root, abs)
if rel == ".." || strings.HasPrefix(rel, ".."+separator) {
    return "", fmt.Errorf("路径 %q 超出工作目录 %q 的范围", p, root)
}
```

测试覆盖了 `../secret.txt`、`../../etc/passwd`、`/etc/passwd`、`sub/../../outside.txt` 四种越界尝试，全部被拒绝。

### 2.2 环境变量白名单

`run_command` **不通传全部环境变量**，只给执行必需的基础变量：

```go
allowEnv: []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "SHELL", "TERM", "USER"}
```

实测验证：`SECRET_API_KEY=sk-xxx` 不会被命令读到。这防止 agent 执行 `printenv` 就把 API Key 泄漏到对话里。

### 2.3 进程组清理

命令超时/取消时，用 `Setpgid` + 杀负 PID 终止**整个进程组**：

```go
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
cmd.Cancel = func() error {
    return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
```

否则 `sh -c "sleep 100"` 会留下孤儿进程。

### 2.4 输出截断

| 场景 | 上限 | 策略 |
| --- | --- | --- |
| 读文件 | 30000 字符 | 提示用 offset 继续 |
| 单行显示 | 500 字符 | 防压缩代码撑爆 |
| 命令输出 | 64 KB | 保留头尾，中间省略 |
| 搜索结果 | 100 条 | 提示用更精确的 pattern |

截断时**保留头 2/3 + 尾 1/3**：开头通常是关键信息，结尾常有错误堆栈。

### 2.5 危险操作默认关闭

`run_command` 需要显式开启：

```bash
ENABLE_COMMAND_TOOL=true
```

即使开启，仍需通过审批器（见下）。

---

## 3. 审批机制（HITL 的基础）

每个工具标注危险等级，由 `Approver` 决定是否放行：

```go
type Approver interface {
    Approve(ctx context.Context, call agent.ToolCall) (allowed bool, reason string)
}
```

默认策略（`AutoApprover`）：

| 等级 | 默认行为 |
| --- | --- |
| `RiskSafe` | 直接执行 |
| `RiskModerate` | 允许（写文件是核心能力），`AllowModerate=true` |
| `RiskDangerous` | **拒绝**，需 `AllowDangerous`（由 `ENABLE_COMMAND_TOOL` 控制） |

被拒绝时**不是报错，而是把"拒绝"作为工具结果回喂给模型**：

```go
result.Output = "操作未执行：" + reason
result.Rejected = true
result.IsError = true
```

这样模型会换个方式（例如改为说明方案而不是直接写文件），而不是整个会话失败。

---

## 4. 事件协议扩展

SSE 新增三个事件（只追加，不改动已有事件）：

| 事件 | 时机 | 关键字段 |
| --- | --- | --- |
| `tool.call` | 模型请求调用工具 | `call_id` `name` `args` `risk` `requires_approval` |
| `tool.approval` | 需要人工确认 | 同 `tool.call` + `reason` |
| `tool.result` | 工具执行完成 | `call_id` `name` `output` `is_error` `rejected` `duration_ms` |

`call_id` 是关联键：`tool.call` 与 `tool.result` 用同一 ID，前端据此配对展示。

---

## 5. 多轮循环（ReAct）

分两层，职责清晰：

```
Loop（多轮编排）
  └─ Runner（单次模型调用）
       └─ 协议适配（OpenAI / Anthropic）
```

### 执行流程

```
第 1 轮: 模型 → 决定调用 read_file
         ↓ tool.call 事件推给前端
         执行工具 → tool.result 事件
         ↓ 结果以 role=tool 消息追加进上下文
第 2 轮: 模型 → 基于工具结果给出最终回答
         ↓ message.done
```

### 关键实现细节

**1. 工具结果必须回喂，且格式正确**

- OpenAI：`messages[]` 里加 `{"role":"tool","tool_call_id":"...","content":"..."}`
- Anthropic：`messages[]` 里加 `{"role":"user","content":[{"type":"tool_result","tool_use_id":"...","content":"..."}]}`

两者的 `assistant` 消息也要保留工具调用声明，否则上下文断裂。

**2. 流式工具参数的累积**

OpenAI 分片返回：首片带 `id`/`name`，后续片只带 `arguments` 片段，按 `index` 拼接。
Anthropic 用 `content_block_start`（`tool_use`）+ `input_json_delta`（`partial_json` 增量）。

**3. 工具调用轮也要上报用量**

即使不发终局 `done`，也要用 Content 为空的 `done` 上报 usage——这一轮的 token 确实消耗了，漏掉会让统计偏少。

**4. 步数上限防死循环**

`MAX_TOOL_STEPS=12`（默认）。达到上限时给出收尾说明而非静默截断，避免用户拿不到任何输出。

---

## 6. 配置

```bash
# 工具可访问的根目录（安全边界）；留空 = 当前工作目录
TOOL_WORKSPACE=
# 是否启用命令执行（危险，默认关）
ENABLE_COMMAND_TOOL=false
# 单次对话最多几轮工具迭代
MAX_TOOL_STEPS=12
# 单个工具执行超时
TOOL_TIMEOUT=120s
```

`GET /api/tools` 可查看当前生效的工具列表（含风险等级）。

---

## 7. 测试覆盖

| 测试 | 覆盖内容 |
| --- | --- |
| `TestReadFilePathTraversalBlocked` | 4 种越界路径全部拒绝 |
| `TestWriteFilePathTraversalBlocked` | 越界写入拒绝 |
| `TestRunCommandDoesNotLeakSecrets` | 敏感环境变量不透传 |
| `TestRunCommandTimeout` | 超时终止 |
| `TestRunCommandWorkdir` | 命令在工作目录执行 |
| `TestEditFileNotUniqueFails` | 多处匹配时拒绝（防误改） |
| `TestEditFileNotFoundString` | 提示先 read_file |
| `TestTruncate` | 头尾保留、中文不切半 |
| `TestLoopSingleToolCall` | 完整工具链路 + 上下文累积 |
| `TestLoopMultipleToolCalls` | 一轮多工具 |
| `TestLoopToolFailureFedBackToModel` | 失败信息回喂 |
| `TestLoopToolRejectedByApprover` | 拒绝后不产生副作用 + 理由回喂 |
| `TestLoopMaxSteps` | 死循环保护 |
| `TestLoopUsageAccumulated` | 跨轮用量累加 |
| `TestToolCallEndToEnd` | HTTP → SSE 完整链路（假模型） |

工具层 30+ 项、Loop 14 项、API 端到端 3 项，全部通过。

---

## 8. 已知限制

| 限制 | 说明 |
| --- | --- |
| 审批为自动策略 | 当前按危险等级自动放行/拒绝，尚未接入「前端弹窗等用户点确认」的交互式审批。`Approver` 接口已就绪，接 UI 只需实现一个走 SSE + REST 的 Approver |
| 无子代理 | subagent 未实现 |
| 无 MCP | 未接入 MCP 工具源 |
| 符号链接未解析 | 路径校验用 `filepath.Rel` 前缀检查，未 `EvalSymlinks`。工作区内指向外部的符号链接可能绕过限制（本地单用户场景风险低，多用户需加固） |
| 无审计日志 | 工具调用落库为 `role=tool` 消息，但没有独立的审计表 |

---

## 8.1 踩坑记录：`role=tool` 消息必须成对落库

### 症状

真实模型下第二轮对话报 400：

```
Messages with role 'tool' must be a response to a preceding message
with 'tool_calls'
```

### 根因

早期实现里 `persistToolMessages` **只落 `role=tool` 的结果消息，没落带 `tool_calls` 的 assistant 消息**。

因为 loop 内部维护的 `working` 上下文是完整的（所以当轮对话正常），但落库时只取了 `ToolResults`。下次请求时，历史里就出现了「没有前置 `tool_calls` 的 `tool` 消息」，OpenAI 直接拒绝。

这是一个**只在第二轮才暴露**的问题——第一轮永远正常。

### 修复（两侧都改）

**写入侧**：`loop.Result` 增加 `AssistantMessages` 字段，executor 按顺序成对落库：

```go
for _, asst := range res.AssistantMessages {
    // 1) 先落 assistant（带 tool_calls）——它是 tool 消息的前置
    e.store.AppendMessage(ctx, out)
    // 2) 再落该消息里每个调用对应的结果
    for _, tc := range asst.ToolCalls {
        e.store.AppendMessage(ctx, toolMsg)
    }
}
```

关键细节：如果 assistant 落库失败，**不能继续落 tool 消息**（否则又产生孤儿）。

**读取侧**：`toMessages` 做防御性清洗，即使历史已损坏也能用：

```go
// 第一遍：收集所有 assistant 声明过的 tool_call id
declared := map[string]bool{}
// ...
// 第二遍：丢弃找不到前置声明的 tool 消息
if m.Role == session.RoleTool && !declared[m.ToolCallID] {
    continue
}
```

这一层很重要：**已经写坏的数据库不需要手动清理**，新版本会自动跳过孤儿消息。OpenAI 与 Anthropic 两个实现都加了。

### 回归测试

| 测试 | 验证 |
| --- | --- |
| `TestOpenAIDropsOrphanToolMessages` | 孤儿 tool 消息不发出去 |
| `TestOpenAIKeepsValidToolChain` | 合法调用链完整保留（含 tool_call_id） |
| `TestOpenAIDropsOnlyOrphanToolMessages` | 混合场景只丢孤儿 |
| `TestOpenAIDropsToolMessageWithoutID` | 缺 tool_call_id 的也丢弃 |
| `TestAnthropicDropsOrphanToolMessages` | Anthropic 侧同样处理 |
| `TestToolMessagesPersistedInPairs` | 写入侧严格成对 |
| `TestSecondTurnHistoryIsValid` | 第二轮历史无孤儿 |

### 教训

**内存里的上下文正确 ≠ 落库的历史正确。** 两者必须分别断言。当初的单元测试只验证了 loop 的 `working` 数组，没验证 `store` 里实际存了什么，所以漏掉了这个缺陷。

---

## 9. 后续可做

1. **交互式审批**：把 `AutoApprover` 换成「发 `tool.approval` 事件 → 等 `POST /approve`」
2. **MCP 接入**：`modelcontextprotocol/go-sdk` 的工具适配成 `tool.Tool` 接口
3. **子代理**：复用 Loop，独立上下文与工具集
4. **审计表**：记录工具调用的完整参数与结果，便于回溯
