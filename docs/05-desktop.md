# 05 · 桌面客户端（Electron）

## 1. 形态说明

桌面版把现有前后端「装进」一个原生窗口，业务代码零改动：

```
┌─────────────────────────────────────────────────┐
│  Electron 主进程（desktop/main.js）              │
│                                                 │
│  1. 用 PORT=0 启动 Go sidecar                    │
│  2. 从 stdout 读取 AGENT_READY http://...:PORT   │
│  3. 创建 BrowserWindow 加载该地址                │
│  4. 退出时回收子进程                             │
└───────────────┬─────────────────────────────────┘
                │ 单端口 HTTP + SSE
                ▼
┌─────────────────────────────────────────────────┐
│  Go 服务（sidecar，打包内置）                    │
│   /api/*  → REST + SSE                          │
│   /*      → 前端静态产物（SPA 回退）              │
└─────────────────────────────────────────────────┘
```

关键点：窗口加载的是 `http://localhost:<随机端口>`，而**不是** `file://`。因此前端里所有 `/api` 相对路径天然可用，SSE、CORS、缓存策略与浏览器模式完全一致——这是选择「本地 HTTP 服务 + 远程 URL」而非「file:// + 自定义协议」的原因。

---

## 2. 启动

```bash
# 开发态：准备 sidecar + 启动 Electron（自动拉起 Go 后端）
make desktop

# 冒烟测试：真实启动应用，校验 sidecar / API / SSE / 进程回收
make desktop-smoke
```

首次运行会自动 `npm install`（约 417 个包，含 Electron 二进制）。

打包：

```bash
make desktop-pack     # 产出 desktop/release/mac-arm64/Agent Rebuild.app
make desktop-verify   # 打包 + 真实启动打包产物做端到端验证
make desktop-test     # 回归：关窗不弹框 / 关窗即退出 / 孤儿进程守护
make desktop-test-packaged  # 验证打包产物关窗即退出（真实点关闭按钮）
```

产物：`desktop/release/mac-arm64/Agent Rebuild.app`（约 240MB，未签名）。

---

## 3. 端口选择：为什么用 `PORT=0`

固定 `:8080` 在用户机器上极易撞端口（用户可能已有服务在跑）。因此：

1. Electron 以 `PORT=0` 启动 sidecar，内核分配空闲端口；
2. Go 在 `Listen` 之后把**真实端口**写入一行机器可读报文到 stdout：
   ```
   AGENT_READY http://localhost:52341
   ```
3. Electron 扫描 stdout 发现该前缀即拿到地址。

`AGENT_READY` 常量在 `cmd/server/main.go`（`ReadyMarker`）与 `desktop/main.js`（`READY_MARKER`）各有一份，**改动时必须同步**。契约由 `cmd/server/main_test.go` 守护（编译真实二进制、启动、断言报文与端口）。

普通命令行运行时该行只是一行普通输出，不影响人工阅读：

```bash
PORT=0 ./bin/agent-server
# AGENT_READY http://localhost:49565
```

---

## 4. 进程生命周期（最容易出问题的地方）

| 场景 | 处理 |
| --- | --- |
| 关闭窗口 | **所有平台都直接退出应用**（单窗口工具型应用，不做「退到 Dock」） |
| 用户 Cmd+Q / 应用退出 | `before-quit` → 停 sidecar |
| 收到 SIGINT/SIGTERM/SIGHUP | 停 sidecar 后退出 |
| sidecar 就绪前崩溃 | 弹错误框并退出，附带最近 15 行后端日志 |
| sidecar 就绪后崩溃 | 弹错误框提示「后端服务已停止」，退出应用（不留空白窗口） |
| 渲染进程崩溃 | 记录 `render-process-gone` 详情，弹框后退出 |
| 主进程未捕获异常 | 记录日志 + 停 sidecar，不留半可用状态 |
| 重复启动应用 | `requestSingleInstanceLock`，聚焦已有窗口，避免起多个 sidecar |

### 为什么关窗一定要退出

Electron 在 macOS 上的默认惯例是「关窗不退出进程，应用留在 Dock」。对编辑器、浏览器这类多窗口应用合理，但对**单窗口工具**是糟糕体验：

1. 用户点关闭按钮，窗口消失但应用还在 Dock；
2. 点 Dock 图标又弹出窗口（还会重新拉起后端），用户感觉「关不掉、一按就回来」；
3. 后台常驻一个进程默默占内存。

因此本项目显式选择「关窗即退出」（`app.on('window-all-closed')` 直接 `app.quit()`），全平台一致。如果将来要做菜单栏常驻，应当用 `Tray` 显式实现，而不是靠关窗不退出的副作用。

`make desktop-test` 的 `test-close-no-dialog.mjs` 会断言「关窗后 Electron 进程完全消失」——把逻辑改回 macOS 惯例后该测试会立刻失败（已实测验证）。

### 「主动停止」必须用独立标志，不能靠时序判断

这是踩过的坑，值得单独说明：

`window-all-closed` 的触发**早于** `before-quit`。如果只用 `shuttingDown`（在 `before-quit` 里置位）来判断「是不是我们自己要停的」，那么关闭窗口时会：

1. `window-all-closed` → `stopSidecar()` 发 SIGTERM；
2. sidecar 正常退出，`exit(code=0)` 触发；
3. 此时 `shuttingDown` **仍是 false** → 误判为崩溃 → 弹出「后端服务已停止」。

因此用独立的 `stoppingSidecar` 标志，在 `stopSidecar()` 里**先**置位，`exit` 回调看到它就静默返回；新一轮 `startSidecar()` 时重置。

`make desktop-test` 专门守护这一点：它劫持 `dialog.showErrorBox` 记录所有弹框，模拟真实关闭流程，断言「关闭时不弹任何错误框」。把标志位改回旧逻辑后该测试会立刻失败并输出被弹出的错误框内容——已实测验证。

### 孤儿进程：靠后端自己的父进程守护

主进程被 `SIGKILL`（强制结束、系统崩溃、任务管理器杀进程）时，来不及执行任何清理逻辑，只能靠后端自救。

Electron 启动 sidecar 时传入 `--parent-pid=<主进程pid>`，Go 侧每 2 秒探测该进程是否存活，发现消失就主动走优雅关闭：

```bash
# 手工验证
./desktop/sidecar/agent-server --parent-pid=99999   # 99999 不存在
# 2s 后日志出现「收到退出信号，开始优雅关闭」并退出
```

`make desktop-test` 中的 `test-orphan-guard.mjs` 会真实 SIGKILL Electron 主进程，断言 15s 内 sidecar 自行退出、端口释放。

> 注意：`spawn` 时设了 `detached: false`，让子进程与父进程同组，作为第二层保险。

停止方式：POSIX 先 `SIGTERM`，5s 未退出则 `SIGKILL`；Windows 用 `taskkill /f /t` 杀进程树。

`make desktop-smoke` 会显式验证「退出后端口已释放」和「无残留 agent-server 进程」，防止孤儿进程静默累积。

---

## 5. 打包注意事项（踩过的坑）

**`asar` 是归档文件，Go 进程读不了。** electron-builder 默认把 `files` 全部塞进 `app.asar`，而 sidecar 与前端产物都必须以**真实文件**形式存在：

```json
"asarUnpack": ["sidecar/**", "ui/**"]
```

解包后路径为 `Contents/Resources/app.asar.unpacked/{sidecar,ui}`，`main.js` 里按优先级探测这些位置。`make desktop-verify` 会断言两者确实在 asar 之外，并真实启动打包产物跑一次 SSE 对话——这是唯一能发现「开发态正常、打包后界面 404」的手段。

其他：

- **未签名**：`identity: null` 跳过签名。分发给他人时 macOS Gatekeeper 会拦截，需自行签名/公证。
- **图标**：`make-icon` 用纯 Node 手写 PNG 编码生成（无第三方依赖），electron-builder 据此生成各平台格式。
- **架构**：默认只打当前平台（macOS arm64 为 `dir` 目标，便于本地验证）。要出 dmg/nsis/AppImage 改 `package.json` 的 `build.mac/win/linux.target`。

---

## 6. 与 Web 版的差异

| 项 | Web（`make demo`） | 桌面（`make desktop`） |
| --- | --- | --- |
| 端口 | 固定 8080 | 随机空闲端口 |
| 启动后端 | 需手动/脚本 | Electron 自动拉起并回收 |
| 访问方式 | 浏览器开地址 | 原生窗口（`hiddenInset` 深色标题栏） |
| 外链 | 当前标签打开 | 拦截并交给系统浏览器 |
| 渲染进程权限 | 浏览器沙箱 | `contextIsolation` + `sandbox`，`nodeIntegration` 关闭，preload 仅暴露只读环境标识 |
| 会话存储 | 内存（重启即失） | 同左（如需持久化见 docs/03） |

---

## 7. 已知限制

1. **会话不持久化**：仍是内存实现，关闭应用即清空。要持久化需实现 `session.Store` 的 SQLite 版本（接口已预留）。
2. **未签名**：跨机器分发会被 Gatekeeper/Defender 拦截。
3. **体积**：约 240MB，主要来自 Electron 运行时。若在意体积可换 Tauri（需 Rust 工具链，产物约 10MB）。
4. **仅当前平台**：默认只构建运行平台，交叉编译需额外配置。
