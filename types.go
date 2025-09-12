// types.go 定义项目中使用的数据结构和类型
// 包含枚举类型、结构体定义和全局变量声明
package main

import (
	"net"      // 用于网络I/O操作
	"sync"     // 用于同步原语，如互斥锁
	"time"     // 用于时间处理
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
// 存储目标地址的路由方式和上次使用时间，用于路由缓存
type targetInfo struct {
	method Method // 路由方式（SSH或Direct）
	time   int64  // 上次使用时间戳
}

// routeResult 路由结果结构体，包含连接、路由方式和延迟信息
// 用于存储路由选择的结果，包括网络连接、路由方式和连接延迟
type routeResult struct {
	conn    net.Conn      // 网络连接
	method  Method        // 路由方式
	latency time.Duration // 连接延迟
}

// ConnectionHealth 连接健康状态结构体
// 用于记录SSH连接的健康状态和最后检查时间
type ConnectionHealth struct {
	healthy     bool      // 连接是否健康
	lastChecked time.Time // 最后检查时间
}

// 全局变量定义
// 这些变量在多个函数和goroutine之间共享
var (
	// routeCache 路由缓存：目标地址->最佳路由方式
	// 用于缓存目标地址的最佳路由方式，避免重复的路由测试
	routeCache = make(map[string]*targetInfo)
	// cacheMutex 缓存读写锁，保证并发安全
	// 用于保护routeCache的并发访问，防止数据竞争
	cacheMutex sync.RWMutex
	// sshPool SSH连接池
	// 用于复用SSH连接，避免频繁创建和销毁连接
	sshPool *SSHConnectionPool
	// connectionLimiter 连接限制信号量
	// 用于限制最大并发连接数，防止资源耗尽
	connectionLimiter = make(chan struct{}, maxConcurrentConnections)
)