/**
 * ask_user 前端状态机的纯逻辑验证。
 *
 * 关注点：
 *   1. awaiting.input 事件要把 pendingQuestion 设起来
 *   2. 切换会话必须清掉挂起态（否则会串台）
 *   3. message.done 不应残留挂起态
 *   4. resume 请求体要带对的 call_id / answer
 *
 * 运行：node web/scripts/verify-ask-user.mjs
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

// ---- 复刻 reducer 中与挂起相关的分支 ----
const initialState = { pendingQuestion: null, messages: [], toolCalls: {}, currentId: null }

function applyPending(state, payload) {
  return { ...state, pendingQuestion: payload }
}

function applyCurrentSet(state, id) {
  return {
    ...state,
    currentId: id,
    messages: [],
    toolCalls: {},
    // 关键：切会话必须清空，否则 A 会话的问题会出现在 B 会话
    pendingQuestion: null,
  }
}

// 复刻 handleEvent 对 awaiting.input 的处理
function applyAwaitingInput(state, data) {
  let next = applyPending(state, {
    callId: data.call_id,
    messageId: data.message_id,
    question: data.question,
    options: data.options ?? [],
  })
  next = {
    ...next,
    messages: next.messages.map((m) =>
      m.id === data.message_id ? { ...m, status: 'streaming' } : m,
    ),
  }
  return next
}

console.log('== 1. awaiting.input 建立挂起态 ==')
{
  const st = applyAwaitingInput(
    { ...initialState, messages: [{ id: 'm1', status: 'pending' }] },
    {
      seq: 5,
      session_id: 's1',
      message_id: 'm1',
      call_id: 'call_abc',
      question: '用哪种数据库？',
      options: ['SQLite', 'Postgres'],
      step_index: 0,
    },
  )
  eq(st.pendingQuestion.callId, 'call_abc', 'callId 被记录（resume 幂等键）')
  eq(st.pendingQuestion.question, '用哪种数据库？', '问题文本被记录')
  eq(st.pendingQuestion.options, ['SQLite', 'Postgres'], '选项被记录')
  eq(st.messages[0].status, 'streaming', '占位消息不再显示 pending')
}

console.log('== 2. 无选项时 options 归一化为空数组 ==')
{
  const st = applyAwaitingInput(initialState, {
    seq: 1,
    session_id: 's1',
    message_id: 'm1',
    call_id: 'c1',
    question: '随便说说',
    step_index: 0,
  })
  eq(st.pendingQuestion.options, [], '缺省 options 应为空数组（前端据此判断自由输入）')
}

console.log('== 3. 切换会话清空挂起态 ==')
{
  const st = applyPending(initialState, {
    callId: 'c1',
    messageId: 'm1',
    question: 'Q',
    options: [],
  })
  eq(st.pendingQuestion !== null, true, '先有挂起态')
  const after = applyCurrentSet(st, 's2')
  eq(after.pendingQuestion, null, '切会话后清空（避免串台）')
  eq(after.currentId, 's2', '会话已切换')
}

console.log('== 4. 提交后立即清空（防重复提交）==')
{
  let st = applyPending(initialState, {
    callId: 'c1',
    messageId: 'm1',
    question: 'Q',
    options: [],
  })
  // resume() 里的第一件事就是清掉
  st = applyPending(st, null)
  eq(st.pendingQuestion, null, '提交瞬间清空，连点不会重复提交')
}

console.log('== 5. resume 请求体构造 ==')
{
  const buildResumeBody = (callId, answer) => ({ call_id: callId, answer })
  eq(
    buildResumeBody('call_x', '用 SQLite'),
    { call_id: 'call_x', answer: '用 SQLite' },
    '请求体带 call_id 与 answer',
  )
}

console.log('== 6. 选项模式下的有效答案 ==')
{
  // 复刻 AskUserCard 的取值逻辑
  const resolve = (hasOptions, selected, typed) => (hasOptions ? (selected ?? '') : typed.trim())
  eq(resolve(true, 'SQLite', '忽略我'), 'SQLite', '有选项时取选中项')
  eq(resolve(true, null, '忽略我'), '', '未选中时为空（不能提交）')
  eq(resolve(false, null, '  自由输入  '), '自由输入', '无选项时取输入并 trim')
  eq(resolve(false, null, '   '), '', '纯空白不能提交')
}

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
