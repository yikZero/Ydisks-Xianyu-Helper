package qrlogin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/cookierefresh"
)

// TestLifecycleGuardsOnNilContextAndClosedManager 覆盖关闭管理器时的空接收者、空 Context、已取消 Context 与会话根 Context 补齐。
func TestLifecycleGuardsOnNilContextAndClosedManager(t *testing.T) {
	// nilManager 保存空管理器指针，用于验证空接收者调用不会 panic。
	var nilManager *Manager
	if err := nilManager.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	// manager 是新构造的管理器。
	manager := NewManager(nil)
	// nilCtx 模拟缺少关闭 Context。
	var nilCtx context.Context
	if err := manager.CloseContext(nilCtx); err == nil {
		t.Fatal("空 Context 必须被拒绝")
	}
	// cancelled 是已取消的关闭 Context。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.CloseContext(cancelled); err == nil {
		t.Fatal("已取消的关闭 Context 必须返回错误")
	}
	// legacy 管理器含一个未补齐根 Context 的历史会话，且没有根 Context 和取消表。
	legacy := &Manager{sessions: map[string]*Session{"s1": {}}}
	manager.mu.Lock()
	// ctx 保存被测调用使用的上下文；ok 保存被测函数返回的成功或命中标志。
	ctx, ok := manager.sessionContextLocked("missing")
	manager.mu.Unlock()
	if ok || ctx != nil {
		t.Fatal("缺失会话不应返回 Context")
	}
	legacy.mu.Lock()
	ctx, ok = legacy.sessionContextLocked("s1")
	legacy.mu.Unlock()
	if !ok || ctx == nil {
		t.Fatal("历史会话应被补齐根 Context")
	}
	// dangling 的会话存在，但取消函数被移除，表示已被删除。
	dangling := NewManager(nil)
	dangling.sessions["s2"] = &Session{lifecycleCtx: context.Background()}
	dangling.mu.Lock()
	ctx, ok = dangling.sessionContextLocked("s2")
	dangling.mu.Unlock()
	if ok || ctx != nil {
		t.Fatal("无取消函数的会话不应继续启动任务")
	}
}

// TestFaceCookieJarAndHelpers 覆盖人脸验证 Cookie Jar 的空值防御、权威/推断快照与辅助函数空输入。
func TestFaceCookieJarAndHelpers(t *testing.T) {
	// nilJar 保存空 Cookie Jar 指针，用于验证空接收者调用不会 panic。
	var nilJar *faceCookieJar
	if nilJar.Cookies(&url.URL{}) != nil {
		t.Fatal("空 Jar 应返回空")
	}
	nilJar.SetCookies(&url.URL{}, []*http.Cookie{{Name: "a"}})
	// ok 保存被测函数返回的成功或命中标志；snapshot 保存被测对象返回的 Cookie 快照。
	if snapshot, ok := nilJar.Snapshot(); snapshot != nil || ok {
		t.Fatal("空 Jar 快照应为空")
	}
	// authoritative 使用显式空快照，视为权威 Jar。
	authoritative := newFaceCookieJar(nil, []cookierefresh.BrowserCookie{})
	// ok 保存被测函数返回的成功或命中标志；snapshot 保存被测对象返回的 Cookie 快照。
	if snapshot, ok := authoritative.Snapshot(); !ok || snapshot == nil {
		t.Fatal("权威空 Jar 应返回非空切片")
	}
	// u 是写入与读取使用的目标地址。
	u := &url.URL{Scheme: "https", Host: "passport.goofish.com", Path: "/"}
	authoritative.SetCookies(u, []*http.Cookie{nil, {Name: " "}})
	authoritative.SetCookies(u, []*http.Cookie{{Name: "a", Value: "1", Domain: "goofish.com", Path: "/"}})
	authoritative.SetCookies(nil, nil)
	// cookies 保存被测函数返回的 Cookie 集合。
	if cookies := authoritative.Cookies(nil); cookies != nil {
		t.Fatal("空地址应返回空")
	}
	// inferred 由扁平 Cookie 推断，不是权威快照。
	inferred := newFaceCookieJar(map[string]string{"unb": "1"}, nil)
	// ok 保存被测函数返回的成功或命中标志。
	if _, ok := inferred.Snapshot(); ok {
		t.Fatal("推断 Jar 不应声称权威")
	}
	setJarCookies(nil, u, map[string]string{"a": "1"})
	setJarCookies(authoritative, nil, map[string]string{"a": "1"})
	if got := collectJarCookies(nil, u); len(got) != 0 {
		t.Fatal("空 Jar 应返回空集合")
	}
	if got := collectJarCookies(authoritative, nil); len(got) != 0 {
		t.Fatal("空地址应跳过")
	}
	if cloneCookieSnapshot(nil) != nil || cloneCookieSnapshot([]cookierefresh.BrowserCookie{}) == nil {
		t.Fatal("克隆快照空值语义错误")
	}
	absorbSessionResponse(nil, "", nil)
	absorbSessionResponse(&Session{}, "", nil)
	finalizeSessionCredentialsLocked(nil)
	if mustParseURL("http://[::1") != nil {
		t.Fatal("非法地址应返回空")
	}
	if _, err := renderQRDataURL(string(make([]byte, 5000))); err == nil {
		t.Fatal("超长内容无法编码为二维码")
	}
}

// TestRunFaceVerificationPreconditions 覆盖人脸验证启动前的会话缺失与缺少临时 Cookie。
func TestRunFaceVerificationPreconditions(t *testing.T) {
	// manager 是没有任何会话的管理器。
	manager := NewManager(nil)
	if err := manager.runFaceVerification(context.Background(), "none", "https://x"); err == nil {
		t.Fatal("会话缺失必须报错")
	}
	manager.sessions["empty"] = &Session{cookies: map[string]string{}}
	if err := manager.runFaceVerification(context.Background(), "empty", "https://x"); err == nil {
		t.Fatal("缺少临时 Cookie 必须报错")
	}
}

// TestFaceStatusPollingBranches 覆盖人脸状态检查的请求失败、非 JSON 正文、未完成状态与会话过期退出。
func TestFaceStatusPollingBranches(t *testing.T) {
	// body 决定桩服务本次返回的正文。
	body := "not json"
	// server 返回当前正文。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	// manager 把所有请求转发到桩服务。
	manager, _, _ := newStubbedManager(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	// client 复用管理器的桩传输。
	client := manager.httpc
	if _, _, err := manager.checkFaceVerification(context.Background(), client, "ht"); err == nil {
		t.Fatal("非 JSON 必须报错")
	}
	body = `{"content":{"code":1}}`
	// done 保存被测函数返回的是否完成标志；err 保存被测调用返回的错误，用于断言失败路径或成功路径是否符合预期。
	if _, done, err := manager.checkFaceVerification(context.Background(), client, "ht"); err != nil || done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	// cancelled 使请求在发出前失败。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := manager.checkFaceVerification(cancelled, client, "ht"); err == nil {
		t.Fatal("取消的请求必须报错")
	}
	if _, err := manager.faceGetHTML(cancelled, client, "https://x", "ref"); err == nil {
		t.Fatal("取消的请求必须报错")
	}
	if _, err := manager.waitFaceVerification(context.Background(), client, "missing", "ht"); err == nil {
		t.Fatal("会话缺失必须报错")
	}
	// expired 会话创建于一小时前且有效期仅一分钟。
	manager.sessions["expired"] = &Session{createdTime: time.Now().Add(-time.Hour), expireTime: time.Minute}
	if _, err := manager.waitFaceVerification(context.Background(), client, "expired", "ht"); err == nil {
		t.Fatal("过期会话必须报错")
	}
	if _, err := json.Marshal(map[string]string{}); err != nil {
		t.Fatal(err)
	}
}
