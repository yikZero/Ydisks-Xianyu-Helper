package adapter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	accountmanager "xianyu-go/internal/account"
	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/browser"
	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
	"xianyu-go/internal/xianyu/mtop"
)

// hookedProfileMTopFake 在资料请求期间执行测试钩子（例如关闭数据库），并可修改请求 Cookie 会话。
type hookedProfileMTopFake struct {
	// orderRuntimeMTopFake 提供资料测试不涉及的其他平台接口默认实现。
	orderRuntimeMTopFake
	// result 是资料请求返回的预置结果。
	result *mtop.UserProfileResult
	// hook 在请求内部执行，用于制造并发故障。
	hook func(session *mtop.CookieSession)
}

// FetchUserProfile 执行钩子后返回预置资料结果。
func (f *hookedProfileMTopFake) FetchUserProfile(ctx context.Context, _ string) (*mtop.UserProfileResult, error) {
	if f.hook != nil {
		f.hook(mtop.CookieSessionFromContext(ctx))
	}
	return f.result, nil
}

// TestAccountProfileRefreshPersistFailureBranches 覆盖资料刷新中响应凭证写回失败、扁平凭证写回失败与资料保存失败的告警路径。
func TestAccountProfileRefreshPersistFailureBranches(t *testing.T) {
	ctx := context.Background()
	// logger 丢弃输出，仅验证分支被执行。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// newPort 构造一个使用给定假平台客户端的资料适配器，并返回可关闭数据库的清理函数。
	newPort := func(t *testing.T, client mtop.Client, recovered *int) (*AccountProfilePort, *db.Store) {
		// cleanup、store 依次为：释放隔离测试数据库的函数；隔离的测试数据库存储。
		store, cleanup := newAdapterTestStore(t)
		t.Cleanup(cleanup)
		return NewAccountProfilePort(NewAccountLoginRepository(store), func() mtop.Client { return client }, nil, func(context.Context, string, error) bool { *recovered++; return false }, logger), store
	}
	// input 是资料刷新输入，账号 cid 的所有者为用户 1。
	input := accountapp.ProfileInput{UserID: 1, AccountID: "cid", Summary: accountapp.Summary{ID: "cid"}}
	// 会话变更写回失败：请求内关闭数据库，使响应 Cookie 持久化失败。
	var store *db.Store
	// recovered 统计恢复回调次数。
	recovered := 0
	// client1 在请求期间改写 Cookie 会话并关闭数据库。
	client1 := &hookedProfileMTopFake{result: &mtop.UserProfileResult{Nickname: "n"}, hook: func(session *mtop.CookieSession) {
		session.ReplaceSnapshot([]cookierefresh.BrowserCookie{{Name: "unb", Value: "2", Domain: ".goofish.com", Path: "/"}})
		_ = store.DB.Close()
	}}
	// port 被测的账号资料适配器。
	var port *AccountProfilePort
	port, store = newPort(t, client1, &recovered)
	// err、result 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if result, err := port.RefreshProfile(ctx, input); err != nil || result.ErrorMessage == "" || recovered != 1 {
		t.Fatalf("会话写回失败: %+v %v recovered=%d", result, err, recovered)
	}
	// 扁平 Cookie 写回失败：资料带 UpdatedCookies，且请求内关闭数据库。
	client2 := &hookedProfileMTopFake{result: &mtop.UserProfileResult{Nickname: "n", UpdatedCookies: "unb=1; flat=new"}, hook: func(*mtop.CookieSession) { _ = store.DB.Close() }}
	port, store = newPort(t, client2, &recovered)
	// err、result 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if result, err := port.RefreshProfile(ctx, input); err != nil || result.ErrorMessage == "" {
		t.Fatalf("扁平写回失败: %+v %v", result, err)
	}
	// 资料保存失败仅告警：成功资料但昵称头像为空时回退兜底值；用触发器让昵称更新失败。
	client3 := &hookedProfileMTopFake{result: &mtop.UserProfileResult{}}
	port, store = newPort(t, client3, &recovered)
	if _, err := store.DB.ExecContext(ctx, `CREATE TRIGGER block_profile BEFORE UPDATE OF nickname ON cookies BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	input.Summary = accountapp.Summary{ID: "cid", Nickname: "缓存昵称", AvatarURL: "//img.example/a.png"}
	// err、result 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if result, err := port.RefreshProfile(ctx, input); err != nil || result.Nickname != "缓存昵称" || result.AvatarURL != "https://img.example/a.png" || result.ErrorMessage != "" {
		t.Fatalf("兜底资料: %+v %v", result, err)
	}
}

// TestAccountSettingsRepositoryOwnershipAndTokens 覆盖账号设置的非所有者拒绝、令牌清理与凭证锁路径。
func TestAccountSettingsRepositoryOwnershipAndTokens(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// admin 是账号所有者；stranger 是不拥有账号的用户标识。
	admin, err := store.Users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// stranger 是不存在的其他用户 ID，用于触发越权分支。
	stranger := admin.ID + 100
	// repository 是被测账号设置适配器。
	repository := NewAccountSettingsRepository(store)
	repository.LockCredentials("cid")()
	if err := repository.ClearTokens(ctx, "cid"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetStatusOwned(ctx, stranger, "cid", false, "x"); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatalf("SetStatusOwned: %v", err)
	}
	if _, err := repository.StatusOwned(ctx, stranger, "cid"); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatalf("StatusOwned: %v", err)
	}
	if _, err := repository.SetPauseOwned(ctx, stranger, "cid", 1); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatalf("SetPauseOwned: %v", err)
	}
	if _, err := repository.GetPauseOwned(ctx, stranger, "cid"); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatalf("GetPauseOwned: %v", err)
	}
	// 数据库关闭后归属复核本身失败，错误经规范化后返回。
	cleanup()
	if _, err := repository.StatusOwned(ctx, admin.ID, "cid"); err == nil {
		t.Fatal("数据库故障应传播")
	}
	// 账号摘要适配器同样验证越权、数据库故障与错误规范化。
	summary := NewAccountSummaryRepository(store)
	if _, err := summary.StatusOwned(ctx, admin.ID, "cid"); err == nil {
		t.Fatal("摘要状态查询应传播数据库故障")
	}
	if _, err := summary.ListAdminSummaries(ctx); err == nil {
		t.Fatal("管理员摘要查询应传播数据库故障")
	}
	if !errors.Is(normalizeAccountSummaryError(db.ErrForbidden), accountapp.ErrForbidden) {
		t.Fatal("Forbidden 应映射为应用层错误")
	}
}

// TestPersistCookieSessionLockedBranches 覆盖 Cookie 会话持久化的空入参、未变更、仓储缺失与写回失败路径。
func TestPersistCookieSessionLockedBranches(t *testing.T) {
	ctx := context.Background()
	// changed、err、handled、value 依次为：标记凭证或会话内容是否发生变化，用于断言；保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言；保存被测函数返回的 Cookie 值，用于断言。
	if value, changed, handled, err := PersistCookieSessionLocked(ctx, nil, nil, nil); value != "" || changed || handled || err != nil {
		t.Fatalf("空入参: %q %v %v %v", value, changed, handled, err)
	}
	// detail 是持久化前的凭证视图。
	detail := &accountapp.CredentialDetail{ID: "cid", Value: "unb=1"}
	// snapshot 是权威 Cookie Jar 的初始内容。
	snapshot := []BrowserCookie{{Name: "unb", Value: "1", Domain: ".goofish.com", Path: "/"}}
	// _, session 是带权威快照但未发生变化的会话。
	_, session := WithCookieSnapshot(ctx, snapshot)
	// changed、err、handled 依次为：标记凭证或会话内容是否发生变化，用于断言；保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言。
	if _, changed, handled, err := PersistCookieSessionLocked(ctx, nil, detail, session); changed || !handled || err != nil {
		t.Fatalf("未变更带快照: %v %v %v", changed, handled, err)
	}
	// _, flat 是未变更的扁平会话，应视为未处理。
	_, flat := WithFlatCookieSession(ctx, "unb=1")
	// changed、err、handled 依次为：标记凭证或会话内容是否发生变化，用于断言；保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言。
	if _, changed, handled, err := PersistCookieSessionLocked(ctx, nil, detail, flat); changed || handled || err != nil {
		t.Fatalf("未变更扁平: %v %v %v", changed, handled, err)
	}
	// 变更后的会话在仓储缺失时报错。
	session.ReplaceSnapshot([]BrowserCookie{{Name: "unb", Value: "2", Domain: ".goofish.com", Path: "/"}})
	// err、handled 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言。
	if _, _, handled, err := PersistCookieSessionLocked(ctx, nil, detail, session); !handled || err == nil {
		t.Fatalf("仓储缺失: %v %v", handled, err)
	}
	// 仓储写回失败时携带错误。
	broken := NewAccountLoginRepository(&db.Store{})
	// err、handled 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言。
	if _, _, handled, err := PersistCookieSessionLocked(ctx, broken, detail, session); !handled || err == nil {
		t.Fatalf("写回失败: %v %v", handled, err)
	}
}

// TestAdapterConstructionHelpers 覆盖适配器构造辅助函数的空依赖与回退分支，以及运行时端口对未运行账号的行为。
func TestAdapterConstructionHelpers(t *testing.T) {
	ctx := context.Background()
	if newCredentialWakeService(nil) != nil {
		t.Fatal("空存储不应构造唤醒服务")
	}
	if browserManagerOrNil(nil) != nil || browserManagerOrNil(&browser.Manager{}) == nil {
		t.Fatal("浏览器接口转换错误")
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// 空协调器回退为默认实现。
	if newAdapter(store, nil, nil, nil).orderDetails == nil {
		t.Fatal("应回退默认订单详情协调器")
	}
	// manager 中没有任何运行实例。
	manager := accountmanager.NewManager(store, &Adapter{}, nil)
	// port 是运行时端口。
	port := NewAccountRuntimePort(manager)
	if err := port.UpdateCookie(ctx, "not-running", "unb=1"); err != nil {
		t.Fatalf("未运行账号应幂等成功: %v", err)
	}
	if err := port.Restart(ctx, "no-such-account"); err == nil {
		t.Fatal("账号不存在时重启应返回错误")
	}
}
