package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAPITokensLifecycle 覆盖创建、认证、列表、吊销和停用用户后的拒绝，并确认只保存摘要。
func TestAPITokensLifecycle(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if created, err := store.Users.Create(ctx, "token-user", "token-user@example.com", "pw"); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	user, err := store.Users.GetByUsername(ctx, "token-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.APITokens.Create(ctx, user.ID, "bot", "admin"); !errors.Is(err, ErrInvalidAPITokenScope) {
		t.Fatalf("未知权限范围应拒绝: %v", err)
	}
	if _, _, err := store.APITokens.Create(ctx, user.ID, "  ", APITokenScopeRead); err == nil {
		t.Fatal("空名称应拒绝")
	}
	plain, token, err := store.APITokens.Create(ctx, user.ID, "grok", APITokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, APITokenPrefix) || !strings.HasPrefix(plain, token.Prefix) || len(plain) < 40 {
		t.Fatalf("token=%q prefix=%q", plain, token.Prefix)
	}
	var stored string
	if err := store.DB.QueryRowContext(ctx, `SELECT token_hash FROM api_tokens WHERE id=?`, token.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == plain || stored != hashAPIToken(plain) {
		t.Fatal("数据库只能保存 Token 摘要")
	}
	identity, err := store.APITokens.Authenticate(ctx, plain)
	if err != nil || identity.UserID != user.ID || identity.Scope != APITokenScopeWrite || identity.Username != "token-user" {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	for _, bad := range []string{"", "ydx_wrong", "no-prefix"} {
		if _, err := store.APITokens.Authenticate(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q 应认证失败: %v", bad, err)
		}
	}
	tokens, err := store.APITokens.List(ctx, user.ID)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == 0 {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
	recent := time.Now().Unix() - 10
	if _, err := store.DB.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, recent, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.APITokens.Authenticate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if tokens, err = store.APITokens.List(ctx, user.ID); err != nil || tokens[0].LastUsedAt != recent {
		t.Fatalf("一分钟内重复认证不应刷新最近使用时间: %+v err=%v", tokens, err)
	}
	if err := store.APITokens.Revoke(ctx, user.ID+1, token.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不能吊销他人的 Token: %v", err)
	}
	if err := store.APITokens.Revoke(ctx, user.ID, token.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.APITokens.Revoke(ctx, user.ID, token.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复吊销应返回 ErrNotFound: %v", err)
	}
	if _, err := store.APITokens.Authenticate(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("吊销后不应通过认证: %v", err)
	}
	second, _, err := store.APITokens.Create(ctx, user.ID, "grok-2", APITokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `UPDATE users SET is_active=0 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.APITokens.Authenticate(ctx, second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("停用用户的 Token 不应通过认证: %v", err)
	}
}
