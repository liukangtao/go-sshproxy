//go:build windows
// +build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setupDaemonSysProcAttr Windows 下设置守护进程属性
func setupDaemonSysProcAttr(cmd *exec.Cmd) {
	// CREATE_NEW_PROCESS_GROUP：创建新进程组，父进程退出后子进程继续运行，
	//   且不受父进程控制台 Ctrl+C（CTRL_C_EVENT）影响；
	// DETACHED_PROCESS：子进程不继承也不创建控制台。这样双击启动时：
	//   父进程退出后控制台窗口立即关闭（不再残留黑窗口），
	//   关闭任何窗口都不会向后台服务发送 CTRL_CLOSE_EVENT（不会误停服务）
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
