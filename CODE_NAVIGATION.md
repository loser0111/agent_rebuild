# 代码导航指南

## 📍 项目概览速查

### 一句话架构
浏览器 → Go HTTP API → Executor → Agent Runner → LLM

### 核心数据流
```
User Input → POST /messages → handleSendMessage → Executor → Agent Loop → Tool Call → Result → SSE Event → Browser
```

---

## 🗂️ 模块职责与文件映射

### 1. HTTP API 层 (`internal/api/`)

**职责**：处理 HTTP 请求，路由到业务逻辑，返回 JSON/SSE 响应

| 文件 | 内容 | 关键函数/类型 |
|------|------|-------------|
| `server.go` | 服务器初始化、中间件 | `Server`、`NewServer` |
| `routes.go` | 路由定义 | 所有 HTTP 路由 |
| `sessions.go` | 会话 CRUD API | `handleGetSessions`、`handleDeleteSession` |
| `messages.go` | **POST /messages 处理** ⭐ | `handleSendMessage` → **数据请求入口** |
| `stream.go` | GET /stream 补齐 | `handleGetStream` |
| `toolpersist_e2e_test.go` / `messages_test.go` | API 测试 | 集成测试 |

**常见问题**：
- "消息如何处理？" → 看 `messages.go:handleSendMessage`
- "为什么有两个 SSE 端点？" → `messages.go` 是主流程，`stream.go` 是重连补齐
- "中间件在哪？" → `server.go:setupMiddleware`

**跳转关系**：
```
messages.go 
├─→ executor/executor.go (执行消息)
├─→ session/store.go (存储消息)
└─→ sse/writer.go (发送 SSE)
```

---

### 2. 执行编排层 (`internal/executor/`)

**职责**：协调 Agent 执行流程，管理检查点，处理工具调用

| 文件 | 内容 | 关键类型 |
|------|------|---------|
| `executor.go` | **Executor 主类** ⭐ | `Executor`、`Execute()` |

**核心逻辑**：
```go
// 伪代码
executor.Execute(ctx, sessionID, userMsg) {
  1. 从 Store 读取会话历史
  2. 创建 Agent Runner（选择模型）
  3. 启动 Agent 循环
  4. 订阅事件，发送给前端
  5. 处理工具调用、审批、人工输入
}
```

**跳转关系**：
```
executor.go
├─→ agent/runner.go (运行 Agent)
├─→ agent/loop/loop.go (ReAct 循环)
├─→ session/store.go (读写会话)
├─→ tool/registry.go (工具查询)
└─→ storage/sqlite/checkpoint_store.go (检查点)
```

---

### 3. Agent 执行层 (`internal/agent/`)

**职责**：与 LLM 交互，实现 ReAct 循环

#### 核心架构
```
Runner (接口) ← 工厂
├─ LLMRunner (真实模型)
│  └─ Anthropic / OpenAI 适配
├─ MockRunner (演示/测试)
└─ AgentLoop (ReAct 流程)
```

| 文件 | 内容 | 关键类型 |
|------|------|---------|
| `runner.go` | Runner 接口 | `Runner` interface |
| `loop/loop.go` | **Agent ReAct 循环** ⭐ | `EventLoop` |
| `llm.go` | LLM 调用通用逻辑 | `LLMRunner`、`CreateChatRequest` |
| `anthropic.go` | Anthropic 专用适配 | `AnthropicRunner` |
| `mock.go` | Mock 实现 | `MockRunner` |
| `baseurl.go` | 动态 baseURL 选择 | `BuildBaseURL` |

**常见问题**：
- "如何添加新的 LLM 提供商？" → 看 `anthropic.go` 的实现，参考 `llm.go`
- "Agent 循环如何工作？" → `loop/loop.go:EventLoop.Run()`
- "工具调用在哪处理？" → `loop/loop.go` 的 tool call 分支

**跳转关系**：
```
executor.go
└─→ agent/runner.go (选择 Runner)
    ├─→ agent/llm.go (LLM 通用)
    ├─→ agent/anthropic.go (Anthropic 适配)
    └─→ agent/loop/loop.go (ReAct 循环)
        ├─→ tool/registry.go (查询工具)
        └─→ tool/tool.go (执行工具)
```

---

### 4. 会话与存储层

#### 4.1 会话模型 (`internal/session/`)

**职责**：定义会话/消息领域模型，存储接口

| 文件 | 内容 |
|------|------|
| `model.go` | 结构体定义：`Session`、`Message`、`ToolCall` 等 |
| `store.go` | **Store 接口** ⭐（CRUD + AppendMessage） |
| `memory.go` | 内存实现（开发/测试用） |
| `checkpoint.go` | 检查点（用于恢复） |

#### 4.2 SQLite 存储 (`internal/storage/sqlite/`)

**职责**：会话数据持久化

| 文件 | 内容 |
|------|------|
| `db.go` | 数据库初始化 |
| `session_store.go` | Store 接口的 SQLite 实现 |
| `checkpoint_store.go` | 检查点持久化 |

**跳转关系**：
```
executor.go
└─→ session/store.go (使用接口)
    ├─→ session/memory.go (内存实现，开发时)
    └─→ storage/sqlite/session_store.go (生产时)
```

---

### 5. 工具系统 (`internal/tool/`)

**职责**：注册、验证、执行工具（read_file、search 等）

| 文件 | 内容 |
|------|------|
| `registry.go` | **工具注册表** ⭐ |
| `tool.go` | Tool 接口 |
| `fs_tools.go` | 文件系统工具（read/write/list） |
| `command_tool.go` | 命令执行工具 |
| `ask_user.go` | 人工输入工具 |

**跳转关系**：
```
agent/loop/loop.go
└─→ tool/registry.go (查询工具)
    └─→ tool/*.go (各工具实现)
```

---

### 6. SSE 事件流 (`internal/sse/`)

**职责**：Server-Sent Events 处理，事件定义，重连补齐

| 文件 | 内容 |
|------|------|
| `event.go` | 事件定义：`Event`、`EventType` |
| `writer.go` | **SSE 写入器** ⭐（flush、ping、重连） |
| `buffer.go` | 环形缓冲区（用于 Last-Event-ID 重放） |
| `hub.go` | 事件分发 hub（可选） |

**跳转关系**：
```
api/messages.go
└─→ sse/writer.go (写 SSE)
    └─→ sse/buffer.go (缓存事件，重连时重放)
```

---

### 7. 配置层 (`internal/config/`)

**职责**：环境变量、LLM 提供商配置

| 文件 | 内容 |
|------|------|
| `config.go` | 总配置 |
| `providers.go` | LLM 提供商枚举 |
| `dotenv.go` | .env 解析 |

---

## 🚀 常见操作的文件导航

### 场景 1️⃣：用户发送消息，我想理解整个流程

**入口文件**：`api/messages.go`

```
1. api/messages.go:handleSendMessage()
   ├─ 验证会话、检查权限
   ├─ 调用 executor.Execute()
   └─ 返回 SSE 流

2. executor/executor.go:Execute()
   ├─ 读取会话历史 (session.Store)
   ├─ 选择 Agent Runner
   ├─ 启动 Agent 循环
   └─ 订阅事件，发给前端

3. agent/loop/loop.go:Run()
   ├─ 把历史发给 LLM
   ├─ 处理 LLM 响应
   └─ 如果需要工具调用：
      ├─ 查询工具 (tool/registry.go)
      ├─ 执行工具 (tool/*.go)
      └─ 把结果回喂给 LLM

4. 循环直到结束，发送 TypeDone 事件
```

**需要阅读的文件**（按顺序）：
1. `api/messages.go` (50 行理解入口)
2. `executor/executor.go` (看 Execute 方法)
3. `agent/loop/loop.go` (看 Run 方法)
4. `tool/registry.go` (看工具调用部分)

**预期查询次数**：4-5 次

---

### 场景 2️⃣：我想添加一个新工具（如 search）

**入口文件**：`tool/registry.go`

```
1. tool/registry.go
   └─ 找到 BuiltinTools 定义
   └─ 看已有工具的模式

2. tool/fs_tools.go
   └─ 参考其他工具的实现

3. 实现新工具
   ├─ 创建 struct 实现 Tool interface
   ├─ 实现 Call() 方法
   └─ 在 registry.go 中注册

4. agent/loop/loop.go
   └─ 看工具调用流程，确保兼容
```

**需要阅读的文件**：
1. `tool/registry.go` (看注册模式)
2. `tool/fs_tools.go` (参考已有实现)
3. `tool/tool.go` (看接口定义)

**预期查询次数**：3-4 次

---

### 场景 3️⃣：SSE 连接断开，我想理解重连流程

**入口文件**：`api/messages.go` + `api/stream.go`

```
1. api/messages.go:handleSendMessage()
   └─ 主 SSE 流程
   └─ 返回 Last-Event-ID 给客户端

2. api/stream.go:handleGetStream()
   └─ 客户端重连端点
   └─ 使用 Last-Event-ID 从缓冲区重放

3. sse/buffer.go
   └─ 环形缓冲区实现
   └─ 按 ID 查找事件
```

**需要阅读的文件**：
1. `api/messages.go` (看 SSE 部分)
2. `api/stream.go` (看重连逻辑)
3. `sse/writer.go` (看事件写入)
4. `sse/buffer.go` (看重放逻辑)

**预期查询次数**：4-5 次

---

## 📊 文件复杂度参考

| 文件 | 行数 | 复杂度 | 学习曲线 |
|------|------|--------|---------|
| `api/messages.go` | ~200 | 🟡 中等 | 需要理解 SSE + Executor |
| `executor/executor.go` | ~150 | 🟡 中等 | 需要理解事件模型 |
| `agent/loop/loop.go` | ~300 | 🔴 高 | 需要理解 ReAct + LLM 协议 |
| `tool/tool.go` | ~50 | 🟢 低 | 简单接口定义 |
| `sse/writer.go` | ~100 | 🟡 中等 | 需要理解 HTTP 流 |

---

## 💡 快速查找技巧

### "我想找 XXX 功能的代码"

| 功能 | 快速查找 |
|------|---------|
| SSE 事件定义 | `grep -r "EventType =" internal/sse` |
| 工具列表 | `grep -r "BuiltinTools" internal/tool` |
| API 路由 | `grep -r "HandleFunc" internal/api` |
| 测试用例 | 找 `*_test.go` 文件 |

### "这个类型在哪定义？"

| 类型 | 位置 |
|------|------|
| `Session` | `session/model.go` |
| `Message` | `session/model.go` |
| `Event` | `sse/event.go` 或 `agent/runner.go` |
| `ToolCall` | `session/model.go` |
| `Runner` | `agent/runner.go` |

---

## 🎯 新人上手路径

1. **了解架构** (10 min)
   - 读 `docs/01-architecture.md`

2. **理解一次数据流** (30 min)
   - 按上面"场景 1️⃣"的文件顺序阅读
   - 关注 struct 定义和函数签名

3. **跑通 demo** (20 min)
   - `make demo` 启动
   - 在浏览器发一条消息
   - 用 VS Code 调试，设断点在 `messages.go`

4. **修改一个小功能** (1-2 h)
   - 比如修改工具名称
   - 按照"场景 2️⃣"找到相关文件
   - 修改并测试

---

## 📝 使用建议

> **给 Claude Agent 的提示**
>
> 当你需要分析 agent_rebuild 项目时，先参考这份导航指南：
> 1. 明确你要分析的"功能"或"场景"
> 2. 参考上面的"常见操作"找到入口文件
> 3. 按照建议的文件顺序阅读
> 4. 这样可以减少 50% 的文件查询

