// proxy.go 实现代理服务核心功能
// 负责启动和管理SOCKS5和HTTP代理服务
package main

import (
	"context" // 用于控制goroutine的上下文
	"errors"  // 用于识别 listener 已关闭等错误
	"fmt"     // 用于格式化错误信息
	"io"
	"log" // 用于记录日志
	"net" // 用于网络I/O操作
	"os"  // 用于主机密钥指纹文件
	"path/filepath"
	"sort" // DNS 结果排序（IPv4 优先）
	"strconv"
	"strings"
	"sync" // 用于限流名额的幂等释放
	"time" // 用于时间处理

	"golang.org/x/crypto/ssh" // 用于SSH客户端连接
)

const (
	hostKeyFileName = "proxy.hostkey" // 主机密钥指纹文件名（TOFU：首次连接学习并固定）

	maxDNSCacheSize = 10000 // DNS缓存最大条目数（防内存无界增长）

	// maxQueueWait 并发限流排队的最长等待时间：超时快速拒绝，避免过载时连接无限堆积
	maxQueueWait = 2 * time.Second
)

// Start 启动代理服务器的所有服务
// 监听器在这里同步创建：端口被占用时立即返回错误，由 main 统一处理，
// 避免在协程里 log.Fatal 跳过 PID 文件、连接池与日志句柄的清理。
func (s *ProxyServer) Start(ctx context.Context) error {
	// 安全提示：监听非环回地址时给出醒目警告（代理本身无认证）
	if !isLoopbackAddr(s.config.ListenAddr) {
		log.Printf("警告: 代理监听地址为 %s（非仅本机），代理无认证，任何可达该地址的设备都可使用，请确保网络可信！", s.config.ListenAddr)
	}

	// 初始化 SSH 配置
	sshConfig := s.createSSHConfig()

	// 初始化 SSH 连接池
	s.sshPool = NewSSHConnectionPool(sshConfig, s.config.SSHHost, strconv.Itoa(s.config.SSHPort), s.config.SSHPoolSize)

	// 预热连接池（提前创建 5 个连接）
	s.sshPool.Prewarm(5)

	// 打开路由日志文件（常开句柄）
	s.openRouteLog()

	// 启动定期检查任务（Shutdown 时通过 stopCh 退出）
	go s.CheckTargetInfo()

	// 启动 SOCKS5 代理
	if s.config.Socks5Port > 0 {
		// SSH 隧道不支持 UDP，启用 UDP ASSOCIATE 后 UDP 流量只能走本机直连出口
		if s.config.EnableUDP {
			log.Println("提示: 已启用 SOCKS5 UDP ASSOCIATE，UDP 流量将走本机直连出口（SSH 隧道不支持 UDP），会暴露本机真实出口 IP")
		} else {
			log.Println("提示: 已禁用 SOCKS5 UDP ASSOCIATE，客户端发起的 UDP 请求将收到“命令不支持(0x07)”应答")
		}

		listener, err := s.listen("SOCKS5", s.config.Socks5Port)
		if err != nil {
			s.cleanupStartFailure()
			return err
		}
		s.acceptWG.Add(1)
		go func() {
			defer s.acceptWG.Done()
			defer listener.Close()
			s.acceptLoop(ctx, listener, "SOCKS5", s.handleConnection, rejectSocks5Busy)
		}()
	}

	// 启动 HTTP 代理
	if s.config.HTTPPort > 0 {
		listener, err := s.listen("HTTP", s.config.HTTPPort)
		if err != nil {
			s.cleanupStartFailure()
			return err
		}
		s.acceptWG.Add(1)
		go func() {
			defer s.acceptWG.Done()
			defer listener.Close()
			s.acceptLoop(ctx, listener, "HTTP", s.handleHTTPProxy, rejectHTTPBusy)
		}()
	}

	return nil
}

// listen 创建并登记监听器
func (s *ProxyServer) listen(name string, port int) (net.Listener, error) {
	addr := net.JoinHostPort(s.config.ListenAddr, strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s代理启动失败(%s): %w", name, addr, err)
	}
	s.trackListener(listener)
	log.Printf("%s代理已启动 %s", name, addr)
	return listener, nil
}

// cleanupStartFailure 启动中途失败时释放已创建的资源（可重复调用）
func (s *ProxyServer) cleanupStartFailure() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.closeListeners()
	s.acceptWG.Wait()
	if s.sshPool != nil {
		s.sshPool.closeAll()
	}
	s.closeRouteLog()
}

// Shutdown 优雅关闭：停止接受新连接、等待在途连接结束、释放空闲SSH连接与日志句柄
func (s *ProxyServer) Shutdown(graceTimeout time.Duration) {
	// 通知后台任务（缓存清理、日志刷盘）退出
	s.stopOnce.Do(func() { close(s.stopCh) })

	// 关闭监听器，Accept 立即返回，不再接受新连接
	s.closeListeners()

	// 先等监听循环退出：activeConn.Add 在循环内完成，
	// 这样后面的 Wait 不会与 Add 并发（WaitGroup 要求计数为 0 时的 Add 先于 Wait）
	s.acceptWG.Wait()

	// 等待在途连接处理完成（带超时，避免长连接导致无法退出）
	done := make(chan struct{})
	go func() {
		s.activeConn.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(graceTimeout):
		log.Println("等待在途连接超时，强制退出")
	}

	// 释放连接池中的空闲连接与日志句柄
	if s.sshPool != nil {
		s.sshPool.closeAll()
	}
	s.closeRouteLog()
}

// serveConn 连接处理入口：并发限流 + 分发到具体协议处理
// reject 在排队超时（系统过载）时调用：由协议侧回送"繁忙"语义后关闭连接
//
// 限流名额只覆盖"握手/建连阶段"：handler 在进入数据转发阶段后调用 established
// 提前归还名额（幂等）。否则 WebSocket、长轮询、大文件下载等长连接会长期占满
// 名额，导致后续所有新请求排队超时后被拒绝。
//
// 在途连接计数由 acceptLoop 在启动协程前完成（见 activeConn 的注释）。
func (s *ProxyServer) serveConn(conn net.Conn, handler func(net.Conn, func()), reject func(net.Conn)) {
	// 限流：达到上限时排队等待（SOCKS5 与 HTTP 共用同一个上限）；
	// 排队超时则快速拒绝——过载时"快速失败"优于让连接无限堆积后集体超时
	select {
	case s.limiter <- struct{}{}:
	case <-time.After(maxQueueWait):
		reject(conn)
		return
	}
	var once sync.Once
	established := func() { once.Do(func() { <-s.limiter }) }
	defer established() // handler 未显式归还时（建连失败等短路径），返回时兜底释放

	handler(conn, established)
}

// rejectSocks5Busy 过载拒绝（SOCKS5侧）：回送"通用失败(0x01)"应答后关闭连接
func rejectSocks5Busy(conn net.Conn) {
	log.Printf("系统繁忙（并发已达上限且排队超时），拒绝连接: %s", conn.RemoteAddr())
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = sendSocks5Response(conn, repGeneralFailure)
}

// rejectHTTPBusy 过载拒绝（HTTP侧）：回送 503 后关闭连接
func rejectHTTPBusy(conn net.Conn) {
	log.Printf("系统繁忙（并发已达上限且排队超时），拒绝连接: %s", conn.RemoteAddr())
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// trackListener 记录监听器，供 Shutdown 时统一关闭
func (s *ProxyServer) trackListener(l net.Listener) {
	s.listenersMu.Lock()
	s.listeners = append(s.listeners, l)
	s.listenersMu.Unlock()
}

// closeListeners 关闭所有监听器
func (s *ProxyServer) closeListeners() {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	for _, l := range s.listeners {
		l.Close()
	}
}

// acceptLoop 接受连接并分发。
// 在途计数（activeConn.Add）必须在本函数内、启动处理协程前完成：
// Shutdown 先 closeListeners、再等 acceptWG 退出，最后才 activeConn.Wait，
// 从时序上保证不会出现"Wait 已开始、Add 才发生"的 WaitGroup 误用。
func (s *ProxyServer) acceptLoop(ctx context.Context, listener net.Listener, name string, handler func(net.Conn, func()), reject func(net.Conn)) {
	var backoff time.Duration

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return // 关闭流程中（listener 已关），正常退出
			}

			// 临时错误（典型如 EMFILE）不能立即 continue：
			// 否则会形成忙循环并把日志刷爆，这里做指数退避
			log.Printf("%s Accept 失败: %v", name, err)
			if backoff == 0 {
				backoff = 50 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		backoff = 0

		s.activeConn.Add(1)
		go func(c net.Conn) {
			defer s.activeConn.Done()
			s.serveConn(c, handler, reject)
		}(conn)
	}
}

// createSSHConfig 创建SSH客户端配置
func (s *ProxyServer) createSSHConfig() *ssh.ClientConfig {
	config := &ssh.ClientConfig{
		User:    s.config.SSHUser,
		Timeout: time.Duration(s.config.SSHTimeout) * time.Second,
	}

	if s.config.Insecure {
		log.Println("警告: 已启用 insecure 模式，跳过 SSH 主机密钥验证（存在中间人风险，仅建议调试时使用）")
		config.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	} else {
		// 默认使用 TOFU（首次信任）：首次连接时记录服务器指纹，之后逐次校验，
		// 防止中间人攻击；服务器重建后需删除指纹文件让其重新学习
		config.HostKeyCallback = s.makeHostKeyCallback()
	}

	config.Auth = []ssh.AuthMethod{ssh.Password(s.config.SSHPasswd)}
	return config
}

// makeHostKeyCallback 返回 TOFU 主机密钥校验回调
// 首次连接：记录服务器指纹到文件；后续连接：逐次比对，不匹配即拒绝
func (s *ProxyServer) makeHostKeyCallback() ssh.HostKeyCallback {
	path := findConfigFile(hostKeyFileName)
	if path == "" {
		path = filepath.Join(primaryConfigDir(), hostKeyFileName)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)

		// 串行化：避免多个并发首次连接同时读写指纹文件
		s.hostKeyMu.Lock()
		defer s.hostKeyMu.Unlock()

		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			if werr := os.WriteFile(path, []byte(fingerprint+"\n"), 0600); werr != nil {
				// 失败即拒绝连接（fail-closed）：仅告警后放行会让 TOFU 静默退化为
				// "不校验主机密钥"，中间人攻击下口令会直接泄露
				return fmt.Errorf("无法写入主机密钥指纹文件 %s: %w（为避免中间人风险已拒绝连接；请修复目录权限，或仅在调试时把配置项 insecure 设为 true）", path, werr)
			}
			log.Printf("首次连接: 已记录服务器 (%s) 主机密钥指纹: %s", s.config.SSHHost, fingerprint)
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取主机密钥指纹文件失败: %w", err)
		}

		saved := strings.TrimSpace(string(data))
		if saved != fingerprint {
			return fmt.Errorf("主机密钥不匹配: 期望 %s，实际 %s（若服务器已重建，请删除 %s 后重试）",
				saved, fingerprint, path)
		}
		return nil
	}
}

// relay 在两个连接之间转发数据，使用 bufferPool 优化性能
// 本方向结束后对目标连接做半关闭（CloseWrite），通知对端"本方向已发送完毕"，
// 同时保留反方向继续传输的能力（TCP 半关闭语义）
func (s *ProxyServer) relay(dst, src net.Conn, done chan struct{}) {
	defer func() { done <- struct{}{} }()
	buf := s.bufferPool.Get().([]byte)
	defer s.bufferPool.Put(buf)
	_, _ = io.CopyBuffer(dst, src, buf)
	closeWrite(dst)
}

// closeWrite 尽力关闭连接的写方向（*net.TCPConn 与 SSH 通道均支持）；
// 不支持半关闭的连接（如 net.Pipe）忽略即可
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// dnsLookupTimeout 单次 DNS 解析超时（与路由探测超时解耦，避免解析僵死拖住请求）
const dnsLookupTimeout = 5 * time.Second

// resolveAll 解析主机地址，返回全部可用 IP（IPv4 优先），带TTL缓存。
//
// 与只取 ips[0] 相比，多地址列表让直连在遇到坏 IP（例如本机无 IPv6 出口）时
// 还能回退到其他地址；同一域名的并发首次解析通过单飞（singleflight）合并为一次。
func (s *ProxyServer) resolveAll(ctx context.Context, targetAddr string) []string {
	host, port, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return []string{targetAddr}
	}

	// 如果已经是 IP，直接返回
	if ip := net.ParseIP(host); ip != nil {
		return []string{targetAddr}
	}

	now := time.Now()

	// 查缓存（未过期的条目才使用）；同时观察是否有同域名的解析正在进行
	s.dnsMu.RLock()
	entry, ok := s.dnsCache[host]
	inflight, hasInflight := s.dnsFlight[host]
	s.dnsMu.RUnlock()
	if ok && now.Before(entry.expire) {
		return hostPorts(entry.ips, port)
	}

	// 已有同域名的解析在进行：等待并复用其结果（并发击穿保护）
	if hasInflight {
		select {
		case <-inflight.done:
			if len(inflight.ips) > 0 {
				return hostPorts(inflight.ips, port)
			}
			return []string{targetAddr}
		case <-ctx.Done():
			return []string{targetAddr}
		}
	}

	// 登记本协程为解析发起者（二次检查：查看期间可能已被其他协程抢先）
	s.dnsMu.Lock()
	if inflight2, ok2 := s.dnsFlight[host]; ok2 {
		s.dnsMu.Unlock()
		select {
		case <-inflight2.done:
			if len(inflight2.ips) > 0 {
				return hostPorts(inflight2.ips, port)
			}
			return []string{targetAddr}
		case <-ctx.Done():
			return []string{targetAddr}
		}
	}
	inflight = &dnsCall{done: make(chan struct{})}
	s.dnsFlight[host] = inflight
	s.dnsMu.Unlock()

	// 解析使用脱离调用方取消的 ctx：单个请求被取消不应让同一域名的其他等待者
	// 全部拿到失败结果；同时保留独立的 5 秒上限，避免解析僵死。
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dnsLookupTimeout)
	ips, lerr := net.DefaultResolver.LookupHost(lookupCtx, host)
	cancel()

	if lerr == nil {
		// IPv4 优先：双栈主机在无 IPv6 出口的环境下先试 IPv6 会白白等待超时
		sort.SliceStable(ips, func(i, j int) bool {
			return net.ParseIP(ips[i]).To4() != nil && net.ParseIP(ips[j]).To4() == nil
		})
	}
	doneAt := time.Now()

	// 存缓存（带过期时间）。容量满时先清理过期项；仍满则淘汰最早过期的条目
	// （TTL 相同时即最早写入的，近似 LRU），保证新域名仍能进入缓存，
	// 而不是在缓存饱和后彻底放弃写入、导致命中率持续下降。
	s.dnsMu.Lock()
	if len(ips) > 0 {
		if _, exists := s.dnsCache[host]; !exists && len(s.dnsCache) >= maxDNSCacheSize {
			for h, e := range s.dnsCache {
				if doneAt.After(e.expire) {
					delete(s.dnsCache, h)
				}
			}
			if len(s.dnsCache) >= maxDNSCacheSize {
				var victim string
				var victimExpire time.Time
				for h, e := range s.dnsCache {
					if victim == "" || e.expire.Before(victimExpire) {
						victim, victimExpire = h, e.expire
					}
				}
				if victim != "" {
					delete(s.dnsCache, victim)
				}
			}
		}
		if _, exists := s.dnsCache[host]; exists || len(s.dnsCache) < maxDNSCacheSize {
			ttl := time.Duration(s.config.DNSCacheTTLSeconds) * time.Second
			if ttl <= 0 {
				ttl = 5 * time.Minute
			}
			s.dnsCache[host] = dnsEntry{ips: ips, expire: doneAt.Add(ttl)}
		}
	}
	inflight.ips = ips
	delete(s.dnsFlight, host)
	s.dnsMu.Unlock()
	close(inflight.done)

	if len(ips) > 0 {
		return hostPorts(ips, port)
	}
	return []string{targetAddr}
}

// resolve 返回首个解析结果（供 UDP 中继使用：UDP 无连接，只能选定一个目标地址）
func (s *ProxyServer) resolve(targetAddr string) string {
	addrs := s.resolveAll(context.Background(), targetAddr)
	if len(addrs) == 0 {
		return targetAddr
	}
	return addrs[0]
}

// hostPorts 把 IP 列表拼接为 host:port（IPv6 自动加方括号）
func hostPorts(ips []string, port string) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, port))
	}
	return out
}

// isLoopbackAddr 判断监听地址是否为环回（仅本机可访问）
func isLoopbackAddr(addr string) bool {
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}
