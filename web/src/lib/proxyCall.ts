/**
 * 代理工具 search_router 的调用解析（见 docs/17-mcp-client.md 12.5、第 8 节）。
 *
 * 自定义工具（MCP）不直接暴露给模型，模型通过 search_router 的 execute 代理执行。
 * 卡片和审批里如果只显示「search_router」加一串嵌套 JSON，用户无法判断到底要执行什么，
 * 所以这里把它展开成「真实工具名 + 真实参数」。纯前端解析，历史消息、刷新恢复的
 * 审批卡片都能用，不依赖后端额外下发字段。
 */

export const ROUTER_TOOL_NAME = 'search_router'

export interface ProxyCallInfo {
  /** list / search / execute；无法解析时为空串 */
  action: string
  /** 一句话说明，例如「代理执行 github/create_issue」「搜索自定义工具：issue」 */
  label: string
  /** execute 的目标工具全名 */
  target?: string
  /** 要展示的参数 JSON（execute 时是目标工具的 arguments） */
  args: string
}

/** 不是 search_router 的调用返回 null。 */
export function parseProxyCall(name: string, rawArgs: string): ProxyCallInfo | null {
  if (name !== ROUTER_TOOL_NAME) return null
  let a: Record<string, unknown> = {}
  try {
    const parsed: unknown = JSON.parse(rawArgs || '{}')
    if (parsed && typeof parsed === 'object') a = parsed as Record<string, unknown>
  } catch {
    // 流式中途参数可能不完整：退回原样展示
    return { action: '', label: '自定义工具代理', args: rawArgs }
  }
  const action = typeof a.action === 'string' ? a.action : ''
  switch (action) {
    case 'execute': {
      const target = typeof a.tool === 'string' && a.tool ? a.tool : '（未指定工具）'
      let inner: unknown = a.arguments ?? {}
      // 后端兼容模型把 arguments 写成 JSON 字符串，展示时同样展开
      if (typeof inner === 'string') {
        try {
          inner = JSON.parse(inner)
        } catch {
          /* 保留字符串 */
        }
      }
      return { action, label: `代理执行 ${target}`, target, args: JSON.stringify(inner, null, 2) }
    }
    case 'search':
      return { action, label: `搜索自定义工具：${String(a.query ?? '')}`, args: rawArgs }
    case 'list':
      return {
        action,
        label: a.server ? `浏览自定义工具：${String(a.server)}` : '浏览自定义工具',
        args: rawArgs,
      }
    default:
      return { action, label: '自定义工具代理', args: rawArgs }
  }
}
