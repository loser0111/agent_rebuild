package mcp

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// stopGrace 是关闭子进程每一步的等待时间（关 stdin → 等 → SIGTERM → 等 → SIGKILL）。
const stopGrace = 2 * time.Second

// stderrTailLines 保留的 stderr 尾部行数，用于把启动失败原因展示到状态面板。
const stderrTailLines = 20

// process 是一个 stdio MCP server 子进程。
//
// 为什么 stdout/stderr 用 os.Pipe 而不是 cmd.StdoutPipe：
// exec 文档明确说 Wait 会关闭 StdoutPipe 的读端，「在读完之前调用 Wait 是错误的」。
// 而我们恰恰需要一个独立 goroutine 尽早 Wait（才能及时知道进程退出），
// 同时让 Client 的读循环自己读到 EOF。自己建管道、Start 后关掉写端，
// 两件事就互不干扰。stdin 仍用 StdinPipe：Wait 关闭它正是我们想要的。
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File

	exited  chan struct{}
	exitErr error // exited 关闭后只读

	tail       *tailBuffer
	stderrDone chan struct{} // stderr 读完（EOF）后关闭

	stopOnce sync.Once
}

// startProcess 启动子进程。cwd 已由调用方解析为绝对路径。
func startProcess(sc ServerConfig, cwd string, log *slog.Logger) (*process, error) {
	cmd := exec.Command(resolveCommand(sc.Command, sc.Env), sc.Args...)
	cmd.Env = sc.Env
	cmd.Dir = cwd
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		_ = outR.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("创建 stderr 管道失败: %w", err)
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		for _, f := range []*os.File{outR, outW, errR, errW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("启动 %q 失败: %w", sc.Command, err)
	}
	// 子进程已经继承了写端；父进程这边必须关掉，否则子进程退出后读端永远等不到 EOF。
	_ = outW.Close()
	_ = errW.Close()

	p := &process{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     outR,
		exited:     make(chan struct{}),
		tail:       newTailBuffer(stderrTailLines),
		stderrDone: make(chan struct{}),
	}

	// stderr：逐行进日志并保留尾部。规范允许 server 往 stderr 打任何日志。
	go func() {
		defer close(p.stderrDone)
		defer errR.Close()
		scanner := bufio.NewScanner(errR)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			p.tail.add(line)
			log.Debug("MCP server stderr", "line", line)
		}
		// 超长行会让 Scanner 停下；必须继续把管道读空，
		// 否则 server 写 stderr 时会因管道满而阻塞，表现为「莫名卡死」。
		_, _ = io.Copy(io.Discard, errR)
	}()

	go func() {
		p.exitErr = cmd.Wait()
		close(p.exited)
	}()

	return p, nil
}

// stop 优雅关闭：关 stdin（规范约定的退出信号）→ 等 → SIGTERM 整组 → 等 → SIGKILL 整组。
// 可重复调用。返回时进程一定已退出，stdout 读端已关闭（Client 读循环随之结束）。
func (p *process) stop() {
	p.stopOnce.Do(func() {
		_ = p.stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(stopGrace):
			terminateGroup(p.cmd)
			select {
			case <-p.exited:
			case <-time.After(stopGrace):
				killGroup(p.cmd)
				<-p.exited
			}
		}
		// 进程已退出，但它派生的孙进程可能还握着 stdout 写端；
		// 主动关读端，保证 Client 读循环一定能结束。
		_ = p.stdout.Close()
		// 等 stderr 读完再返回，exitDetail 拿到的尾部才完整（启动即崩溃的 server，
		// 报错原因往往就在最后一行）。孙进程握着 stderr 时最多等 1s，不无限等。
		select {
		case <-p.stderrDone:
		case <-time.After(time.Second):
		}
	})
}

// exitedOnItsOwn 报告进程是否已经自行退出（在调用 stop 之前判断才有意义）。
func (p *process) exitedOnItsOwn() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// exitDetail 生成附加在错误信息后面的诊断：自行退出时带上退出状态，再附 stderr 尾部。
//
// selfExited 必须在 stop 之前取：stop 之后进程一定已退出，但那是我们杀的，
// 把 "signal: terminated" 展示给用户只会误导排查方向。
func (p *process) exitDetail(selfExited bool) string {
	var parts []string
	if selfExited {
		if p.exitErr != nil {
			parts = append(parts, "进程退出："+p.exitErr.Error())
		} else {
			parts = append(parts, "进程退出（exit 0）")
		}
	}
	if tail := p.tail.String(); tail != "" {
		parts = append(parts, "stderr 最后几行：\n"+tail)
	}
	return strings.Join(parts, "；")
}

// resolveCommand 在**子进程的** PATH 里查找命令。
//
// exec.Command 只用当前进程的 PATH 查找可执行文件，cmd.Env 里的 PATH 不参与。
// 而 docs/17 3.3 建议桌面端用户在配置的 env 里显式给 PATH（从 Dock 启动的
// Electron 应用 PATH 很短，找不到 npx）——不在这里按配置的 PATH 查一次，
// 那条建议就是无效的。找不到时原样返回，让 exec 给出标准的「not found」错误。
func resolveCommand(name string, env []string) string {
	if strings.ContainsAny(name, `/\`) {
		return name
	}
	pathVal := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			pathVal = v
		}
	}
	if pathVal == "" {
		return name
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ".com", ""}
	}
	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, name+ext)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() &&
				(runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return candidate
			}
		}
	}
	return name
}

// tailBuffer 保留最后 n 行文本（并发安全）。
type tailBuffer struct {
	mu    sync.Mutex
	n     int
	lines []string
}

func newTailBuffer(n int) *tailBuffer { return &tailBuffer{n: n} }

func (t *tailBuffer) add(line string) {
	const maxLineLen = 500
	if len(line) > maxLineLen {
		line = line[:maxLineLen] + "…"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.n {
		t.lines = t.lines[len(t.lines)-t.n:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}
