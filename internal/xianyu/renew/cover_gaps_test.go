package renew

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/cookierefresh"
)

// TestRenewAPIFirstEmptyCookiesAndCancelledDelay 验证空凭证直接要求密码登录，以及请求前等待被取消时返回取消错误。
func TestRenewAPIFirstEmptyCookiesAndCancelledDelay(t *testing.T) {
	// result 与 err 是空 Cookie 的续期结果。
	result, err := Service{RetryDelay: -1}.RenewAPIFirst(context.Background(), "  ")
	if err != nil || result.RenewMethod != "none" || !result.NeedPasswordLogin {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// cancelled 是已取消的上下文，配合正延迟触发等待取消分支。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// future 是未来的长登录到期时间，使续期不被跳过。
	future := "havana_lgc_exp=" + time.Now().Add(time.Hour).Format("20060102")
	result, err = Service{RetryDelay: time.Hour}.RenewAPIFirst(cancelled, "unb=1; "+future)
	if err == nil || result == nil || !result.NeedPasswordLogin {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// TestAutoLoginModeFallsThroughToExpired 验证没有任何有效长登录到期时间时跳过续期。
func TestAutoLoginModeFallsThroughToExpired(t *testing.T) {
	// mode 保存被测函数选择的续期模式；reason 保存被测函数返回的跳过或失败原因。
	if mode, reason := autoLoginModeWithoutFatigue(map[string]string{}, time.Now()); mode != "" || reason != "long_login_expired" {
		t.Fatalf("mode=%q reason=%q", mode, reason)
	}
	if renewBusinessOK(nil) || renewBusinessOK([]byte(`{"content":{"data":{"processFinished":true,"resultCode":1}}}`)) {
		t.Fatal("空正文或非 100 结果码不应成功")
	}
}

// TestCallAutoLoginAndLongLoginRequestErrors 覆盖非法端点、请求失败、业务未确认与默认 HTTP 客户端。
func TestCallAutoLoginAndLongLoginRequestErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (Service{SilentHasLoginURL: "http://[::1"}).callAutoLogin(ctx, "unb=1", "havana"); err == nil {
		t.Fatal("非法端点必须报错")
	}
	if _, err := (Service{QueryLoginSettingsURL: "http://[::1"}).QueryLongLoginSettings(ctx, "unb=1"); err == nil {
		t.Fatal("非法查询端点必须报错")
	}
	if _, err := (Service{SetLoginSettingsURL: "http://[::1"}).SetLongLoginSettings(ctx, "unb=1", true); err == nil {
		t.Fatal("非法设置端点必须报错")
	}
	// closed 是已关闭服务，用于请求失败分支。
	closed := httptest.NewServer(http.NotFoundHandler())
	// closedURL 保存已关闭桩服务的地址，用于制造连接被拒绝。
	closedURL := closed.URL
	closed.Close()
	if _, err := (Service{QueryLoginSettingsURL: closedURL, HTTPClient: &http.Client{}}).QueryLongLoginSettings(ctx, "unb=1"); err == nil {
		t.Fatal("连接失败必须报错")
	}
	// rejecting 的设置端点业务结果为失败；nil HTTPClient 使用默认客户端。
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":{"data":{"success":false}}}`))
	}))
	defer rejecting.Close()
	if _, err := (Service{SetLoginSettingsURL: rejecting.URL}).SetLongLoginSettings(ctx, "unb=1", true); err == nil {
		t.Fatal("业务未确认必须报错")
	}
	// 非 nil 空快照视为权威 Jar，覆盖空快照规范化分支。
	if _, err := (Service{QueryLoginSettingsURL: rejecting.URL}).QueryLongLoginSettings(ctx, "", []cookierefresh.BrowserCookie{}); err == nil {
		t.Fatal("缺少 returnValue 必须报错")
	}
}

// TestDoRenewRequestDefaultClientAndCancellation 验证未设置 HTTP 客户端时使用默认客户端，以及调用方取消时返回取消错误。
func TestDoRenewRequestDefaultClientAndCancellation(t *testing.T) {
	// release 在测试结束前放行被阻塞的服务端处理。
	release := make(chan struct{})
	// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	// ctx 在请求发出后立即取消。
	ctx, cancel := context.WithCancel(context.Background())
	// req 是指向阻塞服务的请求。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// callErr 保存被测调用返回的错误，用于判断失败是否按预期传播。
	if _, callErr := (Service{PromiseTimeout: time.Hour}).doRenewRequest(req, "silentHasLogin"); callErr == nil {
		t.Fatal("取消的请求必须返回错误")
	}
}
