import { useEffect, useMemo, useRef, useState } from 'react'
import { useChat } from '../state/chatStore'
import { useModels } from '../state/modelsStore'
import { ModelPicker } from './ModelPicker'
import { AskUserCard } from './AskUserCard'

interface ComposerProps {
  onOpenModelManager: () => void
}

/**
 * 输入区：Enter 发送 / Shift+Enter 换行 / 生成中显示停止按钮。
 *
 * 有挂起提问时，这个输入框同时也是「答题框」：
 * 用户输入的内容按顺序填入第一个未回答的问题，全部答完即提交。
 *
 * 为什么这样做：挂起时把输入框整个禁用、逼用户去卡片里的小输入区打字，
 * 是很别扭的——用户看到问题后第一反应就是在最显眼的输入框里回答。
 * 而且一次只能答一个问题时，多个问题要来回点，体验很差。
 */
export function Composer({ onOpenModelManager }: ComposerProps) {
  const {
    sendMessage,
    stopGeneration,
    isStreaming,
    currentId,
    phase,
    activeModelConfigId,
    setActiveModel,
    pendingQuestions,
    draftAnswers,
    setDraftAnswer,
    resetDrafts,
    resume,
  } = useChat()
  const { models } = useModels()
  const [value, setValue] = useState('')
  const taRef = useRef<HTMLTextAreaElement>(null)

  // 有挂起提问时，输入框进入「答题模式」。
  const awaitingAnswer = pendingQuestions.length > 0

  /**
   * 当前要回答的问题是「第一个还没答的」。
   *
   * 这样用户连续输入多次就能逐个答完所有问题：
   * 输入 → Enter → 该问题的答案记下 → 自动切到下一题。
   *
   * 用 q.key 而不是 q.callId：一次 ask_user 调用可含多个问题，
   * 它们共享 call_id，只有 key 能区分。
   */
  const activeQuestion = useMemo(() => {
    if (!awaitingAnswer) return null
    return pendingQuestions.find((q) => (draftAnswers[q.key] ?? '').trim() === '') ?? null
  }, [awaitingAnswer, pendingQuestions, draftAnswers])

  const allAnswered =
    awaitingAnswer && pendingQuestions.every((q) => (draftAnswers[q.key] ?? '').trim() !== '')

  // 生成中禁用；等待回答时**不禁用**（否则没法答题）。
  const disabled = !currentId || isStreaming
  const canSend = awaitingAnswer
    ? allAnswered || value.trim().length > 0
    : value.trim().length > 0 && !disabled

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

  // 问题集合变化时清空输入，避免上一题的残留串到下一题。
  const signature = pendingQuestions.map((q) => q.key).join(',')
  useEffect(() => {
    setValue('')
  }, [signature])

  /**
   * 提交：分「答题」与「正常发消息」两条路径。
   */
  const submit = async () => {
    if (!currentId || isStreaming) return

    if (awaitingAnswer) {
      const text = value.trim()

      // 把输入内容记到当前问题上。
      const merged = { ...draftAnswers }
      if (activeQuestion && text) {
        merged[activeQuestion.key] = text
        setDraftAnswer(activeQuestion.key, text)
      }

      const stillMissing = pendingQuestions.some((q) => (merged[q.key] ?? '').trim() === '')
      if (stillMissing) {
        // 还有问题没答：只记下这一条，把输入框清空准备答下一题。
        setValue('')
        return
      }

      // 全部答完 → 一次性提交。
      const payload: Record<string, string> = {}
      for (const q of pendingQuestions) {
        payload[q.key] = (merged[q.key] ?? '').trim()
      }
      setValue('')
      resetDrafts()
      await resume(payload)
      return
    }

    const text = value.trim()
    if (!text) return
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

  const total = pendingQuestions.length
  const remaining = pendingQuestions.filter((q) => (draftAnswers[q.key] ?? '').trim() === '').length
  const placeholder = !currentId
    ? '请先在左侧选择或新建会话'
    : awaitingAnswer
      ? activeQuestion
        ? total > 1
          ? `回答第 ${total - remaining + 1} 个问题（共 ${total} 个），Enter 记录`
          : '输入你的回答，Enter 提交'
        : '还有问题未回答，请在上方卡片中补充'
      : '输入消息，Enter 发送，Shift+Enter 换行…'

  return (
    <div className="composer" data-no-window-drag>
      {/* 有提问时，问题卡片顶在输入框上方 */}
      <AskUserCard />

      <div className={`composer-box${disabled ? ' is-disabled' : ''}`}>
        <textarea
          ref={taRef}
          className="composer-input"
          value={value}
          rows={1}
          placeholder={placeholder}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={onKeyDown}
          disabled={disabled}
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
              {isStreaming
                ? '生成中…'
                : awaitingAnswer
                  ? allAnswered
                    ? 'Enter 提交全部答案'
                    : `逐题回答，还剩 ${remaining} 个`
                  : `${value.length} 字`}
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
              title={awaitingAnswer ? (allAnswered ? '提交全部答案' : '记录这条回答') : '发送（Enter）'}
            >
              {awaitingAnswer
                ? allAnswered
                  ? '提交答案'
                  : '记录'
                : '发送'}{' '}
              <span className="kbd-inline">⏎</span>
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
