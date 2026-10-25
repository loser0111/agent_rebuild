/**
 * 工具调用状态机的纯逻辑验证。
 *
 * 为什么单独写这个：reducer 是前端展示正确性的核心，
 * 而项目目前没有引入测试框架（无 vitest/jest）。
 * 这里用 node 直接跑，把逻辑从 React 里剥离出来验证，
 * 避免为了一个断言去拉一整套测试依赖。
 *
 * 运行：node web/scripts/verify-tool-events.mjs
 */

let failed = 0
let passed = 0

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

// ---- 复刻 reducer 中的工具相关分支 ----
function applyToolCall(state, payload) {
  return { ...state, toolCalls: { ...state.toolCalls, [payload.call_id]: payload } }
}

function applyToolResult(state, { call_id, patch }) {
  const prev = state.toolCalls[call_id]
  const base = prev ?? {
    call_id,
    message_id: '',
    name: '',
    args: '',
    risk: 'safe',
    requires_approval: false,
    status: 'running',
  }
  return {
    ...state,
    toolCalls: { ...state.toolCalls, [call_id]: { ...base, ...patch } },
  }
}

console.log('== 1. tool.call 建立运行中条目 ==')
let s = { toolCalls: {} }
s = applyToolCall(s, {
  call_id: 'call_1',
  message_id: 'msg_A',
  name: 'read_file',
  args: '{"path":"a.txt"}',
  risk: 'safe',
  requires_approval: false,
  status: 'running',
})
eq(Object.keys(s.toolCalls).length, 1, '应有 1 条记录')
eq(s.toolCalls['call_1'].status, 'running', '初始状态为 running')
eq(s.toolCalls['call_1'].name, 'read_file', '工具名正确')

console.log('== 2. tool.result 成功完成 ==')
s = applyToolResult(s, {
  call_id: 'call_1',
  patch: { name: 'read_file', output: '文件内容', duration_ms: 12, status: 'success' },
})
eq(s.toolCalls['call_1'].status, 'success', '状态变为 success')
eq(s.toolCalls['call_1'].output, '文件内容', '输出写入')
eq(s.toolCalls['call_1'].duration_ms, 12, '耗时写入')
eq(s.toolCalls['call_1'].args, '{"path":"a.txt"}', '原有参数不被覆盖')

console.log('== 3. 失败的调用 ==')
s = applyToolCall(s, {
  call_id: 'call_2', message_id: 'msg_A', name: 'edit_file',
  args: '{}', risk: 'moderate', requires_approval: false, status: 'running',
})
s = applyToolResult(s, {
  call_id: 'call_2',
  patch: { output: '权限不足', is_error: true, status: 'error' },
})
eq(s.toolCalls['call_2'].status, 'error', '错误状态标记正确')

console.log('== 4. 被拒绝的调用 ==')
s = applyToolCall(s, {
  call_id: 'call_3', message_id: 'msg_A', name: 'run_command',
  args: '{}', risk: 'dangerous', requires_approval: true, status: 'running',
})
s = applyToolResult(s, {
  call_id: 'call_3',
  patch: { rejected: true, status: 'rejected' },
})
eq(s.toolCalls['call_3'].status, 'rejected', '拒绝状态标记正确')
eq(s.toolCalls['call_3'].risk, 'dangerous', '危险等级保留')

console.log('== 5. 结果先于调用到达（重连场景不丢信息） ==')
let s2 = { toolCalls: {} }
s2 = applyToolResult(s2, {
  call_id: 'call_x',
  patch: { name: 'search', output: '结果', status: 'success' },
})
eq(s2.toolCalls['call_x'].status, 'success', '结果能被接收')
eq(s2.toolCalls['call_x'].output, '结果', '输出被保留')

console.log('== 6. 多条调用按到达顺序 ==')
eq(Object.keys(s.toolCalls), ['call_1', 'call_2', 'call_3'], '顺序为插入顺序')

console.log('== 7. 按 message_id 归属过滤 ==')
const ofMsgA = Object.values(s.toolCalls).filter((c) => c.message_id === 'msg_A')
eq(ofMsgA.length, 3, 'msg_A 名下 3 条')
const ofMsgB = Object.values(s.toolCalls).filter((c) => c.message_id === 'msg_B')
eq(ofMsgB.length, 0, 'msg_B 名下 0 条')

console.log('== 8. 重置 ==')
s = { ...s, toolCalls: {} }
eq(Object.keys(s.toolCalls).length, 0, '重置后为空')

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
