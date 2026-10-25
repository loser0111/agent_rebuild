# 08 · 存储与多模型配置

## 1. SQLite 选型：用户不需要安装任何东西

| 项 | 结论 |
| --- | --- |
| 用户是否需要装 SQLite | **不需要**。SQLite 是嵌入式库，数据库就是一个文件 |
| 是否需要 C 编译器 | **不需要**（用纯 Go 驱动） |
| 是否破坏单二进制分发 | **不破坏** |

### 驱动选择

用 **`modernc.org/sqlite`**（SQLite 源码的 Go 翻译版）：

```
CGO_ENABLED=0 构建 ✅
交叉编译 Windows/Linux ✅
链接系统 sqlite 库 ❌（完全没有）
```

实测数据：

| 项 | 数值 |
| --- | --- |
| 二进制体积增量 | 约 +2~3 MB（9.1MB → 11~12MB） |
| 首次编译 | 慢 10~20 秒 |

**不要用 `mattn/go-sqlite3`**：它需要 CGO + 系统 SQLite 库，会破坏单文件分发与交叉编译。

---

## 2. 数据存放位置

数据库文件：`<DATA_DIR>/agent.db`（含 WAL 模式的 `-wal` / `-shm` 文件）

`DATA_DIR` 解析优先级：

1. 显式环境变量 `DATA_DIR`（Electron 会把 userData 传进来）
2. 平台默认：
   - macOS：`~/Library/Application Support/agent-rebuild`
   - Linux：`$XDG_DATA_HOME/agent-rebuild` 或 `~/.local/share/agent-rebuild`
   - Windows：`%LOCALAPPDATA%\agent-rebuild`

---

## 3. Schema

用 SQLite 的 `user_version` pragma 做版本管理。

| 表 | 用途 |
| --- | --- |
| `sessions` | 会话元信息 |
| `messages` | 消息（含工具调用字段、`seq` 保序） |
| `model_configs` | 多模型配置（API Key 加密） |
| `checkpoints` | agent 执行断点（为 A/B/C 预留） |

关键设计：

- **时间存 Unix 毫秒整数**：跨时区无歧义、排序快、比较无精度问题
- **消息用 `seq` 保序**：`created_at` 毫秒精度下同毫秒写入会乱序，`seq` 是会话内自增序号
- **外键 `ON DELETE CASCADE`**：删会话自动删消息与 checkpoint
- **WAL 模式**：写不阻塞读，崩溃恢复更好

### 迁移与兼容

```
user_version <  schemaVersion  → 逐步执行迁移
user_version == schemaVersion  → 直接使用
user_version >  schemaVersion  → 拒绝启动（ErrSchemaTooNew）
```

最后一条很重要：用户用旧版程序打开新版数据库时，**必须拒绝**而不是尝试兼容，否则会损坏数据。

---

## 4. 一个实测发现的严重缺陷（已修复）

### 症状

写入数据 → 优雅关闭 → 重启 → **数据全部消失**。

### 根因

`main.go` 只关闭了 HTTP 服务，**从未关闭 `*sql.DB`**。

SQLite 在 WAL 模式下，新写入先进 `agent.db-wal`，靠 checkpoint 合并进主库。**进程退出时若没有 `Close()`，WAL 不 checkpoint** —— 数据虽在磁盘上，但下次打开时 WAL 可能被视为无效/不一致。

### 修复

`buildDataLayer` 返回 `closer`，在 `run()` 里 `defer` 调用：

```go
store, runner, models, closer, err := buildDataLayer(cfg)
if err != nil {
    return err
}
defer func() {
    if closer != nil {
        if closeErr := closer(); closeErr != nil {
            slog.Error("关闭数据库失败", "err", closeErr)
        }
    }
}()
```

### 验证

| 验证 | 结果 |
| --- | --- |
| 关闭后文件 | `agent.db-wal` 消失（已 checkpoint） |
| 重启后会话 | 1 条，`message_count=2`，内容完整 |
| 回归测试 | `TestRepeatedOpenCloseKeepsData` 连续 5 轮开关，数据零丢失 |

**教训**：SQLite 的 `Close()` 不是可选的清理动作，而是数据安全性的一部分。

---

## 5. 多模型配置

### 数据模型

```go
type Config struct {
    ID           string
    Name         string   // 用户自定义显示名（唯一）
    Protocol     string   // openai | anthropic | mock
    Provider     string   // deepseek | anthropic | ...（决定预设端点）
    BaseURL      string   // 留空用预设
    Model        string
    SystemPrompt string
    Temperature  *float64
    MaxTokens    int
    IsDefault    bool     // 全局唯一
    HasAPIKey    bool     // 只读标记，不返回密钥
    APIKey       string   // 仅内部使用，JSON 不序列化
}
```

### API Key 安全

| 措施 | 实现 |
| --- | --- |
| 加密落盘 | AES-GCM，密钥 = `SHA256("agent_rebuild:" + dataDir绝对路径)` |
| 数据库被拷走 | 无法解密（派生密钥含机器路径） |
| API 响应 | 永不返回明文，只返回 `has_api_key: bool` |
| 日志 | 只打印"已设置/未设置" |
| 更新时不传密钥 | 保留原密钥（前端拿不到明文，无法回填） |

**局限性（诚实说明）**：这防的是「数据库文件泄漏」，不是「本机恶意程序」。真正的方案是 OS Keychain（macOS Keychain / Windows Credential Manager），后续可替换 `Cipher` 实现。

### REST 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/models` | 列出全部（含当前生效的 protocol/model） |
| `POST` | `/api/models` | 新建（首条自动设为默认） |
| `PUT` | `/api/models/{id}` | 更新（`api_key` 留空=不改） |
| `DELETE` | `/api/models/{id}` | 删除（自动指定新默认） |
| `POST` | `/api/models/{id}/default` | 设为默认并**立即生效** |
| `POST` | `/api/models/{id}/test` | 连通性测试（发一次最小请求） |

### 运行时切换

`POST /api/models/{id}/default` 会：

1. 更新数据库的 `is_default`（全局唯一）
2. 用新配置重建 `Runner`（协议/地址/密钥都可能不同）
3. 赋值给 `Server.runner`

因为 `executor` 通过 `runnerFn func() agent.Runner` **延迟读取**，切换立即生效，**无需重启**。实测：

```
切换前: {"protocol":"openai","model":"deepseek-chat"}
切换后: {"protocol":"anthropic","model":"claude-3-5-sonnet-latest"}
```

### 启动时的模型解析顺序

```
数据库默认配置  >  环境变量（.env）
```

这样既支持界面配置（存库），也保留 `.env` 的老路子。

---

## 6. 两种存储驱动对比

| | `memory` | `sqlite` |
| --- | --- | --- |
| 会话持久化 | ❌ 重启即失 | ✅ |
| 模型配置 | ❌（接口 501） | ✅ |
| 适用场景 | 快速试玩、CI | 日常使用、桌面版 |
| 依赖 | 无 | `modernc.org/sqlite` |

切换只需改 `STORE_DRIVER`。

---

## 7. 验证清单

```bash
# 单元测试（含契约测试：SQLite 与内存实现行为必须完全一致）
go test ./internal/session ./internal/storage/sqlite ./internal/modelconfig

# 契约测试专用
go test -run TestSQLiteStoreContract ./internal/session
```

已覆盖：

| 测试 | 验证内容 |
| --- | --- |
| `TestSQLiteStoreContract` | 与内存实现完全相同的 11 项契约（含裁剪模式） |
| `TestPersistenceAcrossReopen` | 关库重开后数据完整（含 usage） |
| `TestMessageOrderStableAfterReopen` | 20 条消息跨重启顺序不乱 |
| `TestCascadeDelete` | 删会话级联删消息 |
| `TestSchemaTooNewRejected` | 版本过新拒绝打开 |
| `TestWALEnabled` | WAL 模式生效 |
| `TestRepeatedOpenCloseKeepsData` | 连续 5 轮开关零丢失 |
| `TestAPIKeyEncryptedAtRest` | 直接查库确认无明文 |
| `TestCipherDifferentSecrets` | 换机器无法解密 |
| `TestListDoesNotLeakKey` | 列表接口不返回密钥 |
| `TestMultiModelSupport` | 4 个配置共存、恰好 1 个默认 |
| `TestDeleteKeepsDefault` | 删默认后自动选新默认 |
| `TestUpdatePreservesKeyWhenEmpty` | 空密钥不覆盖原值 |

---

## 8. 已知限制

| 限制 | 说明 |
| --- | --- |
| 单写者 | SQLite 只允许一个写连接（`SetMaxOpenConns(1)`）。单用户本地应用够用，多实例部署需换 Postgres |
| 消息裁剪 | 超 `MAX_MESSAGES_PER_SESSION` 静默删最旧（保留 system）。有 SQLite 后可考虑改为归档 |
| 密钥加密强度 | 见上文"局限性"，非 OS Keychain |
| checkpoint 表已建但未使用 | schema 预留，待 A/B/C 落地 |
| 桌面端未接入 | Electron 尚未传 `DATA_DIR`，需在 `main.js` 补 |
