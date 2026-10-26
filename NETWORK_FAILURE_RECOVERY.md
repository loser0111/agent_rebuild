# 网络中断与消息恢复问题的深度分析

## 🎯 你问的问题的本质

假设这个场景：

```
用户提问 → 消息1（落库✅）
  ↓
Agent 执行 → 消息2（正在生成）
  ├─ 模型生成：50 tokens ✅ SSE 发送给前端
  ├─ 调用工具A         ✅ 工具消息落库
  ├─ 工具A结果         ✅ 结果消息落库
  ├─ 模型继续生成：100 tokens ✅ SSE 发送给前端
  ├─ 调用工具B         ✅ 工具消息落库
  ├─ 工具B结果         ✅ 结果消息落库
  ├─ 模型最终回答：150 tokens ✅ SSE 发送给前端
  └─ 最后一次性落库消息2内容... ❌ 网络波动，超时！

结果状态：
─────────────────────────────────
DB 中：
  消息1: complete, content="用户提问"
  消息2: streaming, content=""  ← 问题！
  工具A消息: complete
  工具B消息: complete

用户前端：
  消息1: "用户提问"
  消息2: "150 tokens 完整内容"（来自 SSE）
  工具A, B都看得见

下次对话时：
  大模型加载历史...
  SELECT * FROM messages WHERE status="complete"
  结果：只有消息1，消息2被过滤了！
  ↓
  大模型完全看不到消息2的内容和工具结果
  ↓
  ⚠️ 问题：需要重新运行整个消息2吗？
```

**你的问题**：这时候用户需要点击"重试"重新运行消息2吗？这会导致：
- ❌ 前面的工作全部重做
- ❌ Token 浪费
- ❌ 时间浪费

---

## 🔴 我之前建议的缺陷

我说"保持一次性落库，改进超时"，但这有个问题：

```
改进超时虽然能让落库有更多时间完成
但它不能解决"网络中断"导致的失败
网络中断是不能避免的（特别是长任务中）

如果最后的落库最终还是失败了
就陷入了你描述的困境：
├─ 前端看到完整内容
├─ 数据库看不到
├─ 大模型看不到
└─ 用户怎么办？需要重新运行吗？
```

---

## ✅ 真正的解决方案

### 方案 A：Checkpoint + 快速恢复（推荐）

**思路**：记录消息已完成的状态，即使落库失败也能恢复

```go
// 在 TypeDone 时，先记录 checkpoint
case agent.TypeDone:
    content := ev.Content
    if content == "" {
        content = string(acc)
    }
    
    // 第一步：记录 checkpoint（已完成但未落库）
    checkpoint := &session.Checkpoint{
        RunID:  task.ID,
        State:  session.CheckpointDone,
        Result: &session.PendingResult{
            MessageID: req.MessageID,
            Content: content,
            Status: session.StatusComplete,
            Usage: usage,
        },
    }
    
    if err := e.store.SaveCheckpoint(ctx, req.SessionID, checkpoint); err != nil {
        slog.Warn("保存 checkpoint 失败（关键！）", "err", err)
        // 这里失败仍然要继续尝试落库
    }
    
    // 第二步：尝试落库消息
    // 如果失败了，至少 checkpoint 已经保存了
    err := e.persistMessage(persistCtx, req.SessionID, req.MessageID, content, ...)
    
    if err != nil {
        slog.Warn("消息落库失败（但 checkpoint 已保存）", "err", err)
        // 重要：用户下次查询时，可以从 checkpoint 恢复
    }
```

**恢复逻辑**：

```go
// 当用户下次查询历史时
func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
    sessionID := r.PathValue("id")
    
    // 1. 从数据库查询消息
    messages, err := s.store.Messages(ctx, sessionID)
    
    // 2. 检查是否有未落库但已完成的 checkpoint
    checkpoint, _ := s.store.GetCheckpoint(ctx, sessionID)
    if checkpoint != nil && checkpoint.State == session.CheckpointDone {
        // 检查对应的消息是否还在 "streaming"
        for i, msg := range messages {
            if msg.ID == checkpoint.Result.MessageID && 
               msg.Status == session.StatusStreaming {
                // 3. 修复消息：从 checkpoint 恢复
                messages[i].Content = checkpoint.Result.Content
                messages[i].Status = checkpoint.Result.Status
                messages[i].Usage = checkpoint.Result.Usage
                
                slog.Info("从 checkpoint 恢复消息", "msg_id", msg.ID)
                
                // 4. 异步落库到数据库
                go s.asyncPersist(sessionID, messages[i])
            }
        }
        
        // 5. 清理已恢复的 checkpoint
        s.store.DeleteCheckpoint(ctx, sessionID)
    }
    
    respondJSON(w, messages)
}
```

**优点**：
- ✅ 用户看到完整历史（从 checkpoint 恢复）
- ✅ 不需要重新运行消息
- ✅ Token 不浪费
- ✅ 下次大模型看到完整的历史

**代码改动**：20-30 行

---

### 方案 B：流式落库 + 状态追踪（高级）

**思路**：流式落库每个 delta，但追踪"哪些内容已落库"

```go
// 增加追踪字段
type Message struct {
    // ...
    Content        string
    Status         MessageStatus
    ContentVersion int  // 内容版本号（递增）
    StatusVersion  int  // 状态版本号
}

// 流式落库
case agent.TypeDelta:
    err := e.store.UpdateMessage(ctx, sessionID, messageID,
        func(m *session.Message) error {
            m.Content += ev.Delta
            m.ContentVersion++
            return nil
        })

// 完成时
case agent.TypeDone:
    err := e.store.UpdateMessage(ctx, sessionID, messageID,
        func(m *session.Message) error {
            m.Status = session.StatusComplete
            m.StatusVersion = m.ContentVersion  // 同步版本
            m.Usage = usage
            return nil
        })
    
    if err != nil {
        // 即使失败，content 仍然完整（已流式落库）
        // status 仍然是 streaming
        // 但下次恢复时可以检查：ContentVersion > 0 则说明有内容
    }

// 查询时修复不一致
func fixMessageConsistency(msg *Message) {
    if msg.Status == StatusStreaming && msg.ContentVersion > 0 {
        // 说明内容已经全部落库了，只是状态标记没有更新
        msg.Status = StatusComplete
    }
}
```

**优点**：
- ✅ 可以自动检测和修复不一致
- ✅ 内容永不丢失

**缺点**：
- ❌ 更复杂的版本管理
- ❌ 流式落库仍然有一致性风险

---

### 方案 C：异步落库 + Redis 缓存（最安全）

**思路**：主流程快速完成，持久化作为异步后台任务

```go
// 完成时
case agent.TypeDone:
    content := ev.Content
    if content == "" {
        content = string(acc)
    }
    
    // 第一步：快速写入 Redis（几乎不会失败）
    quickResult := &QuickResult{
        MessageID: req.MessageID,
        Content: content,
        Status: StatusComplete,
        Usage: usage,
        Timestamp: time.Now(),
    }
    
    if err := e.cache.Set(ctx, 
        fmt.Sprintf("msg:%s:%s", sessionID, req.MessageID),
        quickResult,
        5*time.Minute); err != nil {
        slog.Error("快速缓存失败（罕见）", "err", err)
    }
    
    // 第二步：立即发送 TypeDone 给前端
    // 不等数据库落库
    task.Publish(sse.EventMessageDone, ...)
    
    // 第三步：异步持久化到数据库
    go func() {
        time.Sleep(100 * time.Millisecond)  // 让前端接收完事件
        
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        
        if err := e.persistMessage(ctx, sessionID, req.MessageID, content, ...); err != nil {
            slog.Warn("异步持久化失败，但 Redis 有副本", "err", err)
        } else {
            // 成功落库，清理 Redis
            e.cache.Delete(ctx, fmt.Sprintf("msg:%s:%s", sessionID, req.MessageID))
        }
    }()
```

**查询时**：

```go
func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
    sessionID := r.PathValue("id")
    
    // 1. 查询数据库
    messages, _ := s.store.Messages(ctx, sessionID)
    
    // 2. 对于 streaming 状态的消息，检查 Redis 缓存
    for i, msg := range messages {
        if msg.Status == StatusStreaming && msg.Content == "" {
            key := fmt.Sprintf("msg:%s:%s", sessionID, msg.ID)
            if cached, err := s.cache.Get(ctx, key); err == nil {
                messages[i] = cached.(*QuickResult).ToMessage()
            }
        }
    }
    
    respondJSON(w, messages)
}
```

**优点**：
- ✅ 用户体验最好（立即看到完成）
- ✅ 容错能力最强
- ✅ 不需要重新运行

**缺点**：
- ❌ 需要 Redis
- ❌ 引入缓存一致性问题

---

## 📊 三个方案对比

| 方案 | 需要重新运行? | 改动量 | 基础设施 | 复杂度 | 推荐度 |
|------|-------------|--------|---------|--------|--------|
| **A: Checkpoint** | ❌ 否 | 20-30 行 | 无需新增 | 低 | ✅ **推荐** |
| **B: 版本追踪** | ❌ 否 | 40-50 行 | 无需新增 | 中 | 💡 不推荐 |
| **C: 异步+Redis** | ❌ 否 | 60+ 行 | 需要 Redis | 高 | 🎯 大规模 |

---

## 🎯 我的建议：采用方案 A

### 为什么

1. **无需新增基础设施**
   - 代码中已经有 Checkpoint 机制！
   - 只需要复用现有的 checkpoint 存储

2. **改动最小**
   - 只需在 TypeDone 时添加 checkpoint 保存

3. **恢复最优雅**
   - 用户不需要重新运行
   - 透明的恢复过程

4. **符合现有架构**
   - Checkpoint 本来就是为了"跨进程恢复"设计的
   - 这里只是扩展其用途

### 实施步骤

**Step 1**：在 TypeDone 时保存 checkpoint

```go
case agent.TypeDone:
    content := ev.Content
    if content == "" {
        content = string(acc)
    }
    usage := ...
    
    // 新增：保存 checkpoint（作为恢复点）
    checkpoint := &session.Checkpoint{
        RunID:  task.ID,
        State:  session.CheckpointDone,
        Result: &session.CompletedResult{
            MessageID:   req.MessageID,
            Content:     content,
            Usage:       usage,
        },
    }
    e.store.SaveCheckpoint(ctx, req.SessionID, checkpoint)
    
    // 原有逻辑：尝试落库
    e.persistMessage(persistCtx, ...)
```

**Step 2**：查询消息时，从 checkpoint 恢复

```go
func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
    messages, _ := s.store.Messages(ctx, sessionID)
    
    // 修复：检查是否有未落库的已完成消息
    checkpoint, _ := s.store.GetCheckpoint(ctx, sessionID)
    if checkpoint != nil && checkpoint.State == CheckpointDone {
        // 从 checkpoint 恢复
        for i, msg := range messages {
            if msg.ID == checkpoint.Result.MessageID {
                messages[i].Content = checkpoint.Result.Content
                messages[i].Status = StatusComplete
                messages[i].Usage = checkpoint.Result.Usage
                // 异步持久化
                go s.asyncPersist(sessionID, messages[i])
            }
        }
    }
    
    respondJSON(w, messages)
}
```

---

## 📈 改进效果

### 对比"我之前的建议"

```
场景：网络中断导致最后的落库失败

我之前的建议：
  ❌ 消息2: streaming, content=""
  ❌ 用户需要重新运行消息2
  ❌ 前面的 tokens 白费
  ❌ 用户很烦

改进后（方案 A）：
  ✅ 消息2: complete, content="完整的150 tokens"（从 checkpoint 恢复）
  ✅ 用户不需要重新运行
  ✅ 一切透明，体验无缝
  ✅ 下次大模型看到完整历史
```

---

## 💡 关键洞察

你的问题揭示了我之前建议的一个**假设的缺陷**：

**假设**："改进超时就够了"

**现实**：
- ✓ 改进超时能减少失败次数
- ✗ 但不能消除网络中断这种不可抗因素
- ✗ 必须有恢复机制

**正确的方案**：
- 改进超时（减少失败）
- 加上 Checkpoint 恢复机制（处理失败）
- 两者结合，容错能力才完整

---

## 📝 总结

你的问题非常深刻，直接指向了系统可靠性的关键：

```
❌ 错误思路：优化超时，期望永不失败
✅ 正确思路：优化超时 + 构建恢复机制
         即使失败也能无缝恢复
```

这也是为什么大型系统都采用"乐观执行 + 悲观恢复"的模式。

