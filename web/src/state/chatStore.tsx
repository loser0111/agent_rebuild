import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  type ReactNode,
} from 'react'
import { api, ApiRequestError } from '../api/client'
import { streamPost } from '../api/sse'
import type {
  HealthResponse,
  Message,
  MessageDoneData,
  MessageDeltaData,
  MessageErrorData,
  MessageInterruptedData,
  MessageStartData,
  ResyncData,
  Session,
  SseEvent,
} from '../api/types'

/** 连接与生成阶段，驱动底部状态栏与输入框禁用逻辑。 */
export type StreamPhase = 'idle' | 'connecting' | 'streaming' | 'reconnecting' | 'error'

export interface ChatState {
  health: HealthResponse | null
  backendOnline: boolean
  sessions: Session[]
  sessionsLoading: boolean
  currentId: string | null
  messages: Message[]
  detailLoading: boolean
  phase: StreamPhase
  /** 正在生成的 assistant 消息 id（服务端权威 id） */
  streamingMessageId: string | null
  startedAt: number | null
  error: string | null
  reconnectHint: string | null
  lastEventId: number
}

const initialState: ChatState = {
  health: null,
  backendOnline: false,
  sessions: [],
  sessionsLoading: true,
  currentId: null,
  messages: [],
  detailLoading: false,
  phase: 'idle',
  streamingMessageId: null,
  startedAt: null,
  error: null,
  reconnectHint: null,
  lastEventId: 0,
}

type Action =
  | { type: 'health/ok'; payload: HealthResponse }
  | { type: 'health/fail' }
  | { type: 'sessions/loading' }
  | { type: 'sessions/loaded'; payload: Session[] }
  | { type: 'sessions/upsert'; payload: Session }
  | { type: 'sessions/patch'; payload: { id: string; patch: Partial<Session> } }
  | { type: 'sessions/remove'; payload: string }
  | { type: 'current/set'; payload: string | null }
  | { type: 'messages/loading' }
  | { type: 'messages/loaded'; payload: { messages: Message[]; lastEventId: number } }
  | { type: 'messages/append'; payload: Message[] }
  | { type: 'messages/rename'; payload: { from: string; to: string; patch?: Partial<Message> } }
  | { type: 'messages/patch'; payload: { id: string; patch: Partial<Message> } }
  | { type: 'messages/appendDelta'; payload: { id: string; delta: string } }
  | { type: 'messages/replaceAll'; payload: Message[] }
  | { type: 'stream/begin'; payload: { messageId: string } }
  | { type: 'stream/target'; payload: string }
  | { type: 'stream/phase'; payload: StreamPhase }
  | { type: 'stream/event'; payload: number }
  | { type: 'stream/end' }
  | { type: 'error/set'; payload: string | null }
  | { type: 'reconnect/hint'; payload: string | null }

function reducer(state: ChatState, action: Action): ChatState {
  switch (action.type) {
    case 'health/ok':
      return { ...state, health: action.payload, backendOnline: true }

    case 'health/fail':
      return { ...state, backendOnline: false }

    case 'sessions/loading':
      return { ...state, sessionsLoading: true }

    case 'sessions/loaded':
      return { ...state, sessions: action.payload, sessionsLoading: false }

    case 'sessions/upsert': {
      const rest = state.sessions.filter((s) => s.id !== action.payload.id)
      return { ...state, sessions: [action.payload, ...rest] }
    }

    case 'sessions/patch':
      return {
        ...state,
        sessions: state.sessions.map((s) =>
          s.id === action.payload.id ? { ...s, ...action.payload.patch } : s,
        ),
      }

    case 'sessions/remove':
      return {
        ...state,
        sessions: state.sessions.filter((s) => s.id !== action.payload),
        currentId: state.currentId === action.payload ? null : state.currentId,
        messages: state.currentId === action.payload ? [] : state.messages,
      }

    case 'current/set':
      return {
        ...state,
        currentId: action.payload,
        messages: [],
        error: null,
        reconnectHint: null,
        lastEventId: 0,
      }

    case 'messages/loading':
      return { ...state, detailLoading: true }

    case 'messages/loaded':
      return {
        ...state,
        messages: action.payload.messages,
        lastEventId: action.payload.lastEventId,
        detailLoading: false,
      }

    case 'messages/append':
      return { ...state, messages: [...state.messages, ...action.payload] }

    case 'messages/rename':
      return {
        ...state,
        messages: state.messages.map((m) =>
          m.id === action.payload.from
            ? { ...m, ...(action.payload.patch ?? {}), id: action.payload.to }
            : m,
        ),
        streamingMessageId:
          state.streamingMessageId === action.payload.from
            ? action.payload.to
            : state.streamingMessageId,
      }

    case 'messages/patch':
      return {
        ...state,
        messages: state.messages.map((m) =>
          m.id === action.payload.id ? { ...m, ...action.payload.patch } : m,
        ),
      }

    case 'messages/appendDelta':
      return {
        ...state,
        messages: state.messages.map((m) =>
          m.id === action.payload.id ? { ...m, content: m.content + action.payload.delta } : m,
        ),
      }

    case 'messages/replaceAll':
      return { ...state, messages: action.payload }

    case 'stream/begin':
      return {
        ...state,
        phase: 'connecting',
        streamingMessageId: action.payload.messageId,
        startedAt: Date.now(),
        error: null,
        reconnectHint: null,
      }

    case 'stream/target':
      return { ...state, streamingMessageId: action.payload }

    case 'stream/phase':
      return { ...state, phase: action.payload }

    case 'stream/event':
      return { ...state, lastEventId: action.payload }

    case 'stream/end':
      return { ...state, phase: 'idle', streamingMessageId: null, startedAt: null }

    case 'error/set':
      return {
        ...state,
        error: action.payload,
        phase: action.payload ? 'error' : state.phase === 'error' ? 'idle' : state.phase,
      }

    case 'reconnect/hint':
      return {
        ...state,
        reconnectHint: action.payload,
        phase: action.payload ? 'reconnecting' : state.phase,
      }

    default:
      return state
  }
}

interface ChatContextValue extends ChatState {
  isStreaming: boolean
  /** 当前会话选用的模型配置 id（空表示用全局默认） */
  activeModelConfigId: string
  createSession: () => Promise<void>
  selectSession: (id: string) => Promise<void>
  deleteSession: (id: string) => Promise<void>
  sendMessage: (content: string) => Promise<void>
  stopGeneration: () => Promise<void>
  resync: () => Promise<void>
  dismissError: () => void
  /** 切换当前会话使用的模型配置（立即持久化到服务端） */
  setActiveModel: (modelConfigId: string) => Promise<void>
}

const ChatContext = createContext<ChatContextValue | null>(null)

/** 乐观渲染用的本地消息，id 带 tmp_ 前缀，收到 message.start 后改为服务端 id。 */
function tempMessage(role: Message['role'], sessionId: string, content: string): Message {
  const now = new Date().toISOString()
  return {
    id: `tmp_${Math.random().toString(36).slice(2, 10)}`,
    session_id: sessionId,
    role,
    content,
    status: role === 'assistant' ? 'pending' : 'complete',
    created_at: now,
    updated_at: now,
  }
}

export function ChatProvider({ children }: { children: ReactNode }) {
  const [state, dispatch] = useReducer(reducer, initialState)

  const abortRef = useRef<AbortController | null>(null)
  /** 是否已收到终止事件（done/error/interrupted），用于区分正常结束与连接中断。 */
  const settledRef = useRef(false)
  const currentIdRef = useRef<string | null>(null)
  currentIdRef.current = state.currentId
  const messagesRef = useRef<Message[]>([])
  messagesRef.current = state.messages
  const streamingRef = useRef<string | null>(null)
  streamingRef.current = state.streamingMessageId

  /** 拉取会话详情（同时是断线后的兜底补齐路径）。 */
  const loadDetail = useCallback(async (id: string) => {
    dispatch({ type: 'current/set', payload: id })
    dispatch({ type: 'messages/loading' })
    try {
      const detail = await api.getSession(id)
      dispatch({
        type: 'messages/loaded',
        payload: { messages: detail.messages, lastEventId: detail.last_event_id },
      })
      if (detail.session.status === 'running') {
        dispatch({
          type: 'reconnect/hint',
          payload: '服务端显示该会话仍在生成中，可稍后点「重新同步」拉取最新内容',
        })
      }
    } catch (err) {
      dispatch({ type: 'messages/loaded', payload: { messages: [], lastEventId: 0 } })
      dispatch({ type: 'error/set', payload: describeError(err) })
    }
  }, [])

  /** 初始化：健康检查 + 会话列表（自动选中最近一个）。 */
  const bootstrap = useCallback(async () => {
    try {
      const health = await api.health()
      dispatch({ type: 'health/ok', payload: health })
    } catch {
      dispatch({ type: 'health/fail' })
      dispatch({ type: 'sessions/loaded', payload: [] })
      dispatch({
        type: 'error/set',
        payload: '无法连接后端服务，请确认已运行 go run ./cmd/server（默认 :8080）',
      })
      return
    }

    try {
      dispatch({ type: 'sessions/loading' })
      const { sessions } = await api.listSessions()
      dispatch({ type: 'sessions/loaded', payload: sessions })
      if (sessions.length > 0 && !currentIdRef.current) {
        await loadDetail(sessions[0].id)
      }
    } catch (err) {
      dispatch({ type: 'sessions/loaded', payload: [] })
      dispatch({ type: 'error/set', payload: describeError(err) })
    }
  }, [loadDetail])

  useEffect(() => {
    void bootstrap()
  }, [bootstrap])

  const createSession = useCallback(async () => {
    try {
      const { session } = await api.createSession({})
      dispatch({ type: 'sessions/upsert', payload: session })
      dispatch({ type: 'current/set', payload: session.id })
      dispatch({ type: 'messages/loaded', payload: { messages: [], lastEventId: 0 } })
    } catch (err) {
      dispatch({ type: 'error/set', payload: describeError(err) })
    }
  }, [])

  const selectSession = useCallback(
    async (id: string) => {
      if (id === currentIdRef.current) return
      if (streamingRef.current) {
        dispatch({ type: 'error/set', payload: '正在生成中，请先停止再切换会话' })
        return
      }
      await loadDetail(id)
    },
    [loadDetail],
  )

  const deleteSession = useCallback(
    async (id: string) => {
      try {
        await api.deleteSession(id)
        dispatch({ type: 'sessions/remove', payload: id })
        if (currentIdRef.current === id) {
          const rest = state.sessions.filter((s) => s.id !== id)
          if (rest.length > 0) await loadDetail(rest[0].id)
        }
      } catch (err) {
        dispatch({ type: 'error/set', payload: describeError(err) })
      }
    },
    [loadDetail, state.sessions],
  )

  /** 单个 SSE 事件 → 状态更新。 */
  const handleEvent = useCallback((ev: SseEvent) => {
    if (ev.id !== null) dispatch({ type: 'stream/event', payload: ev.id })

    switch (ev.event) {
      case 'message.delta': {
        const d = ev.data as MessageDeltaData
        dispatch({ type: 'stream/phase', payload: 'streaming' })
        dispatch({ type: 'messages/appendDelta', payload: { id: d.message_id, delta: d.delta } })
        break
      }
      case 'message.done': {
        const d = ev.data as MessageDoneData
        settledRef.current = true
        dispatch({
          type: 'messages/patch',
          payload: {
            id: d.message_id,
            patch: { content: d.content, status: 'complete', usage: d.usage ?? null },
          },
        })
        break
      }
      case 'message.error': {
        const d = ev.data as MessageErrorData
        settledRef.current = true
        dispatch({
          type: 'messages/patch',
          payload: { id: d.message_id, patch: { status: 'error', error: d.error } },
        })
        dispatch({ type: 'error/set', payload: d.error?.message ?? '生成失败' })
        break
      }
      case 'message.interrupted': {
        const d = ev.data as MessageInterruptedData
        settledRef.current = true
        dispatch({
          type: 'messages/patch',
          payload: { id: d.message_id, patch: { content: d.content, status: 'interrupted' } },
        })
        break
      }
      case 'resync': {
        const d = ev.data as ResyncData
        dispatch({ type: 'messages/replaceAll', payload: d.messages ?? [] })
        dispatch({ type: 'reconnect/hint', payload: null })
        break
      }
      case 'error': {
        settledRef.current = true
        const payload = ev.data as { error?: { message?: string } }
        dispatch({ type: 'error/set', payload: payload.error?.message ?? '流式错误' })
        break
      }
      case 'ping':
        break
      default:
        // 协议约定：未知事件忽略并记录，不得报错。
        console.debug('[sse] 忽略未知事件', ev.event)
        break
    }
  }, [])

  const sendMessage = useCallback(
    async (content: string) => {
      const sessionId = currentIdRef.current
      if (!sessionId) {
        dispatch({ type: 'error/set', payload: '请先新建或选择一个会话' })
        return
      }
      if (streamingRef.current) return

      const userMsg = tempMessage('user', sessionId, content)
      const assistantMsg = tempMessage('assistant', sessionId, '')
      const tempAssistantId = assistantMsg.id

      settledRef.current = false
      dispatch({ type: 'messages/append', payload: [userMsg, assistantMsg] })
      dispatch({ type: 'stream/begin', payload: { messageId: tempAssistantId } })

      const controller = new AbortController()
      abortRef.current = controller
      // 服务端分配的 assistant 消息 id（message.start 到达后填入），
      // 用于流结束时对「仍停在 pending 的临时消息」做兜底收尾。
      let realAssistantId: string | null = null

      // 带上当前会话选用的模型配置，确保「输入框下方切换的模型」真正生效。
      const modelConfigId =
        state.sessions.find((s) => s.id === sessionId)?.model_config_id ?? ''

      await streamPost(
        `/api/sessions/${sessionId}/messages`,
        modelConfigId ? { content, model_config_id: modelConfigId } : { content },
        {
          onEvent: (ev) => {
            if (ev.event === 'message.start') {
              const d = ev.data as MessageStartData
              realAssistantId = d.message_id
              if (ev.id !== null) dispatch({ type: 'stream/event', payload: ev.id })
              // 把临时消息的 id 换成服务端权威 id，后续 delta 就能对上。
              dispatch({
                type: 'messages/rename',
                payload: { from: tempAssistantId, to: d.message_id, patch: { status: 'streaming' } },
              })
              dispatch({ type: 'stream/phase', payload: 'streaming' })
              return
            }
            handleEvent(ev)
          },
          onError: (err) => {
            dispatch({ type: 'error/set', payload: err.message })
          },
        },
        { signal: controller.signal },
      )

      abortRef.current = null
      if (!settledRef.current && !controller.signal.aborted) {
        dispatch({
          type: 'reconnect/hint',
          payload: '连接中断，回答可能不完整。点「重新同步」拉取服务端最新状态',
        })
      } else {
        dispatch({ type: 'reconnect/hint', payload: null })
      }
      // 兜底收尾：若流已结束但消息仍停留在 pending/streaming（例如建流前
      // 就失败、或首个事件即错误），标为中断，避免界面永远显示「生成中」。
      const leftoverId = realAssistantId ?? tempAssistantId
      const leftover = messagesRef.current.find((m) => m.id === leftoverId)
      if (leftover && (leftover.status === 'pending' || leftover.status === 'streaming')) {
        dispatch({
          type: 'messages/patch',
          payload: {
            id: leftoverId,
            patch: {
              status: controller.signal.aborted ? 'interrupted' : leftover.status,
              error: controller.signal.aborted
                ? null
                : { code: 'stream_aborted', message: '连接中断，回答可能不完整' },
            },
          },
        })
      }
      dispatch({ type: 'stream/end' })

      // 刷新会话列表（标题、更新时间、状态都可能变化）。
      try {
        const { sessions } = await api.listSessions()
        dispatch({ type: 'sessions/loaded', payload: sessions })
      } catch {
        /* 列表刷新失败不影响主流程 */
      }
    },
    [handleEvent, state.sessions],
  )

  const stopGeneration = useCallback(async () => {
    const sessionId = currentIdRef.current
    abortRef.current?.abort()
    if (!sessionId) return
    try {
      await api.cancel(sessionId)
    } catch {
      /* 客户端已 abort，服务端取消失败不阻塞 UI */
    }
  }, [])

  const resync = useCallback(async () => {
    dispatch({ type: 'reconnect/hint', payload: null })
    dispatch({ type: 'error/set', payload: null })
    if (currentIdRef.current) {
      await loadDetail(currentIdRef.current)
    } else {
      await bootstrap()
    }
  }, [bootstrap, loadDetail])

  const dismissError = useCallback(() => dispatch({ type: 'error/set', payload: null }), [])

  /** 当前会话已持久化的模型选择；会话没选过则为空串。 */
  const activeModelConfigId = useMemo(() => {
    const cur = state.sessions.find((s) => s.id === state.currentId)
    return cur?.model_config_id ?? ''
  }, [state.sessions, state.currentId])

  /**
   * 切换当前会话使用的模型。
   *
   * 立即写服务端而不是等到下次发送：
   *   - 用户切完再刷新页面，选择还在
   *   - 发送时请求里也会带上 id，双保险
   */
  const setActiveModel = useCallback(
    async (modelConfigId: string) => {
      const sessionId = currentIdRef.current
      if (!sessionId) return
      if (streamingRef.current) {
        dispatch({ type: 'error/set', payload: '正在生成中，请先停止再切换模型' })
        return
      }
      // 乐观更新，失败再回滚。
      const before = state.sessions
      dispatch({
        type: 'sessions/patch',
        payload: { id: sessionId, patch: { model_config_id: modelConfigId } },
      })
      try {
        const { session } = await api.setSessionModel(sessionId, modelConfigId)
        dispatch({ type: 'sessions/upsert', payload: session })
      } catch (err) {
        dispatch({ type: 'sessions/loaded', payload: before })
        dispatch({ type: 'error/set', payload: describeError(err) })
      }
    },
    [state.sessions],
  )

  const value = useMemo<ChatContextValue>(
    () => ({
      ...state,
      isStreaming: Boolean(state.streamingMessageId),
      activeModelConfigId,
      createSession,
      selectSession,
      deleteSession,
      sendMessage,
      stopGeneration,
      resync,
      dismissError,
      setActiveModel,
    }),
    [
      state,
      activeModelConfigId,
      createSession,
      selectSession,
      deleteSession,
      sendMessage,
      stopGeneration,
      resync,
      dismissError,
      setActiveModel,
    ],
  )

  return <ChatContext.Provider value={value}>{children}</ChatContext.Provider>
}

function describeError(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.code === 'session_busy') return '该会话正在生成中，请稍候或先点「停止」'
    return err.message
  }
  if (err instanceof Error) return err.message
  return String(err)
}

export function useChat(): ChatContextValue {
  const ctx = useContext(ChatContext)
  if (!ctx) throw new Error('useChat 必须在 ChatProvider 内使用')
  return ctx
}
