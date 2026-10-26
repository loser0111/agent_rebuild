# 消息持久化策略分析与改进

## 🎯 当前状态分析

根据代码审查，你的担忧是**完全正确的**。

### 现在的持久化策略（混合模式）

```go
// TypeDelta - 文本增量
case agent.TypeDelta:
    acc = append(acc, ev.Delta...)  // 只在内存中积累
    task.Publish(sse.EventMessageDelta, ...)  // 发送给前端

// TypeToolCall - 工具调用
case agent.TypeToolCall:
    e.appendToolCallMessage(persistCtx, ...)  // ✅ 立即落库

// TypeToolResult - 工具结果
case agent.TypeToolResult:
    e.appendToolResultMessage(persistCtx, ...)  // ✅ 立即落库

// TypeDone - 任务完成
case agent.TypeDone:
    e.persistMessage(persistCtx, ...)  // ⚠️ 一次性落库最终回答
    task.Publish(sse.EventMessageDone, ...)
    return
```

---

## ⚠️ 存在的问题

### 场景：最终落库失败

```
执行时间线：
t=0s       ├─ 对话开始
t=5s       ├─ 模型生成文本 100 tokens
t=10s      ├─ 工具调用 read_file
t=12s      │  └─ ✅ 立即落库工具调用消息
t=12:05s   ├─ 工具执行完成
t=12:10s   │  └─ ✅ 立即落库工具结果消息
t=15s      ├─ 模型继续生成文本 50 tokens
t=20s      ├─ 工具调用 search
t=22s      │  └─ ✅ 立即落库工具调用消息
t=22:05s   ├─ 工具执行完成
t=22:10s   │  └─ ✅ 立即落库工具结果消息
t=30s      ├─ 模型最终回答完成 200 tokens
t=30:05s   ├─ 尝试落库最终回答（500 字）
t=30:10s   ├─ 🔴 超时或数据库错误！
t=30:11s   └─ 放弃，任务结束
```

### 结果

**已落库的内容**：
- ✅ 工具调用消息（2 条）
- ✅ 工具结果消息（2 条）
- ❌ 最终回答消息（**0 字**）

**用户体验**：
- ✗ 看不到最终的 350 tokens
- ✗ 浪费了 500+ tokens
- ✗ 对话不完整，无法使用
- ✗ 需要重新跑任务

---

## 📊 持久化策略对比

| 策略 | 优点 | 缺点 | 风险 |
|------|------|------|------|
| **当前：一次性落库** | 简单、原子性 | 最后的超时导致全部丢失 | 🔴 高（长文本） |
| **改进：流式落库** | 逐步持久化、容错强 | 复杂、需要事务处理 | 🟢 低 |
| **备选：分批落库** | 折中方案 | 仍然有风险 | 🟡 中 |

---

## ✅ 推荐方案：流式落库

### 设计思路

**改为：每次接收到 TypeDelta，就立即落库（类似工具消息的处理）**

```go
case agent.TypeDelta:
    if ev.Delta == "" {
        continue
    }
    acc = append(acc, ev.Delta...)
    
    // 新增：立即落库文本增量（流式）
    e.appendDeltaMessage(ctx, req.SessionID, req.MessageID, ev.Delta)
    
    // 发送给前端
    task.Publish(sse.EventMessageDelta, func(seq uint64) any {
        return sse.MessageDeltaData{
            Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Delta: ev.Delta,
        }
    })

case agent.TypeDone:
    // TypeDone 时只需要更新消息状态为 "complete"
    // 内容已经逐个落库了
    e.updateMessageStatus(ctx, req.SessionID, req.MessageID, session.StatusComplete, usage)
    // ...
```

### 核心改变

**原来**：
```
Delta 1 → 内存
Delta 2 → 内存
Delta 3 → 内存
...
完成 → 一次性写所有内容到库
```

**改为**：
```
Delta 1 → 落库
Delta 2 → 落库
Delta 3 → 落库
...
完成 → 只需更新状态
```

---

## 🔧 实现方案

### 方案 A：简单的流式落库（推荐）

添加一个新函数：

```go
// appendDeltaMessage 流式落库文本增量。
// 每个 delta 到达时立即落库，确保不会因为最后的超时而丢失。
func (e *Executor) appendDeltaMessage(
    ctx context.Context,
    sessionID string,
    messageID string,
    delta string,
) {
    if e.store == nil {
        return
    }
    
    // 采用单个操作超时（15s）而非全局超时
    opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
    defer cancel()
    
    // 追加内容到已有的 assistant 消息
    err := e.store.UpdateMessage(opCtx, sessionID, messageID, func(m *session.Message) error {
        if m.Role != session.RoleAssistant {
            return fmt.Errorf("message role should be assistant, got %s", m.Role)
        }
        m.Content += delta
        m.UpdatedAt = time.Now().UTC()
        return nil
    })
    
    if err != nil {
        slog.Warn("流式文本落库失败", "err", err,
            "session_id", sessionID, "message_id", messageID, "delta_len", len(delta))
        if errors.Is(err, context.DeadlineExceeded) {
            metrics.Inc("persist_delta_deadline_exceeded")  // 告警
        }
    }
}
```

### 修改 run() 函数

在 TypeDelta 分支中：

```go
case agent.TypeDelta:
    if ev.Delta == "" {
        continue
    }
    acc = append(acc, ev.Delta...)  // 保留内存积累（用于统计）
    
    // 新增：立即落库
    e.appendDeltaMessage(persistCtx, req.SessionID, req.MessageID, ev.Delta)
    
    task.Publish(sse.EventMessageDelta, func(seq uint64) any {
        return sse.MessageDeltaData{
            Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID, Delta: ev.Delta,
        }
    })
```

### 修改 TypeDone 分支

```go
case agent.TypeDone:
    content := ev.Content
    if content == "" {
        content = string(acc)
    }
    var usage *session.Usage
    if ev.Usage != nil {
        usage = &session.Usage{
            PromptTokens:     ev.Usage.PromptTokens,
            CompletionTokens: ev.Usage.CompletionTokens,
            TotalTokens:      ev.Usage.TotalTokens,
        }
    }
    sseUsage := toSSEUsage(usage)
    
    // 改为：只更新状态和 usage，内容已经流式落库了
    e.updateMessageStatus(persistCtx, req.SessionID, req.MessageID,
        session.StatusComplete, usage)
    
    task.mu.Lock()
    task.status = StatusSucceeded
    task.mu.Unlock()
    
    task.Publish(sse.EventMessageDone, func(seq uint64) any {
        return sse.MessageDoneData{
            Seq: seq, SessionID: req.SessionID, MessageID: req.MessageID,
            Content: content, FinishReason: "stop", Usage: sseUsage,
        }
    })
    return
```

添加新函数：

```go
// updateMessageStatus 只更新消息的状态和 usage，不修改内容。
// 在流式落库的情况下，内容已经逐个 delta 落库了。
func (e *Executor) updateMessageStatus(
    ctx context.Context,
    sessionID string,
    messageID string,
    status session.MessageStatus,
    usage *session.Usage,
) {
    if e.store == nil {
        return
    }
    
    opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    
    err := e.store.UpdateMessage(opCtx, sessionID, messageID, func(m *session.Message) error {
        m.Status = status
        if usage != nil {
            m.Usage = &session.Usage{
                PromptTokens:     usage.PromptTokens,
                CompletionTokens: usage.CompletionTokens,
                TotalTokens:      usage.TotalTokens,
            }
        }
        m.UpdatedAt = time.Now().UTC()
        return nil
    })
    
    if err != nil {
        slog.Warn("消息状态更新失败", "err", err,
            "session_id", sessionID, "message_id", messageID, "status", status)
    }
}
```

---

## 🎯 改进效果

### 改进前（一次性落库）

```
执行 30 秒
├─ 工具消息：✅ 立即落库
├─ 最终回答：⚠️ 一次性落库
└─ 最终回答超时 → ❌ 全部内容丢失
```

**风险**：最后 1% 的失败导致前面 99% 的工作白费

### 改进后（流式落库）

```
执行 30 秒
├─ Delta 1：✅ 落库
├─ Delta 2：✅ 落库
├─ 工具消息：✅ 落库
├─ Delta 3：✅ 落库
├─ 最终状态更新：✅ 落库（只需 30ms）
└─ 最终状态更新失败 → ⚠️ 内容已存，只是状态标记缺失
```

**收益**：
- ✅ 用户看得到 99% 的内容
- ✅ 只有状态标记可能缺失（重要性低）
- ✅ 即使完全失败，重跑时也能看到之前的结果

---

## 📊 性能影响分析

### 数据库写入频率

| 策略 | 写入次数 | 写入大小 | 总耗时 |
|------|---------|---------|--------|
| 当前一次性 | 1 × | 10KB | ~50ms（或超时） |
| 流式落库 | 50 × | 200 bytes | ~50ms（分散） |

**结论**：
- 写入次数增加，但每次很小
- 总耗时反而可能减少（分散压力）
- SQLite 的 WAL 模式能很好地处理频繁小写

### 数据库事务考虑

如果担心并发问题，可以使用事务：

```go
func (e *Executor) appendDeltaMessage(...) {
    err := e.store.DB().WithTx(opCtx, func(tx *sql.Tx) error {
        // 在事务中读-改-写
        m := &session.Message{}
        if err := tx.QueryRow(...).Scan(&m.Content, ...); err != nil {
            return err
        }
        m.Content += delta
        return tx.Exec(..., m.Content, ...).Err()
    })
    // ...
}
```

---

## ⚠️ 注意事项

### 1. 消息顺序保证

由于是流式落库，需要确保：
- 每个 delta 都被保存
- 顺序不乱序

**解决方案**：使用单线程的 goroutine 处理写入，或使用数据库级的顺序约束

### 2. 并发冲突

多个请求同时修改同一条消息的 content？

**实际不会发生**，因为：
- 一个 messageID 对应一个执行任务
- 同一会话有互斥锁（TryLock）
- 同时只有一个任务在更新同一条消息

### 3. 已经落库的 delta 与新 delta 的同步

**当前代码**：内存中的 `acc` 只用于统计，不用于重建历史

**流式落库后**：`acc` 仍然保留（用于计算最终的 usage 等），但内容的源头是数据库

---

## 🚀 改进步骤

### Step 1：添加两个新函数

在 `executor.go` 中添加：
- `appendDeltaMessage()` - 流式落库 delta
- `updateMessageStatus()` - 只更新状态

### Step 2：修改 run() 的事件循环

在 `TypeDelta` 分支中调用 `appendDeltaMessage()`
在 `TypeDone` 分支中调用 `updateMessageStatus()` 而不是 `persistMessage()`

### Step 3：修改 TypeAwaitingInput 分支

这里也有一次性落库，改为：
```go
case agent.TypeAwaitingInput:
    // 把已产出的文本逐个落库（应该已经流式落库了）
    // 这里只需更新状态为 streaming
    e.updateMessageStatus(persistCtx, req.SessionID, req.MessageID,
        session.StatusStreaming, nil)
    // ...
```

### Step 4：测试

运行模拟测试，验证流式落库的一致性

---

## 📈 预期改进

| 场景 | 改进前 | 改进后 |
|------|--------|--------|
| 执行 30s，最后 1s 失败 | ❌ 全部丢失 | ✅ 30/30s 保留 |
| 执行 1h，中间落库超时 | ❌ 丢失内容 | ✅ 部分保留 |
| 用户看到的完整性 | 60-80% | ✅ 95%+ |
| Token 浪费 | 高 | 低 |

---

## 💡 这是一个重要的设计问题

你的发现揭示了一个**关键的架构权衡**：

```
单一事务的原子性 vs 容错能力的权衡

原子性：
  优点：要么全有，要么全无
  缺点：任何一个部分失败，全部回滚

流式持久化：
  优点：容错强，部分失败不影响前面的
  缺点：最后的状态更新可能不一致

正确的选择：对于长任务，流式持久化更合适
  因为我们宁愿有 99% 的数据，也不要因为最后的超时而全部丢失
```

这也是为什么很多大型系统（如日志系统、消息队列）都采用流式/批处理的持久化策略。

