package main

import (
	"bytes"
	"errors"
	"net"
	"testing"
)

// TestParseUDPDatagram 校验 SOCKS5 UDP 请求报文的解析
func TestParseUDPDatagram(t *testing.T) {
	// RSV(2) + FRAG(1) + IPv4 8.8.8.8:53 + payload "hi"
	pkt := []byte{0x00, 0x00, 0x00, atypIPv4, 8, 8, 8, 8, 0x00, 0x35, 'h', 'i'}

	host, port, payload, err := parseUDPDatagram(pkt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "8.8.8.8" || port != 53 {
		t.Errorf("got %s:%d, want 8.8.8.8:53", host, port)
	}
	if !bytes.Equal(payload, []byte("hi")) {
		t.Errorf("payload = %q, want %q", payload, "hi")
	}

	// 域名目标 example.com:53
	domainPkt := []byte{0x00, 0x00, 0x00, atypDomain, 11}
	domainPkt = append(domainPkt, []byte("example.com")...)
	domainPkt = append(domainPkt, 0x00, 0x35, 'x')

	host, port, payload, err = parseUDPDatagram(domainPkt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "example.com" || port != 53 || !bytes.Equal(payload, []byte("x")) {
		t.Errorf("domain packet parsed wrong: %s:%d payload=%q", host, port, payload)
	}
}

// TestParseUDPDatagramErrors 校验错误路径：短包丢弃、分片报文丢弃
func TestParseUDPDatagramErrors(t *testing.T) {
	// 报文过短
	if _, _, _, err := parseUDPDatagram([]byte{0x00, 0x00}); !errors.Is(err, errShortPacket) {
		t.Errorf("expected errShortPacket, got %v", err)
	}

	// FRAG != 0：按 RFC 1928 丢弃
	frag := []byte{0x00, 0x00, 0x01, atypIPv4, 8, 8, 8, 8, 0x00, 0x35, 'h'}
	if _, _, _, err := parseUDPDatagram(frag); !errors.Is(err, errUDPFragmentUnsupport) {
		t.Errorf("expected errUDPFragmentUnsupport, got %v", err)
	}
}

// TestBuildUDPDatagram 校验回送报文的封装格式
func TestBuildUDPDatagram(t *testing.T) {
	src := &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 53}
	got, err := buildUDPDatagram(src, []byte("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x00, 0x00, 0x00, atypIPv4, 8, 8, 8, 8, 0x00, 0x35, 'h', 'i'}
	if !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestUDPAcceptTargetWhitelist 验证只接受"本关联确实发送过报文"的目标回包，
// 防止任意主机借中继端口向客户端注入数据
func TestUDPAcceptTargetWhitelist(t *testing.T) {
	a := &udpAssociation{targets: make(map[string]struct{})}
	dst := &net.UDPAddr{IP: net.ParseIP("8.8.8.8"), Port: 53}

	if a.acceptTarget(dst) {
		t.Error("未发送过报文的目标不应被接受")
	}

	a.recordTarget(dst)
	if !a.acceptTarget(dst) {
		t.Error("已发送过报文的目标应被接受")
	}

	other := &net.UDPAddr{IP: net.ParseIP("8.8.4.4"), Port: 53}
	if a.acceptTarget(other) {
		t.Error("未通信过的其他来源不应被接受")
	}
}
