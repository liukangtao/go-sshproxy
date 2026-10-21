// http.go 实现HTTP/HTTPS协议处理功能
// 负责处理HTTP和HTTPS代理请求，包括CONNECT隧道和普通HTTP请求
package main

import (
	"bufio" // 用于实现带缓冲的I/O操作
	"bytes" // 用于构造 101 响应头
	"errors"
	"fmt" // 用于写入 101 状态行
	"io"  // 用于补发缓冲区残留数据
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// httpIdleTimeout 普通HTTP请求 keep-alive 复用时，等待下一个请求的空闲超时
	httpIdleTimeout = 60 * time.Second

	// maxHTTPHeaderBytes 单个请求头（请求行 + 头部字段）的大小上限。
	// net/http 的 Server 有 MaxHeaderBytes，但直接调用 http.ReadRequest 没有该保护。
	maxHTTPHeaderBytes = 64 * 1024
)

var (
	errHeaderTooLarge  = errors.New("HTTP 请求头超过大小限制")
	errExpectationFail = errors.New("不支持的 Expect")
)

// headerLimitReader 统计每个请求头部阶段的字节数。
// 计数发生在连接读入 bufio 的阶段：遇到空行（\r\n\r\n 或 \n\n）即停止，
// 因此请求体不会被计入；超过上限时置位 tooLarge，由调用方回送 431。
type headerLimitReader struct {
	r         io.Reader
	limit     int
	remaining int
	done      bool
	tooLarge  bool
	window    [4]byte
	winLen    int
}

func newHeaderLimitReader(r io.Reader, limit int) *headerLimitReader {
	return &headerLimitReader{r: r, limit: limit, remaining: limit}
}

// beginRequest 在解析下一个请求前重置计数
func (l *headerLimitReader) beginRequest() {
	l.remaining = l.limit
	l.done = false
	l.tooLarge = false
	l.window = [4]byte{}
	l.winLen = 0
}

func (l *headerLimitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n <= 0 || l.done {
		return n, err
	}

	for i := 0; i < n; i++ {
		l.remaining--
		if l.remaining < 0 {
			l.tooLarge = true
			// 同时返回已读数据与错误：bufio 会先交付数据，再在后续读取时返回该错误
			return n, errHeaderTooLarge
		}

		b := p[i]
		if l.winLen < 4 {
			l.window[l.winLen] = b
			l.winLen++
		} else {
			copy(l.window[:], l.window[1:])
			l.window[3] = b
		}

		if l.winLen == 4 && l.window == [4]byte{'\r', '\n', '\r', '\n'} {
			l.done = true
			return n, err
		}
		// 兼容仅用 \n 分行的客户端
		if l.winLen >= 2 && l.window[l.winLen-1] == '\n' && l.window[l.winLen-2] == '\n' {
			l.done = true
			return n, err
		}
	}
	return n, err
}

// idleReader 在每次读取前刷新连接读超时。
// 用于覆盖请求体上传、keep-alive 等待等"握手超时已清除"的阶段：
// 只要仍有数据到达就不会超时，客户端中途停住则在空闲超时后断开。
type idleReader struct {
	conn    net.Conn
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.timeout > 0 {
		_ = r.conn.SetReadDeadline(time.Now().Add(r.timeout))
	}
	return r.conn.Read(p)
}

// idleWriter 在每次写入前刷新连接写超时：
// 客户端长时间不读取响应时避免单个慢连接无限占用 goroutine 与限流名额
type idleWriter struct {
	conn    net.Conn
	timeout time.Duration
}

func (w *idleWriter) Write(p []byte) (int, error) {
	if w.timeout > 0 {
		_ = w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
	}
	return w.conn.Write(p)
}

// handleHTTPProxy HTTP代理处理入口：解析首个请求后按 CONNECT / 普通请求 分发
// established 在进入数据转发阶段后调用，提前归还限流名额（见 serveConn）
func (s *ProxyServer) handleHTTPProxy(conn net.Conn, established func()) {
	// 握手阶段设置超时：防止客户端连上后长时间不发送请求占用资源
	conn.SetDeadline(time.Now().Add(time.Duration(s.config.HandshakeTimeout) * time.Second))
	defer conn.Close()

	// 首个请求头阶段沿用上面的绝对超时（防止慢速逐字节发送头部拖延）；
	// 解析完成后再启用"每次读取刷新超时"，覆盖请求体上传与 keep-alive 等待
	idle := &idleReader{conn: conn}
	hlr := newHeaderLimitReader(idle, maxHTTPHeaderBytes)
	reader := bufio.NewReader(hlr)
	hlr.beginRequest()

	req, err := http.ReadRequest(reader)
	if err != nil || hlr.tooLarge {
		if hlr.tooLarge {
			writeSimpleResponse(conn, "431 Request Header Fields Too Large")
		} else {
			log.Printf("请求解析失败: %v", err)
			writeSimpleResponse(conn, "400 Bad Request")
		}
		return
	}

	// 首个请求已到达：清除握手阶段的硬超时，避免转发阶段的大文件下载、
	// 长轮询、WebSocket 等长连接被硬超时强制中断
	conn.SetDeadline(time.Time{})
	// 之后每次读取都刷新空闲超时：请求体中途停住或 keep-alive 长期空闲会被回收
	idle.timeout = time.Duration(s.config.HandshakeTimeout) * time.Second

	if req.Method == "CONNECT" {
		// CONNECT 没有请求体，握手后客户端可能立刻发送隧道数据（early data）。
		// 这些字节可能已被 bufio 预读，必须取出并在隧道建立后补发
		early := drainBuffered(reader)
		// drainBuffered 可能经由 idleReader 设置过读超时，进入隧道前必须清除，
		// 否则长连接会在 60 秒后被这个遗留的 deadline 中断
		conn.SetDeadline(time.Time{})
		s.handleHTTPConnect(conn, early, req, established)
		return
	}
	s.handleHTTPPlain(conn, reader, hlr, req, established)
}

// drainBuffered 取出读取器中已预读但尚未消费的字节
func drainBuffered(reader *bufio.Reader) []byte {
	n := reader.Buffered()
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return nil
	}
	return buf
}

// writeSimpleResponse 回送一个无响应体的短响应并关闭连接
func writeSimpleResponse(conn net.Conn, status string) {
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte("HTTP/1.1 " + status + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// handleHTTPConnect 处理 CONNECT 隧道请求：建立隧道后双向转发原始字节
// early 为握手阶段已预读的隧道数据（可能为空）
func (s *ProxyServer) handleHTTPConnect(conn net.Conn, early []byte, req *http.Request, established func()) {
	targetAddr := req.URL.Host
	if targetAddr == "" {
		writeSimpleResponse(conn, "400 Bad Request")
		return
	}

	host, port, err := net.SplitHostPort(targetAddr)
	if err != nil {
		host, port = targetAddr, "443"
	}
	targetAddr = net.JoinHostPort(host, port)

	result := s.selectRoute(targetAddr)
	if result.conn == nil {
		// 回送明确的失败响应，避免客户端只感知到"连接被重置"
		writeSimpleResponse(conn, "502 Bad Gateway")
		return
	}
	// 转发结束后先关闭隧道，再归还底层SSH连接（Direct路径 sshClient 为 nil）；
	// 使用期间连接不驻留池中，避免被池满释放/空闲清理逻辑误关闭
	defer func() {
		result.conn.Close()
		if result.sshClient != nil {
			s.sshPool.Put(result.sshClient)
		}
	}()

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		log.Printf("CONNECT 应答失败: %v", err)
		return
	}

	// 补发握手阶段已读入的隧道数据：
	// 客户端若在 CONNECT 响应前就发送数据（early data），这些字节已被读入缓冲，
	// 直接 relay 原始连接会丢失它们
	if len(early) > 0 {
		if _, werr := result.conn.Write(early); werr != nil {
			log.Printf("转发预读数据失败: %v", werr)
			return
		}
	}

	go s.logRouteResult(targetAddr, result.method.String(), result.latency)

	// 已进入数据转发阶段：归还限流名额，隧道存续期间不再占用并发上限（幂等）
	established()

	done := make(chan struct{}, 2)
	go s.relay(result.conn, conn, done)
	go s.relay(conn, result.conn, done)

	// 等待两个方向都完成：某一方向先结束时只对其做半关闭（CloseWrite），
	// 反方向仍可继续传输，支持"客户端发完关写端等响应"等半关闭场景
	<-done
	<-done
}

// handleHTTPPlain 处理普通HTTP请求：逐请求转发，支持客户端 keep-alive 复用
//
// 设计说明：每个请求独立选路、独立建连、转发完整响应后关闭上游连接；
// 客户端到代理的连接保持开启，可复用于后续任意目标的请求（真正的 keep-alive）。
func (s *ProxyServer) handleHTTPPlain(conn net.Conn, reader *bufio.Reader, hlr *headerLimitReader, req *http.Request, established func()) {
	respWriter := &idleWriter{
		conn:    conn,
		timeout: time.Duration(s.config.HandshakeTimeout) * time.Second,
	}

	for {
		// Expect: 100-continue 必须在写请求体之前处理：
		// 客户端在等 100，而 req.Write 会阻塞读请求体；由代理先回 100 并删除该头，
		// 既解开等待，也避免上游再发一个 100 被误当成最终响应
		if err := handleExpectContinue(conn, req); err != nil {
			if !errors.Is(err, errExpectationFail) {
				log.Printf("处理 Expect 失败: %v", err)
			}
			return
		}

		targetAddr := req.URL.Host
		if targetAddr == "" {
			// 代理请求要求 absolute-form（http://host/path），相对路径无法确定目标
			writeSimpleResponse(conn, "400 Bad Request")
			return
		}
		host, port, err := net.SplitHostPort(targetAddr)
		if err != nil {
			host, port = targetAddr, "80"
		}
		targetAddr = net.JoinHostPort(host, port)

		// Upgrade 请求（典型为 WebSocket）需要保留 Connection/Upgrade 头，
		// 并在收到 101 后切换为双向字节流，不能走"剥离逐跳头 + 逐请求转发"的普通路径
		isUpgrade := isUpgradeRequest(req)

		result := s.selectRoute(targetAddr)
		if result.conn == nil {
			writeSimpleResponse(conn, "502 Bad Gateway")
			return
		}

		if req.URL.Port() == "443" {
			req.URL.Scheme = "https"
		} else {
			req.URL.Scheme = "http"
		}
		req.URL.Host = targetAddr
		req.RequestURI = ""

		if isUpgrade {
			s.serveHTTPUpgrade(conn, reader, req, &result, established)
			return
		}

		// 逐跳头只对当前连接有意义，必须剥离后再发往上游：
		// 尤其 Proxy-Authorization 属于客户端与本代理之间的凭据，不能泄露给源站
		removeHopByHopHeaders(req.Header)
		// 上游连接每请求新建、响应收完即关：诚实声明 close，
		// 不再发送 Keep-Alive 让上游保持一条我们根本不会复用的连接
		req.Header.Set("Connection", "close")

		if err = req.Write(result.conn); err != nil {
			log.Printf("发送请求失败: %v", err)
			s.closeRouteResult(&result)
			return
		}

		// 上游读取器在多个响应之间复用：若为每个响应新建 bufio.Reader，
		// 100 与最终响应同包到达时，后者的字节会滞留在被丢弃的缓冲里
		upReader := bufio.NewReader(result.conn)
		var resp *http.Response
		for {
			resp, err = http.ReadResponse(upReader, req)
			if err != nil {
				log.Printf("读取上游响应失败: %v", err)
				s.closeRouteResult(&result)
				return
			}
			// 1xx 是中间响应（100 Continue / 103 Early Hints 等）：
			// 不能当作最终响应回写，否则真正的 2xx/4xx 会被丢弃
			if resp.StatusCode >= 100 && resp.StatusCode < 200 {
				resp.Body.Close()
				continue
			}
			break
		}

		// 客户端连接的关闭语义只由客户端请求决定：
		// 不透传上游的 "Connection: close"（上游每请求必关），
		// 否则会连带迫使客户端关闭，keep-alive 形同虚设
		clientClose := req.Close
		// 例外：上游响应既无 Content-Length 也不是 chunked（典型为 HTTP/1.0 且无长度），
		// 响应边界只能由"连接关闭"表达。此时若继续对客户端保持连接，
		// 客户端会一直等待 EOF（直到空闲超时），表现为请求悬挂。
		if resp.ContentLength < 0 && len(resp.TransferEncoding) == 0 && !resp.ProtoAtLeast(1, 1) {
			clientClose = true
		}
		resp.Close = clientClose
		resp.Header.Del("Connection")
		resp.Header.Del("Keep-Alive")
		resp.Header.Del("Proxy-Connection")
		werr := resp.Write(respWriter)
		resp.Body.Close()
		s.closeRouteResult(&result)
		if werr != nil {
			log.Printf("回写响应失败: %v", werr)
			return
		}

		go s.logRouteResult(targetAddr, result.method.String(), result.latency)

		// 请求已完整处理：归还限流名额；后续 keep-alive 空闲期与后续请求
		// 均不再占用名额（幂等，首次调用生效）
		established()

		if clientClose {
			return
		}

		// 等待同一条连接上的下一个请求：空闲超时后正常断开
		conn.SetDeadline(time.Now().Add(httpIdleTimeout))
		hlr.beginRequest()
		req, err = http.ReadRequest(reader)
		if err != nil || hlr.tooLarge {
			if hlr.tooLarge {
				writeSimpleResponse(conn, "431 Request Header Fields Too Large")
			}
			return // 客户端断开、空闲超时或请求头超限，均结束
		}
		conn.SetDeadline(time.Time{})

		// keep-alive 连接上出现 CONNECT 属协议误用（隧道不会复用前置连接），明确拒绝
		if req.Method == "CONNECT" {
			writeSimpleResponse(conn, "400 Bad Request")
			return
		}
	}
}

// isUpgradeRequest 判断请求是否要求协议升级：
// 必须同时满足 Upgrade 头非空，且 Connection 的令牌列表中包含 upgrade
// （RFC 7230 要求 Connection 声明升级意图，缺一不可视为升级请求）
func isUpgradeRequest(req *http.Request) bool {
	if req.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range req.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// serveHTTPUpgrade 处理协议升级请求（典型为 WebSocket）。
// 上游返回 101 后转为双向字节转发；上游拒绝升级时按普通响应回写并关闭客户端连接。
func (s *ProxyServer) serveHTTPUpgrade(conn net.Conn, clientReader *bufio.Reader, req *http.Request, result *routeResult, established func()) {
	defer s.closeRouteResult(result)

	// Connection/Upgrade 是本次升级的端到端语义，不能按逐跳头剥离；
	// 这里只移除属于"客户端 ↔ 代理"这一跳的凭据与连接管理头
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Authenticate")

	if err := req.Write(result.conn); err != nil {
		log.Printf("发送 Upgrade 请求失败: %v", err)
		writeSimpleResponse(conn, "502 Bad Gateway")
		return
	}

	upReader := bufio.NewReader(result.conn)
	resp, err := http.ReadResponse(upReader, req)
	if err != nil {
		log.Printf("读取 Upgrade 响应失败: %v", err)
		writeSimpleResponse(conn, "502 Bad Gateway")
		return
	}

	// 上游拒绝升级（如 400/426）：按普通响应原样回写（含响应体），随后关闭连接。
	// 此时连接语义已无法安全复用，明确声明 close。
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Close = true
		resp.Header.Del("Proxy-Connection")
		werr := resp.Write(&idleWriter{
			conn:    conn,
			timeout: time.Duration(s.config.HandshakeTimeout) * time.Second,
		})
		resp.Body.Close()
		if werr != nil {
			log.Printf("回写 Upgrade 拒绝响应失败: %v", werr)
		}
		return
	}

	// 101 不能交给 resp.Write：协议切换后响应体是双向流，Write 会尝试复制 body 而阻塞。
	// 这里手写状态行与头部，原样保留 Upgrade/Connection/Sec-WebSocket-Accept 等字段。
	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/1.1 101 %s\r\n", http.StatusText(http.StatusSwitchingProtocols))
	resp.Header.Write(&head)
	head.WriteString("\r\n")
	if _, err := conn.Write(head.Bytes()); err != nil {
		log.Printf("回写 101 响应失败: %v", err)
		return
	}

	// 两端的缓冲读取器都可能已预读协议切换后的应用数据，必须先补发再 relay，
	// 否则这部分字节会滞留在被丢弃的 bufio 中
	if early := drainBuffered(upReader); len(early) > 0 {
		if _, err := conn.Write(early); err != nil {
			return
		}
	}
	if early := drainBuffered(clientReader); len(early) > 0 {
		if _, err := result.conn.Write(early); err != nil {
			return
		}
	}

	go s.logRouteResult(req.URL.Host, result.method.String(), result.latency)

	// 已进入双向转发阶段：归还限流名额，长连接存续期间不再占用并发上限（幂等）
	established()

	done := make(chan struct{}, 2)
	go s.relay(result.conn, conn, done)
	go s.relay(conn, result.conn, done)
	<-done
	<-done
}

// handleExpectContinue 处理客户端的 Expect 头
// 仅支持 100-continue：立即回送 100 并删除该头，让后续 req.Write 能读到请求体
func handleExpectContinue(conn net.Conn, req *http.Request) error {
	expect := strings.TrimSpace(req.Header.Get("Expect"))
	if expect == "" {
		return nil
	}
	if !strings.EqualFold(expect, "100-continue") {
		writeSimpleResponse(conn, "417 Expectation Failed")
		return errExpectationFail
	}
	if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
		return err
	}
	req.Header.Del("Expect")
	return nil
}

// removeHopByHopHeaders 删除 RFC 7230 定义的逐跳头，以及 Connection 头中列出的字段。
// 这些字段只对单段连接有意义，代理转发时必须重新生成，不能原样透传。
func removeHopByHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(name)
	}
}

// closeRouteResult 关闭路由结果连接并归还底层SSH连接（Direct路径 sshClient 为 nil）
func (s *ProxyServer) closeRouteResult(result *routeResult) {
	if result.conn != nil {
		result.conn.Close()
	}
	if result.sshClient != nil {
		s.sshPool.Put(result.sshClient)
	}
}
