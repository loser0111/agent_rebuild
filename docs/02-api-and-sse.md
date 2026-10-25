# 02 · 接口契约（REST + SSE）

Base URL：`/api`（dev 下 Vite 代理到 `http://localhost:8080`）
编码：请求/响应体均为 `application/json; charset=utf-8`（SSE 端点除外）
时间：所有时间戳为 RFC3339 UTC 字符串（`2025-01-01T08:00:00Z`）
ID：字符串，服务端生成（`sess_` / `msg_` 前缀 + ULID 风格后缀），前端不解析其结构

---

## 0. 通用约定

### 0.1 统一错误体

```json
{ "error": { "code": "session_not_found", "message": "session not found", "request_id": "req_..." } }
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_argument` | 参数缺失/格式错误（如空 content） |
| 404 | `session_not_found` / `message_not_found` | 资源不存在 |
| 409 | `session_busy` | 该会话已有运行中的生成，禁止并发发送 |
| 413 | `payload_too_large` | 请求体 > 1MB |
| 499 | `client_closed` | 客户端断开（仅记日志，通常不返回） |
| 500 | `internal_error` | 未预期错误（不回传内部细节） |
| 503 | `provider_unavailable` | 上游 LLM 不可用 / 未配置 Key |

### 0.2 数据模型

**Session**

```json
{
  "id": "sess_01HQ...",
  "title": "关于 Go SSE 的设计讨论",
  "created_at": "2025-01-01T08:00:00Z",
  "updated_at": "2025-01-01T08:03:12Z",
  "message_count": 4,
  "status": "idle",
  "model": "gpt-4o-mini",
  "meta": { "client": "web" }
}
```

`status`：`idle` | `running` | `error`

**Message**

```json
{
  "id": "msg_01HQ...",
  "session_id": "sess_01HQ...",
  "role": "user",
  "content": "帮我设计一个 SSE 协议",
  "status": "complete",
  "created_at": "2025-01-01T08:00:05Z",
  "updated_at": "2025-01-01T08:00:05Z",
  "usage": { "prompt_tokens": 128, "completion_tokens": 512, "total_tokens": 640 },
  "error": null
}
```

`role`：`user` | `assistant` | `system`
`status`：`pending`（assistant 占位，正在生成）| `streaming` | `complete` | `interrupted`（客户端断开/取消）| `error`

> `usage` 仅 assistant 消息在 `complete` 时出现；`error` 仅在 `status=error` 时非空，形如 `{"code":"provider_unavailable","message":"..."}`。

---

## 1. REST 端点

### 1.1 创建会话 — `POST /api/sessions`

请求（全部可选）：

```json
{ "title": "新会话", "model": "gpt-4o-mini", "system_prompt": "你是一个严谨的助手" }
```

响应 `201 Created`：

```json
{ "session": { "id": "sess_01HQ...", "title": "新会话", "created_at": "...", "updated_at": "...", "message_count": 0, "status": "idle", "model": "gpt-4o-mini" } }
```

规则：`title` 为空则服务端生成（`新会话 · MM-DD HH:mm`）；首条用户消息落库后若标题仍是默认值，则截取前 24 字符作为标题。

### 1.2 会话列表 — `GET /api/sessions?limit=50&cursor=<opaque>`

响应 `200`：

```json
{
  "sessions": [ { "id": "sess_...", "title": "...", "updated_at": "...", "message_count": 4, "status": "idle" } ],
  "next_cursor": null
}
```

按 `updated_at` 倒序；`limit` 默认 50、上限 200。

### 1.3 会话详情（含历史） — `GET /api/sessions/{id}`

响应 `200`：

```json
{
  "session": { "id": "sess_...", "...": "..." },
  "messages": [ { "id": "msg_...", "role": "user", "content": "...", "status": "complete" } ],
  "last_event_id": 12
}
```

`last_event_id` 是该会话事件流当前最大序号，前端拿到后可作为后续断线重连的基线。

### 1.4 删除会话 — `DELETE /api/sessions/{id}`

响应 `204 No Content`。若该会话有运行中生成，先 cancel 再删除。

### 1.5 局部更新（可选，Demo 支持） — `PATCH /api/sessions/{id}`

```json
{ "title": "重命名后的标题" }
```

响应 `200` 返回更新后的 session。

### 1.6 健康检查 — `GET /api/health`

```json
{ "status": "ok", "version": "0.1.0", "provider": "mock", "model": "mock-echo", "uptime_seconds": 42 }
```

---

## 2. 发送消息（SSE 主流程） — `POST /api/sessions/{id}/messages`

```http
POST /api/sessions/sess_01HQ/messages
Content-Type: application/json
Accept: text/event-stream
X-Request-Id: (可选，缺失则服务端生成)

{
  "content": "帮我设计 SSE 协议",
  "client_message_id": "c-8f2a",     // 可选，幂等键
  "stream": true,                     // 默认 true；false 时退化为普通 JSON 聚合响应
  "model": "gpt-4o-mini",             // 可选，覆盖会话默认
  "attachments": []                   // 预留，本期忽略
}
```

**为什么用 POST 而不是 EventSource**：需要请求体、幂等键与自定义头；浏览器 `EventSource` 只支持 GET 且无法取消。SSE 只是「响应体的编码格式」，与请求方法无关。

### 2.1 响应头

```http
HTTP/1.1 200 OK
Content-Type: text/event-stream; charset=utf-8
Cache-Control: no-cache, no-transform
Connection: keep-alive
X-Accel-Buffering: no          # 关掉 nginx 缓冲
X-Request-Id: req_...
```

> 注意：错误在**建立流之前**以普通 JSON + 4xx/5xx 返回（例如 404 会话不存在、409 会话忙）。一旦回了 200 + `text/event-stream`，后续错误只能走 `event: error`。

### 2.2 `stream: false` 退化模式

响应 `200 application/json`：

```json
{ "user_message": { "...": "..." }, "assistant_message": { "id": "msg_...", "content": "完整文本", "status": "complete", "usage": {...} } }
```

便于 `curl` 调试与非流式调用方。

---

## 3. SSE 事件协议

### 3.1 帧格式

每个事件 = 若干 `field: value` 行 + 空行结束（严格遵循 WHATWG EventSource 规范，兼容手写解析器与 `EventSource`）：

```
id: 7
event: message.delta
data: {"message_id":"msg_01HQ","seq":7,"delta":"好的"}
⏎
```

- `id`：**单调递增整数**，会话维度全局唯一（不按消息重置），用于 `Last-Event-ID` 重放。
- `event`：事件名（见下表）。
- `data`：**单行 JSON**（若需多行也必须压成一行；不使用多行 `data:` 以免解析歧义）。
- `retry: 3000` 仅在流的首个事件前发送一次，告知客户端重连间隔。

### 3.2 事件类型

| event | 时机 | data 结构 |
| --- | --- | --- |
| `message.start` | 助手消息创建（占位落库完成） | `{"session_id","message_id","seq","model","created_at"}` |
| `message.delta` | 每收到一段增量 | `{"session_id","message_id","seq","delta"}` |
| `message.done` | 生成正常结束 | `{"session_id","message_id","seq","content","finish_reason","usage":{"prompt_tokens","completion_tokens","total_tokens"}}` |
| `message.error` | 生成失败（消息标记 error） | `{"session_id","message_id","seq","error":{"code","message"}}` |
| `message.interrupted` | 客户端取消 / 服务端超时 | `{"session_id","message_id","seq","content"}` |
| `ping` | 心跳，解决代理/浏览器空闲断连 | `{"ts":"2025-01-01T08:00:00Z"}` |
| `error` | **流级**错误（非某条消息） | `{"error":{"code","message"},"request_id"}` |

事件命名说明：任务要求中的 `message.delta / message.done / error / ping` 是核心集合；`message.start / message.error / message.interrupted` 是必要补充（前端需要知道 message_id 才能把 delta 归位，需要区分「消息失败」与「连接失败」）。

### 3.3 完整示例

```
retry: 3000
: connected

id: 1
event: message.start
data: {"session_id":"sess_01HQ","message_id":"msg_a1","seq":1,"model":"mock-echo","created_at":"2025-01-01T08:00:06Z"}

id: 2
event: message.delta
data: {"session_id":"sess_01HQ","message_id":"msg_a1","seq":2,"delta":"SSE 协议"}

id: 3
event: ping
data: {"ts":"2025-01-01T08:00:16Z"}

id: 4
event: message.delta
data: {"session_id":"sess_01HQ","message_id":"msg_a1","seq":4,"delta":"可以这样设计……"}

id: 5
event: message.done
data: {"session_id":"sess_01HQ","message_id":"msg_a1","seq":5,"content":"SSE 协议可以这样设计……","finish_reason":"stop","usage":{"prompt_tokens":128,"completion_tokens":96,"total_tokens":224}}
```

### 3.4 重连与补齐 — `GET /api/sessions/{id}/stream?last_event_id=<n>`
客户端在 `message.delta` 流中断后（网络抖动、切后台、浏览器丢弃长连接）重新连接：

```http
GET /api/sessions/sess_01HQ/stream?last_event_id=12
Accept: text/event-stream
```

服务端行为：

1. 立即回 `200 text/event-stream` + `retry: 3000`。
2. 从该会话环形缓冲（每会话保留最近 1000 条事件）中重放 `seq > last_event_id` 的事件。
3. 重放结束后若会话仍在 `running`，**转为实时推送**（同一会话的订阅者 fan-out）。
4. 若 `last_event_id` 已被裁剪（差距过大），先发 `event: resync` + 完整消息快照，再转为实时：
   ```
   event: resync
   data: {"session_id":"sess_01HQ","messages":[...],"last_event_id":42}
   ```
5. 无 `last_event_id` 时等价于「从当前开始只订阅实时事件」，并立刻回一个 `ping`。

同时客户端还有一条**兜底路径**（更简单可靠）：断线后直接 `GET /api/sessions/{id}` 拉全量历史覆盖本地状态，再决定是否重连订阅。Demo 前端默认走兜底路径，`/stream` 作为可选增强。

### 3.5 客户端解析器要点（前端 `api/sse.ts`）

- 按 `\n\n` 切帧，逐帧解析 `id:` / `event:` / `data:`；`:` 开头为注释行（心跳注释 `: keep-alive`）忽略。
- `data:` 后可能带一个空格，需 trim 前缀单空格（保留正文空格）。
- 记录最近一次 `id` 到 `lastEventId`，重连时作为 `?last_event_id=`。
- 用 `AbortController` 实现 Stop：`abort()` 后服务端 `ctx.Done()` 触发，消息落库为 `interrupted`。

---

## 4. 取消生成 — `POST /api/sessions/{id}/cancel`

请求体可空。响应分两种：

| 情况 | HTTP | 响应体 |
| --- | --- | --- |
| 该会话确有运行中的生成，已触发取消 | `202 Accepted` | `{ "canceled": true }` |
| 没有运行中的生成（幂等） | `200 OK` | `{ "canceled": false }` |

服务端对该会话运行中的 ctx 调 `cancel()`；对应的 SSE 流会收到 `message.interrupted`（携带已生成内容）后正常结束（HTTP 200，不视为错误），该消息以 `status=interrupted` 落库。

> 实现细节：取消信号同时会取消上游模型请求（`http.NewRequestWithContext`），避免继续消耗 token；已生成的半截内容不会进入后续对话上下文（`internal/agent/llm.go` 中过滤 `interrupted` 的 assistant 消息）。

---

## 5. curl 速查

```bash
# 创建会话
curl -s -X POST localhost:8080/api/sessions -H 'content-type: application/json' -d '{}'

# 列表
curl -s localhost:8080/api/sessions

# 发消息并看流
curl -N -X POST localhost:8080/api/sessions/<id>/messages \
  -H 'content-type: application/json' -H 'accept: text/event-stream' \
  -d '{"content":"你好，做个自我介绍"}'

# 非流式
curl -s -X POST localhost:8080/api/sessions/<id>/messages \
  -H 'content-type: application/json' -d '{"content":"你好","stream":false}'

# 断线补齐
curl -N "localhost:8080/api/sessions/<id>/stream?last_event_id=5"

# 取消
curl -s -X POST localhost:8080/api/sessions/<id>/cancel

# 删除
curl -s -X DELETE localhost:8080/api/sessions/<id> -o /dev/null -w '%{http_code}\n'
```

---

## 6. 兼容性约束（前端与后端共同遵守）

1. 新增事件类型必须**只新增不改动**；客户端遇到未知 `event` 时忽略并打日志，不得报错。
2. 新增 JSON 字段只能追加，前端必须容忍未知字段；后端解析请求时忽略未知字段。
3. `seq` 严格递增，前端据此去重（重放与实时事件可能重叠）。
4. 所有时间字段为 UTC RFC3339；前端本地化展示。
5. 断连不是错误：前端展示「连接已断开，正在重试」并在 3s 后自动补齐。
