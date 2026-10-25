#!/usr/bin/env node
/**
 * 验证「父进程被强杀」时后端会自动退出（不留孤儿进程）。
 *
 * 这是最坏情况：主进程收到 SIGKILL 时来不及执行任何清理逻辑，
 * 只能靠后端自己的 --parent-pid 守护感知父进程消失。
 *
 * 用法：node scripts/test-orphan-guard.mjs
 */
import { spawn, execSync } from 'node:child_process'
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

function countSidecars() {
  try {
    const out = execSync("pgrep -fl 'desktop/sidecar/agent-server' || true", { encoding: 'utf8' })
    return out.split('\n').filter((l) => l.trim()).length
  } catch {
    return 0
  }
}

async function main() {
  console.log('\n▶ 孤儿进程守护测试（父进程被 SIGKILL）')

  const before = countSidecars()
  if (before > 0) {
    fail(`测试开始前已有 ${before} 个残留 sidecar，请先清理`)
    return
  }

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
  const port = Number(new URL(url).port)
  pass(`应用已启动，sidecar 监听 ${port}`)
  if (countSidecars() !== 1) fail(`应有 1 个 sidecar，实际 ${countSidecars()}`)
  else pass('当前有 1 个 sidecar 进程')

  // 关键动作：强杀主进程（不给它清理机会）。
  // 注意：child.pid 是 npx 包装进程，SIGKILL 它并不会杀掉真正的 Electron 主进程，
  // 因此这里显式查出 Electron 主进程 PID（它才是 sidecar 的父进程）。
  console.log('\n▶ 对 Electron 主进程发送 SIGKILL（模拟崩溃/强制结束）')
  let electronPid = child.pid
  try {
    const out2 = execSync("pgrep -f 'Electron.app/Contents/MacOS/Electron' | head -1", {
      encoding: 'utf8',
    }).trim()
    if (out2) electronPid = Number(out2)
  } catch {
    /* 非 macOS 或未匹配到，退化为 child.pid */
  }
  console.log(`  目标 PID = ${electronPid}`)

  try {
    process.kill(electronPid, 'SIGKILL')
  } catch (err) {
    fail(`发送 SIGKILL 失败：${err.message}`)
    return
  }

  // 后端守护有 2s 轮询周期，给足 15s 观察窗口。
  console.log('  等待后端自行退出（守护轮询周期 2s，最多观察 15s）…')
  let gone = false
  const observeDeadline = Date.now() + 15_000
  while (Date.now() < observeDeadline) {
    if (countSidecars() === 0) {
      gone = true
      break
    }
    await new Promise((r) => setTimeout(r, 500))
  }

  if (gone) pass('后端已自行退出（父进程守护生效，无孤儿进程）')
  else {
    fail(`主进程被强杀后 15s，仍有 ${countSidecars()} 个 sidecar 残留`)
    execSync("pkill -9 -f 'desktop/sidecar/agent-server' || true")
  }

  // 端口也应释放
  const portDeadline = Date.now() + 8000
  let released = false
  while (Date.now() < portDeadline) {
    if (!(await isPortListening(port))) {
      released = true
      break
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  if (released) pass(`端口 ${port} 已释放`)
  else fail(`端口 ${port} 仍被占用`)

  if (child.exitCode === null) child.kill('SIGKILL')
  console.log(process.exitCode ? '\n❌ 孤儿进程守护测试失败\n' : '\n✅ 孤儿进程守护测试通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
