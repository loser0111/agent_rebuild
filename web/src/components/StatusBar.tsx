import { useEffect, useState } from 'react'
import { useChat } from '../state/chatStore'

/** 底部状态栏：连接状态、provider/model、流式阶段与耗时。 */
export function StatusBar() {
  const { backendOnline, health, phase, isStreaming, startedAt, lastEventId, messages, currentId } =
    useChat()
  const [elapsed, setElapsed] = useState(0)

  // 生成中每秒刷新计时。
  useEffect(() => {
    if (!startedAt) {
      setElapsed(0)
      return
    }
    const id = window.setInterval(() => {
      setElapsed(Math.floor((Date.now() - startedAt) / 1000))
    }, 500)
    return () => window.clearInterval(id)
  }, [startedAt])

  const statusText = (() => {
    if (!backendOnline) return '后端未连接'
    switch (phase) {
      case 'connecting':
        return '正在连接…'
      case 'streaming':
        return '流式接收中'
      case 'reconnecting':
        return '连接中断，等待重同步'
      case 'error':
        return '发生错误'
      default:
        return '就绪'
    }
  })()

  const statusClass = !backendOnline
    ? 'is-offline'
    : phase === 'error'
      ? 'is-error'
      : phase === 'reconnecting'
        ? 'is-warn'
        : isStreaming
          ? 'is-busy'
          : 'is-ok'

  const assistantCount = messages.filter((m) => m.role === 'assistant').length

  return (
    <footer className="statusbar">
      <div className="status-left">
        <span className={`status-dot ${statusClass}`} />
        <span className="status-text">{statusText}</span>
        {isStreaming && elapsed > 0 && <span className="status-meta">{elapsed}s</span>}
      </div>

      <div className="status-right">
        {health && (
          <span className="status-meta" title="当前 provider / 模型">
            {health.provider} · {health.model}
          </span>
        )}
        {currentId && (
          <span className="status-meta" title="本会话事件序号 / 消息数">
            seq {lastEventId} · {assistantCount} 轮
          </span>
        )}
        <span className="status-meta">v{health?.version ?? '0.1.0'}</span>
      </div>
    </footer>
  )
}
