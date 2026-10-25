# 04 · 联调与 Demo 启动

## 1. 两种运行方式

| 方式 | 命令 | 访问地址 | 适用 |
| --- | --- | --- | --- |
| **开发模式**（前后端分离，热更新） | `make dev` | 前端 <http://localhost:5173><br/>后端 <http://localhost:8080> | 日常开发，改代码即时生效 |
| **单端口模式**（Go 托管前端产物） | `make serve` 或 `make demo` | <http://localhost:8080> | Demo 演示、交付验收 |

两种模式接口完全一致，区别只是前端静态文件由 Vite 还是 Go 提供。

### 开发模式

```bash
make dev
```

等价于两个终端：

```bash
# 终端 A：后端（Mock 模式，零配置）
go run ./cmd/server

# 终端 B：前端
cd web && npm install && npm run dev
```

Vite 把 `/api` 代理到 `http://localhost:8080`（`vite.config.ts` 中 `timeout: 0` 保证 SSE 长连接不被代理切断），因此浏览器侧无跨域问题。

### 单端口模式

```bash
make serve      # 构建前端产物 + 启动（go run）
make demo       # 构建前端产物 + 编译二进制 + 启动
```

原理：`WEB_DIR=web/dist` 时，Go 服务在 `/api/*` 之外的路径提供前端静态文件，未命中的路径回退到 `index.html` 以支持前端路由。

也可手工指定：

```bash
WEB_DIR=web/dist PORT=8080 ./bin/agent-server
# 或
make build-binary && ./bin/agent-server
```

`make build-binary` 会把 `web/dist` 通过 `-ldflags -X main.defaultWebDir=web/dist` 写入二进制，因此**拷贝 binary 到仓库目录内任意位置即可直接运行**（相对路径）。

---

## 2. 静态托管行为（`internal/api/spa.go`）

| 请求 | 行为 |
| --- | --- |
| `/` 或前端路由（如 `/sessions/abc`） | 返回 `index.html`，`Cache-Control: no-cache` |
| `/assets/xxx.js`（命中文件） | 直接返回，`Cache-Control: public, max-age=31536000, immutable` |
| `/assets/missing.js`（有扩展名但不存在） | **404**（不回退 HTML，避免浏览器把 index.html 当初 JS 执行） |
| `/api/nope` | JSON 404（`{"error":{"code":"not_found"}}`，不被 SPA 回退吞掉） |
| 非 GET/HEAD | 405 + `Allow: GET, HEAD` |
| `WEB_DIR` 指向的目录不存在 | 503 + 「请先执行 make build-web」提示 |
| 形如 `/../secret` 的越界路径 | 拒绝（`path.Clean` 归一化，不可读到 dist 之外） |

---

## 3. 验收流程（`make e2e`）

`scripts/e2e.sh` 以 curl 模拟浏览器端的完整用户流程，逐步校验后端状态，24 项检查：

| 阶段 | 检查项 |
| --- | --- |
| 1. 新建会话 | 两个会话创建成功、列表返回正确条数 |
| 2. 发送消息 + 流式 | `delta` 帧数 ≥ 5；序列为 `start → delta × N → done`；`id` 严格递增；**整个流耗时 > 300ms**（证明是渐进推送而非缓冲后一次返回）；`done.content` 与所有 `delta` 拼接完全一致 |
| 3. 刷新后历史仍在 | 重新拉取详情得到完整历史（含状态与 `usage`）；`last_event_id` 可用作重连基线；列表仍包含该会话 |
| 4. 多会话切换 | 两个会话消息完全隔离（互不串台）；事件序号按会话独立编号 |
| 5. 停止生成 | `/cancel` 返回 `canceled:true`；SSE 流收到 `message.interrupted`；消息以 `interrupted` 落库；取消后可立即继续发送（无 409 残留）；重复取消幂等 |
| 6. 单端口托管 | `/` 返回 HTML 且含 `#root`；前端路由回退到 `index.html`；`/api` 未知路径仍是 JSON 404 |

运行：

```bash
make e2e              # 自动构建前端 + 启动单端口服务 + 跑完清理
bash scripts/e2e.sh   # 复用已在运行的服务
```

---

## 4. `curl -N` 手工验证 SSE

```bash
B=http://localhost:8080
SID=$(curl -s -X POST $B/api/sessions -H 'content-type: application/json' -d '{}' \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

curl -N -X POST $B/api/sessions/$SID/messages \
  -H 'content-type: application/json' -H 'accept: text/event-stream' \
  -d '{"content":"验证 SSE 原始输出"}'
```

预期输出（`-N` 关闭 curl 自身缓冲，可看到逐帧到达）：

```
retry: 3000

: connected

id: 1
event: message.start
data: {"seq":1,"session_id":"sess_...","message_id":"msg_...","model":"mock-echo","created_at":"..."}

id: 2
event: message.delta
data: {"seq":2,"session_id":"sess_...","message_id":"msg_...","delta":"（Mock 模式，未配置模型 Key）\n\n"}

id: 3
event: message.delta
data: {"seq":3,"session_id":"sess_...","message_id":"msg_...","delta":"收到你的"}

...

id: 64
event: message.done
data: {"seq":64,"session_id":"sess_...","message_id":"msg_...","content":"...","finish_reason":"stop","usage":{...}}
```

要点：

- `retry: 3000` 只在首个事件前出现一次；
- `: connected` 是注释行（探活），浏览器端忽略；
- 每帧 `id` 与会话维度唯一，且与负载里的 `seq` 完全相等；
- `data` 永远是**单行** JSON（正文换行被转义为 `\n`）。

---

## 5. 联调中修复的问题

| # | 问题 | 现象 | 修复 |
| --- | --- | --- | --- |
| 1 | 取消生成后前端永久停在「生成中」 | `runCtx.Done()` 分支只落库不发事件 | 补发 `message.interrupted`；超时改发 `message.error` |
| 2 | 方法不匹配返回 404 而非 405 | `ServeMux` 的 `/` 兜底优先级高于其自动 405 判定 | 每个路径额外注册无 method 模式，返回 405 + `Allow` |
| 3 | 流式渲染出现半截 Markdown 标记 | Mock/LLM 的 `**粗体**` 被切块边界拆成 `**验` / `证**`，逐字过程中显示孤立星号 | 前端 `parseInline(src, trailing)` 在流式模式下隐藏末尾未闭合标记 |
| 4 | 单端口部署无法访问前端 | Go 只注册了 `/api` 路由 | 新增 `internal/api/spa.go`，`WEB_DIR` 启用静态托管 + SPA 回退 |
| 5 | 缺失静态资源被回退成 HTML | 浏览器把 `index.html` 当 JS 执行，报语法错误 | 含扩展名且文件不存在时直接 404 |
| 6 | 说明脚本中文紧随变量报 `unbound variable` | bash 把全角括号当成变量名字符 | 统一用 `${VAR}` 界定 |

---

## 6. 最终启动命令

```bash
# 方式一：单端口 Demo（推荐验收/演示）
make demo
# → 打开 http://localhost:8080

# 方式二：开发模式（热更新）
make dev
# → 前端 http://localhost:5173 ，后端 http://localhost:8080

# 验证
make e2e          # 24 项端到端验收
make test-all     # 后端 -race + 前端逻辑/渲染检查
```
