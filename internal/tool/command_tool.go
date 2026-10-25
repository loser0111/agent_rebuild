package tool

// 命令执行工具。
//
// 这是最危险的工具，设计上做了多重约束（参考 Codex / Claude Code）：
//   - 工作目录锁定在 workspace
//   - 超时强制终止（含子进程）
//   - 输出截断
//   - 环境变量白名单（不泄漏 API Key 等敏感信息）
//   - 默认需要人工审批（RiskDangerous）

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultCommandTimeout 是命令执行默认超时。
const defaultCommandTimeout = 120 * time.Second

// maxCommandOutput 是命令输出上限（字节）。
const maxCommandOutput = 64 << 10

type runCommandArgs struct {
	Command string `json:"command"`
	// TimeoutMs 可选，覆盖默认超时（上限 10 分钟）。
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

type runCommandTool struct {
	root    string
	timeout time.Duration
	// allowEnv 是要透传给子进程的环境变量白名单。
	// 不传全量环境，避免把 API Key 等泄漏给命令。
	allowEnv []string
}

// NewRunCommandTool 创建命令执行工具。
func NewRunCommandTool(root string) Tool {
	return &runCommandTool{
		root:    root,
		timeout: defaultCommandTimeout,
		// 只透传与执行相关的基础变量。
		allowEnv: []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "SHELL", "TERM", "USER"},
	}
}

func (t *runCommandTool) Spec() Spec {
	return Spec{
		Name: "run_command",
		Description: "在工作目录下执行 shell 命令并返回输出（stdout+stderr）。" +
			"用于构建、测试、运行脚本。**需要用户确认后才会执行**。" +
			"命令有超时限制（默认 120 秒），输出过长会被截断。" +
			"不要用它做文件读写（有专门的 read_file/write_file/edit_file）。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"要执行的命令（通过 shell 执行）"},
				"timeout_ms":{"type":"integer","description":"超时毫秒数，可选，默认 120000，上限 600000"}
			},
			"required":["command"]
		}`),
		Risk: RiskDangerous,
	}
}

func (t *runCommandTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args runCommandArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return Result{}, err
	}
	cmdStr := strings.TrimSpace(args.Command)
	if cmdStr == "" {
		return Result{Output: "command 不能为空", IsError: true}, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	timeout := t.timeout
	if args.TimeoutMs > 0 {
		timeout = time.Duration(args.TimeoutMs) * time.Millisecond
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 通过 shell 执行，支持管道与重定向（用户预期如此）。
	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr)
	if _, err := exec.LookPath("sh"); err != nil {
		cmd = exec.CommandContext(runCtx, "cmd", "/C", cmdStr) // Windows
	}
	cmd.Dir = t.root
	cmd.Env = buildEnv(t.allowEnv)

	// 关键：让子进程在超时/取消时连同子进程一起被杀死，
	// 否则 sh -c 会留下孙进程。
	configureProcessGroup(cmd)

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutW := &limitedWriter{buf: &stdoutBuf, limit: maxCommandOutput}
	stderrW := &limitedWriter{buf: &stderrBuf, limit: maxCommandOutput}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	var sb strings.Builder
	fmt.Fprintf(&sb, "$ %s\n", cmdStr)

	if stdoutBuf.Len() > 0 {
		sb.WriteString(stdoutBuf.String())
		if !strings.HasSuffix(stdoutBuf.String(), "\n") {
			sb.WriteString("\n")
		}
	}
	if stderrBuf.Len() > 0 {
		sb.WriteString("--- stderr ---\n")
		sb.WriteString(stderrBuf.String())
		if !strings.HasSuffix(stderrBuf.String(), "\n") {
			sb.WriteString("\n")
		}
	}

	truncated := stdoutW.Truncated() || stderrW.Truncated()
	if truncated {
		sb.WriteString(fmt.Sprintf("... 输出已截断（上限 %d KB）\n", maxCommandOutput/1024))
	}
	fmt.Fprintf(&sb, "[退出码 %d，耗时 %s]\n", exitCode(runErr), elapsed.Round(time.Millisecond))

	// 超时与取消要明确区分：前者模型可重试（缩短操作），后者是用户中断。
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return Result{
			Output:  sb.String() + fmt.Sprintf("命令超时（%s）已被终止", timeout),
			IsError: true,
		}, nil
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		return Result{Output: sb.String() + "命令已被取消", IsError: true}, nil
	}

	return Result{
		Output:    sb.String(),
		IsError:   runErr != nil,
		Truncated: truncated,
	}, nil
}

func buildEnv(allow []string) []string {
	env := make([]string, 0, len(allow)+2)
	for _, key := range allow {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	// 保证子进程输出不缓冲（便于看到实时结果）。
	env = append(env, "PYTHONUNBUFFERED=1", "NO_COLOR=1")
	return env
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// limitedWriter 是带总量上限的 writer，超出部分丢弃并标记。
type limitedWriter struct {
	buf       *bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil // 假装写入成功，避免命令因 EPIPE 失败
	}
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *limitedWriter) Truncated() bool { return w.truncated }
