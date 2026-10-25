#!/usr/bin/env node
/**
 * 把「后端二进制 + 前端产物」准备好，供 Electron 打包使用。
 *
 *   desktop/sidecar/agent-server    ← bin/agent-server（按当前平台）
 *   desktop/ui/                     ← web/dist
 *
 * 用法：node scripts/prepare-sidecar.mjs
 */
import { execFileSync } from 'node:child_process'
import { cp, mkdir, rm, stat } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const desktopDir = path.resolve(here, '..')
const rootDir = path.resolve(desktopDir, '..')

const binName = process.platform === 'win32' ? 'agent-server.exe' : 'agent-server'
const sidecarDir = path.join(desktopDir, 'sidecar')
const sidecarPath = path.join(sidecarDir, binName)
const uiSrc = path.join(rootDir, 'web', 'dist')
const uiDst = path.join(desktopDir, 'ui')

async function exists(p) {
  try {
    await stat(p)
    return true
  } catch {
    return false
  }
}

async function main() {
  // 1. 构建后端二进制（含默认前端目录的 ldflags，便于独立运行）。
  console.log('▶ 构建 Go sidecar…')
  await mkdir(sidecarDir, { recursive: true })
  execFileSync(
    'go',
    [
      'build',
      '-trimpath',
      '-ldflags',
      '-s -w -X agent_rebuild/internal/api.Version=0.1.0 -X main.defaultWebDir=web/dist',
      '-o',
      sidecarPath,
      './cmd/server',
    ],
    { cwd: rootDir, stdio: 'inherit', env: { ...process.env, CGO_ENABLED: '0' } },
  )

  // 2. 构建前端产物（若缺失）。
  if (!(await exists(path.join(uiSrc, 'index.html')))) {
    console.log('▶ 构建前端产物…')
    execFileSync('npm', ['run', 'build'], { cwd: path.join(rootDir, 'web'), stdio: 'inherit' })
  } else {
    console.log('▶ 复用已有前端产物 web/dist')
  }

  // 3. 拷贝产物到 desktop/ 下（electron-builder 只打包这两个目录）。
  console.log('▶ 同步前端产物到 desktop/ui…')
  await rm(uiDst, { recursive: true, force: true })
  await cp(uiSrc, uiDst, { recursive: true })

  const binStat = await stat(sidecarPath)
  console.log(`✅ 准备完成：sidecar=${path.relative(rootDir, sidecarPath)} (${(binStat.size / 1024 / 1024).toFixed(1)}MB)`)
  console.log(`✅ 前端产物：${path.relative(rootDir, uiDst)}`)
}

main().catch((err) => {
  console.error('❌ 准备失败:', err.message)
  process.exit(1)
})
