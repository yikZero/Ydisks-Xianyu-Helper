package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xianyu-go/internal/db"
)

// runAPITokenCommand 执行 -api-token-* 运维命令：创建时把明文 Token 只打印一次，列表只显示前缀。
func runAPITokenCommand(ctx context.Context, store *db.Store, opts serverOptions, out io.Writer) error {
	// report 汇总命令输出，最后一次写出并返回写入错误。
	var report strings.Builder
	user, err := store.Users.GetByUsername(ctx, opts.apiTokenUser)
	if err != nil {
		return fmt.Errorf("查找用户 %q: %w", opts.apiTokenUser, err)
	}
	switch {
	case opts.apiTokenCreate != "":
		plain, token, err := store.APITokens.Create(ctx, user.ID, opts.apiTokenCreate, opts.apiTokenScope)
		if err != nil {
			return err
		}
		if !opts.apiTokenSave {
			fmt.Fprintf(&report, "已创建 API Token（ID %d，名称 %s，权限 %s）。明文只显示这一次，请立即保存：\n%s\n", token.ID, token.Name, token.Scope, plain)
			break
		}
		path, err := saveAPIToken(plain)
		if err != nil {
			return fmt.Errorf("已创建 API Token %d，但写入文件失败（可用 -api-token-revoke 吊销后重建）: %w", token.ID, err)
		}
		fmt.Fprintf(&report, "已创建 API Token（ID %d，名称 %s，权限 %s），明文已写入 %s\n", token.ID, token.Name, token.Scope, path)
	case opts.apiTokenRevoke > 0:
		if err := store.APITokens.Revoke(ctx, user.ID, opts.apiTokenRevoke); err != nil {
			return fmt.Errorf("吊销 API Token %d: %w", opts.apiTokenRevoke, err)
		}
		fmt.Fprintf(&report, "已吊销 API Token %d\n", opts.apiTokenRevoke)
	default:
		tokens, err := store.APITokens.List(ctx, user.ID)
		if err != nil {
			return err
		}
		if len(tokens) == 0 {
			fmt.Fprintln(&report, "没有 API Token")
		}
		for _, token := range tokens {
			status := "有效"
			if token.RevokedAt > 0 {
				status = "已吊销"
			}
			lastUsed := "从未使用"
			if token.LastUsedAt > 0 {
				lastUsed = time.Unix(token.LastUsedAt, 0).Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(&report, "%d\t%s\t%s…\t%s\t%s\t最近使用 %s\n", token.ID, token.Name, token.Prefix, token.Scope, status, lastUsed)
		}
	}
	_, err = io.WriteString(out, report.String())
	return err
}

// saveAPIToken 把明文 Token 写入 defaultAPITokenFile：目录 0700、文件 0600，已有文件会被覆盖。
func saveAPIToken(plain string) (string, error) {
	path, err := defaultAPITokenFile()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	// 覆盖已有文件时 OpenFile 不会收紧原权限，这里显式改回 0600。
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	if _, err := io.WriteString(file, plain+"\n"); err != nil {
		_ = file.Close()
		return "", err
	}
	return path, file.Close()
}
