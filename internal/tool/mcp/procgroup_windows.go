//go:build windows

package mcp

import "os/exec"

// Windows 没有 POSIX 进程组语义，退化为只结束直接子进程
// （与 internal/tool/procgroup_windows.go 的取舍一致；孙进程清理留待 Job Object）。
func setProcessGroup(cmd *exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killGroup(cmd *exec.Cmd) { terminateGroup(cmd) }
