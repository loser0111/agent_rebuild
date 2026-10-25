#!/usr/bin/env node
/**
 * 验证「打包后的 .app」能否真正运行（而不只是开发态能跑）。
 *
 * 这一步很关键：asar 打包后，Go 进程读不到 asar 内的文件，
 * 前端产物与 sidecar 必须是 asarUnpack 出来的真实目录。
 *
 * 用法：node scripts/verify-package.mjs
 * 前置：npm run pack
 */
import { spawn } from 'node:child_process'
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
    s.setTimeout(1000, () => done(false))
  })
}

/** 按平台定位打包后的可执行文件。 */
async function findExecutable() {
  const candidates =
    process.platform === 'darwin'
      ? [
          path.join(releaseDir, 'mac-arm64', 'Agent Rebuild.app', 'Contents', 'MacOS', 'Agent Rebuild'),
          path.join(releaseDir, 'mac', 'Agent Rebuild.app', 'Contents', 'MacOS', 'Agent Rebuild'),
        ]
      : process.platform === 'win32'
        ? [path.join(releaseDir, 'win-unpacked', 'Agent Rebuild.exe')]
        : [path.join(releaseDir, 'linux-unpacked', 'agent-rebuild-desktop')]

  for (const p of candidates) {
    if (await exists(p)) return p
  }
  return null
}

async function main() {
  console.log('\n▶ 校验打包产物结构')

  const exe = await findExecutable()
  if (!exe) {
    fail('未找到打包后的可执行文件，请先运行 npm run pack')
    return
  }
  pass(`可执行文件：${path.relative(desktopDir, exe)}`)

  // 关键检查：sidecar 与 ui 必须在 asar 之外（Go 读不了 asar）。
  const resourcesDir = path.dirname(path.dirname(exe)) // .../Contents
  const unpacked = path.join(resourcesDir, 'Resources', 'app.asar.unpacked')

  const sidecarPath = path.join(unpacked, 'sidecar', 'agent-server')
  if (await exists(sidecarPath)) pass('sidecar 在 asar 之外（app.asar.unpacked/sidecar）')
  else fail('sidecar 不在 app.asar.unpacked 下，Go 进程将无法启动')

  const uiIndex = path.join(unpacked, 'ui', 'index.html')
  if (await exists(uiIndex)) pass('前端产物在 asar 之外（app.asar.unpacked/ui）')
  else fail('前端产物不在 app.asar.unpacked 下，界面会 404')

  // 检查主进程依赖的本地模块是否都被打包进 asar。
  // 这类遗漏只在打包态暴露（开发态 require 正常），必须显式验证。
  const asarPath = path.join(resourcesDir, 'Resources', 'app.asar')
  if (await exists(asarPath)) {
    try {
      // 该脚本是 ESM，需用动态 import 加载 CJS 模块。
      const { createRequire } = await import('node:module')
      const require2 = createRequire(import.meta.url)
      const asar = require2('@electron/asar')
      const list = asar.listPackage(asarPath)
      const required = ['/main.js', '/preload.js', '/config-loader.js']
      const missing = required.filter((f) => !list.includes(f))
      if (missing.length === 0) {
        pass(`asar 内包含全部本地模块（${required.length} 个）`)
      } else {
        fail(`asar 缺少本地模块（会导致启动即崩溃）：${missing.join(', ')}`)
      }
    } catch (err) {
      fail(`无法检查 asar 内容：${err.message}`)
    }
  }

  // 启动打包后的 app，验证端到端。
  console.log('\n▶ 启动打包后的应用')
  const child = spawn(exe, ['--no-sandbox'], {
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
    if (/启动失败|找不到后端程序/.test(out)) break
    await new Promise((r) => setTimeout(r, 250))
  }

  if (!url) {
    fail(`打包后的应用未就绪。输出：\n${out.slice(-2000)}`)
    child.kill('SIGKILL')
    return
  }
  pass(`打包后的应用已启动 sidecar：${url}`)

  try {
    const health = await fetch(`${url}/api/health`).then((r) => r.json())
    if (health.status === 'ok') pass('/api/health 正常')
    else fail(`/api/health 异常：${JSON.stringify(health)}`)
  } catch (err) {
    fail(`/api/health 失败：${err.message}`)
  }

  try {
    const resp = await fetch(`${url}/`)
    const html = await resp.text()
    if (resp.ok && html.includes('id="root"')) pass('界面由打包后的 sidecar 正常托管')
    else fail(`界面异常：status=${resp.status}（很可能是 ui 没解包到 asar 外）`)
  } catch (err) {
    fail(`界面请求失败：${err.message}`)
  }

  try {
    const created = await fetch(`${url}/api/sessions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    }).then((r) => r.json())
    const resp = await fetch(`${url}/api/sessions/${created.session.id}/messages`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', accept: 'text/event-stream' },
      body: JSON.stringify({ content: '打包产物验证' }),
    })
    const text = await resp.text()
    if (text.includes('event: message.done')) pass('SSE 流式对话在打包产物中可用')
    else fail('打包产物中 SSE 序列不完整')
    await fetch(`${url}/api/sessions/${created.session.id}`, { method: 'DELETE' })
  } catch (err) {
    fail(`SSE 测试失败：${err.message}`)
  }

  const port = Number(new URL(url).port)
  child.kill('SIGTERM')
  const exitDeadline = Date.now() + 15_000
  while (Date.now() < exitDeadline && child.exitCode === null) {
    await new Promise((r) => setTimeout(r, 200))
  }
  if (child.exitCode !== null) pass('应用已退出')
  else {
    fail('应用未在 15s 内退出')
    child.kill('SIGKILL')
  }

  const portDeadline = Date.now() + 10_000
  let released = false
  while (Date.now() < portDeadline) {
    if (!(await isPortListening(port))) {
      released = true
      break
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  if (released) pass(`端口 ${port} 已释放（无孤儿 sidecar）`)
  else fail(`端口 ${port} 仍被占用`)

  console.log(process.exitCode ? '\n❌ 打包产物验证失败\n' : '\n✅ 打包产物验证通过\n')
}

main().catch((err) => {
  console.error('验证脚本异常:', err)
  process.exit(1)
})
