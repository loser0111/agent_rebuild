import { useState } from 'react'
import { useChat } from '../state/chatStore'

/** 左侧会话栏：新建、切换、删除、重命名。 */
export function Sidebar() {
  const {
    sessions,
    sessionsLoading,
    currentId,
    createSession,
    selectSession,
    deleteSession,
    isStreaming,
  } = useChat()

  const [pendingDelete, setPendingDelete] = useState<string | null>(null)

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <div className="brand">
          <span className="brand-mark">◆</span>
          <span className="brand-name">Agent Rebuild</span>
        </div>
        <button
          className="btn-new"
          onClick={() => void createSession()}
          title="新建会话"
          type="button"
        >
          <span className="plus">+</span> 新建会话
        </button>
      </div>

      <div className="session-list" role="list">
        {sessionsLoading && <div className="session-empty">正在加载…</div>}

        {!sessionsLoading && sessions.length === 0 && (
          <div className="session-empty">
            还没有会话
            <br />
            点上方「新建会话」开始
          </div>
        )}

        {sessions.map((s) => {
          const active = s.id === currentId
          return (
            <div
              key={s.id}
              role="listitem"
              className={`session-item${active ? ' is-active' : ''}`}
              onClick={() => void selectSession(s.id)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') void selectSession(s.id)
              }}
              tabIndex={0}
            >
              <div className="session-main">
                <div className="session-title" title={s.title}>
                  {s.title || '未命名会话'}
                </div>
                <div className="session-meta">
                  <span>{formatRelative(s.updated_at)}</span>
                  <span className="dot-sep">·</span>
                  <span>{s.message_count} 条</span>
                  {s.status === 'running' && <span className="tag-running">生成中</span>}
                  {s.status === 'error' && <span className="tag-error">异常</span>}
                </div>
              </div>

              {pendingDelete === s.id ? (
                <div className="session-confirm" onClick={(e) => e.stopPropagation()}>
                  <button
                    className="mini danger"
                    type="button"
                    onClick={() => {
                      setPendingDelete(null)
                      void deleteSession(s.id)
                    }}
                  >
                    删除
                  </button>
                  <button
                    className="mini"
                    type="button"
                    onClick={() => setPendingDelete(null)}
                  >
                    取消
                  </button>
                </div>
              ) : (
                <button
                  className="session-del"
                  type="button"
                  title={isStreaming && active ? '生成中无法删除' : '删除会话'}
                  disabled={isStreaming && active}
                  onClick={(e) => {
                    e.stopPropagation()
                    setPendingDelete(s.id)
                  }}
                >
                  ×
                </button>
              )}
            </div>
          )
        })}
      </div>

      <div className="sidebar-foot">
        <span className="hint">Enter 发送 · Shift+Enter 换行</span>
      </div>
    </aside>
  )
}

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / MM-DD。 */
function formatRelative(iso: string): string {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return ''
  const diff = Date.now() - t
  if (diff < 60_000) return '刚刚'
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`
  const d = new Date(t)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}
