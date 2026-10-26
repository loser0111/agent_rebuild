/**
 * 三个曾出问题的行为回归验证。
 *
 * 背景（都是线上实际踩到的）：
 *   1. 重开软件后，挂起中的 ask_user 显示「失败」——
 *      因为还原时把「还没有 tool 结果」一律判成 error。
 *   2. 同一轮多个 ask_user 只显示第一个，其余静默消失——
 *      因为 loop 遇到第一个挂起就 return。
 *   3. 用户回答后，卡片状态仍停在「调用中」——
 *      回答补的是 tool 消息，没有人更新卡片状态。
 *
 * 运行：cd web && node --experimental-strip-types scripts/verify-ask-user-fixes.mjs
 */

import { rebuildToolCalls } from '../src/state/toolRestore.ts'

let passed = 0
let failed = 0

function eq(actual, expected, label) {
  const a = JSON.stringify(actual)
  const e = JSON.stringify(expected)
  if (a === e) {
    passed++
    console.log(`  ✓ ${label}`)
  } else {
    failed++
    console.log(`  ✗ ${label}\n      实际: ${a}\n      期望: ${e}`)
  }
}

let n = 0
const msg = (role, content, extra = {}) => ({
  id: `m${++n}`,
  session_id: 's',
  role,
  content,
  status: 'complete',
  created_at: '',
  updated_at: '',
  ...extra,
})
const asstCall = (callId, name = 'ask_user') =>
  msg('assistant', '', { tool_calls: [{ id: callId, name, args: '{"question":"q"}' }] })
const toolRes = (callId, output = '答复') => msg('tool', output, { tool_call_id: callId })

console.log('== 1. 挂起中的 ask_user 不能显示为失败 ==')
{
  n = 0
  const answer = msg('assistant', '我需要确认一下')
  const messages = [msg('user', '帮我建表'), answer, asstCall('call_pending')]

  // 不传 pendingCallIds：无法区分「等回答」与「真丢结果」→ 保守判 error（旧行为）
  const noHint = rebuildToolCalls(messages)
  eq(noHint.call_pending.status, 'error', '无提示时保守判 error（保持原有语义）')

  // 传了挂起集合：应识别为 running（正在等回答）
  const withHint = rebuildToolCalls(messages, new Set(['call_pending']))
  eq(withHint.call_pending.status, 'running', '挂起中的调用显示为 running 而非失败')
}

console.log('== 2. 同一轮多个问题都要保留 ==')
{
  n = 0
  const answer = msg('assistant', '两个问题')
  const messages = [
    msg('user', 'q'),
    answer,
    asstCall('call_a'),
    asstCall('call_b'),
    asstCall('call_c'),
  ]
  const view = rebuildToolCalls(messages, new Set(['call_a', 'call_b', 'call_c']))
  eq(Object.keys(view).length, 3, '三个问题全部还原（不静默消失）')
  eq(
    Object.values(view).every((c) => c.status === 'running'),
    true,
    '三个都在等待回答',
  )
  eq(
    Object.values(view).every((c) => c.message_id === answer.id),
    true,
    '聚合在同一轮的摘要下（不是三个散落气泡）',
  )
}

console.log('== 3. 回答后卡片状态应变成功 ==')
{
  n = 0
  const answer = msg('assistant', '问完了')
  // 已补 tool 结果 → 不再是 running，而是 success 且带输出
  const messages = [
    msg('user', 'q'),
    answer,
    asstCall('call_a'),
    toolRes('call_a', '我用 SQLite'),
  ]
  const view = rebuildToolCalls(messages, new Set()) // 已无挂起项
  eq(view.call_a.status, 'success', '已回答的调用显示为完成')
  eq(view.call_a.output, '我用 SQLite', '输出即用户的回答')
}

console.log('== 4. 部分回答：已答完成、未答等待 ==')
{
  n = 0
  const answer = msg('assistant', '两个问题')
  const messages = [
    msg('user', 'q'),
    answer,
    asstCall('call_a'),
    asstCall('call_b'),
    toolRes('call_a', '答了 A'),
  ]
  // 只有 B 还在等
  const view = rebuildToolCalls(messages, new Set(['call_b']))
  eq(view.call_a.status, 'success', 'A 已完成')
  eq(view.call_b.status, 'running', 'B 仍在等待')
}

console.log('== 5. 真丢结果的调用仍判 error（不能矫枉过正）==')
{
  n = 0
  const messages = [msg('user', 'q'), msg('assistant', ''), asstCall('call_lost')]
  const view = rebuildToolCalls(messages, new Set(['call_something_else']))
  eq(view.call_lost.status, 'error', '不在挂起集合里的缺失结果仍是 error')
}

console.log('== 6. 多问题状态累积（复刻 reducer pending/append）==')
{
  const append = (list, q) => [...list.filter((x) => x.callId !== q.callId), q]

  let list = []
  list = append(list, { callId: 'a', question: 'Q1', options: [], messageId: 'm' })
  list = append(list, { callId: 'b', question: 'Q2', options: [], messageId: 'm' })
  eq(list.length, 2, '两个问题都累积下来（覆盖式实现会只剩 1 个）')
  eq(list.map((x) => x.callId), ['a', 'b'], '顺序与模型提问顺序一致')

  // 重连重放同一事件：不能产生重复卡片
  list = append(list, { callId: 'b', question: 'Q2', options: [], messageId: 'm' })
  eq(list.length, 2, '重放同一 call_id 不重复')
}

console.log('== 7. 批量提交的完整性校验 ==')
{
  const allAnswered = (qs, ans) => qs.every((q) => (ans[q.callId] ?? '').trim().length > 0)

  const qs = [{ callId: 'a' }, { callId: 'b' }]
  eq(allAnswered(qs, { a: 'x', b: 'y' }), true, '全部已回答')
  eq(allAnswered(qs, { a: 'x' }), false, '漏答一个不能提交')
  eq(allAnswered(qs, { a: 'x', b: '   ' }), false, '纯空白不算回答')
}

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
