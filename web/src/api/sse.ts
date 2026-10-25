/* 手写 SSE 客户端：用 fetch + ReadableStream 而非 EventSource。
 *
 * 为什么不用 EventSource：
 *   - 只支持 GET，无法携带请求体（消息内容）
 *   - 无法设置自定义头（幂等键、鉴权）
 *   - 无法取消（需要 AbortController 实现「停止生成」）
 *
 * 解析严格遵循 WHATWG 规范：按空行切帧，逐行读 field: value。
 */
import type { SseEvent } from './types'

export interface StreamHandlers {
  onEvent: (ev: SseEvent) => void
  onError?: (err: Error) => void
  onClose?: (reason: 'done' | 'abort' | 'error') => void
}

export interface StreamOptions {
  signal?: AbortSignal
  /** 断线重连基线，作为 last_event_id 查询参数 */
  lastEventId?: number
}

/** POST 一个 JSON 请求体并按 SSE 消费响应流。返回可 await 的 Promise（流结束时 resolve）。 */
export async function streamPost(
  url: string,
  body: unknown,
  handlers: StreamHandlers,
  options: StreamOptions = {},
): Promise<void> {
  let res: Response
  try {
    res = await fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream',
      },
      body: JSON.stringify(body),
      signal: options.signal,
    })
  } catch (err) {
    if (isAbort(err)) {
      handlers.onClose?.('abort')
      return
    }
    handlers.onError?.(toError(err))
    handlers.onClose?.('error')
    return
  }

  // 建流之前的错误是普通 JSON（404/409/503），需要单独处理。
  if (!res.ok) {
    const detail = await readErrorBody(res)
    handlers.onError?.(detail)
    handlers.onClose?.('error')
    return
  }
  if (!res.body) {
    handlers.onError?.(new Error('响应没有可读流（可能是不支持的浏览器）'))
    handlers.onClose?.('error')
    return
  }

  try {
    await consume(res.body, handlers)
    handlers.onClose?.('done')
  } catch (err) {
    if (isAbort(err)) {
      handlers.onClose?.('abort')
      return
    }
    handlers.onError?.(toError(err))
    handlers.onClose?.('error')
  }
}

/** GET 订阅流（断线补齐场景）。 */
export async function streamGet(
  url: string,
  handlers: StreamHandlers,
  options: StreamOptions = {},
): Promise<void> {
  const qs = options.lastEventId ? `?last_event_id=${options.lastEventId}` : ''
  let res: Response
  try {
    res = await fetch(`${url}${qs}`, {
      headers: { Accept: 'text/event-stream' },
      signal: options.signal,
    })
  } catch (err) {
    if (isAbort(err)) {
      handlers.onClose?.('abort')
      return
    }
    handlers.onError?.(toError(err))
    handlers.onClose?.('error')
    return
  }
  if (!res.ok || !res.body) {
    handlers.onError?.(await readErrorBody(res))
    handlers.onClose?.('error')
    return
  }
  try {
    await consume(res.body, handlers)
    handlers.onClose?.('done')
  } catch (err) {
    if (isAbort(err)) {
      handlers.onClose?.('abort')
      return
    }
    handlers.onError?.(toError(err))
    handlers.onClose?.('error')
  }
}

/* ---------------- 内部实现 ---------------- */

async function consume(
  body: ReadableStream<Uint8Array>,
  handlers: StreamHandlers,
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder('utf-8')
  let buffer = ''

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })

      // 帧之间用空行分隔；兼容 \r\n
      let idx: number
      while ((idx = findFrameEnd(buffer)) !== -1) {
        const rawFrame = buffer.slice(0, idx)
        buffer = buffer.slice(idx).replace(/^(\r?\n){2}/, '')
        const ev = parseFrame(rawFrame)
        if (ev) handlers.onEvent(ev)
      }
    }
  } finally {
    try {
      await reader.cancel()
    } catch {
      /* 忽略取消失败 */
    }
  }
}

/** 找到 "\n\n" 或 "\r\n\r\n" 的起始位置 */
function findFrameEnd(buf: string): number {
  const a = buf.indexOf('\n\n')
  const b = buf.indexOf('\r\n\r\n')
  if (a === -1) return b
  if (b === -1) return a
  return Math.min(a, b)
}

/** 解析单帧；注释帧或空帧返回 null */
function parseFrame(raw: string): SseEvent | null {
  let id: number | null = null
  let event = ''
  const dataLines: string[] = []

  for (const line of raw.split(/\r?\n/)) {
    if (line === '' || line.startsWith(':')) continue // 注释（心跳探活）
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    // 规范允许 "data:value"，需去掉冒号后的单个空格
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    switch (field) {
      case 'id': {
        const n = Number.parseInt(value, 10)
        if (!Number.isNaN(n)) id = n
        break
      }
      case 'event':
        event = value
        break
      case 'data':
        dataLines.push(value)
        break
      case 'retry':
        // 后端建议的重连间隔，本期由 UI 固定策略处理
        break
      default:
        break
    }
  }

  if (!event && dataLines.length === 0) return null
  const rawData = dataLines.join('\n')
  let data: unknown = rawData
  if (rawData) {
    try {
      data = JSON.parse(rawData)
    } catch {
      /* 非 JSON 负载保留原始字符串 */
    }
  }
  return { id, event, data }
}

async function readErrorBody(res: Response): Promise<Error> {
  try {
    const text = await res.text()
    const parsed = JSON.parse(text) as { error?: { code?: string; message?: string } }
    return new Error(
      `${parsed.error?.message ?? text}（HTTP ${res.status}${parsed.error?.code ? ` · ${parsed.error.code}` : ''}）`,
    )
  } catch {
    return new Error(`请求失败（HTTP ${res.status}）`)
  }
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError'
}

function toError(err: unknown): Error {
  return err instanceof Error ? err : new Error(String(err))
}
