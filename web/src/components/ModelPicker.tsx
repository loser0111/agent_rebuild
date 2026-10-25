import { useEffect, useRef, useState } from 'react'
import type { ModelConfig } from '../api/types'

interface ModelPickerProps {
  models: ModelConfig[]
  /** 当前选中的配置 id；空串表示未选择（用全局默认） */
  value: string
  /** 未选择时展示的文案 */
  fallbackLabel?: string
  disabled?: boolean
  /** 下拉里是否显示「默认」标记 */
  align?: 'left' | 'right'
  title?: string
  onChange: (modelConfigId: string) => void
  onOpenManager?: () => void
}

/** 协议 → 短标签，用于在选项右侧做次要标识。 */
function protocolLabel(m: ModelConfig): string {
  if (m.protocol === 'mock') return 'mock'
  return m.model || m.protocol
}

/**
 * 模型选择下拉。
 *
 * 自绘而非用 <select>：需要显示「名称 + 模型名 + 默认标记 + 设置入口」，
 * 原生 select 无法承载富内容，也不符合 Codex 的深色观感。
 */
export function ModelPicker({
  models,
  value,
  fallbackLabel = '默认模型',
  disabled,
  align = 'left',
  title,
  onChange,
  onOpenManager,
}: ModelPickerProps) {
  const [open, setOpen] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  const current = models.find((m) => m.id === value)
  const label = current ? current.name : fallbackLabel

  // 点击外部关闭。
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
        title={title ?? '选择模型'}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="model-dot" data-default={current?.is_default ? '1' : undefined} />
        <span className="model-picker-label">{label}</span>
        <span className="model-picker-caret">▾</span>
      </button>

      {open && (
        <div className={`model-menu${align === 'right' ? ' is-right' : ''}`}>
          <div className="model-menu-head">选择模型</div>

          {models.length === 0 && (
            <div className="model-menu-empty">
              还没有配置模型
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

          {models.map((m) => (
            <button
              key={m.id}
              className={`model-menu-item${m.id === value ? ' is-active' : ''}`}
              type="button"
              onClick={() => {
                onChange(m.id)
                setOpen(false)
              }}
            >
              <span className="model-menu-row">
                <span className="model-menu-name">{m.name}</span>
                {m.is_default && <span className="model-badge">默认</span>}
              </span>
              <span className="model-menu-sub">{protocolLabel(m)}</span>
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
                  <span className="model-menu-name">配置模型…</span>
                </span>
              </button>
            </>
          )}
        </div>
      )}
    </div>
  )
}
