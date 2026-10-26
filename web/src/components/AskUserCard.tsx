import { useEffect, useRef } from 'react'
import { useChat } from '../state/chatStore'

/**
 * 问答卡片：模型用 ask_user 提问后，在输入区上方显示问题。
 *
 * 为什么放在输入区而不是消息流里：
 *  — 提问是「必须回答才能继续」的阻塞项，放消息流里容易被滚出视野；
 *  — 用户的实际动作就是「回答」，放在输入框正上方符合操作路径。
 *
 * 答案状态放在 store（draftAnswers）而不是本组件：
 * 同一份答案有两个入口——卡片里的选项/文本框，以及底部主输入框。
 * 放在组件内部时 Composer 读不到，用户就没法用最顺手的输入框答题。
 *
 * 答案以 pending.key 为键（call_id 或 call_id::qid）：
 * 一次 ask_user 调用可含多个问题，它们共享 call_id，
 * 只有 key 能把答案对应回具体问题。
 */
export function AskUserCard() {
  const { pendingQuestions, draftAnswers, isStreaming, resume, setDraftAnswer } = useChat()
  const rootRef = useRef<HTMLDivElement>(null)

  // 出现新问题时聚焦第一个可输入控件。
  const signature = pendingQuestions.map((q) => q.key).join(',')
  useEffect(() => {
    const el = rootRef.current?.querySelector<HTMLElement>('input, textarea')
    el?.focus()
  }, [signature])

  if (pendingQuestions.length === 0) return null

  const multi = pendingQuestions.length > 1

  const isAnswered = (key: string) => (draftAnswers[key] ?? '').trim().length > 0
  const allAnswered = pendingQuestions.every((q) => isAnswered(q.key))

  const submit = async () => {
    if (!allAnswered || isStreaming) return
    const payload: Record<string, string> = {}
    for (const q of pendingQuestions) {
      payload[q.key] = (draftAnswers[q.key] ?? '').trim()
    }
    await resume(payload)
  }

  return (
    <div className="ask-card" data-no-window-drag ref={rootRef}>
      <div className="ask-head">
        <span className="ask-badge">
          {multi ? `需要你的回答（${pendingQuestions.length} 个问题）` : '需要你的回答'}
        </span>
        <span className="ask-hint">
          {multi
            ? '可以在这里选，也可以直接在下方输入框逐个回答'
            : '可以直接在下方输入框回答，或选择下面的选项'}
        </span>
      </div>

      {pendingQuestions.map((q, idx) => {
        const hasOptions = q.options.length > 0
        const current = draftAnswers[q.key] ?? ''
        return (
          <div className="ask-item" key={q.key}>
            {multi && (
              <div className={`ask-index${isAnswered(q.key) ? ' is-done' : ''}`}>
                {isAnswered(q.key) ? '✓' : idx + 1}
              </div>
            )}
            <div className="ask-question">{q.question}</div>

            {hasOptions ? (
              <div className="ask-options">
                {q.options.map((opt) => (
                  <button
                    key={opt}
                    type="button"
                    className={`ask-option${current === opt ? ' is-selected' : ''}`}
                    onClick={() => setDraftAnswer(q.key, opt)}
                    disabled={isStreaming}
                  >
                    {opt}
                  </button>
                ))}
                {/* 选项之外允许自由补充：模型给的两个选项经常都不完全合适 */}
                <input
                  className="ask-option-input"
                  type="text"
                  value={q.options.includes(current) ? '' : current}
                  placeholder="或直接输入…"
                  onChange={(e) => setDraftAnswer(q.key, e.target.value)}
                  onKeyDown={(e) => {
                    if (
                      !multi &&
                      e.key === 'Enter' &&
                      !e.shiftKey &&
                      !e.nativeEvent.isComposing
                    ) {
                      e.preventDefault()
                      void submit()
                    }
                  }}
                  disabled={isStreaming}
                />
              </div>
            ) : (
              <textarea
                className="ask-input"
                value={current}
                rows={2}
                placeholder="输入你的回答，也可以在下方主输入框里回答"
                onChange={(e) => setDraftAnswer(q.key, e.target.value)}
                onKeyDown={(e) => {
                  // 多问题时 Enter 换行更安全：避免答到一半就被提交。
                  if (!multi && e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault()
                    void submit()
                  }
                }}
                disabled={isStreaming}
                spellCheck={false}
              />
            )}
          </div>
        )
      })}

      <div className="ask-actions">
        {multi && !allAnswered && (
          <span className="ask-remaining">
            还有 {pendingQuestions.filter((q) => !isAnswered(q.key)).length} 个问题未回答
          </span>
        )}
        <button
          className="ask-submit"
          type="button"
          onClick={() => void submit()}
          disabled={!allAnswered || isStreaming}
        >
          {isStreaming ? '提交中…' : '提交并继续'}
        </button>
      </div>
    </div>
  )
}
