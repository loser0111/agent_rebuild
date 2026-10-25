#!/usr/bin/env node
/**
 * 桌面端 UI 修复的回归测试。
 *
 * 覆盖三个真实反馈的问题：
 *   1. 程序名与 macOS 红绿灯按钮重叠 → 侧栏顶部需留安全区
 *   2. 双击无法放大窗口 → 需要有 window:toggle-maximize 通道
 *   3. 页面上除对话外的文字都可被选中 → 需有 user-select 策略
 *
 * 这些都是 CSS/接线问题，无法用单元测试覆盖，因此直接对源码做结构断言。
 *
 * 用法：node scripts/test-ui-fixes.mjs
 */
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const root = path.resolve(here, '..', '..')

let passed = 0
let failed = 0
const test = async (name, fn) => {
  try {
    await fn()
    passed++
    console.log(`  ✓ ${name}`)
  } catch (err) {
    failed++
    console.error(`  ✗ ${name}\n    ${err.message}`)
  }
}

const read = (rel) => readFile(path.join(root, rel), 'utf8')

const assertIncludes = (haystack, needle, msg) => {
  if (!haystack.includes(needle)) {
    throw new Error(msg || `缺少片段: ${needle}`)
  }
}

const assertNotIncludes = (haystack, needle, msg) => {
  if (haystack.includes(needle)) {
    throw new Error(msg || `不应包含: ${needle}`)
  }
}

console.log('\n▶ 问题 1：程序名与窗口按钮重叠')

await test('桌面端侧栏顶部有安全区（为红绿灯留空间）', async () => {
  const css = await read('web/src/styles/global.css')
  assertIncludes(css, '.app.is-desktop .sidebar-head', '缺少桌面端侧栏安全区规则')
  // 安全区应显著大于默认的 14px
  const m = /\.app\.is-desktop \.sidebar-head\s*\{[^}]*padding-top:\s*(\d+)px/.exec(css)
  if (!m) throw new Error('未找到 padding-top 声明')
  const px = Number.parseInt(m[1], 10)
  if (px < 30) throw new Error(`padding-top = ${px}px，不足以避开红绿灯按钮（需要 >= 30px）`)
})

await test('红绿灯位置与安全区匹配', async () => {
  const main = await read('desktop/main.js')
  assertIncludes(main, 'trafficLightPosition', '缺少红绿灯位置配置')
  // 位置 y=14 + 按钮高度约 12 → 底部约 26px，安全区应 >= 30px
  const m = /trafficLightPosition:\s*\{\s*x:\s*(\d+),\s*y:\s*(\d+)/.exec(main)
  if (!m) throw new Error('无法解析红绿灯位置')
  const y = Number.parseInt(m[2], 10)
  if (y + 12 > 30) {
    throw new Error(`红绿灯底部 ${y + 12}px 超出安全区，仍会重叠`)
  }
})

await test('品牌区可拖动窗口（-webkit-app-region: drag）', async () => {
  const css = await read('web/src/styles/global.css')
  // 注意：必须匹配 `.brand {` 而不是 `.app.is-desktop .brand {`，
  // 后者只改 padding，不含拖拽声明（正则按首次出现匹配，容易误判）。
  const brandBlock = /(?:^|\n)\.brand\s*\{[^}]*\}/.exec(css)
  if (!brandBlock) throw new Error('未找到 .brand 样式')
  assertIncludes(brandBlock[0], '-webkit-app-region: drag', '.brand 未设置为拖拽区')
})

console.log('\n▶ 问题 2：双击放大窗口')

await test('主进程注册了 window:toggle-maximize 处理器', async () => {
  const main = await read('desktop/main.js')
  assertIncludes(main, "ipcMain.handle('window:toggle-maximize'", '缺少窗口最大化 IPC handler')
  assertIncludes(main, 'isMaximized()', '缺少最大化状态判断')
  assertIncludes(main, 'unmaximize()', '缺少还原逻辑')
})

await test('preload 暴露 windowControl 接口', async () => {
  const preload = await read('desktop/preload.js')
  assertIncludes(preload, 'windowControl', '未暴露 windowControl')
  assertIncludes(preload, 'toggleMaximize', '缺少 toggleMaximize 方法')
  // 必须通过 invoke 走 IPC，不能直接操作窗口
  assertIncludes(preload, "ipcRenderer.invoke('window:toggle-maximize'", '未使用 IPC 调用')
})

await test('渲染层绑定双击事件并排除交互区', async () => {
  const app = await read('web/src/App.tsx')
  assertIncludes(app, 'onDoubleClick', '顶栏缺少双击处理')
  assertIncludes(app, 'data-no-window-drag', '缺少交互区排除标记')
  // 必须排除按钮/输入框/正文，否则双击正文会缩放窗口
  assertIncludes(app, 'closest(', '缺少目标区域判断')
})

await test('消息区与输入区标记为不可拖拽', async () => {
  const chat = await read('web/src/components/ChatView.tsx')
  assertIncludes(chat, 'data-no-window-drag', '消息区未标记 data-no-window-drag')
  const composer = await read('web/src/components/Composer.tsx')
  assertIncludes(composer, 'data-no-window-drag', '输入区未标记 data-no-window-drag')
})

await test('会话项不可拖拽（否则无法点击切换）', async () => {
  const css = await read('web/src/styles/global.css')
  const itemBlock = /\.session-item\s*\{[^}]*\}/.exec(css)
  if (!itemBlock) throw new Error('未找到 .session-item 样式')
  assertIncludes(itemBlock[0], '-webkit-app-region: no-drag', '.session-item 应为 no-drag')
})

console.log('\n▶ 问题 3：文本选择策略')

await test('body 默认禁用文本选择', async () => {
  const css = await read('web/src/styles/global.css')
  const bodyBlock = /body\s*\{[^}]*\}/.exec(css)
  if (!bodyBlock) throw new Error('未找到 body 样式')
  assertIncludes(bodyBlock[0], 'user-select: none', 'body 未禁用选择')
})

await test('对话内容可选中（.md / .user-text）', async () => {
  const css = await read('web/src/styles/global.css')
  // 需有专门的「放开选择」规则块
  const allowBlock = /\.md,\s*\n?\.user-text[^{]*\{[^}]*\}/.exec(css)
  if (!allowBlock) throw new Error('未找到放开选择的规则块')
  assertIncludes(allowBlock[0], 'user-select: text', '对话内容未允许选择')
})

await test('输入框可选中', async () => {
  const css = await read('web/src/styles/global.css')
  const allowBlock = /\.md,\s*\n?\.user-text[^{]*\{[^}]*\}/.exec(css)
  if (!allowBlock) throw new Error('未找到放开选择的规则块')
  assertIncludes(allowBlock[0], 'textarea', '输入框未包含在可选区')
})

await test('界面元素明确禁用选择', async () => {
  const css = await read('web/src/styles/global.css')
  // 按钮、状态栏等不应可选中
  const block = /button,\s*\n?\.badge[^{]*\{[^}]*\}/.exec(css)
  if (!block) throw new Error('未找到界面元素禁用选择规则')
  assertIncludes(block[0], 'user-select: none', '界面元素未禁用选择')
})

console.log('\n▶ 回归防护')

await test('非桌面端不使用 app-region（避免浏览器里无法选文字）', async () => {
  const css = await read('web/src/styles/global.css')
  assertIncludes(css, '.app:not(.is-desktop) .topbar', '缺少非桌面端的 app-region 覆盖')
})

await test('App 仅在桌面环境下绑定窗口控制', async () => {
  const app = await read('web/src/App.tsx')
  assertIncludes(app, 'window.desktopEnv?.isDesktop', '未判断桌面环境')
  // 浏览器里 window.windowControl 为 undefined，必须做空值保护
  assertIncludes(app, 'window.windowControl', '未判断 windowControl 是否存在')
})

await test('IPC handler 不接受外部参数（避免滥用）', async () => {
  const main = await read('desktop/main.js')
  // handler 应为无参函数
  const m = /ipcMain\.handle\('window:toggle-maximize',\s*\(\)\s*=>/.exec(main)
  if (!m) throw new Error('toggle-maximize handler 不应接受参数')
})

console.log(
  failed
    ? `\n❌ UI 修复验证失败（${failed} 项）\n`
    : `\n✅ UI 修复验证通过（${passed} 项）\n`,
)
process.exit(failed ? 1 : 0)
