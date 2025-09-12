// config.go 实现代理服务器的配置管理功能
// 负责定义和初始化所有命令行参数，处理用户配置
package main

import (
	"flag" // 用于解析命令行参数
	"fmt"  // 用于格式化输入输出
	"os"   // 用于访问操作系统功能
)

// 全局配置变量定义
// 这些变量存储用户通过命令行传入的配置参数
var (
	// sshUser SSH用户名，用于连接远程SSH服务器
	sshUser string
	// sshPasswd SSH密码，用于认证SSH连接
	sshPasswd string
	// sshHost SSH主机地址，远程SSH服务器的IP或域名
	sshHost string
	// sshPort SSH端口，远程SSH服务器的端口号
	sshPort string
	// socks5Port SOCKS5代理监听端口，本地SOCKS5代理服务监听的端口
	socks5Port int
	// httpPort HTTP代理监听端口，本地HTTP代理服务监听的端口
	httpPort int
	// WriteLogToFile 是否将日志写入文件，控制路由日志的输出方式
	WriteLogToFile bool
	// foreground 是否在前台运行，如果为true则不启动守护进程
	foreground bool
	// maxConcurrentConnections 最大并发连接数，限制同时处理的连接数量
	maxConcurrentConnections = 1000
	// maxCacheSize 最大缓存条目数，限制路由缓存的最大大小
	maxCacheSize = 10000
)

// init 初始化函数，设置命令行参数
// 在程序启动时自动调用，用于定义和初始化所有命令行参数
func init() {
	// 自定义命令行参数使用说明
	// 当用户运行程序时显示帮助信息
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s \" --sshUser SSH用户 --sshHost SSH主机 --port ssh端口 --socks5 端口 --http 端口 -log <true|false>\")\n", os.Args[0])
		flag.PrintDefaults()
	}
	// 定义各种命令行参数及其默认值和说明
	// 用户可以通过命令行参数覆盖这些默认值
	flag.StringVar(&sshUser, "sshUser", "root", "SSH用户名")
	flag.StringVar(&sshHost, "sshHost", "", "SSH主机")
	flag.StringVar(&sshPort, "port", "22", "SSH端口")
	flag.IntVar(&socks5Port, "socks5", 6111, "SOCKS5代理监听端口")
	flag.IntVar(&httpPort, "http", 8880, "HTTP代理监听端口")
	flag.BoolVar(&WriteLogToFile, "log", false, "是否将日志写入文件")
	flag.StringVar(&sshPasswd, "sshPasswd", "", "SSH密码")
	flag.BoolVar(&foreground, "foreground", false, "是否在前台运行")
	flag.Parse()
}
