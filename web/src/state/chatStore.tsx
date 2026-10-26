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
import { rebuildToolCalls } from './toolRestore'
import type {
  AwaitingInputData,
  HealthResponse,
  Message,
  MessageDoneData,
  MessageDeltaData,
  MessageErrorData,
  MessageInterruptedData,
  MessageStartData,
  PendingQuestion,
  ResyncData,
  Session,
  SseEvent,
  ToolCallData,
  ToolCallView,
  ToolResultData,
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
  /** 当前会话的工具调用记录，按 call_id 索引 */
  toolCalls: Record<string, ToolCallView>
  detailLoading: boolean
  phase: StreamPhase
  /** 正在生成的 assistant 消息 id（服务端权威 id） */
  streamingMessageId: string | null
  startedAt: number | null
  error: string | null
  reconnectHint: string | null
  lastEventId: number
  /**
   * 挂起中的提问（ask_user）。非空时输入框应切换成问答卡片，
   * 用户回答后走 resume 继续生成。
   *
   * 用列表而不是单个：同一轮里模型可能一次问多个问题
   * （「你到哪一步了？」「要不要我改代码？」）。早期实现只保留
   * 第一个，后面的问题在界面上直接消失。
   */
  pendingQuestions: PendingQuestion[]
  /**
   * 用户正在填写的答案（call_id → 文本）。
   *
   * 为什么放在 store 而不是 AskUserCard 的局部 state：
   * 同一份答案要有两个入口——卡片里的选项按钮/文本框，以及
   * 底部主输入框。放在组件内部时 Composer 读不到，用户就没法
   * 用最顺手的输入框答题（只能去卡片里的小输入区）。
   */
  draftAnswers: Record<string, string>
}

const initialState: ChatState = {
  health: null,
  backendOnline: false,
  sessions: [],
  sessionsLoading: true,
  currentId: null,
  messages: [],
  toolCalls: {},
  detailLoading: false,
  phase: 'idle',
  streamingMessageId: null,
  startedAt: null,
  error: null,
  reconnectHint: null,
  lastEventId: 0,
  pendingQuestions: [],
  draftAnswers: {},
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
  | { type: 'tools/call'; payload: ToolCallView }
  | { type: 'tools/result'; payload: { call_id: string; patch: Partial<ToolCallView> } }
  | { type: 'tools/restore'; payload: Record<string, ToolCallView> }
  | { type: 'tools/reset' }
  | { type: 'pending/set'; payload: PendingQuestion[] }
  | { type: 'pending/append'; payload: PendingQuestion }
  | { type: 'draft/set'; payload: { callId: string; value: string } }
  | { type: 'draft/reset' }
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
        toolCalls: {},
        // 挂起状态属于「上一个会话」，切会话必须清掉，
        // 否则会把 A 会话的问题显示在 B 会话的输入区。
        pendingQuestions: [],
        draftAnswers: {},
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

    case 'tools/call':
      return {
        ...state,
        toolCalls: { ...state.toolCalls, [action.payload.call_id]: action.payload },
      }

    case 'tools/result': {
      const prev = state.toolCalls[action.payload.call_id]
      // 结果先于调用事件到达时（重连补齐等）也保留结果，避免丢信息。
      const base: ToolCallView = prev ?? {
        call_id: action.payload.call_id,
        message_id: '',
        name: '',
        args: '',
        risk: 'safe',
        requires_approval: false,
        status: 'running',
      }
      return {
        ...state,
        toolCalls: {
          ...state.toolCalls,
          [action.payload.call_id]: { ...base, ...action.payload.patch },
        },
      }
    }

    case 'tools/restore':
      return { ...state, toolCalls: action.payload }

    case 'tools/reset':
      return { ...state, toolCalls: {} }

    case 'pending/set':
      return { ...state, pendingQuestions: action.payload }

    case 'pending/append': {
      // 同 key 重复到达时替换而非追加，避免重连重放产生重复卡片。
      // 用 key（call_id 或 call_id::qid）而不是 call_id：
      // 一次调用里的多个问题共享 call_id，只有 key 能区分它们。
      const rest = state.pendingQuestions.filter((q) => q.key !== action.payload.key)
      return { ...state, pendingQuestions: [...rest, action.payload] }
    }

    case 'draft/set':
      return {
        ...state,
        draftAnswers: { ...state.draftAnswers, [action.payload.callId]: action.payload.value },
      }

    case 'draft/reset':
      return { ...state, draftAnswers: {} }

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
  /** 回答挂起中的提问（ask_user）并继续生成；answers 以 call_id 为键。 */
  resume: (answers: Record<string, string>) => Promise<void>
  /** 更新某个问题的草稿答案（卡片与输入框共用） */
  setDraftAnswer: (callId: string, value: string) => void
  /** 清空所有草稿 */
  resetDrafts: () => void
  /** 重新查询是否有挂起中的提问 */
  refreshPending: () => Promise<void>
  dismissError: () => void
  /** 切换当前会话使用的模型配置（立即持久化到服务端） */
  setActiveModel: (modelConfigId: string) => Promise<void>
}

const ChatContext = createContext<ChatContextValue | null>(null)

/**
 * 把前端持有的问题列表转成 reducer 状态。
 *
 * 单独抽出来是因为「同一轮多个问题」需要稳定的顺序：
 * 后端按 Pendings 顺序下发，展示顺序应与模型提问顺序一致，
 * 否则用户会觉得第二个问题「跳」到了前面。
 */
function toPendingState(list: PendingQuestion[]): PendingQuestion[] {
  return list.filter((q) => q.callId)
}

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

  /**
   * 从后端取「挂起中的提问」列表。
   *
   * 为什么不能只靠事件：awaiting.input 事件只存在于当次事件流里，
   * 页面刷新、切走再切回、甚至进程重启后都拿不到。而用户最需要的
   * 恰恰是「关掉窗口明天再回答」这种场景，所以必须能主动查一次。
   *
   * 返回数组而不是单个：同一轮里模型可能一次问多个问题。
   */
  const fetchPending = useCallback(async (id: string): Promise<PendingQuestion[]> => {
    try {
      const cp = await api.getCheckpoint(id)
      if (!cp.suspended) return []
      // 新接口返回 questions 列表；旧后端只给单个 call_id，做一次兼容。
      if (cp.questions?.length) {
        return cp.questions.map((q) => ({
          key: q.key || (q.qid ? `${q.call_id}::${q.qid}` : q.call_id),
          callId: q.call_id,
          qid: q.qid ?? '',
          messageId: '',
          question: q.question ?? '',
          options: q.options ?? [],
          multi: q.multi ?? false,
        }))
      }
      if (!cp.call_id) return []
      return [
        {
          key: cp.call_id,
          callId: cp.call_id,
          qid: '',
          messageId: '',
          question: cp.question ?? '',
          options: cp.options ?? [],
          multi: false,
        },
      ]
    } catch {
      // 查不到就当作没有挂起：不能因为恢复失败把正常会话也卡住。
      return []
    }
  }, [])

  /** 重新同步挂起态（供「重新同步」等入口调用）。 */
  const restorePending = useCallback(
    async (id: string) => {
      const list = await fetchPending(id)
      dispatch({ type: 'pending/set', payload: toPendingState(list) })
    },
    [fetchPending],
  )

  /** 拉取会话详情（同时是断线后的兜底补齐路径）。 */
  const loadDetail = useCallback(async (id: string) => {
    dispatch({ type: 'current/set', payload: id })
    dispatch({ type: 'messages/loading' })
    try {
      const detail = await api.getSession(id)

      // 先查一次挂起状态：还原工具卡片时需要用它区分
      // 「正在等回答的调用」与「真的丢了结果的调用」。
      // 挂起时 assistant(tool_calls) 已落库但 tool 结果还没有，
      // 若不区分就会把待回答的 ask_user 显示成「失败」。
      let pending: PendingQuestion[] = []
      if (detail.session.status === 'suspended') {
        pending = await fetchPending(id)
      }
      const pendingIds = new Set(pending.map((p) => p.callId))

      // 历史里的工具调用要先还原成卡片，否则会以纯文本形式铺在对话里。
      const restored = rebuildToolCalls(detail.messages, pendingIds)
      dispatch({ type: 'tools/restore', payload: restored })
      dispatch({
        type: 'messages/loaded',
        payload: { messages: detail.messages, lastEventId: detail.last_event_id },
      })
      dispatch({
        type: 'pending/set',
        payload: toPendingState(pending),
      })
      if (detail.session.status === 'running') {
        // 后端标记为 running，但本端没有正在进行的流：
        // 通常是上次进程异常退出留下的残留状态（后端重启后会自动复位，
        // 若仍出现说明是新一轮异常）。提示用户可尝试重置。
        dispatch({
          type: 'reconnect/hint',
          payload: '该会话被标记为「生成中」，但当前没有正在进行的回答。可尝试重置状态',
        })
      }
    } catch (err) {
      dispatch({ type: 'messages/loaded', payload: { messages: [], lastEventId: 0 } })
      dispatch({ type: 'error/set', payload: describeError(err) })
    }
  }, [fetchPending])

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
      case 'tool.call': {
        const d = ev.data as ToolCallData
        dispatch({
          type: 'tools/call',
          payload: {
            call_id: d.call_id,
            message_id: d.message_id,
            name: d.name,
            args: d.args,
            risk: d.risk,
            requires_approval: d.requires_approval,
            status: 'running',
          },
        })
        break
      }
      case 'tool.result': {
        const d = ev.data as ToolResultData
        dispatch({
          type: 'tools/result',
          payload: {
            call_id: d.call_id,
            patch: {
              name: d.name,
              output: d.output,
              duration_ms: d.duration_ms,
              status: d.rejected ? 'rejected' : d.is_error ? 'error' : 'success',
            },
          },
        })
        break
      }
      case 'resync': {
        const d = ev.data as ResyncData
        dispatch({ type: 'messages/replaceAll', payload: d.messages ?? [] })
        dispatch({ type: 'reconnect/hint', payload: null })
        break
      }
      case 'awaiting.input': {
        // 模型提问后挂起：本轮没有最终回答，输入区改为问答卡片。
        //
        // 同一轮可能有多个问题，后端逐个发事件。这里**追加**到列表，
        // 而不是覆盖——覆盖会让前几个问题一闪而过，用户只看到最后一个。
        const d = ev.data as AwaitingInputData
        settledRef.current = true
        dispatch({ type: 'pending/append', payload: {
          key: d.qid ? `${d.call_id}::${d.qid}` : d.call_id,
          callId: d.call_id,
          qid: d.qid ?? '',
          messageId: d.message_id,
          question: d.question,
          options: d.options ?? [],
          multi: d.multi ?? false,
        } })
        // 占位消息不该继续显示「生成中」的打字动画。
        dispatch({
          type: 'messages/patch',
          payload: { id: d.message_id, patch: { status: 'streaming' } },
        })
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
      // 注意：这里**不能**清空 toolCalls。
      // 卡片是按 message_id 归属到具体某条 assistant 消息的，天然隔离；
      // 早期版本在每次发送前 dispatch('tools/reset')，会把整个会话里
      // 前几轮的工具气泡一起抹掉（第二问开始，第一问的调用就消失了）。
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

  /**
   * 回答挂起中的提问，并继续生成。
   *
   * 与 sendMessage 的关键差异：
   *   - 不再插入 user 消息。用户的回答会由后端落成一条 tool 消息，
   *     界面上应以「工具结果」而非「用户新提问」呈现，
   *     否则对话里会凭空多出一句用户没打过的话。
   *   - 只补一条 assistant 占位消息承载后续增量。
   *   - 请求走 /resume，带上 call_id 做幂等校验。
   */
  const resume = useCallback(
    async (answers: Record<string, string>) => {
      const sessionId = currentIdRef.current
      const pending = state.pendingQuestions
      if (!sessionId) {
        dispatch({ type: 'error/set', payload: '请先选择会话' })
        return
      }
      if (pending.length === 0) {
        dispatch({ type: 'error/set', payload: '当前没有待回答的问题' })
        return
      }
      if (Object.keys(answers).length === 0) return
      if (streamingRef.current) return

      // 先清掉挂起态：避免用户连点两次重复提交。
      // 若请求失败，下面会重新查一次 checkpoint 把它恢复回来。
      dispatch({ type: 'pending/set', payload: [] })
      dispatch({ type: 'draft/reset' })

      // 立刻把被回答的卡片从「调用中」翻成「完成」。
      //
      // 为什么必须在这一刻做，而不是等后端事件：
      // resume 的响应流里只有后续生成的增量，**不会有** tool.result
      // （tool 消息是后端直接写库的，不发事件）。若不在这里主动收尾，
      // 卡片会永远停在「调用中」——用户明明答过了，界面却显示还在等。
      //
      // 注意要用 callId 而不是提交键：卡片是按 call_id 索引的，
      // 一次调用内的多个问题共享同一条卡片。
      for (const q of pending) {
        const answer = answers[q.key]
        if (answer === undefined) continue
        dispatch({
          type: 'tools/result',
          payload: {
            call_id: q.callId,
            patch: { output: answer, status: 'success' },
          },
        })
      }

      const assistantMsg = tempMessage('assistant', sessionId, '')
      const tempAssistantId = assistantMsg.id
      settledRef.current = false
      dispatch({ type: 'messages/append', payload: [assistantMsg] })
      dispatch({ type: 'stream/begin', payload: { messageId: tempAssistantId } })

      const controller = new AbortController()
      abortRef.current = controller
      let realAssistantId: string | null = null

      let failed: string | null = null
      await streamPost(
        `/api/sessions/${sessionId}/resume`,
        { answers },
        {
          onEvent: (ev) => {
            if (ev.event === 'message.start') {
              const d = ev.data as MessageStartData
              realAssistantId = d.message_id
              if (ev.id !== null) dispatch({ type: 'stream/event', payload: ev.id })
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
            failed = err.message
            dispatch({ type: 'error/set', payload: err.message })
          },
        },
        { signal: controller.signal },
      )

      abortRef.current = null

      if (failed) {
        // 提交失败：把消息标为错误，并重新同步挂起态，
        // 让用户能改一下答案再试，而不是卡片凭空消失。
        const leftoverId = realAssistantId ?? tempAssistantId
        dispatch({
          type: 'messages/patch',
          payload: { id: leftoverId, patch: { status: 'error' } },
        })
        // 回滚刚才的乐观更新：这些调用其实没被回答，
        // 必须退回 running，否则卡片会显示「已完成」但问题还在。
        for (const callId of Object.keys(answers)) {
          dispatch({
            type: 'tools/result',
            payload: { call_id: callId, patch: { status: 'running' } },
          })
        }
        dispatch({ type: 'stream/end' })
        await restorePending(sessionId)
        return
      }

      if (!settledRef.current && !controller.signal.aborted) {
        dispatch({
          type: 'reconnect/hint',
          payload: '连接中断，回答可能不完整。点「重新同步」拉取服务端最新状态',
        })
      } else {
        dispatch({ type: 'reconnect/hint', payload: null })
      }

      // 兜底收尾：与 sendMessage 一致，避免停在「生成中」。
      const leftoverId = realAssistantId ?? tempAssistantId
      const leftover = messagesRef.current.find((m) => m.id === leftoverId)
      if (leftover && (leftover.status === 'pending' || leftover.status === 'streaming')) {
        // 若这一轮又被挂起（模型追问），handleEvent 已把 pendingQuestion 设好，
        // 这里只补状态，不能覆盖成 interrupted 让人误以为出错。
        dispatch({
          type: 'messages/patch',
          payload: {
            id: leftoverId,
            patch: { status: controller.signal.aborted ? 'interrupted' : 'complete' },
          },
        })
      }
      dispatch({ type: 'stream/end' })

      try {
        const { sessions } = await api.listSessions()
        dispatch({ type: 'sessions/loaded', payload: sessions })
      } catch {
        /* 列表刷新失败不影响主流程 */
      }
    },
    [handleEvent, restorePending, state.pendingQuestions],
  )

  const stopGeneration = useCallback(async () => {
    const sessionId = currentIdRef.current
    abortRef.current?.abort()
    if (!sessionId) return
    try {
      const res = await api.cancel(sessionId)
      // 后端在"无任务但状态卡住"时会复位 running，此时重新拉一次详情
      // 让提示消失、输入框恢复可用。
      if (!res.canceled) {
        dispatch({ type: 'reconnect/hint', payload: null })
        await loadDetail(sessionId)
      }
    } catch {
      /* 客户端已 abort，服务端取消失败不阻塞 UI */
    }
  }, [loadDetail])

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

  /** 主动刷新挂起态（供「重新同步」等入口调用）。 */
  const refreshPending = useCallback(async () => {
    const id = currentIdRef.current
    if (!id) return
    await restorePending(id)
  }, [restorePending])

  /** 更新草稿答案。 */
  const setDraftAnswer = useCallback((callId: string, value: string) => {
    dispatch({ type: 'draft/set', payload: { callId, value } })
  }, [])

  /** 清空草稿。 */
  const resetDrafts = useCallback(() => dispatch({ type: 'draft/reset' }), [])

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
      resume,
      setDraftAnswer,
      resetDrafts,
      refreshPending,
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
      resume,
      setDraftAnswer,
      resetDrafts,
      refreshPending,
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
