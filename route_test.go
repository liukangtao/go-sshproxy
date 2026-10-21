package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRotateIfTooLarge 验证日志轮转：超限归档、未超限不轮转、旧归档被覆盖、文件缺失安全返回
func TestRotateIfTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	if err := os.WriteFile(path, []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}

	// 超过阈值：应归档为 .old
	rotateIfTooLarge(path, 3)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original file should be renamed away")
	}
	if _, err := os.Stat(path + ".old"); err != nil {
		t.Error("rotated file (.old) should exist")
	}

	// 未超过阈值：不应轮转
	if err := os.WriteFile(path, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	rotateIfTooLarge(path, 100)
	if _, err := os.Stat(path); err != nil {
		t.Error("small file should not be rotated")
	}

	// 再次轮转：旧 .old 应被新归档覆盖
	if err := os.WriteFile(path, []byte("123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	rotateIfTooLarge(path, 3)
	data, err := os.ReadFile(path + ".old")
	if err != nil || string(data) != "123456789" {
		t.Errorf("old file should be overwritten with latest content, got %q err=%v", data, err)
	}

	// 文件不存在：安全返回，不 panic
	rotateIfTooLarge(filepath.Join(dir, "missing.log"), 1)
}
