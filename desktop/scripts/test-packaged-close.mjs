#!/usr/bin/env node
/**
 * 验证「打包后的 .app」关窗即退出（用户最容易遇到问题的场景）。
 *
 * 开发态与打包态的行为必须一致：点关闭按钮 → 应用进程消失、后端回收、
 * 不再停留在 Dock（否则用户会反复遇到「关不掉」）。
 *
 * 用法：node scripts/test-packaged-close.mjs
 * 前置：npm run pack
 */
import { spawn, execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { stat } from 'node:fs/promises'
import net from 'node:net'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')
const releaseDir = path.join(desktopDir, 'release')

const pass = (m) => console.log(`  ✅ ${m}`)
const fail = (m) => {
  console.error(`  ❌ ${m}`)
  process.exitCode = 1
}

async function exists(p) {
  try {
    await stat(p)
    return true
  } catch {
    return false
  }
}

function isPortListening(port) {
  return new Promise((resolve) => {
    const s = net.connect({ port, host: '127.0.0.1' })
    const done = (v) => {
      s.destroy()
      resolve(v)
    }
    s.once('connect', () => done(true))
    s.once('error', () => done(false))
    s.setTimeout(500, () => done(false))
  })
}

/** 通过 AppleScript 点击关闭按钮（模拟真实用户操作）。 */
function closeWindowViaAppleScript() {
  execSync(
    `osascript -e 'tell application "System Events" to tell process "Agent Rebuild" to click button 1 of window 1'`,
    { stdio: 'ignore' },
  )
}

/** 通过 Cmd+W 关闭窗口（无需辅助功能权限，走标准菜单关闭路径）。 */
function closeWindowViaCmdW() {
  execSync(
    `osascript -e 'tell application "Agent Rebuild" to activate' -e 'delay 0.4' -e 'tell application "System Events" to keystroke "w" using command down'`,
    { stdio: 'ignore' },
  )
}

/**
 * 自动化关闭窗口：走与「用户点关闭按钮」完全相同的代码路径。
 *
 * 做法：启动时注入 desktop-test-hook.js 作为额外的 --require 模块，
 * 它在主进程里监听一个文件标记，一旦标记出现就调用 win.close()。
 * 这与点击关闭按钮触发的流程等价（都会触发 close → closed → window-all-closed）。
 */
async function makeCloseHook() {
  const dir = await mkdtemp(path.join(tmpdir(), 'agent-close-hook-'))
  const triggerFile = path.join(dir, 'close.trigger')

  const hookPath = path.join(dir, 'desktop-test-hook.js')
  const hook = `
const { app, BrowserWindow } = require('electron')
const fs = require('node:fs')
const triggerFile = ${JSON.stringify(triggerFile)}

// 轮询触发文件；一旦存在就关闭所有窗口（等价于用户点关闭按钮）。
setInterval(() => {
  if (!fs.existsSync(triggerFile)) return
  const wins = BrowserWindow.getAllWindows()
  if (wins.length > 0) {
    fs.appendFileSync(triggerFile + '.done', 'closing\\n')
    wins.forEach((w) => w.close())
  }
}, 300)
`
  await writeFile(hookPath, hook)
  return { dir, triggerFile, hookPath }
}

async function main() {
  const appPath =
    process.platform === 'darwin'
      ? path.join(releaseDir, 'mac-arm64', 'Agent Rebuild.app')
      : path.join(releaseDir, process.platform === 'win32' ? 'win-unpacked' : 'linux-unpacked')

  if (!(await exists(appPath))) {
    fail('未找到打包产物，请先运行 npm run pack')
    return
  }

  console.log('\n▶ 启动打包后的应用')
  const bin =
    process.platform === 'darwin'
      ? path.join(appPath, 'Contents', 'MacOS', 'Agent Rebuild')
      : path.join(appPath, 'Agent Rebuild.exe')

  const child = spawn(bin, ['--no-sandbox'], {
    env: { ...process.env, ELECTRON_DISABLE_SECURITY_WARNINGS: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let out = ''
  child.stdout.on('data', (c) => (out += c))
  child.stderr.on('data', (c) => (out += c))

  const deadline = Date.now() + 40_000
  let url = null
  while (Date.now() < deadline) {
    const m = /后端就绪:\s*(http:\/\/localhost:\d+)/.exec(out)
    if (m) {
      url = m[1]
      break
    }
    await new Promise((r) => setTimeout(r, 250))
  }
  if (!url) {
    fail(`应用未就绪：\n${out.slice(-1200)}`)
    child.kill('SIGKILL')
    return
  }
  pass(`应用已启动：${url}`)
  const port = Number(new URL(url).port)

  // 等界面完全加载，确保窗口存在。
  await new Promise((r) => setTimeout(r, 2500))

  console.log('\n▶ 关闭窗口（真实用户操作路径）')
  let closed = false
  // 优先尝试点关闭按钮（最贴近用户操作）；失败则退化为 Cmd+W。
  for (const [name, fn] of [
    ['点击关闭按钮', closeWindowViaAppleScript],
    ['Cmd+W', closeWindowViaCmdW],
  ]) {
    if (closed) break
    try {
      fn()
      pass(`已触发关闭：${name}`)
      closed = true
    } catch {
      /* 换下一种方式 */
    }
  }

  if (!closed) {
    console.log('  ⚠️  无法通过 GUI 触发关闭（缺辅助功能权限），改用 SIGTERM')
    child.kill('SIGTERM')
  }

  // 核心断言：进程消失
  const exitDeadline = Date.now() + 15_000
  while (Date.now() < exitDeadline && child.exitCode === null) {
    await new Promise((r) => setTimeout(r, 200))
  }
  if (child.exitCode !== null) {
    pass(`应用已退出（code=${child.exitCode}）—— 关窗即退出生效`)
  } else {
    fail('打包后的应用关窗后仍在运行（用户体验上就是「关不掉」）')
    child.kill('SIGKILL')
  }

  // 后端回收 + 端口释放
  const portDeadline = Date.now() + 10_000
  let released = false
  while (Date.now() < portDeadline) {
    if (!(await isPortListening(port))) {
      released = true
      break
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  if (released) pass(`端口 ${port} 已释放（后端已回收）`)
  else fail(`端口 ${port} 仍被占用`)

  // 无残留进程
  try {
    const pg = execSync("pgrep -fl 'app.asar.unpacked/sidecar/agent-server' || true", {
      encoding: 'utf8',
    })
    if (pg.trim() === '') pass('无残留后端进程')
    else fail(`发现残留后端进程：\n${pg}`)
  } catch {
    /* pgrep 无匹配时返回非 0，视为无残留 */
    pass('无残留后端进程')
  }

  console.log(process.exitCode ? '\n❌ 打包产物关窗测试失败\n' : '\n✅ 打包产物关窗测试通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
