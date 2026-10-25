import { useState, type ReactNode } from 'react'
import {
  isSafeHref,
  parseBlocks,
  parseInline,
  tokenize,
  type Align,
  type Inline,
  type ListItem,
} from '../lib/markdown'

/**
 * 渲染一段 Markdown 文本为 React 节点。
 *
 * streaming=true 时按「流式」模式解析：隐藏末尾未闭合的标记符号，
 * 避免逐字渲染过程中出现半截 `**` / `` ` `` 闪烁。
 */
export function Markdown({ text, streaming = false }: { text: string; streaming?: boolean }) {
  const blocks = parseBlocks(text)
  return (
    <div className="md">
      {blocks.map((b, i) => {
        // 只有最后一块可能是「正在输入中」的块。
        const isLast = i === blocks.length - 1
        const trailing = streaming && isLast
        switch (b.kind) {
          case 'code':
            return <CodeBlock key={i} code={b.code} lang={b.lang} streaming={!b.closed} />

          case 'table':
            return (
              <Table
                key={i}
                header={b.header}
                aligns={b.aligns}
                rows={b.rows}
                trailing={trailing}
              />
            )

          case 'heading': {
            // 对话场景里标题不宜过大，统一降 2 级。
            const Tag = `h${Math.min(b.level + 2, 6)}` as 'h3'
            return (
              <Tag key={i} className="md-heading">
                {renderInline(parseInline(b.text, trailing))}
              </Tag>
            )
          }

          case 'list':
            return (
              <ListBlock key={i} ordered={b.ordered} items={b.items} start={b.start} trailing={trailing} />
            )

          case 'quote':
            return (
              <blockquote key={i} className="md-quote">
                {b.lines.map((l, j) => (
                  <p key={j}>{renderInline(parseInline(l, trailing && j === b.lines.length - 1))}</p>
                ))}
              </blockquote>
            )

          case 'hr':
            return <hr key={i} className="md-hr" />

          default:
            return (
              <p key={i} className="md-p">
                {renderInline(parseInline(b.text, trailing))}
              </p>
            )
        }
      })}
    </div>
  )
}

/** 列表渲染：支持嵌套与任务列表。 */
function ListBlock({
  ordered,
  items,
  start,
  trailing,
}: {
  ordered: boolean
  items: ListItem[]
  start: number
  trailing: boolean
}) {
  const Tag = ordered ? 'ol' : 'ul'
  return (
    <Tag className="md-list" start={ordered ? start : undefined}>
      {items.map((item, i) => (
        <li key={i} className={item.checked !== null ? 'md-task' : undefined}>
          {item.checked !== null && (
            <input
              type="checkbox"
              checked={item.checked}
              readOnly
              className="md-task-box"
              tabIndex={-1}
            />
          )}
          <span>
            {renderInline(parseInline(item.text, trailing && i === items.length - 1))}
          </span>
          {item.children.length > 0 && (
            <ListBlock
              ordered={isOrderedList(item.children)}
              items={item.children}
              start={1}
              trailing={false}
            />
          )}
        </li>
      ))}
    </Tag>
  )
}

/** 子列表的有序性判断（简化：看子项数量>1 且都来自有序语法时由上层决定，这里默认无序）。 */
function isOrderedList(_items: ListItem[]): boolean {
  return false
}

/** 表格渲染：支持列对齐、横向滚动。 */
function Table({
  header,
  aligns,
  rows,
  trailing,
}: {
  header: string[]
  aligns: Align[]
  rows: string[][]
  trailing: boolean
}) {
  const style = (i: number): React.CSSProperties => {
    const a = aligns[i]
    if (a === 'center') return { textAlign: 'center' }
    if (a === 'right') return { textAlign: 'right' }
    return { textAlign: 'left' }
  }

  return (
    <div className="md-table-wrap">
      <table className="md-table">
        <thead>
          <tr>
            {header.map((h, i) => (
              <th key={i} style={style(i)}>
                {renderInline(parseInline(h))}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i}>
              {header.map((_, j) => (
                <td key={j} style={style(j)}>
                  {renderInline(
                    parseInline(row[j] ?? '', trailing && i === rows.length - 1),
                  )}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function renderInline(inlines: Inline[]): ReactNode[] {
  return inlines.map((t, i) => {
    switch (t.kind) {
      case 'code':
        return (
          <code key={i} className="inline-code">
            {t.text}
          </code>
        )
      case 'strong':
        return <strong key={i}>{t.text}</strong>
      case 'em':
        return <em key={i}>{t.text}</em>
      case 'del':
        return <del key={i}>{t.text}</del>
      case 'br':
        return <br key={i} />
      case 'link':
        // 协议白名单：阻止 javascript: 等可执行协议。
        if (!isSafeHref(t.href)) {
          return (
            <span key={i} className="md-unsafe-link" title="不安全的链接已禁用">
              {t.text}
            </span>
          )
        }
        return (
          <a key={i} href={t.href} target="_blank" rel="noreferrer noopener">
            {t.text}
          </a>
        )
      default:
        return <span key={i}>{t.text}</span>
    }
  })
}

/**
 * 代码块：等宽字体 + 轻量语法高亮 + 语言标签 + 复制按钮。
 *
 * streaming=true 表示流式输出中围栏尚未闭合，此时隐藏复制按钮
 * （内容不完整，复制没意义）。
 */
export function CodeBlock({
  code,
  lang,
  streaming = false,
}: {
  code: string
  lang: string
  streaming?: boolean
}) {
  const [copied, setCopied] = useState(false)
  const tokens = tokenize(code, lang)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      // 非 HTTPS 环境下 clipboard 可能不可用，降级用 textarea 方案。
      const ta = document.createElement('textarea')
      ta.value = code
      document.body.appendChild(ta)
      ta.select()
      try {
        document.execCommand('copy')
        setCopied(true)
        window.setTimeout(() => setCopied(false), 1500)
      } finally {
        document.body.removeChild(ta)
      }
    }
  }

  return (
    <div className="code-block">
      <div className="code-head">
        <span className="code-lang">{lang || 'text'}</span>
        {!streaming && (
          <button className="code-copy" onClick={copy} type="button">
            {copied ? '已复制' : '复制'}
          </button>
        )}
      </div>
      <pre>
        <code>
          {tokens.map((t, i) => (
            <span key={i} className={`tok-${t.type}`}>
              {t.text}
            </span>
          ))}
        </code>
      </pre>
    </div>
  )
}
