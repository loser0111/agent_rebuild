# 超时时间的深度分析与改进方案

## 🤔 问题重新审视

你的concern完全合理。让我们重新分析 `persistCtx` 的超时时间应该是多少。

---

## 📊 时间流的关键分析

### 当前理解（有缺陷）

```
runCtx 生命周期：0 ─────────────── T (可能是 1小时)
                 │
                 ├─ persistCtx：T₀ ─── T₀+2min (固定)
                 │
                 ├─ runCtx 最后一个工具调用发生在 T-10s
                 │  └─ 落库需要 5s
                 │     └─ 但 persistCtx 已经过期了！❌
```

### 问题所在

当你设置 `RunTimeout = 1小时` 时，runCtx 会运行 1 小时。但 `persistCtx` 只有 2 分钟，这意味着：

```
场景：1 小时的长任务，最后才调用工具

t=0min      ├─ persistCtx 创建（2min 超时）
t=2min      ├─ persistCtx 超时！ ❌
t=45min     ├─ 模型终于完成思考...
t=50min     ├─ 启动工具调用
t=50:05min  ├─ 尝试落库 ❌❌❌ persistCtx 早就过期了！
t=60min     └─ runCtx 结束
```

**结果**：即使给 persistCtx 设置 2 分钟，长任务仍然会失败。

---

## ⚠️ 更深层的设计问题

### 当前的 persistCtx 设计有个重大缺陷

```go
persistCtx, persistCancel := context.WithTimeout(
    context.Background(), 
    30*time.Second  // 或我之前建议的 2分钟
)
```

**问题**：
1. ❌ 无论 runTimeout 多长，persistCtx 都是固定的
2. ❌ 最后一个工具调用如果超过这个时间，就会丢失
3. ❌ 这个设置与实际的执行模式不匹配

---

## 🎯 正确的解决方案

### 方案 1：完全移除 persistCtx 的超时限制（推荐）

**思路**：persistCtx 不应该有超时，原因是：

1. **executorCtx 已经有超时了**
   - runCtx 由 runTimeout 控制，会及时中止
   - 最坏情况下，runCtx 取消后 goroutine 会自动退出

2. **数据必须被写入**
   - 即使执行很长，落库操作也必须完成
   - 数据库卡死是另一个问题（需要数据库级别的处理）

**代码**：
```go
// 完全移除 persistCtx 的超时限制
persistCtx := context.Background()

// 但在关键地方仍然添加日志防护
func (e *Executor) appendToolCallMessage(ctx context.Context, sessionID string, call agent.ToolCall) {
    if e.store == nil {
        return
    }
    now := time.Now().UTC()
    m := &session.Message{
        // ...
    }
    
    // 添加单个操作的超时（比如 10s），而不是整体超时
    writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    
    if err := e.store.AppendMessage(writeCtx, m); err != nil {
        slog.Warn("工具调用消息落库失败", "err", err,
            "session_id", sessionID, "call_id", call.ID)
        // 重要：记录错误，便于监控
        if errors.Is(err, context.DeadlineExceeded) {
            metrics.Inc("persist_single_operation_timeout")
        }
    }
}
```

**优点**：
- ✅ 永远不会丢失数据
- ✅ 适应任意长度的任务
- ✅ 简单清晰

**风险**：
- 需要信任 runCtx 会及时取消
- 需要良好的数据库连接管理

---

### 方案 2：移除 persistCtx 整体超时，但保留单个操作超时

这是**最推荐的方案**。

**原理**：
```
老的思路：整个持久化阶段有总超时（30s 或 2min）
新的思路：每个数据库操作有独立超时（10-30s）
         但不限制整体的持久化时间
```

**代码改造**：

```go
func (e *Executor) run(
    runCtx context.Context,
    task *Task,
    req Request,
    cancel context.CancelFunc,
    runner agent.Runner,
) {
    defer cancel()
    defer close(task.done)
    defer func() {
        e.mu.Lock()
        delete(e.tasks, req.SessionID)
        e.mu.Unlock()
    }()

    // 改变策略：不设置整体超时，只有 runCtx 有超时
    persistCtx := context.Background()

    finish := func(
        status Status,
        content string,
        msgStatus session.MessageStatus,
        msgErr *session.MessageError,
        usage *sse.Usage,
    ) {
        task.mu.Lock()
        task.status = status
        task.mu.Unlock()
        
        // 最终落库用单个操作的超时（而不是整体超时）
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        e.persistMessage(ctx, req.SessionID, req.MessageID, content, msgStatus, msgErr, usage)
    }

    // ... 后续代码保持不变 ...
}

// 同时修改 appendToolCallMessage 和 appendToolResultMessage
func (e *Executor) appendToolCallMessage(ctx context.Context, sessionID string, call agent.ToolCall) {
    if e.store == nil {
        return
    }
    
    // 单个操作的短超时（而不是依赖外层的 persistCtx）
    opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    
    now := time.Now().UTC()
    m := &session.Message{
        ID:        session.NewMessageID(),
        SessionID: sessionID,
        Role:      session.RoleAssistant,
        Content:   "",
        Status:    session.StatusComplete,
        ToolCalls: []session.ToolCall{{
            ID: call.ID, Name: call.Name, Args: call.Args,
        }},
        CreatedAt: now,
        UpdatedAt: now,
    }
    if err := e.store.AppendMessage(opCtx, m); err != nil {
        slog.Warn("工具调用消息落库失败", "err", err,
            "session_id", sessionID, "call_id", call.ID)
    }
}

func (e *Executor) appendToolResultMessage(ctx context.Context, sessionID string, r agent.ToolResult) {
    if e.store == nil {
        return
    }
    
    opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    
    status := session.StatusComplete
    if r.IsError {
        status = session.StatusError
    }
    now := time.Now().UTC()
    m := &session.Message{
        ID:         session.NewMessageID(),
        SessionID:  sessionID,
        Role:       session.RoleTool,
        Content:    r.Output,
        Status:     status,
        ToolCallID: r.CallID,
        CreatedAt:  now,
        UpdatedAt:  now,
    }
    if err := e.store.AppendMessage(opCtx, m); err != nil {
        slog.Warn("工具结果落库失败", "err", err,
            "session_id", sessionID, "call_id", r.CallID)
    }
}
```

---

## 📋 时间限制的最佳实践

### 三层超时结构

```
┌─────────────────────────────────────────────────────────┐
│ 第 1 层：HTTP 请求超时（由 nginx/负载均衡器控制）        │
│ 示例：30 分钟                                            │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│ 第 2 层：执行超时（runCtx，由 req.RunTimeout 控制）      │
│ 示例：5 分钟/10 分钟（可配置）                           │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│ 第 3 层：单个数据库操作超时（per-operation）            │
│ 示例：15-30 秒                                           │
│ 不应有「全局持久化超时」                                │
└─────────────────────────────────────────────────────────┘
```

### 为什么这样设计

| 层级 | 超时理由 | 是否固定 |
|------|---------|--------|
| HTTP | 防止连接永久占用 | ✅ 固定（较长） |
| runCtx | 限制单个请求的计算量 | ✅ 固定或可配置 |
| 单操作 | 防止数据库真的卡死 | ✅ 固定（较短） |
| 全局持久化 | **不应该有！** | ❌ 移除 |

---

## 🎯 立即改进方案（只改 10 行代码）

### 修改位置

文件：`internal/executor/executor.go`

```go
// 第 417 行附近，改这段代码：

// 旧代码
persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)

// 新代码
// 移除全局 persistCtx 超时限制
// 每个操作有独立的短超时（15-30s），防止数据库卡死
// 但不限制整体持久化时间（可能很长）
persistCtx := context.Background()
```

### 同时修改工具落库函数

```go
// 第 734 行：appendToolCallMessage
func (e *Executor) appendToolCallMessage(ctx context.Context, sessionID string, call agent.ToolCall) {
    if e.store == nil {
        return
    }
    
    // 每个操作独立 15s 超时
    opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    
    // ... 后续代码 ...
    if err := e.store.AppendMessage(opCtx, m); err != nil {  // 改用 opCtx
        slog.Warn("工具调用消息落库失败", "err", err, ...)
    }
}

// 第 761 行：appendToolResultMessage
func (e *Executor) appendToolResultMessage(ctx context.Context, sessionID string, r agent.ToolResult) {
    if e.store == nil {
        return
    }
    
    // 每个操作独立 15s 超时
    opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    
    // ... 后续代码 ...
    if err := e.store.AppendMessage(opCtx, m); err != nil {  // 改用 opCtx
        slog.Warn("工具结果落库失败", "err", err, ...)
    }
}

// 第 788 行：persistMessage（最终消息落库）
func (e *Executor) persistMessage(
    ctx context.Context, sessionID, msgID, content string,
    msgStatus session.MessageStatus, msgErr *session.MessageError, usage *sse.Usage,
) {
    if e.store == nil {
        return
    }
    
    // 最终消息落库用 30s 超时（比工具消息稍长）
    opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    err := e.store.UpdateMessage(opCtx, sessionID, msgID, func(m *session.Message) error {
        // ... 内容 ...
    })
}
```

---

## 📊 改进前后对比

### 改进前（有问题）

```
场景：1 小时的任务，最后调用工具

持久化上下文超时：
  - persistCtx = 30s  ❌ 太短
  - 或 persistCtx = 2min  ❌ 还是太短

结果：长任务的工具调用会失败
```

### 改进后（正确）

```
场景：任意长度的任务，随时调用工具

超时策略：
  - 全局持久化超时：❌ 移除（没有限制）
  - 每个操作超时：✅ 15-30s（防止DB卡死）
  - runCtx 超时：✅ 可配置（限制计算）

结果：
  ✅ 任意长度的任务都能正常落库
  ✅ 单个操作卡死仍然会报错（防护措施）
  ✅ 不会因为超时丢失数据
```

---

## ⚡ 修改总结

### 改什么
1. 移除 `persistCtx` 的全局超时
2. 每个数据库操作添加独立的、较短的超时（15-30s）

### 为什么
- ✅ 支持任意长度的任务
- ✅ 仍然防护数据库卡死
- ✅ 不会丢失工具调用记录

### 改多少代码
- 核心：3 处修改（main run 函数、appendToolCall、appendToolResult）
- 总共：约 20 行代码改动

---

## 🎓 这个设计的启发

这是分布式系统中的**经典模式**：

```
❌ 错误做法：单一的"全局超时"
   - 太短：丢数据
   - 太长：浪费资源

✅ 正确做法：分层超时
   - 全局执行超时（runCtx）：控制整体时间
   - 单个操作超时：防护局部问题
   - 无"持久化超时"：确保数据安全
```

这也是为什么很多生产系统都采用"单个操作有超时，但无全局持久化超时"的设计。

