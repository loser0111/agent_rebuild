/**
 * 读取桌面端配置文件并转换为环境变量。
 *
 * 为什么需要：Go sidecar 只在「进程工作目录」查找 .env，
 * 而打包后的 .app 由 Finder/launchd 启动时工作目录是 /，
 * 导致用户放在项目根目录（或应用数据目录）的 .env 永远读不到，
 * 表现为「配了 Key 却仍走 Mock 模式」（静默失败，极难排查）。
 *
 * 因此由主进程负责查找并解析配置，显式注入给 sidecar。
 *
 * 查找顺序（先命中先用，后者不覆盖前者）：
 *   1. 真实进程环境变量（用户 export 的）
 *   2. 应用数据目录下的 config.env（桌面端推荐的用户级配置）
 *   3. 可执行文件旁边的 .env（便携式放置）
 *   4. 仓库根目录的 .env（开发态）
 */
const fs = require('node:fs')
const path = require('node:path')

/** 解析 .env 文本为键值对。支持注释、export 前缀、引号包裹。 */
function parseDotEnv(text) {
  const out = {}
  for (const rawLine of text.split(/\r?\n/)) {
    const line = rawLine.trim()
    if (line === '' || line.startsWith('#')) continue

    const withoutExport = line.startsWith('export ') ? line.slice(7) : line
    const eq = withoutExport.indexOf('=')
    if (eq <= 0) continue

    const key = withoutExport.slice(0, eq).trim()
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) continue

    let value = withoutExport.slice(eq + 1).trim()
    // 去掉成对引号（保留内部空格与 #）。
    if (
      (value.startsWith('"') && value.endsWith('"') && value.length >= 2) ||
      (value.startsWith("'") && value.endsWith("'") && value.length >= 2)
    ) {
      value = value.slice(1, -1)
    } else {
      // 未加引号时，行尾的 " # 注释" 应被剥离。
      const hash = value.indexOf(' #')
      if (hash !== -1) value = value.slice(0, hash).trim()
    }
    out[key] = value
  }
  return out
}

/** 从候选路径中读取第一个存在的配置文件。 */
function loadConfigFile(candidates, log) {
  for (const file of candidates) {
    try {
      if (!fs.existsSync(file)) continue
      const text = fs.readFileSync(file, 'utf8')
      const parsed = parseDotEnv(text)
      const count = Object.keys(parsed).length
      if (log) log(`已读取配置文件: ${file}（${count} 项）`)
      return { file, values: parsed }
    } catch (err) {
      if (log) log(`读取配置失败 ${file}: ${err.message}`)
    }
  }
  return { file: null, values: {} }
}

/**
 * 解析 sidecar 应使用的环境变量。
 *
 * @param {object} opts
 * @param {string} opts.userDataDir 应用数据目录（存放用户级 config.env）
 * @param {string} opts.appDir      应用所在目录（打包态用于查找同级 .env）
 * @param {string} [opts.devRoot]   开发态仓库根目录
 * @param {object} opts.processEnv  真实进程环境变量
 * @param {function} [opts.log]     日志函数
 * @returns {{ env: object, configFile: string|null, effective: object }}
 */
function resolveSidecarEnv(opts) {
  const { userDataDir, appDir, devRoot, processEnv, log } = opts

  const candidates = [
    // 桌面端推荐位置：用户可在应用数据目录放 config.env，升级应用不会丢失。
    path.join(userDataDir, 'config.env'),
    // 便携式：与可执行文件同级。
    path.join(appDir, '.env'),
  ]
  if (devRoot) {
    candidates.push(path.join(devRoot, '.env'))
  }

  const { file, values } = loadConfigFile(candidates, log)

  // 优先级：真实环境变量 > 配置文件（避免覆盖用户显式 export 的值）。
  const merged = { ...values }
  for (const [k, v] of Object.entries(processEnv)) {
    if (v !== undefined) merged[k] = v
  }

  // 桌面端固定项（必须在最后强制覆盖）。
  merged.PORT = '0'
  merged.WEB_DIR = merged.WEB_DIR || ''

  // 供日志/健康检查展示的关键项（不含密钥明文）。
  const effective = {
    protocol: merged.LLM_PROTOCOL || '(自动推断)',
    provider: merged.LLM_PROVIDER || 'mock',
    model: merged.LLM_MODEL || '(preset)',
    baseURL: merged.LLM_BASE_URL || '(preset)',
    hasKey: Boolean(merged.OPENAI_API_KEY && merged.OPENAI_API_KEY.trim()),
    systemPromptSet: Boolean(merged.SYSTEM_PROMPT && merged.SYSTEM_PROMPT.trim()),
  }

  return { env: merged, configFile: file, effective }
}

module.exports = { parseDotEnv, loadConfigFile, resolveSidecarEnv }
