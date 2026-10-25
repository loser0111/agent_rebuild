#!/usr/bin/env node
/**
 * Electron 壳冒烟测试（无头环境可跑）。
 *
 * 验证主进程的核心契约，而不依赖 GUI 交互：
 *   1. sidecar 能被拉起，并解析出 AGENT_READY 报文；
 *   2. 该地址上 /api/health 与 /（前端首页）都可访问；
 *   3. 应用退出后 sidecar 子进程被回收，端口释放（无孤儿进程）。
 *
 * 做法：用 xvfb 或离屏模式启动真实 Electron 应用，从主进程日志里提取地址，
 * 然后由脚本自己发 HTTP 请求；最后关闭应用并检查进程与端口。
 *
 * 用法：node scripts/smoke-electron.mjs
 */
import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import net from 'node:net'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')

const pass = (m) => console.log(`  ✅ ${m}`)
const fail = (m) => {
  console.error(`  ❌ ${m}`)
  process.exitCode = 1
}

/** 等端口释放（检查是否还有进程监听）。 */
function isPortListening(port) {
  return new Promise((resolve) => {
    const socket = net.connect({ port, host: '127.0.0.1' })
    const done = (v) => {
      socket.destroy()
      resolve(v)
    }
    socket.once('connect', () => done(true))
    socket.once('error', () => done(false))
    socket.setTimeout(1000, () => done(false))
  })
}

async function main() {
  console.log('\n▶ 启动 Electron 应用（离屏模式）')

  const child = spawn(
    process.platform === 'win32' ? 'npx.cmd' : 'npx',
    ['electron', '.', '--no-sandbox'],
    {
      cwd: desktopDir,
      env: { ...process.env, ELECTRON_DISABLE_SECURITY_WARNINGS: '1' },
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  )

  let out = ''
  const onData = (chunk) => {
    out += chunk
  }
  child.stdout.on('data', onData)
  child.stderr.on('data', onData)

  // 等待主进程打印「后端就绪」或超时。
  const deadline = Date.now() + 40_000
  let url = null
  while (Date.now() < deadline) {
    const m = /后端就绪:\s*(http:\/\/localhost:\d+)/.exec(out)
    if (m) {
      url = m[1]
      break
    }
    if (/启动失败|后端进程启动后立即退出/.test(out)) break
    await new Promise((r) => setTimeout(r, 250))
  }

  if (!url) {
    fail(`未拿到后端地址。输出：\n${out.slice(-2000)}`)
    child.kill('SIGKILL')
    return
  }
  pass(`sidecar 已启动并就绪：${url}`)
  if (/加载界面:/.test(out)) pass('主窗口已请求加载界面')
  else fail('未见「加载界面」日志')

  const port = Number(new URL(url).port)

  // HTTP 校验
  try {
    const health = await fetch(`${url}/api/health`).then((r) => r.json())
    if (health.status === 'ok') pass(`/api/health 正常（provider=${health.provider}）`)
    else fail(`/api/health 异常：${JSON.stringify(health)}`)
  } catch (err) {
    fail(`/api/health 请求失败：${err.message}`)
  }

  try {
    const resp = await fetch(`${url}/`)
    const html = await resp.text()
    if (resp.ok && html.includes('id="root"')) pass('前端首页由 sidecar 托管并含 #root 挂载点')
    else fail(`首页异常：status=${resp.status}`)
  } catch (err) {
    fail(`首页请求失败：${err.message}`)
  }

  // 端到端：通过 sidecar 跑一次真实 SSE 流式对话。
  try {
    const created = await fetch(`${url}/api/sessions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    }).then((r) => r.json())
    const sid = created.session.id

    const resp = await fetch(`${url}/api/sessions/${sid}/messages`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', accept: 'text/event-stream' },
      body: JSON.stringify({ content: '桌面壳冒烟测试' }),
    })
    const text = await resp.text()
    if (text.includes('event: message.start') && text.includes('event: message.done')) {
      pass('SSE 流式对话可用（start → done）')
    } else {
      fail('SSE 事件序列不完整')
    }
    await fetch(`${url}/api/sessions/${sid}`, { method: 'DELETE' })
  } catch (err) {
    fail(`SSE 测试失败：${err.message}`)
  }

  // 退出并验证进程回收
  console.log('\n▶ 关闭应用，验证子进程回收')
  child.kill('SIGTERM')

  const exitDeadline = Date.now() + 15_000
  let exited = false
  while (Date.now() < exitDeadline) {
    if (child.exitCode !== null || child.signalCode !== null) {
      exited = true
      break
    }
    await new Promise((r) => setTimeout(r, 200))
  }
  if (exited) pass('Electron 主进程已退出')
  else {
    fail('Electron 主进程未在 15s 内退出')
    child.kill('SIGKILL')
  }

  // 端口应被释放（sidecar 已随之终止）
  let released = false
  const portDeadline = Date.now() + 10_000
  while (Date.now() < portDeadline) {
    if (!(await isPortListening(port))) {
      released = true
      break
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  if (released) pass(`端口 ${port} 已释放（sidecar 已回收，无孤儿进程）`)
  else fail(`端口 ${port} 仍被占用，sidecar 可能残留`)

  // 检查是否还有 agent-server 进程残留
  const ps = spawn('pgrep', ['-fl', 'desktop/sidecar/agent-server'], { stdio: ['ignore', 'pipe', 'ignore'] })
  let psOut = ''
  ps.stdout.on('data', (c) => (psOut += c))
  await new Promise((r) => ps.on('close', r))
  if (psOut.trim() === '') pass('无残留 sidecar 进程')
  else fail(`发现残留进程：\n${psOut}`)

  console.log(process.exitCode ? '\n❌ Electron 壳冒烟失败\n' : '\n✅ Electron 壳冒烟通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
