// e2e_test.go 通过真实监听端口驱动代理，覆盖 HTTP / SOCKS5 / UDP 的端到端行为。
// 说明：测试中的 SSH 目标固定为 127.0.0.1:1（必然被拒绝），
// 因此 selectRoute 的 SSH 分支快速失败、Direct 分支成为实际路径，
// 无需依赖真实 SSH 服务器即可验证转发与协议处理逻辑。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试基础设施 ----------

type testProxy struct {
	srv      *ProxyServer
	socks5   string
	httpAddr string
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("预留端口失败: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTestProxy(t *testing.T, mutate func(*Config)) *testProxy {
	t.Helper()

	socksPort := freeTCPPort(t)
	httpPort := freeTCPPort(t)
	for httpPort == socksPort {
		httpPort = freeTCPPort(t)
	}

	cfg := defaultConfig()
	cfg.ListenAddr = "127.0.0.1"
	cfg.SSHHost = "127.0.0.1"
	cfg.SSHPort = 1 // 必然拒绝：让 SSH 路由快速失败
	cfg.SSHUser = "test"
	cfg.SSHPasswd = "test"
	cfg.SSHTimeout = 2
	cfg.SSHPoolSize = 1
	cfg.HandshakeTimeout = 5
	cfg.RouteTimeout = 5
	cfg.Socks5Port = socksPort
	cfg.HTTPPort = httpPort
	cfg.WriteLogToFile = false
	cfg.UDPIdleTimeout = 5
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}

	srv := NewProxyServer(&cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("启动代理失败: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		srv.Shutdown(3 * time.Second)
	})

	return &testProxy{
		srv:      srv,
		socks5:   net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort)),
		httpAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort)),
	}
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 echo 服务失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func startUDPEchoServer(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 UDP echo 服务失败: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
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
	return pc.LocalAddr().String()
}

func newProxyHTTPClient(t *testing.T, proxyAddr string) *http.Client {
	t.Helper()
	u, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		t.Fatalf("解析代理地址失败: %v", err)
	}
	tr := &http.Transport{
		Proxy:               http.ProxyURL(u),
		MaxIdleConnsPerHost: 8,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// readHTTPHead 读取状态行与响应头（丢弃头内容），返回状态行
func readHTTPHead(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头失败: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return strings.TrimSpace(status)
}

// encodeSocksAddr 按 SOCKS5 规则编码 ATYP+ADDR+PORT
func encodeSocksAddr(host string, port int) []byte {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			b := append([]byte{atypIPv4}, ip4...)
			return binary.BigEndian.AppendUint16(b, uint16(port))
		}
		b := append([]byte{atypIPv6}, ip.To16()...)
		return binary.BigEndian.AppendUint16(b, uint16(port))
	}
	b := append([]byte{atypDomain, byte(len(host))}, []byte(host)...)
	return binary.BigEndian.AppendUint16(b, uint16(port))
}

// socksHandshake 完成无认证握手，返回连接与缓冲读取器
func socksHandshake(t *testing.T, proxyAddr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接 SOCKS5 端口失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("发送握手失败: %v", err)
	}
	br := bufio.NewReader(conn)
	resp := make([]byte, 2)
	if _, err := io.ReadFull(br, resp); err != nil {
		t.Fatalf("读取握手应答失败: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("握手应答异常: %v", resp)
	}
	return conn, br
}

// readSocksReply 解析 SOCKS5 应答，返回 REP 与 BND 地址
func readSocksReply(t *testing.T, r io.Reader) (byte, string, int) {
	t.Helper()
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("读取应答头失败: %v", err)
	}
	if head[0] != 0x05 {
		t.Fatalf("应答版本异常: 0x%02x", head[0])
	}

	var addrLen int
	switch head[3] {
	case atypIPv4:
		addrLen = net.IPv4len
	case atypIPv6:
		addrLen = net.IPv6len
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			t.Fatalf("读取域名长度失败: %v", err)
		}
		addrLen = int(l[0])
	default:
		t.Fatalf("应答地址类型异常: 0x%02x", head[3])
	}

	rest := make([]byte, addrLen+2)
	if _, err := io.ReadFull(r, rest); err != nil {
		t.Fatalf("读取应答地址失败: %v", err)
	}
	host := net.IP(rest[:addrLen]).String()
	if head[3] == atypDomain {
		host = string(rest[:addrLen])
	}
	port := int(binary.BigEndian.Uint16(rest[addrLen:]))
	return head[1], host, port
}

// socksConnect 发起 CONNECT 请求并返回 REP
func socksConnect(t *testing.T, conn net.Conn, br *bufio.Reader, host string, port int) byte {
	t.Helper()
	req := append([]byte{0x05, cmdConnect, 0x00}, encodeSocksAddr(host, port)...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("发送 CONNECT 请求失败: %v", err)
	}
	rep, _, _ := readSocksReply(t, br)
	return rep
}

// ---------- HTTP 代理 ----------

func TestHTTPProxyForwardGET(t *testing.T) {
	type captured struct {
		method string
		path   string
		host   string
		header http.Header
	}
	got := make(chan captured, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- captured{method: r.Method, path: r.URL.RequestURI(), host: r.Host, header: r.Header.Clone()}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("payload-from-backend"))
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	req, err := http.NewRequest(http.MethodGet, backend.URL+"/a/b?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-End-To-End", "keep-me")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("状态码 = %d, 期望 201", resp.StatusCode)
	}
	if string(body) != "payload-from-backend" {
		t.Errorf("响应体 = %q, 期望 payload-from-backend", body)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("上游自定义响应头未透传")
	}

	select {
	case c := <-got:
		if c.method != http.MethodGet {
			t.Errorf("上游收到方法 = %s, 期望 GET", c.method)
		}
		if c.path != "/a/b?x=1" {
			t.Errorf("上游收到路径 = %q, 期望 /a/b?x=1（应为 origin-form）", c.path)
		}
		if c.header.Get("Proxy-Authorization") != "" {
			t.Error("Proxy-Authorization 被泄露给源站")
		}
		if c.header.Get("Proxy-Connection") != "" {
			t.Error("Proxy-Connection 被泄露给源站")
		}
		if c.header.Get("X-End-To-End") != "keep-me" {
			t.Error("端到端自定义头未透传")
		}
		if c.header.Get("Connection") != "close" {
			t.Errorf("上游 Connection = %q, 期望 close（每请求新建上游连接）", c.header.Get("Connection"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("上游未收到请求")
	}
}

func TestHTTPProxyPOSTLargeBody(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	want := sha256.Sum256(payload)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:])))
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	resp, err := client.Post(backend.URL+"/upload", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if string(body) != hex.EncodeToString(want[:]) {
		t.Errorf("上游收到的内容校验和不匹配，长度可能被截断: got=%s", body)
	}
}

func TestHTTPProxyKeepAliveReuse(t *testing.T) {
	makeBackend := func(tag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(tag))
		}))
	}
	b1 := makeBackend("first")
	defer b1.Close()
	b2 := makeBackend("second")
	defer b2.Close()

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	for i, target := range []string{b1.URL, b2.URL} {
		u, _ := url.Parse(target)
		fmt.Fprintf(conn, "GET %s/hi HTTP/1.1\r\nHost: %s\r\n\r\n", target, u.Host)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("第 %d 个请求读取响应失败（keep-alive 可能未生效）: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := []string{"first", "second"}[i]
		if string(body) != want {
			t.Errorf("第 %d 个响应体 = %q, 期望 %q", i+1, body, want)
		}
		if resp.Close {
			t.Errorf("第 %d 个响应不应要求关闭客户端连接", i+1)
		}
	}
}

func TestHTTPProxyChunkedRequest(t *testing.T) {
	type result struct {
		body string
		err  string
	}
	got := make(chan result, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			got <- result{err: err.Error()}
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		got <- result{body: string(body)}
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	u, _ := url.Parse(backend.URL)

	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	fmt.Fprintf(conn, "POST %s/upload HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n\r\n", backend.URL, u.Host)
	_, _ = conn.Write([]byte("5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	select {
	case r := <-got:
		if r.err != "" {
			t.Fatalf("上游读取请求体失败: %s", r.err)
		}
		if r.body != "hello world" {
			t.Errorf("上游收到 body = %q, 期望 hello world（分块内容可能被丢弃）", r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("上游未收到请求")
	}
}

func TestHTTPProxyChunkedResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest ResponseWriter 不支持 Flush")
			return
		}
		for _, part := range []string{"aa", "bb", "cc"} {
			_, _ = w.Write([]byte(part))
			flusher.Flush()
		}
	}))
	defer backend.Close()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	resp, err := client.Get(backend.URL + "/stream")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "aabbcc" {
		t.Errorf("响应体 = %q, 期望 aabbcc", body)
	}
}

func TestHTTPProxyExpectContinue(t *testing.T) {
	got := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- string(body)
		_, _ = w.Write([]byte("done"))
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

	fmt.Fprintf(conn, "POST %s/echo HTTP/1.1\r\nHost: %s\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n",
		backend.URL, u.Host)

	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取 100 响应失败: %v", err)
	}
	if !strings.Contains(status, "100") {
		t.Fatalf("期望代理先回 100 Continue，实际: %q", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 100 响应头失败: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("发送请求体失败: %v", err)
	}

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("读取最终响应失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "done" {
		t.Errorf("最终响应体 = %q, 期望 done", body)
	}

	select {
	case b := <-got:
		if b != "hello" {
			t.Errorf("上游收到 body = %q, 期望 hello", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("上游未收到请求")
	}
}

func TestHTTPProxyHeaderTooLarge(t *testing.T) {
	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	fmt.Fprintf(conn, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nX-Big: %s\r\n\r\n",
		strings.Repeat("a", 70*1024))

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if !strings.Contains(status, "431") {
		t.Errorf("超大请求头应回 431，实际: %q", strings.TrimSpace(status))
	}
}

// TestHTTPProxyHeaderTooLargeKeepAlive 验证 keep-alive 连接上第二个请求的头部同样受限
func TestHTTPProxyHeaderTooLargeKeepAlive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
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

	resp, err := func() (*http.Response, error) {
		fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", backend.URL)
		return http.ReadResponse(br, nil)
	}()
	if err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: 127.0.0.1\r\nX-Big: %s\r\n\r\n",
		backend.URL, strings.Repeat("b", 70*1024))

	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取第二个响应失败: %v", err)
	}
	if !strings.Contains(status, "431") {
		t.Errorf("keep-alive 连接上的超大请求头应回 431，实际: %q", strings.TrimSpace(status))
	}
}

func TestHTTPProxyCONNECTTunnel(t *testing.T) {
	target := startEchoServer(t)
	p := startTestProxy(t, nil)

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

	if _, err := conn.Write([]byte("PING")); err != nil {
		t.Fatalf("隧道写入失败: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("隧道读取失败: %v", err)
	}
	if string(echo) != "PING" {
		t.Errorf("隧道回显 = %q, 期望 PING", echo)
	}
}

func TestHTTPProxyCONNECTEarlyData(t *testing.T) {
	target := startEchoServer(t)
	p := startTestProxy(t, nil)

	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	// CONNECT 头与隧道数据一次性写入：验证预读字节被补发而不是丢弃
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nEARLY", target, target)
	status := readHTTPHead(t, br)
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT 应回 200，实际: %q", status)
	}

	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("early data 未被转发（读取失败）: %v", err)
	}
	if string(echo) != "EARLY" {
		t.Errorf("隧道回显 = %q, 期望 EARLY", echo)
	}
}

func TestHTTPProxyUpstream1xx(t *testing.T) {
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
		_, _ = c.Write([]byte("HTTP/1.1 103 Early Hints\r\nLink: </s.css>; rel=preload\r\n\r\n"))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nfinal"))
		time.Sleep(200 * time.Millisecond)
	}()

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 200（1xx 不能被当成最终响应）", resp.StatusCode)
	}
	if string(body) != "final" {
		t.Errorf("响应体 = %q, 期望 final", body)
	}
}

func TestHTTPProxyRelativeURI(t *testing.T) {
	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "GET /only-path HTTP/1.1\r\nHost: example.com\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if !strings.Contains(status, "400") {
		t.Errorf("相对 URI 应回 400，实际: %q", strings.TrimSpace(status))
	}
}

// TestHTTPProxyUpstreamHTTP10NoLength 验证上游返回 HTTP/1.0 且无 Content-Length 时，
// 客户端能否得到完整的响应边界（否则会一直等待连接关闭）。
func TestHTTPProxyUpstreamHTTP10NoLength(t *testing.T) {
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
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			c.Close()
			return
		}
		// 写完响应体立即关闭：HTTP/1.0 无 Content-Length 时，响应边界就是连接关闭
		_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nBODY-UNTIL-CLOSE"))
		c.Close()
	}()

	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.httpAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr().String(), ln.Addr().String())
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("读取响应体超时/失败: %v（HTTP/1.0 无长度响应缺少结束边界，客户端会挂起到连接关闭）", err)
	}
	if string(body) != "BODY-UNTIL-CLOSE" {
		t.Errorf("响应体 = %q, 期望 BODY-UNTIL-CLOSE", body)
	}
}

// ---------- SOCKS5 ----------

func TestSocks5ConnectIPv4(t *testing.T) {
	target := startEchoServer(t)
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	if rep := socksConnect(t, conn, br, host, port); rep != repSucceeded {
		t.Fatalf("CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}

	if _, err := conn.Write([]byte("SOCKS-PING")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	echo := make([]byte, 10)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(echo) != "SOCKS-PING" {
		t.Errorf("回显 = %q, 期望 SOCKS-PING", echo)
	}
}

func TestSocks5ConnectDomain(t *testing.T) {
	target := startEchoServer(t)
	_, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	if rep := socksConnect(t, conn, br, "localhost", port); rep != repSucceeded {
		t.Fatalf("域名 CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}
	if _, err := conn.Write([]byte("DOM")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	echo := make([]byte, 3)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(echo) != "DOM" {
		t.Errorf("回显 = %q, 期望 DOM", echo)
	}
}

func TestSocks5ConnectIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("本机不支持 IPv6 环回: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)

	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	if rep := socksConnect(t, conn, br, "::1", addr.Port); rep != repSucceeded {
		t.Fatalf("IPv6 CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}
	if _, err := conn.Write([]byte("V6")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	echo := make([]byte, 2)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(echo) != "V6" {
		t.Errorf("回显 = %q, 期望 V6", echo)
	}
}

func TestSocks5BindRejected(t *testing.T) {
	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	req := append([]byte{0x05, cmdBind, 0x00}, encodeSocksAddr("1.2.3.4", 80)...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("发送 BIND 失败: %v", err)
	}
	rep, _, _ := readSocksReply(t, br)
	if rep != repCommandNotSupport {
		t.Errorf("BIND 应答 = 0x%02x, 期望 0x07", rep)
	}
}

func TestSocks5UnknownCommand(t *testing.T) {
	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	req := append([]byte{0x05, 0x09, 0x00}, encodeSocksAddr("1.2.3.4", 80)...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("发送未知命令失败: %v", err)
	}
	rep, _, _ := readSocksReply(t, br)
	if rep != repCommandNotSupport {
		t.Errorf("未知命令应答 = 0x%02x, 期望 0x07", rep)
	}
}

func TestSocks5NoAcceptableAuth(t *testing.T) {
	p := startTestProxy(t, nil)
	conn, err := net.DialTimeout("tcp", p.socks5, 5*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x02}); err != nil { // 仅提供用户名密码认证
		t.Fatalf("发送握手失败: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("读取应答失败: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0xFF {
		t.Errorf("应答 = %v, 期望 [05 ff]", resp)
	}
}

func TestSocks5EarlyData(t *testing.T) {
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
	br := bufio.NewReader(conn)

	// 握手 + 请求 + 负载一次性写入，验证 bufio 预读的负载被补发
	payload := append([]byte{0x05, 0x01, 0x00}, []byte{0x05, cmdConnect, 0x00}...)
	payload = append(payload, encodeSocksAddr(host, port)...)
	payload = append(payload, []byte("EARLY")...)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	handshake := make([]byte, 2)
	if _, err := io.ReadFull(br, handshake); err != nil {
		t.Fatalf("读取握手应答失败: %v", err)
	}
	rep, _, _ := readSocksReply(t, br)
	if rep != repSucceeded {
		t.Fatalf("CONNECT 应答 = 0x%02x, 期望 0x00", rep)
	}

	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("early data 未被转发（读取失败）: %v", err)
	}
	if string(echo) != "EARLY" {
		t.Errorf("回显 = %q, 期望 EARLY", echo)
	}
}

func TestSocks5ConnectRefused(t *testing.T) {
	deadPort := freeTCPPort(t) // 已释放，无人监听

	p := startTestProxy(t, nil)
	conn, br := socksHandshake(t, p.socks5)

	start := time.Now()
	rep := socksConnect(t, conn, br, "127.0.0.1", deadPort)
	elapsed := time.Since(start)

	if rep != repGeneralFailure {
		t.Errorf("目标不可达应答 = 0x%02x, 期望 0x01", rep)
	}
	if elapsed > 6*time.Second {
		t.Errorf("失败判定耗时 %v，超过路由探测超时", elapsed)
	}
}

func TestSocks5UDPAssociate(t *testing.T) {
	udpEcho := startUDPEchoServer(t)
	echoHost, echoPortStr, _ := net.SplitHostPort(udpEcho)
	echoPort, _ := strconv.Atoi(echoPortStr)

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
	_ = udpConn.SetDeadline(time.Now().Add(5 * time.Second))

	payload := []byte("UDP-PAYLOAD")
	pkt := append([]byte{0x00, 0x00, 0x00}, encodeSocksAddr(echoHost, echoPort)...)
	pkt = append(pkt, payload...)
	if _, err := udpConn.Write(pkt); err != nil {
		t.Fatalf("发送 UDP 报文失败: %v", err)
	}

	buf := make([]byte, 65535)
	n, err := udpConn.Read(buf)
	if err != nil {
		t.Fatalf("读取 UDP 回包失败: %v", err)
	}
	host, port, got, err := parseUDPDatagram(buf[:n])
	if err != nil {
		t.Fatalf("解析回包失败: %v: %v", err, buf[:n])
	}
	if host != echoHost || port != echoPort {
		t.Errorf("回包源地址 = %s:%d, 期望 %s:%d", host, port, echoHost, echoPort)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("回包负载 = %q, 期望 %q", got, payload)
	}
}

func TestSocks5UDPDisabled(t *testing.T) {
	p := startTestProxy(t, func(c *Config) { c.EnableUDP = false })
	conn, br := socksHandshake(t, p.socks5)

	req := append([]byte{0x05, cmdUDPAssociate, 0x00}, encodeSocksAddr("0.0.0.0", 0)...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("发送 UDP ASSOCIATE 失败: %v", err)
	}
	rep, _, _ := readSocksReply(t, br)
	if rep != repCommandNotSupport {
		t.Errorf("禁用 UDP 时应答 = 0x%02x, 期望 0x07", rep)
	}
}

// ---------- 限流与生命周期 ----------

// TestLongLivedConnectionsReleaseLimiter 验证长连接进入转发阶段后归还限流名额：
// 并发上限设为 2，先占满 2 条长连接，第 3 个请求仍应被立即处理而不是排队超时。
func TestLongLivedConnectionsReleaseLimiter(t *testing.T) {
	target := startEchoServer(t)
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	p := startTestProxy(t, func(c *Config) { c.MaxConcurrentConnections = 2 })

	longConns := make([]net.Conn, 0, 2)
	for i := 0; i < 2; i++ {
		conn, br := socksHandshake(t, p.socks5)
		if rep := socksConnect(t, conn, br, host, port); rep != repSucceeded {
			t.Fatalf("第 %d 条长连接建立失败: 0x%02x", i+1, rep)
		}
		if _, err := conn.Write([]byte("X")); err != nil {
			t.Fatalf("长连接写入失败: %v", err)
		}
		one := make([]byte, 1)
		if _, err := io.ReadFull(br, one); err != nil {
			t.Fatalf("长连接回显失败: %v", err)
		}
		longConns = append(longConns, conn)
	}
	defer func() {
		for _, c := range longConns {
			c.Close()
		}
	}()

	client := newProxyHTTPClient(t, p.httpAddr)
	start := time.Now()
	resp, err := client.Get(backend.URL + "/")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("长连接占用名额后，新请求失败: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 200（名额未被长连接及时归还）", resp.StatusCode)
	}
	if elapsed > maxQueueWait {
		t.Errorf("新请求耗时 %v，说明排队等待了限流名额", elapsed)
	}
}

// TestShutdownStopsAccepting 验证 Shutdown 后监听端口不再接受连接
func TestShutdownStopsAccepting(t *testing.T) {
	p := startTestProxy(t, nil)
	addr := p.httpAddr
	p.srv.Shutdown(2 * time.Second)

	conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
	if err == nil {
		conn.Close()
		t.Error("Shutdown 后仍能建立连接，监听器未释放")
	}
}

// TestConcurrentMixedTraffic 并发混合流量：HTTP 与 SOCKS5 同时工作，验证无串扰、无丢包
func TestConcurrentMixedTraffic(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("http-ok"))
	}))
	defer backend.Close()

	target := startEchoServer(t)
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	p := startTestProxy(t, nil)
	client := newProxyHTTPClient(t, p.httpAddr)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(backend.URL + "/")
			if err != nil {
				errs <- fmt.Errorf("http 请求失败: %w", err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "http-ok" {
				errs <- fmt.Errorf("http 响应异常: %q", body)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, br := socksHandshake(t, p.socks5)
			if rep := socksConnect(t, conn, br, host, port); rep != repSucceeded {
				errs <- fmt.Errorf("socks 建连失败: 0x%02x", rep)
				return
			}
			if _, err := conn.Write([]byte("ECHO")); err != nil {
				errs <- fmt.Errorf("socks 写入失败: %w", err)
				return
			}
			buf := make([]byte, 4)
			if _, err := io.ReadFull(br, buf); err != nil {
				errs <- fmt.Errorf("socks 读取失败: %w", err)
				return
			}
			if string(buf) != "ECHO" {
				errs <- fmt.Errorf("socks 回显异常: %q", buf)
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
