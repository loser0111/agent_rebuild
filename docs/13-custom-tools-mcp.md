# 13 · 自定义工具：内置 Python MCP Sidecar + 热编辑

> **状态更新（见 `docs/17-mcp-client.md`）**：Go 侧改为先做通用 MCP client，Python sidecar
> 只是 `mcp.json` 里的一条普通 server 配置。本文以下内容被 docs/17 修正或取代：
>
> - 3.2 Adapter「把远端工具包成 tool.Tool 原生暴露」**作废**：自定义工具改为收在代理工具 `search_router`
>   后面（list / search / execute），见 docs/17 第 12 节。
> - 3.3 命名空间 `custom.<name>` **作废**：router 内部用 `server/原始名`。
> - 第 7 节「一律 RiskDangerous」保留为默认值，但用户可在前端面板逐个调整风险等级与开关（docs/17 第 5 节）。
> - 第 5 节 Registry 数据竞争：已按方案 2（`atomic.Pointer[tool.Registry]`）修复。
> - 第 8 节配置项 `ENABLE_CUSTOM_TOOLS` 等：由 `MCP_ENABLED` / `MCP_CONFIG` + `mcp.json` 取代。
> - 第 3、4 节 Client / Adapter / Supervisor：已在 `internal/tool/mcp` 实现（stdio）。
>
> 仍待做：sidecar 本身（Python 模板）、「约定退出码 = 预期重启」不计入失败次数。

> 承接 `docs/09-tools.md` 第 9 节「后续可做 2」：接入 MCP 工具源。
> 动机不是"接第三方 MCP server"，而是给用户一个**不用碰 Go 代码、不用重新编译二进制**的自定义工具面：
> 编辑一个 Python 文件，用官方 MCP SDK 的装饰器定义函数，保存后很快就能在对话里被模型调用。

---

## 0. 结论先行

可行，且与现有架构顺——只是「热编辑」不能理解成"进程内热加载代码"，而要理解成**"文件变化触发体面重启 + Go 侧原子换一批工具"**，重启这一步不省略。

| 问题 | 结论 |
| --- | --- |
| Python 进程内热加载 `.py` 代码（`importlib.reload`）可行吗 | **不可行**，装饰器状态/闭包旧值/类属性极易半生不熟出 bug，业界不这么做 |
| 文件变了之后让进程体面退出、外层重启、重新 `tools/list` 可行吗 | **可行**，而且用户感知不到差别（改完文件等一两秒即生效） |
| Go 这边需要自己实现 MCP JSON-RPC 协议吗 | **需要**，躲不掉——Python sidecar 对 Go 来说就是"一个 stdio MCP server"，将来接别的 MCP server（第三方/远程）复用同一份 client 代码 |
| 现有代码能直接支持"运行中换一批工具"吗 | **不能**，`executor.go` 里有一处真实的数据竞争，必须先修（见第 5 节） |

---

## 1. 范围

**这次要做的**：

- Go 进程启动时（或配置开启后）自动拉起一个本地 Python 子进程，它是一个 stdio MCP server，加载用户项目里 `custom_tools/` 目录下的工具定义。
- Python 侧监听这个目录的文件变化，检测到改动后体面退出（而不是原地热加载）。
- Go 侧的 supervisor 识别"预期内重启"与"真崩溃"，重启后重新握手、重新拿 `tools/list`，替换进当前生效的工具集。

**这次不做的（留给后续）**：

- 接第三方/远程 MCP server（HTTP/SSE transport、鉴权）——Go MCP client 一旦写出来，这条路径复用同一份代码，但本次只验证 stdio + 本地进程。
- 真正的 MCP `notifications/tools/list_changed` 增量通知（见第 12 节，Python 进程存活期间动态增删工具属于另一个问题，本次用更简单粗暴但更可靠的"整进程重启"解决）。
- 交互式人工审批 UI（`docs/12-approval-modes.md` 单独在做，这里只是把自定义工具接进现有 `Approver` 体系，按最高风险等级处理）。

---

## 2. 整体架构

```
┌───────────────────────────────────────────────────────────────┐
│ Go 进程（cmd/server）                                          │
│                                                                 │
│  internal/tool/mcp（新增）                                     │
│   ├─ Supervisor：spawn python、读 ready 标记、                 │
│   │              区分「预期重启」vs「崩溃」、退避重试            │
│   ├─ Client：stdio JSON-RPC（initialize / tools/list /         │
│   │           tools/call）                                     │
│   └─ Adapter：把远端 Spec 包成 tool.Tool，Execute() 转发        │
│               一次 tools/call                                  │
│                                                                 │
│  每次重启完成后：                                                │
│   新 Registry = DefaultRegistry() 的内置工具 + 本轮 MCP 工具     │
│   executor.SetTools(新 Registry, ...)   ← 原子替换（见第 5 节） │
└───────────────────────────┬─────────────────────────────────────┘
                             │ stdio（stdin/stdout，JSON-RPC 2.0）
                             ▼
┌───────────────────────────────────────────────────────────────┐
│ Python 子进程（sidecar，用官方 MCP SDK，如 fastmcp）              │
│                                                                 │
│  1. import custom_tools/ 下用户定义的 @mcp.tool() 函数            │
│  2. stdout 打一行 MCP_READY（Go 侧的就绪标记）                   │
│  3. 后台线程 watch custom_tools/（watchfiles，带防抖）           │
│  4. 检测到变化 → sys.exit(RESTART_EXIT_CODE)，不原地重载         │
└───────────────────────────────────────────────────────────────┘
```

这个「Go 管理一个子进程、靠 stdout 读就绪标记、退出时分场景处理」的模式，你们已经写过两次：`desktop/main.js` 管 Go sidecar（`READY_MARKER`/`stoppingSidecar`/SIGTERM+5s+SIGKILL）、`cmd/server/parent.go` 的 `watchParent`（父进程消失触发自己退出）。这次是同一个模式的第三次应用，只是反过来——Go 变成了父进程，Python 变成了子进程。值得把"管理子进程生命周期"抽成一个可复用的小 helper，而不是第三次复制粘贴一份几乎一样的代码。

---

## 3. Go 侧新增：`internal/tool/mcp` 包

### 3.1 Client（协议层）

MCP 的 stdio transport 本质是：子进程的 stdin/stdout 上跑 JSON-RPC 2.0，每条消息一行（或用 `Content-Length` 分帧，取决于用哪个 SDK，fastmcp 默认是换行分帧）。Go 侧要实现的方法很少：

```go
type Client struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout *bufio.Scanner
    nextID atomic.Int64
    // 挂起中的请求：id -> 回复 channel
    pending sync.Map
}

func (c *Client) Initialize(ctx context.Context) (ServerInfo, error)
func (c *Client) ListTools(ctx context.Context) ([]RemoteToolSpec, error)
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (RemoteToolResult, error)
```

`RemoteToolSpec.InputSchema` 本身就是 JSON Schema，与 `tool.Spec.Parameters`（`json.RawMessage`）同构，**基本不需要做 schema 转换**，直接透传即可。

### 3.2 Adapter（接入现有 `tool.Tool` 接口）

```go
type mcpToolAdapter struct {
    client *Client
    spec   RemoteToolSpec
    prefix string // 见 3.3 命名空间
}

func (a *mcpToolAdapter) Spec() tool.Spec {
    return tool.Spec{
        Name:        a.prefix + a.spec.Name,
        Description: a.spec.Description,
        Parameters:  a.spec.InputSchema,
        Risk:        tool.RiskDangerous, // 见第 7 节，不信任自报风险等级
    }
}

func (a *mcpToolAdapter) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
    res, err := a.client.CallTool(ctx, a.spec.Name, args)
    if err != nil {
        return tool.Result{}, err
    }
    return tool.Result{Output: res.Text, IsError: res.IsError}, nil
}
```

有了这一层，Registry、ReAct loop、审批机制、SSE 的 `tool.call`/`tool.result` 事件**全部不用改**——它们只认 `tool.Tool` 接口，不关心背后是内置 Go 函数还是一次跨进程 RPC。

### 3.3 命名空间

`Registry.Register` 名字重复会直接 `panic`（`tool.go:131`）。用户自定义工具一旦和内置工具（或将来另一个 MCP server）同名，整个重启流程会直接崩掉，而不是优雅报错。给所有 MCP 来源的工具加前缀，例如 `custom.<name>`，顺便也让模型和审批 UI 一眼看出"这是外部工具"。

---

## 4. Supervisor：生命周期与重启策略

### 4.1 就绪探测

Python sidecar 启动后在 stdout 打一行固定格式的标记（对称于 `desktop/main.js` 里 Go 后端打的 `AGENT_READY <url>`）：

```
MCP_READY
```

Go 侧设超时（建议 10-15s，Python 解释器冷启动 + import 用户模块可能比 Go 二进制慢不少），超时未就绪则视为启动失败。

### 4.2 区分「预期重启」与「崩溃」

这是整个设计里最容易偷工减料的一步，不能省：Python 侧检测到文件变化后，用一个**约定的退出码**（例如 `42`）退出，而不是直接 `sys.exit(0)` 或让异常冒出去。Go 侧 supervisor 的退出处理：

```go
switch {
case exitCode == restartExitCode:
    // 预期内：文件变了，用户主动触发的重启。不告警，直接重新 spawn。
    log.Info("自定义工具文件变化，重启 sidecar")
case ctx.Err() != nil:
    // 我们主动要求停止（app 退出），不是异常。
case err != nil:
    // 真崩溃：退避重试，而不是无限狂重启打满 CPU/日志。
    backoff.RetryOrGiveUp()
}
```

退避策略可以直接照搬 `desktop/main.js` 里"主动停止用标志位而不是靠时序判断"的思路——用一个显式的 `restarting bool`，而不是猜退出码之外的信号。崩溃重试要有上限（比如连续 5 次崩溃后放弃，把自定义工具标记为"不可用"但不拖垮整个 Go 进程——这是关键：Python sidecar 起不来，不该导致 agent 主流程（内置工具 + 对话）也挂掉）。

### 4.3 重启后的重新装配

新 Python 进程就绪后，Go 侧：`Initialize()` → `ListTools()` → 对每个返回的 spec 包一层 adapter → 和内置工具（`tool.DefaultRegistry(...)`）一起塞进一个**全新的** `Registry` → 整体替换给 executor（见第 5 节）。

不要想着"diff 一下只更新变化的那几个工具"：`Registry.Register` 的设计前提是一次性装配，想在同一个 Registry 上增量改需要绕开它的 panic-on-duplicate 保护，得不偿失。重建一整个 Registry 是纯内存操作，开销可以忽略，不值得为此改变 Registry 的设计假设。

---

## 5. 前置修复：`executor.go` 的并发问题

现有 `Executor.SetTools` 的文档注释写的是"必须在 Start 之前调用（并发安全：内部有锁）"（`executor.go:312`），这是"一次性装配"的用法假设。但：

- 写入侧（`SetTools`）确实加了 `e.mu.Lock()`（`executor.go:314-316`）。
- 读取侧（`handleStream` 里 `if e.registry == nil || e.registry.Len() == 0`，`executor.go:675`）**没有加锁**。

只装配一次的场景下这完全没问题；但一旦要支持"运行时收到新 `tools/list` 就再调一次 `SetTools`"，这就是一个真实的 data race：某次正在进行的会话流可能读到新旧指针的中间状态，`go test -race` 下会直接报出来。

**必须先修**，两个选项：

1. 把 `handleStream` 里那次读也纳入 `e.mu`（最小改动，但读路径多一次锁竞争）。
2. 把 `registry` 字段换成 `atomic.Pointer[tool.Registry]`：写入侧 `Store(newRegistry)`，读取侧 `Load()`，读路径零锁开销，写路径是原子指针替换，天然无竞争，且语义上更贴合"整体替换一批工具"这个操作本身。

推荐方案 2——不仅修了竞争，也让"热替换 Registry"这个本来就要做的操作有了语言层面的保证（不会读到半新半旧的状态）。

---

## 6. Python 侧约定

sidecar 本身只需要遵守三件事，具体工具怎么写完全交给用户：

1. **用官方/主流 MCP Python SDK（如 `fastmcp`）做协议层**，不要自己拼 JSON-RPC——这部分是协议细节，没必要重新发明，且能保证将来 Go client 按标准 MCP 协议对接任何其他 server 时行为一致。
2. **就绪后打印 `MCP_READY`，用约定退出码表示"预期重启"**（见 4.1/4.2）。
3. **监听目录要防抖**：编辑器保存文件常见模式是"写临时文件 + rename"，一次保存可能触发 2-3 次文件系统事件；用 `watchfiles` 之类库自带的防抖/合并，或手写一个 300-500ms 的 debounce，避免一次保存连续重启好几次。

一个最小示例（供用户参考，不是本次要交付的产物，但值得在文档里给出，降低用户上手门槛）：

```python
from fastmcp import FastMCP
from pathlib import Path
from watchfiles import watch
import sys, threading

mcp = FastMCP("custom-tools")

TOOLS_DIR = Path(__file__).parent / "tools"
for py_file in TOOLS_DIR.glob("*.py"):
    # 动态 import，注册其中的 @mcp.tool() 函数
    ...

def watch_and_restart():
    for _ in watch(TOOLS_DIR, debounce=400):
        sys.exit(42)  # 约定的"预期重启"退出码

threading.Thread(target=watch_and_restart, daemon=True).start()
print("MCP_READY", flush=True)
mcp.run()  # stdio transport，阻塞运行
```

---

## 7. 安全设计：复用现有哲学，不开特例

用户在这个 Python 进程里写的是任意逻辑，本质上和 `run_command` 风险等级相同——不应该因为包了"工具"或"MCP"的壳就被信任：

- **不管 MCP server 自报的 risk/annotations 是什么，Go 侧 adapter 一律标 `RiskDangerous`**（见 3.2），走现成的审批流程。这是"默认不信任外部代码"的延续，与 `run_command` 默认关闭、需要 `ENABLE_COMMAND_TOOL` 显式开启是同一套哲学。
- 整个功能应该有独立开关（例如 `ENABLE_CUSTOM_TOOLS`），默认关闭。CLI/服务器部署场景下，现有 `make build` 产出的是零依赖单一 Go 二进制（README 强调的卖点），一旦默认启用就隐式要求用户机器装 Python + 对应包，这个"零配置即可跑通"的承诺会被打破——所以必须是"不装 Python/不开开关，Go 主流程完全不受影响"，而不是默认体验的一部分。
- 桌面端打包（Electron）如果也要支持这个功能，意味着要在 `desktop-pack`/`desktop-verify` 里再捆一份独立 Python 运行时（如 python-build-standalone），这是一笔不小的打包工作量，建议**第一阶段只做 CLI/开发模式**，桌面端打包留到用户确认真的需要之后再评估。

---

## 8. 配置项（新增，草案）

```bash
# 是否启用自定义工具 sidecar（默认关闭）
ENABLE_CUSTOM_TOOLS=false
# 自定义工具目录（相对 TOOL_WORKSPACE 或绝对路径）
CUSTOM_TOOLS_DIR=custom_tools
# Python 解释器路径（支持 venv），留空则用 PATH 里的 python3
CUSTOM_TOOLS_PYTHON=
# sidecar 就绪超时
CUSTOM_TOOLS_READY_TIMEOUT=15s
# 连续崩溃多少次后放弃重启（自定义工具不可用，但不影响主流程）
CUSTOM_TOOLS_MAX_CRASH_RETRIES=5
```

---

## 9. 失败模式与边界情况

| 场景 | 处理 |
| --- | --- |
| 机器没装 Python / 没装 `fastmcp` | 启动时检测失败，记一条日志，自定义工具集为空，**不影响 Go 主进程启动** |
| sidecar 连续崩溃超过重试上限 | 标记为不可用，`GET /api/tools` 里不再出现这些工具；下次应用重启可重新尝试 |
| 重启进行中，恰好有一次工具调用正在飞 | 这次调用失败，走现有「失败回喂给模型」路径（`Result.IsError=true` + 文字说明），模型自己决定要不要重试——这是现有架构本来就有的能力，不需要为此新设计兜底逻辑 |
| 模型的上下文里还记着一个刚被移除/改名的工具 | `Registry.Invoke` 已经处理了"工具不存在"（`tool.go:186-189`，返回错误信息列出可用工具），复用即可，不用新写 |
| 文件改动过于频繁（比如用户在用自动格式化工具反复触发保存） | 防抖只能缓解单次保存的多事件问题；如果是持续性的频繁改动，退避重试机制会自然限制重启频率 |

---

## 10. 测试计划

| 测试 | 验证点 |
| --- | --- |
| `TestMCPClientInitializeAndListTools` | 跟一个最小 Python fixture server 握手成功，拿到工具列表 |
| `TestMCPAdapterExecuteForwardsCall` | `tools/call` 的参数/返回值正确转发进 `tool.Result` |
| `TestMCPAdapterAlwaysRiskDangerous` | 即使 fixture server 自报 safe，adapter 也固定标 `RiskDangerous` |
| `TestSupervisorDistinguishesRestartFromCrash` | 约定退出码触发静默重启；非约定退出码触发退避重试 |
| `TestSupervisorGivesUpAfterMaxRetries` | 连续崩溃超过上限后停止重试，且不影响 Go 主进程存活 |
| `TestRegistryAtomicSwapNoRace` | `go test -race`：并发调用期间多次 `SetTools`，不触发竞争检测 |
| `TestToolNamespacePrevendsCollisionPanic` | 自定义工具与内置工具同名时，加前缀后不再 panic |
| `TestInFlightCallDuringRestartFeedsBackError` | 重启瞬间的调用失败信息能正确回喂给模型，不中断整轮对话 |

---

## 11. 推荐的落地顺序

1. 先修第 5 节的并发问题（`atomic.Pointer[tool.Registry]`），这是后续一切的前提，且本身是对现有代码的正确性修复，哪怕暂不做 MCP 也该修。
2. 写 `internal/tool/mcp` 的最小 Client（initialize/list/call）+ Adapter，用一个手写的、几行代码的 Python/Go fixture server 验证协议往返（先不接文件监听/重启）。
3. 加 Supervisor（spawn + ready 探测 + 预期重启 vs 崩溃区分 + 退避）。
4. 配置项接入（`ENABLE_CUSTOM_TOOLS` 等），默认关闭。
5. 给用户的 Python 模板（`fastmcp` + `watchfiles` 防抖 + 约定退出码）作为文档/脚手架交付，而不是代码里内置——用户的工具目录应该是他们项目里的普通文件，不是打包进 Go 二进制的资源。
6. 端到端验证：改一个 `.py` 文件 → 观察 sidecar 重启 → 新工具出现在 `GET /api/tools` → 模型能调用到。
7. 桌面端打包支持（独立 Python 运行时）作为单独的后续事项评估，不纳入第一阶段。

---

## 12. 已知限制

| 限制 | 说明 |
| --- | --- |
| 无进程内热加载 | 任何文件变化都触发整进程重启，不支持"只改一个函数体不重启"的极致热更新（这是有意的设计取舍，见第 0 节） |
| 无远程 MCP server | 本次只做 stdio + 本地进程；HTTP/SSE transport、鉴权留给后续，复用同一份 Client/Adapter |
| 无 `list_changed` 增量通知 | MCP 协议支持 server 在不重启的情况下通知 client 工具集变化，本次没用这条路径（改用更简单可靠的整进程重启），后续如果 Python 侧改成"内存里动态注册/反注册 + 发 `list_changed`"，可以作为性能优化叠加，而不是替代重启方案 |
| 无沙箱隔离 | Python sidecar 里跑的是用户自己的代码，信任等级等同 `run_command`，没有做进程级隔离（容器/seccomp 之类），风险已在第 7 节用「默认 RiskDangerous + 默认关闭」的策略覆盖，但如果用户在多人共享的机器上使用，需要用户自己理解这一点 |
| 桌面端未覆盖 | Electron 打包场景需要额外捆绑 Python 运行时，第一阶段不做 |
