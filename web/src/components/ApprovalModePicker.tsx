import { useEffect, useRef, useState } from 'react'
import type { ApprovalMode } from '../api/types'

interface ApprovalModePickerProps {
  value: ApprovalMode | ''
  options: ApprovalMode[]
  disabled?: boolean
  switching?: boolean
  align?: 'left' | 'right'
  onChange: (mode: ApprovalMode) => void
}

/** 模式 → 中文标签 + 一句话说明（下拉里展示，帮用户理解三档差异）。 */
const MODE_INFO: Record<ApprovalMode, { label: string; desc: string }> = {
  manual: { label: '人工审批', desc: '写操作挂起，等你点击批准/拒绝（最安全）' },
  model: { label: '模型审批', desc: '由一次独立模型调用当场裁决，不打断对话' },
  auto: { label: '自动放行', desc: '不做任何审批，按风险开关直接执行（谨慎使用）' },
}

/**
 * 审批模式切换下拉。
 *
 * 自绘而非用 <select>：同 ModelPicker——需要在每个选项下方附一句说明，
 * 原生 select 放不下，也不符合深色观感。
 *
 * 注意这个控件改的是**整个进程**的审批策略，不是某个会话的设置
 * （与旁边的 ModelPicker 语义不同），所以按钮上带一个醒目的风险色点，
 * 提示这是一个会影响后续所有工具调用的全局开关。
 */
export function ApprovalModePicker({
  value,
  options,
  disabled,
  switching,
  align = 'left',
  onChange,
}: ApprovalModePickerProps) {
  const [open, setOpen] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  const info = value ? MODE_INFO[value] : null
  const label = switching ? '切换中…' : info ? info.label : '未启用工具'

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
    <div className="model-picker approval-mode-picker" ref={boxRef} data-no-window-drag>
      <button
        className={`model-picker-btn${open ? ' is-open' : ''}`}
        type="button"
        disabled={disabled || switching || !value}
        title="切换审批模式（影响后续全部工具调用，立即生效）"
        onClick={() => setOpen((v) => !v)}
      >
        <span
          className={`model-dot${value === 'auto' ? ' is-danger' : value === 'model' ? ' is-warn' : ''}`}
        />
        <span className="model-picker-label">审批：{label}</span>
        <span className="model-picker-caret">▾</span>
      </button>

      {open && (
        <div className={`model-menu${align === 'right' ? ' is-right' : ''}`}>
          <div className="model-menu-head">切换审批模式</div>

          {options.map((m) => {
            const mi = MODE_INFO[m]
            return (
              <button
                key={m}
                className={`model-menu-item${m === value ? ' is-active' : ''}`}
                type="button"
                onClick={() => {
                  setOpen(false)
                  if (m !== value) onChange(m)
                }}
              >
                <span className="model-menu-row">
                  <span className="model-menu-name">{mi.label}</span>
                  {m === value && <span className="model-badge">当前</span>}
                </span>
                <span className="model-menu-sub">{mi.desc}</span>
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
