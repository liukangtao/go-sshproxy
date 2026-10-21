// types.go 定义项目中使用的数据结构和类型
// 包含枚举类型、结构体定义和全局变量声明
package main

import (
	"bufio"       // 用于路由日志的带缓冲写入
	"net"         // 用于网络I/O操作
	"os"          // 用于路由日志文件句柄
	"sync"        // 用于同步原语，如互斥锁
	"sync/atomic" // 用于路由缓存时间戳的原子更新
	"time"        // 用于时间处理

	"golang.org/x/crypto/ssh" // 用于SSH客户端连接
)

// Method 定义路由方式的枚举类型
// 用于表示连接目标地址的方式：通过SSH隧道或直连
type Method int

// 定义具体的路由方式常量
// SSH表示通过SSH隧道连接，Direct表示直接连接
const (
	SSH    Method = iota // SSH隧道方式，值为0
	Direct               // 直连方式，值为1
)

// String Method类型的String方法，用于输出路由方式的字符串表示
// 实现Stringer接口，便于日志记录和调试
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

// targetInfo 定义目标信息结构体，用于缓存路由策略
// 存储目标地址的路由方式、上次使用时间和失败标记，用于路由缓存与负缓存
type targetInfo struct {
	method     Method       // 路由方式（SSH或Direct）
	lastActive atomic.Int64 // 上次使用/探测时间戳（原子更新，清理协程读取时无需加锁）
	failed     bool         // 上次探测是否失败（失败条目在负缓存窗口内直接快速失败；读写均受 cacheMu 保护）
}

// dnsCall DNS解析的"单飞"（singleflight）条目：
// 同一域名的并发首次解析合并为一次，避免缓存未命中时的解析击穿
type dnsCall struct {
	done chan struct{} // 关闭表示解析完成（结果字段随后可见）
	ips  []string      // 解析结果（IPv4 优先）；空切片表示解析失败
}

// routeResult 路由结果结构体，包含连接、路由方式、延迟与底层SSH连接
//
// 所有权约定：若结果为SSH路径，sshClient 是承载该隧道的底层SSH连接，
// 调用方必须在转发结束后调用 sshPool.Put(sshClient) 将其归还连接池；
// 使用期间该连接不驻留池中，避免被"池满释放"或"空闲清理"逻辑误关闭，
// 从而保证池中只回收确实空闲的连接。
type routeResult struct {
	conn      net.Conn      // 网络连接（指向目标地址的隧道）
	method    Method        // 路由方式
	latency   time.Duration // 连接延迟
	sshClient *ssh.Client   // SSH路径下的底层连接（Direct路径为nil）
}

// dnsEntry DNS缓存条目：记录解析结果与过期时间
type dnsEntry struct {
	ips    []string  // 解析出的地址列表（IPv4 优先）
	expire time.Time // 过期时间
}

// ConnectionHealth 连接健康状态结构体
// 用于记录SSH连接的健康状态和最后检查时间
type ConnectionHealth struct {
	healthy     bool      // 连接是否健康
	lastChecked time.Time // 最后检查时间
}

// ProxyServer 代理服务器结构体，封装所有状态和逻辑
type ProxyServer struct {
	config     *Config
	sshPool    *SSHConnectionPool
	routeCache map[string]*targetInfo // 值为指针：命中热路径可对 lastActive 做原子续命
	cacheMu    sync.RWMutex
	limiter    chan struct{}
	udpLimiter chan struct{} // 同时存在的 UDP 关联上限（每个关联占 2 个 socket + 3 个协程）
	bufferPool sync.Pool

	dnsCache  map[string]dnsEntry // 目标主机 -> 解析结果（带TTL），减少DNS解析
	dnsFlight map[string]*dnsCall // 进行中的DNS解析（singleflight：同域名并发首查合并为一次）
	dnsMu     sync.RWMutex        // 保护dnsCache与dnsFlight

	activeConn sync.WaitGroup // 正在处理的连接，优雅关闭时用于等待在途请求
	acceptWG   sync.WaitGroup // 监听循环，关闭时先等它退出，保证 activeConn.Add 不会晚于 Wait

	listeners   []net.Listener // 已启动的监听器，关闭时统一释放
	listenersMu sync.Mutex

	stopCh   chan struct{} // 后台任务（缓存清理、日志刷盘）退出信号
	stopOnce sync.Once

	routeLogFile *os.File      // route.log 常开句柄（WriteLogToFile 为 true 时有效）
	routeLogBuf  *bufio.Writer // 带缓冲的写入器：批量落盘，降低高频日志的 syscall 次数
	routeLogSize int64         // 已写入字节数（用于日志大小轮转）
	routeLogMu   sync.Mutex    // 保护routeLogFile/routeLogBuf的写入

	hostKeyMu sync.Mutex // 串行化主机密钥指纹文件的首次读写
}

// NewProxyServer 创建一个新的代理服务器实例
func NewProxyServer(cfg *Config) *ProxyServer {
	return &ProxyServer{
		config:     cfg,
		routeCache: make(map[string]*targetInfo),
		limiter:    make(chan struct{}, cfg.MaxConcurrentConnections),
		udpLimiter: make(chan struct{}, cfg.MaxConcurrentConnections),
		stopCh:     make(chan struct{}),
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 32*1024) // 32KB 缓冲区
			},
		},
		dnsCache:  make(map[string]dnsEntry),
		dnsFlight: make(map[string]*dnsCall),
	}
}
