/**
 * 两个「回答后不生效」的回归验证。
 *
 * 背景（用户实际反馈）：
 *   1. 回答后卡片仍停在「调用中」——
 *      resume 的响应流里没有 tool.result（那两条 tool 消息由后端直接
 *      写库、不发事件），前端不主动收尾就一直转圈。
 *   2. 一次只能答一个问题，且不能用主输入框回答——
 *      答案状态原先锁在 AskUserCard 组件内部，Composer 读不到；
 *      同时输入框在挂起时被整个禁用。
 *
 * 运行：cd web && node scripts/verify-ask-answer.mjs
 */

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

// ---- 复刻 reducer 的 draft 分支（作用于 state 对象）----
const draftSet = (state, callId, value) => ({
  ...state,
  draftAnswers: { ...state.draftAnswers, [callId]: value },
})
const draftReset = (state) => ({ ...state, draftAnswers: {} })

// ---- 复刻 resume 成功后对卡片状态的收尾 ----
function applyAnsweredToolCalls(toolCalls, answers) {
  const next = { ...toolCalls }
  for (const [callId, answer] of Object.entries(answers)) {
    const prev = next[callId] ?? {
      call_id: callId,
      name: 'ask_user',
      args: '',
      risk: 'safe',
      requires_approval: false,
    }
    next[callId] = { ...prev, output: answer, status: 'success' }
  }
  return next
}

// ---- 复刻 Composer 的答题推进逻辑 ----
const nextActiveQuestion = (pending, drafts) =>
  pending.find((q) => (drafts[q.callId] ?? '').trim() === '') ?? null
const allAnswered = (pending, drafts) =>
  pending.length > 0 && pending.every((q) => (drafts[q.callId] ?? '').trim() !== '')

console.log('== 1. 回答后卡片状态必须翻成完成 ==')
{
  const toolCalls = {
    call_a: { call_id: 'call_a', name: 'ask_user', args: '', risk: 'safe', requires_approval: false, status: 'running' },
    call_b: { call_id: 'call_b', name: 'ask_user', args: '', risk: 'safe', requires_approval: false, status: 'running' },
  }
  const after = applyAnsweredToolCalls(toolCalls, { call_a: '阶段3', call_b: '要' })
  eq(after.call_a.status, 'success', '第一张卡片变成功（不再显示调用中）')
  eq(after.call_b.status, 'success', '第二张卡片变成功')
  eq(after.call_a.output, '阶段3', '输出即用户回答')
}

console.log('== 2. 部分回答：只翻已答的那张 ==')
{
  const toolCalls = {
    call_a: { call_id: 'call_a', status: 'running' },
    call_b: { call_id: 'call_b', status: 'running' },
  }
  const after = applyAnsweredToolCalls(toolCalls, { call_a: '答了 A' })
  eq(after.call_a.status, 'success', 'A 已完成')
  eq(after.call_b.status, 'running', 'B 仍在等待（不受影响）')
}

console.log('== 3. 提交失败要回滚（不能假装答过）==')
{
  const toolCalls = { call_a: { call_id: 'call_a', status: 'running' } }
  const optimistic = applyAnsweredToolCalls(toolCalls, { call_a: 'x' })
  eq(optimistic.call_a.status, 'success', '先乐观置成功')
  const rolled = { ...optimistic, call_a: { ...optimistic.call_a, status: 'running' } }
  eq(rolled.call_a.status, 'running', '失败后回滚为 running')
}

console.log('== 4. 输入框逐题推进 ==')
{
  const pending = [{ callId: 'a' }, { callId: 'b' }]
  let st = { draftAnswers: {} }

  eq(nextActiveQuestion(pending, st.draftAnswers).callId, 'a', '初始指向第一个问题')

  st = draftSet(st, 'a', '答案A')
  eq(nextActiveQuestion(pending, st.draftAnswers).callId, 'b', '答完 A 后自动指向 B')
  eq(allAnswered(pending, st.draftAnswers), false, '还有 B 未答，不能提交')

  st = draftSet(st, 'b', '答案B')
  eq(nextActiveQuestion(pending, st.draftAnswers), null, '全部答完后没有待答项')
  eq(allAnswered(pending, st.draftAnswers), true, '此时可以提交')
}

console.log('== 5. 空白答案不算回答 ==')
{
  const pending = [{ callId: 'a' }]
  const st = draftSet({ draftAnswers: {} }, 'a', '   ')
  eq(allAnswered(pending, st.draftAnswers), false, '纯空白不能提交')
  eq(nextActiveQuestion(pending, st.draftAnswers).callId, 'a', '仍指向该问题')
}

console.log('== 6. 提交后清空草稿 ==')
{
  const st = draftReset({ draftAnswers: { a: 'x', b: 'y' } })
  eq(st.draftAnswers, {}, '提交后草稿清空（避免串到下一轮）')
}

console.log('== 7. 输入框在挂起时不应被禁用 ==')
{
  // 复刻 Composer 的 disabled 计算：挂起状态不再参与禁用判断
  const computeDisabled = (currentId, isStreaming) => !currentId || isStreaming
  eq(computeDisabled('s1', false), false, '挂起且空闲时可输入')
  eq(computeDisabled('s1', true), true, '生成中才禁用')
  eq(computeDisabled(null, false), true, '没有会话时禁用')
}

console.log('== 8. 单选问题也能提交 ==')
{
  const pending = [{ callId: 'a' }]
  let st = { draftAnswers: {} }
  eq(allAnswered(pending, st.draftAnswers), false, '未答不能提交')
  st = draftSet(st, 'a', 'SQLite')
  eq(allAnswered(pending, st.draftAnswers), true, '答完即可提交（不需要多问题）')
}

console.log('== 9. 选项问题也支持自由输入 ==')
{
  // 卡片里选了选项后又输入自由文本：以最后输入为准
  let st = draftSet({ draftAnswers: {} }, 'a', 'SQLite')
  eq(st.draftAnswers.a, 'SQLite', '选选项写入草稿')
  st = draftSet(st, 'a', '其实我想用 Postgres')
  eq(st.draftAnswers.a, '其实我想用 Postgres', '自由输入覆盖选项')
}

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
