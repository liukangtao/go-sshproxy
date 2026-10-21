// route.go 实现路由选择和缓存管理功能
// 负责选择最佳路由方式（SSH隧道或直连）并管理路由缓存
package main

import (
	"bufio"   // 用于路由日志的带缓冲写入
	"context" // 用于控制goroutine的上下文
	"errors"  // 用于无可用地址的错误
	"fmt"     // 用于格式化输入输出
	"log"     // 用于记录日志
	"net"     // 用于网络I/O操作
	"os"      // 用于打开路由日志文件
	"path/filepath"
	"sort" // 用于排序
	"time" // 用于时间处理
)

const (
	routeDialTimeout = 5 * time.Second // 探测阶段单个目标地址的TCP握手超时
	routeNegativeTTL = 30              // 探测失败后的负缓存窗口（秒），窗口内直接快速失败
)

// selectRoute 选择到目标地址的最佳路由
//
// 返回的 routeResult 若走SSH路径（sshClient 非 nil），调用方必须在转发结束后
// 调用 s.sshPool.Put(result.sshClient) 归还连接（见 routeResult 所有权约定）
func (s *ProxyServer) selectRoute(targetAddr string) routeResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.RouteTimeout)*time.Second)
	defer cancel()

	resultChan := make(chan routeResult) // 无缓冲：发送成功即代表连接所有权已移交
	failedChan := make(chan struct{}, 2) // 两条探测路径的失败通知
	start := time.Now()

	// 在锁内复制缓存字段：method/failed 由写锁保护，解锁后再读会与
	// storeRouteSuccess/storeRouteFailure 构成数据竞争（go test -race 可复现）
	s.cacheMu.RLock()
	cache, hasCache := s.routeCache[targetAddr]
	var cachedMethod Method
	var cachedFailed bool
	var cachedLastActive int64
	if hasCache {
		cachedMethod = cache.method
		cachedFailed = cache.failed
		cachedLastActive = cache.lastActive.Load()
	}
	s.cacheMu.RUnlock()

	// 负缓存：近期探测失败过的目标直接快速失败，避免每个请求都等满探测超时
	if hasCache && cachedFailed && time.Now().Unix()-cachedLastActive < routeNegativeTTL {
		return routeResult{}
	}

	if hasCache {
		// 缓存命中：优先按上次成功的路径直连，不再每次并发探测
		switch cachedMethod {
		case SSH:
			sshClient, err := s.sshPool.Get(ctx)
			if err == nil {
				// 使用 DialContext：探测超时/取消时立即返回，不阻塞等待底层建立
				sshConn, err := sshClient.DialContext(ctx, "tcp", targetAddr)
				if err == nil {
					// 必须走 storeRouteSuccess：除续期 lastActive 外，还要清除 failed，
					// 否则负缓存过期后的首次成功会让目标重新被判定为"失败"，
					// 形成"成功一次、失败 30 秒"的循环
					s.storeRouteSuccess(targetAddr, SSH)
					return routeResult{
						conn:      sshConn,
						method:    SSH,
						latency:   time.Since(start),
						sshClient: sshClient,
					}
				}
				// 建通道失败（目标不可达等）：归还连接后走重探测
				s.sshPool.Put(sshClient)
			}
		case Direct:
			addrs := s.resolveAll(ctx, targetAddr)
			localConn, err := dialDirect(ctx, addrs, 3*time.Second) // 命中缓存的重试要快
			if err == nil {
				// 同上：清 failed，避免恢复后的目标被负缓存再次挡住
				s.storeRouteSuccess(targetAddr, Direct)
				return routeResult{
					conn:    localConn,
					method:  Direct,
					latency: time.Since(start),
				}
			}
		}
		// 缓存路径失败，继续走并发探测
	}

	// —— 并发探测：SSH 隧道 与 本机直连 同时尝试，取先成功者 ——
	go func() {
		sshClient, err := s.sshPool.Get(ctx)
		if err != nil {
			failedChan <- struct{}{}
			return
		}

		// 使用 DialContext：探测超时/取消时立即返回（底层挂起的通道会在完成后自动清理）
		remoteConn, err := sshClient.DialContext(ctx, "tcp", targetAddr)
		if err != nil {
			// 无活跃隧道，归还连接安全
			s.sshPool.Put(sshClient)
			failedChan <- struct{}{}
			return
		}

		select {
		case resultChan <- routeResult{conn: remoteConn, method: SSH, latency: time.Since(start), sshClient: sshClient}:
			// 所有权已移交，连接由接收方关闭并归还
		case <-ctx.Done():
			remoteConn.Close()
			s.sshPool.Put(sshClient)
		}
	}()

	go func() {
		addrs := s.resolveAll(ctx, targetAddr)
		localConn, err := dialDirect(ctx, addrs, routeDialTimeout)
		if err != nil {
			failedChan <- struct{}{}
			return
		}

		select {
		case resultChan <- routeResult{conn: localConn, method: Direct, latency: time.Since(start)}:
		case <-ctx.Done():
			localConn.Close()
		}
	}()

	// 等待最先成功的结果；两条路径都失败时立即失败返回（不再空等超时）
	failCount := 0
	for {
		select {
		case result := <-resultChan:
			cancel() // 通知落后的探测协程尽快清理
			s.storeRouteSuccess(targetAddr, result.method)
			return result
		case <-failedChan:
			failCount++
			if failCount >= 2 {
				log.Printf("路由探测失败: %s（SSH 与直连均不可用）", targetAddr)
				s.storeRouteFailure(targetAddr)
				return routeResult{}
			}
		case <-ctx.Done():
			// 超时瞬间再补收一次，避免刚好就绪的成功结果被丢弃
			select {
			case result := <-resultChan:
				s.storeRouteSuccess(targetAddr, result.method)
				return result
			default:
			}
			log.Printf("路由探测超时: %s", targetAddr)
			s.storeRouteFailure(targetAddr)
			return routeResult{}
		}
	}
}

// dialDirect 依次尝试解析出的多个目标地址，任一成功即返回。
// 预解析后只拨第一个 IP 会丢失多 A 记录/双栈的容错能力（Happy Eyeballs 被绕过），
// 因此这里按地址列表逐个尝试，整体仍受 ctx 约束。
func dialDirect(ctx context.Context, addrs []string, timeout time.Duration) (net.Conn, error) {
	var lastErr error
	dialer := net.Dialer{Timeout: timeout}
	for _, addr := range addrs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的目标地址")
	}
	return nil, lastErr
}

// storeRouteSuccess 记录一次成功的路由选择（更新路由缓存）
func (s *ProxyServer) storeRouteSuccess(targetAddr string, method Method) {
	s.cacheMu.Lock()
	info, ok := s.routeCache[targetAddr]
	if !ok {
		info = &targetInfo{}
		s.routeCache[targetAddr] = info
	}
	info.method = method
	info.lastActive.Store(time.Now().Unix())
	info.failed = false
	s.cacheMu.Unlock()
}

// storeRouteFailure 记录一次探测失败（写入负缓存，窗口内不再重试）
func (s *ProxyServer) storeRouteFailure(targetAddr string) {
	s.cacheMu.Lock()
	// 复用/保留原路由方式：负缓存过期后的恢复探测会优先沿用上次成功的路径，
	// 避免 method 被重置为零值(SSH)导致无意中改走隧道
	info, ok := s.routeCache[targetAddr]
	if !ok {
		info = &targetInfo{}
		s.routeCache[targetAddr] = info
	}
	info.lastActive.Store(time.Now().Unix())
	info.failed = true
	s.cacheMu.Unlock()
}

// CheckTargetInfo 定期检查并清理过期的目标信息缓存
func (s *ProxyServer) CheckTargetInfo() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.cleanupRouteCache()
		}
	}
}

// cleanupRouteCache 执行一次路由缓存清理（过期条目 + 容量上限淘汰）
func (s *ProxyServer) cleanupRouteCache() {
	currentTime := time.Now().Unix()
	var toDelete []string

	s.cacheMu.RLock()
	for targetAddr, targetInfo := range s.routeCache {
		if currentTime-targetInfo.lastActive.Load() > 30*60 {
			toDelete = append(toDelete, targetAddr)
		}
	}
	s.cacheMu.RUnlock()

	if len(toDelete) > 0 {
		s.cacheMu.Lock()
		// 二次确认：收集期间条目可能刚被访问过，避免误删活跃缓存
		now := time.Now().Unix()
		removed := 0
		for _, addr := range toDelete {
			if info, ok := s.routeCache[addr]; ok && now-info.lastActive.Load() > 30*60 {
				delete(s.routeCache, addr)
				removed++
			}
		}
		s.cacheMu.Unlock()
		if removed > 0 {
			log.Printf("已清理 %d 条过期路由缓存", removed)
		}
	}

	s.cacheMu.RLock()
	overLimit := len(s.routeCache) > s.config.MaxCacheSize
	if overLimit {
		type cacheEntry struct {
			addr string
			time int64
		}
		entries := make([]cacheEntry, 0, len(s.routeCache))
		for addr, info := range s.routeCache {
			entries = append(entries, cacheEntry{addr, info.lastActive.Load()})
		}
		s.cacheMu.RUnlock()

		sort.Slice(entries, func(i, j int) bool {
			return entries[i].time < entries[j].time
		})

		toRemoveCount := len(entries) - s.config.MaxCacheSize
		if toRemoveCount > 0 {
			s.cacheMu.Lock()
			for i := 0; i < toRemoveCount; i++ {
				delete(s.routeCache, entries[i].addr)
			}
			s.cacheMu.Unlock()
			log.Printf("路由缓存超限，已清理 %d 条最早条目", toRemoveCount)
		}
	} else {
		s.cacheMu.RUnlock()
	}
}

// maxLogFileSize 日志文件大小上限：超过后归档为 <文件名>.old，防止日志无限增长
const maxLogFileSize = 10 * 1024 * 1024 // 10MB

// routeLogFlushInterval 路由日志缓冲的定时落盘间隔（日志最多延迟该时间可见）
const routeLogFlushInterval = time.Second

// routeLogBufSize 路由日志写缓冲大小
const routeLogBufSize = 32 * 1024

// rotateIfTooLarge 日志文件超过大小上限时归档为 <path>.old（覆盖更早的归档）
func rotateIfTooLarge(path string, limit int64) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= limit {
		return
	}
	old := path + ".old"
	_ = os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		log.Printf("日志轮转失败(%s): %v", path, err)
		return
	}
	log.Printf("日志已轮转: %s -> %s", path, old)
}

// openRouteLog 打开路由日志文件（常开句柄，避免每次写日志都开关文件）
func (s *ProxyServer) openRouteLog() {
	if !s.config.WriteLogToFile {
		return
	}

	// 与 proxy.log / pid / hostkey 保持一致，固定写入可执行文件所在目录，
	// 避免从其他目录启动时日志散落到工作目录
	path := filepath.Join(primaryConfigDir(), "route.log")
	rotateIfTooLarge(path, maxLogFileSize)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("打开路由日志文件失败: %v", err)
		return
	}
	if st, serr := f.Stat(); serr == nil {
		s.routeLogSize = st.Size()
	}

	s.routeLogMu.Lock()
	s.routeLogFile = f
	s.routeLogBuf = bufio.NewWriterSize(f, routeLogBufSize)
	s.routeLogMu.Unlock()

	// 后台定时把缓冲刷入磁盘：否则低流量时日志会滞留在缓冲里，无法近实时查看
	go func() {
		ticker := time.NewTicker(routeLogFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.flushRouteLog()
			case <-s.stopCh:
				return
			}
		}
	}()

	log.Println("路由日志已开启:", path)
}

// flushRouteLog 把路由日志缓冲刷入磁盘（未开启日志时为空操作）
func (s *ProxyServer) flushRouteLog() {
	s.routeLogMu.Lock()
	defer s.routeLogMu.Unlock()
	if s.routeLogBuf != nil {
		if err := s.routeLogBuf.Flush(); err != nil {
			log.Printf("刷新路由日志失败: %v", err)
		}
	}
}

// closeRouteLog 关闭路由日志文件（关闭前先刷盘，避免缓冲数据丢失）
func (s *ProxyServer) closeRouteLog() {
	s.routeLogMu.Lock()
	defer s.routeLogMu.Unlock()
	if s.routeLogBuf != nil {
		_ = s.routeLogBuf.Flush()
		s.routeLogBuf = nil
	}
	if s.routeLogFile != nil {
		s.routeLogFile.Close()
		s.routeLogFile = nil
	}
}

// logRouteResult 记录路由结果日志
// 使用常开文件句柄 + 缓冲写入 + 串行化，避免高并发下的反复开闭文件与高频小 write
func (s *ProxyServer) logRouteResult(target, method string, latency time.Duration) {
	if !s.config.WriteLogToFile {
		return
	}

	// 构造日志条目
	entry := fmt.Sprintf("[%s] 目标:%-80s 方式:%s\t 耗时:%v\t\n",
		time.Now().Format("2006-01-02 15:04:05"),
		target,
		method,
		latency)

	s.routeLogMu.Lock()
	defer s.routeLogMu.Unlock()
	if s.routeLogBuf == nil {
		return
	}
	n, err := s.routeLogBuf.WriteString(entry)
	if err != nil {
		log.Printf("写入路由日志失败: %v", err)
		return
	}
	s.routeLogSize += int64(n)
	if s.routeLogSize > maxLogFileSize {
		s.rotateRouteLogLocked()
	}
}

// rotateRouteLogLocked 轮转路由日志（须在持有 routeLogMu 时调用）
// 顺序说明：先刷盘并关闭句柄再重命名，否则 Windows 下重命名会因文件被占用而失败
func (s *ProxyServer) rotateRouteLogLocked() {
	path := filepath.Join(primaryConfigDir(), "route.log")
	old := path + ".old"

	if s.routeLogBuf != nil {
		_ = s.routeLogBuf.Flush()
		s.routeLogBuf = nil
	}
	if s.routeLogFile != nil {
		s.routeLogFile.Close()
		s.routeLogFile = nil
	}
	s.routeLogSize = 0

	_ = os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		log.Printf("路由日志轮转失败: %v", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("轮转后重新打开路由日志失败: %v", err)
		return
	}
	s.routeLogFile = f
	s.routeLogBuf = bufio.NewWriterSize(f, routeLogBufSize)
	log.Printf("路由日志已轮转: %s -> %s", path, old)
}
