/**
 * Agent Rebuild 桌面客户端主进程。
 *
 * 职责：
 *   1. 挑一个空闲端口（PORT=0 交给内核分配，避免与本机已占用端口冲突）；
 *   2. 拉起 Go sidecar（打包内置的 agent-server 二进制），把它的 stdout
 *      转发到主进程日志，并从中解析 `AGENT_READY <url>` 就绪行拿到真实端口；
 *   3. 就绪后创建窗口加载该地址（后端同时托管前端静态产物，单端口）；
 *   4. 应用退出时确保子进程被回收（含 macOS 关窗口不退进程、异常崩溃、
 *      SIGINT/SIGTERM、以及 sidecar 提前退出等所有路径）。
 *
 * 安全策略：启用了 contextIsolation + sandbox，禁用 nodeIntegration，
 * 且仅允许加载 localhost 地址（导航/新窗口一律拦截）。
 */
const { app, BrowserWindow, dialog, shell, ipcMain } = require('electron')
const { spawn } = require('node:child_process')
const path = require('node:path')
const fs = require('node:fs')
const { resolveSidecarEnv } = require('./config-loader')

/** 与 cmd/server/main.go 中的 ReadyMarker 保持一致。 */
const READY_MARKER = 'AGENT_READY '

/** sidecar 就绪超时（毫秒）。首启动 + 冷文件系统也足够。 */
const READY_TIMEOUT_MS = 30_000

/** 开发模式：加载 Vite dev server，而不是内置产物。 */
const DEV_MODE = process.env.AGENT_DEV === '1'
const DEV_URL = process.env.AGENT_DEV_URL || 'http://localhost:5173'

/** sidecar 子进程与它的输出缓冲。 */
let sidecar = null
let mainWindow = null
let shuttingDown = false
/**
 * 是否是我们主动要求 sidecar 退出（关窗 / 退出应用 / 重启后端）。
 *
 * 必须与 shuttingDown 区分开：window-all-closed 早于 before-quit 触发，
 * 若只靠 shuttingDown 判断，主动停止会被误判成「后端意外崩溃」并弹错误框。
 */
let stoppingSidecar = false
/** 收集最近日志，崩溃时展示给用户，便于排查。 */
const recentLogs = []

function log(...args) {
  const line = args.join(' ')
  console.log('[desktop]', line)
  recentLogs.push(line)
  if (recentLogs.length > 200) recentLogs.shift()
}

/** 定位 sidecar 二进制：打包后在 app.asar.unpacked/sidecar，开发时在 desktop/sidecar。 */
function resolveSidecarPath() {
  // 可执行文件不能放在 asar 归档内，electron-builder 通过 asarUnpack 解到
  // Resources/app.asar.unpacked/ 下，这里按优先级依次探测。
  const candidates = [
    path.join(process.resourcesPath, 'app.asar.unpacked', 'sidecar', binaryName()),
    path.join(process.resourcesPath, 'sidecar', binaryName()),
    path.join(__dirname, 'sidecar', binaryName()),
    path.join(__dirname, '..', 'bin', 'agent-server'),
  ]
  for (const p of candidates) {
    if (fs.existsSync(p)) return p
  }
  return null
}

function binaryName() {
  return process.platform === 'win32' ? 'agent-server.exe' : 'agent-server'
}

/**
 * 决定桌面端使用的存储驱动。
 *
 * 桌面场景下「关掉应用就丢数据」几乎总是意外，因此默认强制 sqlite。
 * 只有显式设置 AGENT_ALLOW_MEMORY=1 才允许 memory（调试用途）。
 */
function resolveDesktopStoreDriver(env) {
  if (env.AGENT_ALLOW_MEMORY === '1') {
    return 'memory'
  }
  if (env.STORE_DRIVER === 'sqlite') {
    return 'sqlite'
  }
  if (env.STORE_DRIVER && env.STORE_DRIVER !== 'sqlite') {
    // 配置里写了 memory（常见于从命令行版本复制来的 .env）：
    // 桌面端覆盖为 sqlite，并记录原因，避免用户以为数据会持久化。
    log(
      `配置中 STORE_DRIVER=${env.STORE_DRIVER}，桌面端已改为 sqlite（如需内存模式请设 AGENT_ALLOW_MEMORY=1）`,
    )
  }
  return 'sqlite'
}

/** 前端产物目录（由 electron-builder 打包为 asar 外的 ui/）。 */
function resolveUIDir() {
  const candidates = [
    // 打包后：ui 被 asarUnpack 到 app.asar.unpacked/ui
    path.join(process.resourcesPath, 'app.asar.unpacked', 'ui'),
    path.join(process.resourcesPath, 'ui'),
    path.join(__dirname, 'ui'),
    path.join(__dirname, '..', 'web', 'dist'),
  ]
  for (const p of candidates) {
    if (fs.existsSync(path.join(p, 'index.html'))) return p
  }
  return null
}

/**
 * 解析工具工作区根目录。
 *
 * 为什么必须显式指定：
 *   后端 DEFAULT_REGISTRY 在 TOOL_WORKSPACE 为空时会执行 filepath.Abs("")，
 *   拿到的是「进程当前工作目录」。dev 模式下通过 `cd desktop && electron .`
 *   启动，sidecar 继承的 cwd 就是 desktop/，于是所有文件读写都被锁在
 *   agent_rebuild/desktop 里——用户想改项目其它文件时会莫名其妙被拒。
 *
 * 取值优先级：
 *   1. 用户在 config.env 里显式配的 TOOL_WORKSPACE（尊重用户意图）
 *   2. dev 模式：仓库根目录（__dirname 的上一级）
 *   3. 打包模式：用户主目录（app 包内是只读的，不能作为工作区）
 */
function resolveToolWorkspace(devRoot, sidecarEnv) {
  const explicit = (sidecarEnv.TOOL_WORKSPACE || '').trim()
  if (explicit) return path.resolve(explicit)

  const isDev = !app.isPackaged
  const candidate = isDev ? devRoot : app.getPath('home')

  try {
    if (fs.existsSync(candidate) && fs.statSync(candidate).isDirectory()) {
      return path.resolve(candidate)
    }
  } catch {
    /* 目录不可访问时退回主目录 */
  }
  return app.getPath('home')
}

/**
 * 启动 sidecar 并等待就绪。
 * @returns {Promise<string>} 服务地址，如 http://localhost:49565
 */
function startSidecar() {
  const bin = resolveSidecarPath()
  if (!bin) {
    throw new Error(
      '找不到后端程序 agent-server。\n' +
        '开发环境请先执行：make build-binary（或 cd desktop && npm run prepare-sidecar）',
    )
  }

  const uiDir = resolveUIDir()
  if (!uiDir) {
    log('警告：未找到前端产物 ui/，将不会托管界面（仅 API 可用）')
  }

  // 新一轮启动：清除上一轮的「主动停止」标记，避免旧状态影响崩溃判断
  // （macOS 关窗后重新拉起后端的场景）。
  stoppingSidecar = false

  log('启动后端:', bin)
  log('前端产物:', uiDir || '(未找到)')

  // 解析配置：主进程负责找 .env（打包后 cwd 是 /，sidecar 自己找不到）。
  const devRoot = path.resolve(__dirname, '..')
  const { env: sidecarEnv, configFile, effective } = resolveSidecarEnv({
    userDataDir: app.getPath('userData'),
    appDir: path.dirname(bin),
    devRoot,
    processEnv: process.env,
    log,
  })

  // 把关键配置打到日志里（不含密钥明文），便于用户确认「是否真的接上了模型」。
  log('模型配置:',
    `protocol=${effective.protocol}`,
    `provider=${effective.provider}`,
    `model=${effective.model}`,
    `baseURL=${effective.baseURL}`,
    `apiKey=${effective.hasKey ? '已设置' : '未设置'}`,
  )
  if (!configFile) {
    log('未找到配置文件（config.env / .env），将使用环境变量与默认值')
  }
  if (effective.provider === 'mock') {
    log('当前为 Mock 模式：回复为固定演示文案。配置真实模型请见 docs/06-model-setup.md')
  }

  // 通过 --parent-pid 让后端感知本进程：即使主进程被强杀（无法执行清理逻辑），
  // 后端也会自行退出，不会成为孤儿进程占用端口。
  sidecar = spawn(bin, [`--parent-pid=${process.pid}`], {
    env: {
      ...sidecarEnv,
      // 覆盖为桌面端固定值。
      PORT: '0',
      APP_ENV: 'prod',
      LOG_LEVEL: sidecarEnv.LOG_LEVEL || 'info',
      // 让 Go 服务托管前端静态产物，实现单端口运行。
      WEB_DIR: uiDir || '',
      // 数据目录：会话与模型配置持久化到 userData 下。
      // 显式指定而非用平台默认，可保证「卸载应用 = 清除数据」的一致性，
      // 也避免与命令行版本共用同一份数据库。
      DATA_DIR: sidecarEnv.DATA_DIR || path.join(app.getPath('userData'), 'data'),
      // 桌面端默认启用 SQLite 持久化。
      //
      // 注意不能简单用 `|| 'sqlite'`：那样用户显式配的 memory 会被保留，
      // 但桌面场景下「关掉应用就丢数据」几乎总是意外而非本意。
      // 因此只尊重显式 sqlite，其余一律用 sqlite；要 memory 需设 AGENT_ALLOW_MEMORY=1。
      STORE_DRIVER: resolveDesktopStoreDriver(sidecarEnv),
      // 工具工作区：不指定会退化成「sidecar 的 cwd」，
      // 导致文件操作被锁在 desktop/ 目录内。
      TOOL_WORKSPACE: resolveToolWorkspace(devRoot, sidecarEnv),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
    // 显式指定 cwd：不设置会继承 Electron 主进程的工作目录，
    // 在某些启动方式下是 app 包内（只读）或 desktop/，容易踩坑。
    cwd: devRoot,
    // detached=false：子进程与父进程同进程组，父进程被强杀时
    // 系统会一起回收，避免留下孤儿 sidecar 占端口。
    detached: false,
  })

  // 兜底：主进程若被 SIGKILL 等无法捕获的方式终止，
  // 上面的清理逻辑不会执行。这里额外监听进程退出事件做同步清理。
  sidecar.on('spawn', () => {
    log('后端进程已创建 pid=', sidecar && sidecar.pid)
  })

  const readyPromise = new Promise((resolve, reject) => {
    let settled = false
    let buffer = ''

    const finish = (err, url) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      if (err) reject(err)
      else resolve(url)
    }

    const timer = setTimeout(() => {
      finish(
        new Error(
          `后端未在 ${READY_TIMEOUT_MS / 1000}s 内就绪。\n最近日志：\n${recentLogs.slice(-15).join('\n')}`,
        ),
      )
    }, READY_TIMEOUT_MS)

    sidecar.stdout.setEncoding('utf8')
    sidecar.stdout.on('data', (chunk) => {
      // stdout 里既有结构化日志，也有就绪行，按行扫描。
      buffer += chunk
      let idx
      while ((idx = buffer.indexOf('\n')) !== -1) {
        const line = buffer.slice(0, idx).trimEnd()
        buffer = buffer.slice(idx + 1)
        if (line.includes(READY_MARKER)) {
          const url = line.slice(line.indexOf(READY_MARKER) + READY_MARKER.length).trim()
          log('后端就绪:', url)
          finish(null, url)
        } else if (line) {
          log('[server]', line)
        }
      }
    })

    sidecar.stderr.setEncoding('utf8')
    sidecar.stderr.on('data', (chunk) => {
      for (const line of String(chunk).split('\n')) {
        if (line.trim()) log('[server:err]', line.trimEnd())
      }
    })

    sidecar.on('error', (err) => {
      finish(new Error(`无法启动后端进程: ${err.message}`))
    })

    sidecar.on('exit', (code, signal) => {
      log(`后端进程退出 code=${code} signal=${signal}`)
      sidecar = null

      // 主动停止（关窗/退出/重启）：属于预期退出，静默处理，不弹错误框。
      if (stoppingSidecar) {
        stoppingSidecar = false
        if (!settled) {
          // 还没就绪就被我们停掉（例如启动后立刻关窗），用普通失败结束等待。
          finish(new Error('后端进程已被停止'))
        }
        return
      }

      if (!settled) {
        finish(
          new Error(
            `后端进程启动后立即退出（code=${code}, signal=${signal}）。\n` +
              `最近日志：\n${recentLogs.slice(-15).join('\n')}`,
          ),
        )
        return
      }

      // 非主动停止：确实异常退出了。应用若仍在运行则提示并关闭。
      if (!shuttingDown) {
        const how =
          code === 0
            ? '后端进程已退出（code=0，正常退出但非应用主动触发，可能是收到外部信号）'
            : `后端进程意外退出（code=${code}${signal ? `, signal=${signal}` : ''}）`
        dialog.showErrorBox(
          '后端服务已停止',
          `${how}。应用将关闭。\n\n最近日志：\n${recentLogs.slice(-10).join('\n')}`,
        )
        app.quit()
      }
    })
  })

  return readyPromise
}

function createWindow(url) {
  const isDesktopMac = process.platform === 'darwin'
  mainWindow = new BrowserWindow({
    width: 1180,
    height: 800,
    minWidth: 860,
    minHeight: 560,
    backgroundColor: '#0d0d0d',
    title: 'Agent Rebuild',
    // macOS 用 hiddenInset：标题栏隐藏、红绿灯浮在内容上，视觉更贴近 Codex。
    // 代价是失去系统标题栏的默认交互，需要自行实现拖拽与双击放大（见下）。
    titleBarStyle: isDesktopMac ? 'hiddenInset' : 'default',
    trafficLightPosition: { x: 14, y: 14 },
    webPreferences: {
      // 界面不需要任何 Node 能力：完全隔离，降低攻击面。
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      preload: path.join(__dirname, 'preload.js'),
    },
  })

  // ---- 双击放大 ----
  //
  // hiddenInset 下没有系统标题栏，macOS 的「双击标题栏缩放」不会触发。
  // 渲染层监听顶栏双击后通过 IPC（window:toggle-maximize）请求切换状态，
  // 主进程侧只需提供 handler（见 app.whenReady 内）。
  //
  // 这里额外加一个键盘快捷方式作为补充入口。
  mainWindow.webContents.on('before-input-event', (event, input) => {
    if (!isDesktopMac) return
    // Cmd+Ctrl+F：macOS 的全屏快捷键
    if (input.type === 'keyDown' && input.meta && input.control && input.key.toLowerCase() === 'f') {
      event.preventDefault()
      mainWindow.setFullScreen(!mainWindow.isFullScreen())
    }
  })

  // 只允许停留在本应用地址：外部链接交给系统浏览器。
  const allowedOrigin = new URL(url).origin
  const guard = (event, target) => {
    try {
      if (new URL(target).origin !== allowedOrigin) {
        event.preventDefault()
        void shell.openExternal(target)
      }
    } catch {
      event.preventDefault()
    }
  }
  mainWindow.webContents.on('will-navigate', (event, target) => guard(event, target))
  mainWindow.webContents.setWindowOpenHandler(({ url: target }) => {
    void shell.openExternal(target)
    return { action: 'deny' }
  })

  // 渲染进程崩溃：记录并提示（不静默失败）。
  mainWindow.webContents.on('render-process-gone', (_e, details) => {
    log('渲染进程异常:', JSON.stringify(details))
    if (!shuttingDown) {
      dialog.showErrorBox('界面进程异常', `界面已崩溃（${details.reason}）。应用将关闭。`)
      app.quit()
    }
  })

  mainWindow.on('closed', () => {
    mainWindow = null
  })

  log('加载界面:', url)
  mainWindow.loadURL(url)

  // ---- UI 运行时验证钩子（仅 VERIFY_UI=1 时启用）----
  //
  // 为什么放在主进程而不是用浏览器测试工具：
  // 这些行为（user-select、app-region、窗口 IPC）依赖 Electron 环境，
  // 只有真实应用里读到的 computedStyle 才算数。
  //
  // 生产运行时该分支永不进入，不影响任何行为。
  if (process.env.VERIFY_UI === '1') {
    mainWindow.webContents.once('did-finish-load', () => {
      setTimeout(async () => {
        try {
          const probe = await mainWindow.webContents.executeJavaScript(`
            (function () {
              var pick = function (sel) {
                var el = document.querySelector(sel)
                if (!el) return 'NO_EL'
                var st = getComputedStyle(el)
                return st.userSelect || st.webkitUserSelect || 'unknown'
              }
              var region = function (sel) {
                var el = document.querySelector(sel)
                if (!el) return 'NO_EL'
                var st = getComputedStyle(el)
                return st.getPropertyValue('-webkit-app-region') || 'none'
              }
              var head = document.querySelector('.sidebar-head')
              var app = document.querySelector('.app')
              return JSON.stringify({
                headPadTop: head ? getComputedStyle(head).paddingTop : 'NO_EL',
                isDesktopClass: app ? app.className.indexOf('is-desktop') >= 0 : false,
                hasWindowControl: typeof window.windowControl,
                bodySelect: pick('body'),
                mdSelect: pick('.md'),
                textareaSelect: pick('textarea'),
                statusbarSelect: pick('.statusbar'),
                brandDrag: region('.brand'),
                topbarButtonDrag: region('.topbar button'),
              })
            })()
          `)
          const base = JSON.parse(probe)
          // 实测一次 IPC：切换最大化并读回状态
          const toggleWorked = await mainWindow.webContents.executeJavaScript(
            'window.windowControl.toggleMaximize()',
          )
          base.toggleWorked = toggleWorked === true
          log('VERIFY_UI_RESULT', JSON.stringify(base))
        } catch (err) {
          log('VERIFY_UI_FAIL', err.message)
        }
      }, 2000)
    })
  }
}

/** 确保 sidecar 被终止（多路径调用，幂等）。 */
function stopSidecar() {
  // 无论当前是否有进程，都先标记为「主动停止」：
  // stopSidecar 与 exit 事件之间存在竞态，靠标志位而非时序判断意图。
  stoppingSidecar = true
  if (!sidecar) return
  const proc = sidecar
  sidecar = null
  try {
    if (process.platform === 'win32') {
      // Windows 下 kill 不会连带子进程树，用 taskkill 兜底。
      spawn('taskkill', ['/pid', String(proc.pid), '/f', '/t'], { windowsHide: true })
    } else {
      proc.kill('SIGTERM')
      // 5s 内没退出则强杀，防止残留占端口。
      const killer = setTimeout(() => {
        try {
          proc.kill('SIGKILL')
        } catch {
          /* 已退出 */
        }
      }, 5000)
      if (killer.unref) killer.unref()
    }
  } catch (err) {
    log('停止后端失败:', err.message)
    stoppingSidecar = false
  }
}

// 单实例锁：重复启动时聚焦已有窗口，避免起多个 sidecar。
const gotLock = app.requestSingleInstanceLock()
if (!gotLock) {
  app.quit()
} else {
  app.on('second-instance', () => {
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore()
      mainWindow.focus()
    }
  })

  app.whenReady().then(async () => {
    // ---- 窗口控制 IPC ----
    //
    // hiddenInset 标题栏下没有系统标题栏，双击放大需要渲染层主动请求。
    // 这些 handler 只做窗口状态切换，不接受任何参数，无注入风险。
    ipcMain.handle('window:toggle-maximize', () => {
      const win = BrowserWindow.getFocusedWindow() || mainWindow
      if (!win) return false
      if (win.isMaximized()) {
        win.unmaximize()
        return false
      }
      win.maximize()
      return true
    })
    ipcMain.handle('window:is-maximized', () => {
      const win = BrowserWindow.getFocusedWindow() || mainWindow
      return win ? win.isMaximized() : false
    })

    // ---- 文件夹选择 IPC ----
    //
    // workspace 管理弹窗（WorkspaceManager.tsx）需要让用户用系统原生的
    // 文件夹选择器挑目录，而不是手打绝对路径。渲染层被 sandbox 隔绝，
    // 拿不到 Node 的 fs/path，也拿不到真实的文件系统路径（浏览器的
    // <input type="file"> 出于安全不会暴露绝对路径），所以只能经主进程
    // 调 Electron 的 dialog.showOpenDialog，再把选中的路径传回去。
    //
    // allowMultiple 支持一次选多个目录（macOS/Windows 原生对话框都支持
    // 多选文件夹），减少用户一个一个点"+"的次数。取消选择返回空数组，
    // 而不是 null/undefined，渲染层不需要再判空。
    ipcMain.handle('dialog:select-folders', async (_event, options) => {
      const win = BrowserWindow.getFocusedWindow() || mainWindow
      const allowMultiple = Boolean(options && options.allowMultiple)
      const properties = ['openDirectory', 'createDirectory']
      if (allowMultiple) properties.push('multiSelections')
      const result = win
        ? await dialog.showOpenDialog(win, { properties })
        : await dialog.showOpenDialog({ properties })
      if (result.canceled) return []
      return result.filePaths
    })

    let url
    try {
      if (DEV_MODE) {
        // 开发模式：界面来自 Vite dev server，但仍需要后端提供 /api。
        // 若开发者已单独 `make run` 起了后端，这里启动会因端口占用失败，
        // 属于预期情况，只记录不阻塞。
        await startSidecar().catch((err) => {
          log('开发模式未能自启后端（若已单独运行 make run 属正常）:', err.message)
        })
        url = DEV_URL
      } else {
        url = await startSidecar()
      }
      createWindow(url)
    } catch (err) {
      dialog.showErrorBox('启动失败', String(err.message || err))
      app.quit()
    }

    app.on('activate', () => {
      // 关窗即退出，正常流程下不会走到这里。
      // 仅在「窗口被销毁但应用仍存活」的异常态下补救重建。
      if (shuttingDown) return
      if (BrowserWindow.getAllWindows().length > 0) return
      if (DEV_MODE) {
        createWindow(DEV_URL)
        return
      }
      startSidecar()
        .then((u) => createWindow(u))
        .catch((err) => {
          dialog.showErrorBox('启动失败', String(err.message || err))
          app.quit()
        })
    })
  })
}

// 所有窗口关闭：直接退出应用（所有平台一致）。
//
// 这是单窗口工具型应用，没有后台常驻需求，因此不采用 macOS「关窗退到 Dock」
// 的惯例——否则用户点了关闭按钮却感觉「关不掉」（应用仍在 Dock，且再次点击
// 会重新拉起后端、重新出现窗口）。若将来需要菜单栏常驻，应显式改为
// Menu/Tray 形态，而不是依赖关窗不退出。
app.on('window-all-closed', () => {
  stopSidecar()
  app.quit()
})

app.on('before-quit', () => {
  shuttingDown = true
  stopSidecar()
})

// 进程被信号终止时也确保子进程回收。
for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(sig, () => {
    shuttingDown = true
    stopSidecar()
    app.quit()
  })
}

// 主进程未捕获异常：记录日志后退出，不留在半可用状态。
process.on('uncaughtException', (err) => {
  log('未捕获异常:', err && err.stack ? err.stack : String(err))
  dialog.showErrorBox('应用异常', String((err && err.message) || err))
  stopSidecar()
  app.quit()
})
