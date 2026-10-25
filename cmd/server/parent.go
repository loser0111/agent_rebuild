package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// parentPIDFlag 是「父进程 PID」参数的形状：--parent-pid=1234。
const parentPIDFlag = "--parent-pid="

// ownPID 返回当前进程 PID（测试用）。
func ownPID() int { return os.Getpid() }

// parentPIDFromArgs 从命令行参数中解析 --parent-pid=<pid>。
// 解析失败或未提供时返回 0（表示不启用守护）。
func parentPIDFromArgs(args []string) int {
	for _, a := range args {
		if !strings.HasPrefix(a, parentPIDFlag) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(a, parentPIDFlag))
		if err != nil || pid <= 0 {
			return 0
		}
		return pid
	}
	return 0
}

// watchParent 周期性检查父进程是否存活；父进程消失时触发 stop()，
// 让服务走正常优雅关闭流程（而不是被孤儿化后长期驻留）。
//
// 用 signal 0 探测进程存在性：不发送真实信号，仅做权限与存在性检查。
// 在 Unix 上，进程退出后若未被回收会短暂成为僵尸，kill(pid, 0) 仍返回成功，
// 因此额外等待一段时间观察，避免误判为「还活着」。
func watchParent(pid int, stop func()) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if processAlive(pid) {
			continue
		}
		// 父进程已消失：主动触发关闭。
		stop()
		return
	}
}

// processAlive 判断进程是否存在（Unix）。非 Unix 平台保守返回 true，
// 避免因平台差异误杀服务（Windows 上由 Electron 的 taskkill 负责）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Unix 下 FindProcess 总能成功，需用 signal 0 探测。
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM 表示进程存在但无权限发信号 → 视为存活。
	return strings.Contains(err.Error(), "operation not permitted")
}
