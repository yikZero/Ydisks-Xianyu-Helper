package engine

import (
	"context"
	"testing"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
	"xianyu-go/internal/xianyu/mtop"
	"xianyu-go/internal/xianyu/renew"
)

// gapCredentialAccount 构造使用加密密钥与指定 MTOP 的账号，返回账号、存储与上下文。
func gapCredentialAccount(t *testing.T, client mtop.Client) (*Account, *db.Store) {
	t.Helper()
	t.Setenv("XIANYU_DATA_KEY", "engine-gap-coverage-key")
	// acc、store、cleanup 是账号、其数据库与释放函数。
	acc, _, store, cleanup := newRunAccount(t, client)
	t.Cleanup(cleanup)
	return acc, store
}

// corruptCookieValue 把账号 Cookie 密文改成不可解密内容，使运行时读取失败而状态读取仍成功。
func corruptCookieValue(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.DB.ExecContext(context.Background(), `UPDATE cookies SET value='enc:v1:!!!' WHERE id='cid'`); err != nil {
		t.Fatal(err)
	}
}

// blockCookieUpdates 安装触发器使之后所有 Cookie 更新失败，用于验证持久化错误分支。
func blockCookieUpdates(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.DB.ExecContext(context.Background(), `CREATE TRIGGER block_cookie_update BEFORE UPDATE ON cookies BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
}

// gapSnapshot 是登录态检查测试使用的完整 Cookie Jar。
func gapSnapshot() []cookierefresh.BrowserCookie {
	return []cookierefresh.BrowserCookie{
		{Name: "unb", Value: "123", Domain: ".goofish.com", Path: "/"},
		{Name: "sdk", Value: "1", Domain: ".goofish.com", Path: "/"},
	}
}

// TestTryAPIRenewUsingBranches 验证接口续期在门控、禁用、读取失败、并发变更与各持久化路径下的结果。
func TestTryAPIRenewUsingBranches(t *testing.T) {
	ctx := context.Background()
	// empty 是不携带任何 Cookie 变化的续期结果构造函数。
	empty := func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
		return &renew.Result{}, nil
	}
	t.Run("门控被占用", func(t *testing.T) {
		acc, _ := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		// release 占住刷新门，使取消的上下文必然走错误分支。
		release, err := acc.acquireRefreshGate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		// canceled 是已取消的上下文。
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := acc.tryAPIRenewUsing(canceled, empty); err == nil {
			t.Fatal("等待刷新门被取消应返回错误")
		}
	})
	t.Run("账号已禁用", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		store.Cookies.SetStatus(ctx, "cid", false)
		if ok, err := acc.tryAPIRenewUsing(ctx, empty); ok || err != nil {
			t.Fatalf("禁用账号不应续期: ok=%v err=%v", ok, err)
		}
	})
	t.Run("前置读取失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		corruptCookieValue(t, store)
		if _, err := acc.tryAPIRenewUsing(ctx, empty); err == nil {
			t.Fatal("读取失败应返回错误")
		}
	})
	t.Run("数据库Cookie已变化", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if err := store.Cookies.UpdateValueExisting(ctx, "cid", "unb=123; _m_h5_tk=other;"); err != nil {
			t.Fatal(err)
		}
		if ok, err := acc.tryAPIRenewUsing(ctx, empty); ok || err != nil {
			t.Fatalf("无变化结果应返回 false: ok=%v err=%v", ok, err)
		}
		if acc.currentCookieStr() != "unb=123; _m_h5_tk=other;" {
			t.Fatal("运行时应吸收数据库中更新的 Cookie")
		}
	})
	t.Run("重读失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if _, err := acc.tryAPIRenewUsing(ctx, func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
			corruptCookieValue(t, store)
			return &renew.Result{}, nil
		}); err == nil {
			t.Fatal("续期后重读失败应返回错误")
		}
	})
	t.Run("并发变更且无Set-Cookie", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if ok, err := acc.tryAPIRenewUsing(ctx, func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
			_ = store.Cookies.UpdateValueExisting(ctx, "cid", "unb=123; _m_h5_tk=concurrent;")
			return &renew.Result{NewCookies: "x=1"}, nil
		}); ok || err != nil {
			t.Fatalf("旧响应不应写回: ok=%v err=%v", ok, err)
		}
	})
	t.Run("并发变更重放写回失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if _, err := acc.tryAPIRenewUsing(ctx, func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
			_ = store.Cookies.UpdateValueExisting(ctx, "cid", "unb=123; _m_h5_tk=concurrent;")
			blockCookieUpdates(t, store)
			return &renew.Result{SetCookies: []string{"a=1; Path=/"}, NewCookies: "x=1"}, nil
		}); err == nil {
			t.Fatal("重放写回失败应返回错误")
		}
	})
	t.Run("完整快照写回成功", func(t *testing.T) {
		acc, _ := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if ok, err := acc.tryAPIRenewUsing(ctx, func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
			return &renew.Result{Success: true, CookieSnapshotComplete: true, CookieSnapshot: gapSnapshot(), NewCookies: "unb=123; sdk=1"}, nil
		}); !ok || err != nil {
			t.Fatalf("快照续期应成功: ok=%v err=%v", ok, err)
		}
	})
	t.Run("完整快照写回失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if _, err := acc.tryAPIRenewUsing(ctx, func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
			blockCookieUpdates(t, store)
			return &renew.Result{CookieSnapshotComplete: true, CookieSnapshot: gapSnapshot(), NewCookies: "unb=123; sdk=1"}, nil
		}); err == nil {
			t.Fatal("快照写回失败应返回错误")
		}
	})
	t.Run("扁平写回失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if _, err := acc.tryAPIRenewUsing(ctx, func(context.Context, string, []cookierefresh.BrowserCookie) (*renew.Result, error) {
			blockCookieUpdates(t, store)
			return &renew.Result{SetCookies: []string{"a=1"}, NewCookies: "unb=123; a=1"}, nil
		}); err == nil {
			t.Fatal("扁平写回失败应返回错误")
		}
	})
}

// TestPersistAndAdoptCookieErrorBranches 验证扁平写回、迟到续期合并与采纳恢复 Cookie 的边界与错误。
func TestPersistAndAdoptCookieErrorBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("无存储", func(t *testing.T) {
		// acc 没有数据库存储。
		acc := New(Config{CookieID: "mem", CookieStr: "unb=1"})
		if err := acc.credentials.persistRenewFlatCookie(ctx, "a=1"); err != nil {
			t.Fatal(err)
		}
		if err := acc.persistPendingRenewCookies(ctx, &renew.Result{SetCookies: []string{"a=1"}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("metadata读取失败", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if _, err := store.DB.ExecContext(ctx, `UPDATE cookies SET metadata_json='enc:v1:!!!' WHERE id='cid'`); err != nil {
			t.Fatal(err)
		}
		if err := acc.credentials.persistRenewFlatCookie(ctx, "a=1"); err == nil {
			t.Fatal("metadata 解密失败应返回错误")
		}
	})
	t.Run("采纳恢复Cookie", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if acc.adoptRecoveredCookie(ctx, "  ", "测试") || acc.adoptRecoveredCookie(ctx, acc.currentCookieStr(), "测试") {
			t.Fatal("空值或未变化的 Cookie 不应被采纳")
		}
		blockCookieUpdates(t, store)
		if acc.adoptRecoveredCookie(ctx, "unb=123; new=1", "测试") {
			t.Fatal("写库失败不应采纳")
		}
	})
	t.Run("迟到续期合并", func(t *testing.T) {
		acc, store := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		if err := acc.persistPendingRenewCookies(ctx, nil); err != nil {
			t.Fatal(err)
		}
		// held 占住刷新门，取消的上下文必然得到门控错误。
		held, err := acc.acquireRefreshGate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// canceled 是已取消的上下文。
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := acc.persistPendingRenewCookies(canceled, &renew.Result{SetCookies: []string{"a=1"}}); err == nil {
			t.Fatal("门控等待被取消应返回错误")
		}
		held()
		// release 控制本地续期服务；late 是携带准确作用域的迟到结果。
		release := make(chan struct{})
		// pending 是 Promise 超时后仍待交付底层响应的续期结果
		pending := newPendingRenewResult(t, release, "ok")
		close(release)
		// awaitCtx 是等待迟到结果的有界上下文。
		awaitCtx, awaitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer awaitCancel()
		// awaitErr 保存 AwaitPending 调用返回的错误，随后断言它是否符合该场景的预期 ；late 保存底层迟到响应经 AwaitPending 取得的续期结果
		late, awaitErr := pending.AwaitPending(awaitCtx)
		if awaitErr != nil || late == nil {
			t.Fatalf("迟到结果异常: %v", awaitErr)
		}
		if err := acc.persistPendingRenewCookies(ctx, &renew.Result{SetCookies: []string{"a=1"}, UpdatedCookieNames: []string{"a"}}); err != nil {
			t.Logf("无作用域结果的合并返回: %v", err)
		}
		blockCookieUpdates(t, store)
		if err := acc.persistPendingRenewCookies(ctx, late); err == nil {
			t.Fatal("写库失败应返回错误")
		}
		// other、otherStore 是第二个账号，用于验证 Cookie 密文损坏时的读取失败。
		other, otherStore := gapCredentialAccount(t, &fakeRunMtop{token: "t"})
		corruptCookieValue(t, otherStore)
		if err := other.persistPendingRenewCookies(ctx, late); err == nil {
			t.Fatal("读取失败应返回错误")
		}
	})
}
