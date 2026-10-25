# 01 · 整体架构与技术选型

> 项目：agent_rebuild —— 前后端分离的 Agent Demo
> 版本：v0.1（设计基线，后续实现以此文档为准）

---

## 1. 目标与范围

| 目标 | 说明 |
| --- | --- |
| 前后端分离 | Go 后端只提供 JSON + SSE 接口，前端为独立 Vite 工程，可单独构建部署 |
| Codex 风格 UI | 深色、左侧会话栏 + 右侧对话流、等宽代码块、工具/思考过程可折叠 |
| 流式输出 | 基于 SSE（`text/event-stream`），逐 token 增量渲染，支持断线重连与补齐 |
| 会话管理 | 会话创建/列表/详情（含历史消息）/删除，服务端持有完整上下文 |
| 可跑起来的 Demo | 无 API Key 时也能用 Mock Agent 跑通全链路 |

不在本期范围：多用户鉴权、多租户隔离、工具沙箱执行、向量记忆、计费。这些在接口层预留扩展位（见 §7）。

---

## 2. 整体架构

```
┌──────────────────────────────────────────────────────────────┐
│  Browser (Vite + React + TS)                                 │
│                                                              │
│  ┌────────────┐   ┌──────────────┐   ┌────────────────────┐   │
│  │ SessionBar │   │ ChatView     │   │ SSE Client         │   │
│  │ (列表/新建)│   │ (消息流/输入)│   │ fetch+ReadableStream│  │
│  └─────┬──────┘   └──────┬───────┘   └─────────┬──────────┘   │
└────────┼─────────────────┼─────────────────────┼──────────────┘
         │ REST/JSON       │ REST/JSON           │ SSE 流
         ▼                 ▼                     ▼
┌──────────────────────────────────────────────────────────────┐
│  Go HTTP Server (cmd/server)                                 │
│                                                              │
│  internal/api                                                │
│   ├─ Router (net/http ServeMux, method 级路由)                │
│   ├─ Middleware: RequestID / Logger / Recover / CORS / Timeout│
│   ├─ SessionHandler  (CRUD)                                  │
│   └─ MessageHandler  (POST messages → 直接返回 SSE)           │
│                                                              │
│  internal/sse                                                │
│   ├─ Event 结构 + 编码器 (id/event/data)                      │
│   ├─ Writer  (flush + 心跳 ping + 断连检测)                   │
│   └─ RingBuffer (每会话最近 N 条事件，用于 Last-Event-ID 重放) │
│                                                              │
│  internal/session                                            │
│   ├─ Model: Session / Message                                │
│   ├─ Store 接口 (CRUD + AppendMessage)                        │
│   └─ MemoryStore (map + RWMutex)  ← 可替换 SQLite / Redis     │
│                                                              │
│  internal/agent                                              │
│   ├─ Runner 接口: Run(ctx, history) <-chan agent.Event        │
│   ├─ LLMRunner  (OpenAI 兼容 /chat/completions, stream=true)  │
│   ├─ MockRunner (无 Key 时确定性假流，用于 Demo/单测)          │
│   └─ SessionLock: 同一会话串行执行                            │
└───────────────────────────┬──────────────────────────────────┘
                            │ HTTPS + SSE (OpenAI 兼容协议)
                            ▼
                  ┌───────────────────────────┐
                  │  LLM Provider (外部)       │
                  │  OpenAI / 兼容网关 / 本地  │
                  └───────────────────────────┘
```

### 2.1 一次对话的完整时序

```
Browser                Go Server                   Agent Runner          LLM
   │  POST /messages       │                            │                 │
   ├──────────────────────►│ 1. 校验会话 & 占用锁         │                 │
   │                       │ 2. 落库 user message        │                 │
   │                       │ 3. 建 assistant 占位消息     │                 │
   │  200 + text/event-stream                            │                 │
   │◄──────────────────────┤ 4. 立即 flush 响应头         │                 │
   │                       ├─ 5. go Run(ctx, history) ──►│                 │
   │                       │                            ├────────────────►│
   │  event: message.start │ 6. 订阅事件 channel          │                 │
   │◄──────────────────────┤                            │◄──── chunk ─────┤
   │  event: message.delta │◄─── delta ─────────────────┤                 │
   │◄──────────────────────┤ 7. 同步累积到 assistant 内容 │                 │
   │  event: message.delta │◄─── delta ─────────────────┤                 │
   │◄──────────────────────┤                            │                 │
   │  event: message.done  │◄─── done ──────────────────┤                 │
   │◄──────────────────────┤ 8. 落库 assistant 最终态     │                 │
   │  （连接关闭 / 客户端复用）│ 9. 释放会话锁                │                 │
```

关键点：**user message 落库 + assistant 占位消息在返回 SSE 响应头之前完成**，这样即使流在中途断开，会话详情里也能看到「已提问」「回答不完整」的真实状态。

---

## 3. 技术选型

### 3.1 后端

| 维度 | 选择 | 理由 |
| --- | --- | --- |
| 语言 | **Go 1.22+**（最低 1.21） | 官方 `ServeMux` 自 1.22 起支持 `"POST /api/sessions/{id}"` 形式的 method+path 模式，路径参数 `r.PathValue("id")`，无需第三方路由；`http.ResponseController`（1.20+）可安全 flush 与做写超时。若环境锁定 1.21，则退化为 `chi/v5`（接口不变） |
| HTTP 框架 | **标准库 net/http** | SSE 只需「写、flush、监听 ctx」，框架无增益；依赖越少越稳 |
| 路由 | `http.ServeMux`（method 模式） | 零依赖，语义清晰 |
| SSE | **标准库手写编码器**（`internal/sse`） | 事件类型、心跳、重放需要精细控制，第三方库（如 r3labs/sse）反而受限 |
| 并发模型 | goroutine + channel + `context.Context` | 每个请求一个 goroutine；runner 事件经 channel 回传给 writer |
| 配置 | 环境变量 + `.env.example` | 12-factor，Demo 免配置即可跑（Mock 模式） |
| 持久化 | **Store 接口 + MemoryStore** 先行 | 单进程 Demo 够用；接口签名按可持久化设计，后续换 SQLite/Redis 零改动业务代码 |
| 日志 | `log/slog`（JSON handler） | 结构化，与 request_id / session_id 关联 |
| 测试 | `go test` + `httptest` | SSE 用 `httptest.NewServer` + 逐行读 body 断言 |
| LLM SDK | **不引 SDK，直接 `net/http` 调 OpenAI 兼容 `/v1/chat/completions`（stream=true）** | 兼容任何兼容层（OpenAI、DeepSeek、vLLM、Ollama），无版本耦合 |

### 3.2 前端

| 维度 | 选择 | 理由 |
| --- | --- | --- |
| 构建 | **Vite 5 + TypeScript 5** | 秒级 HMR；`/api` 走 dev proxy 到 Go，无跨域问题 |
| 框架 | **React 18**（函数组件 + hooks） | 生态最全，Codex 风格组件（虚拟列表、Markdown）现成 |
| 样式 | **Tailwind CSS + CSS 变量主题** | 深色 token 化（背景 `#0d0d0d`、面板 `#171717`、边框 `#2a2a2a`、强调色自定义），快速贴合 Codex 观感 |
| 状态管理 | React Context + `useReducer`（不引 Redux） | 会话列表 + 当前会话 + 流式缓冲区，规模可控 |
| 数据请求 | 原生 `fetch`（封装 `api.ts`） | 无 axios 必要 |
| SSE 客户端 | **`fetch` + `ReadableStream` 手写解析器** | `EventSource` 不支持 POST body、不支持自定义 header（如 `Last-Event-ID` 之外的鉴权头）、不能取消；手写约 80 行即可完整支持 `id/event/data/retry` |
| Markdown | `react-markdown` + `remark-gfm` | 代码块/表格/列表；代码块用 `highlight.js` 或 `shiki` 轻量高亮 |
| 字体 | UI 用 Inter/系统栈，对话与代码用 `ui-monospace / JetBrains Mono` | Codex 观感的核心 |

### 3.3 开发与运行

| 项 | 方式 |
| --- | --- |
| 本地开发 | 两个进程：`go run ./cmd/server`（:8080）+ `pnpm dev`（:5173，proxy `/api`） |
| 生产形态 | `pnpm build` → `web/dist` → Go 用 `embed.FS` 或静态目录托管，单二进制交付 |
| Mock 模式 | `LLM_PROVIDER=mock`（默认）→ `MockRunner` 按固定节奏吐词，Demo 无需 Key |
| 真实模式 | `LLM_PROVIDER=openai` + `OPENAI_API_KEY` / `OPENAI_BASE_URL` / `LLM_MODEL` |

---

## 4. 目录结构约定

```
agent_rebuild/
├── cmd/
│   └── server/
│       └── main.go              # 装配：config → store → agent → router → ListenAndServe
├── internal/
│   ├── api/
│   │   ├── router.go            # ServeMux 注册所有路由 + 中间件链
│   │   ├── middleware.go        # RequestID / Logger / Recover / CORS / 超时
│   │   ├── sessions.go          # 会话 CRUD handler
│   │   ├── messages.go          # POST /messages（SSE 主流程）+ cancel
│   │   ├── stream.go            # GET /stream（重连补齐）
│   │   ├── errors.go            # 统一错误体 {code,message,request_id}
│   │   └── respond.go           # writeJSON / writeError 助手
│   ├── sse/
│   │   ├── event.go             # Event 类型、事件名常量、JSON 负载结构
│   │   ├── writer.go            # Writer：Write/Flush/Ping/Close，断连处理
│   │   └── buffer.go            # RingBuffer：seq → Event，支持 Last-Event-ID 重放
│   ├── session/
│   │   ├── model.go             # Session / Message / Role / Status
│   │   ├── store.go             # Store 接口 + 错误定义（ErrNotFound ...）
│   │   ├── memory.go            # MemoryStore（map + RWMutex + 环形消息裁剪）
│   │   └── id.go                # ID 生成（时间前缀 + 随机后缀，可换 ULID）
│   ├── agent/
│   │   ├── runner.go            # Runner 接口 + Event（Type/Delta/Done/Err）
│   │   ├── llm.go               # OpenAI 兼容流式实现（SSE 解析）
│   │   ├── mock.go              # 确定性 MockRunner
│   │   ├── lock.go              # 会话级串行锁
│   │   └── prompt.go            # history → messages 转换 / 系统提示
│   └── config/
│       └── config.go            # 环境变量解析 + 校验 + 默认值
├── web/
│   ├── index.html
│   ├── package.json
│   ├── vite.config.ts           # server.proxy['/api'] → http://localhost:8080
│   ├── tailwind.config.ts
│   ├── tsconfig.json
│   └── src/
│       ├── main.tsx
│       ├── App.tsx              # 三栏布局骨架
│       ├── api/
│       │   ├── client.ts        # fetch 封装 + 统一错误
│       │   ├── types.ts         # 与后端 JSON 对齐的类型定义
│       │   └── sse.ts           # 手写 SSE 解析器 + useStream hook
│       ├── state/
│       │   └── chatStore.tsx    # Context + reducer：sessions / messages / streaming
│       ├── components/
│       │   ├── Sidebar.tsx      # 会话列表、新建、删除、重命名
│       │   ├── ChatView.tsx     # 消息流 + 自动滚动 + 空态
│       │   ├── MessageItem.tsx  # 用户/助手气泡、Markdown、复制、时间
│       │   ├── Composer.tsx     # 输入框（Enter 发送 / Shift+Enter 换行 / 停止按钮）
│       │   ├── StatusBar.tsx    # 连接状态 / 模型名 / token 用量
│       │   └── ThemeToggle.tsx
│       └── styles/
│           └── global.css       # Tailwind 指令 + 主题变量
├── docs/
│   ├── 01-architecture.md       # 本文
│   ├── 02-api-and-sse.md        # REST + SSE 契约
│   └── 03-runtime-and-storage.md# 存储、并发、超时与取消
├── Makefile                     # dev / build / test / run
├── .env.example
├── go.mod
└── web/…                        # 见上
```

依赖方向（禁止反向引用）：`cmd/server` → `api` → `session` / `agent` / `sse`；`agent` 不感知 HTTP；`session` 不感知 SSE。

---

## 5. 核心接口（Go 层）

```go
// session.Store —— 可插拔持久化边界
type Store interface {
    Create(ctx context.Context, s *Session) error
    Get(ctx context.Context, id string) (*Session, error)
    List(ctx context.Context, limit int, cursor string) ([]*Session, string, error)
    Update(ctx context.Context, id string, fn func(*Session) error) (*Session, error)
    Delete(ctx context.Context, id string) error

    AppendMessage(ctx context.Context, msg *Message) error
    UpdateMessage(ctx context.Context, sessionID, msgID string, fn func(*Message) error) error
    Messages(ctx context.Context, sessionID string) ([]*Message, error)
}

// agent.Runner —— 执行层边界，HTTP 无关
type Runner interface {
    // Run 把历史消息交给模型，边跑边发出增量事件；返回的 channel 由调用方消费
    Run(ctx context.Context, history []session.Message) <-chan Event
}

type Event struct {
    Type    EventType // Delta | Done | Error | Tool
    Delta   string
    Content string        // Done 时的完整文本
    Usage   *Usage
    Err     error
}
```

`Runner` 只吐「文本增量」，SSE 事件编号、落库、心跳全部在 `api` 层完成——这样换执行引擎（自己实现 tool loop、多模型路由）不影响协议。

---

## 6. 非功能性设计

| 关注点 | 设计 |
| --- | --- |
| 首字延迟 | 收到 POST 后立即写响应头 + 首帧 flush；无 Key 的 Mock 模式首 token < 50ms |
| 吞吐 | 每会话同时只允许 1 个运行（锁），不同会话天然并行 |
| 内存 | 单会话消息上限 `MAX_MESSAGES_PER_SESSION`（默认 200，超出裁剪最旧）；事件环形缓冲 1000 条/会话 |
| 幂等 | `POST /messages` 支持可选 `client_message_id`，重复提交直接返回已有结果（防双击） |
| 可观测 | 每请求 `X-Request-Id`；日志带 `session_id`/`message_id`；`/api/health` 暴露版本与 provider |
| 安全 | CORS 白名单（dev 只放开 :5173）；错误体不回传上游原始 payload/密钥；请求体大小限制 1MB |
| 优雅退出 | `Server.Shutdown(ctx)` → 触发所有运行中 run 的 ctx cancel → 等待 `WaitGroup` 落库收尾 |

---

## 7. 扩展位（预留，不实现）

- **鉴权**：`internal/api/middleware.go` 预留 `AuthMiddleware`，会话表加 `owner_id` 即多用户。
- **工具调用**：`agent.Event` 已有 `Tool` 类型；协议层预留 `tool.call` / `tool.result` 事件。
- **多模型**：`POST /messages` 已支持 `model` 覆盖；`Runner` 可为路由器实现。
- **持久化**：换 `sqlite.go` / `redis.go` 实现 `Store` 即可，`STORE_DRIVER` 切换。
- **分布式**：多实例时把会话事件总线换 Redis Stream，SSE 消费侧变订阅者。
