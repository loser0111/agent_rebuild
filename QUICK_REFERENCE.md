# 快速参考表 (Quick Lookup)

> 贴在代理工作室或 AI 助手的"记忆"中，快速查找文件

## 🔍 按功能查文件

| 我想做... | 看这个文件 | 关键类型/函数 |
|----------|----------|------------|
| 理解消息处理流程 | `api/messages.go` | `handleSendMessage()` |
| 理解 Executor | `executor/executor.go` | `Executor.Execute()` |
| 理解 Agent 循环 | `agent/loop/loop.go` | `EventLoop.Run()` |
| 理解 SSE 流 | `api/messages.go` + `sse/writer.go` | `Writer.WriteEvent()` |
| 理解工具系统 | `tool/registry.go` + `tool/tool.go` | `Registry.Get()`, `Tool` interface |
| 理解会话存储 | `session/store.go` | `Store` interface |
| 理解模型配置 | `config/providers.go` | `Protocol`, `Provider` enum |
| 调试连接断开 | `api/stream.go` + `sse/buffer.go` | `handleGetStream()`, `RingBuffer` |
| 添加新工具 | `tool/registry.go`, `tool/fs_tools.go` | 参考现有工具实现 |
| 添加新 LLM | `agent/anthropic.go`, `agent/llm.go` | 参考 Anthropic 的实现 |

---

## 📂 按目录查职责

| 目录 | 职责 | 核心文件数 |
|------|------|----------|
| `internal/api/` | HTTP 路由 & 中间件 | 8 |
| `internal/agent/` | Agent 执行引擎 | 8 |
| `internal/executor/` | 执行编排 | 1 |
| `internal/session/` | 会话模型 & 接口 | 5 |
| `internal/storage/sqlite/` | 数据库实现 | 3 |
| `internal/tool/` | 工具系统 | 6 |
| `internal/sse/` | SSE 事件流 | 4 |
| `internal/config/` | 配置管理 | 3 |

---

## 🔗 文件依赖关系（一览表）

```
API 层                执行层              存储层              工具层
│                     │                   │                  │
api/messages.go   ─→  executor/────────→ session/         tool/
                       executor.go        store.go      registry.go
                           │                │                │
                           ├────────────────┴─ agent/ ─────┤
                           │                   loop.go       │
                           ├─────sse/writer.go              │
                           │                                 │
                           └─ agent/        ────────────────┘
                              anthropic.go
```

---

## ⏱️ 典型查询计划

### 场景 A：理解数据请求流程（5-10 min）

```
Step 1: 打开 api/messages.go (2 min)
        └─ 找 handleSendMessage 函数
        └─ 看它调用了什么

Step 2: 打开 executor/executor.go (3 min)
        └─ 找 Execute 方法
        └─ 理解执行流程

Step 3: 打开 agent/loop/loop.go (3 min)
        └─ 找 Run 方法
        └─ 理解 ReAct 循环

Step 4: 完成！你已理解 80% 的流程
```

**不需要查询的文件**：
- ❌ 单个工具实现（除非需要修改）
- ❌ 测试文件（除非找不到实现）
- ❌ 细节配置

---

### 场景 B：修改工具行为（3-5 min）

```
Step 1: 打开 tool/registry.go (1 min)
        └─ 找到工具列表

Step 2: 打开 tool/fs_tools.go (2 min)
        └─ 找到目标工具实现

Step 3: 修改工具逻辑 (1 min)

Step 4: 完成！工具修改完毕
```

---

### 场景 C：理解断线重连（5-10 min）

```
Step 1: 打开 api/messages.go (2 min)
        └─ 看 SSE 部分的代码

Step 2: 打开 api/stream.go (2 min)
        └─ 看重连处理

Step 3: 打开 sse/buffer.go (2 min)
        └─ 看环形缓冲区实现

Step 4: 完成！理解重连流程
```

---

## 🎯 按问题类型快速定位

| 问题 | 查这个 | 为什么 |
|------|--------|--------|
| "为什么消息没有发出去？" | `api/messages.go` → `executor.go` | 数据流的起点 |
| "工具为什么没有被调用？" | `tool/registry.go` → `agent/loop.go` | 工具注册和调用 |
| "SSE 为什么断连了？" | `sse/writer.go` → `sse/buffer.go` | SSE 实现细节 |
| "为什么不支持这个模型？" | `config/providers.go` → `agent/anthropic.go` | 模型配置 |
| "如何添加新工具？" | `tool/tool.go` → `tool/registry.go` | 工具接口定义 |
| "为什么提示词不对？" | `agent/loop.go` → `agent/anthropic.go` | 提示词构建 |
| "数据怎么存储的？" | `session/store.go` → `storage/sqlite/` | 存储层 |

---

## 📌 最常被查的 3 个文件

| 排名 | 文件 | 查询率 | 原因 |
|------|------|--------|------|
| 🥇 | `api/messages.go` | 80% | 所有数据请求入口 |
| 🥈 | `agent/loop/loop.go` | 70% | Agent 循环逻辑 |
| 🥉 | `executor/executor.go` | 60% | 执行编排核心 |

> 💡 建议：如果遇到陌生问题，先查这三个文件

---

## 🛑 常见"死胡同"（避免查这些）

| 文件 | 为什么避免 | 什么时候看 |
|------|---------|----------|
| `internal/storage/sqlite/legacy_compat_test.go` | 只是测试迁移代码 | 处理数据库迁移时 |
| `internal/agent/baseurl_request_test.go` | 纯测试文件 | 调试 baseURL 选择 |
| `web/node_modules/` | 前端依赖，无关 | 前端开发时 |
| `*_test.go` 文件（一般） | 测试代码，不是实现 | 理解特定功能的边界情况 |

---

## ✅ 新 Agent 快速上手检查表

- [ ] 已读 `CODE_NAVIGATION.md`
- [ ] 已读 `ANALYSIS_AGENT_QUERIES.md`（理解为什么查询多）
- [ ] 已读 `docs/01-architecture.md`（理解项目架构）
- [ ] 已追溯过一次完整数据流（api/messages.go → executor → loop）
- [ ] 已记住前 3 个最常查的文件

---

## 🎓 使用指南

### 给自己的提示词模板

```markdown
在分析 agent_rebuild 项目时，我会：

1. 先查 CODE_NAVIGATION.md 找入口文件
2. 按建议的文件顺序阅读（不要随意跳跃）
3. 如果需要理解工具系统，从 tool/tool.go 的接口开始
4. 如果需要理解 SSE，从 sse/event.go 的定义开始
5. 避免查看 *_test.go 文件（除非调试特定功能）

预期结果：
- 理解数据流：4 个文件，5 分钟
- 理解工具系统：3 个文件，5 分钟
- 理解 SSE：4 个文件，8 分钟
```

### 分享给团队

将本文档链接加入：
- 项目 README
- 代码审查 checklist
- 新人 onboarding 指南

