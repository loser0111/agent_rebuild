# 17 · MCP Client：接入任意 MCP server 的工具

> 取代 `docs/13-custom-tools-mcp.md` 的「Go 侧只服务一个 Python sidecar」定位：
> 先做一个**通用的 MCP client 层**，能接任意 stdio MCP server（官方 filesystem/github、
> 社区 server、用户自己写的 Python/Node server）；docs/13 里的 Python sidecar
> 将来只是 `mcp.json` 里的一条普通配置。docs/13 中仍然成立的部分（supervisor 思路、
> 风险默认 Dangerous、Registry 原子替换）在本文里落地，并修正了它的命名方案。

---

## 0. 结论先行

| 问题 | 结论 |
| --- | --- |
| 协议层用官方 `modelcontextprotocol/go-sdk` 还是手写 | **本轮手写最小 client（仅标准库）**。官方 SDK 已有稳定 v1，长期看更省心；但本轮开发环境无法拉依赖/编译验证，手写只覆盖 tools 子集（initialize / tools/list / tools/call / list_changed / ping / cancelled），约 400 行，且被 `Client` 类型封装住，将来换 SDK 不影响上层（Manager / Adapter / API） |
| 传输方式 | 本轮只做 **stdio**。HTTP（Streamable HTTP + OAuth）配置能被解析，但状态直接标 `failed: 暂不支持`，不静默忽略 |
| 工具如何暴露给模型 | **不原生暴露**。自定义工具（MCP）全部收在一个代理工具 `search_router` 后面，由模型 list / search / execute（见第 12 节）。发给模型的 `tools` 列表固定为「内置工具 + search_router」 |
| 工具名 | router 内部用 `server/原始名`（如 `github/search_issues`）。它只是 search_router 参数里的普通字符串，不受 OpenAI function name `[a-zA-Z0-9_-]{1,64}` 的约束；早期的 `mcp__<server>__<tool>` 方案随原生暴露一起废弃 |
| 风险等级与开关 | 默认 `RiskDangerous`、启用；用户可在 **前端面板**（存 SQLite）或 `mcp.json` 里显式调整，前端优先 |
| 运行中换工具集 | 必须先修 Executor 的数据竞争：`registry` 改成 `atomic.Pointer[tool.Registry]`，每轮执行开头 `Load()` 一次快照 |
| server 起不来 | 只影响它自己的工具；不阻塞 `AGENT_READY`，不影响内置工具与对话主流程 |

---

## 1. 整体架构

```
cmd/server/main.go
   │  builtin := tool.DefaultRegistry(...)
   │  ct  := customtools.New(builtin, mgr, overrideStore, srv.ReplaceTools)
   │  mgr := mcp.NewManager(cfgs, workspace, OnChange: ct.Rebuild)
   │        Rebuild = mgr.Catalog() + 前端覆盖 → router.New(...)
   │                  → srv.ReplaceTools(builtin.With(router))   ← 原子替换
   │  mgr.Start(ctx)   ← 后台连接，不阻塞启动
   ▼
internal/tool/mcp（新增）
   ├─ config.go    读 mcp.json（mcpServers 格式）、${VAR} 展开、校验
   ├─ client.go    JSON-RPC 2.0（换行分帧）：请求/响应配对、通知、反向请求
   ├─ stdio.go     子进程：独立进程组、stderr 进日志、优雅关闭（关 stdin→SIGTERM→SIGKILL）
   ├─ naming.go    server 名校验、server/tool 全名
   ├─ adapter.go   远端工具 → CatalogTool（基础风险/开关）；CallToolResult → 文本
   └─ manager.go   每个 server 一个 goroutine：连接 → 就绪 → 监听 → 断线退避重连；Catalog() 快照
internal/tool/router（新增）  search_router：list / search / execute、目录描述、参数签名与预校验
internal/customtools（新增）  前端覆盖的存储（SQLite v4）+ 「Catalog + 覆盖 → router」的组装
```

Registry、三档审批、SSE 的 `tool.call`/`tool.result` 事件不用改。loop 只改了一处：
风险等级从「按工具名」改为「按工具名 + 参数」，让 search_router 的 execute 按**被代理的目标工具**
定级（见 12.5）。

---

## 2. 前置修复：工具注册表的数据竞争

改动前 `Executor.streamEvents` 两次无锁读取 `e.registry`（判空、传给 loop），
`api.Server.tools` 在 `/resume`、`/api/tools` 里也是无锁读。只装配一次时没问题，
但 MCP server 是异步连上、可能中途断开、可能发 `list_changed`，Registry 一定会在
运行中被替换，`go test -race` 会直接报出来。

做法：

- `Executor.registry`、`Server.tools` 都改成 `atomic.Pointer[tool.Registry]`。
- `streamEvents` 开头 `Load()` 一次，整轮 ReAct 都用这份快照。**一轮对话中途不会换工具**：
  模型这一轮看到的工具声明，和它调用时实际可用的工具是同一批。
- 新增 `Executor.SetRegistry` / `Server.ReplaceTools`：只换工具集，不重设系统提示、
  步数、审批器（与 `SetApprover` 只换一个字段是同一个理由）。
- Registry 本身保持「一次性装配、重复名 panic」的设计假设不变；换工具 = 建一个新 Registry。
  新增 `Registry.With(extra...)`：复制内置工具 + 追加 MCP 工具，**重名跳过并告警而不是 panic**
  （外部输入不该能把进程打崩）。

---

## 3. 配置

### 3.1 文件格式（兼容 Claude Desktop / Claude Code / Cursor 的 `mcpServers`）

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/notes"],
      "trust_annotations": true
    },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}" },
      "enabled_tools": ["search_issues", "get_issue"],
      "tool_risk": { "search_issues": "safe", "get_issue": "safe" }
    },
    "custom": { "command": "python3", "args": ["custom_tools/server.py"], "disabled": true }
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `description` | 一句话说明这个 server 能做什么，写进给模型看的工具目录（见 12.2）。强烈建议填 |
| `command` / `args` | stdio server 启动命令（必填 command） |
| `env` | 额外环境变量；值支持 `${VAR}` / `${VAR:-默认值}` |
| `cwd` | 工作目录；相对路径按 `TOOL_WORKSPACE` 解析，留空 = `TOOL_WORKSPACE` |
| `type` / `url` / `headers` | `type: "http"` 或只给 `url` → 本轮标 `failed: 暂不支持 HTTP transport` |
| `disabled` | 不启动，状态显示 `disabled` |
| `enabled_tools` / `disabled_tools` | 按**远端原始工具名**白/黑名单；白名单非空时只暴露名单内工具 |
| `trust_annotations` | 信任 server 自报的 `readOnlyHint`（→ safe）/ `destructiveHint:false`（→ moderate） |
| `tool_risk` | 按工具覆盖风险：`safe` / `moderate` / `dangerous`；键 `"*"` 表示该 server 默认值 |
| `startup_timeout_sec` | initialize + 首次 tools/list 超时，默认 30s（`npx -y` 首次要下载包，10s 不够） |

未知字段静默忽略：别的客户端的配置里常带私有字段，严格拒绝会让「直接拷过来用」失效。
代价是拼错字段名不报错——但拼错的都是「放宽类」字段（trust_annotations / tool_risk），
失败方向是**更严格**，可接受。

### 3.2 加载位置与安全边界

| 来源 | 默认 | 说明 |
| --- | --- | --- |
| `MCP_CONFIG`（逗号分隔多个路径） | — | 显式指定时**只**读这些文件 |
| `<DATA_DIR>/mcp.json` | 读 | 用户级配置（macOS：`~/Library/Application Support/agent-rebuild/mcp.json`） |
| `<TOOL_WORKSPACE>/.mcp.json` | **不读** | 需 `MCP_LOAD_PROJECT_CONFIG=true` |

项目级默认不读，是因为 `.mcp.json` 里的 `command` 会被**直接执行**：克隆一个陌生仓库、
把它设为 workspace，就等于运行了仓库作者指定的任意命令。Claude Code 对项目级 server
要求逐个确认，我们没有确认 UI，就退一步改成显式开关。多个文件中同名 server，后读的覆盖先读的。

### 3.3 环境变量白名单

子进程**不继承**完整环境，只传 `PATH`、`HOME`、`USER`、`LOGNAME`、`SHELL`、`LANG`、`LC_ALL`、`TERM`、`TMPDIR`
（Windows 额外 `SystemRoot`、`APPDATA`、`LOCALAPPDATA`、`USERPROFILE`、`PATHEXT`、`COMSPEC`、`TEMP`、`TMP`），
再加配置里的 `env`。理由：Go 进程环境里有 `OPENAI_API_KEY` 等密钥，没理由泄给第三方 server。
需要别的变量就在 `env` 里显式写 `"X": "${X}"`——`${}` 展开时看的是完整环境。

`${VAR}` 引用了未设置且无默认值的变量 → 该 server 配置无效（`failed`），而不是拿空字符串启动
（空 token 启动的 server 往往能起来但每次调用都 401，比直接报错更难排查）。

注意：macOS 从 Dock 启动的 Electron 应用 PATH 很短（不含 `/opt/homebrew/bin`、nvm 路径），
`npx` 可能找不到。桌面端建议 `command` 写绝对路径，或在 `env` 里显式给 `PATH`。

### 3.4 新增环境变量

```bash
MCP_ENABLED=true                 # 总开关；未配置任何 server 时等价于关闭，零开销
MCP_CONFIG=                      # 显式配置文件路径（逗号分隔），留空走默认位置
MCP_LOAD_PROJECT_CONFIG=false    # 是否读 <TOOL_WORKSPACE>/.mcp.json
MCP_CATALOG_BUDGET=3000          # search_router 描述里工具目录的字符预算（见 12.2）
```

---

## 4. 工具命名

自定义工具不原生暴露，名字只出现在 search_router 的参数和结果里：`server/原始名`，
例如 `github/search_issues`。

- server 名在配置阶段校验：`^[a-zA-Z0-9_-]{1,32}$`。不含 `/`，所以全名按**第一个** `/` 切分
  一定能还原出 (server, tool)；MCP 规范的工具名也不含 `/`。
- 原始名原样保留，不清洗、不截断：它不再进 function name，没有 64 字符和字符集的限制。
- 同一 server 内重名（不守规矩的 server）→ 后者跳过并告警。

早期方案 `mcp__<server>__<tool>`（清洗 + 哈希后缀）是为原生暴露设计的，随原生暴露一起废弃。

## 5. 风险等级与开关

每个自定义工具有两个属性：**风险等级**（safe / moderate / dangerous）和**开关**（是否可用）。
优先级从高到低：

```
风险：前端设置 → mcp.json tool_risk[原名] → trust_annotations 下的 readOnlyHint / destructiveHint=false
      → mcp.json tool_risk["*"] → dangerous
开关：前端设置 → mcp.json enabled_tools / disabled_tools → 启用
```

前端设置存 SQLite（`tool_overrides` 表，schema v4），键是 `server/原始名`。只存「用户改过的字段」，
恢复默认 = 删掉这一行，之后又跟随 `mcp.json`。`STORE_DRIVER=memory` 下面板只读，提示用 sqlite 启动。
覆盖可以指向当前不存在的工具（server 还没连上），连上后自然生效。

关掉的工具：不出现在 list / search 结果和目录里；execute 直接返回「已被用户禁用」，不转发给 server。

MCP 规范明确说 annotations 只是提示，客户端不得据此做安全决策（除非信任该 server），
所以 `trust_annotations` 必须是用户逐个 server 显式打开的。

和审批模式的交互（不改 Approver，只是把事实写清楚）：

- `manual`：moderate / dangerous 都挂起等人工点。
- `model`：judge 裁决。
- `auto`：dangerous 只有在 `ENABLE_COMMAND_TOOL=true` 时放行（沿用 `AutoApprover.AllowDangerous`）。
  也就是说关掉命令工具的部署里，默认风险的 MCP 工具在 auto 模式下会被拒——这是有意的：
  「允许执行任意外部代码」是同一类授权。想放行就在面板里把它降到「写操作」。
- 面板把工具设为「安全」时要二次确认：安全 = 任何审批模式下都直接执行。

## 6. 结果映射（`CallToolResult` → `tool.Result`）

| 远端 | 本地 |
| --- | --- |
| `content[].type=text` | 原文，多段以空行拼接 |
| `image` / `audio` | 占位：`[image: image/png, 约 12KB，当前客户端不支持把图片传给模型]` |
| `resource`（内嵌） | 有 `text` 用 text；否则占位 `[resource: uri (mime)]` |
| `resource_link` | `[resource_link: name uri]` |
| `structuredContent` | 仅当没有任何 text 时，JSON 序列化后作为输出（规范要求 server 同时给 text，这是兜底） |
| `isError: true` | `Result.IsError = true`（工具业务失败，模型据此调整） |
| JSON-RPC error / 断连 / 超时 | `Execute` 返回 error → `Registry.Invoke` 包装成 `IsError` 回喂模型 |

输出统一走 `tool.Truncate(…, 30000)`，与 `read_file` 的上限一致。
超时 / 取消时向 server 发 `notifications/cancelled`，让它有机会停掉手里的活。

## 7. 生命周期（Manager）

每个启用的 server 一个 goroutine：

```
connecting ──initialize+tools/list 成功──▶ ready ──进程退出/读流断开──▶ retrying ──退避──▶ connecting
     │                                     │ list_changed → 重新 tools/list（合并抖动）
     └──失败──▶ retrying ……连续失败 5 次──▶ failed（不再自动重试，可在面板点「重启」）
```

- 退避：1s、2s、4s、8s、16s（上限 30s）；一次连接稳定运行 ≥ 60s 后失败计数清零，
  避免「偶尔崩一次」的 server 在几天后累计到上限被永久放弃。
- 任一 server 的工具集变化（就绪 / 断开 / list_changed）→ `OnChange()`，
  customtools 据此取 `Catalog()`、叠加前端覆盖、重建 search_router，再原子替换 Registry。`OnChange` 在 Manager 内串行调用，
  保证「后发生的变化不会被先发生的变化覆盖」。
- 断开时**先**把工具从 Registry 撤掉，**再**关进程：撤掉之后新一轮对话就看不到它们；
  正在飞的调用拿到「连接已关闭」错误回喂模型。
- 进程放在独立进程组；关闭顺序：关 stdin（规范约定的退出信号）→ 等 2s → SIGTERM 整组 → 等 2s → SIGKILL 整组。
  Go 进程被强杀时 stdin 也会被系统关闭，正常的 server 会自行退出。
- stderr 逐行进日志（`slog.Debug`，带 server 名），并保留最后 20 行；
  启动失败 / 进程退出时把它们拼进 `last_error`，面板上能直接看到报错原因。
- stdout 上非 JSON 的行（不守规矩的 server 把日志打到 stdout）记一条告警后跳过，不断开连接；
  单条消息上限 16MB，超出的行整体丢弃。
- server 发来的反向请求：`ping` 回空结果；其余（sampling / roots / elicitation）回 `-32601`。
  我们在 initialize 里没有声明这些 capability，规范上 server 本就不该发。

---

## 8. API 与前端

```
GET  /api/mcp/servers                 → { enabled, overrides_supported, config_paths, servers: [ServerView] }
POST /api/mcp/servers/{name}/restart  → 202；未知 server 404；disabled 409；MCP 未启用 409
PUT  /api/tool-overrides              → body {tool: "server/原始名", risk: ""|safe|moderate|dangerous, enabled: null|true|false}
                                         risk 为空 / enabled 为 null 表示该字段恢复默认；两者都默认则删除覆盖
                                         成功后立即重建工具集；memory 驱动返回 501
```

`ServerView` = 连接状态（`status`、`last_error`、`server_name/version`、`protocol_version`、`restarts`、
`connected_at`、`restartable`、`description`）+ 每个工具的
`{name, original_name, description, base_risk, base_enabled, override_risk, override_enabled, risk, enabled}`：
`base_*` 是 mcp.json / annotations 算出的值，`override_*` 是前端设置，`risk` / `enabled` 是最终生效值。

前端顶栏 🔌 打开「MCP 服务」面板：每个 server 的状态、重启；展开后每个工具一行，
带风险下拉（默认 / 安全 / 写操作 / 高危）和启用开关，改了立即保存并生效（下一轮对话）。
面板打开期间每 2s 轮询状态。工具卡片与审批卡片里，`search_router` 的 execute 调用展开显示
「代理执行 github/create_issue」和真实参数，而不是一串嵌套 JSON。
`GET /api/tools` 只列原生暴露的工具（内置 + search_router）。

## 9. 失败模式

| 场景 | 处理 |
| --- | --- |
| 配置文件不存在 | 跳过，不告警（大多数用户没有） |
| 配置文件 JSON 非法 | 告警，该文件整体忽略，其余文件照常 |
| 单个 server 配置非法（缺 command / 名字不合法 / 变量未设置 / HTTP） | 该 server 状态 `failed` + 原因，其余照常；目录里标「不可用」 |
| 命令找不到 / 启动即退出 | `retrying` → 5 次后 `failed`，`last_error` 带 stderr 尾部 |
| 对话进行中 server 崩溃 | 本轮快照里的工具调用失败回喂模型；下一轮看不到这些工具；重连成功后恢复 |
| 挂起审批期间工具消失 / 被禁用，之后点「批准」 | search_router 的 execute 返回「工具不存在 / 已禁用」回喂模型，不让整轮失败 |
| 挂起审批期间工具被调高风险，之后点「批准」 | 照常执行：用户批准的就是这一次调用，批准本身已经覆盖了更高的风险 |
| list_changed 风暴 | 通知合并：处理一次 tools/list 期间到达的多次通知只再触发一次 |

## 10. 测试

`internal/tool/mcp` 用**测试二进制自身**当 fixture server（`TestMain` 看到环境变量
`MCP_FIXTURE_MODE` 就进入 server 模式），不依赖 Python / Node：

| 测试 | 验证点 |
| --- | --- |
| `TestManagerConnectsAndExposesTools` | 握手、分页 tools/list、Catalog 内容、instructions |
| `TestManagerCallTool*` | 文本拼接、`isError` 映射、反向 ping 不阻塞调用 |
| `TestManagerListChangedRefreshes` | 通知后新工具出现，`OnChange` 被调用 |
| `TestManagerReconnectsAfterCrash` | 进程退出 → retrying → 重新 ready |
| `TestManagerGivesUpAfterMaxFailures` | 连续失败后 `failed`，`last_error` 含 stderr |
| `TestManagerRestart` | 手动重启从 failed 恢复 |
| `TestCallToolCancelSendsNotification` | 超时后 server 收到 `notifications/cancelled` |
| `TestManagerSkipsNoisyStdout` | stdout 杂行不影响连接 |
| `naming_test` / `config_test` / `adapter_test` | server 名校验、`${}` 展开、白名单环境、基础风险/开关、结果渲染 |
| `router_test` | list 分组分页、多关键词模糊搜索（中英文、错别字）、签名渲染、参数预校验、候选建议、按目标定级 |
| `customtools_test` | 覆盖优先级、恢复默认、覆盖未知工具、Rebuild 后 Registry 内容 |
| `loop` 按参数定级测试 | search_router execute 一个 dangerous 工具会触发审批；list/search 不触发 |
| `api/mcp_test` `TestReplaceToolsConcurrentWithRuns` | `-race` 下并发对话与反复换工具集 |
| `tool_test` `TestRegistryWith` | 合并顺序、重名跳过不 panic |

## 11. 已知限制与后续

| 项 | 说明 |
| --- | --- |
| 仅 stdio | Streamable HTTP + OAuth 留给下一步；届时优先评估换官方 go-sdk（只需替换 `Client` 与 `stdio.go`） |
| 仅 tools | resources / prompts / sampling / roots 未实现 |
| 图片结果不进模型 | `tool.Result` 只有文本；要支持需要扩展 Result 与两个 Runner 的消息编码 |
| 配置不热加载 | 改 `mcp.json` 需重启进程；面板上的「重启」只重连已有配置 |
| 项目级配置无逐个确认 | 只有总开关 `MCP_LOAD_PROJECT_CONFIG` |
| 会话级 workspace | server 的 cwd 只用全局 `TOOL_WORKSPACE`；按会话绑定的文件夹将来可经 MCP `roots` 传递 |
| docs/13 Python sidecar | 变成一条普通配置；「改文件自动重启」需要 sidecar 自己退出 + 这里的重连，但重连会走退避并计入失败次数，频繁保存可能触发 `failed`，届时再加「约定退出码 = 预期重启」 |

---

## 12. 工具三分类与 search_router

### 12.1 三类工具

| 类别 | 是谁 | 怎么给模型 |
| --- | --- | --- |
| **meta 工具** | 内置的 read_file / list_dir / search / write_file / edit_file / run_command / ask_user | 永远原生暴露（`tools` 字段） |
| **代理工具** | 唯一一个：`search_router` | 永远原生暴露；只要配置了至少一个 MCP server（哪怕都没连上）就注册 |
| **自定义工具** | MCP server 提供的工具 | **不原生暴露**，只能经 search_router 发现（list / search）和执行（execute） |

为什么不把自定义工具原生暴露：每一步模型请求都要带上全部工具的完整 schema，接两三个 server
就是上百个工具、上万 token，而且一轮 ReAct 有几步就付几次；工具多了模型也更容易选错。
代理方式让 `tools` 列表固定，loop 不需要在一轮中途扩展工具集，也没有「哪些工具已加载」的跨轮状态。

代价：模型用一个新工具至少多一步（先 search 拿参数说明，再 execute），参数填错的概率比原生 schema 高。
12.4 的签名与预校验就是为了把这个代价压下来。

### 12.2 让模型知道自定义工具存在（目录）

模型不知道某类能力存在，就不会去搜。所以 search_router 的 **description 里带一份动态目录**：

```
发现并执行「自定义工具」（由外部 MCP 服务提供，内置工具之外的能力）。
内置工具做不到的事（访问外部系统、数据库、公司内部服务等），先用它查有没有现成工具。
用法：search 按关键词找工具 → 看返回的参数签名 → execute 执行。也可以 list 按服务浏览。

当前可用的自定义工具：
- github（23 个）：GitHub 仓库、Issue、PR 的查询与操作
  search_issues、get_issue、create_issue、list_pull_requests …
- jira（8 个）：公司 Jira 工单的查询与创建
- postgres：暂不可用（启动超时）
```

- **为什么放在工具描述而不是系统提示**：description 跟着 Registry 一起重建，每次 `Rebuild` 自动更新，
  和本轮工具快照严格一致；系统提示在 `SetTools` 时就固定了。系统提示里另加一句**固定**的准则
  （「内置工具做不到时先用 search_router 查找自定义工具」），它不随工具变化。
- **server 的一句话说明**按优先级取：`mcp.json` 的 `description` → server `initialize` 返回的
  `instructions`（截断到一句）→ 自动生成「提供 a、b 等 N 个工具」。
- **按预算降级**（默认 3000 字符，`MCP_CATALOG_BUDGET` 可调）：
  1. 每个工具一行「名字：描述首句」；
  2. 放不下 → 每个 server 一句说明 + 前 8 个工具名；
  3. 还放不下 → 每个 server 一句说明 + 工具数量。
- **不可用的 server 也列出来**，标原因。模型会如实告诉用户这项能力暂时用不了，而不是当它不存在。
- 关掉的工具不进目录；server 下的工具全被关掉时，该 server 也不进目录。

### 12.3 参数与三个动作

```json
{
  "action":    "list | search | execute",
  "query":     "search：关键词，空格分隔，可多个，支持中英文与模糊匹配",
  "server":    "list：只看某个服务（可选）",
  "page":      "list：页码，从 1 开始（可选）",
  "tool":      "execute：工具全名 server/name",
  "arguments": "execute：传给工具的参数对象"
}
```

- **list**：按 server 分组，每页最多 50 个工具，每个一行「全名：描述首句」。不给 server 时先列各 server 概况。
- **search**：返回最多 8 个结果，按相关度排序。每个结果包含全名、完整描述、风险等级（及是否需要用户确认）、
  **参数签名**。没有结果时返回目录概况和换关键词的建议。
- **execute**：先按 12.4 预校验参数，再转发给 server。

匹配算法（不引入依赖）：关键词按空白和逗号切分，统一小写。对每个工具，在「名字 / 描述 / 参数名 / server 说明」里打分：

| 命中方式 | 分数 |
| --- | --- |
| 名字子串 | 6 |
| 参数名子串 | 3 |
| 描述 / server 说明子串 | 2 |
| 名字分段（按 `_` `-` `/` `.` 和驼峰切开）编辑距离 ≤1（关键词 ≥4 字符）或 ≤2（≥8 字符） | 3（容错拼写，如 `isue`、`pullrequest`） |
| 中文关键词：按二字组与描述比对，命中比例 × 2 | ≤2（「创建工单」能匹配「创建 Jira 工单」） |

多个关键词的分数相加；全部关键词都命中额外 +2。只返回总分 > 0 的工具。

### 12.4 参数签名、预校验与报错

- **签名**：JSON Schema 压成一行，比原始 schema 省 token，照着填也更准：
  `github/search_issues(query: string, state?: "open"|"closed", limit?: integer)`，
  后面逐个参数附描述。嵌套对象 / 对象数组只标 `object` / `object[]`，并附上该参数的原始 schema。
- **预校验**（只做最常见、最确定的错误）：必填字段缺失、顶层字段类型不符（string / number / integer /
  boolean / array / object）、enum 取值不在范围内、schema 声明 `additionalProperties: false` 时的未知字段。
  其余交给 server 校验。不通过时不转发，报错里附完整签名。
- **工具名不存在**：返回编辑距离最近的 3 个候选（「是不是要 github/search_issues？」）和该 server 的工具列表；
  只给了原始名（没写 server）且全局唯一时直接按它执行。
- **工具被禁用 / server 不可用**：明确说明原因，不转发。

### 12.5 审批：按被代理的目标定级

search_router 实现 `tool.CallResolver` 接口：

```go
type CallResolver interface {
    // ResolveCall 返回这次调用实际要执行的目标（展示用，空 = 不是代理调用）及其风险。
    ResolveCall(args json.RawMessage) (target string, risk Risk)
}
```

loop 定级时先问它：list / search → safe；execute → 目标工具的生效风险；目标不存在 / 已禁用 → safe
（反正不会执行，只会返回错误）。`agent.ToolCall` 新增 `Target` 字段，ModelApprover 的裁决提示里
写明「通过 search_router 代理执行 github/create_issue」，判官不用自己去解嵌套参数。

定级（ResolveCall）和执行（Execute）用同一个 router 实例、同一份目录快照：同一轮里两者对目标的认识一致。
跨挂起恢复时 router 可能已经换了（12.1 的工具集变化），见第 9 节失败模式表。

### 12.6 效果评估

需要一组几十条「必须用自定义工具才能完成」的问题，统计：模型主动调用 search_router 的比例、
一次 search 后 execute 成功的比例、因为不知道有工具而回答「做不到」的比例。目录写法、降级阈值、
固定准则的措辞都据此调整。本轮不交付评测集。
