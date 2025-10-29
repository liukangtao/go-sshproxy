// route.go 实现路由选择和缓存管理功能
// 负责选择最佳路由方式（SSH隧道或直连）并管理路由缓存
package main

import (
	"context" // 用于控制goroutine的上下文
	"fmt"     // 用于格式化输入输出
	"log"     // 用于记录日志
	"net"     // 用于网络I/O操作
	"os"      // 用于访问操作系统功能
	"sort"    // 用于排序
	"time"    // 用于时间处理
)

// selectRoute 选择到目标地址的最佳路由
// 通过并发测试SSH隧道和直连方式，选择响应最快的路由方式
func selectRoute(targetAddr string, sshPool *SSHConnectionPool) (result routeResult, needUpdate bool) {
	// 创建5秒超时的上下文，防止路由选择过程耗时过长
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 创建结果通道，用于接收路由测试结果
	resultChan := make(chan routeResult)

	// 记录开始时间，用于计算连接延迟
	start := time.Now()

	// 尝试从缓存中获取路由信息
	// 使用读锁保护并发访问
	cacheMutex.RLock()
	cache, hasCache := routeCache[targetAddr]
	cacheMutex.RUnlock()

	// 如果有缓存路由信息，则优先使用
	if hasCache {
		// 根据缓存的路由方式执行相应操作
		switch cache.method {
		case SSH:
			{
				// SSH隧道模式
				// 从连接池获取SSH连接
				sshClient, err := sshPool.Get()
				if err != nil {
					log.Printf("从连接池获取SSH连接失败: %v", err)
					needUpdate = true
				} else {
					defer sshPool.Put(sshClient)
					// 通过SSH隧道连接目标地址
					var sshConn net.Conn
					sshConn, err = sshClient.Dial("tcp", targetAddr)
					if err != nil {
						// 归还连接到连接
						log.Printf("SSH隧道连接失败: %v", err)
						needUpdate = true
					} else {
						// 连接成功，设置路由结果
						result.conn = sshConn
						result.method = SSH
					}
				}
			}
		case Direct:
			{
				// 直连模式
				// 直接连接目标地址
				var err error
				result.conn, err = net.Dial("tcp", targetAddr)
				if err != nil {
					log.Printf("直连连接失败: %v", err)
					needUpdate = true
				}
			}
		default:
			{
				// 未知模式
				log.Printf("未知模式: %s", targetAddr)
				needUpdate = true
			}
		}

		// 计算连接延迟
		result.latency = time.Since(start)
		// 记录路由方式
		result.method = cache.method
	} else {
		// 未命中缓存，需要进行路由选择
		needUpdate = true
	}

	// 如果需要更新路由
	if needUpdate {
		// 启动协程通过SSH隧道连接目标
		// 并发测试SSH隧道方式
		go func() {
			// 从连接池获取SSH连接
			sshClient, err := sshPool.Get()
			if err != nil {
				log.Printf("从连接池获取SSH连接失败: %v", err)
				return
			}
			// 确保连接归还到连接池
			defer sshPool.Put(sshClient)

			// 通过SSH隧道连接目标地址
			remoteConn, err := sshClient.Dial("tcp", targetAddr)
			if err != nil {
				return
			}

			// 将结果发送到结果通道
			select {
			case resultChan <- routeResult{conn: remoteConn, method: SSH, latency: time.Since(start)}:
			case <-ctx.Done():
				// 上下文超时，关闭连接
				remoteConn.Close()
			}
		}()

		// 启动协程直连目标
		// 并发测试直连方式
		go func() {
			// 直接连接目标地址，设置10秒超时
			localConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
			if err != nil {
				return
			}
			// 将结果发送到结果通道
			select {
			case resultChan <- routeResult{conn: localConn, method: Direct, latency: time.Since(start)}:
			case <-ctx.Done():
				// 上下文超时，关闭连接
				localConn.Close()
			}
		}()

		// 等待最快的连接方式
		select {
		case result = <-resultChan:
			// 收到结果，取消其他连接尝试
			cancel()
		case <-time.After(10 * time.Second):
			// 10秒超时，记录超时日志
			log.Printf("连接超时: %s", targetAddr)
			result.conn = nil
		}

		// 更新路由缓存
		if result.conn != nil {
			// 使用写锁保护并发访问
			cacheMutex.Lock()
			routeCache[targetAddr] = targetInfo{
				method: result.method,
				time:   time.Now().Unix(),
			}
			cacheMutex.Unlock()
		}
	}

	// 返回路由结果和是否需要更新缓存的标志
	return result, needUpdate
}

// CheckTargetInfo 定期检查并清理过期的目标信息缓存
// 每5分钟检查一次，清理过期和超限的缓存条目
func CheckTargetInfo() {
	// 循环检查缓存
	for {
		// 每5分钟检查一次
		time.Sleep(5 * time.Minute)
		// 使用写锁保护并发访问
		cacheMutex.Lock()
		currentTime := time.Now().Unix()
		// 清理过期缓存
		for targetAddr, targetInfo := range routeCache {
			// 如果缓存超过30分钟未使用，则删除
			if currentTime-targetInfo.time > 30*60 {
				delete(routeCache, targetAddr)
				log.Printf("清理过期缓存: %s", targetAddr)
			}
		}

		// 如果缓存条目超过最大限制，删除最旧的条目
		if len(routeCache) > maxCacheSize {
			// 创建按时间排序的切片
			type cacheEntry struct {
				addr string
				time int64
			}

			entries := make([]cacheEntry, 0, len(routeCache))
			for addr, info := range routeCache {
				entries = append(entries, cacheEntry{addr, info.time})
			}

			// 按时间排序（旧的在前）
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].time < entries[j].time
			})

			// 删除最旧的条目，直到缓存大小在限制以内
			toRemove := len(routeCache) - maxCacheSize
			for i := 0; i < toRemove; i++ {
				delete(routeCache, entries[i].addr)
				log.Printf("清理超限缓存: %s", entries[i].addr)
			}
		}
		cacheMutex.Unlock()
	}
}

// logRouteResult 记录路由结果日志
// 将路由选择结果记录到日志文件中
func logRouteResult(target, method string, latency time.Duration) {
	// 如果不写入文件则直接返回
	if !WriteLogToFile {
		return
	}

	// 打开或创建日志文件
	f, err := os.OpenFile("route.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("日志文件错误: %v", err)
		return
	}
	// 函数退出时关闭文件
	defer f.Close()

	// 构造日志条目
	entry := fmt.Sprintf("[%s] 目标:%-80s 方式:%s\t 耗时:%v\t\n",
		time.Now().Format("2006-01-02 15:04:05"),
		target,
		method,
		latency)
	// 写入日志文件
	f.WriteString(entry)
}
