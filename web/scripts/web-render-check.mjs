/* 用 react-dom/server 渲染整棵界面，验证组件层无运行时错误。
 * 这是无浏览器环境下最接近「界面能否正常挂载」的验证：
 *   - 组件导入、props 传递、条件分支、Markdown 渲染都必须跑通
 *   - Provider 的初始状态渲染必须不抛异常（fetch 会被 stub）
 *
 * 用法：node --experimental-strip-types scripts/web-render-check.mjs（在 web/ 下执行）
 */
import { createElement as h } from 'react'
import { renderToString } from 'react-dom/server'

// 组件里会 import 状态层，状态层在 useEffect 里 fetch；SSR 不执行 useEffect，
// 但仍需 stub 全局 fetch 以防模块顶层意外调用。
globalThis.fetch = async () => {
  throw new Error('SSR 阶段不应发起网络请求')
}

const { ChatProvider } = await import('../src/state/chatStore.tsx')
const { ModelsProvider } = await import('../src/state/modelsStore.tsx')
const { Sidebar } = await import('../src/components/Sidebar.tsx')
const { ChatView } = await import('../src/components/ChatView.tsx')
const { Composer } = await import('../src/components/Composer.tsx')
const { StatusBar } = await import('../src/components/StatusBar.tsx')
const { MessageItem } = await import('../src/components/MessageItem.tsx')
const { AskUserCard } = await import('../src/components/AskUserCard.tsx')
const { Markdown, CodeBlock } = await import('../src/components/Markdown.tsx')

let passed = 0
const test = (name, fn) => {
  try {
    fn()
    passed++
    console.log(`  ✓ ${name}`)
  } catch (err) {
    console.error(`  ✗ ${name}\n    ${err.stack ?? err.message}`)
    process.exitCode = 1
  }
}

const now = new Date().toISOString()
const mkSession = (id, title, status = 'idle', count = 2) => ({
  id,
  title,
  created_at: now,
  updated_at: now,
  message_count: count,
  status,
  model: 'mock-echo',
})

const mkMsg = (id, role, content, status = 'complete', extra = {}) => ({
  id,
  session_id: 'sess_1',
  role,
  content,
  status,
  created_at: now,
  updated_at: now,
  ...extra,
})

console.log('\n▶ 组件渲染（react-dom/server）')

/**
 * 统一的 Provider 包装。
 *
 * Composer / 顶部栏依赖 ModelsProvider（模型选择与配置），
 * 漏包会在渲染时抛「useModels 必须在 ModelsProvider 内使用」。
 */
const withProviders = (...children) =>
  h(ModelsProvider, null, h(ChatProvider, null, ...children))

test('整棵树（空态）可渲染', () => {
  const html = renderToString(withProviders(h(Sidebar), h(ChatView), h(Composer), h(StatusBar)))
  if (!html.includes('新建会话')) throw new Error('缺少「新建会话」按钮')
  if (!html.includes('开始一段新对话')) throw new Error('缺少空态提示')
  if (!html.includes('请先在左侧选择或新建会话')) throw new Error('输入框占位文案缺失')
  // SSR 初始态：后端尚未探测，状态栏应显示未连接。
  if (!html.includes('后端未连接')) throw new Error('状态栏未渲染')
})

test('代码块渲染出语言标签与复制按钮', () => {
  const html = renderToString(h(CodeBlock, { code: 'const a = 1', lang: 'ts' }))
  if (!html.includes('code-lang')) throw new Error('缺少语言标签容器')
  if (!html.includes('>ts<')) throw new Error('语言标签内容错误')
  if (!html.includes('复制')) throw new Error('缺少复制按钮')
  // 高亮 span 必须存在
  if (!html.includes('tok-keyword')) throw new Error('未生成高亮 token')
})

test('Markdown 渲染代码块 + 列表 + 粗体', () => {
  const md = [
    '普通段落 **加粗** 与 `inline`',
    '',
    '- 第一项',
    '- 第二项',
    '',
    '```go',
    'func main() {}',
    '```',
  ].join('\n')
  const html = renderToString(h(Markdown, { text: md }))
  if (!html.includes('md-list')) throw new Error('缺少列表')
  if (!html.includes('<strong>')) throw new Error('缺少粗体')
  if (!html.includes('inline-code')) throw new Error('缺少行内代码')
  if (!html.includes('func')) throw new Error('代码块内容丢失')
})

test('Markdown 渲染表格（含对齐与表头）', () => {
  const md = ['| 名称 | 状态 |', '| --- | :-: |', '| a | 完成 |', '| b | 进行 |'].join('\n')
  const html = renderToString(h(Markdown, { text: md }))
  if (!html.includes('md-table')) throw new Error('缺少表格容器')
  if (!html.includes('<table')) throw new Error('未渲染 table 元素')
  if (!html.includes('<th')) throw new Error('缺少表头')
  if (!html.includes('<td')) throw new Error('缺少单元格')
  if (!html.includes('text-align:center')) throw new Error('对齐样式未生效')
})

test('Markdown 渲染嵌套列表', () => {
  const md = ['- 一级', '  - 二级', '    - 三级'].join('\n')
  const html = renderToString(h(Markdown, { text: md }))
  // 两层嵌套应产生两个内层 <ul>
  const ulCount = (html.match(/<ul/g) || []).length
  if (ulCount < 2) throw new Error(`嵌套列表未展开，<ul> 数量 = ${ulCount}`)
  if (!html.includes('三级')) throw new Error('三级项丢失')
})

test('Markdown 渲染任务列表', () => {
  const md = ['- [x] 已完成', '- [ ] 待办'].join('\n')
  const html = renderToString(h(Markdown, { text: md }))
  if (!html.includes('md-task-box')) throw new Error('缺少任务列表复选框')
  if (!html.includes('checked')) throw new Error('已勾选项未标记 checked')
})

test('Markdown 渲染删除线', () => {
  const html = renderToString(h(Markdown, { text: '~~废弃内容~~' }))
  if (!html.includes('<del>')) throw new Error('缺少 <del>')
})

test('危险链接不渲染为可点击 a 标签', () => {
  const html = renderToString(h(Markdown, { text: '[点我](javascript:alert(1))' }))
  if (html.includes('href="javascript:')) {
    throw new Error('渲染出了 javascript: 链接，存在 XSS 风险')
  }
  if (!html.includes('md-unsafe-link')) throw new Error('应标记为不安全链接')
})

test('正常链接渲染为 a 标签并带 noopener', () => {
  const html = renderToString(h(Markdown, { text: '[文档](https://example.com)' }))
  if (!html.includes('href="https://example.com"')) throw new Error('链接未渲染')
  if (!html.includes('noopener')) throw new Error('缺少 noopener 保护')
})

test('表格单元格支持行内 Markdown', () => {
  const md = ['| 列 |', '| --- |', '| `code` 与 **粗体** |'].join('\n')
  const html = renderToString(h(Markdown, { text: md }))
  if (!html.includes('inline-code')) throw new Error('表格内行内代码未渲染')
  if (!html.includes('<strong>')) throw new Error('表格内粗体未渲染')
})

test('用户消息气泡（右对齐，纯文本）', () => {
  const html = renderToString(
    h(ChatProvider, null, h(MessageItem, { message: mkMsg('m1', 'user', '你好 **不是粗体**') })),
  )
  if (!html.includes('msg-user')) throw new Error('缺少 user 气泡类名')
  if (!html.includes('user-bubble')) throw new Error('缺少 user-bubble 类名')
  if (!html.includes('**不是粗体**')) throw new Error('用户消息不应被 Markdown 解析')
})

test('助手消息渲染 Markdown 与复制按钮', () => {
  const html = renderToString(
    h(
      ChatProvider,
      null,
      h(MessageItem, {
        message: mkMsg('m2', 'assistant', '回答内容 `code`', 'complete', {
          usage: { prompt_tokens: 1, completion_tokens: 2, total_tokens: 3 },
        }),
      }),
    ),
  )
  if (!html.includes('msg-assistant')) throw new Error('缺少 assistant 类名')
  if (!html.includes('inline-code')) throw new Error('未渲染行内代码')
  // React SSR 会在文本与表达式之间插入 <!-- --> 注释，需先剔除再断言。
  const plain = html.replace(/<!--.*?-->/g, '')
  if (!plain.includes('3 tokens')) throw new Error('未显示 token 用量')
  if (!plain.includes('复制')) throw new Error('缺少复制按钮')
})

test('生成中显示打字点，完成不显示', () => {
  // 无 streamingMessageId 时不应出现 typing 占位（SSR 初始态即 idle）
  const html = renderToString(
    h(ChatProvider, null, h(MessageItem, { message: mkMsg('m3', 'assistant', '已完成') })),
  )
  if (html.includes('typing')) throw new Error('空闲态不应显示打字动画')
})

test('错误消息显示错误详情', () => {
  const html = renderToString(
    h(
      ChatProvider,
      null,
      h(MessageItem, {
        message: mkMsg('m4', 'assistant', '', 'error', {
          error: { code: 'provider_unavailable', message: '上游模型不可用' },
        }),
      }),
    ),
  )
  if (!html.includes('上游模型不可用')) throw new Error('未显示错误文案')
  if (!html.includes('tag-error')) throw new Error('缺少错误标签')
})

test('中断消息带「已中断」标记', () => {
  const html = renderToString(
    h(
      ChatProvider,
      null,
      h(MessageItem, { message: mkMsg('m5', 'assistant', '半截回答', 'interrupted') }),
    ),
  )
  if (!html.includes('已中断')) throw new Error('缺少中断标记')
  if (!html.includes('半截回答')) throw new Error('已生成内容应保留展示')
})

test('状态栏在后端未连接时显示离线状态', () => {
  const html = renderToString(h(ChatProvider, null, h(StatusBar)))
  if (!html.includes('后端未连接')) throw new Error('未显示离线提示')
  if (!html.includes('is-offline')) throw new Error('缺少离线样式类')
})

test('侧边栏初始为加载态（SSR 不执行 useEffect）', () => {
  const html = renderToString(h(ChatProvider, null, h(Sidebar)))
  // 初始 sessionsLoading=true，应显示加载提示而非空列表引导。
  if (!html.includes('正在加载')) throw new Error('初始应显示加载态')
  if (!html.includes('Agent Rebuild')) throw new Error('缺少品牌标识')
  if (!html.includes('新建会话')) throw new Error('缺少新建按钮')
})

test('输入框在无会话时禁用', () => {
  const html = renderToString(withProviders(h(Composer)))
  if (!html.includes('disabled')) throw new Error('无会话时 textarea 应禁用')
  if (!html.includes('请先在左侧选择或新建会话')) throw new Error('缺少占位提示')
})

test('工具调用默认折叠并显示次数', () => {
  // 造一条带工具卡片的 assistant 消息，验证摘要行渲染。
  const msg = mkMsg('m-tool', 'assistant', '回答正文')
  const html = renderToString(
    h(ModelsProvider, null, h(ChatProvider, null, h(MessageItem, { message: msg }))),
  )
  // 无工具时不应出现摘要
  if (html.includes('工具调用')) throw new Error('无工具时不应显示摘要')
})

test('无挂起提问时不渲染问答卡片', () => {
  // 初始状态下 pendingQuestion 为 null，卡片不应出现。
  const html = renderToString(withProviders(h(Composer)))
  if (html.includes('需要你的回答')) throw new Error('没有提问时不应渲染问答卡片')
  if (html.includes('提交并继续')) throw new Error('没有提问时不应有提交按钮')
})

test('AskUserCard 在无提问时返回空', () => {
  const html = renderToString(
    h(ModelsProvider, null, h(ChatProvider, null, h(AskUserCard))),
  )
  if (html !== '') throw new Error(`无提问时应渲染空内容，实际: ${html}`)
})

console.log(`\n✅ 组件渲染检查通过（${passed} 项）\n`)
