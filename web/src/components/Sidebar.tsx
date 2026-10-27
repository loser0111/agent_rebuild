import { useMemo, useState } from 'react'
import { useChat } from '../state/chatStore'
import { useWorkspaces } from '../state/workspacesStore'
import type { Session } from '../api/types'
import { SessionSettingsDialog } from './SessionSettingsDialog'

/** 未绑定任何 workspace 的会话归到这个分组键下（不是真实的 workspace id）。 */
const UNGROUPED_KEY = '__ungrouped__'

interface SessionGroup {
  key: string
  label: string
  sessions: Session[]
  /** 组内最新的 updated_at（毫秒），用于给分组排序。 */
  latestUpdatedAt: number
}

/**
 * 按 workspace 对会话分组，组内保持原有顺序（updated_at 倒序，继承自
 * useChat().sessions），组间按"组内最新一条的时间"倒序——哪个 workspace
 * 最近有动静就排在最上面，而不是固定按名称排或者把未分组会话强制挤到最后。
 *
 * 只有当会话实际分布在 2 个以上的分组时才值得分组展示；只有 1 个分组
 * （典型情况：还没人用过 workspace，全部会话都未绑定）时返回 null，
 * 调用方据此退化为原来的扁平列表，避免给新用户徒增一个无意义的分组标题。
 */
function groupByWorkspace(
  sessions: Session[],
  workspaceNames: Map<string, string>,
): SessionGroup[] | null {
  const order: string[] = []
  const buckets = new Map<string, Session[]>()

  for (const s of sessions) {
    const key = s.workspace_id && s.workspace_id !== '' ? s.workspace_id : UNGROUPED_KEY
    if (!buckets.has(key)) {
      buckets.set(key, [])
      order.push(key)
    }
    buckets.get(key)!.push(s)
  }

  if (order.length < 2) return null

  const groups: SessionGroup[] = order.map((key) => {
    const list = buckets.get(key)!
    const label =
      key === UNGROUPED_KEY
        ? '未分组'
        : workspaceNames.get(key) ?? '已删除的 workspace'
    const latestUpdatedAt = list.reduce(
      (max, s) => Math.max(max, new Date(s.updated_at).getTime() || 0),
      0,
    )
    return { key, label, sessions: list, latestUpdatedAt }
  })

  groups.sort((a, b) => b.latestUpdatedAt - a.latestUpdatedAt)
  return groups
}

/** 左侧会话栏：新建、切换、删除、重命名；可按 workspace 分组展示。 */
export function Sidebar() {
  const {
    sessions,
    sessionsLoading,
    currentId,
    selectSession,
    deleteSession,
    isStreaming,
  } = useChat()
  const { workspaces } = useWorkspaces()

  const [pendingDelete, setPendingDelete] = useState<string | null>(null)
  // 新建会话走弹窗（绑定模型/workspace/审批模式），不再是点一下就直接建空会话。
  const [newSessionOpen, setNewSessionOpen] = useState(false)
  // 折叠状态只存在组件内存里，不做持久化：折叠本身是「临时收起视野」的操作，
  // 下次打开应用重新展开全部，比记住上次状态更不容易让人以为"会话丢了"。
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  const workspaceNames = useMemo(
    () => new Map(workspaces.map((w) => [w.id, w.name] as const)),
    [workspaces],
  )
  const groups = useMemo(
    () => groupByWorkspace(sessions, workspaceNames),
    [sessions, workspaceNames],
  )

  const toggleGroup = (key: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  const renderSession = (s: Session) => {
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
            <button className="mini" type="button" onClick={() => setPendingDelete(null)}>
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
  }

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <div className="brand">
          <span className="brand-mark">◆</span>
          <span className="brand-name">Agent Rebuild</span>
        </div>
        <button
          className="btn-new"
          onClick={() => setNewSessionOpen(true)}
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

        {!sessionsLoading && groups === null && sessions.map(renderSession)}

        {!sessionsLoading &&
          groups !== null &&
          groups.map((g) => {
            const isCollapsed = collapsed.has(g.key)
            return (
              <div className="session-group" key={g.key}>
                <button
                  className="session-group-head"
                  type="button"
                  onClick={() => toggleGroup(g.key)}
                  title={isCollapsed ? '展开' : '折叠'}
                >
                  <span className={`session-group-chevron${isCollapsed ? ' is-collapsed' : ''}`}>
                    ▾
                  </span>
                  <span className="session-group-title">{g.label}</span>
                  <span className="session-group-count">{g.sessions.length}</span>
                </button>
                {!isCollapsed && (
                  <div className="session-group-body">{g.sessions.map(renderSession)}</div>
                )}
              </div>
            )
          })}
      </div>

      <div className="sidebar-foot">
        <span className="hint">Enter 发送 · Shift+Enter 换行</span>
      </div>

      <SessionSettingsDialog open={newSessionOpen} onClose={() => setNewSessionOpen(false)} />
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
