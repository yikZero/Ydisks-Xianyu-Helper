package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/mtop"
	"xianyu-go/internal/xianyu/renew"
)

// tokenFnMtop 在基础 MTOP 替身上提供可编程的 Token 刷新行为。
type tokenFnMtop struct {
	// fakeRunMtop 提供其余 MTOP 接口的固定实现。
	fakeRunMtop
	// fn 是 Token 刷新行为；cookie 与 deviceID 是本次请求的凭证快照与页面设备标识。
	fn func(ctx context.Context, cookie, deviceID string) (*mtop.RefreshResult, error)
}

// RefreshTokenWithDeviceIDContext 调用测试注入的 Token 刷新行为。
func (m *tokenFnMtop) RefreshTokenWithDeviceIDContext(ctx context.Context, cookie, deviceID string) (*mtop.RefreshResult, error) {
	return m.fn(ctx, cookie, deviceID)
}

// captchaTestHandler 在记录型处理器上增加滑块验证回调，返回预设结果。
type captchaTestHandler struct {
	// recordingHandler 提供 Handler 的基础实现。
	*recordingHandler
	// result 与 ok 是滑块验证回调返回的预设值。
	result *mtop.RefreshResult
	ok     bool
}

// OnTokenCaptchaVerification 返回预设的滑块验证结果。
func (h *captchaTestHandler) OnTokenCaptchaVerification(context.Context, string, string, string, string) (*mtop.RefreshResult, bool) {
	return h.result, h.ok
}

// okToken 是成功 Token 刷新结果，过期时间取一小时后。
func okToken() *mtop.RefreshResult {
	return &mtop.RefreshResult{AccessToken: "tok", AccessTokenExpireAt: time.Now().Add(time.Hour).Unix()}
}

// TestRefreshTokenFailureBranches 验证 Token 刷新在门控、读取、重读、空结果、并发变更与写回失败时的结果。
func TestRefreshTokenFailureBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("门控被占用", func(t *testing.T) {
		acc, _ := gapCredentialAccount(t, &tokenFnMtop{})
		release, err := acc.acquireRefreshGate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		// canceled 是已取消的上下文。
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := acc.refreshToken(canceled); err == nil {
			t.Fatal("门控等待取消应报错")
		}
	})
	t.Run("补全设备标识并成功", func(t *testing.T) {
		acc, _ := gapCredentialAccount(t, &tokenFnMtop{fn: func(_ context.Context, _, deviceID string) (*mtop.RefreshResult, error) {
			if deviceID == "" {
				t.Error("设备标识应由 unb 派生")
			}
			return okToken(), nil
		}})
		acc.mu.Lock()
		acc.deviceID = ""
		acc.mu.Unlock()
		if token, _, err := acc.refreshToken(ctx); err != nil || token != "tok" {
			t.Fatalf("刷新应成功: %q %v", token, err)
		}
	})
	t.Run("请求前读取失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &tokenFnMtop{})
		corruptCookieValue(t, store)
		if _, _, err := acc.refreshToken(ctx); err == nil {
			t.Fatal("读取失败应报错")
		}
	})
	t.Run("请求后重读失败", func(t *testing.T) {
		// store 是该账号使用的隔离测试数据库存储，用于构造和破坏持久化状态
		var store *db.Store
		// client 是注入可编程行为的 MTOP 测试替身
		client := &tokenFnMtop{fn: func(context.Context, string, string) (*mtop.RefreshResult, error) {
			corruptCookieValue(t, store)
			return okToken(), nil
		}}
		var acc *Account
		acc, store = gapCredentialAccount(t, client)
		if _, _, err := acc.refreshToken(ctx); err == nil {
			t.Fatal("重读失败应报错")
		}
	})
	t.Run("空结果", func(t *testing.T) {
		acc, _ := gapCredentialAccount(t, &tokenFnMtop{fn: func(context.Context, string, string) (*mtop.RefreshResult, error) { return nil, nil }})
		if _, _, err := acc.refreshToken(ctx); err == nil || !strings.Contains(err.Error(), "未返回结果") {
			t.Fatalf("空结果应报错: %v", err)
		}
	})
	t.Run("并发变更重试耗尽", func(t *testing.T) {
		// store 是该账号使用的隔离测试数据库存储，用于构造和破坏持久化状态
		var store *db.Store
		// calls 记录刷新次数，每次都改写数据库 Cookie 使快照失效。
		calls := 0
		// client 是注入可编程行为的 MTOP 测试替身
		client := &tokenFnMtop{fn: func(context.Context, string, string) (*mtop.RefreshResult, error) {
			calls++
			_ = store.Cookies.UpdateValueExisting(ctx, "cid", "unb=123; _m_h5_tk=v"+strings.Repeat("x", calls)+";")
			return okToken(), nil
		}}
		var acc *Account
		acc, store = gapCredentialAccount(t, client)
		if _, _, err := acc.refreshToken(ctx); err == nil || !strings.Contains(err.Error(), "重试次数已达上限") {
			t.Fatalf("应报重试耗尽: %v", err)
		}
	})
	t.Run("响应Cookie写回失败", func(t *testing.T) {
		// store 是该账号使用的隔离测试数据库存储，用于构造和破坏持久化状态
		var store *db.Store
		// client 是注入可编程行为的 MTOP 测试替身
		client := &tokenFnMtop{fn: func(context.Context, string, string) (*mtop.RefreshResult, error) {
			blockCookieUpdates(t, store)
			// result 保存 okToken 的返回结果，供随后的断言使用
			result := okToken()
			result.UpdatedCookies = "unb=123; _m_h5_tk=new;"
			result.CookieStateChanged = true
			return result, nil
		}}
		var acc *Account
		acc, store = gapCredentialAccount(t, client)
		if _, _, err := acc.refreshToken(ctx); err == nil || !strings.Contains(err.Error(), "保存 token 响应 Cookie") {
			t.Fatalf("应报写回失败: %v", err)
		}
	})
}

// TestAdoptTokenResponseCookiesBranches 验证 Token 响应 Cookie 合并的空值、元数据损坏、完整快照、写回失败与无存储路径。
func TestAdoptTokenResponseCookiesBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("元数据损坏", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &tokenFnMtop{})
		if _, err := store.DB.ExecContext(ctx, `UPDATE cookies SET metadata_json='enc:v1:!!!' WHERE id='cid'`); err != nil {
			t.Fatal(err)
		}
		if _, err := acc.adoptTokenResponseCookies(ctx, "a=1", &mtop.RefreshResult{UpdatedCookies: "a=2", CookieStateChanged: true}); err == nil {
			t.Fatal("元数据损坏应报错")
		}
	})
	t.Run("完整空快照写回成功与失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &tokenFnMtop{})
		// complete 是声明快照完整但 Jar 为空的响应。
		complete := &mtop.RefreshResult{UpdatedCookies: "unb=123", CookieSnapshotComplete: true}
		if cookie, err := acc.adoptTokenResponseCookies(ctx, "a=1", complete); err != nil || cookie != "unb=123" {
			t.Fatalf("完整快照应写回: %q %v", cookie, err)
		}
		blockCookieUpdates(t, store)
		if _, err := acc.adoptTokenResponseCookies(ctx, "a=1", complete); err == nil {
			t.Fatal("写回失败应报错")
		}
	})
	t.Run("无存储", func(t *testing.T) {
		acc := New(Config{CookieID: "mem", CookieStr: "unb=1"})
		if cookie, err := acc.adoptTokenResponseCookies(ctx, "unb=1", &mtop.RefreshResult{UpdatedCookies: "unb=1; b=2", CookieStateChanged: true}); err != nil || cookie != "unb=1; b=2" || acc.currentCookieStr() != "unb=1; b=2" {
			t.Fatalf("无存储应只更新内存: %q %v", cookie, err)
		}
		// cookie 保存被测函数返回的 Cookie 字符串
		if cookie, _ := acc.adoptTokenResponseCookies(ctx, "unb=1; b=2", &mtop.RefreshResult{UpdatedCookies: "unb=1; b=2", CookieStateChanged: true}); cookie != "unb=1; b=2" {
			t.Fatal("相同 Cookie 应原样返回")
		}
	})
}

// TestTokenCaptchaRecoveryBranches 验证滑块恢复在处理器缺失、错误类型不符与写回失败时放弃恢复。
func TestTokenCaptchaRecoveryBranches(t *testing.T) {
	ctx := context.Background()
	// riskErr 是带验证地址的风控错误。
	riskErr := &mtop.RiskVerificationError{API: "token", VerificationURL: "https://verify.example"}
	acc, store := gapCredentialAccount(t, &tokenFnMtop{})
	// ok 表示 tryTokenCaptchaRecovery 是否按预期完成动作
	if _, ok := acc.tryTokenCaptchaRecovery(ctx, "a=1", "dev", riskErr); ok {
		t.Fatal("处理器不支持滑块回调应放弃")
	}
	// handler 返回携带新 Cookie 的验证结果。
	handler := &captchaTestHandler{recordingHandler: &recordingHandler{}, ok: true, result: &mtop.RefreshResult{UpdatedCookies: "unb=123; x5sec=1", CookieStateChanged: true}}
	acc.handler = handler
	// ok 表示 tryTokenCaptchaRecovery 是否按预期完成动作
	if _, ok := acc.tryTokenCaptchaRecovery(ctx, "a=1", "dev", errors.New("plain")); ok {
		t.Fatal("非风控错误应放弃")
	}
	blockCookieUpdates(t, store)
	// ok 表示 tryTokenCaptchaRecovery 是否按预期完成动作
	if _, ok := acc.tryTokenCaptchaRecovery(ctx, "a=1", "dev", riskErr); ok {
		t.Fatal("写回失败应放弃恢复")
	}
}

// TestTokenCacheAndCooldownBranches 验证 Token 缓存的空值、无有效期、写库失败，以及冷却与清理边界。
func TestTokenCacheAndCooldownBranches(t *testing.T) {
	ctx := context.Background()
	acc, store := gapCredentialAccount(t, &tokenFnMtop{})
	acc.saveTokenCache(ctx, "dev", "", 0, "fp")
	acc.saveTokenCache(ctx, "dev", "token", 0, "fp")
	if _, err := store.DB.ExecContext(ctx, `CREATE TRIGGER block_token_insert BEFORE INSERT ON account_tokens BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `CREATE TRIGGER block_token_delete BEFORE DELETE ON account_tokens BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	acc.saveTokenCache(ctx, "dev", "token", time.Now().Add(time.Hour).Unix(), "fp")
	acc.clearTokenCache(ctx)
	// memory 没有存储，保存与清理应只更新内存。
	memory := New(Config{CookieID: "mem", CookieStr: "unb=1"})
	memory.saveTokenCache(ctx, "dev", "token", time.Now().Add(time.Hour).Unix(), "fp")
	memory.clearTokenCache(ctx)
	acc.mu.Lock()
	acc.lastCaptchaFailure = time.Now().Add(-2 * TokenCaptchaFailureCooldown)
	acc.mu.Unlock()
	// remaining 保存冷却剩余时长，已过冷却窗口时应为零
	if remaining := acc.tokenCaptchaCooldownRemaining(); remaining != 0 {
		t.Fatalf("冷却已过应为零: %s", remaining)
	}
}

// TestUpdateCookieAndReloadBranches 验证运行时 Cookie 同步与数据库重载在空值、读取失败与空 Context 下的行为。
func TestUpdateCookieAndReloadBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("无存储空值", func(t *testing.T) {
		// memory 是没有数据库存储的纯内存账号
		memory := New(Config{CookieID: "mem", CookieStr: "unb=1"})
		if err := memory.UpdateCookieContext(ctx, "  "); err != nil {
			t.Fatal(err)
		}
		if err := memory.UpdateCookieContext(nil, "unb=1"); err == nil { //nolint:staticcheck // 刻意验证空 Context 防御。
			t.Fatal("空 Context 应报错")
		}
	})
	t.Run("读取失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &tokenFnMtop{})
		corruptCookieValue(t, store)
		if err := acc.UpdateCookieContext(ctx, "unb=1"); err == nil {
			t.Fatal("读取失败应报错")
		}
		acc.UpdateCookie("unb=1")
	})
	t.Run("数据库值为空", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &tokenFnMtop{})
		if _, err := store.DB.ExecContext(ctx, `UPDATE cookies SET value='' WHERE id='cid'`); err != nil {
			t.Fatal(err)
		}
		if err := acc.UpdateCookieContext(ctx, "unb=1"); err == nil {
			t.Fatal("空值且无 Jar 应报错")
		}
		if acc.reloadCookieFromDB(ctx) {
			t.Fatal("空值且无 Jar 不应重载")
		}
	})
	t.Run("凭证指纹为空时重载", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &tokenFnMtop{})
		acc.mu.Lock()
		acc.credentialFP = ""
		acc.mu.Unlock()
		if err := store.Cookies.UpdateValueExisting(ctx, "cid", "unb=123; _m_h5_tk=reload;"); err != nil {
			t.Fatal(err)
		}
		if !acc.reloadCookieFromDB(ctx) {
			t.Fatal("数据库 Cookie 变化应触发重载")
		}
	})
}

// TestRenewEdgeBranches 验证无续期器、迟到合并无变化与导出的快照辅助路径。
func TestRenewEdgeBranches(t *testing.T) {
	ctx := context.Background()
	acc, _ := gapCredentialAccount(t, &tokenFnMtop{})
	acc.renewer = nil
	if acc.tryAPIRenew(ctx) {
		t.Fatal("无续期器不应续期")
	}
	if err := acc.persistPendingRenewCookies(ctx, &renew.Result{SetCookies: []string{"_m_h5_tk=tk_1; Path=/"}}); err != nil {
		t.Fatal(err)
	}
}
