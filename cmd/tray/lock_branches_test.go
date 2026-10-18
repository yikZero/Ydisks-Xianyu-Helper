//go:build !windows

package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolateUserDirs 把 HOME 与 XDG 缓存目录指向临时目录，避免测试触碰真实用户目录。
func isolateUserDirs(t *testing.T) string {
	t.Helper()
	// home 是伪造的用户主目录。
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return home
}

// TestAcquireTrayInstanceUsesUserCacheDirectory 验证单实例锁位于用户缓存目录下，第二次获取失败，释放后可重新获取。
func TestAcquireTrayInstanceUsesUserCacheDirectory(t *testing.T) {
	isolateUserDirs(t)
	// release、acquired、err 是首次获取结果。
	release, acquired, err := acquireTrayInstance()
	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	// second、secondAcquired、secondErr 是持锁期间的第二次获取结果。
	second, secondAcquired, secondErr := acquireTrayInstance()
	if secondErr != nil || secondAcquired {
		t.Fatalf("持锁期间不应再次获取: %v %v", secondAcquired, secondErr)
	}
	second()
	release()
	// again、againAcquired、againErr 是释放后的重新获取。
	again, againAcquired, againErr := acquireTrayInstance()
	if againErr != nil || !againAcquired {
		t.Fatalf("释放后应可获取: %v %v", againAcquired, againErr)
	}
	again()
}

// TestAcquireTrayInstanceReportsDirectoryFailure 验证缓存目录位置被普通文件占用时返回错误。
func TestAcquireTrayInstanceReportsDirectoryFailure(t *testing.T) {
	// home 是伪造主目录。
	home := isolateUserDirs(t)
	// blocker 是占用缓存根的普通文件，macOS 读 HOME/Library/Caches，其它平台读 XDG_CACHE_HOME。
	blocker := filepath.Join(home, "cache")
	if runtime.GOOS == "darwin" {
		blocker = filepath.Join(home, "Library")
	}
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// acquired、err 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if _, acquired, err := acquireTrayInstance(); err == nil || acquired {
		t.Fatalf("应返回目录创建错误: acquired=%v err=%v", acquired, err)
	}
}

// TestAcquireTrayFileLockOpenFailure 验证锁文件路径不可创建时返回错误。
func TestAcquireTrayFileLockOpenFailure(t *testing.T) {
	// acquired、err 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if _, acquired, err := acquireTrayFileLock(filepath.Join(t.TempDir(), "missing", "tray.lock")); err == nil || acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
}

// TestConfigureTrayLoggerWritesToLogDirectory 验证日志目录可用时托盘日志输出到文件，并在测试后恢复标准日志输出。
func TestConfigureTrayLoggerWritesToLogDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("日志目录位于 /var/log，非 macOS 平台不应在测试中写入系统目录")
	}
	isolateUserDirs(t)
	// previousOutput、previousFlags 是测试前的标准日志输出与标志，结束后恢复。
	previousOutput, previousFlags := log.Writer(), log.Flags()
	t.Cleanup(func() { log.SetOutput(previousOutput); log.SetFlags(previousFlags) })
	// closeLog 关闭托盘日志文件。
	closeLog := configureTrayLogger()
	log.Printf("托盘日志测试")
	closeLog()
	// directory、err 是日志目录路径。
	directory, err := logDirectoryPath()
	if err != nil {
		t.Fatal(err)
	}
	// content、readErr 是日志文件内容。
	content, readErr := os.ReadFile(filepath.Join(directory, "tray.log"))
	if readErr != nil || len(content) == 0 {
		t.Fatalf("日志文件缺失: %v", readErr)
	}
}

// TestConfigureTrayLoggerFallsBackWhenDirectoryUnavailable 验证日志目录无法解析/创建时返回可安全调用的空关闭函数。
func TestConfigureTrayLoggerFallsBackWhenDirectoryUnavailable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("仅 macOS 的日志目录由 HOME 决定，可安全伪造")
	}
	t.Setenv("HOME", "")
	configureTrayLogger()()
	// home 是伪造主目录，Library 被文件占用使目录创建失败。
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "Library"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	configureTrayLogger()()
}
