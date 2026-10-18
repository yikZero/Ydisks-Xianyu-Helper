package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/browser"
	"xianyu-go/internal/chat"
	"xianyu-go/internal/db"
	"xianyu-go/internal/engine"
	"xianyu-go/internal/renewal"
	"xianyu-go/internal/xianyu/cookierefresh"
	"xianyu-go/internal/xianyu/mtop"
	xrenew "xianyu-go/internal/xianyu/renew"
)

// engineCaptchaBrowser 同时实现带引擎标签与快照读取的 token 滑块恢复接口，用于覆盖引擎分支。
type engineCaptchaBrowser struct {
	// cookies 是恢复成功时返回的 Cookie 串。
	cookies string
	// engineName 是返回给适配器的引擎标签。
	engineName string
	err        error
	// snapshotErr 是读取完整 Cookie Jar 时返回的错误。
	snapshotErr error
}

// TokenCaptchaRecover 满足基础恢复接口，本测试替身只走带引擎的实现。
func (b *engineCaptchaBrowser) TokenCaptchaRecover(context.Context, string, string, string, bool, browser.TokenCaptchaURLProvider) (string, error) {
	return b.cookies, b.err
}

// TokenCaptchaRecoverWithEngine 返回预设的 Cookie、引擎标签与错误。
func (b *engineCaptchaBrowser) TokenCaptchaRecoverWithEngine(context.Context, string, string, string, bool, browser.TokenCaptchaURLProvider) (string, string, error) {
	return b.cookies, b.engineName, b.err
}

// TokenCaptchaCookieSnapshot 返回预设的快照读取错误，覆盖回退到 Go 快照合并的路径。
func (b *engineCaptchaBrowser) TokenCaptchaCookieSnapshot(context.Context, string, bool) (string, []cookierefresh.BrowserCookie, error) {
	return "", nil, b.snapshotErr
}

// TestAdapterChatSessionRoleAndSave 覆盖会话角色查询/保存在未装配与已装配聊天服务时的行为。
func TestAdapterChatSessionRoleAndSave(t *testing.T) {
	ctx := context.Background()
	// nilAdapter 是空指针适配器，所有入口必须安全。
	var nilAdapter *Adapter
	// err、row 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的会话行，用于断言。
	if row, err := nilAdapter.ChatSessionRole(ctx, "a", "c", "i"); err != nil || row.AccountRole != "unknown" {
		t.Fatalf("空适配器角色: %+v %v", row, err)
	}
	if err := nilAdapter.SaveChatSessionRole(ctx, "a", "c", "i", "seller", "b", "s", "x"); err == nil {
		t.Fatal("空适配器保存角色应失败")
	}
	// err、ok 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if ok, err := nilAdapter.ItemBelongsToAccount(ctx, "a", "i"); ok || err == nil {
		t.Fatalf("空适配器商品归属: %v %v", ok, err)
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 先不装配聊天服务，再装配。
	adapter := New(store, nil, nil)
	// err、row 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的会话行，用于断言。
	if row, err := adapter.ChatSessionRole(ctx, "cid", "chat", "item"); err != nil || row.AccountRole != "unknown" {
		t.Fatalf("未装配角色: %+v %v", row, err)
	}
	adapter.SetChatService(chat.New(store))
	if _, err := adapter.ChatSessionRole(ctx, "cid", "chat", "item"); err != nil {
		t.Fatalf("已装配角色查询: %v", err)
	}
	// 保存角色允许成功或因会话不存在失败，但必须经过聊天服务而不 panic。
	_ = adapter.SaveChatSessionRole(ctx, "cid", "chat", "item", "seller", "buyer", "seller-id", "local")
}

// TestAdapterHandleMessageReadFallbacks 覆盖已读回执按消息键未命中时回退最近出站消息，以及全部未命中时忽略。
func TestAdapterHandleMessageReadFallbacks(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 装配聊天服务。
	adapter := New(store, nil, nil)
	adapter.SetChatService(chat.New(store))
	if err := adapter.HandleOutgoingChatMessage(ctx, engine.OutgoingChatMessage{AccountID: "cid", ChatID: "chat-r", BuyerID: "buyer", MessageKey: "out-1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	// 消息键不存在但会话存在：回退到最近出站消息。
	if err := adapter.HandleMessageRead(ctx, engine.MessageReadEvent{AccountID: "cid", ChatID: "chat-r", MessageID: "unknown", ReadAt: 10}); err != nil {
		t.Fatalf("回退已读: %v", err)
	}
	// 会话和消息都不存在：忽略。
	if err := adapter.HandleMessageRead(ctx, engine.MessageReadEvent{AccountID: "cid", ChatID: "none", MessageID: "none", ReadAt: 10}); err != nil {
		t.Fatalf("未命中应忽略: %v", err)
	}
	// 无会话标识：不回退直接忽略。
	if err := adapter.HandleMessageRead(ctx, engine.MessageReadEvent{AccountID: "cid", MessageID: "none", ReadAt: 10}); err != nil {
		t.Fatalf("缺会话应忽略: %v", err)
	}
}

// TestAdapterOnAccountEventWithoutNotifier 覆盖未注入通知器时仅记录日志不 panic。
func TestAdapterOnAccountEventWithoutNotifier(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	New(store, nil, nil).OnAccountEvent(context.Background(), "cid", engine.EventSystemError, "warn", "t", "b")
}

// TestOnTokenCaptchaVerificationFailureBranches 覆盖 token 风控恢复的存储缺失、浏览器缺失、失败、空结果、引擎标签与快照回退路径。
func TestOnTokenCaptchaVerificationFailureBranches(t *testing.T) {
	ctx := context.Background()
	// notifier 记录安全验证事件。
	notifier := &fakeNotifier{}
	// bare 是没有任何子仓储的存储，模拟账号存储未初始化。
	bare := New(&db.Store{}, nil, nil)
	bare.SetNotifier(notifier)
	// ok、res 依次为：保存查询结果是否存在或操作是否成功的布尔标记，用于断言；保存被测函数返回的结果对象，用于断言其内容。
	if res, ok := bare.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); ok || res != nil || len(notifier.events) != 1 {
		t.Fatalf("存储缺失: %+v %v %+v", res, ok, notifier.events)
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 先不注入浏览器。
	adapter := New(store, nil, nil)
	adapter.SetNotifier(notifier)
	adapter.SetTokenCaptchaRequester(nil)
	// ok、res 依次为：保存查询结果是否存在或操作是否成功的布尔标记，用于断言；保存被测函数返回的结果对象，用于断言其内容。
	if res, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); ok || res != nil {
		t.Fatalf("浏览器缺失: %+v %v", res, ok)
	}
	// 浏览器失败：写入失败风控日志并通知。
	adapter.SetBrowser(&fakeBrowser{tokenCaptchaErr: errors.New("slider failed")})
	// ok、res 依次为：保存查询结果是否存在或操作是否成功的布尔标记，用于断言；保存被测函数返回的结果对象，用于断言其内容。
	if res, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); ok || res != nil {
		t.Fatalf("浏览器失败: %+v %v", res, ok)
	}
	// 浏览器返回空 Cookie：视为未恢复。
	adapter.SetBrowser(&fakeBrowser{tokenCaptchaResult: "  "})
	// ok、res 依次为：保存查询结果是否存在或操作是否成功的布尔标记，用于断言；保存被测函数返回的结果对象，用于断言其内容。
	if res, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); ok || res != nil {
		t.Fatalf("空 Cookie: %+v %v", res, ok)
	}
	// 带引擎标签的浏览器成功，快照读取失败后回退 Go 合并。
	adapter.SetBrowser(&engineCaptchaBrowser{cookies: "unb=1; x5sec=ok", engineName: "direct", snapshotErr: errors.New("jar unavailable")})
	// result、ok 是引擎标签路径的恢复结果。
	result, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev")
	if !ok || result == nil || result.CookieSnapshotComplete {
		t.Fatalf("引擎路径: %+v %v", result, ok)
	}
	// engineName 读取最近一条风控日志中记录的引擎。
	var engineName string
	if err := store.DB.QueryRowContext(ctx, `SELECT captcha_engine FROM risk_control_logs WHERE cookie_id='cid' ORDER BY id DESC LIMIT 1`).Scan(&engineName); err != nil || engineName != "direct" {
		t.Fatalf("引擎日志: %q %v", engineName, err)
	}
	// 账号不存在时保存 Cookie 失败。
	adapter.SetBrowser(&fakeBrowser{tokenCaptchaResult: "unb=1; x5sec=ok"})
	// ok、res 依次为：保存查询结果是否存在或操作是否成功的布尔标记，用于断言；保存被测函数返回的结果对象，用于断言其内容。
	if res, ok := adapter.OnTokenCaptchaVerification(ctx, "missing-account", "unb=1", "https://v", "dev"); ok || res != nil {
		t.Fatalf("保存失败: %+v %v", res, ok)
	}
}

// TestOnTokenCaptchaVerificationProviderBranches 覆盖链接重取回调在未注入、报错与空结果时的返回。
func TestOnTokenCaptchaVerificationProviderBranches(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// fb 记录回调结果。
	fb := &fakeBrowser{tokenCaptchaResult: "unb=1; x5sec=ok"}
	// adapter 注入浏览器与会报错的重取器。
	adapter := New(store, nil, nil)
	adapter.SetBrowser(fb)
	adapter.SetTokenCaptchaRequester(&fakeCaptchaRequester{err: errors.New("fresh failed")})
	// ok 保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if _, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); !ok {
		t.Fatal("重取失败不影响浏览器恢复")
	}
	if fb.providerURL != "" {
		t.Fatalf("重取失败应无链接: %q", fb.providerURL)
	}
	// 重取器返回空结果同样得到空链接。
	adapter.SetTokenCaptchaRequester(&fakeCaptchaRequester{})
	// ok 保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if _, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); !ok {
		t.Fatal("空结果不影响恢复")
	}
}

// TestOnTokenCaptchaVerificationRemoteFallsBackWhenUnreachable 覆盖远程服务不可达时回退本机逻辑。
func TestOnTokenCaptchaVerificationRemoteFallsBackWhenUnreachable(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// dead 是已关闭的远程端点。
	dead := httptest.NewServer(http.NotFoundHandler())
	// deadURL 已关闭的本地测试端点地址，用于模拟网络不可达。
	deadURL := dead.URL
	dead.Close()
	if err := store.Settings.SetMany(ctx, map[string]string{"captcha.remote_service_url": deadURL, "captcha.remote_secret_key": "s", "captcha.remote_pass_cookies": "true"}); err != nil {
		t.Fatal(err)
	}
	// adapter 回退到浏览器。
	adapter := New(store, nil, nil)
	adapter.SetBrowser(&fakeBrowser{tokenCaptchaResult: "unb=1; x5sec=ok"})
	// ok 保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if _, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); !ok {
		t.Fatal("远程不可达应回退本机")
	}
	// 远程明确失败不回退，直接按失败处理。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"success":false}`)) }))
	defer failing.Close()
	if err := store.Settings.SetMany(ctx, map[string]string{"captcha.remote_service_url": failing.URL}); err != nil {
		t.Fatal(err)
	}
	// ok 保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if _, ok := adapter.OnTokenCaptchaVerification(ctx, "cid", "unb=1", "https://v", "dev"); ok {
		t.Fatal("远程明确失败不应回退")
	}
}

// TestFetchOrderDetailAttemptErrorPaths 覆盖订单详情的账号缺失、空 Cookie、空结果、会话变化与凭证并发变更路径。
func TestFetchOrderDetailAttemptErrorPaths(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 使用可编排的订单详情客户端，并关闭账号限流等待以保持测试快速。
	adapter := New(store, nil, nil)
	adapter.orderDetails = newOrderDetailCoordinator(0, 0, nil)
	adapter.SetOrderDetailClient(&fakeOrderDetailClient{})
	if _, err := adapter.FetchOrderDetail(ctx, "missing", "o1", "i", "b", ""); err == nil || !strings.Contains(err.Error(), "读取订单账号最新 Cookie") {
		t.Fatalf("账号缺失: %v", err)
	}
	// 空 Cookie：清空账号 Cookie 后读取应报空。
	if err := store.Cookies.UpdateValueExisting(ctx, "cid", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.FetchOrderDetail(ctx, "cid", "o2", "i", "b", ""); err == nil {
		t.Fatal("空 Cookie 应失败")
	}
	if err := store.Cookies.UpdateValueExisting(ctx, "cid", "unb=1; _m_h5_tk=tk"); err != nil {
		t.Fatal(err)
	}
	// 平台返回空详情。
	if _, err := adapter.FetchOrderDetail(ctx, "cid", "o3", "i", "b", ""); err == nil || !strings.Contains(err.Error(), "空结果") {
		t.Fatalf("空详情: %v", err)
	}
	// 非会话过期的平台错误原样返回，不触发续期。
	adapter.SetOrderDetailClient(&fakeOrderDetailClient{err: errors.New("boom")})
	if _, err := adapter.FetchOrderDetail(ctx, "cid", "o4", "i", "b", ""); err == nil {
		t.Fatal("平台错误应返回")
	}
	// 会话过期但续期失败（冷却通过后续期服务不可用）：返回合并错误。
	renewal.GlobalCooldown.Reset("cid")
	adapter.SetOrderDetailClient(&fakeOrderDetailClient{err: errors.New("FAIL_SYS_SESSION_EXPIRED::Session过期")})
	adapter.SetRenewService(xrenew.Service{HTTPClient: &http.Client{Timeout: time.Second}, SilentHasLoginURL: "http://127.0.0.1:1", RetryDelay: -1})
	if _, err := adapter.FetchOrderDetail(ctx, "cid", "o5", "i", "b", ""); err == nil || !strings.Contains(err.Error(), "续期失败") {
		t.Fatalf("续期失败: %v", err)
	}
}

// concurrentCookieDetailClient 在详情请求期间改写账号 Cookie，模拟凭证被其他流程并发更新。
type concurrentCookieDetailClient struct {
	// mutate 在请求期间执行，用于改写数据库中的凭证。
	mutate func()
	// detail 是请求返回的订单详情。
	detail *mtop.OrderDetailResult
}

// FetchOrderDetail 先触发并发变更，再返回预设详情。
func (c *concurrentCookieDetailClient) FetchOrderDetail(context.Context, string, string) (*mtop.OrderDetailResult, error) {
	c.mutate()
	return c.detail, nil
}

// TestFetchOrderDetailDropsStaleCookieWhenCredentialChanged 覆盖外部调用期间凭证被更新时丢弃旧响应 Cookie。
func TestFetchOrderDetailDropsStaleCookieWhenCredentialChanged(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 使用会并发改写 Cookie 的客户端。
	adapter := New(store, nil, nil)
	adapter.SetOrderDetailClient(&concurrentCookieDetailClient{
		mutate: func() { _ = store.Cookies.UpdateValueExisting(ctx, "cid", "unb=1; _m_h5_tk=other") },
		detail: &mtop.OrderDetailResult{Quantity: "1", Amount: "1", UpdatedCookies: "unb=1; stale=1"},
	})
	if _, err := adapter.FetchOrderDetail(ctx, "cid", "o6", "i", "b", ""); err != nil {
		t.Fatal(err)
	}
	// saved 是最终保存的 Cookie，不应包含旧响应写入的内容。
	saved, _ := store.Cookies.GetValue(ctx, "cid")
	if strings.Contains(saved, "stale") {
		t.Fatalf("旧响应 Cookie 不应覆盖并发更新: %q", saved)
	}
}

// TestOnPasswordLoginRefreshCoordinatorBranches 覆盖续期冷却、进行中、账号读取失败和成功/失败收尾路径。
func TestOnPasswordLoginRefreshCoordinatorBranches(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 使用独立冷却管理器，避免污染全局状态。
	adapter := New(store, nil, nil)
	adapter.cooldown = renewal.NewCooldownManager()
	// 账号不存在：读取失败路径。
	if adapter.OnPasswordLoginRefresh(ctx, "missing") {
		t.Fatal("账号缺失应失败")
	}
	// 协调器已有进行中的恢复：登记失败。
	if !adapter.beginPasswordLogin("busy") {
		t.Fatal("首次登记应成功")
	}
	if adapter.OnPasswordLoginRefresh(ctx, "busy") {
		t.Fatal("进行中应返回 false")
	}
	adapter.finishPasswordLogin("busy")
	// 无协调器时兼容入口安全。
	bare := &Adapter{}
	if bare.beginPasswordLogin("x") {
		t.Fatal("无协调器应返回 false")
	}
	bare.finishPasswordLogin("x")
	// 结果等待：无共享状态返回 false；已取消返回 false；完成后返回结果。
	if adapter.waitPasswordRenewalResult(ctx, "none") {
		t.Fatal("无状态应返回 false")
	}
	if !adapter.beginPasswordRenewalResult("w") || adapter.beginPasswordRenewalResult("w") {
		t.Fatal("登记语义错误")
	}
	// cancelled 是已取消上下文。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if adapter.waitPasswordRenewalResult(cancelled, "w") {
		t.Fatal("取消应返回 false")
	}
	adapter.finishPasswordRenewalResult("w", true)
	adapter.finishPasswordRenewalResult("w", true)
	if truncateMessage("abcdef", 3) != "abc" || truncateMessage("abc", 0) != "abc" {
		t.Fatal("truncateMessage 边界错误")
	}
}

// TestTryProtocolCredentialRenewBranches 覆盖协议续期的空入参、平台拒绝与无结果分支。
func TestTryProtocolCredentialRenewBranches(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 注入拒绝续期的本地服务。
	adapter := New(store, nil, nil)
	// err、ok 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if ok, err := adapter.tryProtocolCredentialRenew(ctx, nil); ok || err != nil {
		t.Fatalf("空入参: %v %v", ok, err)
	}
	// srv 返回业务失败码，驱动 Success=false 且无 pending 的分支。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":{"data":{"processFinished":true,"resultCode":500}}}`))
	}))
	defer srv.Close()
	adapter.SetRenewService(xrenew.Service{HTTPClient: srv.Client(), SilentHasLoginURL: srv.URL, HasLoginURL: srv.URL, RetryDelay: -1})
	// data 是带可续期标记的平台运行数据。
	data := &db.CookiePlatformRuntimeData{ID: "cid", Value: "unb=1; havana_lgc_exp=9999999999999"}
	// err、ok 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if ok, err := adapter.tryProtocolCredentialRenew(ctx, data); ok || err == nil {
		t.Fatalf("业务失败应返回错误: %v %v", ok, err)
	}
	// 没有续期标记的 Cookie：服务给出跳过或失败，同样不得成功。
	if ok, _ := adapter.tryProtocolCredentialRenew(ctx, &db.CookiePlatformRuntimeData{ID: "cid", Value: "unb=1"}); ok {
		t.Fatal("无续期标记不应成功")
	}
}
