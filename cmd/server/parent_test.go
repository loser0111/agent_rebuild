package main

import (
	"testing"
)

func TestParentPIDFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"未提供", []string{"agent-server"}, 0},
		{"正常", []string{"agent-server", "--parent-pid=1234"}, 1234},
		{"空值", []string{"agent-server", "--parent-pid="}, 0},
		{"非法", []string{"agent-server", "--parent-pid=abc"}, 0},
		{"负数", []string{"agent-server", "--parent-pid=-5"}, 0},
		{"零", []string{"agent-server", "--parent-pid=0"}, 0},
		{"其他参数混排", []string{"-v", "--parent-pid=42", "--other"}, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parentPIDFromArgs(c.args); got != c.want {
				t.Fatalf("parentPIDFromArgs(%v) = %d，期望 %d", c.args, got, c.want)
			}
		})
	}
}

func TestProcessAlive(t *testing.T) {
	// 当前进程必然存活。
	if !processAlive(1) && !processAlive(getOwnPID(t)) {
		t.Skip("无法可靠探测进程存活（平台差异）")
	}
	// 极端 PID 基本不可能存在。
	if processAlive(99999999) {
		t.Fatal("不存在的 PID 应被判定为不存活")
	}
}

func getOwnPID(t *testing.T) int {
	t.Helper()
	return ownPID()
}
