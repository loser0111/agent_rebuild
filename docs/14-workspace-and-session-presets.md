# 14 · Workspace 绑定与会话级预设（Model / Approval Mode）

> 背景：用户提出"会话可以绑定一个 workspace，workspace 可以绑定多个文件夹；workspace 内外的
> 文件权限粗细不同"的构想（见对话记录）。经讨论拆成两段落地：
>
> 1. **本次（这篇文档覆盖的范围）**：只做"会话 ↔ workspace"的绑定关系与持久化，
>    以及"会话创建时预设模型 + 审批模式，之后仍可改、但重新打开会话不会被全局设置覆盖"。
>    **不改动任何权限执行逻辑**——`internal/tool/fs_tools.go` 的越界校验、
>    `run_command` 的风险分级都保持原样。
> 2. **以后（不在本次范围内）**：把 workspace 的多文件夹列表真正接入
>    `tool.Registry` 的路径校验（workspace 内直通、外部触发新的审批挂起），
>    这是一次独立的、涉及 `internal/tool/fs_tools.go` 和挂起机制的改动，
>    留到下一轮再做设计。

---

## 0. 结论先行

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| workspace 的建模方式 | **独立实体**（新表 `workspaces`，有自己的 id/name），会话只存 `workspace_id` | 同一组文件夹可能被多个会话复用（例如都在做同一个代码仓库相关的任务），参照 `model_configs` 的先例：可复用配置独立建表，会话只存引用 ID。 |
| 会话级 `approval_mode` 能否事后改 | **能**：创建时可预设，之后可 `PATCH` 修改 | 和已有的 `model` / `model_config_id` 字段语义完全对齐——会话级选择，非一次性锁死。 |
| 空值语义 | 会话的 `workspace_id` / `approval_mode` 留空 = 跟随进程级全局默认 | 与 `ModelConfigID` 为空时回退全局默认模型完全一致的既有约定，不引入第二套语义。 |
| 本次要不要改 `fs_tools.go` 的路径校验 | **不改** | 用户已明确要求先只做绑定关系，权限执行逻辑是下一阶段的事，范围要分清楚。 |

---

## 1. 现状回顾

在动手之前，先确认了两件事（均已读代码核实）：

- **"会话级选模型"这件事其实已经做完了**：`session.Session` 早就有 `Model` / `ModelConfigID`
  两个字段，`POST /api/sessions` 可以传 `model`，`PATCH /api/sessions/{id}` 可以改
  `model` / `model_config_id`，`handleSendMessage` 发消息时优先用请求里显式指定的值，
  否则回退到会话上次记录的值（`internal/api/messages.go:113-129`）。
  这次**不需要重新发明**，只需要照抄这套模式。
- **"审批模式"目前是进程级全局单值**，和具体会话毫无关系：`Server.approvalMode`
  / `Executor.approver` 都是单个字段，`PUT /api/approval-mode` 切换会影响**所有**
  正在跑的会话（见 `internal/api/server.go` 的 `SetApprovalMode` 注释）。这是本次
  要补的缺口——把它也变成"有会话级覆盖、覆盖值持久化、空值回退全局"的模式。
- `tool.Registry` 目前只支持单一 `Workspace string` 根目录（`internal/tool/registry.go`），
  是一个硬边界（越界直接报错，见 `fs_tools.go` 的 `resolve()`），不是"内松外紧"的分级模型。
  这次新增的 `workspace_id` **只落库、可查询**，暂不会改变任何一次工具调用的实际行为。

---

## 2. 数据模型

### 2.1 新表 `workspaces`

```sql
CREATE TABLE IF NOT EXISTS workspaces (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    -- folders 是 JSON 字符串数组，如 ["/Users/x/proj-a", "/Users/x/proj-b"]。
    -- 不拆成子表：folders 不参与任何查询条件（不会按"某个文件夹属于哪些 workspace"
    -- 去查），JSON 文本列完全够用，拆表反而增加一次 JOIN 成本。
    folders    TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
```

校验规则（`workspace.Input.Validate()`）：

- `Name` 去空白后不能为空，最长 60 字（与 `modelconfig.Input` 一致的上限）。
- `Folders` 去空白、去重、丢弃空字符串；**至少保留 1 个**；每个必须是绝对路径
  （`filepath.IsAbs`），否则在跨会话/跨进程重新打开时相对路径的含义会变
  （相对于谁？不同会话 cwd 可能不同），直接拒绝更安全。
- 不校验文件夹在磁盘上是否真实存在——创建 workspace 时文件夹可能还没创建，
  或者数据库文件被同步到另一台机器，这类校验留给以后真正接入路径执行时再做。

### 2.2 `sessions` 表新增两列

```sql
ALTER TABLE sessions ADD COLUMN workspace_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN approval_mode  TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sessions_workspace ON sessions(workspace_id);
```

**刻意不加外键约束**（对比 `messages.session_id` 有 `FOREIGN KEY ... ON DELETE CASCADE`）：
删除一个 workspace 不应该级联影响引用它的会话——这与现有 `model_config_id` 的处理方式
一致：删除模型配置后，引用它的会话字段原样保留，等到真正发消息时才在执行层报
"配置已不存在"的错误（`executor.ErrModelConfigInvalid`），而不是在删除那一刻去扫描
并清洗所有会话。本次 `workspace_id` 失效时的处理照抄同一套做法（见 §4）。

`approval_mode` 存的是裸字符串（`""` / `"manual"` / `"model"` / `"auto"`），**不在
`internal/session` 包内做取值校验**——该包保持零 internal 依赖的纯领域包定位
（现状确认过：`internal/session` 不 import 任何 `agent_rebuild/internal/*`），
真正的取值校验用 `config.NormalizeApprovalMode`，放在 API 层（已经存在，`PUT
/api/approval-mode` 也是这么做的）。

---

## 3. workspace 包（仿 `internal/modelconfig` 风格）

```go
package workspace

type Workspace struct {
    ID        string
    Name      string
    Folders   []string
    CreatedAt time.Time
    UpdatedAt time.Time
}

type Input struct {
    Name    string
    Folders []string
}

type Store interface {
    Create(ctx context.Context, in Input) (*Workspace, error)
    Get(ctx context.Context, id string) (*Workspace, error)
    List(ctx context.Context) ([]*Workspace, error)
    Update(ctx context.Context, id string, in Input) (*Workspace, error)
    Delete(ctx context.Context, id string) error
}
```

`SQLStore` 实现直接照搬 `modelconfig.SQLStore` 的结构（`SQLQuerier` / `SQLTx` 抽象、
`beginTx` 的 noop 事务退化、`toMillis`/`fromMillis`），但去掉了"默认配置"
（`is_default` / `SetDefault` / `ensureDefault`）那一整套——workspace 之间没有互斥的
"默认"概念，会话不选就是空，不需要兜底成某一个。

---

## 4. 执行层：会话级 `approval_mode` 覆盖

照抄 `RunnerResolver` 已经验证过的模式（见 `internal/executor/executor.go` 的
`ResolveRunner` / `SetResolver`），而不是另起一套新机制：

```go
// executor.go 新增：
type ApproverResolver func(mode string) (loop.Approver, error)

type Executor struct {
    // ...现有字段...
    approverResolver ApproverResolver // nil 表示不支持会话级覆盖
}

func (e *Executor) SetApproverResolver(r ApproverResolver) { ... }

// Request 新增一个字段：
type Request struct {
    // ...
    ApprovalMode string // 会话记录的审批模式；为空则用 e.approver（全局默认）
}
```

`streamEvents` 构造 `loop.Config` 时：

```go
approver := e.approver // 全局默认，始终作为兜底
if req.ApprovalMode != "" && e.approverResolver != nil {
    if resolved, err := e.approverResolver(req.ApprovalMode); err != nil {
        slog.Warn("会话审批模式解析失败，回退全局默认", "err", err, "session_id", req.SessionID)
    } else {
        approver = resolved
    }
}
```

`Server.SetTools` 里顺手注入 resolver：

```go
s.exec.SetApproverResolver(func(mode string) (loop.Approver, error) {
    normalized, err := config.NormalizeApprovalMode(mode)
    if err != nil {
        return nil, err
    }
    return s.buildApprover(normalized), nil
})
```

这样「进程级全局开关」（`PUT /api/approval-mode`）和「会话级覆盖」两种状态同时存在、
互不打架：会话没设置时完全沿用全局开关的实时值（因为 `e.approver` 本身可以被
`SetApprovalMode` 热替换）；会话设置了，则这一会话固定用自己的模式，不随全局开关
波动——这正是"重新打开会话时不会被全局设置改变"这个需求点。

---

## 5. API 变更

- `POST /api/sessions`：`createSessionRequest` 新增 `workspace_id`、`approval_mode`。
  - `workspace_id` 非空时先用 `s.workspaces.Get` 校验存在，不存在返回 404
    （参照现有 `model_config_id` 在创建会话时……实际上现状创建接口还不支持传
    `model_config_id`，只能 PATCH 设置；这次 `workspace_id`/`approval_mode`
    选择在创建接口里就支持，是对现有模式的一个小补强，不是倒退）。
  - `approval_mode` 非空时用 `config.NormalizeApprovalMode` 校验，非法值 400。
- `PATCH /api/sessions/{id}`：`patchSessionRequest` 同样新增这两个可选字段，校验逻辑一致。
- 新增 workspace 的 CRUD：
  - `GET /api/workspaces`、`POST /api/workspaces`
  - `PUT /api/workspaces/{id}`、`DELETE /api/workspaces/{id}`
  - 风格对齐 `internal/api/modelconfigs.go`（`requireModels` → `requireWorkspaces`，
    `respondModelError` → `respondWorkspaceError`）。
- `internal/api/messages.go` / `resume.go`：发消息、resume 时把 `sess.ApprovalMode`
  传进 `executor.Request.ApprovalMode`，与现有 `modelConfigID` 透传完全同构。

---

## 6. 已知限制 / 明确不做的事

1. **路径权限执行完全不受影响**。`workspace_id` 现在只是会话上的一个"标签"，
   `tool.Registry` 依然用启动时配置的单一 `TOOL_WORKSPACE` 根目录，和会话绑的
   workspace 没有任何运行时关联。下一步如果要把两者接起来，需要：
   - 把 `fsConfig.resolve()` 从"单 root"改成"多 root 命中任一即放行"；
   - 决定"越界"时是直接报错（现状）还是挂起等用户临时授权新增文件夹
     （对应 `session.PendingApproval` 之外再加一个 `PendingKind`）；
   - 补 `filepath.EvalSymlinks`，避免符号链接绕过多 root 校验。
   这些都刻意不在本次改动范围内。
2. **前端尚未改动**。创建会话对话框 / 会话设置面板目前还没有"选择 workspace"
   "选择审批模式"的 UI 入口，这些字段目前只能通过直接调用 API 设置。是否需要
   现在就补前端，由用户决定是否作为紧接着的下一步。
3. 本会话所在的沙箱没有 Go 工具链（见 `feedback_sandbox_no_go` 记忆），本次改动
   **没有实际跑过 `go build` / `go test`**，只做了详细的人工静态核查，用户需要在
   本机验证（清单见提交说明末尾）。
