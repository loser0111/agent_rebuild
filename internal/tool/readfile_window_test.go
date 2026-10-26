package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLines 生成一个含 n 行的测试文件，每行内容为 "line-N"。
func writeLines(t *testing.T, dir, name string, n int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	return path
}

func readFile(t *testing.T, reg *Registry, args map[string]any) Result {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := reg.Invoke(context.Background(), "read_file", raw)
	if err != nil {
		t.Fatalf("调用 read_file 失败: %v", err)
	}
	return res
}

// TestReadFileDefaultWindow 大文件默认只返回前 defaultReadLines 行，
// 并明确提示如何继续——避免模型整篇读入、白白烧掉上下文。
func TestReadFileDefaultWindow(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, dir, "big.txt", defaultReadLines+150)

	reg := DefaultRegistry(Options{Workspace: dir})
	res := readFile(t, reg, map[string]any{"path": "big.txt"})

	if !res.Truncated {
		t.Fatal("超过默认窗口应标记 Truncated")
	}
	if !strings.Contains(res.Output, "line-1\n") {
		t.Fatal("应从第 1 行开始")
	}
	if !strings.Contains(res.Output, fmt.Sprintf("line-%d", defaultReadLines)) {
		t.Fatalf("应包含第 %d 行", defaultReadLines)
	}
	if strings.Contains(res.Output, fmt.Sprintf("line-%d", defaultReadLines+1)) {
		t.Fatalf("不应包含第 %d 行（超出默认窗口）", defaultReadLines+1)
	}

	// 关键：必须告诉模型怎么继续，且提示不要重读
	if !strings.Contains(res.Output, fmt.Sprintf("offset=%d", defaultReadLines+1)) {
		t.Fatalf("缺少续读提示 offset=%d，实际尾部: %s",
			defaultReadLines+1, tail(res.Output))
	}
	if !strings.Contains(res.Output, "不要重读") {
		t.Fatalf("缺少「不要重读」提示，实际尾部: %s", tail(res.Output))
	}
}

// TestReadFileExplicitLimitStillWorks 显式 limit 优先于默认窗口。
func TestReadFileExplicitLimitStillWorks(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, dir, "big.txt", 500)

	reg := DefaultRegistry(Options{Workspace: dir})
	res := readFile(t, reg, map[string]any{"path": "big.txt", "limit": 10})

	if !res.Truncated {
		t.Fatal("指定 limit 且未读完应标记 Truncated")
	}
	if !strings.Contains(res.Output, "line-10\n") {
		t.Fatal("应读取到第 10 行")
	}
	if strings.Contains(res.Output, "line-11") {
		t.Fatal("不应超过 limit=10")
	}
	// 显式 limit 用普通截断提示，不套用"默认窗口"话术
	if strings.Contains(res.Output, "默认每次最多") {
		t.Fatalf("显式 limit 不应出现默认窗口提示: %s", tail(res.Output))
	}
}

// TestReadFileSmallFileTellsEof 小文件读完应明确告知已到末尾，
// 防止模型再调一次确认。
func TestReadFileSmallFileTellsEof(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, dir, "small.txt", 20)

	reg := DefaultRegistry(Options{Workspace: dir})
	res := readFile(t, reg, map[string]any{"path": "small.txt"})

	if res.Truncated {
		t.Fatal("小文件不应标记 Truncated")
	}
	if !strings.Contains(res.Output, "文件已全部读取") {
		t.Fatalf("应提示已到末尾，实际尾部: %s", tail(res.Output))
	}
	if !strings.Contains(res.Output, "共 20 行") {
		t.Fatalf("应报告总行数，实际: %s", tail(res.Output))
	}
}

// TestReadFileOffsetContinues 用 offset 续读能拿到后续内容。
func TestReadFileOffsetContinues(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, dir, "big.txt", defaultReadLines+100)

	reg := DefaultRegistry(Options{Workspace: dir})
	res := readFile(t, reg, map[string]any{
		"path": "big.txt", "offset": defaultReadLines + 1,
	})

	if !strings.Contains(res.Output, fmt.Sprintf("line-%d", defaultReadLines+1)) {
		t.Fatalf("续读应从第 %d 行开始", defaultReadLines+1)
	}
	if strings.Contains(res.Output, "line-1\n") {
		t.Fatal("续读不应包含第 1 行")
	}
}

// TestReadFileExactlyAtWindow 行数恰好等于默认窗口时，不应误报截断。
func TestReadFileExactlyAtWindow(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, dir, "exact.txt", defaultReadLines)

	reg := DefaultRegistry(Options{Workspace: dir})
	res := readFile(t, reg, map[string]any{"path": "exact.txt"})

	if res.Truncated {
		t.Fatalf("恰好 %d 行不应标记截断，输出尾部: %s", defaultReadLines, tail(res.Output))
	}
}

// TestReadFileSpecMentionsDefaultLimit 工具描述需告知默认窗口，模型才知道分段。
func TestReadFileSpecMentionsDefaultLimit(t *testing.T) {
	reg := DefaultRegistry(Options{Workspace: t.TempDir()})
	tl, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("read_file 未注册")
	}
	spec := tl.Spec()
	if !strings.Contains(spec.Description, fmt.Sprintf("%d", defaultReadLines)) {
		t.Fatalf("描述应提到默认 %d 行: %s", defaultReadLines, spec.Description)
	}
	if !strings.Contains(string(spec.Parameters), fmt.Sprintf("默认 %d", defaultReadLines)) {
		t.Fatalf("参数说明应提到默认值: %s", spec.Parameters)
	}
}

// tail 返回字符串末尾若干字符，便于断言失败时定位。
func tail(s string) string {
	r := []rune(s)
	if len(r) <= 160 {
		return s
	}
	return "…" + string(r[len(r)-160:])
}
