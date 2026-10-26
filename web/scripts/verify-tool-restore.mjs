/**
 * 历史工具调用还原的回归验证（rebuildToolCalls）。
 *
 * 这里守住两个曾经踩过的坑：
 *   1. 重开会话后，一轮内的所有工具调用必须**合并成一个摘要气泡**，
 *      而不是每条调用各占一个「工具调用 1 次」的小气泡；
 *   2. 卡片要挂到这一轮真正的回答上（有 content 的 assistant），
 *      即使它在历史里的 seq 排在最前面。
 *
 * 运行：cd web && node scripts/verify-tool-restore.mjs
 */
import { rebuildToolCalls, turnHosts } from '../src/state/toolRestore.ts'

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
const asstCall = (callId, name = 'list_dir') =>
  msg('assistant', '', { tool_calls: [{ id: callId, name, args: '{}' }] })
const toolRes = (callId, output = 'ok') =>
  msg('tool', output, { tool_call_id: callId })

// 摘要分组：按 message_id 聚合，得到「气泡数量」与每组调用数。
const group = (view) => {
  const m = new Map()
  for (const c of Object.values(view)) m.set(c.message_id, (m.get(c.message_id) ?? 0) + 1)
  return [...m.entries()].map(([, v]) => v)
}

console.log('== 1. 服务端真实落库顺序：回答在工具消息之前 ==')
{
  // 这正是 sqlite 里的顺序：占位消息（后来的最终回答）seq 小于所有工具消息。
  n = 0
  const answer = msg('assistant', '最终回答')
  const messages = [
    msg('user', '看看项目结构'),
    answer,
    asstCall('c1'),
    asstCall('c2'),
    toolRes('c1'),
    toolRes('c2'),
    asstCall('c3'),
    toolRes('c3'),
  ]
  const view = rebuildToolCalls(messages)
  eq(group(view), [3], '一轮 3 次调用合并成 1 个气泡')
  eq(
    Object.values(view).every((c) => c.message_id === answer.id),
    true,
    '全部挂到那一轮的回答消息上',
  )
}

console.log('== 2. 多轮各自独立聚合 ==')
{
  n = 0
  const a1 = msg('assistant', '第一轮回答')
  const a2 = msg('assistant', '第二轮回答')
  const messages = [
    msg('user', 'q1'),
    a1,
    asstCall('x1'),
    toolRes('x1'),
    msg('user', 'q2'),
    a2,
    asstCall('y1'),
    asstCall('y2'),
    toolRes('y1'),
    toolRes('y2'),
  ]
  const view = rebuildToolCalls(messages)
  eq(group(view), [1, 2], '两轮分别聚合为 1 次 / 2 次')
  eq(
    Object.values(view).filter((c) => c.message_id === a1.id).length,
    1,
    '第一轮卡片归属正确',
  )
  eq(
    Object.values(view).filter((c) => c.message_id === a2.id).length,
    2,
    '第二轮卡片归属正确',
  )
}

console.log('== 3. 本轮没有文字回答（达到步数上限/被中断）==')
{
  n = 0
  const placeholder = msg('assistant', '')
  const messages = [msg('user', 'q'), placeholder, asstCall('z1'), toolRes('z1')]
  const view = rebuildToolCalls(messages)
  eq(group(view), [1], '仍聚合为 1 个气泡')
  eq(
    Object.values(view)[0].message_id,
    placeholder.id,
    '挂到空内容的占位 assistant 上（不会消失）',
  )
}

console.log('== 4. 工具结果状态映射 ==')
{
  n = 0
  const messages = [
    msg('user', 'q'),
    msg('assistant', '答'),
    asstCall('ok'),
    asstCall('bad'),
    asstCall('gone'),
    toolRes('ok'),
    { ...toolRes('bad'), status: 'error' },
    // 'gone' 没有结果消息
  ]
  const view = rebuildToolCalls(messages)
  eq(view.ok.status, 'success', '正常完成')
  eq(view.bad.status, 'error', '结果标记 error')
  eq(view.gone.status, 'error', '缺结果视为 error，不留 running')
}

console.log('== 5. 只有结果、没有配对调用（历史残缺）==')
{
  n = 0
  const answer = msg('assistant', '答')
  const messages = [msg('user', 'q'), answer, toolRes('orphan', '孤立结果')]
  const view = rebuildToolCalls(messages)
  eq(Object.keys(view).length, 1, '不会丢结果')
  eq(view.orphan.message_id, answer.id, '挂到本轮回答上')
}

console.log('== 6. turnHosts 空输入 ==')
eq(turnHosts([]), [], '空消息列表不炸')

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
