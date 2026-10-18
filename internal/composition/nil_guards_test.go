package composition

import (
	"context"
	"testing"
	"time"
)

// TestAccountLoginServiceRejectsUninitializedState 覆盖账号登录服务在依赖缺失时的安全失败，不得出现空指针或静默成功。
func TestAccountLoginServiceRejectsUninitializedState(t *testing.T) {
	ctx := context.Background()
	// services 同时覆盖 nil 接收者与零值服务两种未初始化形态。
	for _, service := range []*accountLoginService{nil, {}} {
		if err := service.CreateCookie(ctx, "a", "c", 1, "manual"); err == nil {
			t.Fatal("CreateCookie 应拒绝未初始化服务")
		}
		if err := service.UpdateCookie(ctx, "a", "c", 1, "manual", 2); err == nil {
			t.Fatal("UpdateCookie 应拒绝未初始化服务")
		}
		// err 保存扫码持久化的错误，未初始化时必须非空。
		if _, err := service.PersistQRLoginSuccess(ctx, 1, "s", nil, "a"); err == nil {
			t.Fatal("PersistQRLoginSuccess 应拒绝未初始化服务")
		}
		if err := service.AuthorizeQRSession("s", 1); err == nil {
			t.Fatal("AuthorizeQRSession 应拒绝未初始化服务")
		}
		service.RegisterQRSession("s", 1, time.Now())
		if got := service.CleanupQRSessions(time.Now()); got != nil {
			t.Fatalf("未初始化服务不应返回清理结果: %v", got)
		}
		// port 保存本次调用返回的凭证端口，用于断言未初始化时不暴露。
		if port := service.CredentialSessionPort(); port != nil {
			t.Fatalf("未初始化服务不应暴露凭证端口: %v", port)
		}
	}
}

// TestServicesNilReceiverGuards 覆盖组合服务在空接收者或缺少子服务时的保守行为。
func TestServicesNilReceiverGuards(t *testing.T) {
	ctx := context.Background()
	// empty 是没有任何子服务的组合服务。
	empty := &Services{}
	if empty.LifecycleContext() != nil || (*Services)(nil).LifecycleContext() != nil {
		t.Fatal("缺少生命周期函数时应返回 nil Context")
	}
	// services 当前待验证的组合服务形态（nil 或零值）。
	for _, services := range []*Services{nil, empty} {
		if err := services.UpdateRunningCookie(ctx, "a", "v"); err != nil {
			t.Fatalf("缺少账号运行时时 Cookie 同步应为空操作: %v", err)
		}
		if err := services.RefreshRuntimeAccount(ctx, "a"); err == nil {
			t.Fatal("缺少订单服务时同步应返回错误")
		}
		if services.RecoverExpiredCredential(ctx, "a") {
			t.Fatal("缺少账号运行时时不应声明已恢复")
		}
	}
	// components 保存空服务登记的生命周期组件列表。
	if components := empty.LifecycleComponents(); len(components) != 0 {
		t.Fatalf("空服务不应登记组件: %v", components)
	}
}

// TestSettingsRuntimeTransportRestartRequiresLifecycleContext 覆盖重启必须使用进程生命周期 Context 的保护。
func TestSettingsRuntimeTransportRestartRequiresLifecycleContext(t *testing.T) {
	// lifecycle 当前提供的生命周期 Context 函数，nil 或返回 nil 均视为未初始化。
	for _, lifecycle := range []func() context.Context{nil, func() context.Context { return nil }} {
		// transport 保存缺少有效生命周期 Context 的设置运行时转发器。
		transport := settingsRuntimeTransport{lifecycleContext: lifecycle}
		if err := transport.Restart(context.Background(), "a"); err == nil {
			t.Fatal("缺少生命周期 Context 时应拒绝重启")
		}
	}
}
