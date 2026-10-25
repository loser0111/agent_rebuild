/* REST 客户端封装：统一错误处理，不引入 axios。 */
import type {
  ApiError,
  CreateSessionResponse,
  HealthResponse,
  ListSessionsResponse,
  ModelConfig,
  ModelConfigInput,
  ModelConfigListResponse,
  SessionDetailResponse,
  TestModelConfigResponse,
} from './types'

/** 业务/网络错误统一类型 */
export class ApiRequestError extends Error {
  code: string
  status: number
  requestId?: string

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message)
    this.name = 'ApiRequestError'
    this.status = status
    this.code = code
    this.requestId = requestId
  }
}

const BASE = '/api'

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch(`${BASE}${path}`, {
      ...init,
      headers: {
        Accept: 'application/json',
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...init?.headers,
      },
    })
  } catch (err) {
    // fetch 只在网络层失败时 reject（后端未启动/断网）
    throw new ApiRequestError(0, 'network_error', `无法连接后端服务：${String(err)}`)
  }

  if (res.status === 204) {
    return undefined as T
  }

  if (!res.ok) {
    let code = 'internal_error'
    let message = `请求失败（HTTP ${res.status}）`
    let requestId: string | undefined
    try {
      const body = (await res.json()) as ApiError
      code = body.error?.code ?? code
      message = body.error?.message ?? message
      requestId = body.error?.request_id
    } catch {
      /* 响应不是 JSON，保留默认信息 */
    }
    throw new ApiRequestError(res.status, code, message, requestId)
  }

  return (await res.json()) as T
}

export const api = {
  health: () => request<HealthResponse>('/health'),

  listSessions: (limit = 50) =>
    request<ListSessionsResponse>(`/sessions?limit=${limit}`),

  createSession: (input: { title?: string; model?: string; model_config_id?: string } = {}) =>
    request<CreateSessionResponse>('/sessions', {
      method: 'POST',
      body: JSON.stringify(input),
    }),

  getSession: (id: string) => request<SessionDetailResponse>(`/sessions/${id}`),

  renameSession: (id: string, title: string) =>
    request<CreateSessionResponse>(`/sessions/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ title }),
    }),

  /** 更新会话的模型选择（PATCH 同时支持 title 与 model_config_id）。 */
  setSessionModel: (id: string, modelConfigId: string) =>
    request<CreateSessionResponse>(`/sessions/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ model_config_id: modelConfigId }),
    }),

  deleteSession: (id: string) =>
    request<void>(`/sessions/${id}`, { method: 'DELETE' }),

  cancel: (id: string) =>
    request<{ canceled: boolean }>(`/sessions/${id}/cancel`, { method: 'POST' }),

  /* ---- 模型配置 ---- */

  listModels: () => request<ModelConfigListResponse>('/models'),

  createModel: (input: ModelConfigInput) =>
    request<ModelConfig>('/models', { method: 'POST', body: JSON.stringify(input) }),

  updateModel: (id: string, input: ModelConfigInput) =>
    request<ModelConfig>(`/models/${id}`, { method: 'PUT', body: JSON.stringify(input) }),

  deleteModel: (id: string) => request<void>(`/models/${id}`, { method: 'DELETE' }),

  setDefaultModel: (id: string) =>
    request<ModelConfig>(`/models/${id}/default`, { method: 'POST' }),

  testModel: (id: string) =>
    request<TestModelConfigResponse>(`/models/${id}/test`, { method: 'POST' }),
}
