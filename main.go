package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Method int

const (
	SSH Method = iota
	Direct
)

// 定义targetinfo结构体
type targetInfo struct {
	method Method
	time   int64
}

var (
	routeCache     = make(map[string]*targetInfo) // 目标地址->最佳路由方式
	cacheMutex     sync.RWMutex
	sshUser        string
	sshHost        string
	sshPort        string
	socks5Port     int
	httpPort       int
	WriteLogToFile bool
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s \" --sshUser SSH用户 --sshHost SSH主机 --port ssh端口 --socks5 端口 --http 端口 -log <true|false>\")\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&sshUser, "sshUser", "", "SSH用户名")
	flag.StringVar(&sshHost, "sshHost", "", "SSH主机")
	flag.StringVar(&sshPort, "port", "22", "SSH端口")
	flag.IntVar(&socks5Port, "socks5", 0, "SOCKS5代理监听端口")
	flag.IntVar(&httpPort, "http", 0, "HTTP代理监听端口")
	flag.BoolVar(&WriteLogToFile, "log", false, "是否将日志写入文件")
}

func (m Method) String() string {
	switch m {
	case SSH:
		return "SSH"
	case Direct:
		return "Direct"
	default:
		return "Unknown"
	}
}

type routeResult struct {
	conn    net.Conn
	method  Method
	latency time.Duration
}

func main() {
	flag.Parse()

	if socks5Port == 0 && httpPort == 0 {
		log.Fatal("必须指定至少一个代理类型（--socks5 或 --http）")
	}

	go CheckTargetInfo()

	config := &ssh.ClientConfig{
		User:            sshUser,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	passwd := ""
	FileCount := 4
	for {
		FileCount--
		if FileCount == 0 {
			break
		}

		fmt.Print("Password: ")
		//从标准输入读取密码，存储到passwd变量中
		_, err := fmt.Scanln(&passwd)
		if err != nil {
			fmt.Println("Password is incorrect.")
			continue
		}
		break
	}

	_, err := os.Stat(passwd)
	if err != nil {
		fmt.Println("Password.")
		config.Auth = []ssh.AuthMethod{ssh.Password(passwd)}
	} else {
		fmt.Println("private key file.")
		// 加载私钥
		key, err := os.ReadFile(passwd)
		if err != nil {
			log.Fatalf("Failed to read private key: %v", err)
		}

		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			log.Fatalf("Failed to parse private key: %v", err)
		}

		config.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}

	// 连接SSH服务器
	sshClient, err := ssh.Dial("tcp", sshHost+":"+sshPort, config)
	if err != nil {
		log.Fatalf("Failed to dial SSH server: %v", err)
	}
	defer sshClient.Close()

	// 启动代理服务
	if socks5Port > 0 {
		go startSocks5Proxy(socks5Port, sshClient)
	}
	if httpPort > 0 {
		go startHttpProxy(httpPort, sshClient)
	}

	select {} // 保持主线程运行
}

func startSocks5Proxy(port int, client *ssh.Client) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", port, err)
	}
	defer listener.Close()

	log.Printf("SOCKS5 proxy started on :%d", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept failed: %v", err)
			continue
		}
		go handleConnection(conn, client)
	}
}

func startHttpProxy(port int, client *ssh.Client) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("HTTP代理启动失败: %v", err)
	}
	defer listener.Close()
	log.Printf("HTTP/HTTPS代理已启动 :%d", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("HTTP连接接受失败: %v", err)
			continue
		}
		go handleHTTPProxy(conn, client)
	}
}

func handleConnection(conn net.Conn, sshClient *ssh.Client) {
	defer conn.Close()
	var result routeResult
	var bneedUpdate bool = false

	if err := socks5Handshake(conn); err != nil {
		log.Printf("SOCKS5握手失败: %v", err)
		return
	}

	targetAddr, err := getTargetAddress(conn)
	if err != nil {
		log.Printf("获取目标地址失败: %v", err)
		return
	}

	// 获取缓存路由（如果存在）
	cacheMutex.RLock()
	cache, hasCache := routeCache[targetAddr]
	cacheMutex.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultChan := make(chan routeResult, 2)
	start := time.Now()

	// 优先使用缓存路由（如果存在）
	if hasCache {
		switch cache.method {
		case SSH:
			{
				result.conn, err = sshClient.Dial("tcp", targetAddr)
				if err != nil {
					bneedUpdate = true
				}
			}
		case Direct:
			{
				result.conn, err = net.Dial("tcp", targetAddr)
				if err != nil {
					bneedUpdate = true
				}
			}
		default:
			log.Printf("不支持的方法: %v", result.method)
			_ = sendSocks5Response(conn, 0x01)
			return
		}

		result.latency = time.Since(start)
		result.method = cache.method
	} else {
		bneedUpdate = true
	}

	if bneedUpdate {
		// SSH隧道连接
		go func() {
			remoteConn, err := sshClient.Dial("tcp", targetAddr)
			if err != nil {
				return
			}
			select {
			case resultChan <- routeResult{remoteConn, SSH, time.Since(start)}:
			case <-ctx.Done():
				remoteConn.Close()
			}
		}()

		// 直连
		go func() {
			localConn, err := net.Dial("tcp", targetAddr)
			if err != nil {
				return
			}
			select {
			case resultChan <- routeResult{localConn, Direct, time.Since(start)}:
			case <-ctx.Done():
				localConn.Close()
			}
		}()

		if hasCache {
			select {
			case result = <-resultChan:
				if result.method == cache.method {
					cancel()
				}
			case <-time.After(5000 * time.Millisecond):
			}
		}

		// 等待最快响应
		if result.conn == nil {
			select {
			case result = <-resultChan:
				cancel()
			case <-time.After(10 * time.Second):
				_ = sendSocks5Response(conn, 0x01)
				return
			}
		}

		// 更新路由缓存
		cacheMutex.Lock()
		routeCache[targetAddr] = &targetInfo{
			method: result.method,
			time:   time.Now().Unix(),
		}
		cacheMutex.Unlock()
	}

	defer result.conn.Close()

	_ = sendSocks5Response(conn, 0x00)

	// 记录日志
	go logRouteResult(targetAddr, result.method.String(), result.latency)

	// 数据转发
	go func() {
		io.Copy(result.conn, conn)
		result.conn.Close()
	}()
	io.Copy(conn, result.conn)
}

func socks5Handshake(conn net.Conn) error {
	buf := make([]byte, 256)

	// 读取版本和方法列表
	n, err := conn.Read(buf[:2])
	if err != nil || n != 2 {
		return errors.New("failed to read methods")
	}

	version := buf[0]
	nMethods := buf[1]

	if version != 0x05 {
		return errors.New("unsupported SOCKS version")
	}

	// 读取所有支持的方法
	n, err = conn.Read(buf[:nMethods])
	if err != nil || n != int(nMethods) {
		return errors.New("failed to read methods list")
	}

	// 选择无认证
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return errors.New("failed to write auth method")
	}

	return nil
}

func getTargetAddress(conn net.Conn) (string, error) {
	buf := make([]byte, 256)

	// 读取请求头
	n, err := conn.Read(buf[:4])
	if err != nil || n != 4 {
		return "", errors.New("failed to read request header")
	}

	version := buf[0]
	cmd := buf[1]
	atyp := buf[3]

	if version != 0x05 || cmd != 0x01 {
		return "", errors.New("unsupported command")
	}

	var host string
	switch atyp {
	case 0x01: // IPv4
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
	case 0x04: // IPv6
		n, err = conn.Read(buf[:16])
		if err != nil || n != 16 {
			return "", errors.New("failed to read IPv6")
		}
		host = net.IP(buf[:16]).String()
	default:
		return "", errors.New("unsupported address type")
	}

	// 读取端口
	n, err = conn.Read(buf[:2])
	if err != nil || n != 2 {
		return "", errors.New("failed to read port")
	}
	port := int(buf[0])<<8 | int(buf[1])

	return fmt.Sprintf("%s:%d", host, port), nil
}

func sendSocks5Response(conn net.Conn, reply byte) error {
	response := []byte{0x05, reply, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, err := conn.Write(response)
	return err
}

func logRouteResult(target, method string, latency time.Duration) {
	if !WriteLogToFile {
		return
	}
	f, err := os.OpenFile("route.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("日志文件错误: %v", err)
		return
	}
	defer f.Close()

	entry := fmt.Sprintf("[%s] 目标:%-80s 方式:%s\t 耗时:%v\t\n",
		time.Now().Format("2006-01-02 15:04:05"),
		target,
		method,
		latency)
	f.WriteString(entry)
}

// HTTP代理处理函数
func handleHTTPProxy(conn net.Conn, sshClient *ssh.Client) {
	defer conn.Close()
	var bNeedUpdate bool = false
	var result routeResult

	// 读取请求头
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("请求解析失败: %v", err)
		return
	}

	// 获取目标地址
	targetAddr := req.URL.Host
	if req.URL.Port() == "" {
		if req.URL.Scheme == "https" {
			targetAddr += ":443"
		} else {
			targetAddr += ":80"
		}
	}

	// 复用路由逻辑
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cacheMutex.RLock()
	cache, hasCache := routeCache[targetAddr]
	cacheMutex.RUnlock()

	start := time.Now()

	if hasCache {
		switch cache.method {
		case SSH:
			{
				// SSH隧道模式
				result.conn, err = sshClient.Dial("tcp", targetAddr)
				if err != nil {
					log.Printf("SSH隧道连接失败: %v", err)
					bNeedUpdate = true
					return
				}
			}
		case Direct:
			{
				// 直连模式
				result.conn, err = net.Dial("tcp", targetAddr)
				if err != nil {
					log.Printf("直连连接失败: %v", err)
					bNeedUpdate = true
					return
				}
			}
		default:
			{
				// 未知模式
				log.Printf("未知模式: %s", targetAddr)
				return
			}
		}

		result.latency = time.Since(start)
		result.method = cache.method
	} else {
		// 未命中缓存，进行路由
		bNeedUpdate = true
	}

	if bNeedUpdate {

		resultChan := make(chan routeResult, 2)

		// SSH隧道连接
		go func() {
			remoteConn, err := sshClient.Dial("tcp", targetAddr)
			if err != nil {
				return
			}
			select {
			case resultChan <- routeResult{remoteConn, SSH, time.Since(start)}:
			case <-ctx.Done():
				remoteConn.Close()
			}
		}()

		// 直连
		go func() {
			localConn, err := net.Dial("tcp", targetAddr)
			if err != nil {
				return
			}
			select {
			case resultChan <- routeResult{localConn, Direct, time.Since(start)}:
			case <-ctx.Done():
				localConn.Close()
			}
		}()

		select {
		case result = <-resultChan:
			cancel()
		case <-time.After(5 * time.Second):
			log.Printf("连接超时: %s", targetAddr)
			return
		}

		// 记录日志
		cacheMutex.Lock()
		routeCache[targetAddr] = &targetInfo{
			method: result.method,
			time:   time.Now().Unix(),
		}
		cacheMutex.Unlock()
	}

	defer result.conn.Close()

	// 转发请求
	if req.Method == "CONNECT" {
		// HTTPS隧道模式
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	} else {
		// 普通HTTP请求
		// 修正请求URL为相对路径
		req.URL.Scheme = "http"
		if req.URL.Port() == "443" {
			req.URL.Scheme = "https"
		}
		req.URL.Host = targetAddr
		req.RequestURI = ""

		// 移除代理相关头部
		req.Header.Del("Proxy-Connection")
		req.Header.Set("Connection", "Keep-Alive")

		// 发送修正后的请求
		err = req.Write(result.conn)
		if err != nil {
			log.Printf("发送请求失败: %v", err)
			return
		}
	}

	go logRouteResult(targetAddr, result.method.String(), result.latency)

	// 双向数据转发
	go func() {
		defer result.conn.Close()
		io.Copy(result.conn, conn)
	}()
	io.Copy(conn, result.conn)
}

func CheckTargetInfo() {
	for {
		time.Sleep(time.Minute)
		cacheMutex.Lock()
		for targetAddr, targetInfo := range routeCache {
			if time.Now().Unix()-targetInfo.time > 60*5 {
				delete(routeCache, targetAddr)
			}
		}
		cacheMutex.Unlock()
	}
}
