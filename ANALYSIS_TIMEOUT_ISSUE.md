# Context Deadline Exceeded 问题分析

## 📋 问题概览

你的日志显示大量的 `context deadline exceeded` 错误：

```
[WARN] 工具结果落库失败       context deadline exceeded
[WARN] 工具调用消息落库失败   context deadline exceeded  
[ERROR] 落库消息最终状态失败  context deadline exceeded
[INFO] http POST /messages    status=200 dur_ms=95372  (95秒!)
```

**关键现象**：
- HTTP 请求耗时 **95 秒**
- 发生了多次数据库操作超时
- 请求最终返回了 200（成功），但数据可能不完整

---

## 🔍 根本原因分析

### 1. 超时时间设置过短

**问题位置**：`internal/executor/executor.go:417`

```go
// 为所有落库操作创建的上下文，总超时 30 秒
persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)
```

**数据流**：
```
HTTP 请求开始 (t=0)
  ├─ 用户消息落库
  ├─ 创建 persistCtx (30s 超时) ← 这里！
  ├─ 启动执行任务 (runCtx)
  │  ├─ 调用 LLM 模型 (t=0-30s)
  │  ├─ 工具调用 1 (t=30-60s)
  │  │  └─ 尝试落库工具调用 ❌ persistCtx 已超时!
  │  ├─ 工具调用 2 (t=60-80s)
  │  │  └─ 尝试落库工具结果 ❌ persistCtx 已超时!
  │  └─ 最终回答 (t=80-95s)
  │     └─ 尝试落库最终内容 ❌ persistCtx 已超时!
  └─ HTTP 请求结束 (t=95s)
```

### 2. 工具执行时间过长

**时间分解**（根据 95秒 推测）：
- LLM 调用：~15-20 秒
- 工具 1 执行：~25 秒（如外部 API、文件操作等）
- 工具 2 执行：~20 秒
- 其他操作：~20 秒
- **总计**：>95 秒

**当工具执行超过 30 秒时**，第一个工具调用的落库已经开始失败。

### 3. 数据库操作堵塞

**可能的原因**：
- SQLite 的默认超时配置
- 数据库连接池耗尽
- 大量并发写操作（多工具调用堵塞彼此）
- 磁盘 I/O 缓慢

---

## 💥 影响分析

### 直接影响

| 内容 | 后果 |
|------|------|
| 工具调用消息未落库 | 历史中缺少工具调用记录 |
| 工具结果未落库 | 历史中缺少工具执行结果 |
| 最终消息未落库 | 用户看不到完整回复 |

### 间接影响

1. **历史信息丢失**
   ```
   用户看到的对话历史：
   [User] 请分析这个文件
   [Assistant] (完整回复但没有工具过程)
   
   实际发生的：
   [User] 请分析这个文件
   [Assistant] 我需要用 read_file 工具
   [Tool] file_content
   [Assistant] 分析结果...
   
   下次模型加载时：
   ❌ 工具记录丢失 → 模型会重复调用工具
   ```

2. **模型循环调用**
   - 工具结果没有落库，下次对话不会被模型看到
   - 模型以为没有执行过这个工具，会重复调用
   - 浪费 token 和时间

3. **用户体验问题**
   - 虽然看到了回复，但无法回溯工具执行过程
   - 无法调试或验证工具输出

---

## 🎯 问题根源：架构设计与超时配置的矛盾

### 当前设计意图

代码注释说明了为什么需要独立的 `persistCtx`：

```go
// 兜底落库用独立 ctx：执行 ctx 可能已取消，不能用它写库。
//
// 注意：这里**不能**用 defer cancel。streamEvents 内部有自己的
// goroutine 负责工具调用落库，外层函数返回时它可能还没写完；
// 若此时取消 persistCtx，就会得到 "context canceled" 而丢失
// 全部 tool_calls 与工具结果。
```

**问题**：
- ✅ 设计正确：执行生命周期独立于 HTTP 连接
- ✅ 实现正确：后台 goroutine 处理工具落库
- ❌ **配置错误**：30 秒超时对于长时间运行的任务太短了

---

## 🚀 解决方案

### 方案 A：增加 persistCtx 超时时间（快速修复）

**位置**：`internal/executor/executor.go:417`

```go
// 旧：30 秒
persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)

// 新：考虑最坏情况
persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Minute)
```

**优点**：
- 修改 1 行代码
- 立即生效

**缺点**：
- 可能隐藏真正的问题（数据库卡住）
- 超时时间还是有上限，长任务可能还是会失败

**适用场景**：快速修复线上问题

---

### 方案 B：动态超时（推荐）

**思路**：基于 `runTimeout` 动态计算 `persistCtx` 的超时

```go
// 改造：runTimeout 影响 persistCtx
type Request struct {
    // ...
    RunTimeout time.Duration  // 整体执行时限
}

// 在 run() 函数中
func (e *Executor) run(runCtx context.Context, task *Task, req Request, ...) {
    // 计算持久化超时：在 runTimeout 基础上再加 2 倍作为缓冲
    persistTimeout := time.Duration(float64(req.RunTimeout) * 2.5)
    if persistTimeout < 2*time.Minute {
        persistTimeout = 2 * time.Minute  // 最小 2 分钟
    }
    
    persistCtx, persistCancel := context.WithTimeout(
        context.Background(), 
        persistTimeout,
    )
    // ...
}
```

**优点**：
- 自适应不同任务的执行时间
- 保留超时保护（防止数据库真的卡住）

**代码改动量**：5-10 行

---

### 方案 C：异步持久化 + 重试机制（终极方案）

**思路**：从同步落库改为异步落库，并添加重试机制

```go
// 工具调用落库改为异步 + 重试
func (e *Executor) appendToolCallMessageAsync(
    sessionID string, 
    call agent.ToolCall,
) {
    // 入队到持久化队列，而不是立即执行
    e.persistQueue.Enqueue(&persistJob{
        Type:       "tool_call",
        SessionID:  sessionID,
        Data:       call,
        RetryCount: 0,
        MaxRetries: 3,
    })
}

// 后台持久化 worker 处理队列
func (e *Executor) persistWorker() {
    for job := range e.persistQueue {
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        err := e.store.AppendMessage(ctx, buildMessage(job.Data))
        cancel()
        
        if err != nil {
            if job.RetryCount < job.MaxRetries {
                job.RetryCount++
                time.Sleep(time.Duration(job.RetryCount) * time.Second)
                e.persistQueue.Enqueue(job)  // 重新入队
            } else {
                slog.Error("持久化失败，已耗尽重试", "err", err, "job", job)
            }
        }
    }
}
```

**优点**：
- 完全解耦 HTTP 响应与数据库写入
- 自动重试，容错能力强
- 不受任何超时限制

**代码改动量**：50-100 行，需要添加队列和 worker

---

## 📊 比较与建议

| 方案 | 修改难度 | 效果 | 推荐度 |
|------|---------|------|--------|
| **A** 增加超时 | ⭐ 极易 | ⭐ 临时缓解 | ⚠️ 快速补丁 |
| **B** 动态超时 | ⭐⭐ 简单 | ⭐⭐⭐ 自适应 | ✅ **推荐** |
| **C** 异步持久化 | ⭐⭐⭐ 中等 | ⭐⭐⭐⭐ 根治 | 🎯 长期方案 |

---

## ⚡ 立即行动（推荐方案 B）

### Step 1：定位代码

`internal/executor/executor.go` 第 417 行附近

### Step 2：修改 Request 结构体

```go
type Request struct {
    // ... 现有字段 ...
    RunTimeout time.Duration  // 已有
}
```

### Step 3：修改 run() 函数

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

    // 【修改这部分】
    // 原：30 秒固定超时
    // 新：基于 runTimeout 动态计算
    persistTimeout := 2 * time.Minute
    if req.RunTimeout > 0 {
        persistTimeout = req.RunTimeout + 1*time.Minute
        if persistTimeout < 2*time.Minute {
            persistTimeout = 2 * time.Minute
        }
    }
    persistCtx, persistCancel := context.WithTimeout(
        context.Background(),
        persistTimeout,
    )
    
    // ... 后续代码不变 ...
}
```

### Step 4：测试

```bash
# 测试 95 秒的长任务
curl -X POST http://localhost:8080/api/sessions/{sessionID}/messages \
  -d '{"content": "这是一个长任务", "stream": true}' \
  -H "Content-Type: application/json"

# 观察日志，不应该再出现 context deadline exceeded
```

---

## 📋 额外建议

### 1. 添加监控告警

```go
// 在 appendToolCallMessage 中
if err != nil {
    if errors.Is(err, context.DeadlineExceeded) {
        metrics.Inc("persist_deadline_exceeded")  // 发送告警
    }
    slog.Warn("工具调用消息落库失败", "err", err, 
        "session_id", sessionID, "call_id", call.ID)
}
```

### 2. 添加日志用于调试

```go
// 在 run() 开始时
slog.InfoContext(runCtx, "执行任务启动",
    "session_id", req.SessionID,
    "run_timeout", req.RunTimeout,
    "persist_timeout", persistTimeout,  // 新增
)
```

### 3. 考虑数据库优化

- 检查 SQLite 的连接池配置
- 考虑使用 Redis 作为高速缓存层
- 在高并发场景下考虑切换到 PostgreSQL

---

## 📌 总结

| 项目 | 说明 |
|------|------|
| **问题** | persistCtx 的 30 秒超时对长任务太短 |
| **表现** | 工具调用/结果落库失败 |
| **影响** | 工具历史丢失，模型重复调用工具 |
| **根本原因** | 设计正确但配置不当 |
| **快速修复** | 方案 A：增加超时 |
| **推荐方案** | 方案 B：动态超时（1-5 行代码） |
| **长期方案** | 方案 C：异步持久化 |

