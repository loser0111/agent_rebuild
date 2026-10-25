# 03 · 会话持久化、并发、超时与取消

---

## 1. 会话持久化方案

### 1.1 演进路线

| 阶段 | 实现 | 适用 | 代价 |
| --- | --- | --- | --- |
| **P0（本期）** | `session.MemoryStore`：`map[string]*Session` + `sync.RWMutex` | 单进程 Demo、本地开发 | 重启丢数据（可接受，Demo 明确声明） |
| P1 | `session.SQLiteStore`：`modernc.org/sqlite`（纯 Go，免 CGO） | 单机持久化 | 加一个依赖 + schema 迁移 |
| P2 | `session.RedisStore`：会话 hash + 消息 list + event stream | 多实例、多副本 | 序列化 + 过期策略 |
| P3 | Postgres + 事件溯源 | 生产多租户 | 复杂度高 |

切换方式：`STORE_DRIVER=memory|sqlite|redis`，`main.go` 里一个 `switch` 构造 `session.Store`。**业务代码（api / agent）只依赖接口**，不改。

### 1.2 Store 接口（契约）

```go
type Store interface {
    Create(ctx context.Context, s *Session) error
    Get(ctx context.Context, id string) (*Session, error)          // 深拷贝返回，防外部改内部
    List(ctx context.Context, opts ListOptions) ([]*Session, string, error)
    Update(ctx context.Context, id string, fn func(*Session) error) (*Session, error) // 读改写原子化
    Delete(ctx context.Context, id string) error

    AppendMessage(ctx context.Context, m *Message) error
    UpdateMessage(ctx context.Context, sessionID, msgID string, fn func(*Message) error) error
    Messages(ctx context.Context, sessionID string) ([]Message, error)
}
```

设计要点：

- **返回深拷贝**，避免调用方持有内部指针造成数据竞争。
- **`Update` 接受闭包**：内存实现持写锁执行，SQLite/Redis 实现可在事务/`WATCH` 内执行，语义一致。
- **`ctx` 贯穿**：SQLite/Redis 实现才能真正受益（Demo 内存实现也照传，保持一致）。
- `List` 用 `cursor` 而非 `offset`，为后续大数据量准备（内存实现用「最后一条 updated_at + id」编码）。

### 1.3 内存实现细节

```go
type MemoryStore struct {
    mu       sync.RWMutex
    sessions map[string]*Session
    messages map[string][]Message       // sessionID -> 消息（按时间正序）
    maxMsgs  int                        // 默认 200，超出从头裁剪
}
```

- 只会被 `Create` 之外的并发读访问，用 `RWMutex`；`AppendMessage`/`Update` 走写锁。
- 裁剪策略：会话消息数超 `MAX_MESSAGES_PER_SESSION` 时丢弃最旧的（保留系统提示，若实现 system 消息则不计入裁剪窗口）。
- 内存估算：单条消息平均 1KB × 200 × 100 会话 ≈ 20MB，可接受；`/api/metrics` 预留打印功能。

### 1.4 数据一致性规则

1. **消息落库先于事件推送**：`message.start` 发出前，assistant 占位消息必须已 `AppendMessage`；`message.done` 发出前必须已 `UpdateMessage(status=complete, content=全文)`。这样「前端看到的 ⊆ 服务端持久化的」。
2. 流中断时（客户端 abort / 超时 / 进程退出），已累积的部分内容以 `status=interrupted` 落库，`content` 保留已生成部分，前端可展示「已中断」标记并允许重试。
3. `Session.updated_at` 与 `message_count` 在每次消息落库时同步更新（同一把锁内，避免不一致）。

---

## 2. 并发模型

### 2.1 goroutine 拓扑（单个对话请求）

```
HTTP handler goroutine
  ├─ 校验会话、占用会话锁（非阻塞 TryLock，失败 → 409 session_busy）
  ├─ 落库 user message + assistant 占位
  ├─ 写 SSE 响应头，起 ticker(15s) 用于 ping
  ├─ go runner.Run(ctx, history) → 产出 channel
  └─ for-select 主循环：
        case ev := <-events:    编码为 SSE → Write → Flush（每个 delta 都 flush）
        case <-ticker.C:        写 ping 事件
        case <-r.Context().Done(): 结束（客户端断开）
        case <-ctx.Done():      结束（服务端取消/超时）
```

- **每个 delta 一次 Flush**：这是 SSE 低延迟的关键；Go 的 `http.Flusher`（或 `http.NewResponseController(w).Flush()`）阻塞代价极低。
- **写失败即退出**：`Write` 返回 error 说明连接已断，立即 cancel ctx 停止上游 LLM 调用，避免空转烧 token。

### 2.2 会话级串行

```go
type Locks struct{ m sync.Map } // sessionID -> *sync.Mutex
```

- 同一会话不允许并行生成：进入时 `TryLock`，失败返回 `409 session_busy`。**不要阻塞等待**——用户能看到明确反馈，而不是转圈。
- 不同会话完全并行，无全局锁。
- 会话删除 / 进程退出时释放对应锁；`Locks` 用 `sync.Map` 惰性创建，删除会话时 `Delete` 条目。

### 2.3 数据竞争防范

- 所有 store 访问经接口 + 内部锁，**不允许** handler 持有 `*Session` 指针跨 goroutine。
- 流式累积文本用 handler 局部变量（单 goroutine 独占），只在 done/interrupted 时一次性写库。
- 事件广播（`/stream` 的 fan-out）用每订阅者独立 buffered channel（容量 256）；慢订阅者 channel 满时**丢弃并标记 resync**，绝不在写端阻塞——避免一个卡死的客户端拖垮整个会话。

### 2.4 死锁/泄漏检查清单（实现时逐条过）

- [ ] 每个 `go func` 都有明确退出条件（ctx.Done 或 channel close）
- [ ] runner 的 channel 由 runner close，handler 只读
- [ ] `Locks` 的 unlock 用 defer，且仅在 TryLock 成功后 defer
- [ ] ticker 必须 `defer ticker.Stop()`
- [ ] 请求级 ctx 与运行级 ctx 分离：`runCtx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))` 仅在需要「客户端断开但后台继续」时使用；Demo 采用「客户端断开即取消」的简单语义

---

## 3. 超时与取消

### 3.1 超时层级

| 层级 | 默认值 | 配置项 | 行为 |
| --- | --- | --- | --- |
| 请求体读取 | 10s | `HTTP_READ_TIMEOUT` | 超时回 408/400 |
| 响应头写出 | 无（SSE 长连接） | — | `Server.WriteTimeout` **必须为 0**，否则长流会被切断 |
| 单次生成总时长 | 180s | `RUN_TIMEOUT` | 超时 cancel，发 `message.interrupted`，落库 `interrupted` |
| 上游 LLM 单次 HTTP | 120s | `LLM_TIMEOUT` | 由 `http.Client.Timeout` + ctx 双重约束 |
| 首 token 超时 | 30s | `LLM_FIRST_TOKEN_TIMEOUT` | 未收到首个 delta → 报 `provider_unavailable` |
| 心跳间隔 | 15s | `SSE_PING_INTERVAL` | 发 `event: ping`（防中间层 60s 空闲断开） |
| 优雅退出 | 10s | — | `Shutdown(ctx)` 等待收尾 |

**关键**：`http.Server.WriteTimeout` 与 `IdleTimeout` 不能按常规设置；用 `ReadHeaderTimeout: 10s` + `ReadTimeout: 30s` + `WriteTimeout: 0` + `IdleTimeout: 120s`。

### 3.2 context 传递链

```
r.Context()                       // 客户端连接生命周期（断开即 Done）
   └─ runCtx (WithTimeout 180s)   // 单次生成上限
        └─ llmCtx (WithTimeout 120s) // 上游调用上限
```

规则：

- **不做 `context.Background()` 逃逸**：Demo 语义是「客户端断开 = 放弃生成」。
- 上游 HTTP 请求一律 `http.NewRequestWithContext(llmCtx, ...)`，保证 cancel 能中断正在读的响应体。
- 读上游 SSE body 时用 `bufio.Scanner` + 每行检查 `ctx.Err()`，并设置 `scanner.Buffer` 上限（单行 1MB），防恶意超长行。

### 3.3 客户端断开处理

```go
case <-r.Context().Done():
    // 1. cancel runCtx（中断上游）
    // 2. 已累积内容以 status=interrupted 落库
    // 3. 记日志：session_id, message_id, bytes_written, err=context.Canceled
    // 4. 释放会话锁
```

- 写 SSE 时不要依赖 `ctx.Done()` 立即感知——TCP 半开的连接需要**实际写失败**才能发现，因此「每次 delta 都 Write+Flush」既是低延迟手段，也是连通性探测。
- `net/http` 的 `ResponseController.SetWriteDeadline(time.Now().Add(30s))` 可为每次 flush 加保底超时，防止写阻塞 goroutine 泄漏（可选增强）。

### 3.4 进程优雅退出

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
go func() { <-ctx.Done(); cancelAllRuns() }()   // 先中断所有生成
srv.Shutdown(shutdownCtx)                       // 再等连接关闭
wg.Wait()                                       // 等落库收尾
```

顺序很重要：先 cancel 生成（让 handler 有时间把 `interrupted` 状态写进 store），再 shutdown。

---

## 4. 配置项总表（`.env.example`）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | HTTP 监听端口 |
| `APP_ENV` | `dev` | dev 时 CORS 放开 5173，日志更详细 |
| `CORS_ORIGINS` | `http://localhost:5173` | 逗号分隔白名单 |
| `STORE_DRIVER` | `memory` | memory / sqlite / redis（后两者预留） |
| `MAX_MESSAGES_PER_SESSION` | `200` | 超出裁剪最旧消息 |
| `SSE_PING_INTERVAL` | `15s` | 心跳间隔 |
| `SSE_BUFFER_SIZE` | `1000` | 每会话事件环形缓冲条数 |
| `RUN_TIMEOUT` | `180s` | 单次生成总时限 |
| `LLM_PROVIDER` | `mock` | mock / openai |
| `LLM_BASE_URL` | `https://api.openai.com/v1` | 任何 OpenAI 兼容网关 |
| `LLM_MODEL` | `gpt-4o-mini` | 默认模型 |
| `OPENAI_API_KEY` | 空 | 空 + provider=openai → 启动即报错退出（快速失败） |
| `LLM_TIMEOUT` | `120s` | 上游请求超时 |
| `SYSTEM_PROMPT` | 内置中文助手提示 | 会话未指定时使用 |
| `LOG_LEVEL` | `info` | debug/info/warn/error |

---

## 5. 可测试性设计

| 层 | 测试方式 |
| --- | --- |
| `session` | 纯单测：并发 goroutine 写读 + `-race` |
| `sse` | 单测编码器输出字节精确匹配（含 `id/event/data` 顺序、空行） |
| `agent` | `MockRunner` 注入可控节奏；`llm.go` 用 `httptest.Server` 返回伪造 SSE 流 |
| `api` | `httptest.NewServer(router)`；客户端逐帧读 body，断言事件序列与顺序、`seq` 递增、字段完整 |
| 断连 | 客户端读 2 个事件后关闭 body，断言 store 中消息为 `interrupted` 且锁可再次获取 |
| 端到端 | Demo 启动脚本 + 一条 `curl -N` 冒烟命令（不引第三方 e2e 框架） |

验收标准（Demo 完成定义）：

1. `go test -race ./...` 全绿。
2. `curl -N` 能看到 `message.start → message.delta × N → message.done`，并含 `ping`。
3. 浏览器新建会话、发消息、看到逐字流式渲染；刷新后历史仍在（内存 store 前提下同进程内）。
4. 生成中点「停止」→ 消息标记为已中断，再次发送可正常执行（无 `409` 残留）。
5. 删除会话后列表与详情均 404。
