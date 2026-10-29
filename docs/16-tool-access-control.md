# 16 · 工具访问控制与调试模式

> 补充 `docs/09-tools.md` 第 2 节「安全设计」。

---

## 1. 工作目录限制（默认行为）

所有文件操作工具（`read_file`、`write_file`、`edit_file`、`list_dir`、`search`）都限制在 `TOOL_WORKSPACE` 内。

**设计意图**：防止 AI 助手越界访问敏感文件（如 `/etc/passwd`）。

**验证逻辑**（`internal/tool/fs_tools.go:54-98`）：

```go
rel, err := filepath.Rel(root, abs)
if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
    return "", fmt.Errorf("路径 %q 超出工作目录 %q 的范围", p, root)
}
```

**测试覆盖**（`internal/tool/workspace_test.go`）：
- `../secret.txt` → 拒绝
- `../../etc/passwd` → 拒绝
- `/etc/passwd` → 拒绝
- `sub/../../outside.txt` → 拒绝

---

## 2. 调试模式：突破工作目录限制

### 用途

| 场景 | 说明 |
|-----|------|
| 跨项目调研 | 访问兄弟目录的文件（如 `../local-agent/...`) |
| 集成测试 | 不受工作目录约束的测试 |
| 开发调试 | 快速定位问题而不用复制文件 |

### 启用方式

```bash
# 启用调试模式
TOOL_ALLOW_OUT_OF_BOUNDS_PATHS=true go run ./cmd/server

# 关闭（默认）
TOOL_ALLOW_OUT_OF_BOUNDS_PATHS=false go run ./cmd/server
```

或者设置在 `.env` 里：

```bash
TOOL_ALLOW_OUT_OF_BOUNDS_PATHS=true
```

### 安全保证

即使启用调试模式，仍然防止访问系统关键目录：

```go
blockedPrefixes := []string{
    "/etc/",
    "/sys/",
    "/var/",
    "/proc/",
    "C:\\Windows\\",
    "C:\\Program Files\\",
}
// 访问这些目录仍然被拒绝
```

### 示例

```bash
# 启用调试模式后，可以访问兄弟目录
TOOL_ALLOW_OUT_OF_BOUNDS_PATHS=true go run ./cmd/server

# 现在在聊天中可以：
# read_file ../local-agent/internal/tool/mcp/client.go
# list_dir ../local-agent/docs
```

---

## 3. 实现细节

### 改动位置

**文件**：`internal/tool/fs_tools.go`

**改动**：
1. 行 24-30：新增 `allowOutOfBoundsPaths` 全局开关
2. 行 54-98：改进 `resolve()` 函数的越界检查

### 向后兼容性

✅ **完全兼容**：默认行为不变，调试模式是 opt-in

### 测试

建议新增测试（`internal/tool/outofbounds_test.go`）：

```go
func TestResolveWithDebugMode(t *testing.T) {
    // 设置环境变量
    oldVal := os.Getenv("TOOL_ALLOW_OUT_OF_BOUNDS_PATHS")
    os.Setenv("TOOL_ALLOW_OUT_OF_BOUNDS_PATHS", "true")
    defer os.Setenv("TOOL_ALLOW_OUT_OF_BOUNDS_PATHS", oldVal)
    
    // 重新初始化 allowOutOfBoundsPaths 变量
    // 注意：这需要 fs_tools.go 中添加 init() 函数来支持
    
    // 测试越界路径
    cfg := fsConfig{Root: "/path/to/root"}
    abs, err := cfg.resolve("../sibling.txt")
    if err != nil {
        t.Fatal("应该允许越界访问")
    }
    // assert abs == "/path/to/sibling.txt"
}

func TestResolveBlocksSystemDirs(t *testing.T) {
    // 设置环境变量
    os.Setenv("TOOL_ALLOW_OUT_OF_BOUNDS_PATHS", "true")
    
    cfg := fsConfig{Root: "/tmp"}
    _, err := cfg.resolve("/etc/passwd")
    if err == nil {
        t.Fatal("应该拒绝访问 /etc/")
    }
}
```

---

## 4. 已知限制

### 环境变量在启动时读取

由于 Go 的 `os.Getenv()` 在程序启动时被调用，改动环境变量后需要**重启进程**才能生效，无法热切换。

如果需要运行时热切换，可以改为：

```go
func isDebugModeEnabled() bool {
    return os.Getenv("TOOL_ALLOW_OUT_OF_BOUNDS_PATHS") == "true"
}

// 在 resolve() 中使用
if !isDebugModeEnabled() {
    return "", fmt.Errorf("...")
}
```

但这会带来轻微的性能开销（每次路径解析都调用 `os.Getenv()`）。

---

## 5. 何时使用

### ✅ 应该启用

- 调研其他项目的实现时
- 跨项目集成测试
- 本地开发调试

### ❌ 不应该启用

- 生产部署
- 多用户环境（与不信任的代码交互）
- 安全敏感的场景

---

## 6. 安全建议

1. **永远不要在生产环境启用**
2. **文档中要明确标注这是调试功能**
3. **监控日志中的越界访问尝试**
4. **定期审计工具调用的文件路径**

