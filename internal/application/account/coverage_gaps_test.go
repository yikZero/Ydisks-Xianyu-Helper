package account

import (
	"context"
	"errors"
	"testing"
)

// verifiedAuthenticationFake 在基础认证替身上追加认证代次闭环与会话校验扩展端口，用于覆盖扩展分支。
type verifiedAuthenticationFake struct {
	// authenticationRepositoryFake 提供认证端口的其余方法。
	authenticationRepositoryFake
	// verifyLoginUser、verifyLoginMatched、verifyLoginErr 是 VerifyLogin 的预置返回值。
	verifyLoginUser    AuthUser
	verifyLoginMatched bool
	verifyLoginErr     error
	// verifiedSessionID、verifiedSessionErr 是 CreateVerifiedSession 的预置返回值。
	verifiedSessionID  string
	verifiedSessionErr error
	// validateErr 是 ValidateSession 的预置返回值。
	validateErr error
}

// VerifyLogin 返回预置的带代次密码校验结果。
func (f *verifiedAuthenticationFake) VerifyLogin(context.Context, string, string) (AuthUser, bool, error) {
	return f.verifyLoginUser, f.verifyLoginMatched, f.verifyLoginErr
}

// CreateVerifiedSession 返回预置的校验后会话签发结果。
func (f *verifiedAuthenticationFake) CreateVerifiedSession(context.Context, AuthUser) (string, error) {
	return f.verifiedSessionID, f.verifiedSessionErr
}

// ValidateSession 返回预置的本地会话校验结果。
func (f *verifiedAuthenticationFake) ValidateSession(context.Context, string, int64) error {
	return f.validateErr
}

// TestLoginUsesVerifiedRepositoryBranches 覆盖认证代次闭环下的校验错误、不匹配、会话签发失败与成功四条路径。
func TestLoginUsesVerifiedRepositoryBranches(t *testing.T) {
	// boom 是注入的固定基础设施错误。
	boom := errors.New("基础设施失败")
	// cases 描述每个分支的替身配置与期望。
	cases := []struct {
		name string
		// fake 是该分支的认证替身。
		fake *verifiedAuthenticationFake
		// wantSession 是期望返回的会话 ID。
		wantSession string
		// wantUser 表示期望返回非空用户。
		wantUser bool
		// wantErr 是期望的错误。
		wantErr error
	}{
		{name: "verify error", fake: &verifiedAuthenticationFake{verifyLoginErr: boom}, wantErr: boom},
		{name: "mismatch", fake: &verifiedAuthenticationFake{}},
		{name: "session error", fake: &verifiedAuthenticationFake{verifyLoginMatched: true, verifiedSessionErr: boom}, wantErr: boom},
		{name: "success", fake: &verifiedAuthenticationFake{verifyLoginMatched: true, verifyLoginUser: AuthUser{ID: 3, Username: "u"}, verifiedSessionID: "sid"}, wantSession: "sid", wantUser: true},
	}
	// tc 是当前用例。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// service、err 是被测认证服务。
			service, err := NewAuthenticationService(tc.fake)
			if err != nil {
				t.Fatal(err)
			}
			// sessionID、user、loginErr 是登录结果。
			sessionID, user, loginErr := service.Login(context.Background(), "u", "p")
			if !errors.Is(loginErr, tc.wantErr) || sessionID != tc.wantSession || (user != nil) != tc.wantUser {
				t.Fatalf("session=%q user=%v err=%v", sessionID, user, loginErr)
			}
		})
	}
}

// TestValidateSessionDelegatesOrRejects 验证会话校验在仓储支持时透传结果，不支持时返回稳定错误。
func TestValidateSessionDelegatesOrRejects(t *testing.T) {
	// plain 的仓储不支持会话校验扩展。
	plain, err := NewAuthenticationService(&authenticationRepositoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.ValidateSession(context.Background(), "sid", 1) == nil {
		t.Fatal("不支持会话校验应返回错误")
	}
	// boom 是校验失败的固定错误。
	boom := errors.New("会话已失效")
	// supported 的仓储支持会话校验。
	supported, err := NewAuthenticationService(&verifiedAuthenticationFake{validateErr: boom})
	if err != nil {
		t.Fatal(err)
	}
	if got := supported.ValidateSession(context.Background(), "sid", 1); !errors.Is(got, boom) {
		t.Fatalf("got=%v", got)
	}
	// ok 的仓储校验通过。
	ok, err := NewAuthenticationService(&verifiedAuthenticationFake{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ok.ValidateSession(context.Background(), "sid", 1); got != nil {
		t.Fatalf("got=%v", got)
	}
}

// TestSettingsServiceAutoConsignAndBargainShortcuts 验证自动确认发货与自动免拼快捷入口通过统一设置更新路径落库。
func TestSettingsServiceAutoConsignAndBargainShortcuts(t *testing.T) {
	// repository 是设置仓储替身。
	repository := &fakeSettingsRepository{}
	// service、err 是账号设置服务。
	service, err := NewSettingsService(repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	// consignErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, consignErr := service.SetAutoConsign(context.Background(), 7, "account", true); consignErr != nil {
		t.Fatalf("SetAutoConsign: %v", consignErr)
	}
	// bargainErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, bargainErr := service.SetAutoBargain(context.Background(), 7, "account", true); bargainErr != nil {
		t.Fatalf("SetAutoBargain: %v", bargainErr)
	}
}

// TestValidateSessionNilReceiver 验证未初始化的认证服务拒绝会话校验。
func TestValidateSessionNilReceiver(t *testing.T) {
	// nilService 是未初始化的认证服务。
	var nilService *AuthenticationService
	if nilService.ValidateSession(context.Background(), "sid", 1) == nil {
		t.Fatal("nil 服务应返回错误")
	}
}
