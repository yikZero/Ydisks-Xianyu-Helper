package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accountapp "xianyu-go/internal/application/account"
)

// authGapPort 是认证端口桩，每个方法返回字段中预设的结果。
type authGapPort struct {
	// initialized、initializedErr 是 IsSystemInitialized 的返回。
	initialized    bool
	initializedErr error
	// initErr 是 InitializeAdmin 的错误。
	initErr error
	// emailName、emailErr 是 UsernameByEmail 的返回。
	emailName string
	emailErr  error
	// loginSID、loginUser、loginErr 是 Login 的返回。
	loginSID  string
	loginUser *accountapp.AuthUser
	loginErr  error
	// verifyUser、verifyOK、verifyErr 是 VerifyPassword 的返回。
	verifyUser accountapp.AuthUser
	verifyOK   bool
	verifyErr  error
	// updatePasswordErr 是 UpdatePassword 的错误。
	updatePasswordErr error
	// updateCredentialsErr 是 UpdateCredentials 的错误。
	updateCredentialsErr error
}

// IsSystemInitialized 返回预设初始化状态。
func (p *authGapPort) IsSystemInitialized(context.Context) (bool, error) {
	return p.initialized, p.initializedErr
}

// InitializeAdmin 返回预设错误。
func (p *authGapPort) InitializeAdmin(context.Context, string, string) (bool, error) {
	return true, p.initErr
}

// UsernameByEmail 返回预设用户名。
func (p *authGapPort) UsernameByEmail(context.Context, string) (string, error) {
	return p.emailName, p.emailErr
}

// Login 返回预设登录结果。
func (p *authGapPort) Login(context.Context, string, string) (string, *accountapp.AuthUser, error) {
	return p.loginSID, p.loginUser, p.loginErr
}

// VerifyPassword 返回预设校验结果。
func (p *authGapPort) VerifyPassword(context.Context, string, string) (accountapp.AuthUser, bool, error) {
	return p.verifyUser, p.verifyOK, p.verifyErr
}

// UpdatePassword 返回预设错误。
func (p *authGapPort) UpdatePassword(context.Context, string, string) (bool, error) {
	return true, p.updatePasswordErr
}

// UpdateCredentials 返回预设错误。
func (p *authGapPort) UpdateCredentials(context.Context, int64, string, string) error {
	return p.updateCredentialsErr
}

// ValidateSession 恒成功。
func (p *authGapPort) ValidateSession(context.Context, string, int64) error { return nil }

// TestInitializeHandlerGapBranches 覆盖首次初始化的格式、状态检查、写入与自动登录失败分支。
func TestInitializeHandlerGapBranches(t *testing.T) {
	// srv、_、cleanup 是基础测试服务及资源释放函数。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// port 是可变认证桩。
	port := &authGapPort{}
	srv.applications.authentication = port
	handler := srv.Router()
	// post 提交初始化请求并返回状态码。
	post := func(body string) int {
		// rec 保存响应。
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/initialize", strings.NewReader(body)))
		return rec.Code
	}
	// goodBody 是合法初始化请求。
	const goodBody = `{"password":"longenough"}`
	if code := post("{"); code != http.StatusBadRequest {
		t.Errorf("bad json status=%d", code)
	}
	port.initializedErr = errors.New("db")
	if code := post(goodBody); code != http.StatusInternalServerError {
		t.Errorf("status check error=%d", code)
	}
	port.initializedErr = nil
	port.initErr = errors.New("init failed")
	if code := post(goodBody); code != http.StatusInternalServerError {
		t.Errorf("init error=%d", code)
	}
	port.initErr = nil
	if code := post(goodBody); code != http.StatusInternalServerError {
		t.Errorf("auto login failure=%d", code)
	}
}

// TestLoginHandlerGapBranches 覆盖登录的限流、邮箱登录失败与缺少凭据分支。
func TestLoginHandlerGapBranches(t *testing.T) {
	// srv、_、cleanup 是基础测试服务及资源释放函数。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// port 是可变认证桩。
	port := &authGapPort{}
	srv.applications.authentication = port
	handler := srv.Router()
	// post 提交登录请求并返回响应记录。
	post := func(body string) *httptest.ResponseRecorder {
		// rec 保存响应。
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body)))
		return rec
	}
	if rec := post(`{"email":"a@b.c","password":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown email status=%d", rec.Code)
	}
	port.emailName = "bob"
	if rec := post(`{"email":"a@b.c","password":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("email login failure status=%d", rec.Code)
	}
	port.loginSID, port.loginUser = "sid", &accountapp.AuthUser{ID: 2, Username: "bob"}
	if rec := post(`{"email":"a@b.c","password":"x"}`); rec.Code != http.StatusOK {
		t.Errorf("email login status=%d", rec.Code)
	}
	if rec := post(`{"username":"bob"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing password status=%d", rec.Code)
	}
	// 连续失败触发限流。
	port.loginSID, port.loginUser, port.loginErr = "", nil, errors.New("bad")
	// limited 记录是否观察到 429。
	limited := false
	// i 循环序号，用于生成不同的测试数据
	for i := 0; i <= loginFailuresPerPrincipal+1; i++ {
		if rec := post(`{"username":"victim","password":"x"}`); rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("missing Retry-After")
			}
			break
		}
	}
	if !limited {
		t.Error("expected rate limiting")
	}
}

// TestPasswordAndCredentialHandlersGapBranches 覆盖密码修改与凭据修改的权限、校验和持久化失败分支。
func TestPasswordAndCredentialHandlersGapBranches(t *testing.T) {
	// srv、_、cleanup 是基础测试服务及资源释放函数。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)
	// port 是注入的认证桩。
	port := &authGapPort{verifyUser: accountapp.AuthUser{ID: 1, Username: "admin"}}
	srv.applications.authentication = port

	// bad 描述两类密码修改接口的校验失败。
	for _, path := range []string{"/change-admin-password", "/change-password"} {
		if rec := serveChatCoverageRequest(handler, cookie, "POST", path, "{"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad json=%d", path, rec.Code)
		}
		if rec := serveChatCoverageRequest(handler, cookie, "POST", path, `{"current_password":"a","new_password":"short"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("%s short password=%d", path, rec.Code)
		}
		if rec := serveChatCoverageRequest(handler, cookie, "POST", path, `{"current_password":"a","new_password":"longenough"}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s wrong current=%d", path, rec.Code)
		}
		port.verifyOK = true
		port.updatePasswordErr = errors.New("db")
		if rec := serveChatCoverageRequest(handler, cookie, "POST", path, `{"current_password":"a","new_password":"longenough"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s update failure=%d", path, rec.Code)
		}
		port.verifyOK = false
		port.updatePasswordErr = nil
	}

	// credentials 路径为旧版凭据修改接口。
	const credentials = "/account/credentials"
	if rec := serveChatCoverageRequest(handler, cookie, "PUT", credentials, "{"); rec.Code != http.StatusBadRequest {
		t.Errorf("credentials bad json=%d", rec.Code)
	}
	// body 构造凭据修改请求。
	body := func(username, current, next string) string {
		return `{"new_username":"` + username + `","current_password":"` + current + `","new_password":"` + next + `"}`
	}
	// cases 描述凭据修改的各类失败。
	cases := []struct {
		// name 是子场景名称。
		name string
		// body 是请求体。
		body string
		// verifyErr、verifyOK 配置密码校验结果。
		verifyErr error
		verifyOK  bool
		// updateErr 是 UpdateCredentials 的错误。
		updateErr error
		want      int
	}{
		{"short username", body("ab", "pw", ""), nil, true, nil, 400},
		{"missing current", body("newname", " ", ""), nil, true, nil, 400},
		{"short new password", body("newname", "pw", "short"), nil, true, nil, 400},
		{"nothing changed", body("admin", "pw", ""), nil, true, nil, 400},
		{"verify infra error", body("newname", "pw", ""), errors.New("db"), false, nil, 500},
		{"password mismatch error", body("newname", "pw", ""), accountapp.ErrPasswordMismatch, false, nil, 401},
		{"wrong password", body("newname", "pw", ""), nil, false, nil, 401},
		{"username taken", body("newname", "pw", ""), nil, true, accountapp.ErrUsernameTaken, 409},
		{"update failure", body("newname", "pw", ""), nil, true, errors.New("db"), 500},
		{"success", body("newname", "pw", "longenough"), nil, true, nil, 200},
	}
	// tc 表示当前凭据修改场景。
	for _, tc := range cases {
		port.verifyErr, port.verifyOK, port.updateCredentialsErr = tc.verifyErr, tc.verifyOK, tc.updateErr
		if rec := serveChatCoverageRequest(handler, cookie, "PUT", credentials, tc.body); rec.Code != tc.want {
			t.Errorf("%s => %d want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}
