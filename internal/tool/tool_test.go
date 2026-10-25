package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRegistry 创建带临时工作区的注册表。
func newTestRegistry(t *testing.T, enableCommand bool) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	reg := DefaultRegistry(Options{Workspace: root, EnableCommand: enableCommand})
	return reg, root
}

func invoke(t *testing.T, reg *Registry, name string, args any) Result {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("序列化参数: %v", err)
		}
		raw = b
	}
	res, err := reg.Invoke(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("Invoke(%s): %v", name, err)
	}
	return res
}

// ---- 注册表 ----

func TestDefaultRegistryTools(t *testing.T) {
	reg, _ := newTestRegistry(t, false)

	// 默认应包含 5 个工具（不含命令执行）。
	if reg.Len() != 5 {
		t.Fatalf("工具数 = %d，期望 5：%v", reg.Len(), reg.Names())
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "search"} {
		if _, ok := reg.Get(want); !ok {
			t.Fatalf("缺少工具 %q，实际: %v", want, reg.Names())
		}
	}
	// 命令执行默认不启用（危险）。
	if _, ok := reg.Get("run_command"); ok {
		t.Fatal("命令执行工具默认不应启用")
	}
}

func TestCommandToolOptIn(t *testing.T) {
	reg, _ := newTestRegistry(t, true)
	if _, ok := reg.Get("run_command"); !ok {
		t.Fatal("显式开启后应有 run_command")
	}
}

func TestReadOnlyMode(t *testing.T) {
	root := t.TempDir()
	reg := DefaultRegistry(Options{Workspace: root, ReadOnly: true})
	if reg.Len() != 3 {
		t.Fatalf("只读模式工具数 = %d，期望 3", reg.Len())
	}
	if _, ok := reg.Get("write_file"); ok {
		t.Fatal("只读模式不应有写工具")
	}
}

func TestUnknownToolReturnsError(t *testing.T) {
	reg, _ := newTestRegistry(t, false)
	res := invoke(t, reg, "nonexistent", nil)
	if !res.IsError {
		t.Fatal("未知工具应返回错误")
	}
	// 错误信息应列出可用工具，帮助模型纠正。
	if !strings.Contains(res.Output, "read_file") {
		t.Fatalf("错误信息应列出可用工具: %q", res.Output)
	}
}

func TestUnknownArgRejected(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "hello")

	// 模型常臆造参数名，应明确报错而非静默忽略。
	res := invoke(t, reg, "read_file", map[string]any{"path": "a.txt", "bogus_field": 1})
	if !res.IsError {
		t.Fatal("未知参数应报错")
	}
	if !strings.Contains(res.Output, "bogus_field") {
		t.Fatalf("应指出未知字段: %q", res.Output)
	}
}

// ---- read_file ----

func writeTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	abs := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return abs
}

func TestReadFileWithLineNumbers(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.go", "package main\n\nfunc main() {}\n")

	res := invoke(t, reg, "read_file", map[string]any{"path": "a.go"})
	if res.IsError {
		t.Fatalf("读取失败: %s", res.Output)
	}
	// 应带行号，便于模型定位编辑位置。
	if !strings.Contains(res.Output, "1\tpackage main") {
		t.Fatalf("应包含带行号的内容: %q", res.Output)
	}
	if !strings.Contains(res.Output, "3\tfunc main() {}") {
		t.Fatalf("行号不正确: %q", res.Output)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "line"+string(rune('0'+i%10)))
	}
	writeTestFile(t, root, "n.txt", strings.Join(lines, "\n"))

	res := invoke(t, reg, "read_file", map[string]any{"path": "n.txt", "offset": 3, "limit": 2})
	if res.IsError {
		t.Fatalf("读取失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "3\t") || !strings.Contains(res.Output, "4\t") {
		t.Fatalf("应只返回第 3-4 行: %q", res.Output)
	}
	if strings.Contains(res.Output, "5\t") {
		t.Fatalf("limit 未生效: %q", res.Output)
	}
	if !res.Truncated {
		t.Fatal("limit 触发时应标记截断")
	}
}

func TestReadFileNotFound(t *testing.T) {
	reg, _ := newTestRegistry(t, false)
	res := invoke(t, reg, "read_file", map[string]any{"path": "nope.txt"})
	if !res.IsError {
		t.Fatal("不存在的文件应报错")
	}
	if !strings.Contains(res.Output, "不存在") {
		t.Fatalf("错误信息应说明文件不存在: %q", res.Output)
	}
}

func TestReadFileIsDirectory(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := invoke(t, reg, "read_file", map[string]any{"path": "sub"})
	if !res.IsError || !strings.Contains(res.Output, "list_dir") {
		t.Fatalf("读目录应提示改用 list_dir: %q", res.Output)
	}
}

// 安全边界：不能读到工作区之外。
func TestReadFilePathTraversalBlocked(t *testing.T) {
	reg, _ := newTestRegistry(t, false)
	cases := []string{
		"../secret.txt",
		"../../etc/passwd",
		"/etc/passwd",
		"sub/../../outside.txt",
	}
	for _, p := range cases {
		res := invoke(t, reg, "read_file", map[string]any{"path": p})
		if !res.IsError {
			t.Fatalf("路径 %q 应被拒绝，实际成功: %q", p, res.Output)
		}
		if !strings.Contains(res.Output, "超出") {
			t.Fatalf("路径 %q 的错误信息应说明越界: %q", p, res.Output)
		}
	}
}

// ---- write_file ----

func TestWriteFileCreatesDirs(t *testing.T) {
	reg, root := newTestRegistry(t, false)

	res := invoke(t, reg, "write_file", map[string]any{
		"path":    "deep/nested/file.txt",
		"content": "hello",
	})
	if res.IsError {
		t.Fatalf("写入失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "已创建") {
		t.Fatalf("应报告创建: %q", res.Output)
	}

	data, err := os.ReadFile(filepath.Join(root, "deep/nested/file.txt"))
	if err != nil {
		t.Fatalf("文件未创建: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("内容 = %q", string(data))
	}
}

func TestWriteFileOverwrites(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "old content")

	res := invoke(t, reg, "write_file", map[string]any{"path": "a.txt", "content": "new"})
	if !strings.Contains(res.Output, "已覆盖") {
		t.Fatalf("应报告覆盖: %q", res.Output)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "new" {
		t.Fatalf("内容 = %q", string(data))
	}
}

func TestWriteFilePathTraversalBlocked(t *testing.T) {
	reg, _ := newTestRegistry(t, false)
	res := invoke(t, reg, "write_file", map[string]any{
		"path":    "../evil.txt",
		"content": "x",
	})
	if !res.IsError {
		t.Fatal("越界写入应被拒绝")
	}
}

// ---- edit_file ----

func TestEditFileUniqueReplace(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.go", "func old() {\n\treturn\n}\n")

	res := invoke(t, reg, "edit_file", map[string]any{
		"path":       "a.go",
		"old_string": "func old()",
		"new_string": "func renamed()",
	})
	if res.IsError {
		t.Fatalf("编辑失败: %s", res.Output)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(data), "func renamed()") {
		t.Fatalf("替换未生效: %q", string(data))
	}
	if strings.Contains(string(data), "func old()") {
		t.Fatalf("旧内容仍在: %q", string(data))
	}
}

func TestEditFileNotUniqueFails(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "dup\ndup\n")

	res := invoke(t, reg, "edit_file", map[string]any{
		"path":       "a.txt",
		"old_string": "dup",
		"new_string": "x",
	})
	if !res.IsError {
		t.Fatal("多处匹配且未指定 replace_all 应失败")
	}
	if !strings.Contains(res.Output, "2 次") {
		t.Fatalf("应指出出现次数: %q", res.Output)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "dup\ndup\n")

	res := invoke(t, reg, "edit_file", map[string]any{
		"path":        "a.txt",
		"old_string":  "dup",
		"new_string":  "x",
		"replace_all": true,
	})
	if res.IsError {
		t.Fatalf("replace_all 应成功: %s", res.Output)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "x\nx\n" {
		t.Fatalf("内容 = %q", string(data))
	}
}

func TestEditFileNotFoundString(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "hello")

	res := invoke(t, reg, "edit_file", map[string]any{
		"path":       "a.txt",
		"old_string": "nonexistent",
		"new_string": "x",
	})
	if !res.IsError {
		t.Fatal("未找到 old_string 应报错")
	}
	// 应给出可操作的提示。
	if !strings.Contains(res.Output, "read_file") {
		t.Fatalf("应提示先读取文件: %q", res.Output)
	}
}

func TestEditFileEmptyOldString(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "x")

	res := invoke(t, reg, "edit_file", map[string]any{
		"path":       "a.txt",
		"old_string": "",
		"new_string": "y",
	})
	if !res.IsError {
		t.Fatal("空 old_string 应报错")
	}
}

// ---- list_dir ----

func TestListDir(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "x")
	writeTestFile(t, root, "sub/b.txt", "y")
	// 噪音目录应被跳过。
	if err := os.MkdirAll(filepath.Join(root, "node_modules/pkg"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := invoke(t, reg, "list_dir", map[string]any{"path": "."})
	if res.IsError {
		t.Fatalf("列举失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.txt") || !strings.Contains(res.Output, "sub/") {
		t.Fatalf("应列出文件与目录: %q", res.Output)
	}
	if strings.Contains(res.Output, "node_modules") {
		t.Fatalf("应跳过 node_modules: %q", res.Output)
	}
}

func TestListDirRecursive(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "sub/b.txt", "y")

	res := invoke(t, reg, "list_dir", map[string]any{"path": ".", "recursive": true})
	if res.IsError {
		t.Fatalf("递归列举失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "b.txt") {
		t.Fatalf("递归应列出子文件: %q", res.Output)
	}
}

func TestListDirNotFound(t *testing.T) {
	reg, _ := newTestRegistry(t, false)
	res := invoke(t, reg, "list_dir", map[string]any{"path": "nope"})
	if !res.IsError {
		t.Fatal("不存在的目录应报错")
	}
}

// ---- search ----

func TestSearch(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.go", "func Hello() {}\nvar x = 1\n")
	writeTestFile(t, root, "b.txt", "Hello world\n")

	res := invoke(t, reg, "search", map[string]any{"pattern": "Hello"})
	if res.IsError {
		t.Fatalf("搜索失败: %s", res.Output)
	}
	// 应返回 文件:行号:内容 格式。
	if !strings.Contains(res.Output, "a.go:1:") {
		t.Fatalf("应包含 a.go:1: %q", res.Output)
	}
	if !strings.Contains(res.Output, "b.txt:1:") {
		t.Fatalf("应包含 b.txt:1: %q", res.Output)
	}
}

func TestSearchGlobFilter(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.go", "target\n")
	writeTestFile(t, root, "b.txt", "target\n")

	res := invoke(t, reg, "search", map[string]any{"pattern": "target", "glob": "*.go"})
	if res.IsError {
		t.Fatalf("搜索失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.go") {
		t.Fatalf("应包含 .go 文件: %q", res.Output)
	}
	if strings.Contains(res.Output, "b.txt") {
		t.Fatalf("glob 未生效，包含 .txt: %q", res.Output)
	}
}

func TestSearchIgnoreCase(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "HELLO\n")

	res := invoke(t, reg, "search", map[string]any{"pattern": "hello", "ignore_case": true})
	if !strings.Contains(res.Output, "HELLO") {
		t.Fatalf("忽略大小写未生效: %q", res.Output)
	}
}

func TestSearchInvalidRegex(t *testing.T) {
	reg, _ := newTestRegistry(t, false)
	res := invoke(t, reg, "search", map[string]any{"pattern": "["})
	if !res.IsError {
		t.Fatal("非法正则应报错")
	}
	if !strings.Contains(res.Output, "正则") && !strings.Contains(res.Output, "正则") {
		t.Fatalf("应说明正则错误: %q", res.Output)
	}
}

func TestSearchNoMatch(t *testing.T) {
	reg, root := newTestRegistry(t, false)
	writeTestFile(t, root, "a.txt", "content")
	res := invoke(t, reg, "search", map[string]any{"pattern": "zzzz"})
	if res.IsError {
		t.Fatal("无匹配不应视为错误")
	}
	if !strings.Contains(res.Output, "未找到") {
		t.Fatalf("应说明无匹配: %q", res.Output)
	}
}

// ---- run_command ----

func TestRunCommand(t *testing.T) {
	reg, _ := newTestRegistry(t, true)

	res := invoke(t, reg, "run_command", map[string]any{"command": "echo hello-from-shell"})
	if res.IsError {
		t.Fatalf("命令失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello-from-shell") {
		t.Fatalf("缺少输出: %q", res.Output)
	}
	if !strings.Contains(res.Output, "退出码 0") {
		t.Fatalf("应报告退出码: %q", res.Output)
	}
}

func TestRunCommandNonZeroExit(t *testing.T) {
	reg, _ := newTestRegistry(t, true)
	res := invoke(t, reg, "run_command", map[string]any{"command": "exit 3"})
	if !res.IsError {
		t.Fatal("非零退出码应标记为错误")
	}
	if !strings.Contains(res.Output, "退出码 3") {
		t.Fatalf("应报告退出码 3: %q", res.Output)
	}
}

func TestRunCommandTimeout(t *testing.T) {
	reg, _ := newTestRegistry(t, true)
	res := invoke(t, reg, "run_command", map[string]any{
		"command":    "sleep 5",
		"timeout_ms": 300,
	})
	if !res.IsError {
		t.Fatal("超时应报错")
	}
	if !strings.Contains(res.Output, "超时") {
		t.Fatalf("应说明超时: %q", res.Output)
	}
}

// 命令必须在工作目录下执行。
func TestRunCommandWorkdir(t *testing.T) {
	reg, root := newTestRegistry(t, true)
	writeTestFile(t, root, "marker.txt", "x")

	res := invoke(t, reg, "run_command", map[string]any{"command": "ls marker.txt"})
	if res.IsError {
		t.Fatalf("命令失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "marker.txt") {
		t.Fatalf("应在工作目录执行: %q", res.Output)
	}
}

// 敏感环境变量不应透传给命令。
func TestRunCommandDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("SECRET_API_KEY", "sk-should-not-leak")
	reg, _ := newTestRegistry(t, true)

	res := invoke(t, reg, "run_command", map[string]any{
		"command": "printenv SECRET_API_KEY || echo NOT_SET",
	})
	if strings.Contains(res.Output, "sk-should-not-leak") {
		t.Fatalf("敏感环境变量泄漏给了命令: %q", res.Output)
	}
	if !strings.Contains(res.Output, "NOT_SET") {
		t.Fatalf("预期命令读不到该变量: %q", res.Output)
	}
}

func TestRunCommandOutputTruncated(t *testing.T) {
	reg, _ := newTestRegistry(t, true)
	// 产生超过 64KB 的输出。
	res := invoke(t, reg, "run_command", map[string]any{
		"command": "seq 1 200000",
	})
	if !res.Truncated {
		t.Fatalf("大输出应标记截断（输出长度 %d）", len(res.Output))
	}
	if !strings.Contains(res.Output, "截断") {
		t.Fatalf("应提示截断: %q", res.Output[:min(200, len(res.Output))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- 通用辅助 ----

func TestTruncate(t *testing.T) {
	t.Run("未超限不截断", func(t *testing.T) {
		s, truncated := Truncate("short", 100)
		if truncated || s != "short" {
			t.Fatalf("不应截断: %q %v", s, truncated)
		}
	})

	t.Run("超限保留头尾", func(t *testing.T) {
		long := strings.Repeat("a", 50) + "MIDDLE" + strings.Repeat("b", 50)
		s, truncated := Truncate(long, 60)
		if !truncated {
			t.Fatal("应标记截断")
		}
		if !strings.HasPrefix(s, "aaa") {
			t.Fatalf("应保留开头: %q", s[:20])
		}
		if !strings.HasSuffix(s, "bbb") {
			t.Fatalf("应保留结尾: %q", s[len(s)-20:])
		}
		if !strings.Contains(s, "省略") {
			t.Fatalf("应说明省略: %q", s)
		}
	})

	t.Run("中文按字符切分", func(t *testing.T) {
		// 不应切断多字节字符导致乱码。
		s, _ := Truncate(strings.Repeat("中", 100), 20)
		if !strings.Contains(s, "中") {
			t.Fatalf("中文被破坏: %q", s)
		}
	})
}

func TestRiskRequiresApproval(t *testing.T) {
	cases := map[Risk]bool{
		RiskSafe:      false,
		RiskModerate:  true,
		RiskDangerous: true,
	}
	for risk, want := range cases {
		if got := risk.RequiresApproval(); got != want {
			t.Fatalf("%s.RequiresApproval() = %v，期望 %v", risk, got, want)
		}
	}
}

func TestSpecsAreValidJSON(t *testing.T) {
	reg, _ := newTestRegistry(t, true)
	for _, s := range reg.Specs() {
		if s.Name == "" || s.Description == "" {
			t.Fatalf("工具 %q 缺少名称或描述", s.Name)
		}
		var probe map[string]any
		if err := json.Unmarshal(s.Parameters, &probe); err != nil {
			t.Fatalf("工具 %q 的参数 Schema 不是合法 JSON: %v", s.Name, err)
		}
		if probe["type"] != "object" {
			t.Fatalf("工具 %q 的参数 Schema 应为 object", s.Name)
		}
	}
}
