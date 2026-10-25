// 用 esbuild 打包 SSR 渲染检查脚本后执行（.tsx 需要 JSX 转译，Node 原生处理不了）。
//
// 产物必须落在 web/ 目录内，否则 react / react-dom 无法从 node_modules 解析。
// 用法：cd web && node scripts/run-render-check.mjs
import { build } from 'esbuild'
import { pathToFileURL } from 'node:url'
import { rm } from 'node:fs/promises'
import path from 'node:path'

const outFile = path.resolve('node_modules/.cache/render-check.mjs')

await rm(outFile, { force: true })

try {
  await build({
    entryPoints: [path.resolve('scripts/web-render-check.mjs')],
    outfile: outFile,
    bundle: true,
    platform: 'node',
    format: 'esm',
    target: 'node20',
    // react / react-dom 不打包，运行时从 web/node_modules 解析
    external: ['react', 'react-dom', 'react/jsx-runtime'],
    jsx: 'automatic',
    logLevel: 'warning',
  })

  await import(pathToFileURL(outFile).href)
} finally {
  await rm(outFile, { force: true })
}
