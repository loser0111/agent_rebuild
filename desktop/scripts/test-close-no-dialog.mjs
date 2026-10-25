#!/usr/bin/env node
/**
 * 关闭应用时不得弹出「后端服务已停止」错误框（回归测试）。
 *
 * 背景：window-all-closed 触发早于 before-quit，早期实现只靠 shuttingDown
 * 判断意图，导致主动关闭窗口时 sidecar 的 exit(code=0) 被误判为崩溃并弹框。
 *
 * 本测试通过注入一个假的 dialog 记录所有弹框，然后模拟真实关闭流程：
 *   1. 启动应用，等 sidecar 就绪；
 *   2. 触发窗口关闭（等价于用户点红色关闭按钮）；
 *   3. 断言：进程退出、端口释放、且**没有**任何错误弹框。
 *
 * 用法：node scripts/test-close-no-dialog.mjs
 */
import { spawn, execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { writeFile, mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import net from 'node:net'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')

const pass = (m) => console.log(`  ✅ ${m}`)
const fail = (m) => {
  console.error(`  ❌ ${m}`)
  process.exitCode = 1
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

/**
 * 生成一个测试用入口：包装真实的 main.js，但在加载前劫持 dialog，
 * 把所有 showErrorBox 调用写入一个标记文件，供测试断言。
 */
async function makeHarness() {
  const dir = await mkdtemp(path.join(tmpdir(), 'agent-close-test-'))
  const markerFile = path.join(dir, 'dialogs.log')

  const harness = `
// 测试壳：劫持 dialog.showErrorBox 记录弹框，然后加载真实主进程逻辑。
const { app, dialog } = require('electron')
const fs = require('node:fs')
const path = require('node:path')

const markerFile = ${JSON.stringify(markerFile)}
fs.writeFileSync(markerFile, '')

// 记录所有错误框（测试关注的是「关闭时不应弹框」）。
dialog.showErrorBox = (title, content) => {
  fs.appendFileSync(markerFile, JSON.stringify({ title, content }) + '\\n')
}

// 等待 sidecar 就绪后，模拟用户点关闭按钮：先关窗，再走 window-all-closed。
// 这里不直接调用 app.quit()，以便复现 window-all-closed 早于 before-quit 的时序。
const originalWhenReady = app.whenReady.bind(app)
app.whenReady = () => originalWhenReady()

require(${JSON.stringify(path.join(desktopDir, 'main.js'))})

// 关闭动作：等界面加载完成后关闭所有窗口。
app.on('browser-window-created', (_e, win) => {
  win.webContents.once('did-finish-load', () => {
    fs.appendFileSync(markerFile, 'WINDOW_LOADED\\n')
    // 给 UI 一点时间完成首屏请求，再关闭。
    setTimeout(() => win.close(), 800)
  })
})
`
  const harnessPath = path.join(dir, 'harness.js')
  await writeFile(harnessPath, harness)
  return { dir, markerFile, harnessPath }
}

async function main() {
  console.log('\n▶ 回归测试：关闭应用不应弹出错误框')

  const { dir, markerFile, harnessPath } = await makeHarness()

  const child = spawn(
    process.platform === 'win32' ? 'npx.cmd' : 'npx',
    ['electron', harnessPath, '--no-sandbox'],
    {
      cwd: desktopDir,
      env: { ...process.env, ELECTRON_DISABLE_SECURITY_WARNINGS: '1' },
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  )

  let out = ''
  child.stdout.on('data', (c) => (out += c))
  child.stderr.on('data', (c) => (out += c))

  // 等 sidecar 就绪
  const deadline = Date.now() + 40_000
  let url = null
  while (Date.now() < deadline) {
    const m = /后端就绪:\s*(http:\/\/localhost:\d+)/.exec(out)
    if (m) {
      url = m[1]
      break
    }
    if (/启动失败/.test(out)) break
    await new Promise((r) => setTimeout(r, 250))
  }

  if (!url) {
    fail(`应用未就绪：\n${out.slice(-1500)}`)
    child.kill('SIGKILL')
    await rm(dir, { recursive: true, force: true })
    return
  }
  pass(`应用已启动：${url}`)
  const port = Number(new URL(url).port)

  // 等待关闭流程走完。
  // 关窗即退出：所有平台都应真正退出进程（不再保留 Dock 后台）。
  // 注意 child.pid 是 npx 包装进程，需同时确认真实 Electron 进程已消失。
  const electronAlive = () => {
    try {
      const out2 = execSync("pgrep -f 'Electron.app/Contents/MacOS/Electron' || true", {
        encoding: 'utf8',
      })
      return out2.trim().length > 0
    } catch {
      return false
    }
  }

  const exitDeadline = Date.now() + 15_000
  while (Date.now() < exitDeadline && electronAlive()) {
    await new Promise((r) => setTimeout(r, 200))
  }
  if (electronAlive()) {
    fail('关闭窗口后 Electron 进程仍在运行（用户会感觉「关不掉」）')
    execSync("pkill -9 -f 'Electron.app/Contents/MacOS/Electron' || true")
  } else {
    pass('关闭窗口后应用进程已完全退出')
  }

  // 端口释放
  let released = false
  const portDeadline = Date.now() + 10_000
  while (Date.now() < portDeadline) {
    if (!(await isPortListening(port))) {
      released = true
      break
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  if (released) pass(`端口 ${port} 已释放（sidecar 已回收）`)
  else fail(`端口 ${port} 仍被占用`)

  // 核心断言：不应有任何错误弹框
  const { readFile } = await import('node:fs/promises')
  const logContent = await readFile(markerFile, 'utf8')
  const dialogLines = logContent.split('\n').filter((l) => l.startsWith('{'))

  if (logContent.includes('WINDOW_LOADED')) pass('界面已加载完成（关闭流程真实触发）')
  else fail('界面未加载完成，测试可能未真正复现关闭路径')

  if (dialogLines.length === 0) {
    pass('关闭过程中未弹出任何错误框（无「后端服务已停止」误报）')
  } else {
    fail(`关闭时仍弹出 ${dialogLines.length} 个错误框：\n${dialogLines.join('\n')}`)
  }

  // 无论哪种平台，最后都收掉测试进程。
  if (child.exitCode === null) child.kill('SIGTERM')
  await new Promise((r) => setTimeout(r, 1000))
  if (child.exitCode === null) child.kill('SIGKILL')

  await rm(dir, { recursive: true, force: true })
  console.log(process.exitCode ? '\n❌ 关闭流程回归测试失败\n' : '\n✅ 关闭流程回归测试通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
