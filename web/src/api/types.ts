/* 与后端 JSON 对齐的类型定义（见 docs/02-api-and-sse.md §0.2） */

export type Role = 'user' | 'assistant' | 'system' | 'tool'

export type MessageStatus =
  | 'pending'
  | 'streaming'
  | 'complete'
  | 'interrupted'
  | 'error'

export type SessionStatus = 'idle' | 'running' | 'error' | 'suspended'

export interface Usage {
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
}

export interface MessageError {
  code: string
  message: string
}

export interface Session {
  id: string
  title: string
  created_at: string
  updated_at: string
  message_count: number
  status: SessionStatus
  model: string
  /** 该会话选用的模型配置 id（空表示用全局默认） */
  model_config_id?: string
  meta?: Record<string, string>
}

/* ---- 模型配置 ---- */

export type ModelProtocol = 'openai' | 'anthropic' | 'mock'

export interface ModelConfig {
  id: string
  name: string
  protocol: ModelProtocol
  provider: string
  base_url: string
  model: string
  system_prompt: string
  temperature?: number | null
  max_tokens: number
  is_default: boolean
  /** 是否已配置密钥（后端永不返回密钥明文） */
  has_api_key: boolean
  created_at: string
  updated_at: string
}

export interface ModelConfigListResponse {
  configs: ModelConfig[]
  current_protocol: string
  current_model: string
}

export interface ModelConfigInput {
  name: string
  protocol: ModelProtocol
  provider?: string
  base_url?: string
  model?: string
  system_prompt?: string
  temperature?: number | null
  max_tokens?: number
  api_key?: string
  is_default?: boolean
}

export interface TestModelConfigResponse {
  ok: boolean
  error?: string
  /** 测试成功时返回的首段输出，便于确认模型真的能回话 */
  preview?: string
  latency_ms?: number
}

/** 后端返回的工具调用记录（落库形态）。 */
export interface StoredToolCall {
  id: string
  name: string
  /** 参数 JSON 原样字符串 */
  args: string
}

export interface Message {
  id: string
  session_id: string
  role: Role
  content: string
  status: MessageStatus
  created_at: string
  updated_at: string
  usage?: Usage | null
  error?: MessageError | null
  /** assistant 发起的工具调用（历史回放用） */
  tool_calls?: StoredToolCall[] | null
  /** tool 消息关联的调用 ID（历史回放用） */
  tool_call_id?: string | null
}

export interface ApiError {
  error: {
    code: string
    message: string
    request_id?: string
  }
}

/* ---- REST 响应 ---- */

export interface CreateSessionResponse {
  session: Session
}

export interface ListSessionsResponse {
  sessions: Session[]
  next_cursor: string | null
}

export interface SessionDetailResponse {
  session: Session
  messages: Message[]
  last_event_id: number
}

export interface HealthResponse {
  status: string
  version: string
  provider: string
  model: string
  env: string
  uptime_seconds: number
}

/* ---- SSE 事件负载 ---- */

export interface MessageStartData {
  seq: number
  session_id: string
  message_id: string
  model: string
  created_at: string
}

export interface MessageDeltaData {
  seq: number
  session_id: string
  message_id: string
  delta: string
}

export interface MessageDoneData {
  seq: number
  session_id: string
  message_id: string
  content: string
  finish_reason: string
  usage?: Usage | null
}

export interface MessageInterruptedData {
  seq: number
  session_id: string
  message_id: string
  content: string
}

export interface MessageErrorData {
  seq: number
  session_id: string
  message_id: string
  error: MessageError
}

export interface StreamErrorData {
  error: MessageError
  request_id?: string
}

export interface PingData {
  ts: string
}

/* ---- 工具调用 ---- */

export type ToolRisk = 'safe' | 'moderate' | 'dangerous'

/** tool.call：模型请求调用工具 */
export interface ToolCallData {
  seq: number
  session_id: string
  message_id: string
  call_id: string
  name: string
  /** 参数的 JSON 字符串（原样保留，可能不是合法 JSON） */
  args: string
  risk: ToolRisk
  requires_approval: boolean
}

/** tool.result：工具执行完成 */
export interface ToolResultData {
  seq: number
  session_id: string
  message_id: string
  call_id: string
  name: string
  output: string
  is_error: boolean
  rejected: boolean
  duration_ms: number
}

/** tool.approval：需要人工审批（人工审批模式下的写操作，挂起等人点击）。 */
export interface ToolApprovalData {
  seq: number
  session_id: string
  message_id: string
  call_id: string
  name: string
  /** 参数的 JSON 字符串（原样保留，可能不是合法 JSON）。 */
  args: string
  risk: ToolRisk
  /** 审批器给出的说明（例如"等待用户确认"/判官模型的裁决理由）。 */
  reason?: string
}

/** 前端聚合后的工具调用视图模型 */
export interface ToolCallView {
  call_id: string
  /** 归属的 assistant 消息 id，用于把卡片挂到正确的那条消息下 */
  message_id: string
  name: string
  args: string
  risk: ToolRisk
  requires_approval: boolean
  /** 尚未收到 result 时为 running */
  status: 'running' | 'success' | 'error' | 'rejected'
  output?: string
  duration_ms?: number
}

export interface ResyncData {
  session_id: string
  last_event_id: number
  messages: Message[]
}

/* ---- 等待用户回答（ask_user）---- */

/** awaiting.input：模型提问后挂起，等待用户答复。 */
export interface AwaitingInputData {
  seq: number
  session_id: string
  message_id: string
  /** 待回答的调用 ID（同一调用内多个问题时相同） */
  call_id: string
  /** 该问题在调用内的短标识（多问题时用于区分） */
  qid?: string
  question: string
  options?: string[]
  /** 是否允许多选 */
  multi?: boolean
  step_index: number
}

/** GET /sessions/{id}/checkpoint 的响应。 */
export interface CheckpointResponse {
  suspended: boolean
  state?: string
  /** 本轮所有待回答项（新格式）。 */
  questions?: Array<{
    /** 挂起类型："input"（ask_user 问答）或 "approval"（人工审批）。 */
    kind?: string
    call_id: string
    qid?: string
    /** 提交时使用的键：call_id 或 call_id::qid */
    key?: string
    question?: string
    options?: string[]
    multi?: boolean
    /** 以下三个仅 kind="approval" 时有值。 */
    name?: string
    args?: string
    risk?: ToolRisk
    reason?: string
  }>
  /** 首个问题的扁平字段（兼容旧前端）。 */
  call_id?: string
  question?: string
  options?: string[]
  step_index?: number
  updated_at?: string
}

/** 前端持有的「待回答提问」视图模型。 */
export interface PendingQuestion {
  /** 挂起类型：'input' = ask_user 问答，'approval' = 人工审批写操作。 */
  kind: 'input' | 'approval'
  /** 提交时使用的键：call_id 或 call_id::qid（仅 kind='input' 时有意义）。 */
  key: string
  callId: string
  qid: string
  messageId: string
  question: string
  options: string[]
  /** 是否允许多选 */
  multi: boolean
  /** 以下四个仅 kind='approval' 时有值。 */
  toolName?: string
  toolArgs?: string
  risk?: ToolRisk
  reason?: string
}


export interface SseEvent {
  id: number | null
  event: string
  data: unknown
}
