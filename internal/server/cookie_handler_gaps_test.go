package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	accountapp "xianyu-go/internal/application/account"
)

// cookieGapSummary 包装账号摘要端口，按需让列表、详情或状态查询失败。
type cookieGapSummary struct {
	// AccountSummaryPort 提供其余摘要能力的真实实现。
	AccountSummaryPort
	// listIDsErr 是 ListOwnedIDs 返回的错误。
	listIDsErr error
	// listSummariesErr 是 ListSummaries 返回的错误。
	listSummariesErr error
	// statusErr 是 StatusOwned 返回的错误。
	statusErr error
	// statusEnabled 是 StatusOwned 成功时返回的启用状态。
	statusEnabled bool
	// ids 非空时替代 ListOwnedIDs 的真实结果。
	ids []string
}

// ListOwnedIDs 返回预设错误、预设账号或真实结果。
func (p cookieGapSummary) ListOwnedIDs(ctx context.Context, userID int64) ([]string, error) {
	if p.listIDsErr != nil {
		return nil, p.listIDsErr
	}
	if p.ids != nil {
		return p.ids, nil
	}
	return p.AccountSummaryPort.ListOwnedIDs(ctx, userID)
}

// ListSummaries 返回预设错误或真实结果。
func (p cookieGapSummary) ListSummaries(ctx context.Context, userID int64) ([]accountapp.AccountSummary, error) {
	if p.listSummariesErr != nil {
		return nil, p.listSummariesErr
	}
	return p.AccountSummaryPort.ListSummaries(ctx, userID)
}

// StatusOwned 返回预设错误或预设启用状态。
func (p cookieGapSummary) StatusOwned(context.Context, int64, string) (bool, error) {
	return p.statusEnabled, p.statusErr
}

// cookieGapRuntime 是账号运行时端口桩。
type cookieGapRuntime struct {
	// statuses 是 RuntimeStatuses 返回的快照。
	statuses map[string]accountapp.RuntimeStatus
	// statusesErr 是 RuntimeStatuses 返回的错误。
	statusesErr error
	// updateErr 是 UpdateCookie 返回的错误。
	updateErr error
}

// UpdateCookie 返回预设错误。
func (r cookieGapRuntime) UpdateCookie(context.Context, string, string) error { return r.updateErr }

// RuntimeStatuses 返回预设快照。
func (r cookieGapRuntime) RuntimeStatuses(context.Context) (map[string]accountapp.RuntimeStatus, error) {
	return r.statuses, r.statusesErr
}

// Restart 不会被调用。
func (r cookieGapRuntime) Restart(context.Context, string) error { return nil }

// RecoverExpiredCredential 不会被调用。
func (r cookieGapRuntime) RecoverExpiredCredential(context.Context, string) bool { return false }

// cookieGapDelete 是账号删除端口桩。
type cookieGapDelete struct {
	err error
}

// Delete 返回预设错误。
func (d cookieGapDelete) Delete(context.Context, int64, string) error { return d.err }

// cookieGapProfile 是账号资料刷新端口桩。
type cookieGapProfile struct {
	err error
}

// RefreshProfile 返回预设错误。
func (p cookieGapProfile) RefreshProfile(context.Context, int64, string) (accountapp.ProfileResult, error) {
	return accountapp.ProfileResult{}, p.err
}

// cookieGapSettings 是账号设置端口桩，仅覆盖启停与设置保存。
type cookieGapSettings struct {
	// AccountSettingsPort 提供其余设置能力的真实实现。
	AccountSettingsPort
	// statusResult、statusErr 是 SetStatus 的返回。
	statusResult accountapp.StatusResult
	statusErr    error
	// settingsResult、settingsErr 是 UpdateSettings 的返回。
	settingsResult accountapp.SettingsResult
	settingsErr    error
	// loginInfoErr 是 UpdateLoginInfo 的返回。
	loginInfoErr error
}

// SetStatus 返回预设结果。
func (s cookieGapSettings) SetStatus(context.Context, int64, string, bool) (accountapp.StatusResult, error) {
	return s.statusResult, s.statusErr
}

// UpdateSettings 返回预设结果。
func (s cookieGapSettings) UpdateSettings(context.Context, accountapp.SettingsUpdateInput) (accountapp.SettingsResult, error) {
	return s.settingsResult, s.settingsErr
}

// UpdateLoginInfo 返回预设错误。
func (s cookieGapSettings) UpdateLoginInfo(context.Context, accountapp.LoginInfoUpdateInput) error {
	return s.loginInfoErr
}

// TestCookieEndpointsRejectUnknownAccountsAndBadBodies 覆盖账号接口在账号不存在和请求体非法时的提前返回。
func TestCookieEndpointsRejectUnknownAccountsAndBadBodies(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// unknownRoutes 是需要先校验账号归属的接口。
	unknownRoutes := []struct{ method, path string }{
		{"GET", "/cookies/ghost/long-login"}, {"PUT", "/cookies/ghost/long-login"},
		{"PUT", "/cookies/ghost/settings"}, {"PUT", "/cookies/ghost/login-info"},
		{"PUT", "/cookies/ghost/status"}, {"DELETE", "/cookies/ghost"},
		{"PUT", "/cookies/ghost/auto-confirm"}, {"GET", "/cookies/ghost/auto-confirm"},
		{"PUT", "/cookies/ghost/remark"}, {"PUT", "/cookies/ghost/pause-duration"},
		{"GET", "/cookies/ghost/pause-duration"}, {"PUT", "/cookies/ghost"},
	}
	// route 表示当前未知账号请求。
	for _, route := range unknownRoutes {
		if rec := serveChatCoverageRequest(handler, cookie, route.method, route.path, `{}`); rec.Code < 400 {
			t.Errorf("%s %s status=%d", route.method, route.path, rec.Code)
		}
	}
	// badBodyRoutes 是账号存在但请求体非法的接口。
	badBodyRoutes := []struct{ method, path string }{
		{"PUT", "/cookies/acc1/long-login"}, {"PUT", "/cookies/acc1/settings"},
		{"PUT", "/cookies/acc1/login-info"}, {"PUT", "/cookies/acc1/status"},
		{"PUT", "/cookies/acc1/pause-duration"}, {"PUT", "/cookies/acc1/remark"},
		{"PUT", "/cookies/acc1/auto-confirm"},
	}
	// route 表示当前坏请求体请求。
	for _, route := range badBodyRoutes {
		if rec := serveChatCoverageRequest(handler, cookie, route.method, route.path, `{`); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s status=%d", route.method, route.path, rec.Code)
		}
	}
}

// TestCookieListAndRuntimeStatusGapBranches 覆盖账号列表、详情和运行状态的读取失败与状态合并分支。
func TestCookieListAndRuntimeStatusGapBranches(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// real 是原始账号摘要端口。
	real := srv.applications.accountSummaries
	handler := srv.Router()
	cookie := loginHelper(t, handler)
	// boom 是通用读取错误。
	boom := errors.New("boom")

	srv.applications.accountSummaries = cookieGapSummary{AccountSummaryPort: real, listIDsErr: boom}
	// path 当前遍历的请求路径
	for _, path := range []string{"/cookies", "/cookies/runtime-status"} {
		if rec := serveChatCoverageRequest(handler, cookie, "GET", path, ""); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s ids error status=%d", path, rec.Code)
		}
	}
	srv.applications.accountSummaries = cookieGapSummary{AccountSummaryPort: real, listSummariesErr: boom}
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/cookies/details", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("details error status=%d", rec.Code)
	}

	srv.applications.accountSummaries = cookieGapSummary{AccountSummaryPort: real, ids: []string{"a", "b", "c", "d"}, statusEnabled: true}
	srv.applications.accountRuntime = cookieGapRuntime{statusesErr: boom}
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/cookies/runtime-status", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("runtime error status=%d", rec.Code)
	}
	srv.applications.accountRuntime = cookieGapRuntime{statuses: map[string]accountapp.RuntimeStatus{"a": {State: "online", Connected: true}, "b": {State: "stopped"}}}
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/cookies/runtime-status", ""); rec.Code != http.StatusOK {
		t.Errorf("runtime merge status=%d", rec.Code)
	}
	// 账号停用：活动实例标记为冲突，缺失实例标记为停用，归属错误同样视为停用。
	srv.applications.accountSummaries = cookieGapSummary{AccountSummaryPort: real, ids: []string{"a", "b", "c"}, statusEnabled: false}
	srv.applications.accountRuntime = cookieGapRuntime{statuses: map[string]accountapp.RuntimeStatus{"a": {State: "online", Connected: true}, "b": {State: "stopped"}}}
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/cookies/runtime-status", ""); rec.Code != http.StatusOK {
		t.Errorf("runtime disabled status=%d", rec.Code)
	}
	srv.applications.accountSummaries = cookieGapSummary{AccountSummaryPort: real, ids: []string{"a"}, statusErr: boom}
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/cookies/runtime-status", ""); rec.Code != http.StatusOK {
		t.Errorf("runtime status error status=%d", rec.Code)
	}
}

// TestCookieMutationGapBranches 覆盖删除、资料刷新、启停和设置保存的错误映射。
func TestCookieMutationGapBranches(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// realSettings 是原始账号设置端口。
	realSettings := srv.applications.accountSettings
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// deleteCases 描述删除端口错误与期望状态。
	deleteCases := []struct {
		err  error
		want int
	}{
		{accountapp.ErrDeleteConflict, 409}, {accountapp.ErrForbidden, 403}, {accountapp.ErrNotFound, 404},
		{errors.New("x"), 500}, {nil, 200},
	}
	// tc 表示当前删除场景。
	for _, tc := range deleteCases {
		srv.applications.accountDelete = cookieGapDelete{err: tc.err}
		if rec := serveChatCoverageRequest(handler, cookie, "DELETE", "/cookies/acc1", ""); rec.Code != tc.want {
			t.Errorf("delete %v => %d want %d", tc.err, rec.Code, tc.want)
		}
	}
	srv.applications.accountDelete = nil
	if rec := serveChatCoverageRequest(handler, cookie, "DELETE", "/cookies/acc1", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("delete without service status=%d", rec.Code)
	}

	// profileCases 描述资料刷新错误与期望状态。
	profileCases := []struct {
		err  error
		want int
	}{{accountapp.ErrForbidden, 403}, {accountapp.ErrNotFound, 403}, {errors.New("x"), 500}}
	// tc 表示当前资料刷新场景。
	for _, tc := range profileCases {
		srv.applications.accountProfile = cookieGapProfile{err: tc.err}
		if rec := serveChatCoverageRequest(handler, cookie, "POST", "/cookies/acc1/refresh-profile", ""); rec.Code != tc.want {
			t.Errorf("profile %v => %d want %d", tc.err, rec.Code, tc.want)
		}
	}

	// statusCases 描述启停端口返回与期望状态。
	statusCases := []struct {
		result accountapp.StatusResult
		err    error
		want   int
	}{
		{accountapp.StatusResult{}, accountapp.ErrForbidden, 403},
		{accountapp.StatusResult{}, accountapp.ErrNotFound, 404},
		{accountapp.StatusResult{}, errors.New("x"), 500},
		{accountapp.StatusResult{RuntimeError: accountapp.ErrRuntimeStopConflict}, nil, 409},
		{accountapp.StatusResult{RuntimeError: errors.New("start failed")}, nil, 503},
	}
	// tc 表示当前启停场景。
	for _, tc := range statusCases {
		srv.applications.accountSettings = cookieGapSettings{AccountSettingsPort: realSettings, statusResult: tc.result, statusErr: tc.err}
		if rec := serveChatCoverageRequest(handler, cookie, "PUT", "/cookies/acc1/status", `{"enabled":true}`); rec.Code != tc.want {
			t.Errorf("status %v/%v => %d want %d", tc.result, tc.err, rec.Code, tc.want)
		}
	}

	// settingsCases 描述设置保存端口返回与期望状态。
	settingsCases := []struct {
		result accountapp.SettingsResult
		err    error
		want   int
	}{
		{accountapp.SettingsResult{TokenCleanupError: errors.New("cleanup")}, nil, 200},
		{accountapp.SettingsResult{RuntimeError: errors.New("restart")}, nil, 503},
		{accountapp.SettingsResult{}, accountapp.ErrForbidden, 403},
		{accountapp.SettingsResult{}, accountapp.ErrNotFound, 404},
		{accountapp.SettingsResult{}, errors.New("bad"), 400},
	}
	// tc 表示当前设置保存场景。
	for _, tc := range settingsCases {
		srv.applications.accountSettings = cookieGapSettings{AccountSettingsPort: realSettings, settingsResult: tc.result, settingsErr: tc.err}
		if rec := serveChatCoverageRequest(handler, cookie, "PUT", "/cookies/acc1/settings", `{"remark":"r"}`); rec.Code != tc.want {
			t.Errorf("settings %v/%v => %d want %d", tc.result, tc.err, rec.Code, tc.want)
		}
	}
	// 登录信息写入失败返回 500，密码为空且显式清除时仍走写入。
	srv.applications.accountSettings = cookieGapSettings{AccountSettingsPort: realSettings, loginInfoErr: errors.New("x")}
	if rec := serveChatCoverageRequest(handler, cookie, "PUT", "/cookies/acc1/login-info", `{"username":"u","clear_password":true}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("login info status=%d", rec.Code)
	}
}
