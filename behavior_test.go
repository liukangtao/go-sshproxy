// behavior_test.go 覆盖内部组件的行为契约：DNS 缓存、连接池、路由缓存、
// 配置加载与头部限流器的状态重置。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---------- DNS 缓存与并发解析 ----------

// TestResolveAllCachesResult 验证解析结果写入缓存且 IPv4 优先
func TestResolveAllCachesResult(t *testing.T) {
	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		DNSCacheTTLSeconds:       60,
	})

	addrs := s.resolveAll(context.Background(), "localhost:8080")
	if len(addrs) == 0 {
		t.Fatal("localhost 解析结果为空")
	}
	if !net.ParseIP(strings.TrimSuffix(addrs[0], ":8080")).To4().Equal(net.IPv4(127, 0, 0, 1)) {
		t.Logf("首个解析结果 = %s（非 127.0.0.1，可能受 hosts 配置影响）", addrs[0])
	}

	s.dnsMu.RLock()
	entry, ok := s.dnsCache["localhost"]
	s.dnsMu.RUnlock()
	if !ok {
		t.Fatal("解析结果未写入 DNS 缓存")
	}
	if !entry.expire.After(time.Now()) {
		t.Error("缓存条目的过期时间应晚于当前时间")
	}

	// 第二次解析应命中缓存（条目未过期且内容一致）
	again := s.resolveAll(context.Background(), "localhost:8080")
	if len(again) != len(addrs) {
		t.Errorf("缓存命中后结果数量不一致: %d vs %d", len(again), len(addrs))
	}
}

// TestResolveAllIPFastPath 验证 IP 字面量不触发解析、不写缓存
func TestResolveAllIPFastPath(t *testing.T) {
	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		DNSCacheTTLSeconds:       60,
	})

	got := s.resolveAll(context.Background(), "127.0.0.1:9090")
	if len(got) != 1 || got[0] != "127.0.0.1:9090" {
		t.Errorf("IP 字面量应原样返回, got %v", got)
	}

	s.dnsMu.RLock()
	n := len(s.dnsCache)
	s.dnsMu.RUnlock()
	if n != 0 {
		t.Errorf("IP 字面量不应写入 DNS 缓存, 实际条目数 %d", n)
	}
}

// TestResolveAllInvalidAddr 验证无端口输入原样返回
func TestResolveAllInvalidAddr(t *testing.T) {
	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		DNSCacheTTLSeconds:       60,
	})
	got := s.resolveAll(context.Background(), "no-port-here")
	if len(got) != 1 || got[0] != "no-port-here" {
		t.Errorf("非法地址应原样返回, got %v", got)
	}
}

// TestResolveAllConcurrentSingleflight 并发首查不应产生重复解析或数据竞争
// （配合 -race 运行可验证 dnsFlight 的同步是否正确）
func TestResolveAllConcurrentSingleflight(t *testing.T) {
	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		DNSCacheTTLSeconds:       60,
	})

	const n = 24
	var wg sync.WaitGroup
	results := make([][]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = s.resolveAll(context.Background(), "localhost:443")
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if len(results[i]) != len(results[0]) {
			t.Errorf("第 %d 个并发解析结果数量 %d, 期望 %d", i, len(results[i]), len(results[0]))
		}
	}

	s.dnsMu.RLock()
	leftover := len(s.dnsFlight)
	s.dnsMu.RUnlock()
	if leftover != 0 {
		t.Errorf("解析完成后 dnsFlight 应清空, 仍有 %d 条", leftover)
	}
}

// ---------- SSH 连接池 ----------

// TestPoolGetAfterClose 验证关闭后的连接池拒绝新建连接
func TestPoolGetAfterClose(t *testing.T) {
	pool := NewSSHConnectionPool(&ssh.ClientConfig{Timeout: time.Second}, "127.0.0.1", "1", 2)
	pool.closeAll()

	_, err := pool.Get(context.Background())
	if !errors.Is(err, errPoolClosed) {
		t.Errorf("关闭后 Get 错误 = %v, 期望 errPoolClosed", err)
	}
}

// TestPoolGetCanceledContext 验证已取消的 ctx 立即返回而不建连
func TestPoolGetCanceledContext(t *testing.T) {
	pool := NewSSHConnectionPool(&ssh.ClientConfig{Timeout: time.Second}, "127.0.0.1", "1", 2)
	defer pool.closeAll()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := pool.Get(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误 = %v, 期望 context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("已取消的 ctx 应立即返回, 实际耗时 %v", elapsed)
	}
}

// TestPoolPutNilSafe 验证 Put(nil) 不 panic 且不污染池
func TestPoolPutNilSafe(t *testing.T) {
	pool := NewSSHConnectionPool(&ssh.ClientConfig{}, "127.0.0.1", "1", 2)
	defer pool.closeAll()

	pool.Put(nil)
	if n := len(pool.connections); n != 0 {
		t.Errorf("Put(nil) 后池内连接数 = %d, 期望 0", n)
	}
}

// TestPoolConcurrentOperations 并发 Get/Put/closeAll 不应 panic、死锁或数据竞争
func TestPoolConcurrentOperations(t *testing.T) {
	pool := NewSSHConnectionPool(&ssh.ClientConfig{Timeout: 200 * time.Millisecond}, "127.0.0.1", "1", 4)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if c, err := pool.Get(ctx); err == nil {
				pool.Put(c)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		pool.closeAll()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("并发 Get/Put/closeAll 超时，可能存在死锁")
	}

	pool.closeAll()
	if !pool.closed.Load() {
		t.Error("池应处于关闭状态")
	}
}

// ---------- 路由缓存 ----------

// TestSelectRouteNegativeCacheFastFail 验证负缓存窗口内直接失败、不再探测
func TestSelectRouteNegativeCacheFastFail(t *testing.T) {
	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		RouteTimeout:             5,
		DNSCacheTTLSeconds:       60,
	})

	const target = "203.0.113.9:80"
	s.storeRouteFailure(target)

	start := time.Now()
	result := s.selectRoute(target)
	elapsed := time.Since(start)

	if result.conn != nil {
		result.conn.Close()
		t.Fatal("负缓存期间不应返回可用连接")
	}
	if elapsed > time.Second {
		t.Errorf("负缓存未生效：耗时 %v（应远小于 RouteTimeout）", elapsed)
	}
}

// TestSelectRouteCacheHitDirect 验证缓存命中 Direct 时直接拨号成功
func TestSelectRouteCacheHitDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动目标监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		RouteTimeout:             3,
		DNSCacheTTLSeconds:       60,
	})
	target := ln.Addr().String()
	s.storeRouteSuccess(target, Direct)

	result := s.selectRoute(target)
	if result.conn == nil {
		t.Fatal("缓存命中 Direct 应成功建连")
	}
	defer result.conn.Close()
	if result.method != Direct {
		t.Errorf("路由方式 = %v, 期望 Direct", result.method)
	}

	s.cacheMu.RLock()
	info := s.routeCache[target]
	failed := info != nil && info.failed
	s.cacheMu.RUnlock()
	if failed {
		t.Error("成功路由后不应保留失败标记")
	}
}

// TestCleanupRouteCacheExpired 验证超过 30 分钟未访问的条目被清理
func TestCleanupRouteCacheExpired(t *testing.T) {
	s := NewProxyServer(&Config{MaxConcurrentConnections: 10, MaxCacheSize: 10})

	stale := &targetInfo{}
	stale.lastActive.Store(time.Now().Add(-31 * time.Minute).Unix())
	s.routeCache["stale:80"] = stale

	fresh := &targetInfo{}
	fresh.lastActive.Store(time.Now().Unix())
	s.routeCache["fresh:80"] = fresh

	s.cleanupRouteCache()

	if _, ok := s.routeCache["stale:80"]; ok {
		t.Error("过期条目应被清理")
	}
	if _, ok := s.routeCache["fresh:80"]; !ok {
		t.Error("活跃条目不应被清理")
	}
}

// TestCleanupRouteCacheOverLimit 验证超容量时淘汰最久未访问的条目
func TestCleanupRouteCacheOverLimit(t *testing.T) {
	s := NewProxyServer(&Config{MaxConcurrentConnections: 10, MaxCacheSize: 3})

	for i := 0; i < 6; i++ {
		info := &targetInfo{}
		// i 越大越旧：保留最新的 3 条（i=0,1,2）
		info.lastActive.Store(time.Now().Add(-time.Duration(i) * time.Second).Unix())
		s.routeCache[fmt.Sprintf("host%d:80", i)] = info
	}

	s.cleanupRouteCache()

	if got := len(s.routeCache); got != 3 {
		t.Errorf("清理后条目数 = %d, 期望 3", got)
	}
	for i := 0; i < 3; i++ {
		addr := fmt.Sprintf("host%d:80", i)
		if _, ok := s.routeCache[addr]; !ok {
			t.Errorf("较新的条目 %s 不应被淘汰", addr)
		}
	}
}

// ---------- 头部限流器 ----------

// TestHeaderLimitReaderResets 验证 beginRequest 重置计数与超限标记
func TestHeaderLimitReaderResets(t *testing.T) {
	hlr := newHeaderLimitReader(strings.NewReader(""), 16)
	hlr.beginRequest()
	hlr.tooLarge = true
	hlr.remaining = -1

	hlr.beginRequest()
	if hlr.tooLarge {
		t.Error("beginRequest 应清除 tooLarge 标记")
	}
	if hlr.remaining != 16 {
		t.Errorf("beginRequest 后 remaining = %d, 期望 16", hlr.remaining)
	}
}

// ---------- 配置加载 ----------

// TestLoadConfigFileMissing 验证配置文件缺失时返回明确错误
func TestLoadConfigFileMissing(t *testing.T) {
	if _, err := os.Stat(filepath.Join(primaryConfigDir(), configFileName)); err == nil {
		t.Skip("测试二进制目录存在配置文件，跳过缺失场景")
	}

	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	if err := loadConfigFile(); err == nil {
		t.Error("配置文件缺失时应返回错误")
	}
}

// TestLoadConfigFileValid 验证合法配置被完整加载到全局配置
func TestLoadConfigFileValid(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"sshHost": "10.0.0.1",
		"sshUser": "tester",
		"sshPasswd": "secret",
		"sshPort": 2222,
		"listen": "127.0.0.1",
		"socks5": 16080,
		"http": 16081,
		"poolSize": 7,
		"enableUDP": false
	}`
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	oldCfg := GlobalConfig
	defer func() { GlobalConfig = oldCfg }()

	if err := loadConfigFile(); err != nil {
		t.Fatalf("加载合法配置失败: %v", err)
	}

	if GlobalConfig.SSHHost != "10.0.0.1" || GlobalConfig.SSHUser != "tester" || GlobalConfig.SSHPasswd != "secret" {
		t.Errorf("SSH 配置未正确加载: %+v", GlobalConfig)
	}
	if GlobalConfig.SSHPort != 2222 || GlobalConfig.Socks5Port != 16080 || GlobalConfig.HTTPPort != 16081 {
		t.Errorf("端口配置未正确加载: %+v", GlobalConfig)
	}
	if GlobalConfig.SSHPoolSize != 7 {
		t.Errorf("poolSize = %d, 期望 7", GlobalConfig.SSHPoolSize)
	}
	if GlobalConfig.EnableUDP {
		t.Error("enableUDP 显式设为 false 但未被加载")
	}
	// 未在文件中出现的字段应保持默认值
	if GlobalConfig.MaxConcurrentConnections != defaultConfig().MaxConcurrentConnections {
		t.Errorf("未声明字段未使用默认值: maxConns=%d", GlobalConfig.MaxConcurrentConnections)
	}
}

// TestLoadConfigFileRejectsUnknownField 验证拼错的字段名会导致加载失败
func TestLoadConfigFileRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFileName),
		[]byte(`{"sshHost":"h","sshUser":"u","sshPasswd":"p","sshPot":22}`), 0644); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if err := loadConfigFile(); err == nil {
		t.Error("未知字段应导致加载失败（避免拼写错误被静默忽略）")
	}
}

// ---------- 地址工具 ----------

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":   true,
		"::1":         true,
		"localhost":   true,
		"0.0.0.0":     false,
		"::":          false,
		"192.168.1.1": false,
		"":            false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, 期望 %v", addr, got, want)
		}
	}
}

func TestHostPortsIPv6Brackets(t *testing.T) {
	got := hostPorts([]string{"::1", "127.0.0.1"}, "8080")
	want := []string{"[::1]:8080", "127.0.0.1:8080"}
	if len(got) != len(want) {
		t.Fatalf("结果数量 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hostPorts[%d] = %q, 期望 %q", i, got[i], want[i])
		}
	}
}

// ---------- 守护进程环境变量 ----------

// TestEnvWithoutDropsInheritedKey 验证派生守护进程前会剔除继承的同名环境变量，
// 否则 os.Getenv 取到旧值会导致子进程重复派生。
func TestEnvWithoutDropsInheritedKey(t *testing.T) {
	t.Setenv(daemonChildEnv, "0")

	for _, kv := range envWithout(daemonChildEnv) {
		if strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(daemonChildEnv)+"=") {
			t.Fatalf("继承的同名变量未被剔除: %s", kv)
		}
	}

	if isDaemonChild() {
		t.Error("环境变量为 0 时不应判定为守护子进程")
	}

	t.Setenv(daemonChildEnv, "1")
	if !isDaemonChild() {
		t.Error("环境变量为 1 时应判定为守护子进程")
	}
}

// TestDNSCacheEvictsWhenFull 验证缓存饱和时会淘汰旧条目并写入新解析结果，
// 而不是在满容量后彻底放弃写入（否则命中率会持续下降）
func TestDNSCacheEvictsWhenFull(t *testing.T) {
	s := NewProxyServer(&Config{
		MaxConcurrentConnections: 10,
		MaxCacheSize:             100,
		DNSCacheTTLSeconds:       60,
	})

	base := time.Now()
	for i := 0; i < maxDNSCacheSize; i++ {
		s.dnsCache[fmt.Sprintf("filler-%d.example", i)] = dnsEntry{
			ips:    []string{"127.0.0.1"},
			expire: base.Add(time.Duration(i+1) * time.Minute),
		}
	}

	addrs := s.resolveAll(context.Background(), "localhost:53")
	if len(addrs) == 0 {
		t.Fatal("localhost 解析失败，无法验证淘汰逻辑")
	}

	s.dnsMu.RLock()
	_, cached := s.dnsCache["localhost"]
	n := len(s.dnsCache)
	s.dnsMu.RUnlock()

	if !cached {
		t.Error("缓存饱和时应淘汰最早过期的条目并写入新域名")
	}
	if n > maxDNSCacheSize {
		t.Errorf("缓存条目数 = %d, 超过上限 %d", n, maxDNSCacheSize)
	}
}

// TestConfigValidateListenAndDebugAddr 验证监听地址与调试端点格式在启动期被校验
func TestConfigValidateListenAndDebugAddr(t *testing.T) {
	base := defaultConfig()
	base.SSHHost = "h"
	base.SSHUser = "u"
	base.SSHPasswd = "p"

	if err := base.validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		wantOK bool
	}{
		{"listen 携带端口", func(c *Config) { c.ListenAddr = "127.0.0.1:8080" }, false},
		{"listen 为 IPv6 字面量", func(c *Config) { c.ListenAddr = "::1" }, true},
		{"listen 为空", func(c *Config) { c.ListenAddr = "" }, true},
		{"debugAddr 缺少端口", func(c *Config) { c.DebugAddr = "127.0.0.1" }, false},
		{"debugAddr 端口越界", func(c *Config) { c.DebugAddr = "127.0.0.1:99999" }, false},
		{"debugAddr 合法", func(c *Config) { c.DebugAddr = "127.0.0.1:6060" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.validate()
			if tc.wantOK && err != nil {
				t.Errorf("期望通过校验，实际报错: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Error("期望校验失败，实际通过")
			}
		})
	}
}
