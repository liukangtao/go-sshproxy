// debug.go 提供可选的性能调试端点（net/http/pprof）
// 默认关闭：仅在配置项 debugAddr 非空时启动（建议仅绑定环回地址，代理本身无认证）
package main

import (
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // 向 DefaultServeMux 注册 /debug/pprof/* 处理器
)

// startDebugServer 启动 pprof 性能调试端点（addr 为空时不启动）
// 用途：CPU/内存/协程等 profile 采集，为性能优化提供实测依据
func startDebugServer(addr string) {
	if addr == "" {
		return
	}

	// 安全提示：pprof 会暴露运行时与内存信息，监听非环回地址时给出警告
	if host, _, err := net.SplitHostPort(addr); err == nil && !isLoopbackAddr(host) {
		log.Printf("警告: 性能调试端点监听非环回地址 %s，pprof 会暴露运行时信息，请确保网络可信！", addr)
	}

	go func() {
		log.Printf("性能调试端点(pprof)已启动: http://%s/debug/pprof/", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("性能调试端点启动失败: %v", err)
		}
	}()
}
