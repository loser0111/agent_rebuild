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
import type { Workspace, WorkspaceInput } from '../api/types'

/**
 * workspace 的全局状态。
 *
 * 结构完全照抄 modelsStore：workspace 的生命周期与「当前对话」无关
 * （管理弹窗、输入框切换器都要用），不塞进 chatStore 以免混入 CRUD 逻辑。
 * 见 docs/14-workspace-and-session-presets.md。
 */
export interface WorkspacesContextValue {
  workspaces: Workspace[]
  loading: boolean
  /** 后端不支持 workspace（memory 驱动返回 501）时的提示 */
  unsupported: boolean
  error: string | null
  refresh: () => Promise<void>
  createWorkspace: (input: WorkspaceInput) => Promise<Workspace>
  updateWorkspace: (id: string, input: WorkspaceInput) => Promise<Workspace>
  deleteWorkspace: (id: string) => Promise<void>
  dismissError: () => void
}

const WorkspacesContext = createContext<WorkspacesContextValue | null>(null)

function describe(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

export function WorkspacesProvider({ children }: { children: ReactNode }) {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [unsupported, setUnsupported] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.listWorkspaces()
      setWorkspaces(res.workspaces ?? [])
      setUnsupported(false)
      setError(null)
    } catch (err) {
      // 501 = 当前存储驱动不支持（memory 模式），这不是错误，只是能力缺失。
      const status = (err as { status?: number }).status
      if (status === 501) {
        setUnsupported(true)
        setWorkspaces([])
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

  const createWorkspace = useCallback(async (input: WorkspaceInput) => {
    const ws = await api.createWorkspace(input)
    setWorkspaces((prev) => [...prev.filter((w) => w.id !== ws.id), ws])
    return ws
  }, [])

  const updateWorkspace = useCallback(async (id: string, input: WorkspaceInput) => {
    const ws = await api.updateWorkspace(id, input)
    setWorkspaces((prev) => prev.map((w) => (w.id === id ? ws : w)))
    return ws
  }, [])

  const deleteWorkspace = useCallback(async (id: string) => {
    await api.deleteWorkspace(id)
    setWorkspaces((prev) => prev.filter((w) => w.id !== id))
  }, [])

  const dismissError = useCallback(() => setError(null), [])

  const value = useMemo<WorkspacesContextValue>(
    () => ({
      workspaces,
      loading,
      unsupported,
      error,
      refresh,
      createWorkspace,
      updateWorkspace,
      deleteWorkspace,
      dismissError,
    }),
    [workspaces, loading, unsupported, error, refresh, createWorkspace, updateWorkspace, deleteWorkspace, dismissError],
  )

  return <WorkspacesContext.Provider value={value}>{children}</WorkspacesContext.Provider>
}

export function useWorkspaces(): WorkspacesContextValue {
  const ctx = useContext(WorkspacesContext)
  if (!ctx) throw new Error('useWorkspaces 必须在 WorkspacesProvider 内使用')
  return ctx
}
