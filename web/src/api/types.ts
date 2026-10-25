/* 与后端 JSON 对齐的类型定义（见 docs/02-api-and-sse.md §0.2） */

export type Role = 'user' | 'assistant' | 'system'

export type MessageStatus =
  | 'pending'
  | 'streaming'
  | 'complete'
  | 'interrupted'
  | 'error'

export type SessionStatus = 'idle' | 'running' | 'error'

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

export interface SseEvent {
  id: number | null
  event: string
  data: unknown
}
