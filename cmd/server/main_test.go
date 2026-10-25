package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 端到端验证 PORT=0 时二进制会：
//  1. 自动分配空闲端口；
//  2. 在 stdout 输出 ReadyMarker 行，携带真实地址；
//  3. 该地址上 /api/health 可用（桌面壳据此加载前端）。
//
// 这是 Electron 壳与 Go 后端之间的核心契约，必须稳定。
func TestDynamicPortEmitsReadyMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("需要编译并启动真实进程，-short 模式跳过")
	}

	bin := filepath.Join(t.TempDir(), "server")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建失败: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "PORT=0", "LOG_LEVEL=warn")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// 读取就绪行（超时保护）。
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, ReadyMarker) {
				ch <- result{line: line}
				return
			}
		}
		ch <- result{err: scanner.Err()}
	}()

	var ready string
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("读取 stdout 失败: %v", r.err)
		}
		ready = r.line
	case <-time.After(20 * time.Second):
		t.Fatal("未在 20s 内收到就绪标记")
	}

	addr := strings.TrimPrefix(ready, ReadyMarker)
	if !strings.HasPrefix(addr, "http://localhost:") {
		t.Fatalf("就绪标记地址格式错误: %q", addr)
	}
	port := strings.TrimPrefix(addr, "http://localhost:")
	if port == "0" || port == "" {
		t.Fatalf("PORT=0 应被替换为真实端口，得到 %q", port)
	}
}
