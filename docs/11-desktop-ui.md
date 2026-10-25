# 11 · 桌面端界面适配

> 记录 hiddenInset 标题栏带来的三个问题及修复：红绿灯遮挡、双击放大失效、文本可选中范围过广。

---

## 背景：为什么会有这些问题

macOS 上用了 `titleBarStyle: 'hiddenInset'`——隐藏系统标题栏，让红绿灯按钮浮在内容之上，视觉上更接近 Codex / Claude Desktop。

代价是**失去系统标题栏的全部默认行为**：

| 系统标题栏提供的能力 | hiddenInset 下 |
| --- | --- |
| 为红绿灯留出空间 | ❌ 内容会顶到按钮下面 |
| 拖动窗口移动 | ❌ 需要自行指定 `-webkit-app-region: drag` |
| 双击标题栏缩放 | ❌ 完全失效 |
| 标题文字居中 | ❌ 需自己布局 |

三个反馈的问题全部源于此。

---

## 问题 1：程序名与红绿灯重叠

### 现象

侧栏顶部的「Agent Rebuild」被红绿灯按钮遮住。

### 根因

侧栏 `padding-top` 是 14px，而红绿灯占用 `y=14~26`（位置 14 + 按钮高约 12），完全重叠。

### 修复

```css
/* 34px = 红绿灯底部 26px + 8px 呼吸间距 */
.app.is-desktop .sidebar-head {
  padding-top: 34px;
}
```

**关键点**：只在桌面环境生效（`.app.is-desktop`），浏览器版本不受影响。

桌面标记来自 preload 注入的 `window.desktopEnv.isDesktop`：

```tsx
const desktop = Boolean(window.desktopEnv?.isDesktop)
<div className={`app${desktop ? ' is-desktop' : ''}`}>
```

### 验证方式

运行时读 `getComputedStyle('.sidebar-head').paddingTop`，确认 >= 30px。

> 第一次改成 38px 时截图发现间距偏大——`capturePage` 截不到窗口边框，必须用整屏截图才看得出真实位置，最后定为 34px。

---

## 问题 2：双击无法放大窗口

### 根因

没有系统标题栏 → macOS 的「双击标题栏缩放」不会触发。

### 修复：三层接线

**1. 主进程提供 IPC handler**

```js
ipcMain.handle('window:toggle-maximize', () => {
  const win = BrowserWindow.getFocusedWindow() || mainWindow
  if (!win) return false
  if (win.isMaximized()) { win.unmaximize(); return false }
  win.maximize()
  return true
})
```

**2. preload 暴露最小接口**

```js
contextBridge.exposeInMainWorld('windowControl', {
  toggleMaximize: () => ipcRenderer.invoke('window:toggle-maximize'),
  isMaximized: () => ipcRenderer.invoke('window:is-maximized'),
})
```

**3. 渲染层绑定双击并排除交互区**

```tsx
const onTitlebarDoubleClick = (e: React.MouseEvent) => {
  if (!desktop || !window.windowControl) return
  const target = e.target as HTMLElement
  // 双击正文/输入框/按钮不应缩放窗口
  if (target.closest('[data-no-window-drag]')) return
  if (target.closest('button, a, input, textarea, .md, .user-text, .session-item')) return
  void window.windowControl.toggleMaximize()
}
```

### 两个易错点

**a. 拖拽区与点击区冲突**

`-webkit-app-region: drag` 的元素**无法接收点击事件**。如果顶栏整体设为 drag，里面的按钮就点不动了。必须显式排除：

```css
.topbar { -webkit-app-region: drag; }
.topbar button { -webkit-app-region: no-drag; }
```

同理，`.session-item` 也必须是 `no-drag`，否则无法点击切换会话。

**b. 浏览器环境无 windowControl**

`window.windowControl` 在 Web 版不存在，必须空值保护，否则点击报错。同时非桌面端要覆盖掉 `app-region`：

```css
.app:not(.is-desktop) .topbar,
.app:not(.is-desktop) .brand {
  -webkit-app-region: no-drag;  /* 浏览器里 drag 区会导致文字无法选中 */
}
```

---

## 问题 3：页面上除对话外都能被选中

### 现象

侧栏标题、状态栏、按钮文字都能被拖选，像网页而不像原生应用。

### 修复：全局禁用 + 白名单放开

```css
/* 默认全部禁用 */
body { user-select: none; }

/* 只对需要的区域放开 */
.md, .user-text, textarea, input, pre, code, .msg-error {
  user-select: text;
}
```

**为什么用「全局禁用 + 白名单」而不是「逐个元素禁用」**：前者不会遗漏新增的 UI 元素，后者每加一个组件都要记得加规则。

### 实测结果

| 区域 | user-select |
| --- | --- |
| `body` | `none` |
| `.md`（对话正文） | `text` |
| `textarea`（输入框） | `text` |
| `.statusbar` | `none` |
| `button` / `.badge` | `none` |

---

## 验证方式

### 静态断言（`make desktop-test` 内的 `test-ui-fixes.mjs`，15 项）

对源码做结构断言：CSS 里有安全区规则、主进程注册了 IPC、preload 暴露了接口、渲染层绑定了双击且排除交互区……

**优点**：快（毫秒级），适合每次提交跑。
**缺点**：正则匹配可能误判（实际踩过一次——匹配到了 `.app.is-desktop .brand` 而非 `.brand`）。

### 运行时验证（`make desktop-ui-check`，10 项）

启动真实 Electron，从渲染进程读 `getComputedStyle` 并实际调用一次 IPC：

```
✓ 侧栏顶部安全区 34px（红绿灯占用约 26px）
✓ 桌面环境标记 is-desktop 已生效
✓ windowControl 已注入
✓ 双击放大通道可用（IPC 调用成功并已最大化）
✓ body 默认不可选中
✓ 对话内容（.md）可选中
✓ 输入框可选中
✓ 状态栏不可选中
✓ 品牌区可拖动窗口
✓ 顶栏按钮不可拖动（避免点击失效）
```

**这是唯一可信的验证**——静态断言说「CSS 里有这行」，运行时验证说「浏览器实际算出来的就是这值」。

实现上通过 `VERIFY_UI=1` 环境变量触达 main.js 里的验证钩子，生产运行时该分支永不进入。

---

## 已知取舍

| 取舍 | 说明 |
| --- | --- |
| 34px 安全区是硬编码 | 红绿灯位置随 macOS 版本可能微调；改变 `trafficLightPosition` 时需同步改 CSS |
| 双击只支持顶栏/品牌区 | 消息区、输入区故意排除（那里双击应选词） |
| 未实现「双击最大化」以外的窗口操作 | 如需最小化按钮，得自己在顶栏画 |
| 拖拽区依赖 Chromium 私有属性 | `-webkit-app-region` 非标准，但 Electron 长期支持 |
