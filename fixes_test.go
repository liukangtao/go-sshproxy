package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestConfigJSONDefaults 校验配置文件省略字段时沿用默认值、未知字段被拒绝
func TestConfigJSONDefaults(t *testing.T) {
	cfg := defaultConfig()
	dec := json.NewDecoder(strings.NewReader(`{"sshHost":"h","sshUser":"u","sshPasswd":"p"}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("decode minimal config: %v", err)
	}
	if cfg.SSHPort != 22 || cfg.Socks5Port != 6111 || cfg.HTTPPort != 8880 || !cfg.EnableUDP {
		t.Errorf("defaults not preserved: %+v", cfg)
	}

	cfg = defaultConfig()
	dec = json.NewDecoder(strings.NewReader(`{"unknownField":1}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err == nil {
		t.Error("unknown field should be rejected")
	}
}

// TestConfigValidate 校验非法参数在启动期被拒绝
func TestConfigValidate(t *testing.T) {
	base := Config{
		SSHPort:                  22,
		Socks5Port:               6111,
		HTTPPort:                 8880,
		MaxConcurrentConnections: 200,
		MaxCacheSize:             10000,
		SSHPoolSize:              50,
		SSHTimeout:               30,
		HandshakeTimeout:         60,
		RouteTimeout:             10,
		DNSCacheTTLSeconds:       300,
	}
	if err := base.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"maxConns=0", func(c *Config) { c.MaxConcurrentConnections = 0 }},
		{"cacheSize=-1", func(c *Config) { c.MaxCacheSize = -1 }},
		{"poolSize=0", func(c *Config) { c.SSHPoolSize = 0 }},
		{"same port", func(c *Config) { c.HTTPPort = c.Socks5Port }},
		{"bad ssh port", func(c *Config) { c.SSHPort = 70000 }},
		{"no listener", func(c *Config) { c.Socks5Port = 0; c.HTTPPort = 0 }},
	}
	for _, tc := range cases {
		cfg := base
		tc.mutate(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}

// TestHeaderLimitReader 校验正常请求可通过、超限请求被标记
func TestHeaderLimitReader(t *testing.T) {
	raw := "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"
	hlr := newHeaderLimitReader(strings.NewReader(raw), 1024)
	br := bufio.NewReader(hlr)
	hlr.beginRequest()

	req, err := http.ReadRequest(br)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if hlr.tooLarge {
		t.Error("normal request should not be marked too large")
	}
	if req.URL.Host != "example.com" {
		t.Errorf("host = %q, want example.com", req.URL.Host)
	}
}

// TestHeaderLimitReaderTooLarge 校验超大请求头被标记
func TestHeaderLimitReaderTooLarge(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nX-Long: " +
		strings.Repeat("a", 200) + "\r\n\r\n"
	hlr := newHeaderLimitReader(strings.NewReader(raw), 64)
	br := bufio.NewReader(hlr)
	hlr.beginRequest()

	// 数据已完整读入时 ReadRequest 可能成功，因此以 tooLarge 标记为准
	_, _ = http.ReadRequest(br)
	if !hlr.tooLarge {
		t.Error("oversized header should set tooLarge")
	}
}

// TestRemoveHopByHopHeaders 校验逐跳头与 Connection 中列出的字段被剥离
func TestRemoveHopByHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom, Keep-Alive")
	h.Set("X-Custom", "secret")
	h.Set("Proxy-Authorization", "Basic x")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Upgrade", "websocket")
	h.Set("X-Keep", "ok")

	removeHopByHopHeaders(h)

	for _, k := range []string{
		"Connection", "X-Custom", "Proxy-Authorization",
		"Proxy-Connection", "Keep-Alive", "Upgrade",
	} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s should be removed, got %q", k, v)
		}
	}
	if h.Get("X-Keep") != "ok" {
		t.Error("end-to-end header should be preserved")
	}
}

// TestUDPAcceptClientBindsPeerIP 校验 UDP 来源 IP 必须与 TCP 对端一致
func TestUDPAcceptClientBindsPeerIP(t *testing.T) {
	a := &udpAssociation{clientIP: net.ParseIP("192.0.2.10")}

	if a.acceptClient(&net.UDPAddr{IP: net.ParseIP("192.0.2.11"), Port: 1234}) {
		t.Error("packet from another IP should be rejected")
	}
	if !a.acceptClient(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}) {
		t.Error("first packet from peer IP should be accepted")
	}
	if a.acceptClient(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 9999}) {
		t.Error("packet from a different port should be rejected after binding")
	}
}

// TestRouteFailureClearedBySuccess 校验成功记录会清除负缓存标记
func TestRouteFailureClearedBySuccess(t *testing.T) {
	s := NewProxyServer(&Config{MaxConcurrentConnections: 10, MaxCacheSize: 100})

	s.storeRouteFailure("example.com:443")
	s.cacheMu.RLock()
	info := s.routeCache["example.com:443"]
	failed := info != nil && info.failed
	s.cacheMu.RUnlock()
	if !failed {
		t.Fatal("failure should set failed flag")
	}

	// 恢复成功必须清除 failed，否则下一次请求会被负缓存直接拒绝
	s.storeRouteSuccess("example.com:443", Direct)
	s.cacheMu.RLock()
	info = s.routeCache["example.com:443"]
	failed = info.failed
	method := info.method
	s.cacheMu.RUnlock()
	if failed {
		t.Error("failed flag should be cleared after success")
	}
	if method != Direct {
		t.Errorf("method = %v, want Direct", method)
	}
}
