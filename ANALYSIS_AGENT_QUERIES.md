# Agent 频繁查询文件原因分析

## 📊 项目现状速览

| 指标 | 数值 |
|------|------|
| Go 源文件 | 44 个 |
| Go 文件总数（含测试） | 92 个 |
| 内部模块数 | 12 个 |
| 设计文档数 | 11 份 |
| 前端文件数 | 6 个 |

---

## 🔴 问题根源分析

### 1. **代码高度耦合，模块间依赖关系复杂**

#### 典型调用链
```
POST /messages (HTTP API)
  ↓
handleSendMessage (api/messages.go)
  ├─ 调用 session.Store (session/store.go)
  ├─ 调用 executor.Executor (executor/executor.go)
  ├─ 订阅 agent.Event (agent/runner.go)
  ├─ 需要 tool.Registry (tool/registry.go)
  ├─ 需要 config.Config (config/config.go)
  └─ 需要 sse.Writer (sse/writer.go)
```

**问题**：一个操作涉及 6-8 个不同的模块文件。Agent 需要：
1. 理解哪些文件需要读取 ❌ 第一次不知道
2. 查询依赖关系 ❌ 需要多次 search/grep
3. 验证跨模块的接口契约 ❌ 需要在多个文件间跳转
4. 找出相关的测试文件 ❌ 需要额外的查询

### 2. **缺乏顶层导航文档**

现有文档虽然详细，但都是**功能维度**的（架构、API、SSE、工具系统等），
没有**代码维度**的导航（"某个功能对应哪些文件"）。

当 Agent 被问"如何处理一个数据请求"时，必须：
- 查询 `api/` 目录 ✓
- Grep `messages` 关键字 ✓
- 打开 `messages.go` ✓
- 读取并理解（发现需要 executor）
- 搜索 executor ✓
- 打开 `executor.go` ✓
- 继续追踪...

这样的链式探索导致大量查询。

### 3. **文件间显式依赖多**

示例：`api/messages.go` 的导入
```go
import (
    "agent_rebuild/internal/agent"
    "agent_rebuild/internal/executor"
    "agent_rebuild/internal/session"
    "agent_rebuild/internal/sse"
    "agent_rebuild/internal/config"
)
```

5 个包的导入意味着 Agent 需要理解这 5 个包才能理解这个文件。
每个包又可能有自己的依赖。

### 4. **业务逻辑复杂，跨越多个技术层**

项目涉及：
- HTTP 路由与中间件 (`api/`)
- 事件流处理 (`sse/`)
- 执行编排 (`executor/` + `agent/loop/`)
- 工具系统 (`tool/`)
- 数据持久化 (`session/` + `storage/sqlite/`)
- 模型配置 (`config/` + `modelconfig/`)

一个"数据请求"需要触及这些所有层级。

### 5. **包结构不够清晰**

- `internal/agent/` - 有 runner.go、llm.go、anthropic.go、loop/loop.go...
  - 模块内部关系不够直观
  - 需要多次查询才能找到正确的入口文件

- `internal/storage/sqlite/` - 有 db.go、session_store.go、checkpoint_store.go...
  - 需要理解哪个是接口，哪个是实现

---

## 📈 查询量统计

### 典型查询模式（处理一个数据请求时）

```
1. 列出 api/ 目录          (list_dir)
2. 搜索 "messages" 关键字   (search) → 3-5 个结果
3. 打开 api/messages.go    (read_file)
4. 理解依赖 → 发现需要 executor
5. 搜索 "executor"          (search)
6. 打开 executor/executor.go (read_file)
7. 理解执行流程 → 发现需要 agent/loop
8. 搜索 "agent loop"        (search)
9. 打开 internal/agent/loop/loop.go (read_file)
10. 理解事件 → 查看 agent/runner.go
11. 搜索工具相关代码       (search)
12. 打开 tool/registry.go   (read_file)
13. 验证某个接口 → 回到 session/store.go
14. ... 循环往复
```

**结果**：20+ 个查询就能完成一个相对简单的问题分析。

---

## ✅ 改进方案

### 方案 A：创建代码导航索引 ⭐⭐⭐⭐⭐ 高优先级

创建 `docs/99-code-index.md`，按功能流程映射文件：

```markdown
## 数据请求处理流程

### 1️⃣ 用户消息入口 (REST API)
- `internal/api/server.go` - 路由注册
- `internal/api/routes.go` - 路由定义
- `internal/api/messages.go` - **POST /messages 处理器** ← 起点

### 2️⃣ 会话与消息模型
- `internal/session/model.go` - 领域模型定义
- `internal/session/store.go` - 存储接口
- `internal/session/memory.go` - 内存实现
- `internal/storage/sqlite/session_store.go` - SQLite 实现

### 3️⃣ 执行编排
- `internal/executor/executor.go` - **执行器主类** ← 核心流程
- `internal/agent/loop/loop.go` - Agent ReAct 循环

### 4️⃣ 模型与工具调用
- `internal/agent/runner.go` - Runner 接口
- `internal/agent/llm.go` - LLM 调用
- `internal/agent/anthropic.go` - Anthropic 适配
- `internal/tool/registry.go` - 工具注册表

### 5️⃣ SSE 流式输出
- `internal/sse/writer.go` - **SSE 写入器**
- `internal/sse/event.go` - 事件定义
- `internal/api/stream.go` - 流处理

### 常见查询 Q&A
**Q: 消息如何从前端送到 LLM？**
A: messages.go → executor.go → agent/loop.go → llm.go

**Q: 工具调用的流程？**
A: executor.go → tool/registry.go → tool/tool.go

**Q: SSE 重连如何工作？**
A: api/messages.go → sse/writer.go → sse/buffer.go
```

**效果**：Agent 第一次就能知道要查哪些文件，减少 50% 的探索性查询。

---

### 方案 B：优化包结构

#### 当前问题
```
internal/agent/
  ├─ runner.go (Runner 接口)
  ├─ llm.go (通用 LLM 逻辑)
  ├─ anthropic.go (Anthropic 适配)
  ├─ mock.go (Mock 实现)
  └─ loop/ (ReAct 循环)
```

**问题**：不清楚哪个是"主入口"，需要多次查询找到执行流程。

#### 改进建议
```
internal/agent/
  ├─ runner.go (Runner 接口 - 不变)
  ├─ executor.go (新文件：Runner 工厂，选择合适的 Runner)
  ├─ providers/
  │  ├─ llm_provider.go (基础 LLM 逻辑)
  │  ├─ anthropic.go (Anthropic 适配)
  │  └─ mock.go (Mock 实现)
  └─ loop/
     └─ loop.go (ReAct 循环)
```

**效果**：结构清晰，Agent 能快速定位"入口"，减少 20-30% 的查询。

---

### 方案 C：编写模块文档头

在每个包的文件中添加详细的包注释，说明职责和关键文件：

```go
// Package executor 负责编排执行流程。
//
// 职责：
//  1. 接收用户消息与会话历史
//  2. 选择合适的 Agent Runner（基于模型配置）
//  3. 启动 Agent 循环，订阅事件
//  4. 管理检查点与人工审批
//
// 核心文件：
//  - executor.go: Executor 接口与主实现
//  - state.go: 执行状态机（如有）
//
// 外部依赖：
//  - agent.Runner: 模型执行引擎
//  - session.Store: 会话数据存储
//  - tool.Registry: 工具注册表
//
// 使用示例：
//
//  ex := executor.New(runner, store, toolReg)
//  events := ex.Execute(ctx, sessionID, userMessage)
//  for evt := range events {
//    // 处理事件
//  }
package executor
```

**效果**：Agent 读取文件时立即获得全景，减少 30-40% 的后续查询。

---

### 方案 D：创建快速参考速查表

创建 `QUICK_REFERENCE.md`：

```markdown
# 快速速查表

## 按文件查（"我在这个文件里"）

| 文件 | 职责 | 看这些其他文件 |
|------|------|----------------|
| `api/messages.go` | 处理 POST /messages | executor/executor.go, sse/writer.go |
| `executor/executor.go` | 执行编排 | agent/runner.go, agent/loop/loop.go |
| `agent/loop/loop.go` | ReAct 循环 | tool/registry.go, tool/tool.go |

## 按功能查（"我想做这个"）

| 功能 | 文件链路 | 主要类型 |
|------|---------|----------|
| 添加新工具 | tool/registry.go → tool/tool.go | Tool interface |
| 添加新模型 | config/providers.go → agent/llm.go | LLMProvider |
| 修改 SSE 行为 | api/messages.go → sse/writer.go | Writer interface |

## 文件依赖关系

```
api/messages.go
├─ executor/executor.go
│  ├─ agent/runner.go
│  │  ├─ agent/loop/loop.go
│  │  │  ├─ tool/registry.go
│  │  │  └─ agent/llm.go
│  │  └─ session/model.go
│  └─ sse/writer.go
└─ session/store.go
```
```

**效果**：Agent 可以一眼找到相关文件，减少 60% 的查询。

---

## 🎯 建议执行顺序

| 优先级 | 方案 | 时间 | 效果 |
|-------|------|------|------|
| 🔴 P0 | 方案 A：代码导航索引 | 1-2h | 减少 50% 查询 |
| 🟠 P1 | 方案 D：快速参考 | 30min | 减少 60% 查询 |
| 🟡 P2 | 方案 C：模块文档头 | 1h | 减少 30-40% 查询 |
| 🟢 P3 | 方案 B：包结构优化 | 2-3h | 减少 20-30% 查询 |

---

## 📋 立即行动

### Step 1：生成文件依赖关系图

```bash
# 可选：用 graphviz 生成依赖图
# go install github.com/KyleBanks/depth/cmd/depth@latest
# depth -dot ./... > deps.dot
```

### Step 2：创建 `docs/99-code-index.md`

基于实际的调用链路，列出"常见查询"及其对应文件。

### Step 3：添加包级注释

在关键模块（executor、agent/loop、sse）的 `.go` 文件顶部加上详细文档。

### Step 4（可选）：重构包结构

如果发现结构混乱，考虑移动文件或新增 package。

---

## 📌 总结

### 为什么查询多？
- ❌ 没有代码导航 → Agent 需要探索
- ❌ 文件高度耦合 → 一个问题牵扯多个文件
- ❌ 缺乏速查表 → Agent 无法快速定位

### 如何改善？
- ✅ 创建导航文档 → Agent 知道要查哪些文件
- ✅ 编写模块文档 → Agent 读文件时获得全景
- ✅ 优化包结构 → 文件关系更清晰

### 预期效果
- 减少 50-60% 的文件查询
- Agent 响应速度提升 3-5 倍
- 新开发者的学习曲线更平缓

