# 超时问题的正确修复方案

> ⚠️ 更新：之前我建议 2 分钟超时，但这仍然不够。用户提出的问题是对的——长任务会超过这个时间。本文提供了更优的解决方案。

---

## 🎯 核心问题

**关键洞察**：`persistCtx` 不应该有一个固定的全局超时时间！

为什么？因为：
- 你的任务可能运行 1 小时、甚至更长
- 工具调用可能发生在任务的任何时刻
- 如果工具调用发生在最后，persistCtx 的全局超时会导致落库失败

```
错误的超时模型：
┌─ 整个 persistCtx 有全局超时 (30s / 2min / ?) ─┐
│                                              │
├─ 工具1 落库                                   │
├─ 工具2 落库                                   │
├─ ...                                        │
└─ 最后一个工具落库 ❌ 超时了（可能仍在超时范围内）  │
```

---

## ✅ 正确的做法（推荐）

### 策略：分离全局超时和单操作超时

**不给 persistCtx 设置全局超时，而是给每个数据库操作设置独立的短超时。**

```go
// 【不再需要】全局 persistCtx 超时
// persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)

// 【改为】
persistCtx := context.Background()  // 无超时限制

// 每个操作自己有短超时
opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()
e.store.AppendMessage(opCtx, m)  // 这里用 opCtx，不用 persistCtx
```

---

## 📝 具体代码改动

### 位置 1：主函数 (executor.go:417)

**改这段**：
```go
// 旧
persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)

// 新
persistCtx := context.Background()
```

### 位置 2：工具调用落库 (executor.go:734)

```go
func (e *Executor) appendToolCallMessage(ctx context.Context, sessionID string, call agent.ToolCall) {
    if e.store == nil {
        return
    }
    
    // 添加这 3 行
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
    if err := e.store.AppendMessage(opCtx, m); err != nil {  // 改用 opCtx
        slog.Warn("工具调用消息落库失败", "err", err,
            "session_id", sessionID, "call_id", call.ID)
    }
}
```

### 位置 3：工具结果落库 (executor.go:761)

```go
func (e *Executor) appendToolResultMessage(ctx context.Context, sessionID string, r agent.ToolResult) {
    if e.store == nil {
        return
    }
    
    // 添加这 3 行
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
    if err := e.store.AppendMessage(opCtx, m); err != nil {  // 改用 opCtx
        slog.Warn("工具结果落库失败", "err", err,
            "session_id", sessionID, "call_id", r.CallID)
    }
}
```

### 位置 4：最终消息落库 (executor.go:788)

```go
func (e *Executor) persistMessage(
    ctx context.Context, sessionID, msgID, content string,
    msgStatus session.MessageStatus, msgErr *session.MessageError, usage *sse.Usage,
) {
    if e.store == nil {
        return
    }
    
    // 添加这 3 行（最终消息可以用稍长的超时，比如 30s）
    opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    err := e.store.UpdateMessage(opCtx, sessionID, msgID, func(m *session.Message) error {
        // 保持原有逻辑
        m.Content = content
        m.Status = msgStatus
        m.Error = msgErr
        if usage != nil {
            m.Usage = &session.Usage{
                PromptTokens:     usage.PromptTokens,
                CompletionTokens: usage.CompletionTokens,
                TotalTokens:      usage.TotalTokens,
            }
        }
        return nil
    })
    if err != nil {
        // 保持原有错误处理
    }
}
```

---

## 🔍 为什么这样做

### 旧方案的问题

```
persistCtx 全局超时 = 2 分钟
│
├─ 1 小时任务开始
├─ (等等... 工具调用发生在 59 分钟后)
├─ 工具调用时 ❌ persistCtx 已经超时了
└─ 结果：落库失败
```

### 新方案的优势

```
persistCtx 无全局超时
│
├─ 1 小时任务开始
├─ (等等... 工具调用发生在 59 分钟后)
├─ 工具调用时
│  ├─ 创建新的 opCtx (15s 超时)
│  ├─ 立即执行落库 (假设 2s 完成)
│  └─ ✅ 成功！
└─ 即使是最后一秒的工具调用也能成功
```

---

## ✨ 三层超时结构

你的系统应该这样设计超时：

```
【第 1 层】HTTP 请求超时
    由负载均衡器/nginx 控制
    示例：30 分钟（很长）

         ↓

【第 2 层】执行超时 (runCtx)
    由 req.RunTimeout 控制
    示例：10 分钟（可配置）

         ↓

【第 3 层】单个数据库操作超时
    每个操作独立 15-30 秒
    防护：数据库真的卡死

❌ 【不应有】全局持久化超时
    这是我之前的错误建议
```

---

## 🧪 测试修改

修改后，测试一个长任务：

```bash
# 发送消息（会触发工具调用和落库）
curl -X POST http://localhost:8080/api/sessions/test-session/messages \
  -d '{"content": "分析这个大文件", "stream": false}' \
  -H "Content-Type: application/json"

# 观察日志
tail -f /tmp/agent-server.log

# 预期：
# ✅ 没有 "context deadline exceeded"
# ✅ 工具调用完成
# ✅ 所有数据都落库了
```

---

## 📊 改进前后

| 场景 | 改进前 | 改进后 |
|------|--------|--------|
| 5 分钟任务 | ✅ 可能成功 | ✅ 确定成功 |
| 30 分钟任务 | ❌ 失败 | ✅ 成功 |
| 1 小时任务 | ❌ 失败 | ✅ 成功 |
| 最后一秒调用工具 | ❌ 失败 | ✅ 成功 |

---

## 📌 关键要点总结

### ❌ 错误思路
- "给整个持久化阶段一个超时时间"（无论多长都不够）

### ✅ 正确思路
- "不给整体超时，但给每个操作设置防护"
- runCtx 限制执行时间
- 每个数据库操作独立 15-30 秒超时

### 🎯 结果
- 任意长度的任务都能正确落库
- 单个数据库操作卡死仍然会检测到
- 不会因为超时而丢失数据

---

## 🚀 立即行动清单

- [ ] 修改位置 1：移除 persistCtx 全局超时
- [ ] 修改位置 2：appendToolCallMessage 添加 opCtx
- [ ] 修改位置 3：appendToolResultMessage 添加 opCtx
- [ ] 修改位置 4：persistMessage 添加 opCtx
- [ ] 重新编译：`make clean && make build`
- [ ] 启动测试：`./bin/agent-server`
- [ ] 验证日志：确认没有 deadline exceeded

---

## 📚 深度阅读

想了解更多为什么这是正确的设计，请看：
`TIMEOUT_DEEP_ANALYSIS.md` 

它包含了分布式系统中的最佳实践和完整的时间流分析。

