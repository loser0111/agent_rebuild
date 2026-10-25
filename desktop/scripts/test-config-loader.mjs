#!/usr/bin/env node
/**
 * 配置加载器测试。
 *
 * 这些断言覆盖了「配了 Key 却不生效」类问题的所有常见根因：
 * 引号处理、行尾注释、export 前缀、优先级、路径查找。
 *
 * 用法：node scripts/test-config-loader.mjs
 */
import { parseDotEnv, resolveSidecarEnv } from '../config-loader.js'
import { mkdtemp, writeFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import assert from 'node:assert/strict'

let passed = 0
const test = (name, fn) => {
  try {
    fn()
    passed++
    console.log(`  ✓ ${name}`)
  } catch (err) {
    console.error(`  ✗ ${name}\n    ${err.message}`)
    process.exitCode = 1
  }
}

const testAsync = async (name, fn) => {
  try {
    await fn()
    passed++
    console.log(`  ✓ ${name}`)
  } catch (err) {
    console.error(`  ✗ ${name}\n    ${err.message}`)
    process.exitCode = 1
  }
}

console.log('\n▶ .env 解析')

test('基础键值', () => {
  assert.deepEqual(parseDotEnv('A=1\nB=two'), { A: '1', B: 'two' })
})

test('忽略注释与空行', () => {
  assert.deepEqual(parseDotEnv('# 注释\n\nA=1\n   \n'), { A: '1' })
})

test('支持 export 前缀', () => {
  assert.deepEqual(parseDotEnv('export LLM_PROVIDER=openai'), { LLM_PROVIDER: 'openai' })
})

test('双引号包裹（保留内部空格）', () => {
  assert.deepEqual(parseDotEnv('SYSTEM_PROMPT="你是 一个 助手"'), {
    SYSTEM_PROMPT: '你是 一个 助手',
  })
})

test('单引号包裹', () => {
  assert.deepEqual(parseDotEnv("KEY='value with space'"), { KEY: 'value with space' })
})

test('未加引号时剥离行尾注释', () => {
  assert.deepEqual(parseDotEnv('LLM_MODEL=gpt-4o-mini # 默认模型'), {
    LLM_MODEL: 'gpt-4o-mini',
  })
})

test('引号内的 # 不被当作注释', () => {
  assert.deepEqual(parseDotEnv('PWD_="abc#def"'), { PWD_: 'abc#def' })
})

test('值中含等号', () => {
  assert.deepEqual(parseDotEnv('KEY=abc=def'), { KEY: 'abc=def' })
})

test('跳过非法键名与无等号行', () => {
  assert.deepEqual(parseDotEnv('not a kv line\n=novalue\nOK=1'), { OK: '1' })
})

test('CRLF 换行', () => {
  assert.deepEqual(parseDotEnv('A=1\r\nB=2\r\n'), { A: '1', B: '2' })
})

console.log('\n▶ 配置解析与优先级')

await testAsync('读取用户级 config.env', async () => {
  const dir = await mkdtemp(path.join(tmpdir(), 'cfg-'))
  try {
    await writeFile(
      path.join(dir, 'config.env'),
      'LLM_PROVIDER=openai\nOPENAI_API_KEY=sk-from-file\nLLM_MODEL=gpt-4o\n',
    )
    const { env, configFile, effective } = resolveSidecarEnv({
      userDataDir: dir,
      appDir: '/nonexistent',
      processEnv: {},
    })
    assert.equal(configFile, path.join(dir, 'config.env'))
    assert.equal(env.LLM_PROVIDER, 'openai')
    assert.equal(env.OPENAI_API_KEY, 'sk-from-file')
    assert.equal(effective.provider, 'openai')
    assert.equal(effective.hasKey, true)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

await testAsync('真实环境变量优先于配置文件', async () => {
  const dir = await mkdtemp(path.join(tmpdir(), 'cfg-'))
  try {
    await writeFile(path.join(dir, 'config.env'), 'LLM_MODEL=from-file\n')
    const { env } = resolveSidecarEnv({
      userDataDir: dir,
      appDir: '/nonexistent',
      processEnv: { LLM_MODEL: 'from-env' },
    })
    assert.equal(env.LLM_MODEL, 'from-env', '环境变量应覆盖配置文件')
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

await testAsync('用户级 config.env 优先于 .env', async () => {
  const userDir = await mkdtemp(path.join(tmpdir(), 'cfg-user-'))
  const appDir = await mkdtemp(path.join(tmpdir(), 'cfg-app-'))
  try {
    await writeFile(path.join(userDir, 'config.env'), 'LLM_MODEL=user-level\n')
    await writeFile(path.join(appDir, '.env'), 'LLM_MODEL=app-level\n')
    const { configFile, env } = resolveSidecarEnv({
      userDataDir: userDir,
      appDir,
      processEnv: {},
    })
    assert.equal(configFile, path.join(userDir, 'config.env'))
    assert.equal(env.LLM_MODEL, 'user-level')
  } finally {
    await rm(userDir, { recursive: true, force: true })
    await rm(appDir, { recursive: true, force: true })
  }
})

await testAsync('无配置文件时回退 mock', async () => {
  const { env, configFile, effective } = resolveSidecarEnv({
    userDataDir: '/nonexistent-a',
    appDir: '/nonexistent-b',
    processEnv: {},
  })
  assert.equal(configFile, null)
  assert.equal(effective.provider, 'mock')
  assert.equal(effective.hasKey, false)
  assert.equal(env.PORT, '0', '桌面端必须强制 PORT=0')
})

await testAsync('未设置 Key 时 hasKey 为 false（供日志提示）', async () => {
  const dir = await mkdtemp(path.join(tmpdir(), 'cfg-'))
  try {
    await writeFile(path.join(dir, 'config.env'), 'LLM_PROVIDER=openai\nOPENAI_API_KEY=\n')
    const { effective } = resolveSidecarEnv({
      userDataDir: dir,
      appDir: '/nonexistent',
      processEnv: {},
    })
    assert.equal(effective.provider, 'openai')
    assert.equal(effective.hasKey, false)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

console.log(`\n✅ 配置加载器测试通过（${passed} 项）\n`)
