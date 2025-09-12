// main.go 主程序包，实现一个支持SOCKS5和HTTP/HTTPS的智能代理服务器
// 这是整个代理服务器的入口点，负责初始化配置、启动服务和处理系统信号
package main

import (
	"context"   // 用于控制goroutine的上下文，实现优雅关闭
	"log"       // 用于记录系统日志
	"os"        // 用于访问操作系统功能，如进程管理
	"os/exec"   // 用于执行外部命令，实现守护进程功能
	"os/signal" // 用于处理系统信号，如中断信号
)

// main 主函数，程序的入口点
// 负责初始化配置、启动代理服务、处理系统信号和优雅关闭
func main() {

	// 如果指定了前台运行模式，则不启动守护进程
	// 这样可以确保程序在前台运行，便于调试和日志查看
	if !foreground {
		// 检查是否已经有守护进程在运行，防止重复启动
		cmd := exec.Command(os.Args[0], append(os.Args[1:], "-foreground=true")...)
		if err := cmd.Start(); err != nil {
			log.Fatalf("启动守护进程失败: %v", err)
		}
		log.Printf("守护进程已启动，PID: %d", cmd.Process.Pid)
		return
	}

	// 检查是否至少指定了一个代理类型
	// 用户必须指定SOCKS5端口或HTTP端口中的至少一个
	if socks5Port == 0 && httpPort == 0 {
		log.Fatal("必须指定至少一个代理类型（--socks5 或 --http）")
	}

	// 启动目标信息检查协程，定期清理过期缓存
	// 这个goroutine会定期检查并清理路由缓存中的过期条目
	go CheckTargetInfo()

	// 创建SSH配置，用于连接远程SSH服务器
	// SSH配置包含了认证信息和连接参数
	config := createSSHConfig()

	// 初始化SSH连接池，用于复用SSH连接
	// 连接池可以避免频繁创建和销毁SSH连接，提高性能
	sshPool = NewSSHConnectionPool(config, sshHost, sshPort, 50) // 最大连接数50

	// 创建带取消功能的上下文，用于控制代理服务的生命周期
	// 当需要关闭服务时，可以通过取消此上下文来通知所有goroutine
	tctx, fcancel := context.WithCancel(context.Background())
	defer fcancel() // 函数退出时取消上下文

	// 启动SOCKS5代理服务
	// 如果用户指定了SOCKS5端口，则启动SOCKS5代理服务
	if socks5Port > 0 {
		go startSocks5Proxy(tctx, socks5Port, sshPool)
	}
	// 启动HTTP代理服务
	// 如果用户指定了HTTP端口，则启动HTTP代理服务
	if httpPort > 0 {
		go startHttpProxy(tctx, httpPort, sshPool)
	}

	// 创建信号通道，用于接收系统信号
	// 监听中断信号（如Ctrl+C）和终止信号
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, os.Kill)

	// 等待系统信号
	// 阻塞直到收到中断或终止信号
	<-signalChan
	log.Println("收到终止信号，正在关闭服务...")

	// 取消上下文，通知所有goroutine优雅关闭
	fcancel()
}
