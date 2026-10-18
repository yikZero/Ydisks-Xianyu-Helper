package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// TestRunAPITokenCommandCreateListRevoke 覆盖 Token 命令的创建（明文只打印一次）、列表（只显示前缀）、吊销和未知用户。
func TestRunAPITokenCommandCreateListRevoke(t *testing.T) {
	ctx := context.Background()
	database, dialect, err := db.Open(ctx, filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := db.NewStore(database, dialect)
	if err := ensureAdmin(ctx, store, "admin@example.com", "admin-password-123"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenCreate: "grok", apiTokenScope: db.APITokenScopeWrite}, &out); err != nil {
		t.Fatal(err)
	}
	created := out.String()
	plainStart := strings.Index(created, db.APITokenPrefix)
	if plainStart < 0 {
		t.Fatalf("创建输出缺少明文 Token: %s", created)
	}
	plain := strings.TrimSpace(created[plainStart:])
	if _, err := store.APITokens.Authenticate(ctx, plain); err != nil {
		t.Fatalf("打印的 Token 应可认证: %v", err)
	}
	out.Reset()
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenList: true}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), plain) || !strings.Contains(out.String(), "grok") || !strings.Contains(out.String(), "有效") {
		t.Fatalf("列表只能显示前缀: %s", out.String())
	}
	out.Reset()
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenRevoke: 1}, &out); err != nil {
		t.Fatal(err)
	}
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenRevoke: 1}, &out); err == nil {
		t.Fatal("重复吊销应报错")
	}
	out.Reset()
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenList: true}, &out); err != nil || !strings.Contains(out.String(), "已吊销") {
		t.Fatalf("吊销后列表=%s err=%v", out.String(), err)
	}
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "nobody", apiTokenList: true}, &out); err == nil {
		t.Fatal("未知用户应报错")
	}
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenCreate: "x", apiTokenScope: "root"}, &out); err == nil {
		t.Fatal("未知权限范围应报错")
	}
}

// TestRunAPITokenCommandSaveWritesPrivateFile 验证 -api-token-save 把明文写入 mcp 子命令默认读取的文件（0600），终端不输出明文。
func TestRunAPITokenCommandSaveWritesPrivateFile(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	database, dialect, err := db.Open(ctx, filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := db.NewStore(database, dialect)
	if err := ensureAdmin(ctx, store, "admin@example.com", "admin-password-123"); err != nil {
		t.Fatal(err)
	}
	path, err := defaultAPITokenFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAPITokenCommand(ctx, store, serverOptions{apiTokenUser: "admin", apiTokenCreate: "grok", apiTokenScope: db.APITokenScopeRead, apiTokenSave: true}, &out); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := strings.TrimSpace(string(content))
	if _, err := store.APITokens.Authenticate(ctx, plain); err != nil {
		t.Fatalf("文件中的 Token 应可认证: %v", err)
	}
	if strings.Contains(out.String(), plain) || !strings.Contains(out.String(), path) {
		t.Fatalf("终端只应提示文件位置: %s", out.String())
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("Token 文件权限应为 0600: %v err=%v", info.Mode(), err)
	}
}
