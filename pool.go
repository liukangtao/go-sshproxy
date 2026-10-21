// pool.go 实现SSH连接池管理功能
// 负责SSH连接的创建、复用、健康检查和清理
package main

import (
	"context" // 用于给建连/排队设置超时，避免过载时长时间占住调用方
	"errors"  // 用于池关闭等错误定义
	"log"
	"net" // 用于带 context 的 TCP 建连
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// maxConcurrentDials 并发SSH建连上限
// 高并发峰值时限制同时在途的握手数量，避免对SSH服务器形成建连风暴
const maxConcurrentDials = 10

// maxHealthCheckConcurrency 健康检查的并发探测上限
// 并发执行使整轮检查耗时不受单个僵死连接（最长 keepaliveTimeout）串行累加拖长
const maxHealthCheckConcurrency = 8

// keepaliveTimeout keepalive 探测超时
// 服务器僵死（TCP未断但不响应）时避免健康检查长时间阻塞
const keepaliveTimeout = 5 * time.Second

// errPoolClosed 连接池已关闭
var errPoolClosed = errors.New("SSH连接池已关闭")

// errKeepaliveTimeout keepalive 探测超时（连接疑似僵死）
var errKeepaliveTimeout = errors.New("keepalive探测超时")

// SSHConnectionPool SSH连接池结构
// 用于管理SSH连接的复用，包含连接通道、配置信息和状态管理
type SSHConnectionPool struct {
	connections chan *ssh.Client  // 连接通道，存储空闲连接
	config      *ssh.ClientConfig // SSH客户端配置
	host        string            // SSH主机地址
	port        string            // SSH端口
	maxSize     int               // 池内最大空闲连接数

	lastUsedMutex sync.RWMutex                      // 保护lastUsed与healthStatus
	lastUsed      map[*ssh.Client]time.Time         // 记录连接最后使用时间
	healthStatus  map[*ssh.Client]*ConnectionHealth // 记录连接健康状态

	dialSem chan struct{} // 限制并发建连的信号量
	closed  atomic.Bool   // 池是否已关闭
	done    chan struct{} // 关闭信号，通知后台协程退出
}

// NewSSHConnectionPool 创建新的SSH连接池
// 初始化连接池并启动定期清理空闲连接、健康检查的goroutine
func NewSSHConnectionPool(config *ssh.ClientConfig, host, port string, maxSize int) *SSHConnectionPool {
	// 创建连接池实例
	pool := &SSHConnectionPool{
		connections:  make(chan *ssh.Client, maxSize),         // 创建指定大小的连接通道
		config:       config,                                  // SSH配置
		host:         host,                                    // SSH主机
		port:         port,                                    // SSH端口
		maxSize:      maxSize,                                 // 最大空闲连接数
		lastUsed:     make(map[*ssh.Client]time.Time),         // 初始化最后使用时间映射
		healthStatus: make(map[*ssh.Client]*ConnectionHealth), // 初始化健康状态映射
		dialSem:      make(chan struct{}, maxConcurrentDials), // 并发建连信号量
		done:         make(chan struct{}),                     // 关闭信号
	}

	// 启动定期清理空闲连接的goroutine
	go pool.cleanupIdleConnections()

	// 启动定期健康检查的goroutine
	go pool.startHealthChecks()

	return pool
}

// Prewarm 预热连接池，提前创建一些连接
func (p *SSHConnectionPool) Prewarm(count int) {
	if count > p.maxSize {
		count = p.maxSize
	}
	log.Printf("正在预热 SSH 连接池，预创建 %d 个连接...", count)
	for i := 0; i < count; i++ {
		go func() {
			client, err := p.createConnection(context.Background())
			if err == nil {
				p.Put(client)
			}
		}()
	}
}

// Get 从连接池获取SSH连接
// 尝试从连接池获取空闲连接，如果连接无效或池为空则创建新连接。
// ctx 用于约束"池为空时的建连"：避免路由探测已超时、调用方却仍阻塞在
// 并发建连信号量或 30 秒 TCP/SSH 握手上。
func (p *SSHConnectionPool) Get(ctx context.Context) (*ssh.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 尝试从连接通道获取连接
	select {
	case conn := <-p.connections:
		// 检查连接是否仍然有效
		if conn != nil {
			// 更新连接最后使用时间，并读取健康状态
			// 在锁内复制健康状态信息，避免解锁后访问指针导致数据竞争
			p.lastUsedMutex.Lock()
			p.lastUsed[conn] = time.Now()
			health, exists := p.healthStatus[conn]

			// 如果存在健康状态记录且最近1分钟内检查过且健康，则直接返回
			var isHealthy bool
			if exists && time.Since(health.lastChecked) < 1*time.Minute && health.healthy {
				isHealthy = true
			}
			p.lastUsedMutex.Unlock()

			if isHealthy {
				return conn, nil
			}

			// 否则进行实时健康检查（带超时，避免僵死连接阻塞请求获取）
			if err := p.keepaliveCheck(conn); err != nil {
				// 连接无效或探测超时：释放它并新建
				// （连接刚从池中取出、无活跃隧道，关闭安全）
				p.forget(conn)
				return p.createConnection(ctx)
			}

			// 连接有效，更新健康状态
			p.lastUsedMutex.Lock()
			p.healthStatus[conn] = &ConnectionHealth{
				healthy:     true,
				lastChecked: time.Now(),
			}
			p.lastUsedMutex.Unlock()
			return conn, nil
		}
		// 连接无效或已关闭，创建新连接
		return p.createConnection(ctx)
	default:
		// 连接池为空，创建新连接
		return p.createConnection(ctx)
	}
}

// keepaliveCheck 带超时的 keepalive 探测
// 用途：Get 与健康检查共用，避免"服务器僵死（TCP未断但不响应）"时长时间阻塞。
// 说明：探测超时后，底层 SendRequest 仍会等待连接层报错后自行结束（有界），不再阻塞调用方
func (p *SSHConnectionPool) keepaliveCheck(conn *ssh.Client) error {
	result := make(chan error, 1)
	go func() {
		_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
		result <- err
	}()

	select {
	case err := <-result:
		return err
	case <-time.After(keepaliveTimeout):
		return errKeepaliveTimeout
	}
}

// Put 将SSH连接放回连接池
// 调用约定：调用方必须确保归还时该连接上已没有活跃隧道（见 routeResult 所有权约定），
// 否则池满释放会连带关闭正在传输的通道
func (p *SSHConnectionPool) Put(conn *ssh.Client) {
	// 检查连接是否为空
	if conn == nil {
		return
	}

	// 池已关闭：直接释放连接，避免泄漏
	if p.closed.Load() {
		p.forget(conn)
		return
	}

	// 更新连接最后使用时间
	p.lastUsedMutex.Lock()
	p.lastUsed[conn] = time.Now()
	// 初始化健康状态（假设放入池中的连接是健康的）
	if _, exists := p.healthStatus[conn]; !exists {
		p.healthStatus[conn] = &ConnectionHealth{
			healthy:     true,
			lastChecked: time.Now(),
		}
	}
	p.lastUsedMutex.Unlock()

	// 尝试将连接放回连接池
	select {
	case p.connections <- conn:
		// 二次确认：closeAll 可能在"检查 closed"与"写入 channel"之间完成排空，
		// 此时通道不会再被消费，连接必须由这里取出关闭，否则永久滞留（连接泄漏）
		if p.closed.Load() {
			for {
				select {
				case c := <-p.connections:
					if c != nil {
						p.forget(c)
					}
				default:
					return
				}
			}
		}
	default:
		// 连接池已满：释放这条空闲连接（调用方已保证其上无活跃隧道）
		p.forget(conn)
	}
}

// forget 从使用/健康记录中移除连接并关闭它
func (p *SSHConnectionPool) forget(conn *ssh.Client) {
	p.lastUsedMutex.Lock()
	delete(p.lastUsed, conn)
	delete(p.healthStatus, conn)
	p.lastUsedMutex.Unlock()
	conn.Close()
}

// createConnection 创建新的SSH连接
// 通过信号量限制并发建连数量，避免峰值时对SSH服务器形成冲击；
// ctx 为建连全过程（排队、TCP 握手、SSH 握手）设置上限。
func (p *SSHConnectionPool) createConnection(ctx context.Context) (*ssh.Client, error) {
	if p.closed.Load() {
		return nil, errPoolClosed
	}

	// 限制并发建连（等待中的调用方会被排队；连接很快，排队时间有限）
	select {
	case p.dialSem <- struct{}{}:
		defer func() { <-p.dialSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	addr := net.JoinHostPort(p.host, p.port)
	timeout := p.config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// 用 DialContext + NewClientConn 替代 ssh.Dial：后者只约束 TCP 建连，
	// 无法在路由探测超时或服务关闭时中止排队/握手
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("SSH连接建立失败(%s): %v", addr, err)
		}
		return nil, err
	}

	// SSH 握手同样受超时约束（服务器僵死时不会无限等待）
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := raw.SetDeadline(deadline); err != nil {
		raw.Close()
		return nil, err
	}

	c, chans, reqs, err := ssh.NewClientConn(raw, addr, p.config)
	if err != nil {
		raw.Close()
		if ctx.Err() == nil {
			// 记录失败原因（主机密钥不匹配、认证失败、网络不可达等）
			log.Printf("SSH 握手失败(%s): %v", addr, err)
		}
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})

	conn := ssh.NewClient(c, chans, reqs)

	// 记录新连接的最后使用时间和健康状态
	p.lastUsedMutex.Lock()
	p.lastUsed[conn] = time.Now()
	p.healthStatus[conn] = &ConnectionHealth{
		healthy:     true,
		lastChecked: time.Now(),
	}
	p.lastUsedMutex.Unlock()

	return conn, nil
}

// closeAll 关闭连接池并释放所有空闲连接
// 该实现不关闭 connections 通道（避免与并发 Put 竞争导致 send on closed channel panic），
// 通过 closed 标志让后续 Put 直接释放连接；可重复调用（幂等）
func (p *SSHConnectionPool) closeAll() {
	if p.closed.Swap(true) {
		return // 已关闭，避免重复 close(done) 导致 panic
	}
	close(p.done)

	// 释放池中所有空闲连接
	for {
		select {
		case conn := <-p.connections:
			if conn != nil {
				p.forget(conn)
			}
		default:
			return
		}
	}
}

// cleanupIdleConnections 定期清理空闲连接
// 启动定时器，定期检查并清理长时间未使用的空闲连接
func (p *SSHConnectionPool) cleanupIdleConnections() {
	// 创建定时器，每10分钟检查一次
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.cleanupIdleConnectionsOnce()
		}
	}
}

// cleanupIdleConnectionsOnce 清理一次空闲连接
// 注意：只处理池中的空闲连接；使用中的连接不在池中（见 Put 的调用约定），不会被误清理
func (p *SSHConnectionPool) cleanupIdleConnectionsOnce() {
	// 收集需要检查的连接数量（只检查当前在池中的连接，避免无限循环）
	numConns := len(p.connections)

	for i := 0; i < numConns; i++ {
		select {
		case conn := <-p.connections:
			p.lastUsedMutex.RLock()
			lastUsed, exists := p.lastUsed[conn]
			p.lastUsedMutex.RUnlock()

			if exists && time.Since(lastUsed) > 1*time.Hour {
				// 连接超时，关闭并清理
				p.forget(conn)
			} else {
				// 未超时，放回池中
				select {
				case p.connections <- conn:
				default:
					// 池满了（虽然这在当前逻辑下不太可能），关闭它
					p.forget(conn)
				}
			}
		default:
			// 通道已空
			return
		}
	}
}

// startHealthChecks 启动定期健康检查
// 定期检查所有连接（含使用中的连接）的健康状态
func (p *SSHConnectionPool) startHealthChecks() {
	// 创建定时器，每30秒检查一次
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.checkAllConnectionsHealth()
		}
	}
}

// checkAllConnectionsHealth 检查所有连接的健康状态
// 遍历所有已知连接并发送keepalive探测（并发执行并限制并发度：
// 单个僵死连接的探测超时不再串行累加拖长整轮检查周期）
func (p *SSHConnectionPool) checkAllConnectionsHealth() {
	// 创建一个临时的连接切片，避免长时间锁定
	p.lastUsedMutex.Lock()
	connections := make([]*ssh.Client, 0, len(p.lastUsed))
	for conn := range p.lastUsed {
		connections = append(connections, conn)
	}
	p.lastUsedMutex.Unlock()

	// 并发探测（信号量限制同时在途的探测数量）
	sem := make(chan struct{}, maxHealthCheckConcurrency)
	var wg sync.WaitGroup
	for _, conn := range connections {
		if conn == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(conn *ssh.Client) {
			defer wg.Done()
			defer func() { <-sem }()

			// 发送keepalive消息检查连接是否仍然有效（带超时保护）
			err := p.keepaliveCheck(conn)

			// 探测超时说明连接已僵死（TCP 未断但不响应）：
			// 底层 SendRequest 会一直阻塞在该连接上，只有关闭连接才能让它返回，
			// 否则每轮健康检查（30 秒）都会累积一个滞留的协程。
			// 僵死连接本身已无法传输数据，关闭不会造成可用性损失。
			if errors.Is(err, errKeepaliveTimeout) {
				log.Printf("健康检查超时，关闭僵死SSH连接(%s:%s)", p.host, p.port)
				p.forget(conn)
				return
			}

			// 其他错误（连接已断开等）仅标记状态、不关闭连接，
			// 避免误伤仍在传输的隧道（无效连接会在下次 Get 时被替换）
			p.lastUsedMutex.Lock()
			// 检查连接是否还在记录中（可能在检查期间已被清理）
			if _, exists := p.lastUsed[conn]; exists {
				if p.healthStatus[conn] == nil {
					p.healthStatus[conn] = &ConnectionHealth{}
				}
				p.healthStatus[conn].healthy = (err == nil)
				p.healthStatus[conn].lastChecked = time.Now()
			}
			p.lastUsedMutex.Unlock()
		}(conn)
	}
	wg.Wait()
}
