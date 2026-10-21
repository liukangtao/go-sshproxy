// socks5.go 实现SOCKS5协议处理功能
// 负责处理SOCKS5代理协议的握手、请求解析、协议应答与命令分发
package main

import (
	"bufio"           // 用于握手/请求解析的带缓冲读取
	"encoding/binary" // 用于解析/构造协议中的大端序字段
	"errors"          // 用于定义协议解析错误
	"fmt"             // 用于格式化错误信息
	"io"              // 用于精确读取固定长度字段
	"log"             // 用于记录日志
	"net"             // 用于网络I/O操作
	"strconv"         // 用于端口字符串转换
	"time"            // 用于握手超时控制
)

// SOCKS5 命令码（RFC 1928 第 4 节）
const (
	cmdConnect      = 0x01 // CONNECT：建立TCP转发
	cmdBind         = 0x02 // BIND：反向连接（本代理未实现）
	cmdUDPAssociate = 0x03 // UDP ASSOCIATE：建立UDP中继
)

// SOCKS5 应答码（RFC 1928 第 6 节）
const (
	repSucceeded          = 0x00 // 成功
	repGeneralFailure     = 0x01 // 通用失败
	repHostUnreachable    = 0x04 // 主机不可达
	repCommandNotSupport  = 0x07 // 命令不支持
	repAddrTypeNotSupport = 0x08 // 地址类型不支持
)

// SOCKS5 地址类型（ATYP）
const (
	atypIPv4   = 0x01 // IPv4地址
	atypDomain = 0x03 // 域名
	atypIPv6   = 0x04 // IPv6地址
)

// SOCKS5 UDP 报文固定前缀长度：RSV(2) + FRAG(1)
const socks5UDPHeaderLen = 3

// 协议解析错误
var (
	errShortPacket          = errors.New("报文长度不足")
	errUnsupportedAddrType  = errors.New("不支持的地址类型")
	errUDPFragmentUnsupport = errors.New("不支持UDP分片(FRAG != 0)")
	errCannotEncodeAddr     = errors.New("无法编码的地址")
)

// socks5Request SOCKS5请求报文
type socks5Request struct {
	Cmd  byte   // 命令码
	Host string // 目标主机（IP字符串或域名）
	Port int    // 目标端口
}

// Addr 返回 host:port 形式的目标地址
func (r socks5Request) Addr() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

// handleConnection 处理SOCKS5连接
// 完成握手后按命令码分发：CONNECT走TCP转发，UDP ASSOCIATE走UDP中继，
// 其余命令按 RFC 1928 回送错误码后再关闭连接
// established 在进入数据转发/UDP中继阶段时调用，提前归还限流名额（见 serveConn）
func (s *ProxyServer) handleConnection(conn net.Conn, established func()) {
	defer conn.Close()

	// 握手与请求解析阶段设置超时：防止客户端连上后长时间不发送数据占用资源；
	// 请求解析完成后清除，避免影响数据转发阶段的长连接
	conn.SetDeadline(time.Now().Add(time.Duration(s.config.HandshakeTimeout) * time.Second))

	// 握手与请求解析使用带缓冲读取：合并多次小读取的系统调用；
	// 其中可能预读的"提前到达"负载数据由 handleConnect 在转发前补发，不会丢失
	reader := bufio.NewReader(conn)

	if err := socks5Handshake(reader, conn); err != nil {
		log.Printf("SOCKS5握手失败: %v", err)
		return
	}

	req, reply, err := readSocks5Request(reader)
	if err != nil {
		// reply 非 0 时必须回送错误码：否则客户端只能感知“连接被断开”，
		// 无法区分“命令不支持”与“网络故障”，其降级/重试逻辑会失效
		if reply != 0 {
			_ = sendSocks5Response(conn, reply)
		}
		log.Printf("获取目标地址失败: %v", err)
		return
	}

	// 进入数据转发/UDP关联阶段：清除握手阶段的超时
	conn.SetDeadline(time.Time{})

	switch req.Cmd {
	case cmdConnect:
		s.handleConnect(conn, reader, req.Addr(), established)
	case cmdUDPAssociate:
		s.handleUDPAssociate(conn, req, established)
	case cmdBind:
		// BIND 命令本代理未实现，按规范告知客户端，使其能走降级分支
		log.Printf("不支持的SOCKS5命令 BIND(0x02): %s", req.Addr())
		_ = sendSocks5Response(conn, repCommandNotSupport)
	default:
		log.Printf("不支持的SOCKS5命令 0x%02x: %s", req.Cmd, req.Addr())
		_ = sendSocks5Response(conn, repCommandNotSupport)
	}
}

// handleConnect 处理SOCKS5 CONNECT(0x01)请求：选择路由并双向转发TCP数据
// reader 为握手/请求解析所用的缓冲读取器：转发前需补发其中可能预读的负载数据
func (s *ProxyServer) handleConnect(conn net.Conn, reader *bufio.Reader, targetAddr string, established func()) {
	result := s.selectRoute(targetAddr)
	if result.conn == nil {
		_ = sendSocks5Response(conn, repGeneralFailure)
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

	if err := sendSocks5Response(conn, repSucceeded); err != nil {
		log.Printf("发送SOCKS5响应失败: %v", err)
		return
	}

	// 补发缓冲中已读入但尚未转发的字节：
	// 流水线客户端可能在收到成功响应前就开始发送负载数据，这些字节可能已被 bufio
	// 预读，直接 relay 原始连接会丢失它们（与 http.go 的 early data 处理一致）
	if n := reader.Buffered(); n > 0 {
		buffered := make([]byte, n)
		if _, rerr := io.ReadFull(reader, buffered); rerr == nil {
			if _, werr := result.conn.Write(buffered); werr != nil {
				log.Printf("转发预读数据失败: %v", werr)
				return
			}
		}
	}

	go s.logRouteResult(targetAddr, result.method.String(), result.latency)

	// 已进入数据转发阶段：归还限流名额，长连接不再占用并发上限（幂等）
	established()

	done := make(chan struct{}, 2)
	go s.relay(result.conn, conn, done)
	go s.relay(conn, result.conn, done)

	// 等待两个方向都完成：某一方向先结束时只对其做半关闭（CloseWrite），
	// 反方向仍可继续传输，支持"客户端发完关写端等响应"等半关闭场景
	<-done
	<-done
}

// socks5Handshake SOCKS5握手过程
// 与客户端协商认证方式，本代理仅支持“无认证(0x00)”
// r 通常为 bufio 包装的读取器（合并小读取的系统调用），w 为原始连接（写响应）
func socks5Handshake(r io.Reader, w io.Writer) error {
	// 读取版本与方法数量（使用 ReadFull 避免TCP短读导致误判）
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return fmt.Errorf("读取版本与方法数量失败: %w", err)
	}

	if head[0] != 0x05 {
		return fmt.Errorf("不支持的SOCKS版本: 0x%02x", head[0])
	}

	nMethods := int(head[1])
	if nMethods == 0 {
		_, _ = w.Write([]byte{0x05, 0xFF})
		return errors.New("客户端未提供任何认证方法")
	}

	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(r, methods); err != nil {
		return fmt.Errorf("读取认证方法列表失败: %w", err)
	}

	// 只有当客户端显式提供“无认证”时才能选中它，否则按规范回 0xFF
	if !containsByte(methods, 0x00) {
		_, _ = w.Write([]byte{0x05, 0xFF})
		return errors.New("客户端未提供无认证方式(0x00)")
	}

	if _, err := w.Write([]byte{0x05, 0x00}); err != nil {
		return fmt.Errorf("发送认证方法失败: %w", err)
	}

	return nil
}

// readSocks5Request 读取并解析SOCKS5请求报文
// 返回值 reply 非 0 时表示需要把该应答码回送给客户端后再关闭连接
func readSocks5Request(r io.Reader) (req socks5Request, reply byte, err error) {
	// 版本(1) + 命令(1) + 保留(1) + 地址类型(1)
	head := make([]byte, 4)
	if _, err = io.ReadFull(r, head); err != nil {
		return req, 0, fmt.Errorf("读取请求头失败: %w", err)
	}
	if head[0] != 0x05 {
		return req, 0, fmt.Errorf("不支持的SOCKS版本: 0x%02x", head[0])
	}
	req.Cmd = head[1]

	// ATYP 已包含在上面读取的 4 字节请求头中(head[3])，此处不可再读：
	// 多读一字节会把目标地址的首字节误判为地址类型，导致整个请求解析错位，
	// 从而对域名及绝大多数 IPv4 请求错误回送 0x08(地址类型不支持)
	atyp := head[3]
	hostLen := 0
	switch atyp {
	case atypIPv4:
		hostLen = net.IPv4len
	case atypIPv6:
		hostLen = net.IPv6len
	case atypDomain:
		lenBuf := make([]byte, 1)
		if _, err = io.ReadFull(r, lenBuf); err != nil {
			return req, repGeneralFailure, fmt.Errorf("读取域名长度失败: %w", err)
		}
		hostLen = int(lenBuf[0])
	default:
		return req, repAddrTypeNotSupport, fmt.Errorf("不支持的地址类型: 0x%02x", atyp)
	}

	// 补齐 ATYP + ADDR + PORT 原始字节后统一解析
	// 注意：域名格式为 ATYP+LEN+DOMAIN+PORT，比 IP 多一个“长度字节”，
	// 必须计入缓冲区长度并回填，否则 readSocks5Addr 会因字节数不足而解析失败
	lenByte := 0
	if atyp == atypDomain {
		lenByte = 1
	}
	raw := make([]byte, 1+lenByte+hostLen+2)
	raw[0] = atyp
	if lenByte == 1 {
		raw[1] = byte(hostLen)
	}
	if _, err = io.ReadFull(r, raw[1+lenByte:]); err != nil {
		return req, repGeneralFailure, fmt.Errorf("读取目标地址失败: %w", err)
	}

	host, port, _, err := readSocks5Addr(raw)
	if err != nil {
		return req, repGeneralFailure, err
	}

	req.Host = host
	req.Port = port
	return req, 0, nil
}

// readSocks5Addr 从 b 的起始位置解析 ATYP + ADDR + PORT
// 返回主机、端口以及本次消费的字节数（用于解析UDP报文中的变长地址）
func readSocks5Addr(b []byte) (host string, port int, consumed int, err error) {
	if len(b) < 1 {
		return "", 0, 0, errShortPacket
	}

	switch b[0] {
	case atypIPv4:
		if len(b) < 1+net.IPv4len+2 {
			return "", 0, 0, errShortPacket
		}
		host = net.IP(b[1 : 1+net.IPv4len]).String()
		port = int(binary.BigEndian.Uint16(b[1+net.IPv4len : 1+net.IPv4len+2]))
		return host, port, 1 + net.IPv4len + 2, nil

	case atypIPv6:
		if len(b) < 1+net.IPv6len+2 {
			return "", 0, 0, errShortPacket
		}
		host = net.IP(b[1 : 1+net.IPv6len]).String()
		port = int(binary.BigEndian.Uint16(b[1+net.IPv6len : 1+net.IPv6len+2]))
		return host, port, 1 + net.IPv6len + 2, nil

	case atypDomain:
		if len(b) < 2 {
			return "", 0, 0, errShortPacket
		}
		dlen := int(b[1])
		if len(b) < 2+dlen+2 {
			return "", 0, 0, errShortPacket
		}
		host = string(b[2 : 2+dlen])
		port = int(binary.BigEndian.Uint16(b[2+dlen : 4+dlen]))
		return host, port, 4 + dlen, nil

	default:
		return "", 0, 0, errUnsupportedAddrType
	}
}

// sendSocks5Response 发送SOCKS5应答（BND.ADDR/BND.PORT 均为 0）
func sendSocks5Response(conn net.Conn, reply byte) error {
	return sendSocks5Reply(conn, reply, nil, 0)
}

// sendSocks5Reply 按 RFC 1928 构造并发送SOCKS5应答
// UDP ASSOCIATE 成功时需要通过 ip/port 告知客户端UDP中继地址
func sendSocks5Reply(conn net.Conn, reply byte, ip net.IP, port int) error {
	addr, err := buildSocks5Addr(ip, port)
	if err != nil {
		return err
	}

	// VER(1) + REP(1) + RSV(1) + ATYP/ADDR/PORT(变长)
	buf := make([]byte, 0, 3+len(addr))
	buf = append(buf, 0x05, reply, 0x00)
	buf = append(buf, addr...)

	_, err = conn.Write(buf)
	return err
}

// buildSocks5Addr 按地址类型编码 ATYP + ADDR + PORT；ip 为 nil 时编码为 0.0.0.0:port
func buildSocks5Addr(ip net.IP, port int) ([]byte, error) {
	var out []byte

	switch {
	case ip == nil:
		out = append(out, atypIPv4, 0, 0, 0, 0)
	case ip.To4() != nil:
		out = append(out, atypIPv4)
		out = append(out, ip.To4()...)
	case ip.To16() != nil:
		out = append(out, atypIPv6)
		out = append(out, ip.To16()...)
	default:
		return nil, errCannotEncodeAddr
	}

	return binary.BigEndian.AppendUint16(out, uint16(port)), nil
}

// containsByte 判断字节切片中是否包含目标字节
func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}
