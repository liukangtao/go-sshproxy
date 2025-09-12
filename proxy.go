// proxy.go 实现代理服务核心功能
// 负责启动和管理SOCKS5和HTTP代理服务
package main

import (
	"context"  // 用于控制goroutine的上下文
	"fmt"      // 用于格式化输入输出
	"log"      // 用于记录日志
	"net"      // 用于网络I/O操作
	"time"     // 用于时间处理

	"golang.org/x/crypto/ssh" // 用于SSH客户端连接
)

// startSocks5Proxy 启动SOCKS5代理服务
// 监听指定端口并处理SOCKS5代理请求
func startSocks5Proxy(ctx context.Context, port int, pool *SSHConnectionPool) {
	// 监听指定端口
	// 创建TCP监听器，监听指定端口的连接
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", port, err)
	}
	// 函数退出时关闭监听器
	// 确保监听器在函数结束时被正确关闭
	defer listener.Close()

	log.Printf("SOCKS5 proxy started on :%d", port)

	// 持续接受客户端连接
	// 循环接受客户端连接并处理
	for {
		// 检查上下文是否取消
		// 通过上下文控制服务的生命周期
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 接受客户端连接
		// 等待并接受新的客户端连接
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept failed: %v", err)
			continue
		}

		// 再次检查上下文是否取消
		// 确保在处理连接前服务未被取消
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 为每个连接启动一个协程处理
		// 使用goroutine并发处理多个客户端连接
		go handleConnection(conn, pool)
	}
}

// startHttpProxy 启动HTTP代理服务
// 监听指定端口并处理HTTP/HTTPS代理请求
func startHttpProxy(ctx context.Context, port int, pool *SSHConnectionPool) {
	// 监听指定端口
	// 创建TCP监听器，监听指定端口的连接
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("HTTP代理启动失败: %v", err)
	}
	// 函数退出时关闭监听器
	// 确保监听器在函数结束时被正确关闭
	defer listener.Close()
	log.Printf("HTTP/HTTPS代理已启动 :%d", port)

	// 持续接受客户端连接
	// 循环接受客户端连接并处理
	for {
		// 检查上下文是否取消
		// 通过上下文控制服务的生命周期
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 接受客户端连接
		// 等待并接受新的客户端连接
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("HTTP连接接受失败: %v", err)
			continue
		}

		// 再次检查上下文是否取消
		// 确保在处理连接前服务未被取消
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 为每个连接启动一个协程处理
		// 使用goroutine并发处理多个客户端连接
		go handleHTTPProxyWithPool(conn, pool)
	}
}

// createSSHConfig 创建SSH客户端配置
// 根据用户配置创建SSH客户端配置信息
func createSSHConfig() *ssh.ClientConfig {
	// 配置SSH客户端
	config := &ssh.ClientConfig{
		User: sshUser, // SSH用户名
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			// 记录主机密钥信息，但不进行严格验证（在生产环境中应该进行完整验证）
			log.Printf("SSH主机: %s, 密钥类型: %s", hostname, key.Type())
			return nil
		},
		Timeout: 30 * time.Second, // 设置连接超时
	}

	// 使用密码认证方式
	config.Auth = []ssh.AuthMethod{ssh.Password(sshPasswd)}
	
	return config
}