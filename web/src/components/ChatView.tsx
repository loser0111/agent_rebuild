import { useEffect, useRef, useState } from 'react'
import { useChat } from '../state/chatStore'
import { MessageItem } from './MessageItem'

/** 主对话区：消息流 + 自动滚动 + 空态。 */
export function ChatView() {
  const { messages, currentId, detailLoading, reconnectHint, resync, isStreaming } = useChat()
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

        {messages.map((m) => (
          <MessageItem key={m.id} message={m} />
        ))}

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
