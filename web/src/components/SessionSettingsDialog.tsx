import { useEffect, useState } from 'react'
import type { ApprovalMode, Session } from '../api/types'
import { useChat } from '../state/chatStore'
import { useModels } from '../state/modelsStore'
import { useWorkspaces } from '../state/workspacesStore'
import { useApprovalMode } from '../state/approvalModeStore'

interface SessionSettingsDialogProps {
  open: boolean
  onClose: () => void
  /**
   * 非空 = 编辑模式（修改这个已存在的会话）；
   * 为空/undefined = 创建模式（新建一个会话，提交即创建）。
   */
  session?: Session | null
}

const MODE_LABEL: Record<ApprovalMode, string> = {
  manual: '人工审批',
  model: '模型审批',
  auto: '自动放行',
}

/**
 * 会话设置弹窗：一次性绑定本会话的模型 / workspace / 审批模式覆盖。
 *
 * 创建与编辑共用一个组件而不是分成两个：字段完全一样，差异只在
 * "提交时是调用 createSession 还是分别调用几个 setActiveXxx"，
 * 拆两份会导致表单字段漂移（改一处忘了改另一处）。
 *
 * 用原生 <select> 而不是自绘的 ModelPicker/WorkspacePicker 那套下拉：
 * 那套下拉是 absolute 定位的浮层，嵌套在 modal 里容易有 z-index/overflow
 * 的麻烦，原生 select 在弹窗里最省心，这个场景也不需要花哨的展示。
 */
export function SessionSettingsDialog({ open, onClose, session }: SessionSettingsDialogProps) {
  const { createSession, setActiveModel, setActiveWorkspace, setActiveApprovalModeOverride } =
    useChat()
  const { models } = useModels()
  const { workspaces, unsupported: workspacesUnsupported } = useWorkspaces()
  const { mode: globalMode, options: approvalModeOptions, toolsEnabled: approvalToolsEnabled } =
    useApprovalMode()

  const editing = Boolean(session)

  const [title, setTitle] = useState('')
  const [modelConfigId, setModelConfigId] = useState('')
  const [workspaceId, setWorkspaceId] = useState('')
  const [approvalMode, setApprovalMode] = useState<ApprovalMode | ''>('')
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)

  // 打开时按模式重置草稿：编辑模式预填当前会话的值，创建模式全部置空。
  useEffect(() => {
    if (!open) return
    setFormError(null)
    setTitle(session?.title ?? '')
    setModelConfigId(session?.model_config_id ?? '')
    setWorkspaceId(session?.workspace_id ?? '')
    setApprovalMode(session?.approval_mode ?? '')
  }, [open, session])

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
    setSaving(true)
    setFormError(null)
    try {
      if (!editing) {
        await createSession({
          title: title.trim() || undefined,
          modelConfigId: modelConfigId || undefined,
          workspaceId: workspaceId || undefined,
          approvalMode: approvalMode || undefined,
        })
      } else {
        // 编辑模式：只对真正改动过的字段发请求，避免无意义的 PATCH。
        if (modelConfigId !== (session?.model_config_id ?? '')) {
          await setActiveModel(modelConfigId)
        }
        if (workspaceId !== (session?.workspace_id ?? '')) {
          await setActiveWorkspace(workspaceId)
        }
        if (approvalMode !== (session?.approval_mode ?? '')) {
          await setActiveApprovalModeOverride(approvalMode)
        }
      }
      onClose()
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="modal-scrim" onMouseDown={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="会话设置"
        onMouseDown={(e) => e.stopPropagation()}
        data-no-window-drag
      >
        <header className="modal-head">
          <h2 className="modal-title">{editing ? '会话设置' : '新建会话'}</h2>
          <button className="icon-btn" type="button" onClick={onClose} title="关闭">
            ✕
          </button>
        </header>

        <div className="modal-body">
          <div className="model-form">
            {!editing && (
              <div className="field">
                <label className="field-label">标题（可选）</label>
                <input
                  className="text-input"
                  value={title}
                  placeholder="留空则用首条消息自动生成"
                  onChange={(e) => setTitle(e.target.value)}
                />
              </div>
            )}

            <div className="field">
              <label className="field-label">模型</label>
              <select
                className="text-input"
                value={modelConfigId}
                onChange={(e) => setModelConfigId(e.target.value)}
              >
                <option value="">默认模型</option>
                {models.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name}
                    {m.is_default ? '（默认）' : ''}
                  </option>
                ))}
              </select>
            </div>

            <div className="field">
              <label className="field-label">Workspace</label>
              {workspacesUnsupported ? (
                <div className="field-hint">
                  当前后端以内存存储运行，不支持 workspace（需要 STORE_DRIVER=sqlite）。
                </div>
              ) : (
                <>
                  <select
                    className="text-input"
                    value={workspaceId}
                    onChange={(e) => setWorkspaceId(e.target.value)}
                  >
                    <option value="">不绑定</option>
                    {workspaces.map((w) => (
                      <option key={w.id} value={w.id}>
                        {w.name}（{w.folders.length} 个文件夹）
                      </option>
                    ))}
                  </select>
                  {workspaces.length === 0 && (
                    <div className="field-hint">还没有 workspace，可以先在顶栏 📁 里创建</div>
                  )}
                </>
              )}
            </div>

            {approvalToolsEnabled && (
              <div className="field">
                <label className="field-label">本会话的审批模式</label>
                <select
                  className="text-input"
                  value={approvalMode}
                  onChange={(e) => setApprovalMode(e.target.value as ApprovalMode | '')}
                >
                  <option value="">
                    跟随全局{globalMode ? `（当前：${MODE_LABEL[globalMode]}）` : ''}
                  </option>
                  {approvalModeOptions.map((m) => (
                    <option key={m} value={m}>
                      {MODE_LABEL[m]}
                    </option>
                  ))}
                </select>
                <div className="field-hint">
                  固定选某一档后，这个会话不再随全局审批开关的后续切换而改变
                </div>
              </div>
            )}

            {formError && <div className="notice notice-error">{formError}</div>}

            <div className="modal-actions">
              <button className="btn-ghost" type="button" onClick={onClose}>
                取消
              </button>
              <button className="btn-primary" type="button" disabled={saving} onClick={() => void submit()}>
                {saving ? '保存中…' : editing ? '保存' : '创建'}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
