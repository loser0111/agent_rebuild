#!/usr/bin/env node
/**
 * 端到端验证：Electron 壳能把配置文件正确注入给 Go sidecar。
 *
 * 这是「配了 Key 却不生效」这一整类问题的守门测试。
 * 做法：往应用数据目录写一个 config.env，启动 Electron，
 * 然后断言：
 *   1. 主进程日志里打印的 provider/model 与配置一致；
 *   2. /api/health 返回的 provider/model 与配置一致（说明真的传到了后端）。
 *
 * 注意：不发起真实模型请求（避免消耗额度），只验证配置传递链路。
 *
 * 用法：node scripts/test-config-e2e.mjs
 */
import { spawn, execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { writeFile, mkdtemp, rm, mkdir } from 'node:fs/promises'
import { tmpdir } from 'node:os'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')

const pass = (m) => console.log(`  ✅ ${m}`)
const fail = (m) => {
  console.error(`  ❌ ${m}`)
  process.exitCode = 1
}

async function main() {
  console.log('\n▶ 验证配置文件注入链路')

  // 用临时的 userData 目录隔离测试，避免污染真实配置。
  const fakeUserData = await mkdtemp(path.join(tmpdir(), 'agent-userdata-'))
  const configPath = path.join(fakeUserData, 'config.env')
  await writeFile(
    configPath,
    [
      '# 测试用配置',
      'LLM_PROVIDER=openai',
      'OPENAI_API_KEY=sk-test-not-real',
      'LLM_MODEL=test-model-from-config',
      'LLM_BASE_URL=https://example.invalid/v1',
    ].join('\n'),
  )
  console.log(`  已写入测试配置：${configPath}`)

  // 通过 --user-data-dir 让 Electron 使用临时目录（config.env 就在其中）。
  const child = spawn(
    process.platform === 'win32' ? 'npx.cmd' : 'npx',
    ['electron', '.', '--no-sandbox', `--user-data-dir=${fakeUserData}`],
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
    if (/启动失败/.test(out)) break
    await new Promise((r) => setTimeout(r, 250))
  }

  if (!url) {
    fail(`应用未就绪：\n${out.slice(-1500)}`)
    child.kill('SIGKILL')
    await rm(fakeUserData, { recursive: true, force: true })
    return
  }

  // 1. 主进程日志应显示读取到配置文件
  if (/已读取配置文件:.*config\.env/.test(out)) pass('主进程已读取 config.env')
  else fail(`主进程未记录读取配置文件。日志片段：\n${out.slice(-800)}`)

  // 2. 主进程日志应打印正确的模型配置（不含密钥明文）
  if (/provider=openai/.test(out) && /model=test-model-from-config/.test(out)) {
    pass('主进程日志中的 provider/model 与配置一致')
  } else {
    fail('主进程日志中的模型配置不正确')
  }
  if (/apiKey=已设置/.test(out)) pass('日志正确识别「API Key 已设置」')
  else fail('日志未提示 API Key 已设置')
  if (/sk-test-not-real/.test(out)) fail('日志泄漏了 API Key 明文！')
  else pass('日志未泄漏 API Key 明文')

  // 3. 后端 /api/health 应反映配置（证明真的注入到了 sidecar）
  try {
    const health = await fetch(`${url}/api/health`).then((r) => r.json())
    if (health.provider === 'openai') pass(`后端 provider = openai`)
    else fail(`后端 provider = ${health.provider}，期望 openai（配置未传到 sidecar）`)
    if (health.model === 'test-model-from-config') pass('后端 model 与配置文件一致')
    else fail(`后端 model = ${health.model}，期望 test-model-from-config`)
  } catch (err) {
    fail(`/api/health 请求失败：${err.message}`)
  }

  // 收尾
  try {
    execSync("pkill -f 'Electron.app/Contents/MacOS/Electron' || true")
  } catch {
    /* ignore */
  }
  if (child.exitCode === null) child.kill('SIGTERM')
  await new Promise((r) => setTimeout(r, 1500))
  if (child.exitCode === null) child.kill('SIGKILL')
  await rm(fakeUserData, { recursive: true, force: true })

  console.log(process.exitCode ? '\n❌ 配置注入链路验证失败\n' : '\n✅ 配置注入链路验证通过\n')
}

main().catch((err) => {
  console.error('测试脚本异常:', err)
  process.exit(1)
})
