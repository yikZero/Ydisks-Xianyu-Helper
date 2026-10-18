package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// freeAddr 获取一个空闲 TCP 端口（立即释放，供测试绑定）。
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// TestRun_ServesHealthAndShutdowns 启动 HTTP 服务，/health 可访问，ctx 取消后优雅退出。
func TestRun_ServesHealthAndShutdowns(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Addr = freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	// 轮询 /health 直到可用（最多 3s）。
	url := "http://" + srv.Addr + "/health"
	var ok bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ok = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		cancel()
		t.Fatal("Run 启动后 /health 3s 内不可访问")
	}

	// 取消 ctx → Run 应优雅退出。
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run 应返回 nil，got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在 ctx 取消后 5s 内退出")
	}
}

func TestPublishRecoveryLifecycleStopsBeforeWorkerWait(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	// err 表示测试批量恢复组件启动失败的原因。
	if err := startTestBatchRecovery(srv, ctx); err != nil {
		t.Fatalf("启动批量发布恢复扫描器: %v", err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		_ = closeTestBatchRecovery(srv, context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("批量发布恢复扫描器关闭后没有退出")
	}
}

// TestNewRejectsMissingRequiredDependencies 确保 HTTP Server 只接受完整的不可变依赖快照。
func TestNewRejectsMissingRequiredDependencies(t *testing.T) {
	// emptyErr 表示缺少认证、应用服务和基础设施端口时的构造校验失败。
	if _, emptyErr := New(Dependencies{}); emptyErr == nil {
		t.Fatal("缺少依赖快照时应返回构造错误")
	}
	// source、cleanup 提供已经按组合根完成构造的基线 Server。
	source, _, cleanup := newTestServer(t)
	defer cleanup()
	// missingApplications 保存故意移除应用服务集合后的构造输入。
	missingApplications := Dependencies{
		Auth: source.Auth, WebDir: source.WebDir, Addr: source.Addr, Logger: source.Logger,
		DatabaseHealth: source.databaseHealth,
	}
	// applicationsErr 表示构造阶段没有应用服务集合时的失败。
	if _, applicationsErr := New(missingApplications); applicationsErr == nil {
		t.Fatal("缺少应用服务集合时应返回构造错误")
	}
	// incompleteApplications 在容器存在但未绑定全部路由 Port 时也必须于启动前失败。
	incompleteApplications := Dependencies{
		Auth: source.Auth, WebDir: source.WebDir, Addr: source.Addr, Logger: source.Logger,
		DatabaseHealth: source.databaseHealth, Applications: NewApplicationPorts(ApplicationPortsInput{}),
	}
	// incompleteErr 是容器存在但缺少路由所需 Port 时的构造失败。
	if _, incompleteErr := New(incompleteApplications); incompleteErr == nil {
		t.Fatal("缺少必需应用 Port 时应返回构造错误")
	}
}

// TestNewAcceptsPrebuiltApplicationSet 验证新组合根入口只接收已经装配完成的应用服务集合。
func TestNewAcceptsPrebuiltApplicationSet(t *testing.T) {
	// source、cleanup 提供一个由兼容测试装配器构造完成的完整服务集合。
	source, _, cleanup := newTestServer(t)
	defer cleanup()
	// dependencies 将已有构造结果转换为不可变 Server 依赖快照。
	dependencies := Dependencies{
		Auth: source.Auth, WebDir: source.WebDir, Addr: source.Addr, Logger: source.Logger,
		DatabaseHealth: source.databaseHealth,
		Applications:   source.applications,
	}
	// serverInstance、constructErr 保存纯注入入口的构造结果。
	serverInstance, constructErr := New(dependencies)
	if constructErr != nil {
		t.Fatalf("New: %v", constructErr)
	}
	if serverInstance == nil || serverInstance.applications == nil || serverInstance.applications.orders != source.applications.orders {
		t.Fatal("新组合根入口未保留预构造应用服务集合")
	}
}

// TestServerBindReportsOccupiedPort 验证端口冲突在应用 worker 启动前由同步 Bind 返回。
func TestServerBindReportsOccupiedPort(t *testing.T) {
	// occupied、listenErr 分别是故意占用的本机监听器及创建错误。
	occupied, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("占用测试端口失败: %v", listenErr)
	}
	defer occupied.Close()
	// srv、cleanup 分别是待绑定的 HTTP Server 及测试资源释放函数。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Addr = occupied.Addr().String()
	// bindErr 是与已占用端口冲突时必须同步返回的绑定错误。
	bindErr := srv.Bind()
	if bindErr == nil {
		t.Fatal("已占用端口的 Bind 应同步返回错误")
	}
	if srv.started {
		t.Fatal("绑定失败不能把 HTTP 服务标记为已启动")
	}
}

// TestServerStopReleasesBoundListener 验证应用启动失败回滚可关闭已绑定但尚未 Serve 的端口。
func TestServerStopReleasesBoundListener(t *testing.T) {
	// srv、cleanup 分别是待测试的 HTTP Server 及测试资源释放函数。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Addr = freeAddr(t)
	// bindErr 是为启动失败回滚预绑定监听器时的错误。
	if bindErr := srv.Bind(); bindErr != nil {
		t.Fatalf("Bind: %v", bindErr)
	}
	// stopErr 是关闭尚未进入 Serve 的监听器时的错误。
	if stopErr := srv.Stop(context.Background()); stopErr != nil {
		t.Fatalf("停止已绑定监听器失败: %v", stopErr)
	}
	// replacement、listenErr 分别验证同一地址已经由 Stop 释放的监听器及创建错误。
	replacement, listenErr := net.Listen("tcp", srv.Addr)
	if listenErr != nil {
		t.Fatalf("停止后端口未释放: %v", listenErr)
	}
	defer replacement.Close()
}
