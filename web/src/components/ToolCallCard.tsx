import { useMemo, useState } from 'react'
import type { ToolCallView } from '../api/types'

/** 参数 JSON 美化；解析失败原样显示（模型可能产出不完整的 JSON）。 */
function prettyArgs(raw: string): string {
  const text = raw?.trim()
  if (!text) return '(无参数)'
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return raw
  }
}

/** 结果过长时截断，避免撑爆界面；保留足够上下文。 */
const MAX_OUTPUT = 4000

function clipOutput(text: string): { body: string; clipped: boolean } {
  if (text.length <= MAX_OUTPUT) return { body: text, clipped: false }
  return { body: text.slice(0, MAX_OUTPUT), clipped: true }
}

/** 单条工具调用卡片：可折叠，展示参数与结果。 */
export function ToolCallCard({ call }: { call: ToolCallView }) {
  // 运行中的调用默认展开（让用户看到正在做什么）；
  // 已完成的默认折叠（避免长输出淹没回答）。
  const [open, setOpen] = useState(call.status === 'running')

  const argsText = useMemo(() => prettyArgs(call.args), [call.args])

  const statusLabel =
    call.status === 'running'
      ? '调用中'
      : call.status === 'success'
        ? '完成'
        : call.status === 'error'
          ? '失败'
          : '已拒绝'

  const output = call.output ? clipOutput(call.output) : null

  return (
    <div className={`tool-card is-${call.status}`}>
      <button
        className="tool-card-head"
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        <span className={`tool-status-dot is-${call.status}`} aria-hidden="true" />

        <span className="tool-name">{call.name || '工具'}</span>

        <span className={`tool-status-text is-${call.status}`}>
          {call.status === 'running' && <span className="tool-spinner" aria-hidden="true" />}
          {statusLabel}
        </span>

        {call.risk === 'dangerous' && <span className="tool-badge is-danger">高危</span>}
        {call.risk === 'moderate' && <span className="tool-badge is-warn">写操作</span>}
        {call.requires_approval && <span className="tool-badge is-warn">需确认</span>}

        {typeof call.duration_ms === 'number' && call.status !== 'running' && (
          <span className="tool-duration">{call.duration_ms}ms</span>
        )}

        <span className={`tool-caret${open ? ' is-open' : ''}`} aria-hidden="true">
          ▾
        </span>
      </button>

      {open && (
        <div className="tool-card-body">
          <div className="tool-section">
            <div className="tool-section-label">请求参数</div>
            <pre className="tool-pre">{argsText}</pre>
          </div>

          {call.status === 'running' ? (
            <div className="tool-section">
              <div className="tool-section-label">执行结果</div>
              <div className="tool-pending">等待工具返回…</div>
            </div>
          ) : (
            <div className="tool-section">
              <div className="tool-section-label">
                执行结果
                {call.status === 'error' && <span className="tool-flag is-error">错误</span>}
                {call.status === 'rejected' && <span className="tool-flag is-warn">未执行</span>}
              </div>
              {output ? (
                <>
                  <pre className="tool-pre is-output">{output.body}</pre>
                  {output.clipped && (
                    <div className="tool-truncated">
                      输出过长，仅显示前 {MAX_OUTPUT} 字符
                    </div>
                  )}
                </>
              ) : (
                <div className="tool-pending">（无输出）</div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/** 一组工具调用（按到达顺序）。 */
export function ToolCallList({ calls }: { calls: ToolCallView[] }) {
  if (calls.length === 0) return null
  return (
    <div className="tool-list">
      {calls.map((c) => (
        <ToolCallCard key={c.call_id} call={c} />
      ))}
    </div>
  )
}

/** 把一组工具调用压缩成一句摘要，如「工具调用 3 次 · list_dir, search」。 */
function summarizeCalls(calls: ToolCallView[]): string {
  const names = Array.from(new Set(calls.map((c) => c.name).filter(Boolean)))
  const head = names.slice(0, 3).join(', ')
  const more = names.length > 3 ? ` 等 ${names.length} 个` : ''
  return head ? `${head}${more}` : '工具调用'
}

/**
 * 折叠式工具调用摘要。
 *
 * 默认收起成一行，挂在消息头部（Agent 标签右侧），避免大段工具输出
 * 淹没真正的回答；需要时点开才展开完整卡片。
 *
 * 运行中的调用会自动展开：用户需要看见"正在做什么"，
 * 收起反而像是卡住了。
 */
export function ToolCallSummary({ calls }: { calls: ToolCallView[] }) {
  const running = calls.some((c) => c.status === 'running')
  const anyError = calls.some((c) => c.status === 'error')
  const [open, setOpen] = useState(false)

  // 运行中强制展开（不写进 state，避免用户手动收起后又被覆盖）。
  const expanded = open || running

  if (calls.length === 0) return null

  return (
    <div className={`tool-summary${expanded ? ' is-open' : ''}`}>
      <button
        className={`tool-summary-btn${anyError ? ' has-error' : ''}${running ? ' is-running' : ''}`}
        type="button"
        onClick={() => setOpen((v) => !v)}
        title={expanded ? '收起工具调用详情' : '展开工具调用详情'}
      >
        {running && <span className="tool-spinner" aria-hidden="true" />}
        {!running && (
          <span
            className={`tool-status-dot is-${anyError ? 'error' : 'success'}`}
            aria-hidden="true"
          />
        )}
        <span className="tool-summary-count">
          {running ? '正在调用' : '工具调用'} {calls.length} 次
        </span>
        <span className="tool-summary-names">{summarizeCalls(calls)}</span>
        <span className={`tool-caret${expanded ? ' is-open' : ''}`} aria-hidden="true">
          ▾
        </span>
      </button>

      {expanded && (
        <div className="tool-summary-body">
          <ToolCallList calls={calls} />
        </div>
      )}
    </div>
  )
}
