# 07 · 编排层选型与接口契约

> 目标：为 A（消息快照）、B（工具幂等重放）、C（HITL 中断）三项需求确定
> **不会阻碍后续迭代**的接口契约。
>
> 本文档的结论有实测依据：已完成 eino ADK v0.9.22 的可编译 spike（源码已清理，
> 结论沉淀于此）。
>
> **实施进度**：阶段 1（执行生命周期解耦）与阶段 2（Runner 接口改造准备）已完成，
> 见第 9 节。

---

## 1. 实测：eino ADK 可行性评估

### 1.1 spike 做了什么

用 eino 的 ADK 实现该项目现有的 `Runner` 接口，验证能力与阻抗失配程度。

**结论：可以实现，能力真实可用。**

已核实的 API（v0.9.22，全部来自源码）：

| 能力 | API | 状态 |
| --- | --- | --- |
| HITL 中断 | `adk.Interrupt` / `StatefulInterrupt` / `CompositeInterrupt` | ✅ 存在 |
| 断点恢复 | `Runner.Resume(ctx, checkPointID, opts...)` | ✅ 存在 |
| 断点存储 | `adk.CheckPointStore`（Get/Set/LastCheckpoint） | ✅ 存在 |
| 流式事件 | `Runner.Run` 返回 `*AsyncIterator[*AgentEvent]` | ✅ 存在 |
| 会话取消 | `CancelHandle` / `CancelError` / `StreamCanceledError` | ✅ 存在 |

### 1.2 实测到的差异（这是决策依据）

| # | 差异 | 影响 |
| --- | --- | --- |
| 1 | 构造器是 `NewChatModelAgent`（非 ReAct 命名） | 小，只是命名认知 |
| 2 | 消息类型 `[]*schema.Message`（指针切片），本项目 `[]session.Message`（值切片 + 自有类型） | **中**：每次调用都要全量类型转换，且要保住 `tool_call_id` |
| 3 | 事件模型嵌套：`AgentEvent{Output:{MessageOutput:{Role, Message}}}` vs 本项目扁平 `Event{Type, Delta, Tool, Err}` | **大**：需按 Role 分支转换，且 `MessageOutput` 有 Message / MessageStream 两种形态 |
| 4 | eino 的 checkpoint 存**序列化后的图状态**（`[]byte`，不透明） | **大**：与本项目 `session.Store`（强类型、可查询、可展示）无法共用一套结构，只能并列两套 |
| 5 | `checkPointID` 由框架生成，需成为新的外部契约暴露给前端 | 中：前端要存这个 ID |

### 1.3 依赖与体积实测

| 指标 | 数值 |
| --- | --- |
| eino 引入的模块数 | **78 个** |
| `go.sum` 行数 | 139 行 |
| 含 eino 的最小二进制 | **11 MB** |
| 本项目当前二进制 | **9.1 MB** |

也就是说：**只为「挂一个 agent」就引入 78 个模块**，其中包含 bytedance/sonic、gopkg、prometheus 等（eino 自带监控/序列化依赖树）。

---

## 2. 选型结论

### 结论：**自研编排层，不引入 eino**

### 理由（按权重排序）

**1. 阻抗失配集中在两处最难调和的地方**

- 事件模型：eino 嵌套 + Role 驱动 vs 本项目扁平 + EventType 驱动
- 存储模型：eino 不透明 `[]byte` vs 本项目强类型领域对象

这不是「写个适配器」能解决的，而是**每次框架升级都要重新对齐**。而 eino 是 v0.9.x，升级必然会有 breaking change。

**2. 本项目已有自己的编排基础**

| 已有能力 | 位置 | 对 A/B/C 的价值 |
| --- | --- | --- |
| SSE 事件流 + `seq` 单调递增 | `internal/sse/hub.go` | 直接作为 HITL 事件通道 |
| 环形缓冲 + `Last-Event-ID` 重放 | `internal/sse/buffer.go` | 断线恢复已具备 |
| 会话状态机（pending/streaming/interrupted/error） | `internal/session/model.go` | 中断语义已有 |
| `Store.Update` 原子读改写 | `internal/session/store.go` | checkpoint 原子写基础 |
| `Snapshot` / `Snapshotter` | `internal/session/store.go` | A 项基础 |
| 会话级串行锁 | `internal/agent/lock.go` | loop 并发控制 |

引入 eino 等于在已有编排之上再叠一层，形成双轨。

**3. HITL 的真实复杂度低于框架假设**

eino 的 Interrupt 是为**任意 DAG 节点 + 嵌套子图**设计的。本项目的 agent loop 是**线性 ReAct 循环**（模型 → 工具 → 模型），中断点只有「工具执行前」一处。

用图引擎解决线性中断，抽象成本大于实现成本。

**4. 桌面形态要求轻依赖**

单二进制 + 无外部服务是当前形态的核心价值。78 个模块 / +2MB 与之相悖。

### 什么情况下应改选 eino

诚实列出会让我改主意的条件：

| 条件 | 说明 |
| --- | --- |
| subagent 需要**嵌套多层中断**（子图内部暂停、父图继续） | 手写状态机成本陡增 |
| 需要**并行分支**各自独立 checkpoint | 如同时跑 3 个 subagent 各自可暂停 |
| 团队不愿维护 500+ 行状态机 | 长期维护成本考量 |
| 需要多机分布式续跑 | 单机 checkpoint 不够 |

**建议**：先把自研版本做到 subagent 阶段（阶段 7），届时若发现嵌套中断成本过高，再评估迁移。因为接口契约（本文档第 3 节）设计得当的话，迁移是**替换 loop 实现**，而非重写全部。

---

## 3. 接口契约（核心产出）

本节是后续所有实现的地基。**先定契约，再选实现。**

### 3.1 执行模型：生命周期必须与 HTTP 连接解耦

#### 现状问题（已核实代码）

```go
// internal/api/messages.go:42,98
ctx := r.Context()
runCtx, cancel := context.WithTimeout(ctx, s.cfg.RunTimeout)
```

`runCtx` 派生自 `r.Context()`，意味着：

> 客户端断开 → 生成立即中止。

这对纯聊天合理（省 token），但对 A/B/C 是**致命**的：

| 场景 | 当前行为 | 期望行为 |
| --- | --- | --- |
| HITL 等审批 | 用户关页面去开会 → 任务死亡 | 必须存活，等用户回来 approve |
| 长任务（RAG + 多工具） | 180s 超时被砍 | 应跨请求继续 |
| 用户刷新页面 | 生成中止 | 应继续，重连后补发事件 |

**HITL 尤其矛盾**：等审批可能几分钟到几小时，但 `RUN_TIMEOUT=180s` 且连接随时可断——用户还没点确认，任务已死。

#### 契约

```go
// 执行任务的生命周期独立于请求，SSE 连接只是"观察者"。
//
// 连接断开 → detach（不再推送），但不 cancel。
// 只有显式 Stop / 超时 / 会话删除才 cancel。
```

```go
// Executor 负责管理执行中的任务，与 HTTP 层解耦。
type Executor interface {
    // Start 启动一次执行；返回可订阅的任务句柄。
    Start(ctx context.Context, sessionID string, req Request) (*Task, error)

    // Get 取回正在运行的任务（用于重连后订阅）。
    Get(sessionID string) (*Task, bool)

    // Stop 显式终止（对应 POST /cancel）。
    Stop(sessionID string) error
}

type Task struct {
    ID        string
    SessionID string
    // Subscribe 返回事件流；可多次订阅（重连）。
    Subscribe(afterSeq uint64) (<-chan Event, func(), error)
    // Done 在任务终止时关闭。
    Done() <-chan struct{}
}
```

关键点：

- `Task.Subscribe` 可被多次调用（重连、多标签页）
- 事件通过 `sse.Hub` 扇出（**已有基础设施**）
- 连接断开只 `unsub`，不影响任务

### 3.2 Runner 接口

```go
// Runner 只负责「一次模型调用」，不负责多轮循环。
// 多轮（工具调用、ReAct）由 loop 负责。
type Runner interface {
    Run(ctx context.Context, req Request) <-chan Event
}

type Request struct {
    // History 到当前步为止的完整对话（含 tool 消息）。
    History []session.Message
    // Tools 本轮可用工具。nil 表示不启用工具。
    Tools []ToolSpec
    // Resume 非 nil 表示从断点继续（B 项幂等重放的关键）。
    Resume *Checkpoint
}

type ToolSpec struct {
    Name        string
    Description string
    // Schema 是 JSON Schema 格式的参数定义。
    Schema string
    // RequiresApproval 标记为 true 时，执行前需用户确认（C 项）。
    RequiresApproval bool
}
```

**设计理由**：

- `Runner` 保持「单轮」职责。协议层不需要知道工具怎么执行，工具层不需要知道用什么模型。
- `Request.Tools` 让协议适配层（OpenAI/Anthropic）能把工具声明翻译成各自的 `tools` 字段。
- `Request.Resume` 让同一接口同时支持「全新开始」和「断点续跑」。

### 3.3 Event 接口

```go
type EventType string

const (
    TypeDelta    EventType = "delta"     // 文本增量（已有）
    TypeDone     EventType = "done"      // 生成结束（已有）
    TypeError    EventType = "error"     // 错误（已有）

    // 以下为 A/B/C 新增
    TypeToolCall     EventType = "tool.call"      // 模型请求调用工具
    TypeToolApproval EventType = "tool.approval"  // 需用户确认（C 项）
    TypeToolResult   EventType = "tool.result"    // 工具执行结果（B 项落库 + 推送）
)

type Event struct {
    Type    EventType
    Delta   string     // TypeDelta
    Content string     // TypeDone
    Usage   *Usage     // TypeDone

    // 以下按需填一
    ToolCall   *ToolCall   // TypeToolCall / TypeToolApproval
    ToolResult *ToolResult // TypeToolResult
    Err        error       // TypeError
}

type ToolCall struct {
    ID   string // 供应商返回的 tool_call_id（B 项幂等的主键）
    Name string
    Args string
}

type ToolResult struct {
    CallID  string
    Output  string
    IsError bool
    // Rejected 表示用户拒绝执行（C 项）。
    // 语义：不是错误，而是把"拒绝"作为工具结果喂回模型。
    Rejected bool
}
```

**关键语义（容易做错）**：

用户拒绝执行工具时：

```go
// ✅ 正确：作为工具结果喂回模型，让模型换个方式回答
ToolResult{CallID: id, Output: "用户拒绝了此操作：" + reason, IsError: true, Rejected: true}

// ❌ 错误：中断整个会话，用户得重新提问
```

### 3.4 Checkpoint

```go
type LoopState string

const (
    StateRunning          LoopState = "running"
    StateAwaitingApproval LoopState = "awaiting_approval" // C 项暂停点
    StateDone             LoopState = "done"
    StateFailed           LoopState = "failed"
)

type Checkpoint struct {
    Version   int    // 结构版本，便于演进（参考 SnapshotVersion 先例）
    SessionID string
    StepIndex int    // 循环第几步

    State LoopState

    // Messages 到这一步的完整历史（含 tool 消息）。
    Messages []session.Message

    // Pending 等待审批的工具调用（State=awaiting_approval 时非 nil）。
    Pending *ToolCall

    // Completed 已完成的工具调用结果 —— B 项幂等的核心。
    // key = tool_call_id
    Completed map[string]ToolResult

    Usage     session.Usage
    UpdatedAt time.Time
}
```

```go
// Checkpointer 独立于 session.Store，不动现有契约测试。
type Checkpointer interface {
    Save(ctx context.Context, cp *Checkpoint) error
    Load(ctx context.Context, sessionID string) (*Checkpoint, error)
    // List 返回历史检查点（时间旅行用，可选实现）。
    List(ctx context.Context, sessionID string) ([]*Checkpoint, error)
    Delete(ctx context.Context, sessionID string) error
}
```

**为什么独立接口**：`session.Store` 有严格的契约测试（深拷贝、原子读改写、ctx 语义）。把 checkpoint 塞进去会污染那个契约。独立接口可以后续单独换 SQLite 实现。

### 3.5 Message 结构扩展

```go
// 追加，不改现有值
const RoleTool Role = "tool"

type Message struct {
    // ... 现有字段全部不动

    // ToolCalls assistant 发起的工具调用（可多个）。
    ToolCalls []ToolCall `json:"tool_calls,omitempty"`

    // ToolCallID tool 消息关联的调用 ID。
    ToolCallID string `json:"tool_call_id,omitempty"`
}
```

因为带 `omitempty`，**前端和现有测试不受影响**。纯加法。

---

## 4. 分层与职责

```
internal/agent/
├── runner.go          Runner 接口 + Event + ToolSpec
├── openai/            协议适配（建议换官方 SDK）
├── anthropic/         协议适配
├── loop/              ★ 自研编排（核心资产）
│   ├── loop.go        ReAct 循环：模型 → 工具 → 模型
│   ├── checkpoint.go  Checkpointer 实现
│   └── approval.go    HITL 状态机
├── tool/
│   ├── registry.go    注册表 + 危险等级
│   ├── mcp/           MCP（官方 go-sdk v1.8.0）
│   └── cli/           CLI 工具（沙箱约束）
└── executor/          ★ 执行生命周期（与 HTTP 解耦）
```

| 层 | 职责 | 依赖谁 |
| --- | --- | --- |
| 协议适配 | 请求/响应 ↔ Event 翻译 | 官方 SDK |
| loop | ReAct 循环、工具调度、终止条件 | 只依赖 `Runner` + `Tool` |
| tool | 工具实现与危险等级 | MCP SDK / os/exec |
| executor | 任务生命周期、订阅扇出 | sse.Hub |

---

## 5. 实施顺序（每步可独立验证）

| 阶段 | 内容 | 依赖 | 风险 |
| --- | --- | --- | --- |
| 1 | `Executor` + 执行与连接解耦 | 无 | 中（触及 api 层事件循环） |
| 2 | `Runner` 接口改造（`Request`/`Tools`/`Resume`） | 1 | 低（5 文件） |
| 3 | `Checkpointer` + `MemoryCheckpointer` | 2 | 低（纯增量） |
| 4 | `loop` 单轮 + checkpoint 落盘 | 3 | 中 |
| 5 | 工具调用（不含审批） | 4 | 中 |
| 6 | B：`Completed` 幂等重放 | 5 | 低 |
| 7 | C：`RequiresApproval` + approve 接口 + 前端 UI | 6 | 中 |
| 8 | SQLite Checkpointer | 3 | 低 |
| 9 | MCP / CLI 工具 | 5 | 中 |
| 10 | subagent（此时评估是否需 eino） | 7 | 待定 |

**阶段 1 是关键**：它是 HITL 的生死线。现在改是改 `sendMessageStream` 的 select 结构；等工具/subagent 都加进来再改，等于重写整个执行流。

---

## 6. 需要现在就定 vs 可以以后定

### 必须现在定（承重决策）

| 决策 | 为什么现在定 |
| --- | --- |
| 执行生命周期是否解耦 | 触及 api 层事件循环结构，后期改动成本指数上升 |
| `Runner` 接口形态 | 5 文件现在改 vs 15+ 文件后期改 |
| 编排层自研 vs eino | 定了才能开始写 loop |

### 可以以后定

| 决策 | 说明 |
| --- | --- |
| checkpoint 存储介质（内存 → SQLite） | 接口不变，换实现即可 |
| 工具注册表细节 | 随工具增加演进 |
| MCP 接入方式 | 独立模块 |
| subagent 隔离粒度 | 到阶段 10 才需要 |
| 前端审批 UI 形态 | 事件契约定了就行 |

---

## 7. 附：为什么不用 langchaingo

同 eino 一起评估过，结论更明确地不用：

| 维度 | langchaingo |
| --- | --- |
| 版本 | `v0.1.15`（更早期，API 更不稳定） |
| HITL | 无内置 interrupt/resume |
| 定位 | Chain/RAG 管道，非 agent 编排 |

它对 A/B/C 三项都没有直接帮助，且版本更早期。

---

## 8. 关于「用哪个模型 SDK」

与编排层选型**独立**。建议：

```go
require (
    github.com/openai/openai-go            v1.12.0  // 官方，v1 稳定
    github.com/anthropics/anthropic-sdk-go v1.84.0  // 官方，v1 稳定
    github.com/modelcontextprotocol/go-sdk v1.8.0   // 官方，v1 稳定
)
```

三者都是 v1 稳定版。换 SDK 只影响 `internal/agent/{openai,anthropic}/adapter.go`，因为 `Runner` 接口把差异隔离了。

---

## 9. 实施记录：阶段 1 已完成（执行生命周期解耦）

### 9.1 做了什么

新增 `internal/executor` 包，把执行任务的生命周期从 HTTP 请求中剥离。

**改动前**（问题所在）：

```go
// internal/api/messages.go
runCtx, cancel := context.WithTimeout(r.Context(), RunTimeout)
//                                       ↑ 客户端断开 → 生成中止
```

**改动后**：

```go
// internal/executor/executor.go
base := context.WithoutCancel(ctx)          // 剥离取消信号
runCtx, cancel := context.WithTimeout(base, timeout)

// 客户端断开只 detach，任务继续
```

### 9.2 行为变化（重要）

| 场景 | 改动前 | 改动后 |
| --- | --- | --- |
| 客户端断开 | 生成中止，消息标记 `interrupted` | **任务继续跑完**，状态 `complete` |
| 显式取消（`POST /cancel`） | 中断 | 中断（不变） |
| 重连 | 部分内容丢失 | 通过 `/stream?last_event_id=N` 补齐 |
| 会话锁 | 请求结束释放 | **任务结束**才释放 |

这一变化已由测试固化：

- `TestClientDisconnectMarksInterrupted` → 改为断言「断开后跑完 + 可重连补齐」
- 新增 `TestCancelStillInterrupts` → 确保显式取消仍立即中断

### 9.3 解决的两个技术难点

**难点 1：`message.start` 与后续事件的顺序竞态**

任务在 `Start` 返回后立即开跑，而 `Subscribe` 稍后才建立，导致：
- `start` 与 `delta` 的 seq 乱序
- 快速失败时 `error` 可能先于 `start` 出现

**解法**：给 `Task` 加启动闸门（`gate`）。执行体开跑前等待 gate，由首个 `Subscribe` 关闭它：

```go
// executor.go
select {
case <-task.gate:    // 等观察者就绪
case <-runCtx.Done():
case <-e.closed:
}
e.run(...)
```

保证「publish(start) → subscribe → 执行体开跑」顺序确定。

**难点 2：`start` 不应被重放**

`start` 是「本次连接握手」，重连时前端已知 `message_id`（历史里能查到）。
若写入缓冲会导致每次重连重复收到。

**解法**：`handleStream` 重放时显式跳过 `EventMessageStart`；`handleSendMessage` 中正常写入缓冲（保证 seq 单调）。

### 9.4 新增能力

```go
type Executor interface {
    Start(ctx, req) (*Task, error)   // 启动任务（生命周期独立）
    Get(sessionID) (*Task, bool)     // 取回运行中任务
    Stop(sessionID) bool             // 显式终止
    Shutdown(ctx) error              // 优雅退出
}

type Task struct { ... }
func (t *Task) Subscribe(after uint64) (replay []Event, live <-chan Event, cancel func(), ok bool)
func (t *Task) Done() <-chan struct{}
func (t *Task) LastSeq() uint64
```

`Subscribe` 可被多次调用（重连、多标签页），`cancel` 只释放订阅**不终止任务**。

### 9.5 验证结果

| 验证项 | 结果 |
| --- | --- |
| `go test -race ./...` | 6 包全绿，无竞态 |
| 实测：断开 2s 后查状态 | `status: complete`，内容 260 字符完整 |
| 实测：重连补齐 | 重放 61 个 delta，`start` 重放数 = 0 |
| `make e2e` | 24 项全部通过 |
| 桌面端打包验证 | 通过 |

### 9.6 下一步（阶段 2）

`Runner` 接口改造：

```go
// 当前
Run(ctx context.Context, history []session.Message) <-chan Event

// 目标
Run(ctx context.Context, req Request) <-chan Event
type Request struct {
    History []session.Message
    Tools   []ToolSpec      // 新增
    Resume  *Checkpoint     // 新增
}
```

影响面：2 处调用点 + 3 个 Runner 实现（Mock / OpenAI / Anthropic）。
executor 已通过 `runnerFn func() agent.Runner` 延迟取 Runner，改造时无需调整 executor。
