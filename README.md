# agent_rebuild

对 agent 项目进行重建：**前后端分离的 Agent Demo**（Go 后端 + Codex 风格前端），支持 SSE 流式输出与会话管理。

## 设计文档

| 文档 | 内容 |
| --- | --- |
| [docs/01-architecture.md](docs/01-architecture.md) | 整体架构图、技术选型（Go 1.22 net/http + Vite/React/TS）、目录结构约定、核心接口、非功能性设计 |
| [docs/02-api-and-sse.md](docs/02-api-and-sse.md) | REST 契约（会话 CRUD）、`POST /messages` SSE 主流程、SSE 事件协议与重连补齐、取消接口、curl 速查 |
| [docs/03-runtime-and-storage.md](docs/03-runtime-and-storage.md) | 会话持久化（Store 接口 + 内存实现 → SQLite/Redis）、并发模型、超时与取消、配置项、测试与验收标准 |
| [docs/04-e2e-and-demo.md](docs/04-e2e-and-demo.md) | 两种运行方式（开发模式 / 单端口托管）、静态托管行为、`make e2e` 验收流程、联调修复记录 |
| [docs/05-desktop.md](docs/05-desktop.md) | 桌面客户端（Electron）：sidecar 启动协议、进程生命周期、asar 解包要点、打包与验证 |
| [docs/06-model-setup.md](docs/06-model-setup.md) | 接入真实模型：配置文件位置、各服务商配置、验证方法、常见问题排查 |
| [docs/07-orchestration-decision.md](docs/07-orchestration-decision.md) | **编排层选型与接口契约**：eino 实测评估、Runner/Checkpoint/Event/Message 契约、执行生命周期解耦方案 |
| [docs/08-storage-and-models.md](docs/08-storage-and-models.md) | **SQLite 持久化与多模型配置**：驱动选型、schema、API Key 加密、REST 接口、运行时切换 |
| [docs/09-tools.md](docs/09-tools.md) | **工具系统**：6 个内置工具、安全设计（路径隔离/环境白名单/进程组/截断）、审批机制、ReAct 多轮循环 |
| [docs/10-markdown-rendering.md](docs/10-markdown-rendering.md) | **前端 Markdown 渲染**：支持的语法、链接协议白名单（防 XSS）、流式渲染鲁棒性、手写解析的坑 |
| [docs/11-desktop-ui.md](docs/11-desktop-ui.md) | **桌面端界面适配**：hiddenInset 下的红绿灯避让、双击放大接线、文本选择策略、运行时验证 |
| [docs/12-approval-modes.md](docs/12-approval-modes.md) | **审批模式设计**：manual/model/auto 三档、复用 ask_user 的挂起机制、`Approver` 接口改造 |
| [docs/13-custom-tools-mcp.md](docs/13-custom-tools-mcp.md) | **自定义工具（MCP sidecar）**：Python 子进程 + 热编辑设计、Go MCP client、supervisor 重启策略、并发修复前提 |

## 一句话概览

浏览器（Vite + React + TypeScript，Codex 风格深色界面）
↔ Go HTTP 服务（`net/http` ServeMux：REST 会话管理 + 标准库手写 SSE 流 + 心跳/重放）
↔ Agent 执行层（`Runner` 接口：MockRunner / OpenAI 兼容流式）↔ LLM。

会话数据先落在 `session.Store` 接口的内存实现上（map + RWMutex），后续可零业务改动切换 SQLite / Redis。

---

## 快速启动

### 0. 环境要求

| 依赖 | 版本 | 说明 |
| --- | --- | --- |
| Go | **1.22+**（开发环境 1.25 通过） | 用到 ServeMux 的 method+path 路由 |
| Node.js | **18+**（开发环境 24 通过） | 前端构建 |
| npm | 9+ | 随 Node 安装 |
| make | 任意 | 可选，也可直接看下面的等价命令 |

**零配置即可跑通**：默认 `LLM_PROVIDER=mock`，不需要任何 API Key。

### 1. 一键开发（推荐）

```bash
make dev
```

- 后端：<http://localhost:8080>（`go run ./cmd/server`，日志在 `/tmp/agent-server.log`）
- 前端：<http://localhost:5173>（Vite，`/api` 自动代理到后端，无跨域问题）

停止后端：`make stop`。

### 1b. 单端口 Demo（交付/演示推荐）

```bash
make demo
```

构建前端产物 + 编译二进制并启动，浏览器打开 **<http://localhost:8080>** 即可（前后端同一个端口）。
详见 [docs/04-e2e-and-demo.md](docs/04-e2e-and-demo.md)。

### 1c. 桌面客户端（Electron）

```bash
make desktop        # 开发态启动原生窗口（自动拉起 Go 后端）
make desktop-pack   # 打包 → desktop/release/mac-arm64/Agent Rebuild.app
make desktop-verify # 打包 + 真实启动产物验证端到端
```

详见 [docs/05-desktop.md](docs/05-desktop.md)。

### 2. 分别启动（两个终端）

```bash
# 终端 A —— 后端
go run ./cmd/server

# 终端 B —— 前端
cd web && npm install && npm run dev
```

### 3. 生产构建

```bash
make build          # 后端 → bin/agent-server，前端 → web/dist
./bin/agent-server  # 前台运行
```

等价命令：

```bash
CGO_ENABLED=0 go build -o bin/agent-server ./cmd/server
cd web && npm install && npm run build
```

### 4. 接入真实模型

配置分两个维度（**协议**决定代码路径，**服务商**决定端点与模型）：

```bash
# 例：Anthropic，两行即可
LLM_PROVIDER=anthropic
OPENAI_API_KEY=sk-ant-你的密钥
```

支持的 provider：`mock` `openai` `anthropic`(claude) `deepseek` `moonshot`(kimi)
`zhipu`(glm) `qwen`(通义) `siliconflow` `openrouter` `groq` `ollama` `vllm` `lmstudio`

支持的协议：`openai`(默认) `anthropic` `mock`；不写 `LLM_PROTOCOL` 时按 provider 自动推断。

命令行方式：

```bash
cp .env.example .env    # 编辑填入 provider 与 Key
make demo
```

桌面版请把配置放到 `~/Library/Application Support/agent-rebuild-desktop/config.env`
（打包后的 .app 工作目录是 `/`，读不到项目根目录的 `.env`）。

完整说明见 **[docs/06-model-setup.md](docs/06-model-setup.md)**。

### 5. 冒烟测试（不依赖前端）

```bash
make smoke                          # 复用已在跑的后端
START_SERVER=1 make smoke           # 自动拉起后端、跑完即关
```

会依次校验：健康检查 → 创建会话 → SSE 事件序列（start → delta → done）→ 历史落库 → 删除并确认 404。

---

## 常用命令

| 命令 | 作用 |
| --- | --- |
| `make help` | 列出所有目标 |
| `make dev` | 同时启动前后端 |
| `make run` | 只跑后端（Mock 模式） |
| `make build` | 构建后端二进制 + 前端产物 |
| `make test` | `go test -race ./...` |
| `make test-web` | 前端逻辑 + 组件渲染检查（无需浏览器） |
| `make test-all` | Go + 前端全部检查 |
| `make vet` | `go vet` + gofmt 格式检查 |
| `make smoke` | SSE 全链路冒烟 |
| `make e2e` | 端到端联调验收（24 项，自动起单端口服务） |
| `make serve` / `make demo` | 单端口运行（前端由 Go 托管） |
| `make build-binary` | 构建含前端产物的单一可执行文件 |
| `make desktop` | 启动 Electron 桌面客户端 |
| `make desktop-smoke` | 桌面壳冒烟（sidecar / API / SSE / 进程回收） |
| `make desktop-pack` | 打包桌面应用到 `desktop/release/` |
| `make desktop-verify` | 打包 + 验证打包产物能真正运行 |
| `make desktop-test` | 桌面壳回归（关窗不弹框 / 关窗即退出 / 孤儿守护 / 持久化 / UI 静态检查） |
| `make desktop-ui-check` | **运行时** UI 验证（真实应用内读计算样式 + 实测 IPC） |
| `make desktop-test-packaged` | 验证打包产物关窗即退出 |
| `make stop` / `make clean` | 停止后端 / 清理产物 |

---

## 目录结构

```
.
├── cmd/server/          # 入口：装配 config → store → runner → api → http.Server
├── internal/
│   ├── api/             # 路由、中间件、统一错误体、SSE 端点
│   ├── sse/             # 手写 SSE 编码器 + 环形缓冲（断线重放）
│   ├── session/         # 领域模型 + Store 接口 + 内存实现
│   ├── agent/           # Runner 接口、Mock、OpenAI/Anthropic 流式
│   │   └── loop/        # ReAct 多轮工具循环（模型 → 工具 → 模型）
│   ├── tool/            # 工具系统（read/write/edit/list/search/run_command）
│   ├── storage/sqlite/  # SQLite 会话存储
│   ├── modelconfig/     # 多模型配置（API Key 加密）
│   └── config/          # 环境变量解析 + .env 加载
├── web/                 # Vite + React + TypeScript 前端
│   ├── scripts/         # 无需浏览器的前端检查（esbuild + react-dom/server）
│   └── src/
│       ├── api/         # REST 客户端、手写 SSE 客户端、类型定义
│       ├── state/       # ChatProvider：会话列表 / 消息流 / 流式状态机
│       ├── components/  # Sidebar / ChatView / MessageItem / Composer / StatusBar / Markdown
│       ├── lib/         # Markdown 解析器 + 轻量语法高亮（无第三方依赖）
│       └── styles/      # 设计 token（Codex 深色观感）
├── desktop/             # Electron 桌面壳
│   ├── main.js          # 主进程：拉起 sidecar / 等待就绪 / 加载界面 / 回收进程
│   ├── preload.js       # 最小暴露（contextIsolation + sandbox）
│   └── scripts/         # prepare-sidecar / make-icon / smoke-electron / verify-package
├── scripts/
│   ├── smoke.sh         # curl 冒烟脚本（后端协议）
│   └── web-smoke.mjs    # Markdown / 高亮解析器断言
├── docs/                # 设计与接口文档
└── Makefile
```

---

## 前端说明

| 关注点 | 实现 |
| --- | --- |
| 布局 | 左侧会话栏（新建/切换/删除，删除需二次确认）+ 顶栏 + 主对话区 + 输入区 + 底部状态栏；窄屏自动折叠会话栏 |
| 流式渲染 | `fetch` + `ReadableStream` 手写 SSE 解析（不用 `EventSource`：它不支持 POST body / 自定义头 / 取消），逐帧增量追加，带闪烁光标与打字点 |
| 停止生成 | `AbortController.abort()` 中断请求 + `POST /cancel` 通知服务端；已生成内容以 `interrupted` 落库并显示「已中断」标记 |
| Markdown | 自研轻量解析器（代码块/列表/引用/标题/粗斜体/链接）+ 语法高亮（Go/TS/JS/Python/Bash/SQL/JSON），全 React 元素渲染，不用 `dangerouslySetInnerHTML` |
| 代码块 | 等宽字体 + 语言标签 + 悬停显示的复制按钮（`clipboard` 不可用时降级 `execCommand`） |
| 输入框 | `Enter` 发送、`Shift+Enter` 换行（输入法组合中不拦截）、高度自适应、生成中切换为「停止」按钮 |
| 状态栏 | 连接状态点 + 文案（就绪/连接中/流式接收中/连接中断/错误）+ 生成耗时 + provider·model + `seq` 事件序号 + 版本 |
| 错误处理 | 统一错误条（`session_busy` 等有专门文案）；流异常中断显示「重新同步」引导，点击后拉全量历史兜底 |
| Vite 代理 | `/api` → `http://localhost:8080`，`timeout: 0` 保证 SSE 长连接不被代理切断 |

---

## 当前进度

- [x] 设计方案与技术方案文档
- [x] 项目骨架与构建脚本（`go build` / 前端 `npm run build` / `make test` 全绿）
- [x] 后端会话管理模块（`session.Store` + 内存实现，89% 覆盖率，含契约测试与并发测试）
- [x] 后端 SSE 流式接口与 Agent 执行层（会话 CRUD + `POST /messages` SSE + `/stream` 补齐 + `/cancel`，含事件序列集成测试）
- [x] Codex 风格前端界面（会话栏 / 流式对话 / Markdown+高亮 / Enter 发送 / 停止生成 / 状态栏）
- [x] 前后端联调并启动 Demo（单端口托管 + `make e2e` 24 项验收全通过）
- [x] 桌面客户端（Electron）：Go sidecar + 随机端口 + 进程回收 + 打包验证
- [x] SQLite 持久化 + 多模型配置（API Key 加密、运行时切换）
- [x] 工具系统（6 个内置工具 + ReAct 多轮循环 + 安全隔离 + 审批机制）

## 快速验收

```bash
make demo     # 构建并启动 → 打开 http://localhost:8080
make e2e      # 24 项端到端验收（新建/流式/刷新/切换/停止/单端口）
make test-all # 后端 -race + 前端逻辑与渲染检查
```

验收覆盖：新建会话 → 发送消息 → 流式逐字渲染（`curl -N` 可视 + 逐帧到达校验）→ 刷新后历史仍在 → 多会话切换不串台 → 停止生成（`message.interrupted` + 落库 + 可续发）→ 单端口静态托管与 SPA 回退。详见 [docs/04-e2e-and-demo.md](docs/04-e2e-and-demo.md)。
