import { useEffect, useRef, useState } from 'react'
import { useChat } from '../state/chatStore'
import { MessageItem } from './MessageItem'

/** 主对话区：消息流 + 自动滚动 + 空态。 */
export function ChatView() {
  const { messages, currentId, detailLoading, reconnectHint, resync, isStreaming, stopGeneration, toolCalls } =
    useChat()
  const scrollRef = useRef<HTMLDivElement>(null)
  const [stuckToBottom, setStuckToBottom] = useState(true)

  // 用户向上滚动时不强制拉回底部（避免阅读历史被打断）。
  const onScroll = () => {
    const el = scrollRef.current
    if (!el) return
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight
    setStuckToBottom(distance < 80)
  }

  // 新内容到达时，仅在「已贴底」状态下滚动。
  useEffect(() => {
    const el = scrollRef.current
    if (!el || !stuckToBottom) return
    el.scrollTop = el.scrollHeight
  }, [messages, stuckToBottom])

  useEffect(() => {
    setStuckToBottom(true)
  }, [currentId])

  if (!currentId) {
    return (
      <div className="chat-scroll" data-no-window-drag>
        <div className="empty-state">
          <div className="empty-mark">◆</div>
          <h1>开始一段新对话</h1>
          <p>在左侧新建会话，或直接选择已有会话继续。</p>
          <div className="empty-kbd">
            <kbd>Enter</kbd> 发送 <span className="sep">·</span> <kbd>Shift</kbd>+
            <kbd>Enter</kbd> 换行
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="chat-scroll" ref={scrollRef} onScroll={onScroll} data-no-window-drag>
      <div className="chat-inner">
        {reconnectHint && (
          <div className="banner banner-warn">
            <span>{reconnectHint}</span>
            <button className="banner-btn" type="button" onClick={() => void resync()}>
              重新同步
            </button>
            {currentId && (
              <button
                className="banner-btn"
                type="button"
                title="若会话卡在「生成中」，点此复位状态"
                onClick={() => void stopGeneration()}
              >
                重置状态
              </button>
            )}
          </div>
        )}

        {detailLoading && messages.length === 0 && (
          <div className="chat-loading">正在加载会话…</div>
        )}

        {!detailLoading && messages.length === 0 && (
          <div className="empty-state compact">
            <h2>这个会话还没有消息</h2>
            <p>在下方输入框提问，回答会以流式逐字呈现。</p>
          </div>
        )}

        {messages.map((m) => {
          // tool 结果已展示在所属的卡片里，不再单独渲染成一条消息，
          // 否则内容会重复出现一遍。
          if (m.role === 'tool') return null

          // 「只带 tool_calls、content 为空」的 assistant 消息：
          // 如果卡片挂在它身上（工具调用后没有最终回答）就必须渲染，
          // 否则卡片会整个消失；挂在后续回答上时才跳过。
          if (m.role === 'assistant' && !m.content && m.tool_calls?.length) {
            const hosted = Object.values(toolCalls).some((c) => c.message_id === m.id)
            if (!hosted) return null
          }

          return <MessageItem key={m.id} message={m} />
        })}

        {isStreaming && <div className="chat-tail-spacer" />}
      </div>

      {!stuckToBottom && (
        <button
          className="jump-bottom"
          type="button"
          onClick={() => {
            const el = scrollRef.current
            if (el) el.scrollTop = el.scrollHeight
            setStuckToBottom(true)
          }}
        >
          ↓ 跳到最新
        </button>
      )}
    </div>
  )
}
