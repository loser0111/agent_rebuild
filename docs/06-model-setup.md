# 06 · 接入真实模型

默认是 **Mock 模式**（`LLM_PROVIDER=mock`），回复为固定演示文案，不需要任何 Key。

## 概念：协议 ≠ 厂商

配置分成两个维度，这是本项目的核心设计：

| 配置项 | 含义 | 作用 |
| --- | --- | --- |
| `LLM_PROTOCOL` | **协议族** | 决定用哪个 Runner（代码路径） |
| `LLM_PROVIDER` | **服务商** | 决定 BaseURL 与默认模型（预设数据） |

为什么分开：

- 同一协议能对接大量厂商（OpenAI 兼容阵营极大：DeepSeek / Ollama / vLLM / 各类网关）
- 同一厂商可能提供多种协议（Anthropic 用 Messages API，不兼容 OpenAI）
- 只用一个字段会导致「`LLM_PROVIDER=openai` 但实际连的是 Ollama」这类语义混乱

**向后兼容**：`LLM_PROTOCOL` 可以完全不写，此时按 `LLM_PROVIDER` 的预设自动推断。所有旧配置无需修改。

### 已实现的协议

| `LLM_PROTOCOL` | 端点 | 鉴权 | Runner |
| --- | --- | --- | --- |
| `openai`（默认） | `POST /chat/completions` | `Authorization: Bearer <key>` | `LLMRunner` |
| `anthropic` | `POST /messages` | `x-api-key` + `anthropic-version` | `AnthropicRunner` |
| `mock` | 无网络请求 | — | `MockRunner` |

别名：`claude` / `anthropic-api` / `messages` → `anthropic`；`openai-compat` → `openai`。

---

## 1. 支持的 provider 别名

`LLM_PROVIDER` 大小写不敏感、忽略首尾空格，并支持常见别名。

| `LLM_PROVIDER` | 协议 | 自动匹配的 BaseURL | 默认模型 | 需要 Key |
| --- | --- | --- | --- | --- |
| `mock` | mock | （无） | `mock-echo` | 否 |
| `openai` | openai | `https://api.openai.com/v1` | `gpt-4o-mini` | ✅ |
| `anthropic`（`claude`） | **anthropic** | `https://api.anthropic.com/v1` | `claude-3-5-sonnet-latest` | ✅ |
| `deepseek` | openai | `https://api.deepseek.com/v1` | `deepseek-chat` | ✅ |
| `moonshot`（`kimi`） | openai | `https://api.moonshot.cn/v1` | `moonshot-v1-8k` | ✅ |
| `zhipu`（`glm`） | openai | `https://open.bigmodel.cn/api/paas/v4` | `glm-4-flash` | ✅ |
| `qwen`（`dashscope`/`tongyi`） | openai | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `qwen-plus` | ✅ |
| `siliconflow` | openai | `https://api.siliconflow.cn/v1` | `Qwen/Qwen2.5-7B-Instruct` | ✅ |
| `openrouter` | openai | `https://openrouter.ai/api/v1` | `openai/gpt-4o-mini` | ✅ |
| `groq` | openai | `https://api.groq.com/openai/v1` | `llama-3.3-70b-versatile` | ✅ |
| `ollama`（`local`） | openai | `http://localhost:11434/v1` | `qwen2.5:7b` | 否 |
| `vllm` | openai | `http://localhost:8000/v1` | **需显式指定** | 否 |
| `lmstudio` | openai | `http://localhost:1234/v1` | **需显式指定** | 否 |

填了未知值会直接启动失败，并列出全部可选值——不会静默回退到 Mock。

---

## 2. 最小配置示例

### Anthropic（Claude）

```bash
LLM_PROVIDER=anthropic
OPENAI_API_KEY=sk-ant-你的密钥
```

只需两行。`LLM_PROTOCOL` 会自动推断为 `anthropic`，走 Messages API。
换模型：`LLM_MODEL=claude-3-opus-latest`

### DeepSeek

```bash
LLM_PROVIDER=deepseek
OPENAI_API_KEY=sk-你的deepseek密钥
```

换推理模型：`LLM_MODEL=deepseek-reasoner`

### Moonshot（Kimi）/ 智谱 GLM / 通义千问

```bash
LLM_PROVIDER=kimi      # 或 glm / qwen
OPENAI_API_KEY=你的密钥
```

### 本地 Ollama

```bash
LLM_PROVIDER=ollama
LLM_MODEL=qwen2.5:7b
```

先启动：`ollama serve && ollama pull qwen2.5:7b`

### 云端 Anthropic 兼容网关

```bash
LLM_PROTOCOL=anthropic
LLM_PROVIDER=anthropic
LLM_BASE_URL=https://your-proxy/v1
OPENAI_API_KEY=xxx
```

### 自定义 OpenAI 兼容网关

```bash
LLM_PROVIDER=openai
LLM_BASE_URL=http://my-gateway:8000/v1
LLM_MODEL=my-model
OPENAI_API_KEY=任意非空值
```

---

## 3. 配置优先级

```
显式环境变量  >  配置文件（config.env / .env）  >  provider 预设默认值
```

- `LLM_BASE_URL` 留空 → 用 provider 预设；有值 → **始终覆盖**
- `LLM_MODEL` 同理
- `LLM_PROTOCOL` 留空 → 按 provider 预设推断；有值 → 以它为准
- `mock` 模式下 BaseURL 会被清空（不发起任何网络请求）

### 组合校验

为避免「以为在调真模型，实际是假数据」：

- `LLM_PROTOCOL=mock` 只能配 `LLM_PROVIDER=mock`
- `LLM_PROVIDER=mock` 只能用 `LLM_PROTOCOL=mock`

除此之外的组合都允许（例如 openai 协议配任意厂商）。

---

## 4. 配置文件放哪里（关键）

| 运行方式 | 配置文件位置 |
| --- | --- |
| `make run` / `make demo` / `make dev` | 仓库根目录的 `.env` |
| 桌面版（`make desktop` / 打包的 .app） | **`<userData>/config.env`**（推荐） |
| 桌面版（开发态） | 也会回退读取仓库根目录的 `.env` |

打包后的 `.app` 由 Finder 启动时工作目录是 `/`，Go 进程自己**找不到**项目根目录的 `.env`。因此 Electron 主进程负责查找并注入，查找顺序：

1. 真实环境变量（优先级最高）
2. `<userData>/config.env`　← 桌面端推荐
3. `<可执行文件目录>/.env`　← 便携式
4. `<仓库根>/.env`　← 仅开发态

macOS 上通常是：

```bash
~/Library/Application\ Support/agent-rebuild-desktop/config.env
```

---

## 5. 如何确认配置生效

**健康检查**

```bash
curl localhost:8080/api/health
# {"protocol":"anthropic","provider":"anthropic","model":"claude-3-5-sonnet-latest",...}
```

`protocol` 与 `provider` 分别返回，能立刻看出用的是哪条代码路径。

**启动日志**

```
服务启动 addr=http://localhost:8080 provider=anthropic model=claude-3-5-sonnet-latest
```

**界面** 顶部显示 `provider · model`。

---

## 6. 常见问题

| 现象 | 原因 | 解决 |
| --- | --- | --- |
| `不支持的 LLM_PROVIDER="xxx"` | 别名拼错 | 按第 1 节表格；错误信息会列出可选值 |
| `不支持的 LLM_PROTOCOL="xxx"` | 协议名拼错 | 只能是 `openai` / `anthropic` / `mock` |
| `LLM_PROTOCOL=mock 只能与 LLM_PROVIDER=mock 搭配` | 组合不自洽 | 去掉 `LLM_PROTOCOL` 或改成正确协议 |
| `需要设置 OPENAI_API_KEY` | 云端服务商必须有 Key | 补上（Anthropic 的 Key 也填这里） |
| `需要显式指定 LLM_MODEL` | `vllm`/`lmstudio` 无默认模型 | 加上 `LLM_MODEL` |
| 配了 Key 仍是 Mock | 配置文件路径不对 | 看启动日志的 `已读取配置文件` |
| 报 401 | Key 错误/过期 | 重新生成 |
| 报 404 | BaseURL 路径不对 | 智谱是 `/v4`，其他多为 `/v1` |

### 排查顺序

```bash
# 1. 绕过本项目，先确认网关可用（Anthropic 示例）
curl -s https://api.anthropic.com/v1/messages \
  -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model":"claude-3-5-sonnet-latest","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'

# 2. 确认后端读到的配置
curl -s localhost:8080/api/health | python3 -m json.tool

# 3. debug 日志
LOG_LEVEL=debug make run
```

---

## 7. 安全提醒

- `.env` 与 `config.env` 已在 `.gitignore` 排除，**不要提交**。
- 桌面版日志只打印 `apiKey=已设置/未设置`，**不打印 Key 明文**（有测试守着）。
- 后端不会把上游原始错误体透传给浏览器，只返回归一化错误码。

---

## 8. 扩展新的 provider / 协议

### 新增 provider（同一协议，新厂商）

编辑 `internal/config/providers.go` 的 `providers` map：

```go
"newprovider": {
    Name:         "newprovider",
    Protocol:     ProtocolOpenAI,      // 沿用现有协议
    BaseURL:      "https://api.newprovider.com/v1",
    DefaultModel: "their-model",
    RequiresKey:  true,
},
```

再在 `ProviderNames()` 的 `order` 里加名字，补一条测试。

### 新增协议（如 Google Gemini）

1. 在 `internal/agent` 实现新 Runner：

```go
type Runner interface {
    Run(ctx context.Context, history []session.Message) <-chan Event
}
```

只需产出 `TypeDelta` / `TypeDone` / `TypeError` 事件——**无需关心 HTTP、SSE、会话、前端**，这些层完全不感知协议。

2. 在 `internal/config/providers.go` 加协议常量、别名、`ProtocolNames()`。
3. 在 `cmd/server/main.go` 的 `buildRunner` 加一个 `case`。

这正是把协议与厂商分离的收益：新增一种协议是**纯增量**改动。
