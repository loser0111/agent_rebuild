import { useEffect, useState } from 'react'
import type { Workspace, WorkspaceInput } from '../api/types'
import { useWorkspaces } from '../state/workspacesStore'

interface WorkspaceManagerProps {
  open: boolean
  onClose: () => void
}

/** 表单草稿：folders 直接是数组，不再经过文本解析。 */
type Draft = {
  id?: string
  name: string
  folders: string[]
}

function emptyDraft(): Draft {
  return { name: '', folders: [] }
}

function draftFrom(ws: Workspace): Draft {
  return { id: ws.id, name: ws.name, folders: [...ws.folders] }
}

function toInput(draft: Draft): WorkspaceInput {
  return { name: draft.name, folders: draft.folders }
}

/** 是否处于桌面壳（能拿到原生文件夹选择器）。非桌面端（纯浏览器调试）降级为手动输路径。 */
function hasFileDialog(): boolean {
  return typeof window !== 'undefined' && Boolean(window.fileDialog)
}

/**
 * workspace 管理弹窗：列表 + 表单。
 *
 * folders 的编辑方式：桌面端点「选择文件夹…」弹系统原生目录选择器
 * （经 desktop/preload.js 暴露的 window.fileDialog，拿到的是真实绝对
 * 路径，不会手误打错）；非桌面端（没有 Electron 壳时，比如纯浏览器调试）
 * 退化成一个手动输入框 + 添加按钮，两条路径最终都落到同一个 folders 数组。
 */
export function WorkspaceManager({ open, onClose }: WorkspaceManagerProps) {
  const {
    workspaces,
    loading,
    unsupported,
    error,
    refresh,
    createWorkspace,
    updateWorkspace,
    deleteWorkspace,
    dismissError,
  } = useWorkspaces()

  const [draft, setDraft] = useState<Draft | null>(null)
  const [saving, setSaving] = useState(false)
  const [picking, setPicking] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [manualPath, setManualPath] = useState('')

  useEffect(() => {
    if (!open) {
      setDraft(null)
      setFormError(null)
      setManualPath('')
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  const submit = async () => {
    if (!draft) return
    setSaving(true)
    setFormError(null)
    try {
      const input = toInput(draft)
      if (draft.id) {
        await updateWorkspace(draft.id, input)
      } else {
        await createWorkspace(input)
      }
      setDraft(null)
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  const remove = async (ws: Workspace) => {
    if (!window.confirm(`删除 workspace「${ws.name}」？绑定它的会话会变成未绑定状态。`)) return
    try {
      await deleteWorkspace(ws.id)
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err))
    }
  }

  /** 追加文件夹，按路径去重（同一个路径选两次不会出现两行）。 */
  const addFolders = (paths: string[]) => {
    setDraft((prev) => {
      if (!prev) return prev
      const merged = [...prev.folders]
      for (const p of paths) {
        if (p && !merged.includes(p)) merged.push(p)
      }
      return { ...prev, folders: merged }
    })
  }

  const removeFolder = (index: number) => {
    setDraft((prev) => {
      if (!prev) return prev
      return { ...prev, folders: prev.folders.filter((_, i) => i !== index) }
    })
  }

  const pickFolders = async () => {
    if (!window.fileDialog) return
    setPicking(true)
    try {
      const picked = await window.fileDialog.selectFolders({ allowMultiple: true })
      if (picked.length > 0) addFolders(picked)
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err))
    } finally {
      setPicking(false)
    }
  }

  const addManualPath = () => {
    const p = manualPath.trim()
    if (!p) return
    addFolders([p])
    setManualPath('')
  }

  const folderCount = draft?.folders.length ?? 0

  return (
    <div className="modal-scrim" onMouseDown={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="workspace 管理"
        onMouseDown={(e) => e.stopPropagation()}
        data-no-window-drag
      >
        <header className="modal-head">
          <h2 className="modal-title">workspace 管理</h2>
          <button className="icon-btn" type="button" onClick={onClose} title="关闭">
            ✕
          </button>
        </header>

        <div className="modal-body">
          {unsupported && (
            <div className="notice notice-warn">
              当前后端以内存存储运行，不支持保存 workspace。请用 <code>STORE_DRIVER=sqlite</code> 启动。
            </div>
          )}

          {error && (
            <div className="notice notice-error">
              <span>{error}</span>
              <button className="banner-btn" type="button" onClick={dismissError}>
                知道了
              </button>
            </div>
          )}

          <div className="notice">
            目前绑定 workspace 只是记录在会话上，<strong>还没有接入工具的实际文件访问权限判断</strong>
            （见 docs/14-workspace-and-session-presets.md）。
          </div>

          {!draft && (
            <>
              <div className="model-list">
                {loading && <div className="model-empty">加载中…</div>}
                {!loading && workspaces.length === 0 && (
                  <div className="model-empty">还没有 workspace，点下方「新增 workspace」开始。</div>
                )}

                {workspaces.map((w) => (
                  <div className="model-row" key={w.id}>
                    <div className="model-row-main">
                      <div className="model-row-title">
                        <span className="model-row-name">{w.name}</span>
                      </div>
                      <div className="model-row-sub">
                        {w.folders.length === 0
                          ? '没有绑定文件夹'
                          : w.folders.join(' · ')}
                      </div>
                    </div>

                    <div className="model-row-actions">
                      <button className="btn-ghost" type="button" onClick={() => setDraft(draftFrom(w))}>
                        编辑
                      </button>
                      <button className="btn-ghost is-danger" type="button" onClick={() => void remove(w)}>
                        删除
                      </button>
                    </div>
                  </div>
                ))}
              </div>

              <div className="modal-actions">
                <button className="btn-ghost" type="button" onClick={() => void refresh()}>
                  刷新
                </button>
                <button
                  className="btn-primary"
                  type="button"
                  onClick={() => {
                    setFormError(null)
                    setDraft(emptyDraft())
                  }}
                >
                  新增 workspace
                </button>
              </div>
            </>
          )}

          {draft && (
            <div className="model-form">
              <div className="field">
                <label className="field-label">名称</label>
                <input
                  className="text-input"
                  value={draft.name}
                  placeholder="例如：我的代码仓库"
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                />
                <div className="field-hint">用于在选择器里区分不同 workspace</div>
              </div>

              <div className="field">
                <label className="field-label">文件夹</label>

                <div className="folder-list">
                  {draft.folders.length === 0 && (
                    <div className="field-hint">还没有文件夹</div>
                  )}
                  {draft.folders.map((f, i) => (
                    <div className="folder-row" key={`${f}-${i}`}>
                      <span className="folder-path" title={f}>
                        {f}
                      </span>
                      <button
                        className="icon-btn"
                        type="button"
                        title="移除"
                        onClick={() => removeFolder(i)}
                      >
                        ✕
                      </button>
                    </div>
                  ))}
                </div>

                {hasFileDialog() ? (
                  <button
                    className="btn-ghost folder-add-btn"
                    type="button"
                    disabled={picking}
                    onClick={() => void pickFolders()}
                  >
                    <span className="plus">+</span> {picking ? '选择中…' : '选择文件夹…'}
                  </button>
                ) : (
                  <div className="folder-manual-add">
                    <input
                      className="text-input"
                      value={manualPath}
                      placeholder="/Users/you/code/project（绝对路径）"
                      onChange={(e) => setManualPath(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') {
                          e.preventDefault()
                          addManualPath()
                        }
                      }}
                    />
                    <button className="btn-ghost" type="button" onClick={addManualPath}>
                      添加
                    </button>
                  </div>
                )}

                <div className="field-hint">
                  必须是绝对路径；已添加 {folderCount} 个，至少需要 1 个才能保存
                  {!hasFileDialog() && '（非桌面端拿不到原生选择器，只能手动输入）'}
                </div>
              </div>

              {formError && <div className="notice notice-error">{formError}</div>}

              <div className="modal-actions">
                <button className="btn-ghost" type="button" onClick={() => setDraft(null)}>
                  取消
                </button>
                <button
                  className="btn-primary"
                  type="button"
                  disabled={saving || !draft.name.trim() || folderCount === 0}
                  onClick={() => void submit()}
                >
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
