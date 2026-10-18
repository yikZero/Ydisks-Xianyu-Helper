//go:build darwin

package main

import (
	"strings"
	"testing"
)

// TestLogDirectoryPathUsesHomeLibraryLogs 验证 macOS 日志目录位于 HOME/Library/Logs，且 HOME 缺失时返回错误。
func TestLogDirectoryPathUsesHomeLibraryLogs(t *testing.T) {
	// home 是伪造的主目录。
	home := t.TempDir()
	t.Setenv("HOME", home)
	// directory、err 是解析出的日志目录。
	directory, err := logDirectoryPath()
	if err != nil || directory != home+"/Library/Logs/YdisksXianyuHelper" {
		t.Fatalf("directory=%q err=%v", directory, err)
	}
	t.Setenv("HOME", "")
	if _, err := logDirectoryPath(); err == nil {
		t.Fatal("HOME 缺失时应报错")
	}
}

// TestServiceActionRejectsUnknownAction 验证未知动作在调用 launchctl 之前就被拒绝；HOME 缺失时同样提前失败。
func TestServiceActionRejectsUnknownAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := serviceAction("bogus"); err == nil || !strings.Contains(err.Error(), "未知服务操作") {
		t.Fatalf("err=%v", err)
	}
	t.Setenv("HOME", "")
	if err := serviceAction("start"); err == nil || !strings.Contains(err.Error(), "获取用户目录失败") {
		t.Fatalf("err=%v", err)
	}
}
