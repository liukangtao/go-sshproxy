// e2e_extra_test.go 补充端到端边界场景：
// HEAD/204 语义、TCP 半关闭、二进制透传、UDP IPv6、握手超时与过载拒绝。
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHTTPProxyHEAD 验证 HEAD 请求不携带响应体，但保留 Content-Length
func TestHTTPProxyHEAD(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("hello world"))
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	resp, err := client.Head(backend.URL + "/")
	if err != nil {
		t.Fatalf("HEAD 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 HEAD 响应体失败: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("HEAD 响应体应为空，实际 %d 字节", len(body))
	}
	if resp.ContentLength != 11 {
		t.Errorf("Content-Length = %d, 期望 11", resp.ContentLength)
	}
}

// TestHTTPProxyNoContent 验证 204 无响应体时客户端不会挂起
func TestHTTPProxyNoContent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	resp, err := client.Get(backend.URL + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("状态码 = %d, 期望 204", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("204 响应体应为空，实际 %q", body)
	}
}

// TestHTTPProxyGzipBinaryPassthrough 验证压缩/二进制响应体逐字节透传
func TestHTTPProxyGzipBinaryPassthrough(t *testing.T) {
	raw := bytes.Repeat([]byte{0x00, 0x01, 0xFF, 0xFE, 0x7F, 0x80}, 4096) // 24 KiB 二进制
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := buf.Bytes()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	fmt.Fprintf(conn, "GET %s/blob HTTP/1.1\r\nHost: %s\r\nAccept-Encoding: identity\r\n\r\n", backend.URL, u.Host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, 期望 gzip", resp.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(body, compressed) {
		t.Errorf("压缩响应体被改写：收到 %d 字节，期望 %d 字节", len(body), len(compressed))
	}
}

// TestSocks5HalfClose 验证客户端关写端后，仍能收到目标返回的数据（TCP 半关闭语义）
func TestSocks5HalfClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动上游失败: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c) // 读到客户端 EOF
		_, _ = c.Write([]byte("BYE"))
	}()

	addr := ln.Addr().(*net.TCPAddr)
	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	if rep := socksConnect(t, conn, br, "127.0.0.1", addr.Port); rep != repSucceeded {
		t.Fatalf("CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}
	if _, err := conn.Write([]byte("REQUEST")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatal("连接不是 *net.TCPConn，无法执行半关闭")
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("关写端失败: %v", err)
	}

	buf := make([]byte, 3)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("关写端后未能收到目标数据: %v", err)
	}
	if string(buf) != "BYE" {
		t.Errorf("收到 %q, 期望 BYE", buf)
	}
}

// TestHTTPProxyCONNECTUpstreamRefused 验证 CONNECT 目标不可达时回 502 而不是直接断链
func TestHTTPProxyCONNECTUpstreamRefused(t *testing.T) {
	deadPort := freeTCPPort(t)
	p := startTestProxy(t, nil)

	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(conn)

	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(deadPort))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	status := readHTTPHead(t, br)
	if !strings.Contains(status, "502") {
		t.Errorf("目标不可达应回 502，实际: %q", status)
	}
}

// TestSocks5UDPAssociateIPv6 验证 UDP 中继能向 IPv6 目标发送报文
func TestSocks5UDPAssociateIPv6(t *testing.T) {
	pc, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Skipf("本机不支持 IPv6 UDP: %v", err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	echoAddr := pc.LocalAddr().(*net.UDPAddr)

	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	req := append([]byte{0x05, cmdUDPAssociate, 0x00}, encodeSocksAddr("0.0.0.0", 0)...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("发送 UDP ASSOCIATE 失败: %v", err)
	}
	rep, bndHost, bndPort := readSocksReply(t, br)
	if rep != repSucceeded {
		t.Fatalf("UDP ASSOCIATE 应答 = 0x%02x, 期望 0x00", rep)
	}

	udpConn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(bndHost), Port: bndPort})
	if err != nil {
		t.Fatalf("连接 UDP 中继失败: %v", err)
	}
	defer udpConn.Close()
	_ = udpConn.SetDeadline(time.Now().Add(3 * time.Second))

	pkt := append([]byte{0x00, 0x00, 0x00}, encodeSocksAddr("::1", echoAddr.Port)...)
	pkt = append(pkt, []byte("V6UDP")...)
	if _, err := udpConn.Write(pkt); err != nil {
		t.Fatalf("发送 UDP 报文失败: %v", err)
	}

	buf := make([]byte, 65535)
	n, err := udpConn.Read(buf)
	if err != nil {
		t.Fatalf("IPv6 UDP 目标无回包（出口 socket 可能只支持 IPv4）: %v", err)
	}
	_, _, payload, perr := parseUDPDatagram(buf[:n])
	if perr != nil {
		t.Fatalf("解析回包失败: %v", perr)
	}
	if string(payload) != "V6UDP" {
		t.Errorf("回包负载 = %q, 期望 V6UDP", payload)
	}
}

// TestSOCKS5HandshakeTimeout 验证客户端连上后不发送数据会被握手超时断开
func TestSOCKS5HandshakeTimeout(t *testing.T) {
	p := startTestProxy(t, func(c *Config) { c.HandshakeTimeout = 1 })

	conn, err := net.DialTimeout("tcp", p.socks5, 5*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))

	start := time.Now()
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	elapsed := time.Since(start)

	if rerr == nil {
		t.Fatal("静默连接不应收到数据")
	}
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatalf("静默连接未被握手超时断开（等待 %v 后仍处于打开状态）", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("握手超时断开耗时 %v，超过配置的 1 秒过多", elapsed)
	}
}

// TestHTTPOverloadRejected 验证并发名额被占满且排队超时后回 503（快速失败）
func TestHTTPOverloadRejected(t *testing.T) {
	p := startTestProxy(t, func(c *Config) { c.MaxConcurrentConnections = 1 })

	// 占满唯一的名额（模拟握手缓慢的连接）
	p.srv.limiter <- struct{}{}
	defer func() { <-p.srv.limiter }()

	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(maxQueueWait + 5*time.Second))

	fmt.Fprintf(conn, "GET http://127.0.0.1:1/ HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")

	start := time.Now()
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if !strings.Contains(status, "503") {
		t.Errorf("过载时应回 503，实际: %q", strings.TrimSpace(status))
	}
	if elapsed > maxQueueWait+2*time.Second {
		t.Errorf("过载拒绝耗时 %v，超出排队上限", elapsed)
	}
}

// TestHTTPProxyHTTP10Client 验证 HTTP/1.0 客户端（默认短连接）在响应后被断开
func TestHTTPProxyHTTP10Client(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("legacy"))
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	// HTTP/1.0 请求默认短连接（无 Connection 头）
	fmt.Fprintf(conn, "GET %s/ HTTP/1.0\r\n\r\n", backend.URL)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "legacy" {
		t.Errorf("响应体 = %q, 期望 legacy", body)
	}

	// 响应结束后代理应关闭连接，客户端读到 EOF
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Error("HTTP/1.0 请求处理完毕后连接仍保持打开")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("HTTP/1.0 请求处理完毕后连接未关闭（客户端需等到超时）")
	}
}

// TestHTTPProxyClientConnectionClose 验证客户端要求关闭时，响应后连接被断开
func TestHTTPProxyClientConnectionClose(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("bye"))
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", backend.URL, u.Host)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "bye" {
		t.Errorf("响应体 = %q, 期望 bye", body)
	}
	if !resp.Close {
		t.Error("响应未声明 Connection: close")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Error("客户端要求关闭后连接仍保持打开")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("客户端要求关闭后连接未断开（等待到超时）")
	}
}

// TestHTTPProxyLargeResponseBody 验证大响应体（8 MiB）逐字节完整下发
func TestHTTPProxyLargeResponseBody(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 1<<20) // 8 MiB
	want := sha256.Sum256(payload)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	resp, err := client.Get(backend.URL + "/big")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取大响应失败: %v", err)
	}
	if len(body) != len(payload) {
		t.Fatalf("响应长度 = %d, 期望 %d", len(body), len(payload))
	}
	if got := sha256.Sum256(body); got != want {
		t.Error("大响应体内容校验和不匹配（数据被截断或改写）")
	}
}

// TestSocks5ByteByByteHandshake 验证逐字节送达的握手与请求能被正确解析（TCP 短读）
func TestSocks5ByteByByteHandshake(t *testing.T) {
	target := startEchoServer(t)
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.socks5, 5*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	stream := []byte{0x05, 0x01, 0x00}
	stream = append(stream, 0x05, cmdConnect, 0x00)
	stream = append(stream, encodeSocksAddr(host, port)...)
	stream = append(stream, []byte("BYTE")...)

	for _, b := range stream {
		if _, err := conn.Write([]byte{b}); err != nil {
			t.Fatalf("逐字节写入失败: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	br := bufio.NewReader(conn)
	handshake := make([]byte, 2)
	if _, err := io.ReadFull(br, handshake); err != nil {
		t.Fatalf("读取握手应答失败（可能未按固定长度读取）: %v", err)
	}
	rep, _, _ := readSocksReply(t, br)
	if rep != repSucceeded {
		t.Fatalf("CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}

	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(echo) != "BYTE" {
		t.Errorf("回显 = %q, 期望 BYTE", echo)
	}
}

// TestShutdownWithInflightConnection 验证存在长连接时 Shutdown 仍能在宽限期内返回
func TestShutdownWithInflightConnection(t *testing.T) {
	target := startEchoServer(t)
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)
	if rep := socksConnect(t, conn, br, host, port); rep != repSucceeded {
		t.Fatalf("CONNECT 应答 = 0x%02x", rep)
	}
	if _, err := conn.Write([]byte("Z")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(br, one); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}

	start := time.Now()
	p.srv.Shutdown(500 * time.Millisecond)
	elapsed := time.Since(start)

	conn.Close()
	if elapsed > 3*time.Second {
		t.Errorf("Shutdown 耗时 %v，未在宽限期后及时返回", elapsed)
	}
}

// TestHTTPProxyWebSocketUpgrade 验证 Upgrade 握手透传与 101 之后的双向数据转发
func TestHTTPProxyWebSocketUpgrade(t *testing.T) {
	const (
		wsKey        = "dGhlIHNhbXBsZSBub25jZQ=="
		wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
		wsAcceptWant = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" // RFC 6455 示例值
	)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动上游失败: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}

		if req.Header.Get("Upgrade") != "websocket" ||
			!strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") {
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
			return
		}

		sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + wsGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)

		// 协议切换后原样回显应用数据
		_, _ = io.Copy(c, br)
	}()

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	upstream := ln.Addr().String()
	fmt.Fprintf(conn, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n",
		upstream, upstream, wsKey)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("读取 101 响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("状态码 = %d, 期望 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		t.Errorf("Upgrade = %q, 期望 websocket（升级头未透传）", got)
	}
	if got := resp.Header.Get("Connection"); !strings.Contains(strings.ToLower(got), "upgrade") {
		t.Errorf("Connection = %q, 期望包含 upgrade", got)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wsAcceptWant {
		t.Errorf("Sec-WebSocket-Accept = %q, 期望 %q", got, wsAcceptWant)
	}

	if _, err := conn.Write([]byte("WS-DATA")); err != nil {
		t.Fatalf("协议切换后写入失败: %v", err)
	}
	echo := make([]byte, 7)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("协议切换后读取失败: %v", err)
	}
	if string(echo) != "WS-DATA" {
		t.Errorf("回显 = %q, 期望 WS-DATA", echo)
	}
}

// TestHTTPProxyUpgradeRejected 验证上游拒绝升级时，代理按普通响应回写并关闭连接
func TestHTTPProxyUpgradeRejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动上游失败: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 7\r\n\r\nrefused"))
	}()

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	upstream := ln.Addr().String()
	fmt.Fprintf(conn, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		upstream, upstream)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	if string(body) != "refused" {
		t.Errorf("响应体 = %q, 期望 refused", body)
	}
}
