import { useEffect, useState } from 'react'
import { ChatProvider, useChat } from './state/chatStore'
import { ModelsProvider } from './state/modelsStore'
import { ApprovalModeProvider } from './state/approvalModeStore'
import { WorkspacesProvider } from './state/workspacesStore'
import { Sidebar } from './components/Sidebar'
import { ChatView } from './components/ChatView'
import { Composer } from './components/Composer'
import { StatusBar } from './components/StatusBar'
import { ModelManager } from './components/ModelManager'
import { WorkspaceManager } from './components/WorkspaceManager'
import { SessionSettingsDialog } from './components/SessionSettingsDialog'

/** 桌面壳注入的窗口控制接口（仅 Electron 下存在）。 */
interface WindowControl {
  toggleMaximize: () => Promise<boolean>
  isMaximized: () => Promise<boolean>
}

/**
 * 桌面壳注入的原生文件夹选择器（仅 Electron 下存在）。
 *
 * 渲染层被 contextIsolation+sandbox 隔绝，拿不到真实文件系统路径
 * （浏览器 <input type="file"> 出于安全不会暴露绝对路径），只能经
 * 主进程弹系统原生对话框，再把选中路径传回来——WorkspaceManager.tsx
 * 用它实现"点 + 选文件夹"而不是手打路径。
 */
interface FileDialog {
  /** 取消选择时返回空数组，不是 null/undefined。 */
  selectFolders: (options?: { allowMultiple?: boolean }) => Promise<string[]>
}

declare global {
  interface Window {
    desktopEnv?: { isDesktop: boolean; platform: string }
    windowControl?: WindowControl
    fileDialog?: FileDialog
  }
}

/** 判断是否运行在桌面壳中。 */
function isDesktop(): boolean {
  return Boolean(window.desktopEnv?.isDesktop)
}

/** Codex 风格主界面：左侧会话栏 + 右侧对话区 + 底部状态栏。 */
export default function App() {
  const desktop = isDesktop()
  return (
    <ModelsProvider>
      <ApprovalModeProvider>
        <WorkspacesProvider>
          <ChatProvider>
            <Shell desktop={desktop} />
          </ChatProvider>
        </WorkspacesProvider>
      </ApprovalModeProvider>
    </ModelsProvider>
  )
}

function Shell({ desktop }: { desktop: boolean }) {
  const { error, dismissError, currentId, sessions } = useChat()
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [modelManagerOpen, setModelManagerOpen] = useState(false)
  const [workspaceManagerOpen, setWorkspaceManagerOpen] = useState(false)
  const [sessionSettingsOpen, setSessionSettingsOpen] = useState(false)

  const current = sessions.find((s) => s.id === currentId)

  // Cmd/Ctrl + K 新建会话的快捷键提示位（此处仅阻止默认搜索行为）
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  /**
   * 桌面端双击顶栏放大/还原。
   *
   * 为什么需要手动实现：macOS 用了 hiddenInset（隐藏系统标题栏），
   * 「双击标题栏缩放」的系统行为随之失效。
   *
   * 只对「非交互区」生效：事件目标若在消息正文、输入框、按钮内，
   * 或带有 data-no-window-drag 标记，则忽略。
   */
  const onTitlebarDoubleClick = (e: React.MouseEvent) => {
    if (!desktop || !window.windowControl) return
    const target = e.target as HTMLElement | null
    if (!target) return
    if (target.closest('[data-no-window-drag]')) return
    if (target.closest('button, a, input, textarea, .md, .user-text, .session-item')) return
    void window.windowControl.toggleMaximize()
  }

  return (
    <div
      className={`app${sidebarOpen ? ' sidebar-open' : ''}${desktop ? ' is-desktop' : ''}`}
    >
      <div className="sidebar-wrap">
        <Sidebar />
      </div>

      <main className="main">
        <header className="topbar" onDoubleClick={onTitlebarDoubleClick}>
          <button
            className="icon-btn only-mobile"
            type="button"
            onClick={() => setSidebarOpen((v) => !v)}
            title="切换会话列表"
          >
            ☰
          </button>
          <div className="topbar-title">
            {current ? current.title || '未命名会话' : 'Agent Rebuild'}
          </div>

          <div className="topbar-right">
            {current && <div className="topbar-model">{current.model}</div>}
            {/* 当前会话设置：绑定模型/workspace/审批模式覆盖，需先选中一个会话 */}
            {current && (
              <button
                className="icon-btn"
                type="button"
                onClick={() => setSessionSettingsOpen(true)}
                title="会话设置"
                aria-label="会话设置"
              >
                ⚙️
              </button>
            )}
            {/* 窗口右上角的 workspace 管理入口 */}
            <button
              className="icon-btn"
              type="button"
              onClick={() => setWorkspaceManagerOpen(true)}
              title="workspace 管理"
              aria-label="workspace 管理"
            >
              📁
            </button>
            {/* 窗口右上角的模型配置入口 */}
            <button
              className="icon-btn"
              type="button"
              onClick={() => setModelManagerOpen(true)}
              title="模型配置"
              aria-label="模型配置"
            >
              <span className="icon-sliders" aria-hidden="true" />
            </button>
          </div>
        </header>

        {error && (
          <div className="banner banner-error">
            <span>{error}</span>
            <button className="banner-btn" type="button" onClick={dismissError}>
              知道了
            </button>
          </div>
        )}

        <ChatView />
        <Composer
          onOpenModelManager={() => setModelManagerOpen(true)}
          onOpenWorkspaceManager={() => setWorkspaceManagerOpen(true)}
        />
        <StatusBar />
      </main>

      {sidebarOpen && <div className="scrim" onClick={() => setSidebarOpen(false)} />}

      <ModelManager open={modelManagerOpen} onClose={() => setModelManagerOpen(false)} />
      <WorkspaceManager open={workspaceManagerOpen} onClose={() => setWorkspaceManagerOpen(false)} />
      <SessionSettingsDialog
        open={sessionSettingsOpen}
        onClose={() => setSessionSettingsOpen(false)}
        session={current}
      />
    </div>
  )
}
