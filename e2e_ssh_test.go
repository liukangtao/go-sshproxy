// e2e_ssh_test.go 覆盖 SSH 隧道路径：
// 用本地最小 SSH 服务器（密码认证 + direct-tcpip 通道）验证
// "路由缓存命中 SSH → 池中取连接 → 开通道 → 双向转发" 的完整链路。
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// directTCPIPPayload 对应 RFC 4254 direct-tcpip 通道的附加数据
type directTCPIPPayload struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

// startTestSSHServer 启动一个只支持 direct-tcpip 的最小 SSH 服务器
func startTestSSHServer(t *testing.T) (string, int) {
	return startTestSSHServerWithGlobalReply(t, true)
}

// startTestSSHServerWithGlobalReply 启动 SSH 服务器。
// replyGlobal 为 false 时故意不回复 global request，用于模拟
// "TCP 未断但不响应 keepalive" 的僵死连接。
func startTestSSHServerWithGlobalReply(t *testing.T, replyGlobal bool) (string, int) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成主机密钥失败: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("构造签名器失败: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if meta.User() == "test" && string(pass) == "test" {
				return nil, nil
			}
			return nil, fmt.Errorf("认证失败")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 SSH 服务器失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSHConnection(c, cfg, replyGlobal)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func serveSSHConnection(c net.Conn, cfg *ssh.ServerConfig, replyGlobal bool) {
	sconn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sconn.Close()

	if replyGlobal {
		go ssh.DiscardRequests(reqs)
	} else {
		// 消费但不回复：客户端的 SendRequest(wantReply=true) 会一直等待，
		// 从而模拟僵死服务器（TCP 连接仍然存活）
		go func() {
			for range reqs {
			}
		}()
	}

	for newChannel := range chans {
		go serveDirectTCPIP(newChannel)
	}
}

func serveDirectTCPIP(newChannel ssh.NewChannel) {
	if newChannel.ChannelType() != "direct-tcpip" {
		_ = newChannel.Reject(ssh.UnknownChannelType, "仅支持 direct-tcpip")
		return
	}

	var payload directTCPIPPayload
	if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "负载解析失败")
		return
	}

	target := net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port)))
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	ch, chReqs, err := newChannel.Accept()
	if err != nil {
		upstream.Close()
		return
	}
	go ssh.DiscardRequests(chReqs)

	go func() {
		_, _ = io.Copy(ch, upstream)
		_ = ch.CloseWrite()
	}()
	go func() {
		_, _ = io.Copy(upstream, ch)
		upstream.Close()
		_ = ch.Close()
	}()
}

// sshTestProxy 启动一个指向本地 SSH 服务器的代理
func sshTestProxy(t *testing.T, sshHost string, sshPort int) *testProxy {
	t.Helper()
	return startTestProxy(t, func(c *Config) {
		c.SSHHost = sshHost
		c.SSHPort = sshPort
		c.SSHUser = "test"
		c.SSHPasswd = "test"
		c.Insecure = true // 测试中不触碰项目根的真实 proxy.hostkey
		c.SSHPoolSize = 2
	})
}

// TestSSHRouteTunnel 验证缓存命中 SSH 时确实通过 SSH 通道转发数据
func TestSSHRouteTunnel(t *testing.T) {
	sshHost, sshPort := startTestSSHServer(t)
	target := startEchoServer(t)

	p := sshTestProxy(t, sshHost, sshPort)
	// 预置路由缓存，避免并发探测选中直连路径
	p.srv.storeRouteSuccess(target, SSH)

	result := p.srv.selectRoute(target)
	if result.conn == nil {
		t.Fatal("SSH 路由未建立隧道")
	}
	defer func() {
		result.conn.Close()
		if result.sshClient != nil {
			p.srv.sshPool.Put(result.sshClient)
		}
	}()

	if result.method != SSH {
		t.Fatalf("路由方式 = %v, 期望 SSH", result.method)
	}
	if result.sshClient == nil {
		t.Fatal("SSH 路径必须返回底层连接用于归还")
	}

	if _, err := result.conn.Write([]byte("TUNNEL")); err != nil {
		t.Fatalf("隧道写入失败: %v", err)
	}
	echo := make([]byte, 6)
	if _, err := io.ReadFull(result.conn, echo); err != nil {
		t.Fatalf("隧道读取失败: %v", err)
	}
	if string(echo) != "TUNNEL" {
		t.Errorf("隧道回显 = %q, 期望 TUNNEL", echo)
	}
}

// TestPoolKeepaliveTimeoutDropsConnection 验证僵死连接（TCP 存活但不回应 keepalive）
// 在健康检查超时后被关闭：否则 SendRequest 会永久阻塞，每轮检查累积一个滞留协程。
func TestPoolKeepaliveTimeoutDropsConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等待 keepalive 超时")
	}

	host, port := startTestSSHServerWithGlobalReply(t, false)

	cfg := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.Password("test")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	pool := NewSSHConnectionPool(cfg, host, strconv.Itoa(port), 2)
	defer pool.closeAll()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := pool.Get(ctx)
	if err != nil {
		t.Fatalf("建立 SSH 连接失败: %v", err)
	}
	pool.Put(client)

	start := time.Now()
	pool.checkAllConnectionsHealth()
	elapsed := time.Since(start)

	if elapsed < keepaliveTimeout {
		t.Fatalf("健康检查耗时 %v，未达到 keepalive 超时阈值 %v", elapsed, keepaliveTimeout)
	}

	pool.lastUsedMutex.Lock()
	remaining := len(pool.lastUsed)
	pool.lastUsedMutex.Unlock()
	if remaining != 0 {
		t.Errorf("僵死连接未被移除，lastUsed 仍有 %d 条记录（探测协程会持续滞留）", remaining)
	}
}

// TestSocks5OverSSHTunnel 验证 SOCKS5 CONNECT 经 SSH 隧道端到端可用
func TestSocks5OverSSHTunnel(t *testing.T) {
	sshHost, sshPort := startTestSSHServer(t)
	target := startEchoServer(t)
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	p := sshTestProxy(t, sshHost, sshPort)
	p.srv.storeRouteSuccess(target, SSH)

	conn, br := socksHandshake(t, p.socks5)
	if rep := socksConnect(t, conn, br, host, port); rep != repSucceeded {
		t.Fatalf("CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}

	if _, err := conn.Write([]byte("VIA-SSH")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	echo := make([]byte, 7)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(echo) != "VIA-SSH" {
		t.Errorf("回显 = %q, 期望 VIA-SSH", echo)
	}
}

// TestHTTPConnectOverSSHTunnel 验证 HTTP CONNECT 经 SSH 隧道端到端可用
func TestHTTPConnectOverSSHTunnel(t *testing.T) {
	sshHost, sshPort := startTestSSHServer(t)
	target := startEchoServer(t)

	p := sshTestProxy(t, sshHost, sshPort)
	p.srv.storeRouteSuccess(target, SSH)

	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	status := readHTTPHead(t, br)
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT 应回 200，实际: %q", status)
	}

	if _, err := conn.Write([]byte("HTTP-SSH")); err != nil {
		t.Fatalf("隧道写入失败: %v", err)
	}
	echo := make([]byte, 8)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("隧道读取失败: %v", err)
	}
	if string(echo) != "HTTP-SSH" {
		t.Errorf("隧道回显 = %q, 期望 HTTP-SSH", echo)
	}
}
