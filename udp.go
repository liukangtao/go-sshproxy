// udp.go 实现SOCKS5 UDP ASSOCIATE(0x03)的UDP中继能力
// 说明：SSH 的 direct-tcpip 通道不支持 UDP，因此 UDP 流量只能走本机直连出口，
// 该出口是本机真实网络，与 TCP 走SSH隧道出口的行为不同，请知悉。
package main

import (
	"encoding/binary" // 用于构造UDP报文头中的端口字段
	"errors"          // 用于区分 socket 关闭与临时读错误
	"log"             // 用于记录日志
	"net"             // 用于网络I/O操作
	"strconv"         // 用于端口字符串转换
	"sync"            // 用于同步原语
	"sync/atomic"     // 用于无锁计数与时间戳
	"time"            // 用于时间处理
)

// maxUDPDatagram UDP 收发缓冲区大小（含SOCKS5 UDP头）
const maxUDPDatagram = 65535

// udpReadRetryDelay UDP 读错误后的退避时间：避免持续性错误形成忙循环
const udpReadRetryDelay = 10 * time.Millisecond

// maxUDPTargetsPerAssoc 单个 UDP 关联记录的目标地址上限：
// 仅用于过滤回包来源，超出后不再记录新目标（防止恶意客户端撑大内存）
const maxUDPTargetsPerAssoc = 1024

// udpAssociation 表示一条SOCKS5 UDP关联：一个客户端 对应 一对UDP socket
type udpAssociation struct {
	server *ProxyServer

	relayConn *net.UDPConn // 面向客户端的中继socket，其地址会通过应答告知客户端
	outConn   *net.UDPConn // 面向真实目标地址的出口socket

	clientIP net.IP // TCP 控制连接的对端 IP：UDP 报文必须来自同一 IP

	clientMu sync.RWMutex
	client   *net.UDPAddr // 客户端UDP源地址，由第一条报文确定后锁定

	idleTimeout time.Duration
	lastActive  atomic.Int64  // 最近一次收发时间（UnixNano）；用 atomic.Int64 保证 32 位平台对齐
	sentPackets atomic.Uint64 // 转发到目标的报文数
	recvPackets atomic.Uint64 // 回送给客户端的报文数

	// 已发送过报文的目标地址集合（key 为 "ip:port"）：
	// 只接受这些来源的回包，避免任意主机借中继端口向客户端注入数据
	targetsMu sync.RWMutex
	targets   map[string]struct{}
}

// handleUDPAssociate 处理SOCKS5 UDP ASSOCIATE请求
// 建立UDP中继并把中继地址回给客户端，关联生命周期与TCP控制连接一致
// established 在中继建立完成（应答已发出）后调用，提前归还限流名额（见 serveConn）
func (s *ProxyServer) handleUDPAssociate(conn net.Conn, req socks5Request, established func()) {
	if !s.config.EnableUDP {
		log.Printf("UDP ASSOCIATE 已被禁用，拒绝来自 %s 的请求", conn.RemoteAddr())
		_ = sendSocks5Response(conn, repCommandNotSupport)
		return
	}

	// UDP 关联建立后不再占用握手限流名额，因此需要独立上限：
	// 每个关联占 2 个 socket + 3 个协程，不设上限时可被大量创建直到 fd 耗尽
	select {
	case s.udpLimiter <- struct{}{}:
		defer func() { <-s.udpLimiter }()
	default:
		log.Printf("UDP 关联数已达上限，拒绝来自 %s 的请求", conn.RemoteAddr())
		_ = sendSocks5Response(conn, repGeneralFailure)
		return
	}

	// 中继socket需要绑定到一个客户端可达的本机地址：
	// 取TCP控制连接的本机地址，保证与客户端配置的代理地址一致
	tcpLocal, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok || tcpLocal.IP == nil || tcpLocal.IP.IsUnspecified() {
		log.Printf("无法确定UDP中继的绑定地址: %v", conn.LocalAddr())
		_ = sendSocks5Response(conn, repGeneralFailure)
		return
	}

	// RFC 1928 要求 UDP 关联只服务发起该关联的客户端：
	// 记录 TCP 对端 IP，后续 UDP 报文必须来自同一 IP（端口由首个报文确定）
	tcpRemote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || tcpRemote.IP == nil {
		log.Printf("无法确定UDP客户端的源地址: %v", conn.RemoteAddr())
		_ = sendSocks5Response(conn, repGeneralFailure)
		return
	}

	// 1) 中继socket：绑定本机IP，端口由系统分配
	relayConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: tcpLocal.IP, Port: 0})
	if err != nil {
		log.Printf("UDP中继socket创建失败: %v", err)
		_ = sendSocks5Response(conn, repGeneralFailure)
		return
	}
	defer relayConn.Close()

	// 2) 出口socket：用于与真实目标通信，使用通配地址以兼容IPv4/IPv6目标
	outConn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		log.Printf("UDP出口socket创建失败: %v", err)
		_ = sendSocks5Response(conn, repGeneralFailure)
		return
	}
	defer outConn.Close()

	// 3) 把UDP中继地址通过 BND.ADDR/BND.PORT 回给客户端
	bound := relayConn.LocalAddr().(*net.UDPAddr)
	if err := sendSocks5Reply(conn, repSucceeded, bound.IP, bound.Port); err != nil {
		log.Printf("UDP中继地址应答失败: %v", err)
		return
	}

	a := &udpAssociation{
		server:      s,
		relayConn:   relayConn,
		outConn:     outConn,
		clientIP:    tcpRemote.IP,
		idleTimeout: time.Duration(s.config.UDPIdleTimeout) * time.Second,
		targets:     make(map[string]struct{}),
	}
	a.lastActive.Store(time.Now().UnixNano())

	log.Printf("UDP ASSOCIATE 已建立: 客户端=%s 中继=%s 声明目标=%s",
		conn.RemoteAddr(), bound, req.Addr())

	done := make(chan struct{})
	var once sync.Once
	closeAssoc := func() { once.Do(func() { close(done) }) }

	// RFC 1928：TCP控制连接断开时，UDP关联随之失效
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				closeAssoc()
				return
			}
		}
	}()

	// 空闲回收，避免客户端异常退出导致socket与协程泄漏
	if a.idleTimeout > 0 {
		go a.watchIdle(done, closeAssoc)
	}

	// 任一转发循环退出（socket 被关闭）时关闭关联，
	// 这样即使 udpIdle 为 0（不启用空闲回收），主协程也不会永久阻塞在 <-done
	go func() {
		a.clientToTarget(done)
		closeAssoc()
	}()
	go func() {
		a.targetToClient(done)
		closeAssoc()
	}()

	// UDP中继已就绪：归还限流名额，关联存续期间不再占用并发上限（幂等）
	established()

	// 阻塞至关联结束；返回后 defer 会关闭两个socket，转发协程随即退出
	<-done

	log.Printf("UDP ASSOCIATE 已注销: 客户端=%s 中继=%s 上行=%d包 下行=%d包",
		conn.RemoteAddr(), bound,
		a.sentPackets.Load(), a.recvPackets.Load())
}

// watchIdle 周期性检查关联空闲时间，超时后关闭关联
func (a *udpAssociation) watchIdle(done <-chan struct{}, closeAssoc func()) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			last := time.Unix(0, a.lastActive.Load())
			if time.Since(last) > a.idleTimeout {
				log.Printf("UDP关联空闲超时(%v)，准备注销: 客户端=%s",
					a.idleTimeout, a.relayConn.LocalAddr())
				closeAssoc()
				return
			}
		}
	}
}

// clientToTarget 把客户端发来的UDP报文解封装后转发到目标地址
func (a *udpAssociation) clientToTarget(done <-chan struct{}) {
	buf := make([]byte, maxUDPDatagram)

	for {
		n, src, err := a.relayConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 单次读错误不应永久终止转发（Windows 上 ICMP 端口不可达等
			// 可能表现为读错误），退避后继续
			log.Printf("UDP中继读取失败: %v", err)
			time.Sleep(udpReadRetryDelay)
			continue
		}

		select {
		case <-done:
			return
		default:
		}

		// RFC 1928：必须丢弃来自客户端以外的报文
		if !a.acceptClient(src) {
			continue
		}
		a.lastActive.Store(time.Now().UnixNano())

		host, port, payload, err := parseUDPDatagram(buf[:n])
		if err != nil || host == "" {
			continue
		}

		// 域名目标走带缓存的解析；UDP出口为本机直连，故使用直连DNS
		dst, err := net.ResolveUDPAddr("udp", a.server.resolve(net.JoinHostPort(host, strconv.Itoa(port))))
		if err != nil {
			continue
		}

		if _, err := a.outConn.WriteToUDP(payload, dst); err == nil {
			// 记录目标地址，作为后续回包的来源白名单
			a.recordTarget(dst)
			a.sentPackets.Add(1)
		}
	}
}

// targetToClient 把目标返回的UDP报文封装后回送给客户端
func (a *udpAssociation) targetToClient(done <-chan struct{}) {
	buf := make([]byte, maxUDPDatagram)

	for {
		n, src, err := a.outConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("UDP出口读取失败: %v", err)
			time.Sleep(udpReadRetryDelay)
			continue
		}

		select {
		case <-done:
			return
		default:
		}

		client := a.clientAddr()
		if client == nil {
			// 客户端地址尚未确定，无从回送，丢弃
			continue
		}

		// 只转发来自"本关联确实发过报文的目标"的响应：
		// 否则任意主机只要猜到随机中继端口，就能向客户端注入伪造数据
		if !a.acceptTarget(src) {
			continue
		}
		a.lastActive.Store(time.Now().UnixNano())

		packet, err := buildUDPDatagram(src, buf[:n])
		if err != nil {
			continue
		}

		if _, err := a.relayConn.WriteToUDP(packet, client); err == nil {
			a.recvPackets.Add(1)
		}
	}
}

// acceptClient 记录并校验客户端UDP源地址
// 来源 IP 必须与 TCP 控制连接的对端一致；端口在首个报文到达时锁定，
// 后续其他来源一律拒绝（防止同网段主机抢注关联或把中继当作反射器）
func (a *udpAssociation) acceptClient(src *net.UDPAddr) bool {
	if !src.IP.Equal(a.clientIP) {
		return false
	}

	a.clientMu.RLock()
	bound := a.client
	a.clientMu.RUnlock()

	if bound != nil {
		return bound.Port == src.Port
	}

	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if a.client == nil {
		cp := *src
		a.client = &cp
		log.Printf("UDP关联绑定客户端地址: %s", cp.String())
	}
	return a.client.Port == src.Port
}

// clientAddr 返回已绑定的客户端地址（可能为 nil）
func (a *udpAssociation) clientAddr() *net.UDPAddr {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.client
}

// recordTarget 记录一个已发送过报文的目标地址（用于过滤回包来源）
func (a *udpAssociation) recordTarget(dst *net.UDPAddr) {
	if dst == nil {
		return
	}
	key := dst.String()

	a.targetsMu.Lock()
	if len(a.targets) < maxUDPTargetsPerAssoc {
		a.targets[key] = struct{}{}
	}
	a.targetsMu.Unlock()
}

// acceptTarget 判断回包来源是否为已发送过报文的目标
func (a *udpAssociation) acceptTarget(src *net.UDPAddr) bool {
	if src == nil {
		return false
	}
	key := src.String()

	a.targetsMu.RLock()
	_, ok := a.targets[key]
	a.targetsMu.RUnlock()
	return ok
}

// parseUDPDatagram 解析SOCKS5 UDP请求报文
// 格式：RSV(2) FRAG(1) ATYP(1) DST.ADDR(变长) DST.PORT(2) DATA
func parseUDPDatagram(b []byte) (host string, port int, payload []byte, err error) {
	if len(b) < socks5UDPHeaderLen+1 {
		return "", 0, nil, errShortPacket
	}
	// 本代理不支持分片，按 RFC 1928 丢弃 FRAG != 0 的报文
	if b[2] != 0x00 {
		return "", 0, nil, errUDPFragmentUnsupport
	}

	host, port, consumed, err := readSocks5Addr(b[socks5UDPHeaderLen:])
	if err != nil {
		return "", 0, nil, err
	}

	return host, port, b[socks5UDPHeaderLen+consumed:], nil
}

// buildUDPDatagram 按SOCKS5 UDP报文格式封装来自目标地址的数据
func buildUDPDatagram(src *net.UDPAddr, payload []byte) ([]byte, error) {
	// RSV(2) + FRAG(1) + ATYP/ADDR/PORT(最长19) + DATA
	out := make([]byte, 0, socks5UDPHeaderLen+19+len(payload))
	out = append(out, 0x00, 0x00, 0x00)

	if ip4 := src.IP.To4(); ip4 != nil {
		out = append(out, atypIPv4)
		out = append(out, ip4...)
	} else if ip16 := src.IP.To16(); ip16 != nil {
		out = append(out, atypIPv6)
		out = append(out, ip16...)
	} else {
		return nil, errCannotEncodeAddr
	}

	out = binary.BigEndian.AppendUint16(out, uint16(src.Port))
	out = append(out, payload...)

	return out, nil
}
