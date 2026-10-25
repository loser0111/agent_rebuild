//go:build windows

package tool

import "os/exec"

// configureProcessGroup 在 Windows 上使用默认行为。
//
// Windows 没有 POSIX 进程组语义；通过 exec.CommandContext 的默认取消
// 已经能终止直接子进程。孙进程的清理依赖 taskkill（如需可后续增强）。
func configureProcessGroup(cmd *exec.Cmd) {}
