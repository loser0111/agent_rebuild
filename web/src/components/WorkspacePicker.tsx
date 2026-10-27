import { useEffect, useRef, useState } from 'react'
import type { Workspace } from '../api/types'

interface WorkspacePickerProps {
  workspaces: Workspace[]
  /** 当前会话绑定的 workspace id；空串表示未绑定 */
  value: string
  disabled?: boolean
  align?: 'left' | 'right'
  title?: string
  onChange: (workspaceId: string) => void
  onOpenManager?: () => void
}

/** workspace 右侧的次要标识：绑了几个文件夹。 */
function folderLabel(w: Workspace): string {
  const n = w.folders?.length ?? 0
  return n === 1 ? '1 个文件夹' : `${n} 个文件夹`
}

/**
 * workspace 选择下拉，样式与交互完全照抄 ModelPicker——
 * 两者都是「会话级、可随时切换、未选则用某个默认值」的同一类控件。
 *
 * 注意：目前选中某个 workspace 只是把 workspace_id 记到会话上，
 * **不会**改变任何工具调用的实际文件访问范围（见
 * docs/14-workspace-and-session-presets.md 第 6 节已知限制）。
 */
export function WorkspacePicker({
  workspaces,
  value,
  disabled,
  align = 'left',
  title,
  onChange,
  onOpenManager,
}: WorkspacePickerProps) {
  const [open, setOpen] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  const current = workspaces.find((w) => w.id === value)
  const label = current ? current.name : '未绑定 workspace'

  useEffect(() => {
    if (!open) return
    const onDocDown = (e: MouseEvent) => {
      if (!boxRef.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDocDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div className="model-picker" ref={boxRef} data-no-window-drag>
      <button
        className={`model-picker-btn${open ? ' is-open' : ''}`}
        type="button"
        disabled={disabled}
        title={title ?? '选择本次会话绑定的 workspace'}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="model-dot" />
        <span className="model-picker-label">{label}</span>
        <span className="model-picker-caret">▾</span>
      </button>

      {open && (
        <div className={`model-menu${align === 'right' ? ' is-right' : ''}`}>
          <div className="model-menu-head">选择 workspace</div>

          {value !== '' && (
            <button
              className="model-menu-item"
              type="button"
              onClick={() => {
                onChange('')
                setOpen(false)
              }}
            >
              <span className="model-menu-row">
                <span className="model-menu-name">不绑定</span>
              </span>
            </button>
          )}

          {workspaces.length === 0 && (
            <div className="model-menu-empty">
              还没有 workspace
              {onOpenManager && (
                <button
                  className="model-menu-link"
                  type="button"
                  onClick={() => {
                    setOpen(false)
                    onOpenManager()
                  }}
                >
                  去添加
                </button>
              )}
            </div>
          )}

          {workspaces.map((w) => (
            <button
              key={w.id}
              className={`model-menu-item${w.id === value ? ' is-active' : ''}`}
              type="button"
              onClick={() => {
                onChange(w.id)
                setOpen(false)
              }}
            >
              <span className="model-menu-row">
                <span className="model-menu-name">{w.name}</span>
              </span>
              <span className="model-menu-sub">{folderLabel(w)}</span>
            </button>
          ))}

          {onOpenManager && (
            <>
              <div className="model-menu-sep" />
              <button
                className="model-menu-item is-action"
                type="button"
                onClick={() => {
                  setOpen(false)
                  onOpenManager()
                }}
              >
                <span className="model-menu-row">
                  <span className="model-menu-name">管理 workspace…</span>
                </span>
              </button>
            </>
          )}
        </div>
      )}
    </div>
  )
}
