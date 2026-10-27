/* REST 客户端封装：统一错误处理，不引入 axios。 */
import type {
  ApiError,
  ApprovalMode,
  ApprovalModeResponse,
  CheckpointResponse,
  CreateSessionResponse,
  HealthResponse,
  ListSessionsResponse,
  ModelConfig,
  ModelConfigInput,
  ModelConfigListResponse,
  SessionDetailResponse,
  TestModelConfigResponse,
  Workspace,
  WorkspaceInput,
  WorkspaceListResponse,
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

  createSession: (
    input: {
      title?: string
      model?: string
      model_config_id?: string
      /** 创建时预设绑定的 workspace；见 docs/14-workspace-and-session-presets.md */
      workspace_id?: string
      /** 创建时预设的会话级审批模式覆盖；留空表示跟随全局 */
      approval_mode?: ApprovalMode | ''
    } = {},
  ) =>
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

  /** 切换会话绑定的 workspace；传空串表示解绑。 */
  setSessionWorkspace: (id: string, workspaceId: string) =>
    request<CreateSessionResponse>(`/sessions/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ workspace_id: workspaceId }),
    }),

  /**
   * 切换会话级审批模式覆盖；传空串表示回退到跟随全局（/api/approval-mode）。
   *
   * 与下面 setApprovalMode 的区别：这个只影响当前会话，不影响全局开关，
   * 也不影响其他会话——见 docs/14-workspace-and-session-presets.md。
   */
  setSessionApprovalMode: (id: string, mode: ApprovalMode | '') =>
    request<CreateSessionResponse>(`/sessions/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ approval_mode: mode }),
    }),

  deleteSession: (id: string) =>
    request<void>(`/sessions/${id}`, { method: 'DELETE' }),

  cancel: (id: string) =>
    request<{ canceled: boolean; status_reset?: boolean }>(`/sessions/${id}/cancel`, {
      method: 'POST',
    }),

  /* ---- 等待用户回答（ask_user）---- */

  /** 查询会话是否有挂起中的提问（刷新页面后恢复问答卡片）。 */
  getCheckpoint: (id: string) => request<CheckpointResponse>(`/sessions/${id}/checkpoint`),

  /**
   * 回答挂起中的提问并继续生成。
   *
   * 走 SSE：后端会先补一条 tool 结果，再重新进入工具循环，
   * 后续的文本增量与正常回答一致。
   */
  resumeUrl: (id: string) => `${BASE}/sessions/${id}/resume`,

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

  /* ---- workspace（会话可绑定的多文件夹集合，见 docs/14-workspace-and-session-presets.md） ---- */

  listWorkspaces: () => request<WorkspaceListResponse>('/workspaces'),

  createWorkspace: (input: WorkspaceInput) =>
    request<Workspace>('/workspaces', { method: 'POST', body: JSON.stringify(input) }),

  updateWorkspace: (id: string, input: WorkspaceInput) =>
    request<Workspace>(`/workspaces/${id}`, { method: 'PUT', body: JSON.stringify(input) }),

  deleteWorkspace: (id: string) => request<void>(`/workspaces/${id}`, { method: 'DELETE' }),

  /* ---- 审批模式（运行时切换，见 docs/12-approval-modes.md） ---- */

  getApprovalMode: () => request<ApprovalModeResponse>('/approval-mode'),

  setApprovalMode: (mode: 'manual' | 'model' | 'auto') =>
    request<ApprovalModeResponse>('/approval-mode', {
      method: 'PUT',
      body: JSON.stringify({ mode }),
    }),
}
