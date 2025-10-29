// socks5.go 实现SOCKS5协议处理功能
// 负责处理SOCKS5代理协议的握手、地址解析和数据转发
package main

import (
	"errors" // 用于处理错误
	"fmt"    // 用于格式化输入输出
	"io"     // 用于基本的I/O操作
	"log"    // 用于记录日志
	"net"    // 用于网络I/O操作
)

// handleConnection 处理SOCKS5连接
// 处理单个SOCKS5客户端连接，完成握手、地址解析、路由选择和数据转发
func handleConnection(conn net.Conn, sshPool *SSHConnectionPool) {
	// 函数退出时关闭客户端连接
	// 确保连接在函数结束时被正确关闭
	defer conn.Close()

	// 进行SOCKS5握手
	// 与客户端完成SOCKS5协议握手过程
	if err := socks5Handshake(conn); err != nil {
		log.Printf("SOCKS5握手失败: %v", err)
		return
	}

	// 获取目标地址
	// 从客户端请求中解析出目标地址
	targetAddr, err := getTargetAddress(conn)
	if err != nil {
		log.Printf("获取目标地址失败: %v", err)
		return
	}

	// 选择路由
	// 根据目标地址选择最佳路由方式（SSH隧道或直连）
	result, needUpdate := selectRoute(targetAddr, sshPool)
	if result.conn == nil && needUpdate {
		// 路由选择失败，发送连接失败响应
		_ = sendSocks5Response(conn, 0x01)
		return
	}
	defer result.conn.Close()

	// 发送SOCKS5连接成功响应
	// 通知客户端连接已建立
	if err := sendSocks5Response(conn, 0x00); err != nil {
		log.Printf("发送SOCKS5响应失败: %v", err)
		return
	}

	// 记录路由结果日志
	// 异步记录路由选择结果
	go logRouteResult(targetAddr, result.method.String(), result.latency)

	// 数据双向转发
	// 启动goroutine处理从客户端到目标的数据转发
	go func() {
		io.Copy(result.conn, conn) // 从客户端到目标
	}()
	// 处理从目标到客户端的数据转发
	io.Copy(conn, result.conn) // 从目标到客户端
}

// socks5Handshake SOCKS5握手过程
// 与客户端完成SOCKS5协议握手，协商认证方式
func socks5Handshake(conn net.Conn) error {
	// 创建缓冲区
	// 用于读取客户端发送的数据
	buf := make([]byte, 256)

	// 读取版本和方法列表
	// 读取SOCKS协议版本和认证方法数量
	n, err := conn.Read(buf[:2])
	if err != nil || n != 2 {
		return errors.New("failed to read methods")
	}

	// SOCKS版本
	// 检查是否为SOCKS5版本
	version := buf[0]
	// 支持的方法数量
	nMethods := buf[1]

	if version != 0x05 {
		return errors.New("unsupported SOCKS version")
	}

	// 读取所有支持的方法
	// 读取客户端支持的认证方法列表
	n, err = conn.Read(buf[:nMethods])
	if err != nil || n != int(nMethods) {
		return errors.New("failed to read methods list")
	}

	// 选择无认证方式(0x00)
	// 选择不需要认证的方式回复客户端
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return errors.New("failed to write auth method")
	}

	return nil
}

// getTargetAddress 获取目标地址
// 从SOCKS5请求中解析出目标地址和端口
func getTargetAddress(conn net.Conn) (string, error) {
	// 创建缓冲区
	// 用于读取客户端发送的目标地址信息
	buf := make([]byte, 256)

	// 读取请求头(版本、命令、保留字段、地址类型)
	// 读取SOCKS5请求的基本信息
	n, err := conn.Read(buf[:4])
	if err != nil || n != 4 {
		return "", errors.New("failed to read request header")
	}

	// SOCKS版本
	version := buf[0]
	// 命令类型
	cmd := buf[1]
	// 地址类型
	atyp := buf[3]

	if version != 0x05 || cmd != 0x01 {
		return "", errors.New("unsupported command")
	}

	// 目标主机地址
	var host string
	// 根据地址类型解析目标地址
	switch atyp {
	case 0x01: // IPv4地址
		n, err = conn.Read(buf[:4])
		if err != nil || n != 4 {
			return "", errors.New("failed to read IPv4")
		}
		host = net.IP(buf[:4]).String()
	case 0x03: // 域名
		n, err = conn.Read(buf[:1])
		if err != nil || n != 1 {
			return "", errors.New("failed to read domain length")
		}
		domainLen := buf[0]
		n, err = conn.Read(buf[:domainLen])
		if err != nil || n != int(domainLen) {
			return "", errors.New("failed to read domain")
		}
		host = string(buf[:domainLen])
	case 0x04: // IPv6地址
		n, err = conn.Read(buf[:16])
		if err != nil || n != 16 {
			return "", errors.New("failed to read IPv6")
		}
		host = net.IP(buf[:16]).String()
	default:
		return "", errors.New("unsupported address type")
	}

	// 读取端口(2字节)
	n, err = conn.Read(buf[:2])
	if err != nil || n != 2 {
		return "", errors.New("failed to read port")
	}
	// 端口转换为整数
	port := int(buf[0])<<8 | int(buf[1])

	// 返回完整地址
	return fmt.Sprintf("%s:%d", host, port), nil
}

// sendSocks5Response 发送SOCKS5响应
// 向客户端发送SOCKS5响应，通知连接建立结果
func sendSocks5Response(conn net.Conn, reply byte) error {
	// 构造响应包：版本(0x05)、回复(0x00成功/0x01失败)、保留(0x00)、绑定地址(0x01 IPv4)、IP(0.0.0.0)、端口(0)
	response := []byte{0x05, reply, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, err := conn.Write(response)
	return err
}
