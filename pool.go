// pool.go 实现SSH连接池管理功能
// 负责SSH连接的创建、复用、健康检查和清理
package main

import (
	"sync"    // 用于同步原语，如互斥锁
	"time"    // 用于时间处理

	"golang.org/x/crypto/ssh" // 用于SSH客户端连接
)

// SSHConnectionPool SSH连接池结构
// 用于管理SSH连接的复用，包含连接通道、配置信息和状态管理
type SSHConnectionPool struct {
	connections   chan *ssh.Client             // 连接通道，用于存储可用的SSH连接
	config        *ssh.ClientConfig            // SSH客户端配置
	host          string                       // SSH主机地址
	port          string                       // SSH端口
	maxSize       int                          // 连接池最大大小
	mu            sync.Mutex                   // 互斥锁，保护连接池状态
	lastUsed      map[*ssh.Client]time.Time    // 记录连接最后使用时间
	healthStatus  map[*ssh.Client]*ConnectionHealth // 记录连接健康状态
	lastUsedMutex sync.RWMutex                 // 保护lastUsed的读写锁
}

// NewSSHConnectionPool 创建新的SSH连接池
// 初始化连接池并启动定期清理空闲连接的goroutine
func NewSSHConnectionPool(config *ssh.ClientConfig, host, port string, maxSize int) *SSHConnectionPool {
	// 创建连接池实例
	pool := &SSHConnectionPool{
		connections:  make(chan *ssh.Client, maxSize), // 创建指定大小的连接通道
		config:       config,                          // SSH配置
		host:         host,                            // SSH主机
		port:         port,                            // SSH端口
		maxSize:      maxSize,                         // 最大连接数
		lastUsed:     make(map[*ssh.Client]time.Time), // 初始化最后使用时间映射
		healthStatus: make(map[*ssh.Client]*ConnectionHealth), // 初始化健康状态映射
	}

	// 启动定期清理空闲连接的goroutine
	// 定期检查并关闭长时间未使用的连接
	go pool.cleanupIdleConnections()
	
	// 启动定期健康检查的goroutine
	// 定期检查连接池中连接的健康状态
	go pool.startHealthChecks()

	return pool
}

// Get 从连接池获取SSH连接
// 尝试从连接池获取现有连接，如果连接无效或池为空则创建新连接
func (p *SSHConnectionPool) Get() (*ssh.Client, error) {
	// 尝试从连接通道获取连接
	select {
	case conn := <-p.connections:
		// 检查连接是否仍然有效
		if conn != nil {
			// 更新连接最后使用时间
			p.lastUsedMutex.Lock()
			p.lastUsed[conn] = time.Now()
			p.lastUsedMutex.Unlock()

			// 检查连接健康状态（如果超过1分钟未检查，则认为需要重新检查）
			p.lastUsedMutex.Lock()
			health, exists := p.healthStatus[conn]
			p.lastUsedMutex.Unlock()
			
			// 如果存在健康状态记录且最近1分钟内检查过且健康，则直接返回
			if exists && time.Since(health.lastChecked) < 1*time.Minute && health.healthy {
				return conn, nil
			}

			// 否则进行实时健康检查
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			if err == nil {
				// 连接有效，更新健康状态
				p.lastUsedMutex.Lock()
				p.healthStatus[conn] = &ConnectionHealth{
					healthy:     true,
					lastChecked: time.Now(),
				}
				p.lastUsedMutex.Unlock()
				return conn, nil
			}
			
			// 连接无效，关闭它并更新健康状态
			p.lastUsedMutex.Lock()
			delete(p.lastUsed, conn)
			delete(p.healthStatus, conn)
			p.lastUsedMutex.Unlock()
			conn.Close()
		}
		// 连接无效或已关闭，创建新连接
		return p.createConnection()
	default:
		// 连接池为空，创建新连接
		return p.createConnection()
	}
}

// Put 将SSH连接放回连接池
// 将使用完的连接放回连接池，如果池已满则关闭连接
func (p *SSHConnectionPool) Put(conn *ssh.Client) {
	// 检查连接是否为空
	if conn == nil {
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
		// 成功放回连接池
	default:
		// 连接池已满，关闭连接
		p.lastUsedMutex.Lock()
		delete(p.lastUsed, conn)
		delete(p.healthStatus, conn)
		p.lastUsedMutex.Unlock()
		conn.Close()
	}
}

// createConnection 创建新的SSH连接
// 根据配置信息创建新的SSH连接
func (p *SSHConnectionPool) createConnection() (*ssh.Client, error) {
	// 建立SSH连接
	conn, err := ssh.Dial("tcp", p.host+":"+p.port, p.config)
	if err != nil {
		return nil, err
	}

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

// closeAll 关闭连接池中的所有连接
// 关闭连接池中的所有连接并清理状态
func (p *SSHConnectionPool) closeAll() {
	// 关闭连接通道
	close(p.connections)
	// 关闭所有连接
	for conn := range p.connections {
		if conn != nil {
			p.lastUsedMutex.Lock()
			delete(p.lastUsed, conn)
			delete(p.healthStatus, conn)
			p.lastUsedMutex.Unlock()
			conn.Close()
		}
	}

	// 清理lastUsed映射
	p.lastUsedMutex.Lock()
	p.lastUsed = make(map[*ssh.Client]time.Time)
	p.healthStatus = make(map[*ssh.Client]*ConnectionHealth)
	p.lastUsedMutex.Unlock()
}

// cleanupIdleConnections 定期清理空闲连接
// 启动定时器，定期检查并清理空闲连接
func (p *SSHConnectionPool) cleanupIdleConnections() {
	// 创建定时器，每10分钟检查一次
	ticker := time.NewTicker(10 * time.Minute) // 每10分钟检查一次
	defer ticker.Stop()

	// 循环检查空闲连接
	for {
		<-ticker.C
		p.cleanupIdleConnectionsOnce()
	}
}

// cleanupIdleConnectionsOnce 清理一次空闲连接
// 执行一次空闲连接清理操作
func (p *SSHConnectionPool) cleanupIdleConnectionsOnce() {
	// 锁定lastUsed映射
	p.lastUsedMutex.Lock()
	defer p.lastUsedMutex.Unlock()

	// 获取当前时间
	now := time.Now()
	// 遍历所有连接，检查是否超时
	for conn, lastUsed := range p.lastUsed {
		// 如果连接1小时未使用，则关闭它
		if now.Sub(lastUsed) > 1*time.Hour {
			// 从lastUsed中移除
			delete(p.lastUsed, conn)
			delete(p.healthStatus, conn)

			// 尝试从连接池中获取该连接并关闭
			select {
			case c := <-p.connections:
				if c == conn {
					c.Close()
				} else {
					// 如果不是目标连接，放回去
					select {
					case p.connections <- c:
					default:
						// 连接池满，关闭连接
						c.Close()
					}
				}
			default:
				// 连接不在池中，直接关闭
				conn.Close()
			}
		}
	}
}

// startHealthChecks 启动定期健康检查
// 定期检查连接池中所有连接的健康状态
func (p *SSHConnectionPool) startHealthChecks() {
	// 创建定时器，每30秒检查一次
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		<-ticker.C
		p.checkAllConnectionsHealth()
	}
}

// checkAllConnectionsHealth 检查所有连接的健康状态
// 遍历连接池中的所有连接并检查其健康状态
func (p *SSHConnectionPool) checkAllConnectionsHealth() {
	// 创建一个临时的连接切片，避免长时间锁定
	p.lastUsedMutex.Lock()
	connections := make([]*ssh.Client, 0, len(p.lastUsed))
	for conn := range p.lastUsed {
		connections = append(connections, conn)
	}
	p.lastUsedMutex.Unlock()

	// 检查每个连接的健康状态
	for _, conn := range connections {
		// 发送keepalive消息检查连接是否仍然有效
		_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
		
		// 更新健康状态
		p.lastUsedMutex.Lock()
		if p.healthStatus[conn] == nil {
			p.healthStatus[conn] = &ConnectionHealth{}
		}
		p.healthStatus[conn].healthy = (err == nil)
		p.healthStatus[conn].lastChecked = time.Now()
		p.lastUsedMutex.Unlock()
	}
}