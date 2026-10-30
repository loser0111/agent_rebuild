import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type {
  McpServerStatus,
  McpServersResponse,
  McpStatus,
  McpToolView,
  ToolOverrideInput,
  ToolRisk,
} from '../api/types'

interface McpPanelProps {
  open: boolean
  onClose: () => void
}

/** 面板打开期间的轮询间隔：connecting → ready 通常在几秒内完成，2s 足够跟上又不吵。 */
const POLL_MS = 2000

const STATUS_LABEL: Record<McpStatus, string> = {
  connecting: '连接中',
  ready: '已就绪',
  retrying: '重连中',
  failed: '已失败',
  disabled: '已禁用',
}

const STATUS_DOT: Record<McpStatus, string> = {
  connecting: 'is-busy',
  ready: 'is-ok',
  retrying: 'is-warn',
  failed: 'is-error',
  disabled: '',
}

const EXAMPLE = `{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/notes"],
      "trust_annotations": true
    }
  }
}`

/**
 * MCP 服务面板：每个 server 的连接状态、重启；每个工具的风险等级与开关
 * （见 docs/17-mcp-client.md 第 5、8 节）。
 *
 * 能改的只有「风险等级」和「开关」，存 SQLite，改完立即对之后的对话生效。
 * server 配置本身（command 等）仍然只能改文件：command 会被直接执行，
 * 从 UI 写入等于给前端开了一个「执行任意命令」的口子。
 *
 * 轮询只在面板打开时进行，关闭即停——状态只有用户在看时才有意义。
 */
export function McpPanel({ open, onClose }: McpPanelProps) {
  const [data, setData] = useState<McpServersResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [restarting, setRestarting] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  /** 正在保存的工具全名：保存期间禁用该行控件，避免连点产生乱序请求 */
  const [saving, setSaving] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      setData(await api.listMcpServers())
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    }
  }, [])

  useEffect(() => {
    if (!open) return
    void refresh()
    const timer = window.setInterval(() => void refresh(), POLL_MS)
    return () => window.clearInterval(timer)
  }, [open, refresh])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  const restart = async (name: string) => {
    setRestarting(name)
    try {
      await api.restartMcpServer(name)
      await refresh()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setRestarting(null)
    }
  }

  /**
   * 保存一个工具的设置。PUT 是整体替换语义，所以总是带上两个字段的当前覆盖值，
   * 只改其中一个。把风险调成「安全」需要确认：安全 = 任何审批模式下都直接执行。
   */
  const saveOverride = async (t: McpToolView, patch: Partial<Omit<ToolOverrideInput, 'tool'>>) => {
    const next: ToolOverrideInput = {
      tool: t.name,
      risk: t.override_risk,
      enabled: t.override_enabled,
      ...patch,
    }
    const becomesSafe = (next.risk || t.base_risk) === 'safe' && t.risk !== 'safe'
    if (
      becomesSafe &&
      !window.confirm(`把「${t.name}」设为「安全」后，它在任何审批模式下都会直接执行、不再请你确认。确定吗？`)
    ) {
      return
    }
    setSaving(t.name)
    try {
      setData(await api.setToolOverride(next))
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(null)
    }
  }

  const toggle = (name: string) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(name)) next.delete(name)
      else next.add(name)
      return next
    })
  }

  const servers = data?.servers ?? []

  return (
    <div className="modal-scrim" onMouseDown={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="MCP 服务"
        onMouseDown={(e) => e.stopPropagation()}
        data-no-window-drag
      >
        <header className="modal-head">
          <h2 className="modal-title">MCP 服务</h2>
          <button className="icon-btn" type="button" onClick={onClose} title="关闭">
            ✕
          </button>
        </header>

        <div className="modal-body">
          {error && <div className="notice notice-error">{error}</div>}

          {data && !data.enabled && (
            <div className="notice notice-warn">
              MCP 已关闭。去掉 <code>MCP_ENABLED=false</code> 后重启应用即可启用。
            </div>
          )}

          {data && data.enabled && servers.length === 0 && (
            <EmptyHint paths={data.config_paths} />
          )}

          {!data && !error && <div className="model-empty">加载中…</div>}

          {servers.length > 0 && (
            <div className="model-list">
              {servers.map((s) => (
                <ServerRow
                  key={s.name}
                  server={s}
                  expanded={expanded.has(s.name)}
                  restarting={restarting === s.name}
                  onToggle={() => toggle(s.name)}
                  onRestart={() => void restart(s.name)}
                  editable={Boolean(data?.overrides_supported)}
                  saving={saving}
                  onSave={(t, patch) => void saveOverride(t, patch)}
                />
              ))}
            </div>
          )}

          {data && data.enabled && servers.length > 0 && (
            <div className="field-hint">
              这些工具不直接暴露给模型，模型通过 <code>search_router</code> 发现并执行它们。
              默认按「高危」处理，可以逐个调整风险等级或关闭；改动对之后的对话生效。
              {!data.overrides_supported && (
                <> 当前以内存存储运行，不能保存设置，请用 <code>STORE_DRIVER=sqlite</code> 启动。</>
              )}
              <br />
              配置文件：{data.config_paths.join('、') || '（无）'}。修改配置文件后需重启应用。
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

interface ServerRowProps {
  server: McpServerStatus
  expanded: boolean
  restarting: boolean
  onToggle: () => void
  onRestart: () => void
  editable: boolean
  saving: string | null
  onSave: (t: McpToolView, patch: Partial<Omit<ToolOverrideInput, 'tool'>>) => void
}

function ServerRow({
  server: s,
  expanded,
  restarting,
  onToggle,
  onRestart,
  editable,
  saving,
  onSave,
}: ServerRowProps) {
  // disabled 与配置无效的 server 重启没用（后端回 409），由后端告知，前端不自己猜。
  const canRestart = s.restartable
  const enabledCount = s.tools.filter((t) => t.enabled).length
  const sub = [
    s.server_name && `${s.server_name}${s.server_version ? ' ' + s.server_version : ''}`,
    s.status === 'ready' && `${enabledCount}/${s.tools.length} 个工具启用`,
    s.restarts > 0 && `重连 ${s.restarts} 次`,
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <div className="model-row mcp-row">
      <div className="model-row-main">
        <div className="model-row-title">
          <span className={`status-dot ${STATUS_DOT[s.status]}`} />
          <span className="model-row-name">{s.name}</span>
          <span className="mcp-status-label">{STATUS_LABEL[s.status]}</span>
        </div>
        {s.description && <div className="model-row-sub">{s.description}</div>}
        <div className="model-row-sub">{sub || s.command || s.source}</div>

        {s.last_error && <pre className="mcp-error">{s.last_error}</pre>}

        {s.tools.length > 0 && (
          <button className="btn-ghost mcp-tools-toggle" type="button" onClick={onToggle}>
            {expanded ? '收起工具' : `查看与设置工具（${s.tools.length}）`}
          </button>
        )}
        {expanded && (
          <ul className="mcp-tools">
            {s.tools.map((t) => (
              <ToolRow
                key={t.name}
                tool={t}
                editable={editable}
                busy={saving === t.name}
                onSave={(patch) => onSave(t, patch)}
              />
            ))}
          </ul>
        )}
      </div>

      <div className="model-row-actions">
        {canRestart && (
          <button className="btn-ghost" type="button" disabled={restarting} onClick={onRestart}>
            {restarting ? '重启中…' : '重启'}
          </button>
        )}
      </div>
    </div>
  )
}

const RISK_LABEL: Record<ToolRisk, string> = {
  safe: '安全',
  moderate: '写操作',
  dangerous: '高危',
}

interface ToolRowProps {
  tool: McpToolView
  editable: boolean
  busy: boolean
  onSave: (patch: Partial<Omit<ToolOverrideInput, 'tool'>>) => void
}

/**
 * 一个工具的设置行：开关 + 风险下拉。
 *
 * 下拉的第一项是「默认（xx）」= 跟随 mcp.json / annotations 算出的基础值，
 * 选它就是恢复默认；开关同理，和基础值一致时发 null（恢复默认），
 * 这样以后改 mcp.json 仍然能生效，而不是被一次无意义的覆盖钉死。
 */
function ToolRow({ tool: t, editable, busy, onSave }: ToolRowProps) {
  const disabled = !editable || busy
  const overridden = t.override_risk !== '' || t.override_enabled !== null
  return (
    <li className={`mcp-tool${t.enabled ? '' : ' is-off'}`}>
      <label className="mcp-tool-switch" title={t.enabled ? '已启用，点击关闭' : '已关闭，点击启用'}>
        <input
          type="checkbox"
          checked={t.enabled}
          disabled={disabled}
          onChange={(e) => {
            const want = e.target.checked
            onSave({ enabled: want === t.base_enabled ? null : want })
          }}
        />
      </label>
      <div className="mcp-tool-main">
        <code title={t.name}>{t.original_name}</code>
        {t.description && <div className="mcp-tool-desc">{t.description}</div>}
      </div>
      <select
        className="text-input mcp-tool-risk"
        value={t.override_risk}
        disabled={disabled}
        onChange={(e) => onSave({ risk: e.target.value as ToolRisk | '' })}
        title="风险等级决定是否需要你确认后才执行"
      >
        <option value="">默认（{RISK_LABEL[t.base_risk]}）</option>
        <option value="safe">安全（直接执行）</option>
        <option value="moderate">写操作（需确认）</option>
        <option value="dangerous">高危（需确认）</option>
      </select>
      {overridden && editable && (
        <button
          className="btn-ghost mcp-tool-reset"
          type="button"
          disabled={disabled}
          title="两项都恢复为配置文件里的默认值"
          onClick={() => onSave({ risk: '', enabled: null })}
        >
          恢复默认
        </button>
      )}
    </li>
  )
}

function EmptyHint({ paths }: { paths: string[] }) {
  return (
    <div className="notice">
      <div>还没有配置任何 MCP server。在下面的位置创建配置文件后重启应用：</div>
      <ul className="mcp-paths">
        {paths.length === 0 && <li>（未能确定配置路径，可用 MCP_CONFIG 环境变量指定）</li>}
        {paths.map((p) => (
          <li key={p}>
            <code>{p}</code>
          </li>
        ))}
      </ul>
      <div>格式与 Claude Desktop / Claude Code 的 mcpServers 相同，例如：</div>
      <pre className="mcp-example">{EXAMPLE}</pre>
    </div>
  )
}
