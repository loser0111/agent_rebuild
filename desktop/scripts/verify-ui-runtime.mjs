#!/usr/bin/env node
/**
 * 桌面端 UI 修复的运行时验证（在真实应用内执行）。
 *
 * 与 test-ui-fixes.mjs 的区别：
 *   - 那个是静态源码断言（快，但可能被正则误判）
 *   - 这个是启动真实 Electron 并从渲染进程读 computedStyle / 实际调用 IPC
 *
 * 验证三项反馈：
 *   1. 侧栏顶部安全区足以避开 macOS 红绿灯
 *   2. 双击顶栏可放大窗口（through IPC）
 *   3. 只有对话内容与输入框可选中文本
 *
 * 用法：node scripts/verify-ui-runtime.mjs
 */
import { spawn, execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')

let passed = 0
let failed = 0
const pass = (m) => {
  passed++
  console.log(`  ✓ ${m}`)
}
const fail = (m) => {
  failed++
  console.error(`  ✗ ${m}`)
}

/**
 * 启动应用并在渲染进程内执行断言脚本。
 *
 * 做法：通过环境变量 VERIFY_UI 触达 main.js 里的一段「仅在验证时启用」的钩子，
 * 该钩子用 executeJavaScript 读取真实样式并调用 IPC。
 */
async function main() {
  console.log('\n▶ UI 修复运行时验证')

  const child = spawn(
    process.platform === 'win32' ? 'npx.cmd' : 'npx',
    ['electron', '.', '--no-sandbox'],
    {
      cwd: desktopDir,
      env: { ...process.env, VERIFY_UI: '1', ELECTRON_DISABLE_SECURITY_WARNINGS: '1' },
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  )

  let out = ''
  child.stdout.on('data', (c) => (out += c))
  child.stderr.on('data', (c) => (out += c))

  // 等待验证结果输出
  const deadline = Date.now() + 45_000
  let resultLine = null
  while (Date.now() < deadline) {
    const m = /VERIFY_UI_RESULT (\{.*\})/.exec(out)
    if (m) {
      resultLine = m[1]
      break
    }
    if (/启动失败/.test(out)) break
    await new Promise((r) => setTimeout(r, 300))
  }

  try {
    execSync("pkill -f 'Electron.app/Contents/MacOS/Electron' || true")
  } catch {
    /* ignore */
  }
  if (child.exitCode === null) child.kill('SIGTERM')
  await new Promise((r) => setTimeout(r, 800))
  if (child.exitCode === null) child.kill('SIGKILL')

  if (!resultLine) {
    fail(`未拿到验证结果。输出片段：\n${out.slice(-1500)}`)
    console.log(`\n❌ 运行时验证失败（${failed} 项）\n`)
    process.exit(1)
  }

  let r
  try {
    r = JSON.parse(resultLine)
  } catch (e) {
    fail(`结果不是合法 JSON: ${e.message}`)
    process.exit(1)
  }

  // ---- 问题 1：安全区 ----
  const padTop = Number.parseInt(String(r.headPadTop || '0'), 10)
  if (padTop >= 30) pass(`侧栏顶部安全区 ${padTop}px（红绿灯占用约 26px）`)
  else fail(`侧栏安全区仅 ${padTop}px，不足以避开红绿灯`)

  if (r.isDesktopClass) pass('桌面环境标记 is-desktop 已生效')
  else fail('is-desktop 未生效，桌面端样式不会应用')

  // ---- 问题 2：窗口控制 ----
  if (r.hasWindowControl === 'object') pass('windowControl 已注入')
  else fail(`windowControl 未注入（typeof = ${r.hasWindowControl}）`)

  if (r.toggleWorked === true) pass('双击放大通道可用（IPC 调用成功并已最大化）')
  else fail('toggleMaximize 调用失败')

  // ---- 问题 3：文本选择 ----
  if (r.bodySelect === 'none') pass('body 默认不可选中')
  else fail(`body user-select = ${r.bodySelect}，期望 none`)

  if (r.mdSelect === 'text') pass('对话内容（.md）可选中')
  else fail(`.md user-select = ${r.mdSelect}，期望 text`)

  if (r.textareaSelect === 'text') pass('输入框可选中')
  else fail(`textarea user-select = ${r.textareaSelect}，期望 text`)

  if (r.statusbarSelect === 'none') pass('状态栏不可选中')
  else fail(`状态栏 user-select = ${r.statusbarSelect}，期望 none`)

  // ---- 额外：拖拽区 ----
  if (r.brandDrag === 'drag') pass('品牌区可拖动窗口')
  else fail(`品牌区 app-region = ${r.brandDrag}，期望 drag`)

  // ---- 额外：交互区不能拖窗口（否则点不到） ----
  if (r.topbarButtonDrag === 'no-drag') pass('顶栏按钮不可拖动（避免点击失效）')
  else fail(`顶栏按钮 app-region = ${r.topbarButtonDrag}，期望 no-drag`)

  console.log(
    failed
      ? `\n❌ 运行时验证失败（${failed} 项）\n`
      : `\n✅ 运行时验证通过（${passed} 项）\n`,
  )
  process.exit(failed ? 1 : 0)
}

main().catch((err) => {
  console.error('验证脚本异常:', err)
  process.exit(1)
})
