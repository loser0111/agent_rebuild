//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让 server 在独立进程组中运行。
//
// 两个目的：
//   - 关闭时能杀掉整组：`npx -y pkg` 实际跑的是 npx → node 两层，只杀直接
//     子进程会留下孤儿 node 进程（与 internal/tool 的 run_command 同一个理由）；
//   - 终端里按 Ctrl-C 时，SIGINT 不会同时直达 server：由 Go 进程按
//     「先撤工具、再关进程」的顺序收尾，而不是 server 先死、正在飞的调用全部报错。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
