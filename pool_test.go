package main

import (
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHConnectionPoolInit(t *testing.T) {
	config := &ssh.ClientConfig{User: "test", Timeout: time.Second}
	pool := NewSSHConnectionPool(config, "localhost", "22", 5)
	defer pool.closeAll()

	if pool.maxSize != 5 {
		t.Errorf("expected maxSize 5, got %d", pool.maxSize)
	}
	if pool.healthStatus == nil {
		t.Error("healthStatus map should be initialized")
	}
	if pool.lastUsed == nil {
		t.Error("lastUsed map should be initialized")
	}
	if cap(pool.connections) != 5 {
		t.Errorf("expected connections channel cap 5, got %d", cap(pool.connections))
	}
}

func TestSSHConnectionPoolPutNil(t *testing.T) {
	pool := NewSSHConnectionPool(&ssh.ClientConfig{}, "localhost", "22", 2)
	defer pool.closeAll()

	// 空连接应被直接忽略，不应 panic
	pool.Put(nil)
}

func TestSSHConnectionPoolCloseAllIdempotent(t *testing.T) {
	pool := NewSSHConnectionPool(&ssh.ClientConfig{}, "localhost", "22", 2)

	pool.closeAll()
	pool.closeAll() // 重复调用应安全返回（幂等），不应 panic

	if !pool.closed.Load() {
		t.Error("pool should be marked as closed after closeAll")
	}
}
