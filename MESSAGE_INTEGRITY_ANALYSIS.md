# 消息完整性与一致性问题深度分析

## 🎯 你提出的核心问题

**"消息完整性"涉及两个层面**：

1. **数据完整性**：所有的 delta 都被保存了吗？
2. **语义完整性**：消息的状态（status）是否与内容相匹配？

你的担忧指向了一个**深层的架构问题**。

---

## 📊 消息状态机分析

### 当前的状态定义

```go
// 状态转移：pending → streaming → complete | interrupted | error
const (
    StatusPending     MessageStatus = "pending"      // 刚创建，还没开始流式接收
    StatusStreaming   MessageStatus = "streaming"    // 正在接收 delta
    StatusComplete    MessageStatus = "complete"     // 接收完成，全部 delta 已到达
    StatusInterrupted MessageStatus = "interrupted"  // 中途被取消
    StatusError       MessageStatus = "error"        // 出错
)
```

### 问题所在

**如果采用流式落库每个 delta，会出现什么情况？**

```
时间线：
t=0s     ├─ 创建消息，status = "pending"
t=0:05s  ├─ 接收 delta 1 (50 chars)
t=0:10s  │  ├─ 流式落库 delta 1 → content = "我是一个助手"
t=0:10s  │  └─ 状态仍然是 "streaming" ✅
t=0:15s  ├─ 此时用户从另一个标签页查询历史
t=0:15s  │  └─ 看到：content="我是一个助手", status="streaming"
t=0:20s  ├─ 接收 delta 2 (50 chars)
t=0:25s  │  └─ 流式落库 delta 2 → content = "我是一个助手，很高兴..."
t=0:30s  ├─ 所有 delta 接收完成
t=0:35s  ├─ 更新 status = "complete"，同时记录 usage
t=0:40s  ├─ 🔴 超时！status 更新失败
t=0:41s  └─ 消息最终状态：content="完整", status="streaming" ❌ 不一致！
```

### 大模型交互的问题

**当用户问下一个问题时**：

```go
// 加载历史消息
history := store.Messages(sessionID)

// history 中可能包含：
// {
//   id: "msg_xxx",
//   role: "assistant",
//   content: "我是一个助手，很高兴...",
//   status: "streaming",  // ❌ 问题：这条消息还在"流式"吗？
//   usage: nil            // ❌ 没有 usage 信息
// }

// 大模型怎么处理这条消息？
// 1. 应该把它作为完整的消息吗？
// 2. 还是当作"未完成"的消息忽略？
// 3. 还是当作错误状态？
```

---

## ⚠️ 流式落库的三个陷阱

### 陷阱 1：状态不一致

```
数据库里的消息：
{
  content: "完整的 500 字回复",
  status: "streaming"        // ❌ 矛盾！内容完整但状态还是流式
}

查询这条消息的代码会困惑：
if msg.Status == "streaming" {
    // 预期内容不完整
    // 但实际内容是完整的
}
```

### 陷阱 2：部分可见性

```
前端 A：正在流式接收消息
前端 B：同时打开另一个标签页，查询历史

前端 B 看到的消息：
{
  content: "我是一个...",    // 部分内容
  status: "streaming"
}

预期：这条消息还在接收中
实际：可能已经接收完了，只是 status 标记错了
```

### 陷阱 3：大模型的误解

```
下一轮对话，加载历史：

history = [
  { role: "user", content: "第一个问题" },
  { role: "assistant", content: "...", status: "streaming" },  // ⚠️
  { role: "user", content: "第二个问题" },
  { role: "assistant", content: "...", status: "complete" }
]

大模型看到 status="streaming" 的消息，会疑惑：
- 这是一条未完成的消息吗？
- 应该被包含在上下文中吗？
- 还是应该被过滤掉？
```

---

## 🔍 根本问题：什么是"完整的消息"？

### 定义的歧义

**对前端来说**：
- "完整" = SSE 连接关闭，接收到 TypeDone 事件
- 内容已经在浏览器中积累完成了

**对数据库来说**：
- "完整" = 所有字段都写入并一致了
- content、status、usage 都正确

**对大模型来说**：
- "完整" = 可以被包含在上下文中的消息
- 需要 status="complete" 并且有有效的 content

**这三个定义可能不同步！**

---

## ✅ 正确的解决方案

### 方案 1：两阶段提交（推荐）

**思路**：确保消息的所有字段原子性更新

```
第一阶段：流式接收与缓存
  ├─ 前端通过 SSE 流式接收 delta
  ├─ 服务端在内存中积累 delta（在 Task 对象中）
  └─ 状态标记为 "streaming"

第二阶段：原子完成
  ├─ 所有 delta 接收完毕，runCtx 得到 TypeDone
  ├─ 计算 usage 信息
  ├─ 原子更新数据库：
  │  ├─ content（完整的，一次性写）
  │  ├─ status（从 streaming 改为 complete）
  │  └─ usage（添加 token 统计）
  ├─ 🎯 这是一个原子操作，要么全成功要么全失败
  └─ 发送 TypeDone 事件给前端
```

**优点**：
- ✅ 数据库中的消息始终是一致的
- ✅ status 与 content 永不矛盾
- ✅ 大模型看到的消息总是"完整"的或"流式中"的，从不混淆

**代码实现**：

```go
// 在 run() 函数中，原保持一次性落库不变
case agent.TypeDone:
    content := ev.Content
    if content == "" {
        content = string(acc)  // 内存中积累的所有 delta
    }
    var usage *session.Usage
    if ev.Usage != nil {
        usage = &session.Usage{
            PromptTokens:     ev.Usage.PromptTokens,
            CompletionTokens: ev.Usage.CompletionTokens,
            TotalTokens:      ev.Usage.TotalTokens,
        }
    }
    
    // 原子操作：一次性更新所有字段
    // 使用事务确保一致性
    err := e.store.UpdateMessage(persistCtx, sessionID, req.MessageID, 
        func(m *session.Message) error {
            // 检查状态一致性
            if m.Status != session.StatusStreaming {
                return fmt.Errorf("unexpected status: %s", m.Status)
            }
            
            // 一次性更新：content + status + usage
            m.Content = content
            m.Status = session.StatusComplete
            m.Usage = usage
            m.UpdatedAt = time.Now().UTC()
            return nil
        })
    
    if err != nil {
        slog.Error("落库消息最终状态失败", "err", err, ...)
        // 这里失败 = 整个消息不更新
        // 仍然是 status="streaming" + 空 content
        // 用户下次查询时会看到消息仍在流式中
        // 可以点击"重试"让模型继续
    } else {
        // 落库成功
        task.Publish(sse.EventMessageDone, ...)
    }
```

---

### 方案 2：三层架构（高可用）

**思路**：引入中间层缓存，确保数据不丢失

```
前端
  ↓ SSE
服务端内存缓存（内存中积累 delta）
  ↓ 事件完成后
Redis/Memcached（快速持久化，允许不一致）
  ↓ 异步批处理
主数据库（最终一致，数据完整）
```

**流程**：

```go
// 第一层：内存积累（现有逻辑不变）
case agent.TypeDelta:
    acc = append(acc, ev.Delta...)  // 在内存中
    task.Publish(...)

// 第二层：任务完成时，先写到快速存储
case agent.TypeDone:
    fastCache.Set(sessionID + ":msg:" + messageID, &Message{
        Content: string(acc),
        Status: "pending_persist",  // 特殊状态
        CreatedAt: time.Now(),
    })
    
    // 立即发送 TypeDone 给前端，不等数据库
    task.Publish(sse.EventMessageDone, ...)
    
    // 异步持久化到主数据库
    go func() {
        time.Sleep(time.Second)  // 给用户刷新的机会
        e.persistToDB(sessionID, messageID, string(acc))
    }()
```

**优点**：
- ✅ 前端立即看到完整内容（从缓存）
- ✅ 大模型加载历史时可以先从缓存取，再从DB同步
- ✅ 数据库慢或超时不影响用户体验

**缺点**：
- ❌ 需要额外的基础设施（Redis）
- ❌ 引入了缓存一致性问题
- ❌ 更复杂的错误处理

---

### 方案 3：消息版本化（可恢复）

**思路**：允许消息状态不一致，但可以恢复

```go
// 消息增加一个"版本"字段
type Message struct {
    // ... 现有字段 ...
    Content    string
    Status     MessageStatus
    ContentVer int  // 内容版本号
    StatusVer  int  // 状态版本号
}

// 流式落库每个 delta 的同时记录版本
case agent.TypeDelta:
    acc = append(acc, ev.Delta...)
    
    // 流式落库
    err := e.store.UpdateMessage(persistCtx, sessionID, messageID,
        func(m *session.Message) error {
            m.Content += ev.Delta
            m.ContentVer++
            m.UpdatedAt = time.Now().UTC()
            return nil
        })

// 最后一步：确认完整性
case agent.TypeDone:
    err := e.store.UpdateMessage(persistCtx, sessionID, messageID,
        func(m *session.Message) error {
            m.Status = session.StatusComplete
            m.StatusVer = m.ContentVer  // 版本同步
            m.Usage = usage
            return nil
        })
    
    if err != nil {
        // 即使这里失败，content 已经完整了
        // 下次查询时可以检测到 ContentVer != StatusVer
        // 表示消息已完整但状态标记未更新，可以修复
    }
```

**优点**：
- ✅ 可以检测不一致
- ✅ 可以自动修复
- ✅ 数据不丢失

**缺点**：
- ❌ 版本管理复杂
- ❌ 需要修复逻辑

---

## 📊 方案对比

| 方案 | 一致性 | 容错性 | 复杂度 | 推荐度 |
|------|--------|--------|--------|--------|
| **现状**（一次性落库） | ✅ 强 | ❌ 弱（最后超时全丢） | 低 | ❌ 有风险 |
| **方案 1**（两阶段提交） | ✅ 强 | ⚠️ 中（内容保留，状态可能缺失） | 低 | ✅ **推荐** |
| **方案 2**（三层架构） | ⚠️ 最终一致 | ✅ 强 | 高 | 🎯 大规模 |
| **方案 3**（消息版本化） | ⚠️ 可恢复 | ✅ 强 | 中 | 💡 学术性 |

---

## 🎯 推荐方案详解

### 我建议采用"方案 1 + 改进的超时"

**理由**：

1. **最小化改动**
   - 不改变现有的一次性落库逻辑
   - 只改进超时配置（移除全局超时，给单个操作足够的时间）

2. **保证一致性**
   - 消息的 content、status、usage 总是一致的
   - 大模型不会看到混淆的状态

3. **可接受的容错**
   - 如果最后的落库失败，内容仍然在内存中（用户看得到）
   - 消息会标记为 "streaming" 状态（表示未完成）
   - 用户可以点击"重试"继续

4. **SSE 的语义清晰**
   - SSE 流式传输 = 前端流式接收
   - 数据库持久化 = 原子操作
   - 两者不混淆

---

## 🛠️ 具体实施

### Step 1：确保超时足够

改 executor.go:417（现在应该已经改成无全局超时了）

```go
// ✅ 无全局 persistCtx 超时
persistCtx := context.Background()

// ✅ 每个操作独立超时
opCtx, cancel := context.WithTimeout(persistCtx, 30*time.Second)
defer cancel()
```

### Step 2：使用事务确保一致性

```go
// 在最终落库时使用事务
err := e.store.DB().BeginTx(ctx, nil).Commit()

// 或者让 UpdateMessage 本身使用事务
err := e.store.UpdateMessage(persistCtx, sessionID, messageID,
    func(m *session.Message) error {
        // 所有更新在同一个事务内
        m.Content = content
        m.Status = session.StatusComplete
        m.Usage = usage
        m.UpdatedAt = time.Now().UTC()
        return nil
    })
```

### Step 3：添加日志用于诊断

```go
// TypeDone 时
case agent.TypeDone:
    slog.InfoContext(ctx, "消息完成，准备落库",
        "session_id", req.SessionID,
        "message_id", req.MessageID,
        "content_len", len(content),
        "usage", usage,
    )
    
    err := e.persistMessage(persistCtx, ...)
    
    if err != nil {
        slog.ErrorContext(ctx, "消息落库失败，用户仍能看到内容（SSE）",
            "err", err,
            "session_id", req.SessionID,
        )
        // 但消息仍然是 StatusStreaming，不完整
    }
```

---

## 📌 关键总结

### 你的问题的本质

**"消息完整性"有两个维度**：

1. **数据完整性**：内容是否全部被保存
   - 流式落库可以改进这个（但引入复杂度）

2. **语义完整性**：status 是否准确反映实际状态
   - 这是更重要的
   - 一定不能 status="streaming" + content="完整"

### 最佳实践

```
✅ 不要尝试流式落库文本内容
✅ 保持原子性：content、status、usage 一起更新
✅ 改进超时，不要限制整体持久化时间
✅ 如果最后的落库失败，让用户看到"消息未完成"的状态
✅ 这样既安全，又语义清晰
```

### SSE 与大模型交互的正确模型

```
SSE 前端：
  ├─ 逐个接收 delta
  ├─ 在浏览器中积累为完整内容
  └─ 显示给用户

后端持久化：
  ├─ 在内存中保持 delta 积累
  ├─ 完成后进行原子落库
  └─ 确保数据库中的消息语义清晰

大模型下一轮：
  ├─ 加载消息历史
  ├─ 只包含 status="complete" 的消息
  └─ 确保上下文完整且一致
```

