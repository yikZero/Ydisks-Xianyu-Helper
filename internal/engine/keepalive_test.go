package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/mtop"
)

// countingMtop 包装 fakeRunMtop，计数 RefreshToken 调用次数。
type countingMtop struct {
	fakeRunMtop
	calls int32
}

// RefreshTokenWithDeviceIDContext 刷新令牌WithDeviceID上下文。
func (c *countingMtop) RefreshTokenWithDeviceIDContext(_ context.Context, _ string, _ string) (*mtop.RefreshResult, error) {
	atomic.AddInt32(&c.calls, 1)
	return &mtop.RefreshResult{AccessToken: c.token, AccessTokenExpireAt: time.Now().Add(time.Hour).Unix()}, nil
}

type statusMtop struct {
	fakeRunMtop
	result *mtop.LoginStatusResult
	err    error
}

type failingCountingMtop struct {
	fakeRunMtop
	calls int32
	err   error
}

// RefreshTokenWithDeviceIDContext 刷新令牌WithDeviceID上下文。
func (c *failingCountingMtop) RefreshTokenWithDeviceIDContext(_ context.Context, _ string, _ string) (*mtop.RefreshResult, error) {
	atomic.AddInt32(&c.calls, 1)
	return nil, c.err
}

func saveBoundToken(t *testing.T, store *db.Store, acc *Account, token string, expireAt int64) {
	t.Helper()
	if err := store.Tokens.SaveBound(context.Background(), acc.CookieID, acc.deviceID, token, expireAt,
		credentialCookieFingerprint(acc.currentCookieStr())); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshTokenNetworkFailureClearsMemoryButPreservesFreshCache(t *testing.T) {
	mtopClient := &failingCountingMtop{
		fakeRunMtop: fakeRunMtop{token: "unused"},
		err:         errors.New("network connection reset"),
	}
	acc, _, store, cleanup := newRunAccount(t, mtopClient)
	defer cleanup()
	saveBoundToken(t, store, acc, "cached-tok", time.Now().Add(time.Hour).Unix())
	acc.mu.Lock()
	acc.currentToken = "old-memory-token"
	acc.mu.Unlock()
	if _, _, err := acc.refreshToken(context.Background()); err == nil {
		t.Fatal("网络失败应返回错误")
	}
	acc.mu.Lock()
	current := acc.currentToken
	acc.mu.Unlock()
	if current != "" {
		t.Fatalf("刷新网络失败后应清空内存 token: %q", current)
	}
	if cached, err := store.Tokens.Get(context.Background(), "cid"); err != nil || cached.AccessToken != "cached-tok" {
		t.Fatalf("网络失败应保留未过期数据库缓存: cached=%+v err=%v", cached, err)
	}
}

func TestRefreshTokenBusinessFailureClearsMemoryAndCache(t *testing.T) {
	mtopClient := &failingCountingMtop{
		fakeRunMtop: fakeRunMtop{token: "unused"},
		err:         errors.New("token API business rejected"),
	}
	acc, _, store, cleanup := newRunAccount(t, mtopClient)
	defer cleanup()
	saveBoundToken(t, store, acc, "cached-tok", time.Now().Add(time.Hour).Unix())
	acc.mu.Lock()
	acc.currentToken = "old-memory-token"
	acc.mu.Unlock()
	if _, _, err := acc.refreshToken(context.Background()); err == nil {
		t.Fatal("业务失败应返回错误")
	}
	acc.mu.Lock()
	current := acc.currentToken
	acc.mu.Unlock()
	if current != "" {
		t.Fatalf("业务失败后应清空内存 token: %q", current)
	}
	if tk, err := store.Tokens.Get(context.Background(), "cid"); err != nil || tk.AccessToken != "" || tk.DeviceID != acc.deviceID {
		t.Fatalf("业务失败应清 token 并保留 device ID: tk=%+v err=%v", tk, err)
	}
}

// CheckLoginStatusContext 检查登录状态上下文。
func (s *statusMtop) CheckLoginStatusContext(context.Context, string) (*mtop.LoginStatusResult, error) {
	return s.result, s.err
}

// TestReloadCookieFromDB_AdoptsNewCookieAndClearsCache DB cookie 变更时采纳新 cookie 并清旧 token 缓存。
func TestReloadCookieFromDB_AdoptsNewCookieAndClearsCache(t *testing.T) {
	acc, _, store, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok-1"})
	defer cleanup()
	// 预置旧 session 的 token 缓存。
	oldDeviceID := acc.deviceID
	saveBoundToken(t, store, acc, "old-tok", time.Now().Add(time.Hour).Unix())
	// 外部更新 DB cookie（模拟重新扫码），userID=0 复用现有 user_id。
	newCookie := "unb=123; _m_h5_tk=newtk_2; sgcookie=abc"
	if err := store.Cookies.Save(context.Background(), "cid", newCookie, 0); err != nil {
		t.Fatalf("Save: %v", err)
	}
	acc.reloadCookieFromDB(context.Background())
	if got := acc.CurrentCookieStr(); got != newCookie {
		t.Fatalf("应采纳 DB 新 cookie，got %s", got)
	}
	// 普通 Cookie Jar 更新不等于页面 reload：旧 token 清除，device ID 保持。
	if tk, err := store.Tokens.Get(context.Background(), "cid"); err != nil || tk.AccessToken != "" || tk.DeviceID != oldDeviceID || acc.deviceID != oldDeviceID {
		t.Fatalf("cookie 变更后应清 token 并保持运行时 device ID: runtime=%q tk=%+v err=%v", acc.deviceID, tk, err)
	}
}
