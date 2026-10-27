# Bug 分析：前端消息展示不完整

## 问题现象
后端返回的AI回复包含完整内容，但前端只展示了部分内容。
示例：
- 后端返回：`**当前Python版本：Python 3.14.0**\n\n系统中只安装了 python3...`
- 前端显示：缺少上述内容

## 根本原因分析

### 1. SSE 事件缓冲丢失 ⚠️ **主要原因**

**位置**：`internal/sse/hub.go` 第 62-68 行

```go
// 慢订阅者：缓冲满时丢弃该条，由后续 last_event_id 补齐兜底，绝不在写端阻塞。
for _, ch := range st.subs {
    select {
    case ch <- ev:
    default:              // ← 缓冲满时直接丢弃！
    }
}
```

**问题**：
- 每个订阅者通道缓冲大小固定为 256（第 87 行）
- 当生成速度过快，前端处理不及时时，事件会被丢弃
- 丢弃的事件包括：`message.delta`（文本增量）、工具调用等

### 2. 环形缓冲可能裁剪 ⚠️ **次要原因**

**位置**：`internal/sse/buffer.go` 第 22-30 行

```go
func (b *Buffer) Push(ev Event) uint64 {
    b.buf = append(b.buf, ev)
    if len(b.buf) > b.size {
        b.buf = b.buf[len(b.buf)-b.size:]  // ← 超容量时丢弃最旧事件
    }
    return ev.ID
}
```

**问题**：
- 全局 SSE 缓冲容量由 `SSE_BUFFER_SIZE` 控制（默认 1000）
- 当事件过多时，最旧的事件被裁剪
- 重连时若 `last_event_id` 早于缓冲下界，无法补齐

### 3. 前端未实现重连补齐机制 ⚠️ **防御缺陷**

**位置**：`web/src/state/chatStore.tsx`

**问题**：
- 当前端 SSE 连接断开时，缺失的 `message.delta` 事件无法恢复
- 虽然后端支持 `resync` 机制，但前端需要显式处理
- 现有代码未见实现与 resync 关联的逻辑

## 影响范围

### 何时触发
1. **生成文本过长**：长文本生成速度快，前端处理不及时 → 事件缓冲满 → 丢弃
2. **网络抖动**：短暂断连，重连时错过的事件已被裁剪
3. **高延迟客户端**：浏览器卡顿，事件消费慢 → 缓冲溢出

### 表现
- 后端日志显示 `persistMessage` 调用成功，消息完整落库
- 前端 SSE 接收不到部分 `message.delta` 事件
- 用户界面显示内容不完整

## 验证方式

1. **后端验证**：
```bash
sqlite3 ~/Library/Application\ Support/agent-rebuild/agent.db \
  "SELECT id, length(content) as len FROM messages WHERE role='assistant' ORDER BY created_at DESC LIMIT 3;"
```
查看实际存储的消息长度

2. **前端验证**：
打开浏览器开发者工具 → Network → EventStream
观察接收到的 `message.delta` 事件数量与内容完整性

3. **重现步骤**：
- 发起一个预期会产生长回复的问题
- 在回复过程中故意刷新页面（网络抖动模拟）
- 观察重连后内容是否缺失

## 修复方案

### 方案 A：增加订阅者缓冲大小（短期止血）
**优点**：改动最少，立即生效  
**缺点**：治标不治本，大文本仍会丢弃

```go
// internal/sse/hub.go 第 87 行
ch := make(chan Event, 256)  // ← 改为 4096 或根据配置
```

**配置化建议**：
```go
type Config struct {
    // ...
    SSESubscriberBufferSize int  // 新增，默认 1024
}

// 然后在 hub.go 中使用
ch := make(chan Event, h.bufferSize)
```

### 方案 B：增加全局缓冲容量（根本修复）
**优点**：保证重连补齐成功率  
**缺点**：内存占用增加

```go
// .env
SSE_BUFFER_SIZE=10000  # 从 1000 提升到 10000
```

### 方案 C：实现前端重连补齐逻辑（完整防御）
**优点**：客户端主动拉取全量消息，绝不丢失  
**缺点**：需要重构前端状态管理

```typescript
// web/src/state/chatStore.tsx
case 'message.resync': {
    // 接收全量消息快照，重建本地状态
    dispatch({
        type: 'messages/replace',
        payload: d.messages
    })
    break
}
```

### 方案 D：推荐的综合方案（生产级）

**1. 后端改进**：
```go
// config.go
SSESubscriberBufferSize: envInt("SSE_SUBSCRIBER_BUFFER_SIZE", 4096),
SSEBufferSize:           envInt("SSE_BUFFER_SIZE", 10000),

// sse/hub.go
ch := make(chan Event, h.subscriberBufferSize)
```

**2. 前端改进**：
```typescript
// chatStore.tsx 添加 resync 处理
case 'resync': {
    const d = ev.data as ResyncData
    // 清空旧消息，用快照替换
    dispatch({ type: 'messages/reset', payload: d.messages })
    break
}

// 连接断开时标记需要重连
window.addEventListener('beforeunload', () => {
    // 保存 lastEventId 到 localStorage
    localStorage.setItem('lastEventId', lastEventId.toString())
})
```

**3. 文档更新**：
更新 `.env.example` 和用户指南，推荐配置：
```env
SSE_SUBSCRIBER_BUFFER_SIZE=4096
SSE_BUFFER_SIZE=10000
```

## 代码修改位置

| 文件 | 行号 | 改动 |
|------|------|------|
| `internal/config/config.go` | ~70 | 新增 `SSESubscriberBufferSize` 配置 |
| `internal/sse/hub.go` | 87 | 使用配置的缓冲大小 |
| `.env.example` | ~20 | 新增缓冲大小说明注释 |
| `web/src/state/chatStore.tsx` | ~650 | 处理 resync 事件 |
| `web/src/state/types.ts` | - | 定义 `ResyncData` 类型 |

## 优先级
🔴 **高** - 影响消息完整性，影响用户体验和数据准确性

## 测试用例

```go
// 在 api/messages_test.go 中添加
func TestMessageDeltaNotLost(t *testing.T) {
    // 1. 创建会话
    // 2. 发送会产生长回复的问题
    // 3. 验证所有 message.delta 事件都被接收
    // 4. 验证最终内容与落库消息完全一致
}

func TestSSEBufferCapacityHandle(t *testing.T) {
    // 1. 配置小的缓冲大小（如 10）
    // 2. 发起快速生成
    // 3. 断连重连
    // 4. 验证是否收到 resync 或补齐事件
}
```
