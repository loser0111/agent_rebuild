//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup 让命令在独立进程组中运行，
// 这样超时/取消时可以杀掉整组（含 sh -c 派生的孙进程）。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负 PID = 杀整个进程组。
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
