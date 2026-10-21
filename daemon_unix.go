//go:build !windows
// +build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setupDaemonSysProcAttr Unix/Linux 下设置守护进程属性
func setupDaemonSysProcAttr(cmd *exec.Cmd) {
	// Unix 下设置 Setsid，使子进程脱离控制终端成为会话组长
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
