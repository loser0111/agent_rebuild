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
import type { ApprovalMode } from '../api/types'

/**
 * 审批模式的全局状态（见 docs/12-approval-modes.md）。
 *
 * 为什么单独拆一个 Provider，而不是塞进 chatStore 或 modelsStore：
 *   - 审批模式是**整个进程**级别的开关（Server.SetApprovalMode 直接换掉
 *     executor 持有的那一个 Approver），不像 ModelPicker 选的是"这个会话
 *     用哪套模型配置"——它与"当前会话"无关，与 ModelsProvider 的语义
 *     完全不同，混进去会让人误以为这是会话级设置。
 *   - chatStore 已经承担 SSE 状态机，没必要再掺入这个跟对话主流程
 *     无关的横切开关。
 */
export interface ApprovalModeContextValue {
  /** 当前生效模式；工具系统未启用（tools_enabled=false）时为空串。 */
  mode: ApprovalMode | ''
  options: ApprovalMode[]
  toolsEnabled: boolean
  loading: boolean
  /** 切换中（禁用按钮避免重复点击）。 */
  switching: boolean
  error: string | null
  refresh: () => Promise<void>
  setMode: (mode: ApprovalMode) => Promise<void>
  dismissError: () => void
}

const ApprovalModeContext = createContext<ApprovalModeContextValue | null>(null)

function describe(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

export function ApprovalModeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ApprovalMode | ''>('')
  const [options, setOptions] = useState<ApprovalMode[]>(['manual', 'model', 'auto'])
  const [toolsEnabled, setToolsEnabled] = useState(false)
  const [loading, setLoading] = useState(true)
  const [switching, setSwitching] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.getApprovalMode()
      setModeState(res.mode)
      setOptions(res.options ?? ['manual', 'model', 'auto'])
      setToolsEnabled(res.tools_enabled)
      setError(null)
    } catch (err) {
      setError(describe(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const setMode = useCallback(async (next: ApprovalMode) => {
    setSwitching(true)
    try {
      const res = await api.setApprovalMode(next)
      setModeState(res.mode)
      setOptions(res.options ?? ['manual', 'model', 'auto'])
      setToolsEnabled(res.tools_enabled)
      setError(null)
    } catch (err) {
      // 切换失败不回滚本地显示值——直接重拉一次真实状态最可靠，
      // 避免「UI 显示已切换但后端其实没变」的假象。
      setError(describe(err))
      await refresh()
    } finally {
      setSwitching(false)
    }
  }, [refresh])

  const dismissError = useCallback(() => setError(null), [])

  const value = useMemo<ApprovalModeContextValue>(
    () => ({ mode, options, toolsEnabled, loading, switching, error, refresh, setMode, dismissError }),
    [mode, options, toolsEnabled, loading, switching, error, refresh, setMode, dismissError],
  )

  return <ApprovalModeContext.Provider value={value}>{children}</ApprovalModeContext.Provider>
}

export function useApprovalMode(): ApprovalModeContextValue {
  const ctx = useContext(ApprovalModeContext)
  if (!ctx) throw new Error('useApprovalMode 必须在 ApprovalModeProvider 内使用')
  return ctx
}
