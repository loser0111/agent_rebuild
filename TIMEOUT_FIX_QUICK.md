# 快速修复指南

## 🎯 5 分钟快速修复

### 问题确认

检查日志中是否有这些错误：
```
context deadline exceeded
工具结果落库失败
工具调用消息落库失败
```

如果有，那你的项目就受到了这个问题的影响。

---

## ⚡ 推荐修复（方案 B - 动态超时）

### 文件位置
```
agent_rebuild/internal/executor/executor.go
第 417 行附近
```

### 修改内容

**找到这部分代码**：
```go
persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)
```

**替换为**：
```go
// 计算持久化超时：给工具执行留充足时间
persistTimeout := time.Duration(float64(req.RunTimeout) * 2)
if persistTimeout < 2*time.Minute {
    persistTimeout = 2 * time.Minute  // 最小 2 分钟
}
persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
```

### 验证修改

```bash
# 1. 重新编译
make build

# 2. 启动服务
./bin/agent-server

# 3. 观察日志
tail -f /tmp/agent-server.log

# 4. 发送一个会触发工具调用的请求
# 日志中应该没有 "context deadline exceeded" 错误
```

---

## 🔍 验证修复是否有效

### 检查项

- [ ] 日志中没有出现 `context deadline exceeded`
- [ ] 工具调用记录正常落库
- [ ] 对话历史中包含完整的工具调用过程

### 测试命令

```bash
# 使用 mock runner 测试（无需 API Key）
curl -X POST http://localhost:8080/api/sessions/test-session/messages \
  -d '{"content": "分析这个文件 /tmp/test.txt", "stream": false}' \
  -H "Content-Type: application/json"

# 检查数据库中的消息记录
# sqlite3 ~/.local/agent_rebuild/sessions.db "SELECT role, content, status FROM messages ORDER BY created_at DESC LIMIT 10;"
```

---

## 🚨 如果问题还没有解决

### 可能的原因

1. **数据库本身很慢**
   - 检查 SQLite 文件大小：`ls -lh ~/.local/agent_rebuild/sessions.db`
   - 如果超过 100MB，可能需要 VACUUM 或迁移到 PostgreSQL

2. **工具执行特别耗时**
   - 在日志中添加时间戳
   - 找出哪个工具耗时最长

3. **修改没有生效**
   - 确保重新编译：`make clean && make build`
   - 确保停止了旧的进程

### 进阶诊断

在 `run()` 函数开始处添加日志：

```go
slog.InfoContext(
    runCtx,
    "执行任务启动",
    "session_id", req.SessionID,
    "run_timeout", req.RunTimeout,
    "persist_timeout", persistTimeout,
)
```

观察日志中的 `persist_timeout` 值是否合理（应该是 2 分钟或更长）。

---

## 📊 修复前后对比

### 修复前
```
[WARN] 工具调用消息落库失败       context deadline exceeded
[WARN] 工具结果落库失败           context deadline exceeded
[ERROR] 落库消息最终状态失败      context deadline exceeded
HTTP 请求耗时 95372ms（响应可能不完整）
```

### 修复后
```
[INFO] 执行任务启动                run_timeout=60s persist_timeout=2m
（无错误日志）
HTTP 请求耗时 95372ms（响应完整，所有数据已落库）
```

---

## 📝 下一步（可选但推荐）

### 添加监控告警

在代码中找到这些地方：

```go
// executor.go 第 735 行和 761 行
if err := e.store.AppendMessage(ctx, m); err != nil {
    if errors.Is(err, context.DeadlineExceeded) {
        // 新增：发送告警或指标
        metrics.Inc("persist_deadline_exceeded")
    }
    slog.Warn("工具调用消息落库失败", "err", err, ...)
}
```

这样可以及时发现问题。

---

## ❓ 常见问题

**Q: 为什么 30 秒不够？**
A: 因为 HTTP 请求耗时 95 秒（包括 LLM 调用、工具执行等），而工具调用的落库发生在这个过程中。当超过 30 秒时，persistCtx 已经超时了。

**Q: 改为 2 分钟会不会太长？**
A: 不会。2 分钟是一个合理的上限，足以覆盖大多数正常场景。如果真的出现数据库卡住，2 分钟后还会有日志警告。

**Q: 这是个 bug 吗？**
A: 更像是配置问题。原设计假设单次执行不超过 30 秒，但实际的工具调用可能更长。

