package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	accountapp "xianyu-go/internal/application/account"
)

// qrGapLogin 是扫码账号登录端口桩。
type qrGapLogin struct {
	// AccountLoginPort 提供未覆盖方法的默认（空）实现。
	AccountLoginPort
	// authorizeErr 是 AuthorizeQRSession 的错误。
	authorizeErr error
	// persistErr 是 PersistQRLoginSuccess 的错误。
	persistErr error
	// expired 是 CleanupQRSessions 报告的过期会话。
	expired []string
}

// RegisterQRSession 忽略登记。
func (l *qrGapLogin) RegisterQRSession(string, int64, time.Time) {}

// AuthorizeQRSession 返回预设错误。
func (l *qrGapLogin) AuthorizeQRSession(string, int64) error { return l.authorizeErr }

// CleanupQRSessions 返回预设的过期会话。
func (l *qrGapLogin) CleanupQRSessions(time.Time) []string { return l.expired }

// PersistQRLoginSuccess 返回预设结果。
func (l *qrGapLogin) PersistQRLoginSuccess(context.Context, int64, string, map[string]any, string) (AccountLoginResult, error) {
	return AccountLoginResult{AccountID: "acc-new", IsNew: true}, l.persistErr
}

// qrGapPlatform 是二维码平台端口桩，并记录被删除的会话。
type qrGapPlatform struct {
	// generateErr 是 GenerateQRCode 的错误。
	generateErr error
	// status 是 GetSessionStatus 返回的状态。
	status map[string]any
	// completeErr 是 CompleteVerification 的错误。
	completeErr error
	// unb 是 CompleteVerification 返回的平台账号标识。
	unb string
	// deleted 记录 DeleteSession 收到的会话标识。
	deleted []string
}

// GenerateQRCode 返回预设结果。
func (p *qrGapPlatform) GenerateQRCode(context.Context) (string, string, error) {
	return "sid", "https://qr", p.generateErr
}

// GetSessionStatus 返回预设状态。
func (p *qrGapPlatform) GetSessionStatus(string) map[string]any { return p.status }

// CompleteVerification 返回预设结果。
func (p *qrGapPlatform) CompleteVerification(context.Context, string) (string, string, error) {
	return "cookie-placeholder", p.unb, p.completeErr
}

// DeleteSession 记录删除请求。
func (p *qrGapPlatform) DeleteSession(id string) { p.deleted = append(p.deleted, id) }

// TestQRLoginHandlerGapBranches 覆盖二维码接口的服务缺失、平台错误、持久化错误与账号不匹配分支。
func TestQRLoginHandlerGapBranches(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// login 是可变账号登录桩。
	login := &qrGapLogin{}
	// platform 是可变平台桩。
	platform := &qrGapPlatform{status: map[string]any{"status": "success", "cookies": "secret"}, unb: "unb1"}
	srv.applications.accountLogin = login
	srv.applications.qrLogin = platform
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// generate 失败映射：取消、超时和其他错误均为 502。
	for _, generateErr := range []error{context.Canceled, context.DeadlineExceeded, errors.New("x")} {
		platform.generateErr = generateErr
		if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/generate", ""); rec.Code != http.StatusBadGateway {
			t.Errorf("generate %v status=%d", generateErr, rec.Code)
		}
	}
	platform.generateErr = nil
	login.expired = []string{"old-session"}
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/generate", ""); rec.Code != http.StatusOK {
		t.Errorf("generate ok status=%d", rec.Code)
	}
	if len(platform.deleted) == 0 {
		t.Error("expired session should be deleted from platform")
	}
	srv.applications.accountLogin = nil
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/generate", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("generate without login service status=%d", rec.Code)
	}
	srv.applications.accountLogin = login
	srv.applications.qrLogin = nil
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/generate", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("generate without qr service status=%d", rec.Code)
	}
	// 其余接口在二维码服务缺失时同样返回 500。
	for _, route := range []struct{ method, path string }{
		{"GET", "/qr-login/check/s1"}, {"GET", "/qr-login/status/s1"}, {"POST", "/qr-login/complete-verification/s1"},
	} {
		if rec := serveChatCoverageRequest(handler, cookie, route.method, route.path, ""); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s without qr service status=%d", route.path, rec.Code)
		}
	}
	srv.applications.qrLogin = platform

	// 会话所有权失败：会话不存在时同时清理平台会话；账号登录服务缺失时 500。
	login.authorizeErr = accountapp.ErrQRLoginSessionNotFound
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/qr-login/check/s1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unowned status=%d", rec.Code)
	}
	login.authorizeErr = nil
	srv.applications.accountLogin = nil
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/qr-login/check/s1", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("owner check without login service status=%d", rec.Code)
	}
	srv.applications.accountLogin = login

	// status 持久化：成功状态保存失败返回 500；非成功状态直接返回。
	login.persistErr = errors.New("db")
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/qr-login/status/s1", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("status persist failure=%d", rec.Code)
	}
	login.persistErr = nil
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/qr-login/status/s1", ""); rec.Code != http.StatusOK {
		t.Errorf("status persist ok=%d", rec.Code)
	}
	platform.status = map[string]any{"status": "waiting"}
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/qr-login/status/s1", ""); rec.Code != http.StatusOK {
		t.Errorf("status waiting=%d", rec.Code)
	}

	// complete-verification 的各类分支。
	platform.completeErr = errors.New("risk")
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/complete-verification/s1", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("complete failure=%d", rec.Code)
	}
	platform.completeErr = nil
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/complete-verification/s1", "{"); rec.Code != http.StatusBadRequest {
		t.Errorf("complete bad body=%d", rec.Code)
	}
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/complete-verification/s1", `{"target_account_id":"other"}`); rec.Code != http.StatusConflict {
		t.Errorf("complete mismatch=%d", rec.Code)
	}
	login.persistErr = errors.New("db")
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/complete-verification/s1", `{}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("complete persist failure=%d", rec.Code)
	}
	login.persistErr = nil
	platform.status = map[string]any{"cookie_snapshot": "snapshot-placeholder"}
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/qr-login/complete-verification/s1", `{"target_account_id":"unb1"}`); rec.Code != http.StatusOK {
		t.Errorf("complete ok=%d", rec.Code)
	}
}

// TestPublicQRStatusMapsOptionalFields 覆盖对外扫码状态映射中的全部可选字段。
func TestPublicQRStatusMapsOptionalFields(t *testing.T) {
	// got 是映射后的对外状态。
	got := publicQRStatus(map[string]any{
		"status": "success", "success": true, "message": "ok", "verification_screenshot": "shot",
		"face_qr_url": "face", "session_id": "s", "account_id": "a", "is_new_account": true, "cookies": "secret",
	})
	if !got.Success || got.Message != "ok" || got.VerificationScreenshot != "shot" || got.FaceQRURL != "face" || got.SessionID != "s" || got.AccountID != "a" || !got.IsNewAccount {
		t.Errorf("mapped status = %+v", got)
	}
}
