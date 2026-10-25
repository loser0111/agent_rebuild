import { useEffect, useRef, useState } from 'react'
import { useChat } from '../state/chatStore'
import { useModels } from '../state/modelsStore'
import { ModelPicker } from './ModelPicker'

interface ComposerProps {
  onOpenModelManager: () => void
}

/** 输入区：Enter 发送 / Shift+Enter 换行 / 生成中显示停止按钮。 */
export function Composer({ onOpenModelManager }: ComposerProps) {
  const { sendMessage, stopGeneration, isStreaming, currentId, phase, activeModelConfigId, setActiveModel } =
    useChat()
  const { models } = useModels()
  const [value, setValue] = useState('')
  const taRef = useRef<HTMLTextAreaElement>(null)

  const disabled = !currentId || isStreaming
  const canSend = value.trim().length > 0 && !disabled

  // 自适应高度：随内容增长，最多 220px。
  useEffect(() => {
    const ta = taRef.current
    if (!ta) return
    ta.style.height = 'auto'
    ta.style.height = `${Math.min(ta.scrollHeight, 220)}px`
  }, [value])

  // 会话切换后聚焦输入框。
  useEffect(() => {
    if (currentId) taRef.current?.focus()
  }, [currentId])

  const submit = async () => {
    const text = value.trim()
    if (!text || disabled) return
    setValue('')
    await sendMessage(text)
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Enter 发送，Shift+Enter 换行；输入法组合中不拦截。
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      void submit()
    }
  }

  return (
    <div className="composer" data-no-window-drag>
      <div className={`composer-box${disabled ? ' is-disabled' : ''}`}>
        <textarea
          ref={taRef}
          className="composer-input"
          value={value}
          rows={1}
          placeholder={
            currentId ? '输入消息，Enter 发送，Shift+Enter 换行…' : '请先在左侧选择或新建会话'
          }
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={onKeyDown}
          disabled={!currentId}
          spellCheck={false}
        />

        <div className="composer-actions">
          {/* 输入框下方的模型切换器：随时可换，无需离开输入区。 */}
          <div className="composer-left">
            <ModelPicker
              models={models}
              value={activeModelConfigId}
              fallbackLabel="默认模型"
              disabled={!currentId || isStreaming}
              title={
                isStreaming ? '生成中不能切换模型' : '切换本次会话使用的模型'
              }
              onChange={(id) => void setActiveModel(id)}
              onOpenManager={onOpenModelManager}
            />
            <span className="composer-hint">
              {isStreaming ? '生成中…' : `${value.length} 字`}
            </span>
          </div>

          {isStreaming ? (
            <button className="btn-stop" type="button" onClick={() => void stopGeneration()}>
              <span className="stop-icon" /> 停止
            </button>
          ) : (
            <button
              className="btn-send"
              type="button"
              onClick={() => void submit()}
              disabled={!canSend}
              title="发送（Enter）"
            >
              发送 <span className="kbd-inline">⏎</span>
            </button>
          )}
        </div>
      </div>

      {phase === 'reconnecting' && (
        <div className="composer-note">连接已断开，正在等待重新同步…</div>
      )}
    </div>
  )
}
