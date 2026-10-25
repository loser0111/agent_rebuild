#!/usr/bin/env node
/**
 * 验证桌面端 SQLite 持久化：关掉应用再打开，会话应当还在。
 *
 * 这直接回答「桌面版重启后历史还在吗」——用户最直观的体验点。
 *
 * 用法：node scripts/test-desktop-persistence.mjs
 */
import { spawn, execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { mkdtemp, rm, stat } from 'node:fs/promises'
import { tmpdir } from 'node:os'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')

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

/** 启动 Electron 并等待 sidecar 就绪，返回 {child, url}。 */
async function launch(userDataDir) {
  const child = spawn(
    process.platform === 'win32' ? 'npx.cmd' : 'npx',
    ['electron', '.', '--no-sandbox', `--user-data-dir=${userDataDir}`],
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
  while (Date.now() < deadline) {
    const m = /后端就绪:\s*(http:\/\/localhost:\d+)/.exec(out)
    if (m) return { child, url: m[1], log: () => out }
    if (/启动失败/.test(out)) break
    await new Promise((r) => setTimeout(r, 250))
  }
  child.kill('SIGKILL')
  throw new Error(`应用未就绪:\n${out.slice(-1200)}`)
}

/** 优雅关闭（SIGTERM 让 Electron 走 before-quit → 关数据库）。 */
async function shutdown(child) {
  try {
    execSync("pkill -TERM -f 'Electron.app/Contents/MacOS/Electron' || true")
  } catch {
    /* ignore */
  }
  const deadline = Date.now() + 12_000
  while (Date.now() < deadline && child.exitCode === null) {
    await new Promise((r) => setTimeout(r, 200))
  }
  if (child.exitCode === null) child.kill('SIGKILL')
  // 等 sidecar 也退出（它要完成 db.Close）。
  await new Promise((r) => setTimeout(r, 2000))
}

async function main() {
  console.log('\n▶ 桌面端持久化验证（关闭应用再打开）')

  const userData = await mkdtemp(path.join(tmpdir(), 'agent-persist-'))
  const dbPath = path.join(userData, 'data', 'agent.db')

  // ---- 第一次启动：写入数据 ----
  const first = await launch(userData)
  pass(`第一次启动: ${first.url}`)

  const created = await fetch(`${first.url}/api/sessions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ title: '桌面持久化测试' }),
  }).then((r) => r.json())
  const sessionId = created.session.id

  await fetch(`${first.url}/api/sessions/${sessionId}/messages`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ content: '关掉应用后这条消息应该还在' }),
  })
  pass('已写入会话与消息')

  const modelResp = await fetch(`${first.url}/api/models`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      name: '桌面测试模型',
      protocol: 'openai',
      provider: 'deepseek',
      model: 'deepseek-chat',
      api_key: 'sk-desktop-secret-xyz',
    }),
  })
  if (modelResp.status !== 201) fail(`创建模型配置失败: ${modelResp.status}`)
  else pass('已写入模型配置（含加密密钥）')

  // 记录数据库路径应位于 userData 下。
  if (!(await exists(dbPath))) {
    // 也可能落在 userData 其它位置，扫描一遍。
    const found = execSync(`find "${userData}" -name 'agent.db' 2>/dev/null || true`, {
      encoding: 'utf8',
    }).trim()
    if (found) pass(`数据库位于: ${found.replace(userData, '<userData>')}`)
    else fail('未找到 agent.db，持久化可能未启用')
  } else {
    pass(`数据库位于: data/agent.db（在 userData 下）`)
  }

  await shutdown(first.child)
  pass('应用已关闭（触发 db.Close）')

  // ---- 第二次启动：验证数据还在 ----
  const second = await launch(userData)
  pass(`第二次启动: ${second.url}`)

  try {
    const detail = await fetch(`${second.url}/api/sessions/${sessionId}`).then((r) => r.json())
    if (detail.session?.title === '桌面持久化测试') pass('会话在重启后仍在')
    else fail(`会话丢失: ${JSON.stringify(detail).slice(0, 200)}`)

    if (detail.messages?.length === 2) pass(`消息在重启后仍在（${detail.messages.length} 条）`)
    else fail(`消息数 = ${detail.messages?.length}，期望 2`)
  } catch (err) {
    fail(`读取会话失败: ${err.message}`)
  }

  try {
    const list = await fetch(`${second.url}/api/models`).then((r) => r.json())
    const found = list.configs?.find((c) => c.name === '桌面测试模型')
    if (found) pass('模型配置在重启后仍在')
    else fail('模型配置丢失')
    if (found && found.has_api_key) pass('API Key 标记存在（已加密落盘）')
    else fail('API Key 丢失')
    if (JSON.stringify(list).includes('sk-desktop-secret-xyz')) fail('响应中泄漏了明文密钥！')
    else pass('响应中无明文密钥')
  } catch (err) {
    fail(`读取模型配置失败: ${err.message}`)
  }

  await shutdown(second.child)

  // 清理
  try {
    execSync("pkill -9 -f 'desktop/sidecar/agent-server' || true")
  } catch {
    /* ignore */
  }
  await rm(userData, { recursive: true, force: true })

  console.log(process.exitCode ? '\n❌ 桌面端持久化验证失败\n' : '\n✅ 桌面端持久化验证通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
