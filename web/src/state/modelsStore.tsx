import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { api } from '../api/client'
import type { ModelConfig, ModelConfigInput } from '../api/types'

/**
 * 模型配置的全局状态。
 *
 * 为什么单独拆一个 Provider 而不是塞进 chatStore：
 *   - 模型配置的生命周期与「当前对话」无关（配置弹窗、输入框切换器都要用）
 *   - chatStore 已经承担 SSE 状态机，再掺入 CRUD 会难维护
 *   - 这里的失败不应影响对话主流程，反之亦然
 */
export interface ModelsContextValue {
  models: ModelConfig[]
  loading: boolean
  /** 后端不支持模型配置（memory 驱动返回 501）时的提示 */
  unsupported: boolean
  error: string | null
  /** 默认配置（is_default 为真，回退到第一个） */
  defaultModel: ModelConfig | null
  refresh: () => Promise<void>
  createModel: (input: ModelConfigInput) => Promise<ModelConfig>
  updateModel: (id: string, input: ModelConfigInput) => Promise<ModelConfig>
  deleteModel: (id: string) => Promise<void>
  setDefault: (id: string) => Promise<void>
  testModel: (id: string) => Promise<{ ok: boolean; message: string }>
  dismissError: () => void
}

const ModelsContext = createContext<ModelsContextValue | null>(null)

function describe(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

export function ModelsProvider({ children }: { children: ReactNode }) {
  const [models, setModels] = useState<ModelConfig[]>([])
  const [loading, setLoading] = useState(true)
  const [unsupported, setUnsupported] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.listModels()
      setModels(res.configs ?? [])
      setUnsupported(false)
      setError(null)
    } catch (err) {
      // 501 = 当前存储驱动不支持（memory 模式），这不是错误，只是能力缺失。
      const status = (err as { status?: number }).status
      if (status === 501) {
        setUnsupported(true)
        setModels([])
      } else {
        setError(describe(err))
      }
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const createModel = useCallback(async (input: ModelConfigInput) => {
    const cfg = await api.createModel(input)
    setModels((prev) => [...prev.filter((m) => m.id !== cfg.id), cfg])
    return cfg
  }, [])

  const updateModel = useCallback(async (id: string, input: ModelConfigInput) => {
    const cfg = await api.updateModel(id, input)
    setModels((prev) => prev.map((m) => (m.id === id ? cfg : m)))
    return cfg
  }, [])

  const deleteModel = useCallback(async (id: string) => {
    await api.deleteModel(id)
    setModels((prev) => prev.filter((m) => m.id !== id))
  }, [])

  const setDefault = useCallback(async (id: string) => {
    await api.setDefaultModel(id)
    // 设为默认会取消其他配置的默认标记，直接整体重拉最稳。
    await refresh()
  }, [refresh])

  const testModel = useCallback(async (id: string) => {
    const res = await api.testModel(id)
    return {
      ok: res.ok,
      message: res.ok
        ? `连通正常${res.latency_ms ? `（${res.latency_ms}ms）` : ''}${res.preview ? `：${res.preview}` : ''}`
        : res.error || '测试失败',
    }
  }, [])

  const dismissError = useCallback(() => setError(null), [])

  const defaultModel = useMemo(() => {
    if (models.length === 0) return null
    return models.find((m) => m.is_default) ?? models[0]
  }, [models])

  const value = useMemo<ModelsContextValue>(
    () => ({
      models,
      loading,
      unsupported,
      error,
      defaultModel,
      refresh,
      createModel,
      updateModel,
      deleteModel,
      setDefault,
      testModel,
      dismissError,
    }),
    [
      models,
      loading,
      unsupported,
      error,
      defaultModel,
      refresh,
      createModel,
      updateModel,
      deleteModel,
      setDefault,
      testModel,
      dismissError,
    ],
  )

  return <ModelsContext.Provider value={value}>{children}</ModelsContext.Provider>
}

export function useModels(): ModelsContextValue {
  const ctx = useContext(ModelsContext)
  if (!ctx) throw new Error('useModels 必须在 ModelsProvider 内使用')
  return ctx
}
