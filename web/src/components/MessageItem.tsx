import { useMemo, useState } from 'react'
import type { Message, ToolCallView } from '../api/types'
import { Markdown } from './Markdown'
import { useChat } from '../state/chatStore'
import { ToolCallSummary } from './ToolCallCard'

/** 单条消息：用户气泡 / 助手气泡（Markdown + 工具调用 + 复制 + 状态标记）。 */
export function MessageItem({
  message,
  calls,
}: {
  message: Message
  /** 归属本轮的调用（由 ChatView 按轮次聚合后传入，可能跨多条占位消息）。 */
  calls?: ToolCallView[]
}) {
  const { streamingMessageId, toolCalls, pendingQuestions } = useChat()
  const isUser = message.role === 'user'
  const streaming = streamingMessageId === message.id
  const [copied, setCopied] = useState(false)

  // 兜底：调用方没传时按 message_id 自行过滤。
  //
  // 但这条路径只适合「一轮只有一条 assistant」的简单场景；
  // 挂起→恢复会产生两条占位消息，必须靠 ChatView 传聚合结果，
  // 否则同一轮会被拆成两个气泡。
  const ownCalls = useMemo(() => {
    if (calls) return calls
    return Object.values(toolCalls).filter((c) => c.message_id === message.id)
  }, [calls, toolCalls, message.id])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(message.content)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      /* 忽略：部分环境不支持 clipboard */
    }
  }

  if (isUser) {
    return (
      <div className="msg msg-user">
        <div className="msg-body user-bubble">
          <div className="user-text">{message.content}</div>
        </div>
        <div className="msg-actions">
          <button className="act" type="button" onClick={copy} title="复制">
            {copied ? '已复制' : '复制'}
          </button>
        </div>
      </div>
    )
  }

  const empty = !message.content
  // 这一轮是否在等用户回答（ask_user 挂起）。
  // 只认「当前挂起」这一条，历史里已答过的轮次不再提示。
  const awaiting = pendingQuestions.length > 0 && streaming

  return (
    <div className="msg msg-assistant">
      <div className="msg-role">
        <span className="avatar">◆</span>
        <span>Agent</span>
        {message.status === 'interrupted' && <span className="tag-warn">已中断</span>}
        {message.status === 'error' && <span className="tag-error">失败</span>}
        {awaiting && <span className="tag-awaiting">等待回答</span>}
        {streaming && !awaiting && <span className="tag-running">生成中</span>}
        {/* 工具调用折叠成一行摘要，挂在 Agent 标签右侧 */}
        {ownCalls.length > 0 && <ToolCallSummary calls={ownCalls} />}
      </div>

      <div className="msg-body">
        {empty && streaming && awaiting ? (
          <div className="tool-pending">（已向用户提问，等待回答后继续）</div>
        ) : empty && streaming ? (
          <div className="typing">
            <span />
            <span />
            <span />
          </div>
        ) : empty && message.status === 'error' ? (
          <div className="msg-error">{message.error?.message ?? '生成失败'}</div>
        ) : empty && ownCalls.length > 0 ? (
          <div className="tool-pending">
            {message.status === 'interrupted' ? '（本轮被中断，未产生文字回答）' : '（本轮未产生文字回答）'}
          </div>
        ) : (
          <>
            <Markdown text={message.content} streaming={streaming} />
            {streaming && <span className="caret" />}
          </>
        )}

        {message.status === 'error' && message.error && message.content && (
          <div className="msg-error">{message.error.message}</div>
        )}
      </div>

      {!empty && (
        <div className="msg-actions">
          <button className="act" type="button" onClick={copy} title="复制整条回答">
            {copied ? '已复制' : '复制'}
          </button>
          {message.usage && (
            <span className="usage" title="token 用量">
              {message.usage.total_tokens} tokens
            </span>
          )}
        </div>
      )}
    </div>
  )
}
