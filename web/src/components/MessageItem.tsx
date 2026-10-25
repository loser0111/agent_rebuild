import { useMemo, useState } from 'react'
import type { Message } from '../api/types'
import { Markdown } from './Markdown'
import { useChat } from '../state/chatStore'
import { ToolCallList } from './ToolCallCard'

/** 单条消息：用户气泡 / 助手气泡（Markdown + 工具调用 + 复制 + 状态标记）。 */
export function MessageItem({ message }: { message: Message }) {
  const { streamingMessageId, toolCalls } = useChat()
  const isUser = message.role === 'user'
  const streaming = streamingMessageId === message.id
  const [copied, setCopied] = useState(false)

  // 只取归属当前这条消息的工具调用，保持到达顺序。
  const calls = useMemo(
    () => Object.values(toolCalls).filter((c) => c.message_id === message.id),
    [toolCalls, message.id],
  )

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

  return (
    <div className="msg msg-assistant">
      <div className="msg-role">
        <span className="avatar">◆</span>
        <span>Agent</span>
        {message.status === 'interrupted' && <span className="tag-warn">已中断</span>}
        {message.status === 'error' && <span className="tag-error">失败</span>}
        {streaming && <span className="tag-running">生成中</span>}
      </div>

      <div className="msg-body">
        {/* 工具调用在文字之前：模型先调工具、拿到结果后才开始组织回答 */}
        {calls.length > 0 && <ToolCallList calls={calls} />}

        {empty && streaming ? (
          <div className="typing">
            <span />
            <span />
            <span />
          </div>
        ) : empty && message.status === 'error' ? (
          <div className="msg-error">{message.error?.message ?? '生成失败'}</div>
        ) : empty && calls.length > 0 ? null : (
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
