/**
 * 历史工具调用 → 前端卡片视图的还原逻辑。
 *
 * 抽成独立模块的原因：这段算法决定了「刷新/重开会话后工具气泡长什么样」，
 * 是纯函数但曾在两个 bug 里出过问题（见下方注释），需要能被单独断言。
 * 这里不依赖 React，只依赖 api/types 的类型。
 */
import type { Message, ToolCallView } from '../api/types'

/**
 * 计算「每条消息所属轮次的宿主消息 id」。
 *
 * 一轮 = 一条 user 消息到下一条 user 消息之间的所有消息。
 * 轮内宿主优先级：
 *   1. 有 content 的 assistant —— 就是这一轮的回答（流式时卡片挂的正是它）；
 *   2. 不带 tool_calls 的 assistant —— 占位消息（回答为空/被中断时）；
 *   3. 轮内第一条 assistant —— 兜底，保证卡片至少不会消失。
 */
export function turnHosts(messages: Message[]): string[] {
  const hosts = new Array<string>(messages.length).fill('')
  let i = 0
  while (i < messages.length) {
    let end = i + 1
    while (end < messages.length && messages[end].role !== 'user') end++

    let host = ''
    for (let k = i; k < end && !host; k++) {
      const m = messages[k]
      if (m.role === 'assistant' && m.content) host = m.id
    }
    for (let k = i; k < end && !host; k++) {
      const m = messages[k]
      if (m.role === 'assistant' && !m.tool_calls?.length) host = m.id
    }
    for (let k = i; k < end && !host; k++) {
      if (messages[k].role === 'assistant') host = messages[k].id
    }

    for (let k = i; k < end; k++) hosts[k] = host
    i = end
  }
  return hosts
}

/**
 * 把（实时收到的）工具调用按「轮次宿主」分组，供渲染使用。
 *
 * 为什么需要它：挂起→恢复会新建一条 assistant 占位消息，
 * 于是同一轮里出现两条 assistant：
 *
 *   占位 A（挂起时那一轮）→ ask_user 卡片
 *   占位 B（resume 之后）  → read_file/search 卡片 + 最终回答
 *
 * 若各按自己的 message_id 渲染，用户就会看到两个「Agent」气泡，
 * 明明是同一轮对话却被劈成两半。而刷新后用的 rebuildToolCalls
 * 是按轮次聚合的，只显示一个气泡——实时与刷新不一致本身就是 bug。
 *
 * 这里复用同一套 turnHosts 规则，把实时数据也归到轮次宿主上，
 * 保证两种视图完全一致。
 */
export function groupCallsByHost(
  messages: Message[],
  calls: ToolCallView[],
): Record<string, ToolCallView[]> {
  const out: Record<string, ToolCallView[]> = {}
  if (messages.length === 0 || calls.length === 0) return out

  const indexOf = new Map(messages.map((m, i) => [m.id, i]))
  const hosts = turnHosts(messages)

  for (const c of calls) {
    const idx = indexOf.get(c.message_id)
    // 归属消息已不存在（被裁剪、或 id 对不上）时退回到自身，
    // 宁可单独显示也不要让卡片凭空消失。
    const hostId = idx === undefined ? c.message_id : hosts[idx] || messages[idx].id
    if (!hostId) continue
    ;(out[hostId] ??= []).push(c)
  }
  return out
}

/**
 * 计算「哪条消息是本轮的宿主」的映射（messageId → 宿主 messageId）。
 *
 * ChatView 用它跳过同轮里多余的那条 assistant 占位消息，
 * 保证一轮只渲染一个 Agent 气泡。
 *
 * 与 groupCallsByHost 分开是刻意的：后者在「有工具调用」时才需要，
 * 而跳过多余气泡在**没有**工具调用时同样要生效
 * （例如挂起后用户直接结束，只留下两条空占位）。
 */
export function hostMap(messages: Message[]): Record<string, string> {
  const out: Record<string, string> = {}
  if (messages.length === 0) return out
  const hosts = turnHosts(messages)
  for (let i = 0; i < messages.length; i++) {
    const h = hosts[i]
    if (h) out[messages[i].id] = h
  }
  return out
}

/**
 * 从历史消息还原工具调用视图。
 *
 * 后端把工具调用存成两类消息：
 *   - assistant 消息带 tool_calls（发起）
 *   - tool 消息带 tool_call_id（结果）
 * 重开应用时前端只能拿到这些数据，需要重新拼成卡片，
 * 否则用户看到的就是一堆裸露的文本（工具结果直接铺在对话里）。
 *
 * 归属策略：**按轮次聚合**，一轮内的所有调用共用同一个宿主消息。
 *
 * 为什么不能按「消息先后顺序」往后找宿主：
 * 服务端实际落库顺序是这样的（见 sqlite 里的 seq）——
 *
 *   seq1 user
 *   seq2 assistant(content=最终回答)   ← 占位消息在 user 之后立刻 append，
 *   seq3 assistant(tool_calls)            seq 天然小于后面所有工具消息
 *   seq4 tool ...
 *
 * 也就是说「回答」在历史里排在最前面。若往后找第一条有内容的 assistant，
 * 永远找不到，每条工具调用只能各自成卡，界面上就散成一堆「工具调用 1 次」
 * 的单条气泡。而流式过程中所有卡片共用占位消息 id（= 那一轮的回答），
 * 所以重开会话要还原成一模一样的形态，就必须按轮次聚合。
 */
export function rebuildToolCalls(
  messages: Message[],
  pendingCallIds?: Set<string>,
): Record<string, ToolCallView> {
  const out: Record<string, ToolCallView> = {}
  const hosts = turnHosts(messages)

  // 按 tool_call_id 索引结果消息。
  const results = new Map<string, Message>()
  for (const m of messages) {
    if (m.role === 'tool' && m.tool_call_id) {
      results.set(m.tool_call_id, m)
    }
  }

  for (let i = 0; i < messages.length; i++) {
    const m = messages[i]
    if (m.role !== 'assistant' || !m.tool_calls?.length) continue

    // 极端情况下整轮都没有 assistant（历史残缺）时挂到自身，
    // 这样卡片至少还能靠 ChatView 的兜底渲染出来。
    const hostId = hosts[i] || m.id
    for (const tc of m.tool_calls) {
      const res = results.get(tc.id)
      let status: ToolCallView['status'] = 'success'
      if (!res) {
        // 没有 tool 结果有两种情况，必须区分：
        //
        //  1. 该调用正在等用户回答（ask_user 挂起）→ running
        //     挂起时 assistant(tool_calls) 已经落库，但 tool 结果要等
        //     用户回答后才补。若一律判 error，用户一重开软件就看到
        //     「ask_user 失败」，会以为提问根本没送达。
        //  2. 真的丢了结果（执行崩溃/历史残缺）→ error
        status = pendingCallIds?.has(tc.id) ? 'running' : 'error'
      } else if (res.status === 'error') {
        status = 'error'
      } else if (res.status === 'interrupted') {
        status = 'rejected'
      }
      out[tc.id] = {
        call_id: tc.id,
        message_id: hostId,
        name: tc.name,
        args: tc.args,
        risk: 'safe',
        requires_approval: false,
        status,
        output: res?.content ?? '',
      }
    }
  }

  // 兜底：只有结果、没有配对调用的（历史可能残缺），
  // 同样挂到所属轮次的宿主上，避免结果凭空消失。
  for (let i = 0; i < messages.length; i++) {
    const m = messages[i]
    if (m.role !== 'tool' || !m.tool_call_id) continue
    if (out[m.tool_call_id]) continue
    out[m.tool_call_id] = {
      call_id: m.tool_call_id,
      message_id: hosts[i] || '',
      name: '工具',
      args: '',
      risk: 'safe',
      requires_approval: false,
      status: m.status === 'error' ? 'error' : 'success',
      output: m.content,
    }
  }

  return out
}
