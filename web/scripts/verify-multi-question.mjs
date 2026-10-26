/**
 * 「一次 ask_user 问多个问题」的前端状态验证。
 *
 * 核心难点：**多个问题共享同一个 call_id**。
 *   - 卡片是按 call_id 索引的（一个调用一条卡片）；
 *   - 答案必须按 key（call_id::qid）区分，否则互相覆盖。
 *
 * 如果这里搞错，表现就是：答了 3 个问题，只有 1 个生效，另外 2 个丢失。
 *
 * 运行：cd web && node scripts/verify-multi-question.mjs
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

// 复刻后端 answerKeyOf / 前端 pending.key 的构造规则
const keyOf = (callId, qid) => (qid ? `${callId}::${qid}` : callId)

// 复刻 reducer pending/append（按 key 去重）
const pendingAppend = (list, q) => [...list.filter((x) => x.key !== q.key), q]

// 复刻 draft/set
const draftSet = (state, key, value) => ({
  ...state,
  draftAnswers: { ...state.draftAnswers, [key]: value },
})

console.log('== 1. 一个问题调用产生多条 pending（共享 call_id）==')
{
  let list = []
  list = pendingAppend(list, { key: keyOf('call_m', 'db'), callId: 'call_m', qid: 'db', question: '用哪种库？' })
  list = pendingAppend(list, { key: keyOf('call_m', 'scope'), callId: 'call_m', qid: 'scope', question: '改哪些文件？' })
  list = pendingAppend(list, { key: keyOf('call_m', 'q3'), callId: 'call_m', qid: 'q3', question: '要测试吗？' })

  eq(list.length, 3, '三个问题都保留（不能因为 call_id 相同就合并掉）')
  eq(list.every((q) => q.callId === 'call_m'), true, '三者共享同一个 call_id')
  eq(new Set(list.map((q) => q.key)).size, 3, '三个 key 互不相同')
}

console.log('== 2. 答案按 key 存储，不会互相覆盖 ==')
{
  let st = { draftAnswers: {} }
  st = draftSet(st, keyOf('call_m', 'db'), 'SQLite')
  st = draftSet(st, keyOf('call_m', 'scope'), '只是 src/')
  st = draftSet(st, keyOf('call_m', 'q3'), '要')

  eq(Object.keys(st.draftAnswers).length, 3, '三条答案分别保存')
  eq(st.draftAnswers['call_m::db'], 'SQLite', 'db 的答案正确')
  eq(st.draftAnswers['call_m::scope'], '只是 src/', 'scope 的答案正确')
  eq(st.draftAnswers['call_m::q3'], '要', 'q3 的答案正确')

  // 若错误地按 call_id 作为键，三者会挤在同一个槽位。
  const wrong = { call_m: '要' }
  eq(Object.keys(wrong).length, 1, '（反例）按 call_id 存会塌成 1 条')
}

console.log('== 3. 逐题推进：一次答一个，直到全部答完 ==')
{
  const pending = [
    { key: 'call_m::db' },
    { key: 'call_m::scope' },
    { key: 'call_m::q3' },
  ]
  let st = { draftAnswers: {} }
  const next = () =>
    pending.find((q) => (st.draftAnswers[q.key] ?? '').trim() === '') ?? null

  eq(next().key, 'call_m::db', '先答第 1 题')
  st = draftSet(st, 'call_m::db', 'SQLite')
  eq(next().key, 'call_m::scope', '再答第 2 题')
  st = draftSet(st, 'call_m::scope', 'src/')
  eq(next().key, 'call_m::q3', '再答第 3 题')
  st = draftSet(st, 'call_m::q3', '要')
  eq(next(), null, '全部答完')
}

console.log('== 4. 卡片状态按 call_id 收尾（一个调用一条卡片）==')
{
  // 三个问题 → 只有一条 toolCall（call_id 相同）
  const toolCalls = { call_m: { call_id: 'call_m', status: 'running' } }
  const answers = {
    'call_m::db': 'SQLite',
    'call_m::scope': 'src/',
    'call_m::q3': '要',
  }
  const pending = [
    { key: 'call_m::db', callId: 'call_m' },
    { key: 'call_m::scope', callId: 'call_m' },
    { key: 'call_m::q3', callId: 'call_m' },
  ]
  // 复刻 resume 的乐观收尾：按 callId 去重后置 success
  const after = { ...toolCalls }
  for (const q of pending) {
    if (answers[q.key] === undefined) continue
    after[q.callId] = { ...after[q.callId], output: answers[q.key], status: 'success' }
  }
  eq(Object.keys(after).length, 1, '仍然只有一条卡片')
  eq(after.call_m.status, 'success', '卡片收尾为完成')
}

console.log('== 5. 提交时答案键必须是 key 而不是 call_id ==')
{
  const pending = [
    { key: 'call_m::db', callId: 'call_m' },
    { key: 'call_m::scope', callId: 'call_m' },
  ]
  const drafts = { 'call_m::db': 'A', 'call_m::scope': 'B' }
  const payload = {}
  for (const q of pending) payload[q.key] = drafts[q.key]

  eq(Object.keys(payload).length, 2, '两个键都提交（按 call_id 会塌成 1 个）')
  eq(payload['call_m::db'], 'A', 'db 答案')
  eq(payload['call_m::scope'], 'B', 'scope 答案')
}

console.log('== 6. 单问题调用仍用裸 call_id（向后兼容）==')
{
  eq(keyOf('call_single', ''), 'call_single', '没有 qid 时键就是 call_id')
  eq(keyOf('call_single', 'q1'), 'call_single::q1', '有 qid 时加后缀')
}

console.log('== 7. 重连重放不产生重复卡片 ==')
{
  let list = []
  const q = { key: 'call_m::db', callId: 'call_m', qid: 'db' }
  list = pendingAppend(list, q)
  list = pendingAppend(list, q) // 重放同一事件
  eq(list.length, 1, '同 key 重放不重复')
}

console.log('== 8. 不同调用的同号 qid 不冲突 ==')
{
  let list = []
  list = pendingAppend(list, { key: keyOf('call_a', 'q1'), callId: 'call_a', qid: 'q1' })
  list = pendingAppend(list, { key: keyOf('call_b', 'q1'), callId: 'call_b', qid: 'q1' })
  eq(list.length, 2, '两个调用的 q1 各自独立')
  eq(new Set(list.map((x) => x.key)).size, 2, 'key 前缀不同，互不覆盖')
}

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
