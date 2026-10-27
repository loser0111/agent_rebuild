/**
 * 预加载脚本。
 *
 * 界面通过 HTTP/SSE 与后端通信，绝大多数能力不需要主进程。
 * 这里只暴露三类最小接口：
 *   1. desktopEnv —— 只读的环境标识
 *   2. windowControl —— 窗口缩放（隐藏标题栏后需自行实现双击放大）
 *   3. fileDialog —— 原生文件夹选择器（workspace 管理用，渲染层拿不到真实路径）
 *
 * contextIsolation=true + sandbox=true 下，渲染进程无法访问 Node API，
 * 只能通过这里显式暴露的方法与主进程通信。
 */
const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('desktopEnv', {
  isDesktop: true,
  platform: process.platform,
  versions: {
    electron: process.versions.electron,
    chrome: process.versions.chrome,
  },
})

contextBridge.exposeInMainWorld('windowControl', {
  /**
   * 切换最大化 / 还原。
   *
   * 用途：hiddenInset 标题栏下，macOS 的「双击标题栏缩放」不会触发，
   * 需要在渲染层自己监听双击并调用此方法。
   */
  toggleMaximize: () => ipcRenderer.invoke('window:toggle-maximize'),
  /** 查询当前是否最大化（用于图标/提示状态）。 */
  isMaximized: () => ipcRenderer.invoke('window:is-maximized'),
})

contextBridge.exposeInMainWorld('fileDialog', {
  /**
   * 弹出系统原生的文件夹选择器，返回选中的绝对路径数组。
   * 用户取消选择时返回空数组（不是 null），调用方可以直接当数组用。
   */
  selectFolders: (options) => ipcRenderer.invoke('dialog:select-folders', options ?? {}),
})
