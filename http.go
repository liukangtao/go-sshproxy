// http.go 实现HTTP/HTTPS协议处理功能
// 负责处理HTTP和HTTPS代理请求，包括CONNECT隧道和普通HTTP请求
package main

import (
	"bufio"    // 用于实现带缓冲的I/O操作
	"io"       // 用于基本的I/O操作
	"log"      // 用于记录日志
	"net"      // 用于网络I/O操作
	"net/http" // 用于HTTP客户端和服务端实现
	"time"     // 用于时间处理
)

// handleHTTPProxyWithPool HTTP代理处理函数（使用连接池）
// 处理单个HTTP/HTTPS代理请求，完成路由选择和数据转发
func handleHTTPProxyWithPool(conn net.Conn, sshPool *SSHConnectionPool) {
	// 限制最大并发连接数
	// 通过信号量控制并发连接数，防止资源耗尽
	connectionLimiter <- struct{}{}
	defer func() { <-connectionLimiter }()

	// 设置连接超时
	// 防止连接长时间占用资源
	conn.SetDeadline(time.Now().Add(60 * time.Second))

	// 函数退出时关闭客户端连接
	// 确保连接在函数结束时被正确关闭
	defer conn.Close()

	// 读取HTTP请求
	// 使用带缓冲的读取器解析HTTP请求
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("请求解析失败: %v", err)
		return
	}

	// 获取目标地址
	// 从HTTP请求中提取目标地址
	targetAddr := req.URL.Host
	
	// 如果端口未指定，根据请求类型设置默认端口
	if req.URL.Port() == "" {
		// 对于HTTPS隧道请求(CONNECT方法)使用443端口
		if req.Method == "CONNECT" {
			targetAddr += ":443"
		} else {
			// 对于普通HTTP请求使用80端口
			targetAddr += ":80"
		}
	}

	// 选择路由
	// 根据目标地址选择最佳路由方式（SSH隧道或直连）
	result, _ := selectRoute(targetAddr, sshPool)
	if result.conn == nil {
		// 路由选择失败，直接返回
		return
	}

	// 函数退出时关闭目标连接
	// 确保目标连接在函数结束时被正确关闭
	defer result.conn.Close()

	// 根据请求类型处理连接
	if req.Method == "CONNECT" {
		// HTTPS隧道模式：发送连接建立成功响应
		// CONNECT方法用于建立HTTPS隧道
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	} else {
		// 普通HTTP请求：修正请求并转发
		// 设置正确的协议方案
		if req.URL.Port() == "443" {
			req.URL.Scheme = "https"
		} else {
			req.URL.Scheme = "http"
		}
		
		// 设置目标主机和清除请求URI
		req.URL.Host = targetAddr
		req.RequestURI = ""

		// 移除代理相关头部
		// 清理代理相关的HTTP头部字段
		req.Header.Del("Proxy-Connection")
		req.Header.Set("Connection", "Keep-Alive")

		// 发送修正后的请求到目标服务器
		err = req.Write(result.conn)
		if err != nil {
			log.Printf("发送请求失败: %v", err)
			return
		}
	}

	// 记录路由结果日志
	// 异步记录路由选择结果
	go logRouteResult(targetAddr, result.method.String(), result.latency)

	// 数据双向转发
	// 启动goroutine处理从客户端到目标的数据转发
	go func() {
		defer result.conn.Close()
		io.Copy(result.conn, conn) // 从客户端到目标
	}()
	// 处理从目标到客户端的数据转发
	io.Copy(conn, result.conn) // 从目标到客户端
}