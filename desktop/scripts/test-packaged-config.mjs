#!/usr/bin/env node
/**
 * 验证「打包后的 .app」能读到桌面版配置（userData/config.env）。
 *
 * 这是用户实际使用路径：Finder 双击 .app 时工作目录是 /，
 * 后端自己找不到 .env，必须由主进程注入。
 *
 * 用法：node scripts/test-packaged-config.mjs
 * 前置：npm run pack
 */
import { spawn, execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { writeFile, mkdtemp, rm, stat } from 'node:fs/promises'
import { tmpdir } from 'node:os'

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

async function main() {
  console.log('\n▶ 验证打包产物的配置加载（userData/config.env）')

  const appPath = path.join(releaseDir, 'mac-arm64', 'Agent Rebuild.app')
  const bin = path.join(appPath, 'Contents', 'MacOS', 'Agent Rebuild')
  if (!(await exists(bin))) {
    fail('未找到打包产物，请先 npm run pack')
    return
  }

  const userData = await mkdtemp(path.join(tmpdir(), 'agent-pack-cfg-'))
  await writeFile(
    path.join(userData, 'config.env'),
    [
      'LLM_PROVIDER=openai',
      'OPENAI_API_KEY=sk-packaged-test',
      'LLM_MODEL=packaged-model',
      'LLM_BASE_URL=https://packaged.invalid/v1',
    ].join('\n'),
  )

  const child = spawn(bin, ['--no-sandbox', `--user-data-dir=${userData}`], {
    // 关键：故意用根目录作为 cwd，模拟 Finder 双击启动
    cwd: '/',
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
    if (/启动失败/.test(out)) break
    await new Promise((r) => setTimeout(r, 250))
  }

  if (!url) {
    fail(`应用未就绪（cwd=/ 场景）：\n${out.slice(-1500)}`)
    child.kill('SIGKILL')
    await rm(userData, { recursive: true, force: true })
    return
  }
  pass('应用在 cwd=/ 下正常启动（模拟 Finder 双击）')

  if (/已读取配置文件:.*config\.env/.test(out)) pass('主进程读到了 userData/config.env')
  else fail('主进程未读取配置文件')

  try {
    const health = await fetch(`${url}/api/health`).then((r) => r.json())
    if (health.provider === 'openai') pass('后端 provider = openai')
    else fail(`后端 provider = ${health.provider}，期望 openai（配置未注入）`)
    if (health.model === 'packaged-model') pass('后端 model 与配置文件一致')
    else fail(`后端 model = ${health.model}，期望 packaged-model`)
  } catch (err) {
    fail(`/api/health 失败：${err.message}`)
  }

  if (/sk-packaged-test/.test(out)) fail('日志泄漏 API Key 明文')
  else pass('日志未泄漏 API Key')

  try {
    execSync("pkill -f 'Agent Rebuild.app/Contents/MacOS/Agent Rebuild' || true")
  } catch {
    /* ignore */
  }
  if (child.exitCode === null) child.kill('SIGTERM')
  await new Promise((r) => setTimeout(r, 1200))
  if (child.exitCode === null) child.kill('SIGKILL')
  await rm(userData, { recursive: true, force: true })

  console.log(process.exitCode ? '\n❌ 打包产物配置验证失败\n' : '\n✅ 打包产物配置验证通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
