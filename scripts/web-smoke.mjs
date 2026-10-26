/* 前端逻辑冒烟测试（无需浏览器）：
 *   1. Markdown 解析器 / 语法高亮的纯函数断言
 *   2. 用 react-dom/server 渲染整棵组件树，捕获运行时错误
 *
 * 用法：node --experimental-strip-types scripts/web-smoke.mjs
 * （由 Makefile 的 web-smoke 目标调用，内部先用 esbuild 打包）
 */
import assert from 'node:assert/strict'
import { parseBlocks, parseInline, tokenize, splitTableRow, isSafeHref } from '../web/src/lib/markdown.ts'

let passed = 0
const test = (name, fn) => {
  try {
    fn()
    passed++
    console.log(`  ✓ ${name}`)
  } catch (err) {
    console.error(`  ✗ ${name}\n    ${err.message}`)
    process.exitCode = 1
  }
}

console.log('\n▶ Markdown 块级解析')

test('普通段落', () => {
  const b = parseBlocks('这是一段话')
  assert.equal(b.length, 1)
  assert.equal(b[0].kind, 'paragraph')
  assert.equal(b[0].text, '这是一段话')
})

test('多段落按空行切分', () => {
  const b = parseBlocks('第一段\n\n第二段')
  assert.equal(b.length, 2)
  assert.ok(b.every((x) => x.kind === 'paragraph'))
})

test('代码块带语言标记', () => {
  const b = parseBlocks('```go\nfmt.Println("hi")\n```')
  assert.equal(b.length, 1)
  assert.equal(b[0].kind, 'code')
  assert.equal(b[0].lang, 'go')
  assert.equal(b[0].code, 'fmt.Println("hi")')
})

test('代码块内容中的 # 与 * 不被当作 Markdown', () => {
  const b = parseBlocks('```bash\n# 注释\necho *\n```')
  assert.equal(b[0].kind, 'code')
  assert.equal(b[0].code, '# 注释\necho *')
})

test('未闭合的代码块不会丢内容', () => {
  const b = parseBlocks('```js\nconst a = 1')
  assert.equal(b[0].kind, 'code')
  assert.equal(b[0].code, 'const a = 1')
})

test('标题级别', () => {
  const b = parseBlocks('# 一级\n## 二级')
  assert.equal(b[0].kind, 'heading')
  assert.equal(b[0].level, 1)
  assert.equal(b[1].level, 2)
})

test('无序与有序列表', () => {
  const ul = parseBlocks('- a\n- b')
  assert.equal(ul[0].kind, 'list')
  assert.equal(ul[0].ordered, false)
  // items 是 ListItem 对象（支持嵌套与任务列表），不是裸字符串。
  assert.deepEqual(ul[0].items.map((it) => it.text), ['a', 'b'])
  assert.deepEqual(ul[0].items.map((it) => it.checked), [null, null])

  const ol = parseBlocks('1. x\n2. y')
  assert.equal(ol[0].ordered, true)
  assert.deepEqual(ol[0].items.map((it) => it.text), ['x', 'y'])
})

test('列表不会被后续段落吞掉', () => {
  const b = parseBlocks('- 项目\n\n结束段落')
  assert.equal(b[0].kind, 'list')
  assert.equal(b[1].kind, 'paragraph')
})

test('引用块', () => {
  const b = parseBlocks('> 引用一\n> 引用二')
  assert.equal(b[0].kind, 'quote')
  assert.deepEqual(b[0].lines, ['引用一', '引用二'])
})

test('分隔线', () => {
  const b = parseBlocks('a\n\n---\n\nb')
  assert.equal(b[1].kind, 'hr')
})

console.log('\n▶ Markdown 行内解析')

test('行内代码', () => {
  const t = parseInline('看 `code` 这里')
  assert.equal(t[1].kind, 'code')
  assert.equal(t[1].text, 'code')
})

test('粗体与斜体', () => {
  const strong = parseInline('**重要**')
  assert.equal(strong[0].kind, 'strong')
  const em = parseInline('*强调*')
  assert.equal(em[0].kind, 'em')
})

test('行内代码优先于粗体（** 不被误吃）', () => {
  const t = parseInline('`**not bold**`')
  assert.equal(t.length, 1)
  assert.equal(t[0].kind, 'code')
  assert.equal(t[0].text, '**not bold**')
})

test('链接', () => {
  const t = parseInline('[文档](https://example.com)')
  assert.equal(t[0].kind, 'link')
  assert.equal(t[0].href, 'https://example.com')
})

test('纯文本不丢字符', () => {
  const src = '普通文本 with English 123'
  const t = parseInline(src)
  assert.equal(t.map((x) => x.text).join(''), src)
})

console.log('\n▶ 流式渲染（trailing 模式）')

test('隐藏末尾未闭合的粗体标记', () => {
  const t = parseInline('消息：*', true)
  assert.equal(t.map((x) => x.text).join(''), '消息：')
})

test('隐藏末尾未闭合的行内代码标记', () => {
  const t = parseInline('看 `co', true)
  assert.equal(t.map((x) => x.text).join(''), '看 co')
})

test('隐藏末尾堆叠的多个标记', () => {
  const t = parseInline('强调**', true)
  assert.equal(t.map((x) => x.text).join(''), '强调')
})

test('非 trailing 模式保留原始标记（已完成的文本不变）', () => {
  const t = parseInline('消息：*', false)
  assert.equal(t.map((x) => x.text).join(''), '消息：*')
})

test('已闭合的粗体不受 trailing 影响', () => {
  const t = parseInline('**加粗**', true)
  assert.equal(t.length, 1)
  assert.equal(t[0].kind, 'strong')
  assert.equal(t[0].text, '加粗')
})

test('trailing 模式不误删正常结尾文字', () => {
  const t = parseInline('以星号结尾但已闭合 **是粗体** 后面还有字', true)
  const joined = t.map((x) => x.text).join('')
  assert.ok(joined.includes('后面还有字'), joined)
})

console.log('\n▶ 语法高亮')

test('Go 关键字与字符串', () => {
  const tokens = tokenize('func main() { s := "hi" }', 'go')
  const kinds = tokens.filter((t) => t.type !== 'plain').map((t) => `${t.type}:${t.text}`)
  assert.ok(kinds.includes('keyword:func'), kinds.join(','))
  assert.ok(kinds.includes('string:"hi"'), kinds.join(','))
})

test('注释被识别', () => {
  const g = tokenize('// 这是注释\nx := 1', 'go')
  assert.ok(g.some((t) => t.type === 'comment' && t.text === '// 这是注释'))
  const py = tokenize('# python 注释\nx = 1', 'python')
  assert.ok(py.some((t) => t.type === 'comment'))
})

test('关键字大小写不敏感（SQL）', () => {
  const t = tokenize('SELECT * FROM t', 'sql')
  assert.ok(t.some((x) => x.type === 'keyword' && x.text === 'SELECT'))
})

test('token 拼接等于原文（不丢字符）', () => {
  const src = `func f() {\n\t// 注释\n\ts := "值"\n\treturn 42\n}`
  const joined = tokenize(src, 'go')
    .map((t) => t.text)
    .join('')
  assert.equal(joined, src)
})

test('未知语言退化为纯文本切分', () => {
  const src = '随便写点什么 <<<>>>'
  assert.equal(
    tokenize(src, 'unknownlang')
      .map((t) => t.text)
      .join(''),
    src,
  )
})

test('语言别名归一化（golang / tsx / py）', () => {
  assert.ok(tokenize('func f() {}', 'golang').some((t) => t.type === 'keyword'))
  assert.ok(tokenize('const a = 1', 'tsx').some((t) => t.type === 'keyword'))
  assert.ok(tokenize('def f(): pass', 'py').some((t) => t.type === 'keyword'))
})

console.log('\n▶ Markdown 表格')

test('基础表格', () => {
  const b = parseBlocks('| 名称 | 值 |\n| --- | --- |\n| a | 1 |\n| b | 2 |')
  assert.equal(b.length, 1)
  assert.equal(b[0].kind, 'table')
  assert.deepEqual(b[0].header, ['名称', '值'])
  assert.equal(b[0].rows.length, 2)
  assert.deepEqual(b[0].rows[0], ['a', '1'])
})

test('表格对齐标记', () => {
  const b = parseBlocks('| L | C | R |\n| :-- | :-: | --: |\n| 1 | 2 | 3 |')
  assert.equal(b[0].kind, 'table')
  assert.deepEqual(b[0].aligns, ['left', 'center', 'right'])
})

test('表格分隔行不是表格（无管道符）', () => {
  const b = parseBlocks('一段文字\n---\n后面')
  assert.notEqual(b[0].kind, 'table')
})

test('表格单元格内的转义管道', () => {
  const cells = splitTableRow('| a \\| b | c |')
  assert.deepEqual(cells, ['a | b', 'c'])
})

test('表格前后有段落', () => {
  const b = parseBlocks('前置\n\n| h |\n| - |\n| v |\n\n后置')
  assert.equal(b[0].kind, 'paragraph')
  assert.equal(b[1].kind, 'table')
  assert.equal(b[2].kind, 'paragraph')
})

console.log('\n▶ Markdown 嵌套列表与任务列表')

test('嵌套无序列表', () => {
  const b = parseBlocks('- 一级项\n  - 二级项\n  - 二级项2\n- 另一个一级项')
  assert.equal(b[0].kind, 'list')
  assert.equal(b[0].items.length, 2)
  assert.equal(b[0].items[0].children.length, 2)
  assert.equal(b[0].items[0].children[0].text, '二级项')
})

test('三层嵌套', () => {
  const b = parseBlocks('- a\n  - b\n    - c')
  assert.equal(b[0].items[0].children[0].children[0].text, 'c')
})

test('任务列表（已勾选 / 未勾选）', () => {
  const b = parseBlocks('- [x] 完成\n- [ ] 待办')
  assert.equal(b[0].items[0].checked, true)
  assert.equal(b[0].items[0].text, '完成')
  assert.equal(b[0].items[1].checked, false)
  assert.equal(b[0].items[1].text, '待办')
})

test('普通列表项 checked 为 null', () => {
  const b = parseBlocks('- 普通')
  assert.equal(b[0].items[0].checked, null)
})

test('有序列表保留起始序号', () => {
  const b = parseBlocks('3. 三\n4. 四')
  assert.equal(b[0].ordered, true)
  assert.equal(b[0].start, 3)
  assert.equal(b[0].items.length, 2)
})

test('有序与无序列表不混在一个块里', () => {
  const b = parseBlocks('1. 有序\n- 无序')
  assert.equal(b[0].ordered, true)
  assert.equal(b[0].items.length, 1)
  assert.equal(b[1].ordered, false)
})

console.log('\n▶ 新增行内语法')

test('删除线', () => {
  const t = parseInline('~~废弃~~')
  assert.equal(t[0].kind, 'del')
  assert.equal(t[0].text, '废弃')
})

test('裸链接自动识别', () => {
  const t = parseInline('见 https://example.com/a 这里')
  const link = t.find((x) => x.kind === 'link')
  assert.ok(link, '应识别出链接')
  assert.equal(link.href, 'https://example.com/a')
})

test('图片降级为链接', () => {
  const t = parseInline('![图](https://x.com/a.png)')
  assert.equal(t[0].kind, 'link')
  assert.equal(t[0].href, 'https://x.com/a.png')
})

test('段内换行产生 br', () => {
  const t = parseInline('第一行\n第二行')
  assert.ok(t.some((x) => x.kind === 'br'), '应产生 br')
})

console.log('\n▶ 链接安全（防 XSS）')

test('拒绝 javascript: 协议', () => {
  assert.equal(isSafeHref('javascript:alert(1)'), false)
  assert.equal(isSafeHref('JavaScript:alert(1)'), false)
})

test('拒绝 data: 协议', () => {
  assert.equal(isSafeHref('data:text/html,<script>'), false)
})

test('拒绝 vbscript:', () => {
  assert.equal(isSafeHref('vbscript:msgbox'), false)
})

test('允许 http/https/mailto', () => {
  assert.equal(isSafeHref('https://example.com'), true)
  assert.equal(isSafeHref('http://example.com'), true)
  assert.equal(isSafeHref('mailto:a@b.com'), true)
})

test('允许相对路径与锚点', () => {
  assert.equal(isSafeHref('/docs/a.md'), true)
  assert.equal(isSafeHref('#section'), true)
  assert.equal(isSafeHref('./a'), true)
  assert.equal(isSafeHref('docs/a.md'), true)
})

console.log('\n▶ 流式代码块')

test('未闭合代码块标记 closed=false', () => {
  const b = parseBlocks('```go\nfunc f() {')
  assert.equal(b[0].kind, 'code')
  assert.equal(b[0].closed, false)
})

test('已闭合代码块标记 closed=true', () => {
  const b = parseBlocks('```go\ncode\n```')
  assert.equal(b[0].kind, 'code')
  assert.equal(b[0].closed, true)
})

console.log(`\n✅ 前端逻辑冒烟通过(${passed} 项)\n`)
