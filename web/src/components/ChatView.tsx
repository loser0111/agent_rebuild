import { useEffect, useMemo, useRef, useState } from 'react'
import { useChat } from '../state/chatStore'
import { groupCallsByHost, hostMap } from '../state/toolRestore'
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

  /**
   * 按轮次把工具调用聚合到宿主消息上。
   *
   * 为什么不能在 MessageItem 里各自过滤（按 message_id）：
   * 挂起→恢复会新建一条 assistant 占位消息，同一轮因此有两条：
   *
   *   占位 A（挂起那轮）→ ask_user 卡片
   *   占位 B（resume 后）→ read_file/search 卡片 + 最终回答
   *
   * 各按自己的 id 渲染就会出现两个「Agent」气泡，把同一轮劈成两半
   * （截图里的现象）。而刷新后走的 rebuildToolCalls 是按轮次聚合的，
   * 只显示一个气泡——实时与刷新必须一致。
   */
  const callsByHost = useMemo(
    () => groupCallsByHost(messages, Object.values(toolCalls)),
    [messages, toolCalls],
  )

  /** 汇总到某条消息名下的工具调用（可能来自同轮的其他占位消息）。 */
  const callsFor = (messageId: string) => callsByHost[messageId] ?? []

  /** 消息 → 本轮宿主消息 id（用于跳过同轮多余的占位气泡）。 */
  const hosts = useMemo(() => hostMap(messages), [messages])
  const hostOf = (messageId: string) => hosts[messageId] || messageId

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

          // 同一轮里的「非宿主」assistant 消息不再单独渲染。
          //
          // 典型场景：挂起时占位 A 承载 ask_user 卡片，resume 后新建的
          // 占位 B 承载后续工具调用与最终回答。A 与 B 属于同一轮
          // （中间没有新的 user 消息），必须合并成一个 Agent 气泡，
          // 否则界面上会出现两个「Agent」标题，把一轮对话劈成两半。
          if (m.role === 'assistant') {
            const hostId = hostOf(m.id)
            if (hostId && hostId !== m.id) return null
          }

          return <MessageItem key={m.id} message={m} calls={callsFor(m.id)} />
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
