package main

import (
	"bytes"
	"errors"
	"net"
	"testing"
)

// TestReadSocks5Addr 校验 ATYP + ADDR + PORT 的解析与消费字节数
func TestReadSocks5Addr(t *testing.T) {
	// IPv6 ::1 + 端口 80
	ipv6 := []byte{atypIPv6}
	ipv6 = append(ipv6, make([]byte, 15)...)
	ipv6 = append(ipv6, 1)
	ipv6 = append(ipv6, 0x00, 0x50)

	// 域名 example.com + 端口 443
	domain := []byte{atypDomain, 11}
	domain = append(domain, []byte("example.com")...)
	domain = append(domain, 0x01, 0xBB)

	cases := []struct {
		name         string
		raw          []byte
		wantHost     string
		wantPort     int
		wantConsumed int
	}{
		{"ipv4", []byte{atypIPv4, 127, 0, 0, 1, 0x1F, 0x90}, "127.0.0.1", 8080, 7},
		{"ipv6", ipv6, "::1", 80, 19},
		{"domain", domain, "example.com", 443, 15},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port, consumed, err := readSocks5Addr(c.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != c.wantHost || port != c.wantPort || consumed != c.wantConsumed {
				t.Errorf("got %s:%d consumed=%d, want %s:%d consumed=%d",
					host, port, consumed, c.wantHost, c.wantPort, c.wantConsumed)
			}
		})
	}
}

// TestReadSocks5AddrErrors 校验解析错误路径
func TestReadSocks5AddrErrors(t *testing.T) {
	if _, _, _, err := readSocks5Addr(nil); !errors.Is(err, errShortPacket) {
		t.Errorf("expected errShortPacket for empty input, got %v", err)
	}
	if _, _, _, err := readSocks5Addr([]byte{0x09}); !errors.Is(err, errUnsupportedAddrType) {
		t.Errorf("expected errUnsupportedAddrType, got %v", err)
	}
}

// TestReadSocks5Request 通过 net.Pipe 构造请求字节流，校验完整请求解析
func TestReadSocks5Request(t *testing.T) {
	domainReq := []byte{0x05, cmdConnect, 0x00, atypDomain, 11}
	domainReq = append(domainReq, []byte("example.com")...)
	domainReq = append(domainReq, 0x01, 0xBB)

	cases := []struct {
		name     string
		payload  []byte
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"ipv4 connect", []byte{0x05, cmdConnect, 0x00, atypIPv4, 93, 184, 216, 34, 0x01, 0xBB}, "93.184.216.34", 443, false},
		{"domain connect", domainReq, "example.com", 443, false},
		{"bad version", []byte{0x04, cmdConnect, 0x00, atypIPv4, 1, 2, 3, 4, 0, 80}, "", 0, true},
		{"unsupported atyp", []byte{0x05, cmdConnect, 0x00, 0x09, 1, 2, 3, 4, 0, 80}, "", 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()

			go func() {
				client.Write(c.payload)
				client.Close()
			}()

			req, _, err := readSocks5Request(server)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.Host != c.wantHost || req.Port != c.wantPort {
				t.Errorf("got %s:%d, want %s:%d", req.Host, req.Port, c.wantHost, c.wantPort)
			}
		})
	}
}

// TestBuildSocks5Addr 校验应答地址编码
func TestBuildSocks5Addr(t *testing.T) {
	b, err := buildSocks5Addr(nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(b, []byte{atypIPv4, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("nil ip encoding wrong: %v", b)
	}

	b, err = buildSocks5Addr(net.IPv4(127, 0, 0, 1), 8080)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(b, []byte{atypIPv4, 127, 0, 0, 1, 0x1F, 0x90}) {
		t.Errorf("ipv4 encoding wrong: %v", b)
	}
}
