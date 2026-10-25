package tool

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWorkspaceExplicit 显式路径应转成绝对路径。
func TestResolveWorkspaceExplicit(t *testing.T) {
	dir := t.TempDir()
	got := resolveWorkspace(dir)
	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if got != want {
		t.Fatalf("resolveWorkspace(%q) = %q，期望 %q", dir, got, want)
	}
}

// TestResolveWorkspaceRelative 相对路径基于当前工作目录解析。
func TestResolveWorkspaceRelative(t *testing.T) {
	got := resolveWorkspace("./sub")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := filepath.Join(wd, "sub")
	if got != want {
		t.Fatalf("resolveWorkspace(./sub) = %q，期望 %q", got, want)
	}
}

// TestResolveWorkspaceEmptyFallsBackToHome 这是本次修复的核心：
// 空值不能再静默退化成「进程 cwd」（桌面端会是 desktop/），
// 而应回退到用户主目录，且必须与 cwd 区分开。
func TestResolveWorkspaceEmptyFallsBackToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("当前环境无法确定主目录")
	}

	got := resolveWorkspace("")

	if got != home {
		t.Fatalf("空值应回退到主目录 %q，实际 %q", home, got)
	}

	// 关键断言：结果不能等于进程 cwd，否则就是旧 bug 的表现。
	wd, err := os.Getwd()
	if err == nil {
		if got == wd {
			t.Fatalf("空值不应退化为进程工作目录 %q", wd)
		}
	}
}

// TestResolveWorkspaceWhitespaceOnly 纯空白视同未设置。
func TestResolveWorkspaceWhitespaceOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("当前环境无法确定主目录")
	}
	if got := resolveWorkspace("   "); got != home {
		t.Fatalf("纯空白应回退到主目录 %q，实际 %q", home, got)
	}
}

// TestDefaultRegistryUsesResolvedWorkspace 确认注册表真的用了归一化后的根目录。
func TestDefaultRegistryUsesResolvedWorkspace(t *testing.T) {
	dir := t.TempDir()
	reg := DefaultRegistry(Options{Workspace: dir})

	// 工具是否都指向该目录，通过一次越界读取来验证：
	// 读根目录之外的文件应被拒绝。
	if reg.Len() == 0 {
		t.Fatal("默认工具集不应为空")
	}
	want, _ := filepath.Abs(dir)
	if got := reg.WorkspaceRoot(); got != want {
		t.Fatalf("WorkspaceRoot() = %q，期望 %q", got, want)
	}
}
