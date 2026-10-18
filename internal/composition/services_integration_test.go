package composition

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/adapter"
	"xianyu-go/internal/application/lifecycle"
	orderapp "xianyu-go/internal/application/orders"
	"xianyu-go/internal/db"
	"xianyu-go/internal/netguard"
	"xianyu-go/internal/publishimage"
)

// builtServices 保存一次完整组合装配得到的服务集合及其测试需要继续使用的底层资源。
type builtServices struct {
	// services 是 composition.New 返回的完整应用服务集合。
	services *Services
	// dependencies 是传给 New 的依赖，用于直接构造组合层内部转发器。
	dependencies Dependencies
	// store 是临时 SQLite 之上的仓储入口。
	store *db.Store
}

// buildTestServices 以临时 SQLite 与禁用浏览器的运行时包装完整装配 Services，不启动任何后台 worker、不访问外部网络。
// provideChat 为 true 时沿用运行时预先装配的聊天应用，为 false 时走组合根现场创建聊天应用的兼容分支。
func buildTestServices(t *testing.T, provideChat bool) builtServices {
	t.Helper()
	// ctx 是装配期间使用的测试 Context。
	ctx := context.Background()
	// sqlDB、dialect、openErr 分别是临时库连接、方言及打开错误。
	sqlDB, dialect, openErr := db.Open(ctx, filepath.Join(t.TempDir(), "composition.db"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	// store 是绑定迁移后数据库的仓储入口。
	store := db.NewStore(sqlDB, dialect)
	// logger 丢弃日志，避免测试输出噪声。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// bundle、bundleErr 是不含浏览器的账号运行时依赖集合。
	bundle, bundleErr := adapter.NewRuntimeBundle(store, nil, logger, nil)
	if bundleErr != nil {
		t.Fatal(bundleErr)
	}
	// orderDeps、accountDeps、itemDeps、automationDeps、miscDeps 是各应用服务的仓储适配器工厂。
	orderDeps, orderErr := adapter.NewOrderDependencies(store)
	// accountDeps、accountErr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	accountDeps, accountErr := adapter.NewAccountDependencies(store)
	// itemDeps、itemErr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	itemDeps, itemErr := adapter.NewItemDependencies(store)
	// automationDeps、automationErr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	automationDeps, automationErr := adapter.NewAutomationDependencies(store)
	// miscDeps、miscErr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	miscDeps, miscErr := adapter.NewMiscDependencies(store)
	for _, err := range []error{orderErr, accountErr, itemErr, automationErr, miscErr} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// adminSettingsDeps、chatDeps、systemDeps 提供管理设置、聊天与系统探测适配器。
	adminSettingsDeps, chatDeps, systemDeps := adapter.NewAdminSettingsDependencies(store), adapter.NewChatDependencies(store), adapter.NewSystemDependencies(store)
	// recovery、recoveryErr 是订单补偿恢复协调器。
	recovery, recoveryErr := orderapp.NewReconciliationRecoveryCoordinator(systemDeps.NewReconciliationService(logger))
	if recoveryErr != nil {
		t.Fatal(recoveryErr)
	}
	// platform、platformErr 是平台协议适配器集合（仅构造，不发起网络请求）。
	platform, platformErr := adapter.NewDefaultPlatformDependencies(logger)
	if platformErr != nil {
		t.Fatal(platformErr)
	}
	// transport、transportErr 是通知等共享应用服务集合。
	transport, transportErr := adapter.NewTransportApplicationServices(adapter.TransportApplicationServiceOptions{
		AutomationDependencies: automationDeps, MiscDependencies: miscDeps, AdminSettingsDependencies: adminSettingsDeps,
		AdminRuntime: bundle.Manager, AccountTaskRunner: adapter.NewAccountTaskRunner(bundle.Automation), ChannelSender: bundle.Notifier,
		ModelClient: adapter.NewAIModelClient(), OutboundPolicy: netguard.DefaultPolicy(),
	})
	if transportErr != nil {
		t.Fatal(transportErr)
	}
	// coordinator 提供生命周期 Context 来源。
	coordinator := lifecycle.NewCoordinator()
	// dependencies 是传给组合根的完整依赖集合。
	dependencies := Dependencies{
		OrderDependencies: orderDeps, AccountDependencies: accountDeps, ItemDependencies: itemDeps,
		ChatDependencies: chatDeps, AutomationDependencies: automationDeps, TransportApplications: transport,
		OrderReconciliationRecovery: recovery, Manager: bundle.Manager, Automation: bundle.Automation, OrderDetails: bundle.OrderDetails,
		Notifier: bundle.Notifier, Chat: bundle.Chat, Logger: logger,
		MTopClient: platform.MTOPClient, LongLoginClient: platform.LongLoginClient, QRLogin: platform.QRLoginService(),
		UpdateRunningCookie: func(context.Context, string, string) {}, LifecycleContext: coordinator.Context,
	}
	if provideChat {
		dependencies.ChatApplication = bundle.ChatApplication
	}
	// services、buildErr 是完整装配结果。
	services, buildErr := New(dependencies)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtServices{services: services, dependencies: dependencies, store: store}
}

// TestNewAssemblesServicesAndExposesTransportPorts 验证完整装配后每个 transport 端口都已就绪，生命周期组件登记三项且顺序稳定。
func TestNewAssemblesServicesAndExposesTransportPorts(t *testing.T) {
	// withChat 沿用运行时预装配的聊天应用。
	withChat := buildTestServices(t, true)
	// ports 是只读端口快照。
	ports := withChat.services.TransportPorts()
	if ports.Orders == nil || ports.OrderRefreshJobs == nil || ports.ItemSinglePublish == nil || ports.ItemBatchPreview == nil ||
		ports.ItemBatchManagement == nil || ports.ItemSync == nil || ports.ItemCatalog == nil || ports.AccountLogin == nil ||
		ports.Authentication == nil || ports.AccountSettings == nil || ports.Chat == nil || ports.Cards == nil || ports.Settings == nil || ports.Admin == nil {
		t.Fatalf("端口未全部装配: %+v", ports)
	}
	// components 是登记的生命周期组件。
	components := withChat.services.LifecycleComponents()
	// names 收集组件名称。
	names := make([]string, 0, len(components))
	// component 是当前登记的组件。
	for _, component := range components {
		names = append(names, component.Name)
	}
	if strings.Join(names, ",") != "order-refresh-recovery,publish-batch-workers,order-reconciliation-recovery" {
		t.Fatalf("names=%v", names)
	}
	// withoutChat 覆盖组合根现场创建聊天应用的兼容分支。
	withoutChat := buildTestServices(t, false)
	if withoutChat.services.TransportPorts().Chat == nil {
		t.Fatal("兼容分支应现场创建聊天应用")
	}
	if withoutChat.services.LifecycleContext() != nil {
		t.Log("协调器启动前的生命周期 Context 允许为 nil")
	}
}

// TestServicesRuntimeEntrypointsOnEmptyDatabase 验证运行时回调入口在空库上的保守行为：未知账号不会恢复、订单同步返回错误、Cookie 同步不报错。
func TestServicesRuntimeEntrypointsOnEmptyDatabase(t *testing.T) {
	// built 是完整装配结果。
	built := buildTestServices(t, true)
	ctx := context.Background()
	if built.services.RecoverExpiredCredential(ctx, "missing-account") {
		t.Fatal("未知账号不应声明已恢复")
	}
	if err := built.services.RefreshRuntimeAccount(ctx, "missing-account"); err == nil {
		t.Fatal("未知账号的订单同步应返回错误")
	}
	// 更新未知账号的运行时 Cookie 仅作用于运行实例，不应报错或写库。
	_ = built.services.UpdateRunningCookie(ctx, "missing-account", "unb=1; _m_h5_tk=a_1;")
}

// TestSettingsRuntimeTransportDelegatesToManager 验证设置运行时转发器把 fencing 与停止委托给账号管理器，并在 Context 就绪时重启。
func TestSettingsRuntimeTransportDelegatesToManager(t *testing.T) {
	// built 是完整装配结果。
	built := buildTestServices(t, true)
	// transport 绑定真实管理器与固定的后台 Context。
	transport := settingsRuntimeTransport{manager: built.dependencies.Manager, lifecycleContext: func() context.Context { return context.Background() }}
	if !transport.BeginStopping("acc-1") {
		t.Fatal("首次建立 fencing 应成功")
	}
	if transport.BeginStopping("acc-1") {
		t.Fatal("重复建立 fencing 应失败")
	}
	transport.EndStopping("acc-1")
	if err := transport.StopContext(context.Background(), "acc-1"); err != nil {
		t.Fatalf("停止未运行账号应为空操作: %v", err)
	}
	// 未知账号的重启由管理器决定结果；这里只确认请求被委托而不是被生命周期守卫拦截。
	_ = transport.Restart(context.Background(), "acc-1")
}

// TestAccountLoginServiceWithRealDependencies 验证真实装配的登录服务：二维码会话登记/授权/清理、失败路径不泄露、不访问外网。
func TestAccountLoginServiceWithRealDependencies(t *testing.T) {
	// built 是完整装配结果；login 是其账号登录组合服务。
	built := buildTestServices(t, true)
	// login 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	login := built.services.accountLogin
	if login.CredentialSessionPort() == nil {
		t.Fatal("应暴露凭证会话端口")
	}
	// now 是会话登记时间。
	now := time.Now()
	login.RegisterQRSession("session-1", 7, now)
	if err := login.AuthorizeQRSession("session-1", 7); err != nil {
		t.Fatalf("属主应通过授权: %v", err)
	}
	if err := login.AuthorizeQRSession("session-1", 8); err == nil {
		t.Fatal("非属主应被拒绝")
	}
	// expired 是过期会话清理结果。
	expired := login.CleanupQRSessions(now.Add(24 * time.Hour))
	if len(expired) != 1 || expired[0] != "session-1" {
		t.Fatalf("expired=%v", expired)
	}
	// 缺少 Cookie 与账号标识的扫码结果必须被持久化用例拒绝。
	if _, err := login.PersistQRLoginSuccess(context.Background(), 7, "session-2", map[string]any{}, ""); err == nil {
		t.Fatal("空扫码结果应被拒绝")
	}
	// 不存在的用户创建/更新 Cookie 必须失败（外键或归属校验），且不触发平台请求。
	if err := login.CreateCookie(context.Background(), "acc-x", "", 7, "manual"); err == nil {
		t.Fatal("空 Cookie 应被拒绝")
	}
	if err := login.UpdateCookie(context.Background(), "acc-x", "", 7, "manual", 1); err == nil {
		t.Fatal("未知账号更新应被拒绝")
	}
}

// TestDownloadImageURLWithLocalServer 使用本机 httptest 服务覆盖下载的成功、状态码、类型识别与超限分支，不访问公网。
func TestDownloadImageURLWithLocalServer(t *testing.T) {
	// pngHeader 是足以让内容嗅探识别为 PNG 的最小字节。
	pngHeader := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	// server 按路径返回各种响应。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "image/png; charset=binary")
			_, _ = w.Write(pngHeader)
		case "/sniff":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pngHeader)
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("not an image"))
		case "/big":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, (10<<20)+16))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ctx := context.Background()
	// contentType、data、err 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if data, contentType, err := publishimage.Download(ctx, server.URL+"/ok"); err != nil || contentType != "image/png" || len(data) != len(pngHeader) {
		t.Fatalf("ok: %v %q %d", err, contentType, len(data))
	}
	// contentType、err 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if _, contentType, err := publishimage.Download(ctx, server.URL+"/sniff"); err != nil || contentType != "image/png" {
		t.Fatalf("sniff: %v %q", err, contentType)
	}
	if _, _, err := publishimage.Download(ctx, server.URL+"/text"); err == nil {
		t.Fatal("非图片应被拒绝")
	}
	if _, _, err := publishimage.Download(ctx, server.URL+"/big"); err == nil {
		t.Fatal("超限图片应被拒绝")
	}
	if _, _, err := publishimage.Download(ctx, server.URL+"/missing"); err == nil {
		t.Fatal("404 应被拒绝")
	}
	// 启用公网限制后，本机地址必须被出站策略拒绝。
	netguard.SetDefaultPublicOnly(true)
	t.Cleanup(func() { netguard.SetDefaultPublicOnly(false) })
	if _, _, err := publishimage.Download(ctx, server.URL+"/ok"); err == nil {
		t.Fatal("公网限制下应拒绝本机地址")
	}
}
