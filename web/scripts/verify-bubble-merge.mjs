/**
 * 「同一轮被拆成两个 Agent 气泡」的回归验证。
 *
 * 现象（截图）：挂起→恢复后，同一轮对话出现两个「Agent」标题：
 *   气泡 A：工具调用 1 次 ask_user
 *   气泡 B：工具调用 3 次 read_file, search + 最终回答
 *
 * 根因：挂起时占位消息 A 承载 ask_user 卡片；resume 会**新建**占位
 * 消息 B 承载后续生成。前端各按 message_id 过滤工具调用，
 * 于是两套卡片落在两条消息上，渲染成两个气泡。
 *
 * 关键风险：合并不能过度。不同轮次（中间隔着 user 消息）必须分开。
 *
 * 运行：cd web && node --experimental-strip-types scripts/verify-bubble-merge.mjs
 */

import { groupCallsByHost, hostMap, rebuildToolCalls } from '../src/state/toolRestore.ts'

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
const call = (callId, messageId, name = 'ask_user', status = 'success') => ({
  call_id: callId,
  message_id: messageId,
  name,
  args: '{}',
  risk: 'safe',
  requires_approval: false,
  status,
})

console.log('== 1. 挂起→恢复：两个占位合并到同一宿主 ==')
{
  n = 0
  const user = msg('user', '问')
  const holderA = msg('assistant', '', { tool_calls: [{ id: 'c_ask', name: 'ask_user', args: '{}' }] })
  const holderB = msg('assistant', '最终回答', {
    tool_calls: [{ id: 'c_read', name: 'read_file', args: '{}' }, { id: 'c_search', name: 'search', args: '{}' }],
  })

  // 两条 assistant 中间没有新的 user → 属于同一轮
  const messages = [user, holderA, holderB]
  const calls = [
    call('c_ask', holderA.id),
    call('c_read', holderB.id, 'read_file'),
    call('c_search', holderB.id, 'search'),
  ]

  const hosts = hostMap(messages)
  eq(hosts[holderA.id], holderB.id, '占位 A 的宿主是 B（有内容的回答）')
  eq(hosts[holderB.id], holderB.id, 'B 自己是宿主')

  const grouped = groupCallsByHost(messages, calls)
  eq(Object.keys(grouped).length, 1, '三个调用归到同一个宿主（只渲染一个气泡）')
  eq(grouped[holderB.id].length, 3, '宿主名下 3 个调用')

  // A 不是宿主 → ChatView 会跳过它，不渲染第二个 Agent 气泡
  eq(hosts[holderA.id] !== holderA.id, true, 'A 会被跳过、不单独成气泡')
}

console.log('== 2. 不同轮次必须分开（不能过度合并）==')
{
  n = 0
  const u1 = msg('user', '第一问')
  const a1 = msg('assistant', '第一答')
  const u2 = msg('user', '第二问')
  const a2 = msg('assistant', '第二答')

  const messages = [u1, a1, u2, a2]
  const hosts = hostMap(messages)
  eq(hosts[a1.id], a1.id, '第一轮的答是它自己的宿主')
  eq(hosts[a2.id], a2.id, '第二轮的答是它自己的宿主')
  eq(hosts[a1.id] !== hosts[a2.id], true, '两轮不会合并')

  const calls = [call('c1', a1.id, 'read_file'), call('c2', a2.id, 'search')]
  const grouped = groupCallsByHost(messages, calls)
  eq(Object.keys(grouped).length, 2, '两轮的调用分属两个宿主')
  eq(grouped[a1.id].length, 1, '第一轮 1 个调用')
  eq(grouped[a2.id].length, 1, '第二轮 1 个调用')
}

console.log('== 3. 同一轮多次挂起（连续 resume）也合并 ==')
{
  n = 0
  const user = msg('user', '问')
  const holderA = msg('assistant', '', { tool_calls: [{ id: 'c1', name: 'ask_user', args: '{}' }] })
  const holderB = msg('assistant', '', { tool_calls: [{ id: 'c2', name: 'ask_user', args: '{}' }] })
  const holderC = msg('assistant', '终于答完了', {
    tool_calls: [{ id: 'c3', name: 'read_file', args: '{}' }],
  })

  const messages = [user, holderA, holderB, holderC]
  const hosts = hostMap(messages)
  eq(hosts[holderA.id], holderC.id, 'A 归到最终回答')
  eq(hosts[holderB.id], holderC.id, 'B 也归到最终回答')

  const calls = [
    call('c1', holderA.id),
    call('c2', holderB.id),
    call('c3', holderC.id, 'read_file'),
  ]
  const grouped = groupCallsByHost(messages, calls)
  eq(Object.keys(grouped).length, 1, '三次挂起仍只有一个气泡')
  eq(grouped[holderC.id].length, 3, '宿主名下 3 个调用')
}

console.log('== 4. 没有最终回答时挂到第一条（卡片不能消失）==')
{
  n = 0
  const user = msg('user', '问')
  const onlyHolder = msg('assistant', '', { tool_calls: [{ id: 'c1', name: 'ask_user', args: '{}' }] })

  const messages = [user, onlyHolder]
  const hosts = hostMap(messages)
  eq(hosts[onlyHolder.id], onlyHolder.id, '唯一一条 assistant 自己就是宿主')

  const calls = [call('c1', onlyHolder.id, 'ask_user', 'running')]
  const grouped = groupCallsByHost(messages, calls)
  eq(Object.keys(grouped).length, 1, '仍然渲染一个气泡')
  eq(grouped[onlyHolder.id][0].status, 'running', '等待中的卡片保留 running 状态')
}

console.log('== 5. 用户消息不参与宿主计算 ==')
{
  n = 0
  const u1 = msg('user', '问1')
  const u2 = msg('user', '问2')
  const messages = [u1, u2]
  const hosts = hostMap(messages)
  eq(hosts[u1.id], undefined, 'user 消息没有宿主概念')
  eq(hosts[u2.id], undefined, 'user 消息没有宿主概念')
}

console.log('== 6. 调用归属的消息已不存在时不丢卡片 ==')
{
  n = 0
  const user = msg('user', '问')
  const ans = msg('assistant', '答')
  const messages = [user, ans]
  // 调用指向一条已被裁剪掉的消息
  const calls = [call('c_lost', 'm_gone', 'read_file')]
  const grouped = groupCallsByHost(messages, calls)
  eq(Object.keys(grouped).length, 1, '卡片仍在（不会凭空消失）')
  eq(Object.keys(grouped)[0], 'm_gone', '退回到原始归属，交给调用方兜底')
}

console.log('== 7. 实时聚合结果与刷新后还原一致 ==')
{
  n = 0
  const user = msg('user', '问')
  const holderA = msg('assistant', '', { tool_calls: [{ id: 'c_ask', name: 'ask_user', args: '{}' }] })
  const toolMsg = msg('tool', '用户答了', { tool_call_id: 'c_ask' })
  const holderB = msg('assistant', '最终回答', {
    tool_calls: [{ id: 'c_read', name: 'read_file', args: '{}' }],
  })
  const toolMsg2 = msg('tool', '文件内容', { tool_call_id: 'c_read' })

  const messages = [user, holderA, toolMsg, holderB, toolMsg2]

  // 实时路径：calls 由 SSE 累积
  const liveCalls = [call('c_ask', holderA.id), call('c_read', holderB.id, 'read_file')]
  const live = groupCallsByHost(messages, liveCalls)

  // 刷新路径：从历史还原
  const restored = rebuildToolCalls(messages)

  eq(Object.keys(live).length, 1, '实时：一个气泡')
  eq(new Set(Object.values(restored).map((c) => c.message_id)).size, 1, '刷新：也是一个气泡')
  eq(
    Object.keys(live)[0],
    Object.values(restored)[0].message_id,
    '两种视图的宿主一致（实时=刷新）',
  )
}

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
