import { useEffect, useState } from 'react'
import { useChat } from '../state/chatStore'
import { findDuplicateExecutedCall } from '../state/toolRestore'
import { parseProxyCall } from '../lib/proxyCall'

/** 参数 JSON 美化；解析失败原样显示（模型可能产出不完整的 JSON）。 */
function prettyArgs(raw: string | undefined): string {
  const text = (raw ?? '').trim()
  if (!text) return '(无参数)'
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

/** 重复调用提示文案：status 不同措辞不同，让用户一眼知道"之前那次结果如何"。 */
function duplicateHintText(status: 'running' | 'success' | 'error' | 'rejected'): string {
  switch (status) {
    case 'success':
      return '提示：这条操作与之前一次已成功执行的调用参数完全相同，可能是模型重复发起——批准前请确认是否真的需要再执行一次。'
    case 'running':
      return '提示：这条操作与之前一次仍在执行中的调用参数完全相同，可能是模型重复发起。'
    case 'rejected':
      return '提示：这条操作与之前一次被拒绝的调用参数完全相同，模型可能又发起了一次同样的请求。'
    default:
      return '提示：这条操作与之前一次执行失败的调用参数完全相同，模型可能又发起了一次同样的请求。'
  }
}

/**
 * 审批卡片：人工审批模式下，模型想执行写操作（write_file / run_command 等）
 * 时在这里展示，用户逐条点「批准」或「拒绝」。
 *
 * 为什么是逐条按钮而不是像 AskUserCard 那样攒够了再一次性提交：
 * 审批决定之间没有互相依赖的前后关系（不像问答可能需要先看到第一题的
 * 上下文才能回答第二题），没必要强迫用户等到"全部想好了"才能行动——
 * 看清楚一条就能当场决定，不想看另一条也可以先放着。
 *
 * 但提交仍然走同一个 resume 请求：选中的决定攒在本组件的本地 state 里，
 * 用户点"提交决定"时一次性带上所有已做出的决定（未决定的留给下一次提交）。
 */
export function ApprovalCard() {
  const { pendingQuestions, isStreaming, resume, toolCalls } = useChat()
  const pendings = pendingQuestions.filter((q) => q.kind === 'approval')

  // 本地决定草稿：call_id -> 'approve' | 'deny'。单独放在组件内部而不是
  // store——审批决定不需要像 ask_user 答案那样被主输入框共享读写。
  const [drafts, setDrafts] = useState<Record<string, 'approve' | 'deny'>>({})
  const [reasons, setReasons] = useState<Record<string, string>>({})
  const [denyingId, setDenyingId] = useState<string | null>(null)

  const signature = pendings.map((p) => p.callId).join(',')
  useEffect(() => {
    // 挂起集合变化（新一轮审批 / 提交后清空）时清掉本地草稿，
    // 避免上一轮的决定错配到这一轮的 call_id 上。
    setDrafts({})
    setReasons({})
    setDenyingId(null)
  }, [signature])

  if (pendings.length === 0) return null

  const decidedCount = Object.keys(drafts).length
  const allDecided = decidedCount === pendings.length

  const submit = async () => {
    if (decidedCount === 0 || isStreaming) return
    // 拒绝理由只取第一个"拒绝"项的说明，与后端 Reason 字段的单一语义对齐
    // （resumeRequest.Reason 目前是整次请求级别的一个说明，不是逐条的）。
    const firstDenyReason = Object.entries(drafts).find(([, d]) => d === 'deny')
    const reason = firstDenyReason ? reasons[firstDenyReason[0]] : undefined
    await resume({ decisions: drafts, reason })
  }

  return (
    <div className="approval-card" data-no-window-drag>
      <div className="ask-head">
        <span className="approval-badge">
          {pendings.length > 1 ? `需要你的审批（${pendings.length} 项操作）` : '需要你的审批'}
        </span>
        <span className="ask-hint">模型想执行以下操作，请逐条批准或拒绝</span>
      </div>

      {pendings.map((p) => {
        const decision = drafts[p.callId]
        const isDenying = denyingId === p.callId
        // 同名同参数、但不是自己的调用：提示用户"这可能是模型重复发起的操作"。
        // 纯提示，不拦截——参数相同不代表这次一定不该批准。
        const duplicate = findDuplicateExecutedCall(p.toolName, p.toolArgs, p.callId, toolCalls)
        // 代理调用：审批的对象是被代理的自定义工具，标题和参数都展示它（风险等级后端已按它定级）。
        const proxy = parseProxyCall(p.toolName ?? '', p.toolArgs ?? '')
        return (
          <div className="approval-item" key={p.callId}>
            <div className="approval-item-head">
              <span className="tool-name">{proxy?.target ?? (p.toolName || '工具')}</span>
              {proxy?.target && <span className="tool-proxy-target">自定义工具 · 经 search_router</span>}
              {p.risk === 'dangerous' && <span className="tool-badge is-danger">高危</span>}
              {p.risk === 'moderate' && <span className="tool-badge is-warn">写操作</span>}
              {decision === 'approve' && <span className="approval-decided is-approve">已批准</span>}
              {decision === 'deny' && <span className="approval-decided is-deny">已拒绝</span>}
            </div>

            {p.reason && <div className="approval-reason">{p.reason}</div>}

            {duplicate && (
              <div className="approval-duplicate-hint" title={`call_id: ${duplicate.call_id}`}>
                {duplicateHintText(duplicate.status)}
              </div>
            )}

            <pre className="tool-pre">{proxy ? proxy.args : prettyArgs(p.toolArgs)}</pre>

            {isDenying ? (
              <div className="approval-deny-box">
                <textarea
                  className="ask-input"
                  rows={2}
                  placeholder="拒绝理由（可选），会告知模型换一种方式"
                  value={reasons[p.callId] ?? ''}
                  onChange={(e) => setReasons((r) => ({ ...r, [p.callId]: e.target.value }))}
                  disabled={isStreaming}
                  spellCheck={false}
                />
                <div className="approval-item-actions">
                  <button
                    type="button"
                    className="approval-btn"
                    onClick={() => setDenyingId(null)}
                    disabled={isStreaming}
                  >
                    取消
                  </button>
                  <button
                    type="button"
                    className="approval-btn is-deny"
                    onClick={() => {
                      setDrafts((d) => ({ ...d, [p.callId]: 'deny' }))
                      setDenyingId(null)
                    }}
                    disabled={isStreaming}
                  >
                    确认拒绝
                  </button>
                </div>
              </div>
            ) : (
              <div className="approval-item-actions">
                <button
                  type="button"
                  className={`approval-btn is-deny${decision === 'deny' ? ' is-selected' : ''}`}
                  onClick={() => setDenyingId(p.callId)}
                  disabled={isStreaming}
                >
                  拒绝
                </button>
                <button
                  type="button"
                  className={`approval-btn is-approve${decision === 'approve' ? ' is-selected' : ''}`}
                  onClick={() => setDrafts((d) => ({ ...d, [p.callId]: 'approve' }))}
                  disabled={isStreaming}
                >
                  批准
                </button>
              </div>
            )}
          </div>
        )
      })}

      <div className="ask-actions">
        {!allDecided && (
          <span className="ask-remaining">
            已决定 {decidedCount}/{pendings.length} 项
          </span>
        )}
        <button
          className="ask-submit"
          type="button"
          onClick={() => void submit()}
          disabled={decidedCount === 0 || isStreaming}
        >
          {isStreaming ? '提交中…' : allDecided ? '提交全部决定' : `提交已决定的 ${decidedCount} 项`}
        </button>
      </div>
    </div>
  )
}
