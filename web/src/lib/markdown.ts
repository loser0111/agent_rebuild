/* 轻量 Markdown 渲染：覆盖对话场景需要的子集。
 *
 * 为什么不引 react-markdown：
 *   - 体积（+80KB 依赖链）与版本风险
 *   - 流式渲染需要精细控制「未闭合标记」的处理，第三方库反而难改
 *
 * 支持的语法：
 *   块级：代码块（含未闭合的流式态）、标题、有序/无序列表（嵌套 + 任务列表）、
 *        引用、分隔线、表格（含对齐）、段内换行
 *   行内：行内代码、粗体、斜体、删除线、链接、裸链接自动识别
 *
 * 设计：先按行切块，再对行内语法做 tokenize。
 * 所有输出都是 React 元素，不使用 dangerouslySetInnerHTML，天然免疫 XSS。
 */

export type Align = 'left' | 'center' | 'right' | null

export type Block =
  | { kind: 'code'; lang: string; code: string; closed: boolean }
  | { kind: 'heading'; level: number; text: string }
  | { kind: 'list'; ordered: boolean; items: ListItem[]; start: number }
  | { kind: 'quote'; lines: string[] }
  | { kind: 'paragraph'; text: string }
  | { kind: 'hr' }
  | { kind: 'table'; header: string[]; aligns: Align[]; rows: string[][] }

/** 列表项：支持嵌套（children）与任务列表（checked）。 */
export interface ListItem {
  text: string
  /** 任务列表：true/false 表示勾选态，null 表示普通列表项。 */
  checked: boolean | null
  children: ListItem[]
}

/** 把原文切成块级结构。 */
export function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n/g, '\n').split('\n')
  const blocks: Block[] = []
  let i = 0

  while (i < lines.length) {
    const line = lines[i]

    // ---- 代码围栏 ```lang / ~~~ ----
    const fence = /^\s*(```|~~~)\s*([\w+#.-]*)\s*$/.exec(line)
    if (fence) {
      const marker = fence[1]
      const lang = fence[2] || ''
      const buf: string[] = []
      i++
      let closed = false
      while (i < lines.length) {
        if (new RegExp(`^\\s*${marker}\\s*$`).test(lines[i])) {
          closed = true
          i++
          break
        }
        buf.push(lines[i])
        i++
      }
      // closed=false 表示流式输出中围栏还没闭合，前端仍要当成代码块渲染
      blocks.push({ kind: 'code', lang, code: buf.join('\n'), closed })
      continue
    }

    if (line.trim() === '') {
      i++
      continue
    }

    // ---- 分隔线 ----
    if (/^\s*([-*_])\1{2,}\s*$/.test(line)) {
      blocks.push({ kind: 'hr' })
      i++
      continue
    }

    // ---- 表格 ----
    // 判定：当前行含 |，下一行是分隔行（如 | --- | :--: |）
    if (line.includes('|') && i + 1 < lines.length && isTableSeparator(lines[i + 1])) {
      const header = splitTableRow(line)
      const aligns = parseAligns(lines[i + 1])
      i += 2
      const rows: string[][] = []
      while (i < lines.length && lines[i].includes('|') && lines[i].trim() !== '') {
        rows.push(splitTableRow(lines[i]))
        i++
      }
      blocks.push({ kind: 'table', header, aligns, rows })
      continue
    }

    // ---- 标题 ----
    const heading = /^(#{1,6})\s+(.*)$/.exec(line)
    if (heading) {
      blocks.push({ kind: 'heading', level: heading[1].length, text: heading[2].trim() })
      i++
      continue
    }

    // ---- 引用 ----
    if (/^\s*>\s?/.test(line)) {
      const buf: string[] = []
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        buf.push(lines[i].replace(/^\s*>\s?/, ''))
        i++
      }
      blocks.push({ kind: 'quote', lines: buf })
      continue
    }

    // ---- 列表（支持嵌套与任务列表）----
    const listMatch = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(line)
    if (listMatch) {
      const ordered = /^\d/.test(listMatch[2])
      const startNum = ordered ? Number.parseInt(listMatch[2], 10) || 1 : 1
      const { items, next } = parseList(lines, i, ordered)
      blocks.push({ kind: 'list', ordered, items, start: startNum })
      i = next
      continue
    }

    // ---- 段落：连续非空行合并（保留段内换行）----
    const buf: string[] = []
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !/^\s*(```|~~~)/.test(lines[i]) &&
      !/^(#{1,6})\s+/.test(lines[i]) &&
      !/^\s*>\s?/.test(lines[i]) &&
      !/^(\s*)([-*+]|\d+[.)])\s+/.test(lines[i]) &&
      !/^\s*([-*_])\1{2,}\s*$/.test(lines[i])
    ) {
      buf.push(lines[i])
      i++
    }
    if (buf.length > 0) {
      blocks.push({ kind: 'paragraph', text: buf.join('\n') })
    }
  }

  return blocks
}

/* ---------------- 列表解析（递归处理嵌套） ---------------- */

interface ParsedList {
  items: ListItem[]
  next: number
}

/** 计算一行开头的缩进宽度（tab 按 2 空格算）。 */
function indentOf(line: string): number {
  let width = 0
  for (const ch of line) {
    if (ch === ' ') width++
    else if (ch === '\t') width += 2
    else break
  }
  return width
}

/**
 * 解析从 start 开始的一个列表（可能嵌套）。
 *
 * 嵌套判定：后续行缩进 > 当前列表项缩进，则视为该项的子列表。
 * 这是手写 Markdown 解析最容易出错的地方，这里单独成函数并有测试覆盖。
 */
function parseList(lines: string[], start: number, ordered: boolean): ParsedList {
  const items: ListItem[] = []
  let i = start
  let baseIndent = -1

  while (i < lines.length) {
    const line = lines[i]
    const m = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(line)
    if (!m) {
      // 允许列表项内的续行（缩进的非列表行），并入上一项文本
      if (items.length > 0 && line.trim() !== '' && indentOf(line) > (baseIndent < 0 ? 0 : baseIndent)) {
        items[items.length - 1].text += '\n' + line.trim()
        i++
        continue
      }
      break
    }

    const indent = indentOf(line)
    const itemOrdered = /^\d/.test(m[2])

    if (baseIndent < 0) {
      baseIndent = indent
    }
    // 缩进小于基础缩进 → 属于外层列表，结束本次解析
    if (indent < baseIndent) break
    // 同级但列表类型不同 → 视为新列表
    if (indent === baseIndent && itemOrdered !== ordered) break

    // 缩进大于基础缩进 → 子列表，递归解析
    if (indent > baseIndent && items.length > 0) {
      const sub = parseList(lines, i, itemOrdered)
      items[items.length - 1].children.push(...sub.items)
      i = sub.next
      continue
    }

    const rawText = m[3]
    // 任务列表：[ ] / [x] / [X]
    const task = /^\[([ xX])\]\s+(.*)$/.exec(rawText)
    if (task) {
      items.push({ text: task[2], checked: task[1].toLowerCase() === 'x', children: [] })
    } else {
      items.push({ text: rawText, checked: null, children: [] })
    }
    i++
  }

  return { items, next: i }
}

/* ---------------- 表格解析 ---------------- */

/** 判断一行是否是表格分隔行：| --- | :--: | ---: | */
function isTableSeparator(line: string): boolean {
  const trimmed = line.trim()
  if (!trimmed.includes('-')) return false
  // 去掉首尾 | 后按 | 切分，每段必须是 :?-{1,}:?
  const cells = trimmed.replace(/^\|/, '').replace(/\|$/, '').split('|')
  if (cells.length === 0) return false
  return cells.every((c) => /^\s*:?-+:?\s*$/.test(c))
}

/** 切分表格行，忽略首尾管道符，处理转义的 \|。 */
export function splitTableRow(line: string): string[] {
  let s = line.trim()
  if (s.startsWith('|')) s = s.slice(1)
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1)

  const cells: string[] = []
  let buf = ''
  for (let i = 0; i < s.length; i++) {
    const ch = s[i]
    if (ch === '\\' && s[i + 1] === '|') {
      buf += '|'
      i++
      continue
    }
    if (ch === '|') {
      cells.push(buf.trim())
      buf = ''
      continue
    }
    buf += ch
  }
  cells.push(buf.trim())
  return cells
}

/** 解析对齐方式。 */
export function parseAligns(line: string): Align[] {
  const s = line.trim().replace(/^\|/, '').replace(/\|$/, '')
  return s.split('|').map((c) => {
    const t = c.trim()
    const left = t.startsWith(':')
    const right = t.endsWith(':')
    if (left && right) return 'center'
    if (right) return 'right'
    if (left) return 'left'
    return null
  })
}

/* ---------------- 行内解析 ---------------- */

/** 行内 token。 */
export type Inline =
  | { kind: 'text'; text: string }
  | { kind: 'code'; text: string }
  | { kind: 'strong'; text: string }
  | { kind: 'em'; text: string }
  | { kind: 'del'; text: string }
  | { kind: 'link'; text: string; href: string }
  | { kind: 'br' }

/**
 * 解析行内语法。顺序很重要：行内代码优先级最高，避免 `**` 被误解析。
 *
 * 当 trailing=true（流式渲染中）时，末尾未闭合的标记字符会被隐藏，
 * 避免用户看到 `**` / `` ` `` 这类半截语法符号闪烁。
 */
export function parseInline(src: string, trailing = false): Inline[] {
  const out: Inline[] = []
  let rest = src

  const pushText = (t: string) => {
    if (!t) return
    const last = out[out.length - 1]
    if (last && last.kind === 'text') {
      last.text += t
      return
    }
    out.push({ kind: 'text', text: t })
  }

  while (rest.length > 0) {
    // 硬换行（段内 \n）
    if (rest[0] === '\n') {
      out.push({ kind: 'br' })
      rest = rest.slice(1)
      continue
    }

    // 行内代码（支持 `` 双反引号包裹含反引号的内容）
    const codeDouble = /^``([^`]|`(?!`))+``/.exec(rest)
    if (codeDouble) {
      out.push({ kind: 'code', text: codeDouble[0].slice(2, -2).trim() })
      rest = rest.slice(codeDouble[0].length)
      continue
    }
    const code = /^`([^`\n]+)`/.exec(rest)
    if (code) {
      out.push({ kind: 'code', text: code[1] })
      rest = rest.slice(code[0].length)
      continue
    }

    // 粗体（**text** 或 __text__）
    const strong = /^(\*\*|__)((?:[^_]|\/(?!\1))[\s\S]*?)\1/.exec(rest)
    if (strong) {
      out.push({ kind: 'strong', text: strong[2] })
      rest = rest.slice(strong[0].length)
      continue
    }

    // 删除线 ~~text~~
    const del = /^~~([^~\n]+)~~/.exec(rest)
    if (del) {
      out.push({ kind: 'del', text: del[1] })
      rest = rest.slice(del[0].length)
      continue
    }

    // 斜体（*text* 或 _text_，避免与粗体冲突）
    const em = /^[*_]([^*_\n]+)[*_]/.exec(rest)
    if (em) {
      out.push({ kind: 'em', text: em[1] })
      rest = rest.slice(em[0].length)
      continue
    }

    // 链接 [text](url) 与图片 ![alt](url)（图片降级为链接展示）
    const image = /^!\[([^\]]*)\]\(([^)\s]+)\)/.exec(rest)
    if (image) {
      out.push({ kind: 'link', text: image[1] || image[2], href: image[2] })
      rest = rest.slice(image[0].length)
      continue
    }
    const link = /^\[([^\]]+)\]\(([^)\s]+)\)/.exec(rest)
    if (link) {
      out.push({ kind: 'link', text: link[1], href: link[2] })
      rest = rest.slice(link[0].length)
      continue
    }

    // 裸链接 http(s)://...（模型常直接贴 URL）
    const bare = /^https?:\/\/[^\s<>)\]"']+/.exec(rest)
    if (bare) {
      out.push({ kind: 'link', text: bare[0], href: bare[0] })
      rest = rest.slice(bare[0].length)
      continue
    }

    // 普通字符（一次吃到下一个特殊符号前）
    const nextSpecial = rest.slice(1).search(/[`*_[\]~\n]|https?:\/\//)
    const take = nextSpecial === -1 ? rest.length : nextSpecial + 1
    pushText(rest.slice(0, take))
    rest = rest.slice(take)
  }

  if (trailing && out.length > 0) {
    return stripTrailingMarker(out)
  }
  return out
}

/**
 * 去掉末尾未闭合的 Markdown 标记符号（流式渲染专用）。
 *
 * 例：`消息：*` → `消息：`，``看 `co`` → `看 co`。
 * 这样逐字渲染时不会出现半截 `**` 或反引号闪烁。
 *
 * 只删**标记符号本身**，不删内容：流式输出到一半时，`co` 是模型真的
 * 要说的话，吞掉它会导致文字闪一下又出现，比看到半截反引号更难受。
 *
 * 两种未闭合形态：
 *  1. 标记滞留在末尾（`消息：*`、`强调**`）—— 剥掉尾部所有标记字符；
 *  2. 标记在中间、后面还有内容（``看 `co``）—— 只删掉那个未配对的反引号。
 */
function stripTrailingMarker(inlines: Inline[]): Inline[] {
  const copy = inlines.map((t) => ({ ...t }))
  const last = copy[copy.length - 1]
  if (!last || last.kind !== 'text') return copy

  const before = last.text
  let text = before

  // 形态 1：尾部堆叠的标记字符（`**` 没有内容包住也算未闭合）。
  text = text.replace(/[`*_~[\]]+$/, '')

  // 形态 2：中间残留奇数个反引号 → 行内代码未闭合，删掉最后一个反引号。
  const backticks = (text.match(/`/g) ?? []).length
  if (backticks % 2 === 1) {
    const i = text.lastIndexOf('`')
    text = text.slice(0, i) + text.slice(i + 1)
  }

  if (text === before) return copy
  if (text === '') {
    copy.pop()
  } else {
    last.text = text
  }
  return copy
}

/**
 * 链接协议白名单校验。
 *
 * 必须做：模型可能输出 `[点我](javascript:alert(1))`，
 * 直接渲染成 <a href> 就是可点击的 XSS。
 */
export function isSafeHref(href: string): boolean {
  const h = href.trim().toLowerCase()
  if (h.startsWith('http://') || h.startsWith('https://')) return true
  if (h.startsWith('mailto:')) return true
  if (h.startsWith('/') || h.startsWith('#') || h.startsWith('./') || h.startsWith('../')) return true
  // 无协议的相对路径（如 docs/a.md）
  return !/^[a-z][a-z0-9+.-]*:/.test(h)
}

/* ---------------- 语法高亮 ---------------- */

type TokenType =
  | 'keyword'
  | 'string'
  | 'comment'
  | 'number'
  | 'func'
  | 'type'
  | 'punct'
  | 'plain'

export interface Token {
  type: TokenType
  text: string
}

/** 各语言的保留字（够用即可，不追求完备）。 */
const KEYWORDS: Record<string, string[]> = {
  go: [
    'package', 'import', 'func', 'return', 'if', 'else', 'for', 'range', 'type', 'struct',
    'interface', 'map', 'chan', 'go', 'defer', 'select', 'case', 'default', 'switch',
    'var', 'const', 'nil', 'true', 'false', 'break', 'continue', 'fallthrough', 'make',
    'new', 'error', 'string', 'int', 'bool', 'byte', 'rune', 'any',
  ],
  ts: [
    'import', 'export', 'from', 'const', 'let', 'var', 'function', 'return', 'if', 'else',
    'for', 'while', 'class', 'interface', 'type', 'extends', 'implements', 'new', 'async',
    'await', 'try', 'catch', 'finally', 'throw', 'null', 'undefined', 'true', 'false',
    'this', 'super', 'default', 'as', 'enum', 'readonly', 'public', 'private', 'void',
  ],
  js: [
    'import', 'export', 'from', 'const', 'let', 'var', 'function', 'return', 'if', 'else',
    'for', 'while', 'class', 'extends', 'new', 'async', 'await', 'try', 'catch', 'finally',
    'throw', 'null', 'undefined', 'true', 'false', 'this', 'super', 'default', 'typeof',
  ],
  python: [
    'def', 'class', 'import', 'from', 'return', 'if', 'elif', 'else', 'for', 'while',
    'try', 'except', 'finally', 'with', 'as', 'lambda', 'None', 'True', 'False', 'and',
    'or', 'not', 'in', 'is', 'pass', 'raise', 'yield', 'async', 'await', 'self',
  ],
  bash: [
    'if', 'then', 'else', 'elif', 'fi', 'for', 'in', 'do', 'done', 'while', 'case', 'esac',
    'function', 'return', 'export', 'local', 'echo', 'cd', 'set', 'exit',
  ],
  json: ['true', 'false', 'null'],
  sql: [
    'select', 'from', 'where', 'insert', 'into', 'values', 'update', 'set', 'delete',
    'join', 'left', 'right', 'inner', 'outer', 'on', 'group', 'by', 'order', 'having',
    'limit', 'offset', 'create', 'table', 'index', 'primary', 'key', 'as', 'and', 'or',
  ],
}

const ALIASES: Record<string, string> = {
  golang: 'go',
  typescript: 'ts',
  tsx: 'ts',
  javascript: 'js',
  jsx: 'js',
  py: 'python',
  sh: 'bash',
  shell: 'bash',
  zsh: 'bash',
  yml: 'yaml',
}

function normalizeLang(lang: string): string {
  const l = lang.toLowerCase()
  return ALIASES[l] ?? l
}

/** 词法切分，返回带类型的 token 列表。支持 // 与 # 注释、字符串、数字。 */
export function tokenize(code: string, lang: string): Token[] {
  const language = normalizeLang(lang)
  const keywords = KEYWORDS[language] ?? []
  const kw = new Set(keywords)
  const tokens: Token[] = []
  let i = 0

  const push = (type: TokenType, text: string) => {
    if (!text) return
    tokens.push({ type, text })
  }

  while (i < code.length) {
    const ch = code[i]
    const rest = code.slice(i)

    // 行注释：// 或 #
    if (rest.startsWith('//') || (ch === '#' && language !== 'json')) {
      const end = code.indexOf('\n', i)
      const text = end === -1 ? rest : code.slice(i, end)
      push('comment', text)
      i += text.length
      continue
    }

    // 块注释 /* ... */
    if (rest.startsWith('/*')) {
      const end = code.indexOf('*/', i + 2)
      const stop = end === -1 ? code.length : end + 2
      push('comment', code.slice(i, stop))
      i = stop
      continue
    }

    // 字符串（' " `）
    if (ch === '"' || ch === "'" || ch === '`') {
      let j = i + 1
      while (j < code.length) {
        if (code[j] === '\\') {
          j += 2
          continue
        }
        if (code[j] === ch) {
          j++
          break
        }
        if (code[j] === '\n' && ch !== '`') break
        j++
      }
      push('string', code.slice(i, j))
      i = j
      continue
    }

    // 数字
    if (/[0-9]/.test(ch)) {
      const m = /^[0-9][0-9a-fA-FxX._]*/.exec(rest)
      if (m) {
        push('number', m[0])
        i += m[0].length
        continue
      }
    }

    // 标识符 / 关键字
    if (/[A-Za-z_$]/.test(ch)) {
      const m = /^[A-Za-z_$][\w$]*/.exec(rest)
      if (m) {
        const word = m[0]
        const lower = word.toLowerCase()
        if (kw.has(word) || kw.has(lower)) {
          push('keyword', word)
        } else if (code[i + word.length] === '(') {
          push('func', word)
        } else if (/^[A-Z]/.test(word)) {
          push('type', word)
        } else {
          push('plain', word)
        }
        i += word.length
        continue
      }
    }

    // 标点与空白
    if (/[\s]/.test(ch)) {
      let j = i
      while (j < code.length && /\s/.test(code[j])) j++
      push('plain', code.slice(i, j))
      i = j
      continue
    }
    push('punct', ch)
    i++
  }

  return tokens
}
