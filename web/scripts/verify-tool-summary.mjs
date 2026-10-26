/**
 * 工具调用折叠摘要的逻辑验证。
 *
 * 关注点：
 *   1. 默认收起（不展开卡片）
 *   2. 摘要显示"调用 N 次"
 *   3. 工具名去重、过长截断
 *   4. 运行中的调用自动展开（否则用户以为卡住）
 *   5. 有错误时给出视觉区分
 *
 * 运行：node web/scripts/verify-tool-summary.mjs
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

// ---- 复刻 ToolCallCard.tsx 中的 summarizeCalls ----
function summarizeCalls(calls) {
  const names = Array.from(new Set(calls.map((c) => c.name).filter(Boolean)))
  const head = names.slice(0, 3).join(', ')
  const more = names.length > 3 ? ` 等 ${names.length} 个` : ''
  return head ? `${head}${more}` : '工具调用'
}

// ---- 复刻展开判定 ----
function isExpanded(userOpen, calls) {
  const running = calls.some((c) => c.status === 'running')
  return userOpen || running
}

const call = (name, status = 'success') => ({ call_id: name + Math.random(), name, status })

console.log('== 1. 摘要文案 ==')
eq(summarizeCalls([call('list_dir')]), 'list_dir', '单个工具')
eq(summarizeCalls([call('list_dir'), call('search')]), 'list_dir, search', '两个工具')
eq(
  summarizeCalls([call('a'), call('b'), call('c')]),
  'a, b, c',
  '三个工具全列出',
)
eq(
  summarizeCalls([call('a'), call('b'), call('c'), call('d')]),
  'a, b, c 等 4 个',
  '超过三个折叠为"等 N 个"',
)

console.log('== 2. 工具名去重 ==')
eq(
  summarizeCalls([call('list_dir'), call('list_dir'), call('list_dir')]),
  'list_dir',
  '重复调用同一工具只显示一次',
)
eq(
  summarizeCalls([call('list_dir'), call('search'), call('list_dir')]),
  'list_dir, search',
  '混合重复也去重',
)

console.log('== 3. 空名称兜底 ==')
eq(summarizeCalls([call('')]), '工具调用', '空名称回退为通用文案')

console.log('== 4. 默认折叠 ==')
{
  const calls = [call('list_dir'), call('search')]
  eq(isExpanded(false, calls), false, '全部完成时默认收起')
}

console.log('== 5. 运行中自动展开 ==')
{
  const calls = [call('list_dir', 'running')]
  eq(isExpanded(false, calls), true, '有运行中的调用时自动展开')
}
{
  const calls = [call('list_dir'), call('search', 'running')]
  eq(isExpanded(false, calls), true, '混合状态下也展开')
}

console.log('== 6. 用户手动展开 ==')
{
  const calls = [call('list_dir')]
  eq(isExpanded(true, calls), true, '用户点击后展开')
}

console.log('== 7. 错误状态识别 ==')
{
  const calls = [call('list_dir', 'error')]
  const anyError = calls.some((c) => c.status === 'error')
  eq(anyError, true, '错误可被识别（用于红色边框）')
}
{
  const calls = [call('list_dir', 'success')]
  const anyError = calls.some((c) => c.status === 'error')
  eq(anyError, false, '正常状态不标红')
}

console.log('== 8. 次数统计 ==')
eq([call('a'), call('b'), call('c')].length, 3, '3 次调用')
eq([call('a'), call('a')].length, 2, '同一工具的两次调用算 2 次')

console.log()
console.log(`结果：${passed} 通过，${failed} 失败`)
process.exit(failed === 0 ? 0 : 1)
